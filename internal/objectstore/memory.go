package objectstore

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"time"
)

// signingSecret is the fixed secret the in-memory store signs with.
//
// FIXED, NOT RANDOM: "a test that cannot reproduce a signature is a test that
// cannot assert on one. This is a fake's key in a process that never talks to a
// real bucket; the real key lives in the environment for the S3 store and is
// never in this repository." `AGENTS.md`: "No secrets in the repo."
const signingSecret = "darkroom-test-signing-key"

// urlPrefix is the scheme the in-memory store's presigned URLs carry. It is also
// how a test tells the two implementations apart, and it is deliberately a
// scheme rather than a hostname: `memory://darkroom/…` is a URL no HTTP client
// will resolve, so a `memory://` URL that reached a real transport would fail
// loudly rather than quietly.
const urlPrefix = "memory://darkroom/"

// Stats counts what this store was asked to do, so a test can assert that the
// client path really did PUT straight to storage instead of the bytes passing
// through the service. A signed-upload design that quietly proxied every upload
// would still pass every functional test and fail this one.
type Stats struct {
	// Puts is writes by the SERVICE (Store.Put). A client's bytes never go
	// through here, which is what the signed-upload pattern is for.
	Puts uint64
	// Gets is full object reads (Store.Get). A metadata probe does not count.
	Gets uint64
	// Heads is metadata probes (Store.Head). Separate from Gets so a test can
	// assert "the service checked but did not download".
	Heads uint64
	// Deletes is removals (Store.Delete).
	Deletes uint64
	// Presigns is minted URLs (Store.PresignPut).
	Presigns uint64
}

// Memory is the fake. Always compiled, used by every test and by
// `DARKROOM_OBJECT_STORE=memory` in development.
//
// One `sync.Mutex` rather than an `RWMutex`: the operations are microseconds
// long and a write lock is simpler to reason about than a read lock held across
// somebody else's code. Nothing here blocks on IO — that is what makes the
// in-memory store a usable fake rather than a slower real one.
type Memory struct {
	mu      sync.Mutex
	objects map[string]*storedObject

	// stats has its own mutex so a test reading Stats does not contend with a
	// write, and so the count is readable while a long ApplyPresignedPut holds
	// `mu`. Two mutexes rather than one because the Rust original had two locks
	// and collapsing them would couple two unrelated invariants.
	statsMu sync.Mutex
	stats   Stats
}

// storedObject is one key.
//
// Bytes are nil until something writes to it, and nil bytes is what makes a
// key that was only ever PRESIGNED read as absent. That is the whole reason this
// type exists separately from a plain map[string][]byte: presigning creates the
// entry, and an object that was never PUT to it must still read as missing, or
// `complete` would accept an upload that never happened.
type storedObject struct {
	bytes       []byte
	contentType string
	signedPuts  map[string]*signedPut
}

// signedPut is a granted, not-yet-used (or used) permission to write one key.
//
// Key and Signature are retained for the audit trail in a debug dump. The
// verification path reads the key back out of the URL instead, because the URL
// is what arrived — that is deliberate: verifying a key the STORE remembered
// rather than the key the URL carries would make a rewritten URL verify against
// something other than what it says.
type signedPut struct {
	key         string
	contentType string
	maxBytes    int64
	expiresAt   uint64
	signature   string
}

// NewMemory returns an empty in-memory store.
func NewMemory() *Memory {
	return &Memory{objects: map[string]*storedObject{}}
}

// The interface is satisfied by the constructor's return type, and the assertion
// is in this file rather than in a test: a method that drifts out of the
// interface has to break the BUILD of the package, not fail a test run that
// somebody might not have looked at.
var _ Store = (*Memory)(nil)

// Stats returns a copy of the counters. A copy rather than the live struct, so a
// test cannot mutate a counter it is about to assert on.
func (m *Memory) Stats() Stats {
	m.statsMu.Lock()
	defer m.statsMu.Unlock()
	return m.stats
}

// Len is how many objects HOLD BYTES.
//
// It counts entries with content, not entries, and the distinction is the whole
// point: PresignPut creates an entry so it has somewhere to record the grant, and
// an entry that holds nothing is not a stored object. Counting entries instead
// would make this return 1 after a presign and a client-shaped store — so the
// delete test asserts this goes to zero, which is stronger than asserting a key
// is gone, because a Delete that removed one object and left a second would pass
// the weaker assertion.
func (m *Memory) Len() int {
	m.mu.Lock()
	defer m.mu.Unlock()
	n := 0
	for _, object := range m.objects {
		if len(object.bytes) > 0 {
			n++
		}
	}
	return n
}

// IsEmpty reports whether nothing is stored.
func (m *Memory) IsEmpty() bool { return m.Len() == 0 }

