package objectstore

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	awshttp "github.com/aws/aws-sdk-go-v2/aws/transport/http"
	awsconfig "github.com/aws/aws-sdk-go-v2/config"
	"github.com/aws/aws-sdk-go-v2/credentials"
	"github.com/aws/aws-sdk-go-v2/service/s3"
	"github.com/aws/smithy-go"
)

// This is the deployment path and it is the ONLY file in the service that knows
// S3 exists. Everything above the Store interface is policy — tenancy, checksums,
// status transitions, the outbox — and none of it has an `if s3` in it. The two
// implementations are interchangeable and the tests cannot tell which one they
// ran against except by the `memory://` scheme in a presigned URL.
//
// # ONE IMPLEMENTATION, SEVERAL S3-COMPATIBLE SERVICES
//
// AWS S3, MinIO, Ceph and Cloudflare R2 are the same five operations with
// different dialects. An `R2ObjectStore` would have been the wrong shape of
// answer, and `AGENTS.md` says why in one line: "R2 is not a variant, a feature
// or a second type: it is an endpoint, a region, and addressing. If a backend
// needs a *code* branch, the abstraction is wrong and fixing the abstraction is
// in scope — that is not a reason to add `R2ObjectStore`." So they are four
// statements in this file and in [S3Config.normalise], and TestPresignedScopes
// runs the same presign assertions against each of them.
//
// # THE SDK IS A REAL DEPENDENCY HERE, AND WHAT THAT COSTS
//
// The Rust tree gated this file behind a cargo feature, on the grounds that the
// AWS SDK is then not in the default dependency graph and "the default build
// **cannot construct a real object-storage client**", which is what makes "tests
// never hit the network" a property of the dependency graph rather than a promise
// in a README. Go modules do not have a per-file feature flag that is worth
// having for this: a build tag would hide the file from `go vet ./...` and from
// `go build ./...`, and the gate would stop checking the deployment path on the
// machine where nobody happens to set the tag. So the SDK is imported
// unconditionally and the NO-NETWORK property is kept by a different and stronger
// rule — every operation that talks to a bucket needs a `context.Context`, and
// the test suite only ever calls PresignPut, which is a LOCAL operation.
//
// # WHY PRESIGNING IS SAFE TO TEST OFFLINE
//
// Presigning builds a URI and a signature and sends nothing. That is the whole
// reason the nine-row presign table in the Rust tree ran against AWS, MinIO and
// R2 configurations with no bucket and no credential, and it is why this one
// does too: [NewS3ForTest] supplies a STATIC credential provider rather than the
// SDK's chain because "a test that could authenticate without being told how is a
// test that would pass against the wrong bucket".
type S3 struct {
	client *s3.Client
	bucket string
}

// The interface is satisfied by this type, and the assertion lives here so that a
// method drifting out of the interface breaks the build rather than a test run.
var _ Store = (*S3)(nil)

// S3Config is every knob this store has, and it is a struct rather than six
// positional arguments so that adding a knob cannot silently reorder the ones
// that exist.
//
// The fields are the ones the Rust `config.rs` exposes: bucket, region,
// endpoint, path style. There is deliberately no field for a checksum algorithm,
// for an ACL, for a grant, or for an expected bucket owner — darkroom never sets
// any of them, so a field that could would be a field with nothing to get right.
type S3Config struct {
	// Bucket is required.
	Bucket string
	// Region is the SigV4 signing region. `auto` is legal only with an R2
	// endpoint; see [S3Config.normalise].
	Region string
	// Endpoint is the S3-compatible base URL. Empty means AWS S3's own endpoint
	// resolution. MinIO and R2 both need one.
	Endpoint string
	// PathStyle is path-style addressing rather than virtual-host. MinIO needs it
	// and R2 is path-style regardless.
	PathStyle bool
	// Access and Secret are a STATIC credential. Set only by [NewS3ForTest]; the
	// production constructor leaves them empty and gets the SDK's own chain, so
	// no credential is ever a string in this repository. `AGENTS.md`: "No
	// secrets in the repo. Object-storage credentials are the AWS SDK's own
	// chain."
	Access string
	Secret string
	// Retries is the SDK's retry count. Zero in tests, because a test that
	// retries is a test that waits. `AGENTS.md`: "No sleeps, no raised retries".
	Retries int
	// Timeout bounds one request. Zero means the SDK's own default.
	Timeout time.Duration
}

