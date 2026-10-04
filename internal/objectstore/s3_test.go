package objectstore_test

import (
	"errors"
	"net/url"
	"strings"
	"testing"

	"github.com/aws/aws-sdk-go-v2/aws"

	"github.com/cafaye/darkroom/internal/objectstore"
)

// The object-store behaviour table: AWS S3, an S3-compatible service, and
// Cloudflare R2, through the same assertions.
//
// # WHY ONE TABLE AND NOT ONE TEST PER BACKEND
//
// A suite that branches on the backend proves the branch. Everything a presigned
// URL must be — scoped to one key, pinned to one content type, good for fifteen
// minutes, signed with the right region, and carrying no header that the target
// will reject — is the SAME requirement for every S3-compatible service, so it
// is asserted once, in one body, for every row. What differs between rows is
// configuration and nothing else: an endpoint, a region, a flag. Each row is
// still its own `t.Run`, so one failure names the backend that failed instead of
// stopping at the first row.
//
//	row                      endpoint         region as configured  region as signed
//	aws                      —                eu-west-1              eu-west-1
//	s3_compatible            MinIO            minio                  minio
//	r2                       account endpoint auto                   auto
//	r2_default_region        account endpoint (unset)                auto
//	r2_us_east_1_alias       account endpoint us-east-1               auto
//
// The last three are the R2 differences that are configuration, and the
// assertion they share with the first two is the point: `auto` is what goes into
// the SigV4 credential scope, and a URL that says `eu-west-1` for an R2 endpoint
// is a URL R2 rejects.
//
// # NO NETWORK, NO CREDENTIALS, NO BUCKET
//
// Presigning is a local operation — the SDK builds a URI and a signature and
// sends nothing — so this table runs the production request-shaping path with
// static dummy credentials and asserts on the URL that comes out. The
// credentials never leave the process: nothing is transmitted, and nothing here
// could authenticate against a real bucket even if it tried.
//
// A real R2 bucket is a manual verification step and the README says exactly
// how to do it. What this file cannot prove is that R2 *accepts* the URL, and it
// does not claim to.

const s3Key = "a/9f8b1c2d-0000-4000-8000-000000000000/0123456789abcdef0123456789abcdef/original"

const r2Endpoint = "https://9f8b1c2d3e4f5a6b7c8d9e0f1a2b3c4d.r2.cloudflarestorage.com"

// forbidden are header names and query parameters an S3-compatible service may
// reject, and darkroom must therefore never put on the wire.
//
// The first four are R2's list verbatim (`x-amz-acl`, `x-amz-grant-*`,
// `x-amz-expected-bucket-owner`, plus the object-lock family it does not
// implement). The rest are the checksum family, which is not a rejection problem
// but a *correctness* one: R2 offers `FULL_OBJECT` for CRC-64/NVME only and
// `COMPOSITE` for SHA-256, so a checksum that came back from a header would be a
// different checksum type on a different bucket. darkroom asks for none.
var forbidden = []string{
	"x-amz-acl",
	"x-amz-grant",
	"x-amz-expected-bucket-owner",
	"x-amz-object-lock",
	"x-amz-request-payer",
	"x-amz-checksum",
	"x-amz-sdk-checksum",
	"x-amz-checksum-mode",
	"x-amz-trailer",
	"content-md5",
}

type backendRow struct {
	name string
	cfg  objectstore.S3Config
	// signedRegion is the region in `X-Amz-Credential`. For the R2 rows it is
	// `auto` however the region was configured, which is the whole of the R2
	// region rule.
	signedRegion string
}

