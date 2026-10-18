// Package objectstore is darkroom's object storage boundary.
//
// # THE BOUNDARY, STATED
//
// darkroom's HTTP layer never learns whether the bytes live in a map or in a
// bucket. It holds a [Store] and calls five methods: presign, head, get, put,
// delete. Everything above this line is policy — tenant scoping, checksum
// verification, status transitions, the outbox — and everything below is
// transport. There is no "if s3 then" above the line, and no policy below it.
//
// The two implementations are [Memory] (always compiled, used by every test and
// by `DARKROOM_OBJECT_STORE=memory` in development) and [S3] (the same
// implementation whether the bucket is AWS S3, MinIO, Ceph or Cloudflare R2).
//
// # ONE IMPLEMENTATION, SEVERAL S3-COMPATIBLE SERVICES
//
// AWS S3, MinIO and Cloudflare R2 differ in ways that each bit somebody once.
// They are handled in [S3] and in the config layer that builds it, never in a
// second implementation and never in a branch on a backend name:
//
//   - R2's region is `auto`. `us-east-1` and the empty string alias to it, but
//     SigV4 has to be signed with `auto` or the request is rejected. An R2
//     endpoint is normalised to `auto` at startup, and an R2 endpoint paired
//     with a real region is refused there rather than failing on the first
//     upload.
//   - R2's endpoint is account-scoped (`https://<ACCOUNT_ID>.r2
//     .cloudflarestorage.com`) and there is no global one, so it is
//     configuration and never a constant in this file.
//   - R2 has no `FULL_OBJECT` SHA-256. Its compatibility table offers
//     `COMPOSITE` for SHA-256 and `FULL_OBJECT` for CRC-64/NVME only, so a
//     checksum read back from a header is not the object's sha256. This is why
//     [ObjectMeta] has no checksum field and why the complete path reads the
//     object back: a correctness property may not depend on which backend
//     answered.
//   - R2 rejects ACL, grant, object-lock and expected-bucket-owner headers.
//     darkroom never sets them and has no configuration that could, so there is
//     nothing to get wrong.
//
// The interface is why those are four lines of configuration rather than a
// second store: each difference is a *request-shaping* fact, and request
// shaping is entirely below this line. `AGENTS.md` states the rule this
// preserves — "One `ObjectStore` implementation, several S3-compatible
// services. R2 is not a variant, a feature or a second type: it is an endpoint,
// a region, and addressing. If a backend needs a *code* branch, the abstraction
// is wrong and fixing the abstraction is in scope — that is not a reason to add
// `R2ObjectStore`."
//
// # WHY THE INTERFACE IS AN INTERFACE
//
// The Rust tree boxed a vtable per object behind `async_trait` and the
// alternative it rejected in its own header comment was a generic
// `ObjectStore<S>`, because a generic would monomorphise every call site per
// implementation and put S3's error types into the compiler's inference for
// the in-memory path. Go takes neither escape: an interface method is already
// one dynamic dispatch and already returns the interface's own error type, so
// there is no boxing to justify and no generic to avoid. What that buys is the
// same thing it bought in Rust — the service's error surface is exactly [Error]
// and its sentinels, which is the thing tests assert on.
//
// # WHY PRESIGNING LIVES HERE AND NOT IN THE HANDLER
//
// A presigned URL is a credential: it is a bearer token for exactly one object
// key, valid for a fixed window. Which of those two facts the store enforces
// differs by backend — S3 does it with SigV4, the in-memory store does it with
// a signature over `(key, expiry)`. A handler that built URLs itself would have
// to know that, so the interface owns presigning and the handler only ever holds
// a finished URL. The TTL and the key are chosen by callers of
// [Store.PresignPut] passing both, never inferred.
//
// # WHY THE TRAIT IS []byte-SHAPED AND NOT A STREAM
//
// `Get` hands back a `[]byte`. A stream in the interface is a decision the
// interface would have to make on behalf of BOTH implementations, and the only
// caller that wants bounded memory — the complete-time verification read of a
// 1 GiB object — is served by `checksum.SHA256HexStreaming` over chunks, which
// is a property of the CHECKSUM and not of the transport. The Rust tree reached
// the same place with `bytes::Bytes` and put the bounded read in `checksum.rs`,
// which is where this one puts it too.
//
// What it costs is stated rather than hidden: `Get` against a real 1 GiB object
// is one contiguous 1 GiB allocation. `AGENTS.md` makes the trade explicit and
// accepts it — "the one read is the trade stated in the README" — for the
// checksum this read exists to compute. A packet that needs a bounded read adds
// a ranged method rather than changing what every implementation of this
// interface means.
package objectstore