func (m *Memory) count(f func(*Stats)) {
	m.statsMu.Lock()
	defer m.statsMu.Unlock()
	f(&m.stats)
}

// PresignPut mints a URL scoped to exactly one key, one content type and one
// byte ceiling, expiring in ttl.
//
// It also CREATES the key's entry, with no bytes in it, which is why Head has to
// treat an empty object as absent rather than as present.
func (m *Memory) PresignPut(_ context.Context, key, contentType string, maxBytes int64, ttl time.Duration) (PresignedPut, error) {
	expiresAt := unixNow() + uint64(ttl/time.Second)
	signature := sign(key, expiresAt)

	m.mu.Lock()
	object, ok := m.objects[key]
	if !ok {
		object = &storedObject{contentType: contentType, signedPuts: map[string]*signedPut{}}
		m.objects[key] = object
	}
	object.signedPuts[signature] = &signedPut{
		key:         key,
		contentType: contentType,
		maxBytes:    maxBytes,
		expiresAt:   expiresAt,
		signature:   signature,
	}
	m.mu.Unlock()

	m.count(func(s *Stats) { s.Presigns++ })

	return PresignedPut{
		// url.PathEscape and not a hand-rolled encoder: it escapes `/`, `%`, `?`
		// and `#`, which is the whole requirement, and a URL builder that breaks
		// on a `/` is a trap for the next person to change the key format. The
		// Rust original hand-rolled three replacements for the same reason and
		// with a smaller net.
		URL:       urlPrefix + url.PathEscape(key) + "?expires=" + strconv.FormatUint(expiresAt, 10) + "&signature=" + signature,
		Key:       key,
		ExpiresIn: int64(ttl / time.Second),
	}, nil
}

// Head reports what is at key, or ErrNotFound.
//
// A key that was only ever presigned holds no bytes. Reporting it as present
// would let `complete` accept an upload that never happened, which is the entire
// reason this method has a case that `Get` does not.
func (m *Memory) Head(_ context.Context, key string) (ObjectMeta, error) {
	m.count(func(s *Stats) { s.Heads++ })

	m.mu.Lock()
	defer m.mu.Unlock()
	object, ok := m.objects[key]
	if !ok || len(object.bytes) == 0 {
		return ObjectMeta{}, notFound("head")
	}
	return ObjectMeta{
		Key:         key,
		ByteSize:    int64(len(object.bytes)),
		ContentType: object.contentType,
	}, nil
}

// Get returns the bytes at key, or ErrNotFound.
//
// It returns the stored slice rather than a copy. That is a deliberate decision
// with a stated consequence, not an oversight: a []byte returned from an
// interface cannot be a Rust-style `Bytes` — an immutable, refcounted handle —
// without a third-party dependency, so the two honest options are a copy per
// call or a shared slice. This takes the shared slice and says so, because the
// alternative is a full copy of every object on every read of a service whose
// largest read is 1 GiB. A caller that MUTATES what Get returned corrupts the
// store, and the callers in this repository do not mutate: they hash it or
// transform it into a new buffer.
func (m *Memory) Get(_ context.Context, key string) ([]byte, error) {
	m.count(func(s *Stats) { s.Gets++ })

	m.mu.Lock()
	defer m.mu.Unlock()
	object, ok := m.objects[key]
	if !ok || len(object.bytes) == 0 {
		return nil, notFound("get")
	}
	return object.bytes, nil
}

// Put writes b at key. The SERVICE's write path — variant generation only. The
// client writes through its presigned URL, which is what keeps a 1 GiB upload
// from being proxied through the API process.
func (m *Memory) Put(_ context.Context, key string, b []byte, contentType string) error {
	m.count(func(s *Stats) { s.Puts++ })

	m.mu.Lock()
	defer m.mu.Unlock()
	m.objects[key] = &storedObject{bytes: b, contentType: contentType, signedPuts: map[string]*signedPut{}}
	return nil
}

// Delete removes key.
//
// IDEMPOTENT: deleting a key that is not there is a success, because
// `DELETE /v1/assets/:id` is retryable and a 500 on the second attempt would
// make a safe operation look broken. The same rule `AGENTS.md` states as "the
// rule is absence, not refusal", applied to the one operation where the caller
// already knows the answer.
func (m *Memory) Delete(_ context.Context, key string) error {
	m.count(func(s *Stats) { s.Deletes++ })

	m.mu.Lock()
	defer m.mu.Unlock()
	delete(m.objects, key)
	return nil
}

