package objectstore_test

import (
	"bytes"
	"context"
	"errors"
	"net/url"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/cafaye/darkroom/internal/objectstore"
	"github.com/cafaye/darkroom/internal/storagekey"
)

var ctx = context.Background()

const (
	keyA = "a/3f1b2c4d-5e6f-4a7b-8c9d-0e1f2a3b4c5d/0123456789abcdef0123456789abcdef/original"
	keyB = "a/3f1b2c4d-5e6f-4a7b-8c9d-0e1f2a3b4c5d/fedcba9876543210fedcba9876543210/original"
)

// The headline property, and the reason the whole three-axis scope exists.
//
// A URL for asset A must not write at asset B's key. If it could, then a client
// that leaked one presigned URL — in a proxy log, a Referer header, a browser
// history — could overwrite any object in the bucket whose key it could name,
// and key predictability would be the only thing between two tenants. This is
// the storage-side half of the rule `AGENTS.md` states as "A URL that permits any
// key is a URL that permits overwriting another tenant's object."
func TestAssetAsPresignedURLCannotWriteAtAssetBsKey(t *testing.T) {
	s := objectstore.NewMemory()

	urlA, err := s.PresignPut(ctx, keyA, "image/png", 1024, objectstore.PresignTTL)
	if err != nil {
		t.Fatalf("presigning A: %v", err)
	}
	if _, err := s.PresignPut(ctx, keyB, "image/png", 1024, objectstore.PresignTTL); err != nil {
		t.Fatalf("presigning B: %v", err)
	}

	// The attack: take A's URL — signature, expiry and all — and rewrite the key
	// segment to B's. Everything else is untouched and still looks legitimate.
	rewritten := rewriteKey(t, urlA.URL, keyB)
	if rewritten == urlA.URL {
		t.Fatal("the rewrite changed nothing, so this test is not exercising anything")
	}

	// Refused. It must be refused at the SIGNATURE, before the length or the
	// content type, because the signature is the only thing covering the key.
	if err := s.ApplyPresignedPut(ctx, rewritten, bytes.Repeat([]byte{7}, 512), "image/png"); err == nil {
		t.Fatal("asset A's presigned URL wrote at asset B's key")
	}

	// And B still holds nothing, which is the assertion that distinguishes "the
	// write was refused" from "the write was refused and then something wrote
	// anyway".
	if _, err := s.Head(ctx, keyB); !errors.Is(err, objectstore.ErrNotFound) {
		t.Fatalf("after the refused cross-key write, head(B) = %v, want ErrNotFound", err)
	}
	// A is untouched too: a refused write must not have emptied A either.
	if _, err := s.Head(ctx, keyA); !errors.Is(err, objectstore.ErrNotFound) {
		t.Fatalf("after the refused cross-key write, head(A) = %v, want ErrNotFound", err)
	}

	// And the legitimate write still works, so the guard is not simply refusing
	// everything. A test that only asserted refusal would pass against a store
	// that refuses all writes.
	if err := s.ApplyPresignedPut(ctx, urlA.URL, bytes.Repeat([]byte{7}, 512), "image/png"); err != nil {
		t.Fatalf("A's own URL was refused: %v", err)
	}
	if _, err := s.Head(ctx, keyA); err != nil {
		t.Fatalf("head(A) after a legitimate write: %v", err)
	}
}