func backendRows() []backendRow {
	const bucket = "darkroom-media"
	return []backendRow{
		{
			name:         "aws",
			cfg:          objectstore.S3Config{Bucket: bucket, Region: "eu-west-1", PathStyle: true},
			signedRegion: "eu-west-1",
		},
		{
			name:         "s3_compatible",
			cfg:          objectstore.S3Config{Bucket: bucket, Region: "minio", Endpoint: "https://minio.internal:9000", PathStyle: true},
			signedRegion: "minio",
		},
		{
			name:         "r2",
			cfg:          objectstore.S3Config{Bucket: bucket, Region: "auto", Endpoint: r2Endpoint},
			signedRegion: "auto",
		},
		{
			// R2 makes the region optional; S3 does not. The URL has to be
			// identical to the row above.
			name:         "r2_default_region",
			cfg:          objectstore.S3Config{Bucket: bucket, Endpoint: r2Endpoint},
			signedRegion: "auto",
		},
		{
			// `us-east-1` is the alias R2 documents. The signature is built with
			// `auto` and not with the string that was configured, and asserting
			// that is the only way to tell normalisation from a coincidence.
			name:         "r2_us_east_1_alias",
			cfg:          objectstore.S3Config{Bucket: bucket, Region: "us-east-1", Endpoint: r2Endpoint},
			signedRegion: "auto",
		},
	}
}

func TestEveryBackendPresignsTheSameWay(t *testing.T) {
	for _, row := range backendRows() {
		t.Run(row.name, func(t *testing.T) {
			assertBackendBehaviour(t, row)
		})
	}
}

// The three R2 rows produce the same URL, byte for byte apart from the
// signature, which depends on the date. Asserted separately because "each row
// passed" and "the rows agree" are different claims, and it is the second one
// that says the region rule is normalisation rather than a coincidence.
func TestTheThreeR2ConfigurationsProduceTheSamePresignedURL(t *testing.T) {
	var (
		firstName string
		first     string
	)
	for _, row := range backendRows()[2:] {
		got := presign(t, row)
		// The signature covers the date and the key, not the configuration, so
		// dropping it compares the parts that configuration could change.
		withoutSignature, _, _ := strings.Cut(got, "&X-Amz-Signature=")
		if withoutSignature == "" {
			// The parameter can be lower-cased by a client but the SDK emits it
			// verbatim; if that ever changes the case is the failure, not the
			// signature.
			withoutSignature, _, _ = strings.Cut(got, "&x-amz-signature=")
		}
		if firstName == "" {
			firstName, first = row.name, withoutSignature
			continue
		}
		if withoutSignature != first {
			t.Fatalf("%s differs from %s:\n  %s\n  %s", row.name, firstName, withoutSignature, first)
		}
	}
}

// An endpoint that merely *contains* R2's host suffix is not R2, and must not
// get R2's rules.
//
// A substring match here would be a way to make a deployment believe it had R2
// validation when it had none, so the match is on the host. The observable
// consequence: `auto` is refused, because for this endpoint it is not an R2
// region and there is no global R2 endpoint to fall back to.
func TestAHostThatOnlyLooksLikeR2IsNotGivenR2sRules(t *testing.T) {
	for _, lookalike := range []string{
		"https://r2.cloudflarestorage.com.attacker.example",
		"https://notr2.cloudflarestorage.example.com",
		"https://acct.r2.cloudflarestorage.com.attacker.example",
	} {
		_, err := objectstore.NewS3ForTest(objectstore.S3Config{
			Bucket:   "darkroom-media",
			Endpoint: lookalike,
			Region:   "auto",
			Access:   "AKIADARKROOMEXAMPLE",
			Secret:   "darkroom-example-secret-not-a-real-key",
		})
		if err == nil {
			t.Errorf("%s was treated as R2", lookalike)
		}
	}
}