import (
	"context"
	"errors"
	"fmt"
	"time"
)

// PresignTTL is how long a presigned PUT is valid.
//
// DELIBERATELY SHORT, and the number is in one place because it is a security
// property, not a tuning knob: "a URL that lives for an hour is a URL that can
// be found in a proxy log, a browser history, or a Referer header and replayed
// for the rest of that hour" (`src/objectstore/mod.rs`). Fifteen minutes is the
// same rule.
const PresignTTL = 900 * time.Second // 15 minutes

// MaxUploadBytes is the cap on a single upload.
//
// S3 has a 5 GiB single-PUT limit and this is well under it, so "presigned PUT"
// never becomes "multipart upload" — which would be a second flow, a second set
// of presigned URLs, and a second thing to get wrong. A request above this is
// a 422 before any URL is issued.
const MaxUploadBytes int64 = 1024 * 1024 * 1024 // 1 GiB

// ObjectMeta is what the store says about an object: its size, its type, and
// nothing else.
//
// # THERE IS DELIBERATELY NO CHECKSUM FIELD
//
// It was there, and it was the defect `src/objectstore/mod.rs` was changed for.
// The complete path used to ask the backend what checksum it had recorded and
// compare the client's claim against that answer, which is a correctness
// property that exists on AWS S3 and does not exist on Cloudflare R2: R2's S3
// compatibility table offers `FULL_OBJECT` for CRC-64/NVME only, so for SHA-256
// the answer is either absent or a `COMPOSITE` value that is not the object's
// sha256. A field that is set on one backend and unset on another is a field
// whose absence silently disables a check.
//
// Size and content type stayed because they are measurements every
// S3-compatible service reports honestly. A checksum is not a measurement: it
// is a backend's *claim* about bytes, and this service makes its own.
// `AGENTS.md`: "The checksum is computed from the bytes, never from a backend's
// claim about them."
type ObjectMeta struct {
	Key string
	// ByteSize is the measured length, not the length the client declared.
	ByteSize    int64
	ContentType string
}

// PresignedPut is a finished, ready-to-use presigned URL.
type PresignedPut struct {
	// URL is the absolute URL the client PUTs to.
	URL string
	// Key is the one key this URL is scoped to. Also returned to the client so
	// it can see what it is about to write to; never used to build another URL.
	Key string
	// ExpiresIn is seconds until expiry, so the client can schedule its own PUT.
	ExpiresIn int64
}

// Sentinels for the three failures the service reasons about.
//
// `AGENTS.md` asks for exactly this shape on the other side of the wire: "The
// rule is absence, not refusal. […] `store::find_asset` returns `Ok(None)` for
// another account's row, never a `StoreError`, because a distinguishable error
// is the same oracle one layer down." So [ErrNotFound] is a value callers
// compare with errors.Is rather than a detail string they pattern-match, and
// there is nothing else in this list that can be confused with it.
var (
	// ErrNotFound means the key is not there. A client-visible state at complete
	// time (409) and a normal result for Head.
	ErrNotFound = errors.New("object not found")

	// ErrTooLarge means the backend refused because the bytes are bigger than the
	// URL's scope. A presigned URL is scoped to one key AND one content length;
	// this is the error a client gets when it ignores the second half of that.
	ErrTooLarge = errors.New("object is larger than the presigned scope allows")

	// ErrUnavailable means the backend is not answering. 503, never 500: the
	// request is fine and retrying it later is the correct client behavior.
	ErrUnavailable = errors.New("object storage unavailable")
)