// THE THREE AXES OF SCOPE, and each is load-bearing, so each is its own case
// rather than one assertion with three clauses. The first half is what a
// presigned PUT authorises; the second half is what it must not.
func TestAPresignedPutIsScopedToOneKeyOneContentTypeAndOneLength(t *testing.T) {
	s := objectstore.NewMemory()

	put, err := s.PresignPut(ctx, keyA, "image/png", 1024, objectstore.PresignTTL)
	if err != nil {
		t.Fatalf("presigning: %v", err)
	}

	// AXIS ONE: exactly one key. The key is IN the URL and the URL verifies
	// against exactly that key — asserted by rewriting it in the test above, and
	// here by asserting the URL names what it was issued for.
	if put.Key != keyA {
		t.Errorf("PresignedPut.Key = %q, want %q", put.Key, keyA)
	}
	if decoded := decodedKey(t, put.URL); decoded != keyA {
		t.Errorf("the URL carries %q, want %q", decoded, keyA)
	}
	if !strings.Contains(put.URL, "%2F") {
		t.Errorf("the key was not escaped in the URL, so a / in it would make the URL ambiguous: %s", put.URL)
	}
	if put.ExpiresIn != 900 {
		t.Errorf("ExpiresIn = %d, want 900", put.ExpiresIn)
	}

	t.Run("under the cap with the right type is accepted", func(t *testing.T) {
		if err := s.ApplyPresignedPut(ctx, put.URL, bytes.Repeat([]byte{1}, 512), "image/png"); err != nil {
			t.Fatalf("a valid presigned PUT was refused: %v", err)
		}
		got, err := s.Get(ctx, keyA)
		if err != nil {
			t.Fatalf("get: %v", err)
		}
		if len(got) != 512 {
			t.Fatalf("stored %d bytes, want 512", len(got))
		}
	})

	t.Run("over the cap is refused with ErrTooLarge", func(t *testing.T) {
		err := s.ApplyPresignedPut(ctx, put.URL, bytes.Repeat([]byte{0}, 2048), "image/png")
		if !errors.Is(err, objectstore.ErrTooLarge) {
			t.Fatalf("over the cap: %v, want ErrTooLarge", err)
		}
	})

	t.Run("exactly the cap is accepted", func(t *testing.T) {
		// The boundary, because `>` against `>=` is the off-by-one this case
		// exists for and a round-number test would pass with either.
		if err := s.ApplyPresignedPut(ctx, put.URL, bytes.Repeat([]byte{2}, 1024), "image/png"); err != nil {
			t.Fatalf("a write of exactly the cap was refused: %v", err)
		}
	})

	t.Run("the wrong content type is refused", func(t *testing.T) {
		// A URL signed for image/png must not authorise storing an HTML document
		// at that key. Same bytes, same length, different type.
		err := s.ApplyPresignedPut(ctx, put.URL, bytes.Repeat([]byte{3}, 512), "text/html")
		if err == nil {
			t.Fatal("a URL signed for image/png stored a text/html document")
		}
		// And it must NOT be ErrTooLarge: the two are different client mistakes
		// and a 422 naming the wrong one teaches the client nothing.
		if errors.Is(err, objectstore.ErrTooLarge) {
			t.Fatalf("a content-type refusal reported as a size refusal: %v", err)
		}
		// Nor NotFound: the key is right there.
		if errors.Is(err, objectstore.ErrNotFound) {
			t.Fatalf("a content-type refusal reported the key as absent: %v", err)
		}
	})

	t.Run("a refused write does not change what is stored", func(t *testing.T) {
		got, err := s.Get(ctx, keyA)
		if err != nil {
			t.Fatalf("get: %v", err)
		}
		if !bytes.Equal(got, bytes.Repeat([]byte{2}, 1024)) {
			t.Fatal("a refused write changed the stored bytes")
		}
	})
}