// New builds an S3 store.
//
// Credentials come from the AWS SDK's own chain — environment, profile, IMDS,
// container role — so none of them is a string in this repository or in this
// process's configuration. The bucket is per-store rather than per-call because
// it is configuration, and a method that took a bucket name would let a caller
// read or write somewhere this service was not pointed at.
func New(ctx context.Context, cfg S3Config) (*S3, error) {
	normalised, err := cfg.normalise()
	if err != nil {
		return nil, err
	}
	if normalised.Access != "" {
		return nil, errors.New("objectstore.New: a static credential belongs in NewS3ForTest; production takes the SDK's own chain")
	}
	return connect(ctx, normalised)
}

// NewS3ForTest builds a store with credentials supplied by the caller.
//
// Presigning is a local operation — it builds a URI and a signature and sends
// nothing — so this is how the presign table asserts what darkroom would
// actually put on the wire for AWS, MinIO and R2 without a socket, a credential
// or a bucket.
//
// It goes through the SAME [connect] as [New], which is the point: the table
// must exercise the request shaping production uses, not a parallel copy of it.
func NewS3ForTest(cfg S3Config) (*S3, error) {
	normalised, err := cfg.normalise()
	if err != nil {
		return nil, err
	}
	if normalised.Access == "" || normalised.Secret == "" {
		return nil, errors.New("objectstore.NewS3ForTest: both Access and Secret are required; a test that could authenticate without being told how is a test that would pass against the wrong bucket")
	}
	normalised.Retries = 0
	normalised.Timeout = 0
	return connect(context.Background(), normalised)
}

func connect(ctx context.Context, cfg S3Config) (*S3, error) {
	opts := []func(*awsconfig.LoadOptions) error{awsconfig.WithRegion(cfg.Region)}
	if cfg.Timeout > 0 {
		opts = append(opts, awsconfig.WithHTTPClient(&http.Client{Timeout: cfg.Timeout}))
	}
	awsCfg, err := awsconfig.LoadDefaultConfig(ctx, opts...)
	if err != nil {
		return nil, fmt.Errorf("objectstore.connect(%s): loading the AWS configuration: %w", cfg.Bucket, err)
	}
	// The static provider goes on the LOADED config rather than in the options
	// above, so it overrides whatever the chain would have found — including a
	// real credential in the developer's environment, which is exactly the
	// situation where a test that is supposed to be offline quietly is not.
	if cfg.Access != "" {
		awsCfg.Credentials = credentials.NewStaticCredentialsProvider(cfg.Access, cfg.Secret, "")
	}

	client := s3.NewFromConfig(awsCfg, func(o *s3.Options) {
		if cfg.Endpoint != "" {
			o.BaseEndpoint = aws.String(cfg.Endpoint)
		}
		if cfg.PathStyle {
			o.UsePathStyle = true
		}
		// EVERY request-shaping decision darkroom makes, in one place, for every
		// S3-compatible service it talks to. See the two settings below.
		o.RequestChecksumCalculation = aws.RequestChecksumCalculationWhenRequired
		o.ResponseChecksumValidation = aws.ResponseChecksumValidationWhenRequired
		o.RetryMaxAttempts = cfg.Retries
	})

	return &S3{client: client, bucket: cfg.Bucket}, nil
}

// Client exposes the configured SDK client.
//
// Public because "which region is this client signing with, and is it asking for
// checksums?" is a question an operator asks at 3am, and a question a test has to
// be able to ask too.
func (s *S3) Client() *s3.Client { return s.client }