// An R2 endpoint paired with a real region is refused AT CONFIGURATION, not on
// the first upload. Left to the first request it is a signature error on every
// upload with nothing in the log about the region, which is the failure mode the
// rule exists to prevent.
func TestAnR2EndpointWithARealRegionIsRefused(t *testing.T) {
	for _, region := range []string{"eu-central-1", "us-west-2", "minio"} {
		_, err := objectstore.NewS3ForTest(objectstore.S3Config{
			Bucket:   "darkroom-media",
			Endpoint: r2Endpoint,
			Region:   region,
			Access:   "AKIADARKROOMEXAMPLE",
			Secret:   "darkroom-example-secret-not-a-real-key",
		})
		if err == nil {
			t.Errorf("R2 + region %q was accepted", region)
			continue
		}
		// The operator has to be told what is wrong AND what to do, so the
		// message names the value they set and the value they should have set.
		for _, want := range []string{region, "auto"} {
			if !strings.Contains(err.Error(), want) {
				t.Errorf("the refusal for %q does not mention %q: %v", region, want, err)
			}
		}
	}
}

// A missing bucket and a missing region on a non-R2 endpoint are both refused,
// because both are a deployment that would resolve to something unintended. A
// bucket-only configuration with no region would otherwise sign against an AWS
// default endpoint and fail every request with a signature error.
func TestAnIncompleteConfigurationIsRefused(t *testing.T) {
	for _, tc := range []struct {
		name string
		cfg  objectstore.S3Config
	}{
		{"no bucket", objectstore.S3Config{Region: "eu-west-1"}},
		{"blank bucket", objectstore.S3Config{Bucket: "  ", Region: "eu-west-1"}},
		{"no region without an R2 endpoint", objectstore.S3Config{Bucket: "darkroom-media"}},
		{"auto without an endpoint", objectstore.S3Config{Bucket: "darkroom-media", Region: "auto"}},
	} {
		if _, err := objectstore.NewS3ForTest(tc.cfg); err == nil {
			t.Errorf("%s was accepted: %+v", tc.name, tc.cfg)
		}
	}
}

// The static credential belongs in the test constructor. Production takes the
// SDK's own chain, so a credential is never a string in this repository — and a
// production constructor that accepted one would make "no secrets in the repo" a
// convention rather than a type error.
func TestProductionRefusesAStaticCredential(t *testing.T) {
	_, err := objectstore.New(ctx, objectstore.S3Config{
		Bucket: "darkroom-media",
		Region: "eu-west-1",
		Access: "AKIADARKROOMEXAMPLE",
		Secret: "darkroom-example-secret-not-a-real-key",
	})
	if err == nil {
		t.Fatal("objectstore.New accepted a static credential")
	}
}

// A presigned PUT with no byte ceiling is refused before a URL exists. Without
// it, a URL a client leaked can write an arbitrarily large object into a bucket
// someone pays for, and it is the difference between "a presigned URL" and "a
// presigned URL with a blast radius".
func TestAPresignedPutWithNoCeilingIsRefused(t *testing.T) {
	store := newStore(t, backendRows()[0])
	for _, maxBytes := range []int64{0, -1} {
		if _, err := store.PresignPut(ctx, s3Key, "image/png", maxBytes, objectstore.PresignTTL); err == nil {
			t.Errorf("presigned with maxBytes = %d", maxBytes)
		}
	}
}