// A forged URL is rejected. The tampering here is in the SIGNATURE, which is the
// half an attacker would change, and in the EXPIRY, which is the half they would
// extend.
func TestATamperedURLIsRefused(t *testing.T) {
	s := objectstore.NewMemory()
	put, err := s.PresignPut(ctx, keyA, "image/png", 1024, objectstore.PresignTTL)
	if err != nil {
		t.Fatalf("presigning: %v", err)
	}
	body := []byte("x")

	t.Run("a forged signature", func(t *testing.T) {
		tampered := strings.Replace(put.URL, "signature=", "signature=00", 1)
		if err := s.ApplyPresignedPut(ctx, tampered, body, "image/png"); err == nil {
			t.Fatal("a forged signature was accepted")
		}
	})

	t.Run("a truncated signature", func(t *testing.T) {
		tampered := put.URL[:strings.Index(put.URL, "signature=")+len("signature=")] + "deadbeef"
		if err := s.ApplyPresignedPut(ctx, tampered, body, "image/png"); err == nil {
			t.Fatal("a truncated signature was accepted")
		}
	})

	t.Run("an extended expiry", func(t *testing.T) {
		// The signature covers the expiry, so moving the expiry invalidates it.
		// A store that signed only the key would accept this.
		tampered := bumpExpires(t, put.URL, uint64(time.Now().Add(365*24*time.Hour).Unix()))
		if err := s.ApplyPresignedPut(ctx, tampered, body, "image/png"); err == nil {
			t.Fatal("a URL with an extended expiry was accepted")
		}
	})

	t.Run("an unknown query parameter", func(t *testing.T) {
		// Strict on purpose: a fake that ignores what it does not recognise would
		// accept a URL carrying a parameter it never agreed to.
		if err := s.ApplyPresignedPut(ctx, put.URL+"&x-darkroom-anything=1", body, "image/png"); err == nil {
			t.Fatal("a URL with an unrecognised query parameter was accepted")
		}
	})

	t.Run("a dropped signature", func(t *testing.T) {
		tampered := put.URL[:strings.Index(put.URL, "&signature=")]
		if err := s.ApplyPresignedPut(ctx, tampered, body, "image/png"); err == nil {
			t.Fatal("a URL with no signature was accepted")
		}
	})

	t.Run("a URL that is not one of ours", func(t *testing.T) {
		if err := s.ApplyPresignedPut(ctx, "https://evil.example/k?expires=1&signature=x", body, "image/png"); err == nil {
			t.Fatal("an https URL was accepted by the in-memory store")
		}
		if err := s.ApplyPresignedPut(ctx, "memory://darkroom/../escape?expires=1&signature=x", body, "image/png"); err == nil {
			t.Fatal("a traversal in the key path was accepted")
		}
	})

	t.Run("nothing was written", func(t *testing.T) {
		if _, err := s.Head(ctx, keyA); !errors.Is(err, objectstore.ErrNotFound) {
			t.Fatalf("after six refused URLs, head = %v, want ErrNotFound", err)
		}
		// Len counts objects that hold bytes, not key entries — PresignPut creates
		// an entry so it can record the grant — so this is the assertion that six
		// forged URLs stored nothing.
		if s.Len() != 0 {
			t.Fatalf("the store holds %d objects, want 0", s.Len())
		}
	})
}

// A zero TTL expires immediately, which is how this is tested without a sleep:
// "No sleeps, no raised retries, no loosened assertions. A test that needs an
// expired URL uses a zero-second TTL. […] None of them waits." (`AGENTS.md`)
func TestAnExpiredURLIsRefused(t *testing.T) {
	s := objectstore.NewMemory()
	put, err := s.PresignPut(ctx, keyA, "image/png", 1024, 0)
	if err != nil {
		t.Fatalf("presigning: %v", err)
	}
	if put.ExpiresIn != 0 {
		t.Fatalf("ExpiresIn = %d, want 0", put.ExpiresIn)
	}
	if err := s.ApplyPresignedPut(ctx, put.URL, []byte("x"), "image/png"); err == nil {
		t.Fatal("a zero-TTL URL was not already expired")
	}
}