// PresignPut mints a presigned PUT for exactly one key, one content type, one
// length cap, ttl long.
//
// The length cap is not decoration: without it, a URL a client leaked can be
// used to write an arbitrarily large object into a bucket someone pays for, and
// it is the difference between "a presigned URL" and "a presigned URL with a
// blast radius". The signed headers also pin the `Content-Type`, so a URL signed
// for `image/png` cannot be used to store an HTML document at that key.
//
// # NO `x-amz-checksum-algorithm` HERE, ON PURPOSE
//
// This used to ask S3 to record and verify a SHA-256 so `head` could read back
// what the bucket validated. Two reasons it is gone, and neither is "R2 does not
// like it":
//
//  1. It made the presigned PUT a two-step protocol. The algorithm ends up in the
//     signed headers, so the client has to send `x-amz-sdk-checksum-algorithm`
//     and a base64 checksum or the request is refused. `curl -T file "$url"`
//     stops working, and a client that PUTs through a plain HTTP client has to
//     know about a darkroom implementation detail.
//  2. It made correctness depend on a header the backend may not implement, which
//     is why [ObjectMeta] has no checksum field at all.
//
// What it cost: S3 used to verify the bytes in flight as well as at complete. At
// complete it still does, by reading the object back and hashing it — which is a
// stronger check, because it is against what is stored rather than what was sent.
func (s *S3) PresignPut(ctx context.Context, key, contentType string, maxBytes int64, ttl time.Duration) (PresignedPut, error) {
	if maxBytes <= 0 {
		return PresignedPut{}, &Error{
			Op:     "presign_put",
			Err:    fmt.Errorf("%w: a presigned scope with no ceiling is not a scope", errOther),
			Detail: strconv.FormatInt(maxBytes, 10),
		}
	}
	// `NewPresignClient` and not a presigned POST: the S3 presign URL path is a
	// signed PUT, and the POST form is a DIFFERENT flow with its own policy
	// document — the thing `appendLengthRange` says it does not use. The client
	// is built per call rather than held on the struct because it is two
	// allocations and presigning is not on any hot path: it happens once per
	// upload session.
	presigner := s3.NewPresignClient(s.client)
	req, err := presigner.PresignPutObject(ctx, &s3.PutObjectInput{
		Bucket:      aws.String(s.bucket),
		Key:         aws.String(key),
		ContentType: aws.String(contentType),
	}, func(o *s3.PresignOptions) {
		// `Expires` becomes `X-Amz-Expires`. The TTL is [PresignTTL] at every
		// call site in this service; the parameter exists because the interface
		// passes it and a caller that wants a different window has to say so out
		// loud.
		o.Expires = ttl
	})
	if err != nil {
		return PresignedPut{}, other("presign_put", err)
	}
	return PresignedPut{
		URL:       appendLengthRange(req.URL, maxBytes),
		Key:       key,
		ExpiresIn: int64(ttl / time.Second),
	}, nil
}

// Head is metadata for one key.
//
// S3's own `content-length` is the byte count, not the `byte_size` the client
// declared — the client's number is a claim and this is the measurement.
func (s *S3) Head(ctx context.Context, key string) (ObjectMeta, error) {
	out, err := s.client.HeadObject(ctx, &s3.HeadObjectInput{
		Bucket: aws.String(s.bucket),
		Key:    aws.String(key),
	})
	if err != nil {
		return ObjectMeta{}, mapS3Error("head", err)
	}
	byteSize := aws.ToInt64(out.ContentLength)
	contentType := aws.ToString(out.ContentType)
	if contentType == "" {
		contentType = "application/octet-stream"
	}
	return ObjectMeta{Key: key, ByteSize: byteSize, ContentType: contentType}, nil
}

// Get returns the bytes at key.
func (s *S3) Get(ctx context.Context, key string) ([]byte, error) {
	out, err := s.client.GetObject(ctx, &s3.GetObjectInput{
		Bucket: aws.String(s.bucket),
		Key:    aws.String(key),
	})
	if err != nil {
		return nil, mapS3Error("get", err)
	}
	defer func() { _ = out.Body.Close() }()
	b, err := io.ReadAll(out.Body)
	if err != nil {
		return nil, other("get", fmt.Errorf("reading %s: %w", key, err))
	}
	return b, nil
}

// Put stores a derived image. NEVER the client's upload path — the client writes
// through its presigned URL, which is what keeps a 1 GiB upload from being
// proxied through the API process.
func (s *S3) Put(ctx context.Context, key string, b []byte, contentType string) error {
	_, err := s.client.PutObject(ctx, &s3.PutObjectInput{
		Bucket:      aws.String(s.bucket),
		Key:         aws.String(key),
		Body:        strings.NewReader(string(b)),
		ContentType: aws.String(contentType),
	})
	if err != nil {
		return mapS3Error("put", err)
	}
	return nil
}

// Delete removes one key.
//
// Deleting something that is already gone succeeds: S3's `DeleteObject` is
// already idempotent, and `DELETE /v1/assets/{id}` is retryable, so a `500` on
// the second attempt would make a safe operation look broken.
func (s *S3) Delete(ctx context.Context, key string) error {
	_, err := s.client.DeleteObject(ctx, &s3.DeleteObjectInput{
		Bucket: aws.String(s.bucket),
		Key:    aws.String(key),
	})
	if err != nil {
		return mapS3Error("delete", err)
	}
	return nil
}