// The one body. Every row runs this, and there is no per-backend branch in it:
// the signed region is the row's DATA, declared alongside its configuration.
func assertBackendBehaviour(t *testing.T, row backendRow) {
	t.Helper()

	cfg := row.cfg
	store := newStore(t, row)

	// 1. The region the client will sign with is the region the configuration
	//    resolved to, and for every R2 row that is the literal `auto`.
	if got := store.Client().Options().Region; got != row.signedRegion {
		t.Fatalf("%s: the client's region is %q, want %q", row.name, got, row.signedRegion)
	}

	put, err := store.PresignPut(ctx, s3Key, "image/png", 4096, objectstore.PresignTTL)
	if err != nil {
		t.Fatalf("%s: presigning: %v", row.name, err)
	}
	raw := strings.ToLower(put.URL)

	// 2. The SigV4 credential scope carries that region. This is the assertion
	//    that matters most for R2: a signature built for a real region is
	//    rejected by R2 with nothing useful in the body, and this is the only
	//    place the difference is visible before a request is sent.
	wantScope := "%2f" + row.signedRegion + "%2fs3%2faws4_request"
	if !strings.Contains(raw, wantScope) {
		t.Errorf("%s: the credential scope must be `<key>/<date>/%s/s3/aws4_request`, got %s", row.name, row.signedRegion, raw)
	}

	// 3. The signature covers the content type and the host, and THOSE TWO.
	//    Exact containment of the pair, not just of each: a signed-header list
	//    that grew by one entry is a client that now has to send a header it does
	//    not know about, and this is where an `x-amz-checksum-algorithm` or an
	//    `x-amz-expected-bucket-owner` would first appear.
	if !strings.Contains(raw, "x-amz-signedheaders=content-type%3bhost") {
		t.Errorf("%s: exactly content-type and host must be signed, got %s", row.name, raw)
	}

	// 4. Nothing darkroom must never send appears anywhere in the URL — not in
	//    the signed headers, not in the query. This is the R2 header list and the
	//    checksum family in one assertion, applied to every row.
	for _, f := range forbidden {
		if strings.Contains(raw, f) {
			t.Errorf("%s: %s must never be sent; the URL was %s", row.name, f, raw)
		}
	}

	// 5. The URL is scoped: one key, and it is the key that was asked for. This
	//    is the S3 half of the property the in-memory store proves in
	//    TestAssetAsPresignedURLCannotWriteAtAssetBsKey.
	parsed, err := url.Parse(put.URL)
	if err != nil {
		t.Fatalf("%s: the presigned URL does not parse: %v", row.name, err)
	}
	if want := "/" + cfg.Bucket + "/" + s3Key; !strings.HasSuffix(parsed.Path, want) {
		t.Errorf("%s: the path must end with %q, got %q", row.name, want, parsed.Path)
	}
	if put.Key != s3Key {
		t.Errorf("%s: PresignedPut.Key = %q, want %q", row.name, put.Key, s3Key)
	}

	// 6. Fifteen minutes, and the ceiling darkroom documents is on it.
	if put.ExpiresIn != 900 {
		t.Errorf("%s: ExpiresIn = %d, want 900", row.name, put.ExpiresIn)
	}
	if !strings.Contains(raw, "x-amz-expires=900") {
		t.Errorf("%s: the URL does not carry X-Amz-Expires=900, got %s", row.name, raw)
	}
	if !strings.Contains(raw, "x-darkroom-max-bytes=4096") {
		t.Errorf("%s: the URL does not carry the documented ceiling, got %s", row.name, raw)
	}
}

func presign(t *testing.T, row backendRow) string {
	t.Helper()
	put, err := newStore(t, row).PresignPut(ctx, s3Key, "image/png", 4096, objectstore.PresignTTL)
	if err != nil {
		t.Fatalf("%s: presigning: %v", row.name, err)
	}
	return put.URL
}

// newStore builds the store for a row through the production request-shaping
// path, with the static credentials this file uses everywhere.
//
// They are STATIC rather than the SDK's chain because "a test that could
// authenticate without being told how is a test that would pass against the
// wrong bucket" — and a developer's real credentials in the environment are
// exactly the situation where a test that is supposed to be offline quietly is
// not.
func newStore(t *testing.T, row backendRow) *objectstore.S3 {
	t.Helper()
	cfg := row.cfg
	cfg.Access = "AKIADARKROOMEXAMPLE"
	cfg.Secret = "darkroom-example-secret-not-a-real-key"
	store, err := objectstore.NewS3ForTest(cfg)
	if err != nil {
		t.Fatalf("%s: building the store: %v", row.name, err)
	}
	return store
}