// ApplyPresignedPut applies a presigned PUT exactly as a bucket would: verify the
// signature, the key, the content type, the length cap, and the expiry — in that
// order, and all five — then store.
//
// THIS EXISTS SO A TEST CAN EXERCISE THE REJECTION PATHS. Nothing in the
// production code path calls it; it is the fake's simulation of the client side
// of a presigned PUT. It takes a context for uniformity with the interface even
// though it touches nothing that can block, because a test-only method that
// looks different from the thing it is simulating invites a reader to assume it
// behaves differently.
//
// The four rejections are the whole reason the in-memory store is a usable fake
// rather than a stub. A fake that only "worked" would let a bug in scope or TTL
// ship, because nothing in the test suite would notice.
func (m *Memory) ApplyPresignedPut(_ context.Context, rawURL string, b []byte, contentType string) error {
	now := unixNow()

	// The parse and the signature check happen BEFORE the lock, so a forged URL
	// never reaches the store's state at all. A URL is attacker-supplied input,
	// so nothing in it is trusted before the signature verifies.
	grant, ok := grantForURL(rawURL)
	if !ok {
		return &Error{Op: "apply_presigned_put", Err: errOther, Detail: "signature is not valid"}
	}

	m.mu.Lock()
	defer m.mu.Unlock()

	object, ok := m.objects[grant.key]
	if !ok {
		return notFound("apply_presigned_put")
	}
	signed, ok := object.signedPuts[grant.signature]
	if !ok {
		return &Error{Op: "apply_presigned_put", Err: errOther, Detail: "no such presigned grant"}
	}

	// Expiry is checked before anything else that could leak a timing difference
	// about the object's state, and before the content type and length, because
	// those two answer questions about the REQUEST rather than about whether the
	// grant was still live.
	if now >= signed.expiresAt {
		return &Error{Op: "apply_presigned_put", Err: errOther, Detail: "presigned url has expired"}
	}
	if signed.contentType != contentType {
		return &Error{Op: "apply_presigned_put", Err: errOther, Detail: "content type is outside the signed scope"}
	}
	if int64(len(b)) > signed.maxBytes {
		return tooLarge("apply_presigned_put")
	}

	object.bytes = b
	object.contentType = contentType
	return nil
}

// grant is what a presigned URL is parsed back into.
type grant struct {
	key       string
	signature string
}

// grantForURL parses a presigned URL back into the key and signature it carries,
// checking the signature before either is used.
func grantForURL(rawURL string) (grant, bool) {
	rest, ok := strings.CutPrefix(rawURL, urlPrefix)
	if !ok {
		return grant{}, false
	}
	encodedKey, query, ok := strings.Cut(rest, "?")
	if !ok {
		return grant{}, false
	}
	var (
		expires   uint64
		haveExp   bool
		signature string
	)
	for _, pair := range strings.Split(query, "&") {
		k, v, ok := strings.Cut(pair, "=")
		if !ok {
			return grant{}, false
		}
		switch k {
		case "expires":
			n, err := strconv.ParseUint(v, 10, 64)
			if err != nil {
				return grant{}, false
			}
			expires, haveExp = n, true
		case "signature":
			signature = v
		default:
			// An UNRECOGNISED query parameter is refused rather than ignored. A
			// fake that ignores what it does not recognise is a fake that accepts
			// a URL carrying a `x-darkroom-anything` it never agreed to, and the
			// whole value of this parse is that it is strict.
			return grant{}, false
		}
	}
	if !haveExp || signature == "" {
		return grant{}, false
	}
	key, err := url.PathUnescape(encodedKey)
	if err != nil {
		return grant{}, false
	}
	// Constant-time comparison would matter against a real timing attack; here
	// it matters that a forged URL is rejected AT ALL, and the string compare
	// does that. A test asserts a tampered signature is refused. This is a
	// fake's signature over a fake's URL — see sign.
	if sign(key, expires) != signature {
		return grant{}, false
	}
	return grant{key: key, signature: signature}, true
}

// sign is the signature: sha256 over the secret, the key and the expiry, with a
// NUL separator between each so that no two distinct triples can produce the
// same input. Not cryptographically load-bearing — a fake's URL is not a
// security boundary against anyone who is not already running the test binary —
// but it is a *real* signature, so the fake rejects a tampered URL the way S3
// does and the scope assertions in the test suite mean something.
//
// `crypto/sha256` and nothing hand-rolled: `AGENTS.md` says "No `unsafe`" and the
// Go analogue of "no hand-rolled crypto" is the same rule — the standard
// library's digest is the one that has been reviewed.
func sign(key string, expiresAt uint64) string {
	h := sha256.New()
	h.Write([]byte(signingSecret))
	h.Write([]byte{0})
	h.Write([]byte(key))
	h.Write([]byte{0})
	h.Write([]byte(strconv.FormatUint(expiresAt, 10)))
	return hex.EncodeToString(h.Sum(nil))
}

// unixNow is whole seconds since the epoch.
//
// Seconds and not nanoseconds because the URL carries the expiry as seconds and
// because a URL's lifetime is not something a nanosecond precision could
// protect.
func unixNow() uint64 { return uint64(time.Now().Unix()) }