// The distinction that makes `complete` correct: presigning creates a key, and an
// object that was never PUT to it must still read as MISSING. If it read as
// present, `complete` would accept an upload that never happened — the exact
// failure the signed-upload pattern exists to make impossible.
func TestAPresignedButUnwrittenKeyReadsAsAbsent(t *testing.T) {
	s := objectstore.NewMemory()
	if _, err := s.PresignPut(ctx, keyA, "image/png", 1024, objectstore.PresignTTL); err != nil {
		t.Fatalf("presigning: %v", err)
	}

	if _, err := s.Head(ctx, keyA); !errors.Is(err, objectstore.ErrNotFound) {
		t.Fatalf("head = %v, want ErrNotFound", err)
	}
	if _, err := s.Get(ctx, keyA); !errors.Is(err, objectstore.ErrNotFound) {
		t.Fatalf("get = %v, want ErrNotFound", err)
	}

	// Head on a key that was NEVER presigned reports the same thing. The two
	// cases are indistinguishable from outside, which is the point: a caller
	// must not be able to enumerate which keys have been presigned.
	never := "a/nobody/nothing/original"
	if _, errHead := s.Head(ctx, never); !errors.Is(errHead, objectstore.ErrNotFound) {
		t.Fatalf("head of an unknown key = %v, want ErrNotFound", errHead)
	}
	if _, errGet := s.Get(ctx, never); !errors.Is(errGet, objectstore.ErrNotFound) {
		t.Fatalf("get of an unknown key = %v, want ErrNotFound", errGet)
	}

	// And once something IS written it becomes visible, so this is not a store
	// that reports everything as absent.
	if err := s.ApplyPresignedPut(ctx, mustPresign(t, s, keyA), []byte("payload"), "image/png"); err != nil {
		t.Fatalf("apply: %v", err)
	}
	if _, err := s.Head(ctx, keyA); err != nil {
		t.Fatalf("head after a write: %v", err)
	}
}

// `head` reports the measurement and `get` reports the bytes, and the two cannot
// disagree: a size in `head` that is not the length of what `get` returned would
// mean the service verified one object and told a client about another.
// `ObjectMeta` carries no checksum, so there is nothing here to compare against
// except the bytes themselves.
func TestHeadAndGetAgreeAboutWhatIsStored(t *testing.T) {
	s := objectstore.NewMemory()
	payload := []byte("hello darkroom")
	if err := s.Put(ctx, keyA, payload, "text/plain"); err != nil {
		t.Fatalf("put: %v", err)
	}

	meta, err := s.Head(ctx, keyA)
	if err != nil {
		t.Fatalf("head: %v", err)
	}
	if meta.ByteSize != int64(len(payload)) {
		t.Errorf("ByteSize = %d, want %d", meta.ByteSize, len(payload))
	}
	if meta.ContentType != "text/plain" {
		t.Errorf("ContentType = %q, want text/plain", meta.ContentType)
	}
	if meta.Key != keyA {
		t.Errorf("Key = %q, want %q", meta.Key, keyA)
	}

	readBack, err := s.Get(ctx, keyA)
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if int64(len(readBack)) != meta.ByteSize {
		t.Fatalf("get returned %d bytes and head reported %d", len(readBack), meta.ByteSize)
	}
	if !bytes.Equal(readBack, payload) {
		t.Fatalf("get returned %q, want %q", readBack, payload)
	}

	// A HEAD must not download. `AGENTS.md` on why head exists at all: head is
	// "how `the upload never landed` is answered without downloading a gigabyte
	// to discover the gigabyte is not there", and a fake that cannot count would
	// make that claim untestable.
	if s.Stats().Gets != 1 || s.Stats().Heads != 1 {
		t.Fatalf("stats = %+v, want one get and one head", s.Stats())
	}
}

// Delete is idempotent, because `DELETE /v1/assets/:id` is retryable and a 500 on
// the second attempt would make a safe operation look broken.
func TestDeleteIsIdempotent(t *testing.T) {
	s := objectstore.NewMemory()
	if err := s.Put(ctx, keyA, []byte("x"), "text/plain"); err != nil {
		t.Fatalf("put: %v", err)
	}

	if err := s.Delete(ctx, keyA); err != nil {
		t.Fatalf("first delete: %v", err)
	}
	// The strong form: the store is empty, not merely missing this key.
	if !s.IsEmpty() || s.Len() != 0 {
		t.Fatalf("after one delete the store holds %d objects, want 0", s.Len())
	}
	if err := s.Delete(ctx, keyA); err != nil {
		t.Fatalf("second delete: %v", err)
	}
	if err := s.Delete(ctx, "a/never/existed/original"); err != nil {
		t.Fatalf("deleting a key that was never there: %v", err)
	}
	if _, err := s.Head(ctx, keyA); !errors.Is(err, objectstore.ErrNotFound) {
		t.Fatalf("head after delete = %v, want ErrNotFound", err)
	}
	if got := s.Stats().Deletes; got != 3 {
		t.Fatalf("Deletes = %d, want 3", got)
	}
}