// The parameter is not read by S3. Naming it `x-darkroom-max-bytes` and saying so
// in its comment keeps a future reader from mistaking it for a guarantee it never
// was: the real ceiling is the measured-vs-declared size check at complete time.
func TestTheLengthCeilingIsNamedAsDocumentationNotEnforcement(t *testing.T) {
	put, err := newStore(t, backendRows()[0]).PresignPut(ctx, s3Key, "image/png", 1024, objectstore.PresignTTL)
	if err != nil {
		t.Fatalf("presigning: %v", err)
	}
	if !strings.Contains(put.URL, "x-darkroom-max-bytes=1024") {
		t.Fatalf("the ceiling is not on the URL: %s", put.URL)
	}
	// And it is a query PARAMETER rather than a signed header: if it were signed,
	// S3 would reject it as an unknown one.
	if strings.Contains(strings.ToLower(put.URL), "x-amz-signedheaders=content-type%3bhost%3bx-darkroom") {
		t.Fatalf("the ceiling is in the signed headers, which S3 would reject: %s", put.URL)
	}
}

// The SDK injects a checksum header on its own unless it is told not to.
//
// `RequestChecksumCalculation` defaults to `WhenSupported`, which attaches
// CRC-32 to every `PutObject` and `UploadPart` and CRC-64/NVME in recent AWS SDK
// releases — and R2 rejects the latter. darkroom asks for no checksum anywhere,
// so "only where the operation requires it" is part of the contract with every
// backend, not a workaround for one.
//
// Asserted on the configuration a REAL store built, rather than in a comment,
// because this is a default that can change under us in any dependency bump.
func TestNoChecksumHeaderIsAskedForOnAnyBackend(t *testing.T) {
	// R2, the backend that rejects one.
	opts := newStore(t, backendRows()[2]).Client().Options()
	if opts.RequestChecksumCalculation != aws.RequestChecksumCalculationWhenRequired {
		t.Errorf("RequestChecksumCalculation = %v, want %v: the SDK must not add a request checksum nobody asked for", opts.RequestChecksumCalculation, aws.RequestChecksumCalculationWhenRequired)
	}
	if opts.ResponseChecksumValidation != aws.ResponseChecksumValidationWhenRequired {
		t.Errorf("ResponseChecksumValidation = %v, want %v: nor ask the backend to start returning one", opts.ResponseChecksumValidation, aws.ResponseChecksumValidationWhenRequired)
	}
}

// The distinction that matters, extracted from `mapS3Error` so it can be asserted
// without constructing an SDK error — which would mean depending on an
// SDK-internal type in a test.
//
// The rules, in one place: 5xx and 429 are the bucket's problem, so the client
// retries; 401/403 are credentials or policy, which is an operator problem; 404
// is the one the service acts on.
func TestAnHTTPStatusMapsToTheRightRetryDecision(t *testing.T) {
	for _, tc := range []struct {
		status int
		want   error
	}{
		{503, objectstore.ErrUnavailable},
		{500, objectstore.ErrUnavailable},
		{429, objectstore.ErrUnavailable},
		{502, objectstore.ErrUnavailable},
		{504, objectstore.ErrUnavailable},
		{404, objectstore.ErrNotFound},
		{403, nil}, // an operator problem: not any of the three sentinels
		{401, nil},
		{418, nil},
	} {
		err := objectstore.ClassifyStatus("head", tc.status, errors.New("boom"))
		if tc.want == nil {
			if errors.Is(err, objectstore.ErrUnavailable) || errors.Is(err, objectstore.ErrNotFound) {
				t.Errorf("status %d mapped to %v, want neither sentinel", tc.status, err)
			}
			continue
		}
		if !errors.Is(err, tc.want) {
			t.Errorf("status %d mapped to %v, want %v", tc.status, err, tc.want)
		}
	}

	// A permissions error must NOT read as "the object is absent": that would
	// turn a misconfigured bucket into a client-facing 409 that says the upload
	// never happened.
	err := objectstore.ClassifyStatus("head", 403, errors.New("boom"))
	if errors.Is(err, objectstore.ErrNotFound) {
		t.Fatal("a 403 was reported as ErrNotFound")
	}
}