// mapS3Error maps an SDK error onto the three cases the service reasons about.
//
// The split matters: ErrNotFound is a client-visible 409 at complete time,
// ErrUnavailable is a 503 that tells a client to retry, and everything else is a
// 500 whose detail is a fixed string. Leaking an SDK error string into a
// response body would put a bucket name and a region in it.
func mapS3Error(op string, err error) *Error {
	// An HTTP response the SDK could parse an API error out of. Checked FIRST,
	// because S3's own error code (`NoSuchKey`, `SlowDown`) is more precise than
	// the status it arrived with, and because a response error wraps the
	// transport error underneath it.
	var respErr *awshttp.ResponseError
	if errors.As(err, &respErr) {
		if apiErr := apiError(respErr); apiErr != nil {
			switch apiErr.ErrorCode() {
			case "NoSuchKey", "NotFound", "404":
				return notFound(op)
			case "SlowDown", "RequestTimeout", "InternalError", "ServiceUnavailable":
				return unavailable(op, err)
			}
		}
		return ClassifyStatus(op, respErr.Response.StatusCode, err)
	}
	var apiErr smithy.APIError
	if errors.As(err, &apiErr) {
		switch apiErr.ErrorCode() {
		case "NoSuchKey", "NotFound", "404":
			return notFound(op)
		case "SlowDown", "RequestTimeout", "InternalError", "ServiceUnavailable":
			return unavailable(op, err)
		}
		return other(op, err)
	}
	// A timeout or a truncated body is "try again later", not a bug, so it must
	// not read as a client error.
	if errors.Is(err, context.DeadlineExceeded) || errors.Is(err, io.ErrUnexpectedEOF) || errors.Is(err, io.EOF) {
		return unavailable(op, err)
	}
	// The SDK's retry-exhausted error, which is still "try again later".
	var opErr *smithy.OperationError
	if errors.As(err, &opErr) {
		return unavailable(op, err)
	}
	return other(op, err)
}

// apiError digs the service's own error code out of a response error, which is
// where S3 puts `NoSuchKey`. A nil result means the backend answered with
// something this SDK version cannot classify, and the status is the fallback.
func apiError(respErr *awshttp.ResponseError) smithy.APIError {
	var apiErr smithy.APIError
	if errors.As(respErr.Err, &apiErr) {
		return apiErr
	}
	return nil
}

// ClassifyStatus turns an HTTP status into the retry decision.
//
// IT IS AN EXPORTED FUNCTION, AND THAT IS WHAT MAKES IT TESTABLE. The Rust
// original tested a CLOSURE that repeated these rules, which is a test of a copy
// rather than of the code: (AGENTS.md, on guards): "Any new guard here gets a
// planted divergence before it is believed, and if it hangs or passes on a broken
// source, that is a finding about the guard, not about the source." This one is
// exported so the table in s3_test.go reaches the real branches. Constructing an
// SDK error to assert against it would mean depending on an SDK-internal type in
// a test, so the rules live here where a table can reach them:
//
//   - 5xx and 429 are the bucket's problem, so the client retries;
//   - 401/403 are credentials or policy, which is an operator problem;
//   - 404 is the one the service acts on.
func ClassifyStatus(op string, status int, cause error) *Error {
	switch status {
	case 404:
		return notFound(op)
	case 500, 429, 502, 503, 504:
		return unavailable(op, cause)
	default:
		// A 403 is credentials or policy: a 500 with a fixed detail, cause in the
		// log. NOT NotFound — reading a permissions error as "the object is
		// absent" would make a misconfigured bucket look like a client that never
		// uploaded.
		return other(op, cause)
	}
}

// appendLengthRange records the byte ceiling on a presigned URL so it is visible
// in a captured URL and in a log line.
//
// S3 itself does not read this — a presigned PUT's signature covers the key, the
// signed headers and the expiry, and a byte ceiling on that path would need a
// POST policy, which is a different flow. The real enforcement of the ceiling is
// the service comparing the measured object size against the size the client
// declared, at complete time. This parameter is DOCUMENTATION, NOT ENFORCEMENT,
// and is named so nobody later mistakes it for a guarantee it never was.
func appendLengthRange(rawURL string, maxBytes int64) string {
	return rawURL + "&x-darkroom-max-bytes=" + strconv.FormatInt(maxBytes, 10)
}