// The signed-upload pattern is only worth anything if the client's bytes never
// pass through this service. A fake that could not count would let a
// proxying implementation pass every functional test.
func TestAClientsWriteNeverGoThroughPut(t *testing.T) {
	s := objectstore.NewMemory()
	put, err := s.PresignPut(ctx, keyA, "image/png", 1024, objectstore.PresignTTL)
	if err != nil {
		t.Fatalf("presigning: %v", err)
	}
	if s.Stats().Puts != 0 {
		t.Fatalf("presigning counted as a service Put: %+v", s.Stats())
	}
	if err := s.ApplyPresignedPut(ctx, put.URL, []byte("payload"), "image/png"); err != nil {
		t.Fatalf("apply: %v", err)
	}
	if s.Stats().Puts != 0 {
		t.Fatalf("a client write counted as a service Put: %+v", s.Stats())
	}
	if s.Stats().Presigns != 1 {
		t.Fatalf("Presigns = %d, want 1", s.Stats().Presigns)
	}
}

// The error surface, asserted through `errors.Is` rather than through strings.
// `AGENTS.md`: a distinguishable error is the oracle one layer down, so the
// distinctions have to be exactly these three and the operation name has to
// travel with them.
func TestErrorsAreWrappedSentinelsCarryingTheOperation(t *testing.T) {
	s := objectstore.NewMemory()

	_, err := s.Head(ctx, "a/nobody/nothing/original")
	if !errors.Is(err, objectstore.ErrNotFound) {
		t.Fatalf("head of an absent key: %v, want ErrNotFound", err)
	}
	if !strings.Contains(err.Error(), "objectstore.head") {
		t.Fatalf("the error does not name the operation: %v", err)
	}
	// Absent is NOT unavailable and NOT too large. Conflating them would turn a
	// completed upload into a retry loop.
	if errors.Is(err, objectstore.ErrUnavailable) || errors.Is(err, objectstore.ErrTooLarge) {
		t.Fatalf("an absent key reported as %v", err)
	}

	_, err = s.Get(ctx, "a/nobody/nothing/original")
	if !errors.Is(err, objectstore.ErrNotFound) {
		t.Fatalf("get of an absent key: %v, want ErrNotFound", err)
	}
}

// Both implementations have to satisfy the same interface, and this is the test
// that says so out loud rather than leaving it to whichever service is written
// first. `AGENTS.md` on the abstractions in this repository: the HTTP layer "holds
// an `Arc<dyn ObjectStore>`", and in Go that is a `Store` value.
func TestBothImplementationsSatisfyTheInterface(t *testing.T) {
	var stores []objectstore.Store
	stores = append(stores, objectstore.NewMemory())

	s3, err := objectstore.NewS3ForTest(objectstore.S3Config{
		Bucket:  "darkroom-media",
		Region:  "us-east-1",
		Access:  "test-access-key",
		Secret:  "test-secret-key",
		Retries: 0,
	})
	if err != nil {
		t.Fatalf("building an S3 store: %v", err)
	}
	stores = append(stores, s3)

	for _, st := range stores {
		if st == nil {
			t.Fatal("a nil store in the list")
		}
	}
}