// Error is the store's error surface: one of the sentinels above, a cause, and
// the operation that produced it.
//
// It is a wrapped sentinel rather than a bare one so that a caller can ask
// `errors.Is(err, ErrNotFound)` without giving up the cause, and so that the
// operation name reaches a log without reaching a response body. `AGENTS.md`
// holds both halves of that rule: probe failures are "logged, never returned in
// detail", and an SDK error string would put a bucket name and a region in a
// client-visible message.
type Error struct {
	// Op is the method that failed: "presign_put", "head", "get", "put",
	// "delete", "apply_presigned_put".
	Op string
	// Err is the sentinel this error IS, or the cause it wraps.
	Err error
	// Detail is an operator-facing note. It never reaches a response body.
	Detail string
}

func (e *Error) Error() string {
	msg := fmt.Sprintf("objectstore.%s: %v", e.Op, e.Err)
	if e.Detail != "" {
		msg += ": " + e.Detail
	}
	return msg
}

// Unwrap is what makes errors.Is and errors.As reach the sentinel.
func (e *Error) Unwrap() error { return e.Err }

// notFound, tooLarge and unavailable build the three sentinels. They exist so
// that the sentinel is always wrapped — never returned bare — because a bare
// sentinel loses the operation name, and the operation name is what a log line
// is read for.
func notFound(op string) *Error { return &Error{Op: op, Err: ErrNotFound} }
func tooLarge(op string) *Error { return &Error{Op: op, Err: ErrTooLarge} }
func unavailable(op string, cause error) *Error {
	return &Error{Op: op, Err: ErrUnavailable, Detail: cause.Error()}
}

func other(op string, cause error) *Error {
	return &Error{Op: op, Err: fmt.Errorf("%w: %w", errOther, cause), Detail: cause.Error()}
}

// errOther is the inner sentinel for "anything else": 500 with a fixed detail,
// the cause in the log. It is not exported, because there is no caller that
// should be branching on it — `AGENTS.md` on the HTTP layer's error mapping:
// everything not NotFound/TooLarge/Unavailable is a 500 whose detail is fixed.
var errOther = errors.New("object store error")

// Store is the five operations this service needs from object storage, and no
// more.
//
// A method with one too many is a method that is hard to implement, and the
// in-memory store is the thing that keeps the tests fast — so this is exactly
// the set the service uses and nothing speculative.
//
// `context.Context` is the first parameter on every method because that is what
// a Go fleet does with anything that can block on IO, and every one of these
// can.
type Store interface {
	// PresignPut mints a URL that authorizes a PUT of at most maxBytes to key,
	// for contentType, expiring in ttl.
	//
	// The scope is BOTH halves of that sentence: exactly one key, and a byte
	// ceiling. A URL that permits any key is a URL that permits overwriting
	// another tenant's object, which is the whole thing tenant isolation is
	// for.
	PresignPut(ctx context.Context, key, contentType string, maxBytes int64, ttl time.Duration) (PresignedPut, error)

	// Head reports what is at key, or ErrNotFound. The measured size and content
	// type, and nothing about the bytes themselves.
	//
	// Head is how "the upload never landed" is answered without downloading a
	// gigabyte to discover the gigabyte is not there. It is not, and cannot be,
	// evidence about content: a backend that reports the right size, the right
	// type and a wrong checksum is a real backend on a real day.
	Head(ctx context.Context, key string) (ObjectMeta, error)

	// Get returns the bytes at key. The only way this service learns what
	// storage actually holds, and therefore the only basis for a checksum
	// decision.
	//
	// Used at complete time to verify the client's claim against the bytes, and
	// by variant generation to read the original. That complete-time read is not
	// an optimisation left over from a fallback path: it is the whole
	// verification. See [ObjectMeta] for why nothing above this line may ask the
	// backend for a checksum instead.
	Get(ctx context.Context, key string) ([]byte, error)

	// Put writes b at key. Used by variant generation to store a derived image,
	// never by the client upload path — the client writes through the presigned
	// URL, not through this service. That is what keeps a 1 GiB upload from
	// being proxied through the API process.
	Put(ctx context.Context, key string, b []byte, contentType string) error

	// Delete removes key. Deleting something that is already gone succeeds:
	// delete is idempotent because `DELETE /v1/assets/{id}` is retryable.
	Delete(ctx context.Context, key string) error
}