// The generated keys this store is handed in production come from storagekey, so
// the round trip presign → write → read is asserted on real generated keys rather
// than on a hand-written one. A hand-written key is a key nobody has checked for
// URL-hostile characters.
func TestGeneratedKeysSurviveTheURLRoundTrip(t *testing.T) {
	s := objectstore.NewMemory()
	for i := 0; i < 50; i++ {
		key := storagekey.GenerateOriginal("3f1b2c4d-5e6f-4a7b-8c9d-0e1f2a3b4c5d").String()
		put, err := s.PresignPut(ctx, key, "image/png", 1024, objectstore.PresignTTL)
		if err != nil {
			t.Fatalf("presigning %s: %v", key, err)
		}
		if got := decodedKey(t, put.URL); got != key {
			t.Fatalf("the URL carries %q, want %q", got, key)
		}
		if err := s.ApplyPresignedPut(ctx, put.URL, []byte("payload"), "image/png"); err != nil {
			t.Fatalf("applying %s: %v", key, err)
		}
		meta, err := s.Head(ctx, key)
		if err != nil {
			t.Fatalf("head %s: %v", key, err)
		}
		if meta.Key != key {
			t.Fatalf("head reported key %q, want %q", meta.Key, key)
		}
	}
}

func mustPresign(t *testing.T, s objectstore.Store, key string) string {
	t.Helper()
	put, err := s.PresignPut(ctx, key, "image/png", 1024, objectstore.PresignTTL)
	if err != nil {
		t.Fatalf("presigning %s: %v", key, err)
	}
	return put.URL
}

// rewriteKey returns rawURL with its key segment replaced, keeping the query —
// and therefore the signature and the expiry — exactly as they were.
func rewriteKey(t *testing.T, rawURL, key string) string {
	t.Helper()
	rest, ok := strings.CutPrefix(rawURL, objectstoreURLPrefixForTest)
	if !ok {
		t.Fatalf("%q does not start with the in-memory URL prefix", rawURL)
	}
	_, query, ok := strings.Cut(rest, "?")
	if !ok {
		t.Fatalf("%q has no query", rawURL)
	}
	return objectstoreURLPrefixForTest + url.PathEscape(key) + "?" + query
}

// decodedKey parses the key back out of a presigned URL, which is what the store
// does. It uses net/url rather than the store's own parser so the test does not
// assert the parser against itself.
func decodedKey(t *testing.T, rawURL string) string {
	t.Helper()
	rest, ok := strings.CutPrefix(rawURL, objectstoreURLPrefixForTest)
	if !ok {
		t.Fatalf("%q does not start with the in-memory URL prefix", rawURL)
	}
	encoded, _, _ := strings.Cut(rest, "?")
	key, err := url.PathUnescape(encoded)
	if err != nil {
		t.Fatalf("unescaping %q: %v", encoded, err)
	}
	return key
}

// bumpExpires returns rawURL with its `expires` value replaced and everything
// else — the prefix, the key, and the signature — untouched.
//
// # THIS HELPER WAS WRONG TWICE, AND BOTH VERSIONS PASSED
//
// The first discarded `strings.Cut`'s THIRD return and kept the head, building
// `?9999999999&1791141187` — no `expires=`, no `signature=`. The second kept
// the head and forgot that `Cut` EXCLUDES the separator from it, building
// `...?1822676348&signature=…` — no `expires=`. Either way the store refused the
// URL for being unparseable rather than for being tampered with, and every case
// in TestATamperedURLIsRefused passed against a helper that was testing nothing.
//
// Both were found by planting a divergence — removing `expiresAt` from the
// signature, which is exactly the bug this helper is the guard for — and
// watching the test stay green. That is the failure `AGENTS.md` names: "Any new
// guard here gets a planted divergence before it is believed, and if it hangs or
// passes on a broken source, that is a finding about the guard, not about the
// source."
func bumpExpires(t *testing.T, rawURL string, expires uint64) string {
	t.Helper()
	before, after, ok := strings.Cut(rawURL, "expires=")
	if !ok {
		t.Fatalf("%q has no expires parameter", rawURL)
	}
	_, rest, ok := strings.Cut(after, "&")
	if !ok {
		t.Fatalf("%q has no signature parameter", rawURL)
	}
	return before + "expires=" + strconv.FormatUint(expires, 10) + "&" + rest
}

const objectstoreURLPrefixForTest = "memory://darkroom/"
