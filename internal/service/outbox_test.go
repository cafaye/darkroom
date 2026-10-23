package service

import (
	"context"
	"encoding/json"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/anywaye/darkroom/internal/darkroomdb/pgtest"
	"github.com/anywaye/darkroom/internal/objectstore"
	"github.com/anywaye/darkroom/internal/problem"
)

func TestEveryEventTypeHasThreeSegmentsAndThePublisherPrefix(t *testing.T) {
	// THE MACHINE-CHECKED FORM OF core's GRAMMAR, on the types this package can emit.
	// A fourth segment or a missing prefix is a type no consumer can route, and it is
	// the kind of typo that surfaces only when something tries to subscribe — which is
	// a day, not a build.
	types := eventTypesForTest()
	if len(types) == 0 {
		t.Fatal("no event types: a guard that walks an empty list proves nothing")
	}
	for _, eventType := range types {
		segments := splitDots(string(eventType))
		if len(segments) != 3 {
			t.Errorf("%q is %d segments, want 3", eventType, len(segments))
			continue
		}
		if segments[0] != Source {
			t.Errorf("%q is not prefixed by its publisher %q", eventType, Source)
		}
		for _, segment := range segments[1:] {
			if !isSnakeCase(segment) {
				t.Errorf("%q: %q is not snake_case", eventType, segment)
			}
		}
		// AND THE ACTION IS IN core's v0 VOCABULARY. `ready` is core's "state, never
		// command" form and `created` is the past tense; a fourth verb would need a
		// core change first.
		switch segments[2] {
		case "ready", "deleted", "created":
		default:
			t.Errorf("%q: %q is not in the v0 action vocabulary", eventType, segments[2])
		}
	}
}

func TestTheEnvelopeCarriesExactlyTheRequiredAttributes(t *testing.T) {
	// core: "Undeclared envelope attributes are rejected: `additionalProperties` is
	// `false`." So the envelope carries these SEVEN keys and no others — asserted on
	// the key set rather than on a rendering, because a rendering that also carried an
	// eighth key would pass a test that only checked the seven.
	event := NewEvent(EventAssetReady, "ast_01J9", nowFixed(), map[string]any{"asset_id": "ast_01J9"})
	envelope := event.Envelope()

	want := map[string]bool{
		"specversion": true, "id": true, "type": true, "source": true,
		"subject": true, "time": true, "data": true,
	}
	for key := range envelope {
		if !want[key] {
			t.Errorf("the envelope carries %q, which core does not declare", key)
		}
	}
	for key := range want {
		if _, present := envelope[key]; !present {
			t.Errorf("the envelope is missing %q", key)
		}
	}
	if envelope["specversion"] != "1.0" {
		t.Errorf("specversion = %v, want 1.0", envelope["specversion"])
	}
	if envelope["source"] != Source {
		t.Errorf("source = %v, want %q", envelope["source"], Source)
	}
	// The envelope id is the DEDUPE KEY and is stable across reuse: "a retry publishes
	// the same `id` and the consumer's dedupe key actually dedupes."
	if first, second := event.Envelope()["id"], event.Envelope()["id"]; first != second {
		t.Error("building the envelope twice produced two ids")
	}
	other := NewEvent(EventAssetReady, "ast_01J9", nowFixed(), map[string]any{})
	if other.Envelope()["id"] == envelope["id"] {
		t.Error("two events produced the same id")
	}
	// AND THE TIME IS RFC3339 UTC, which is what core's envelope schema says.
	if stamp, _ := event.Envelope()["time"].(string); len(stamp) == 0 || stamp[len(stamp)-1] != 'Z' {
		t.Errorf("time = %v, want RFC3339 in UTC", event.Envelope()["time"])
	}
}

func TestTheReservedCodeSetIsTheOnlyOne(t *testing.T) {
	// The service can only fail in the ways it names. This walks the set the
	// constructors above can produce and asserts each one's code is one core reserves —
	// because an invented slug is a code no client's switch has a case for.
	for _, err := range []*problem.Error{
		problem.NotFound("x"), problem.Conflict("x"), problem.Invalid("x"),
		problem.Internal("x"), problem.Unavailable("x"),
		problem.IdempotencyKeyReused("x"), problem.Unauthorized("x"), problem.Forbidden("x"),
	} {
		known := false
		switch err.Code() {
		case problem.CodeNotFound, problem.CodeConflict, problem.CodeValidationFailed,
			problem.CodeInternal, problem.CodeUnavailable, problem.CodeIdempotencyKeyReused,
			problem.CodeUnauthorized, problem.CodeForbidden:
			known = true
		}
		if !known {
			t.Errorf("%q is not a code core reserves", err.Code())
		}
	}
}

// ----------------------------------------------------------------------------
// the ledger, against a real database
// ----------------------------------------------------------------------------

func TestAReservationIsTakenOnceAndThenReplayed(t *testing.T) {
	pg := pgtest.New(t)
	svc := New(pg.Store(), objectstore.NewMemory())
	ctx := context.Background()

	const key, endpoint = "9f1c-key", "POST /v1/uploads"
	scope := PrincipalScope(aliceUser, aliceAccount)
	hash := RequestHash([]byte(`{"filename":"photo.png"}`))

	// 1. THE FIRST REQUEST TAKES THE RESERVATION. A nil replay with a nil error means
	// "you hold it, do the work".
	replay, pErr := svc.ReserveIdempotent(ctx, key, endpoint, scope, hash)
	if pErr != nil {
		t.Fatalf("the first reserve: %v", pErr)
	}
	if replay != nil {
		t.Fatal("the first request was given a replay; nothing had been recorded yet")
	}

	// 2. A CONCURRENT REQUEST IS TOLD 409, not handed an empty success:
	// "Returning 'success with an empty body' would be worse than an error: the
	// client would believe the upload was created and then never have an id for it."
	_, pErr = svc.ReserveIdempotent(ctx, key, endpoint, scope, hash)
	pErrExpect(t, pErr, 409, "conflict", "a concurrent request with the same key")

	// 3. THE ORIGINAL RESPONSE IS RECORDED.
	svc.CompleteIdempotent(ctx, key, endpoint, scope, 201, []byte(`{"id":"ast_1"}`))

	// 4. AND THE REPLAY IS THE ORIGINAL RESPONSE: same status, same JSON.
	//
	// THE BODY IS COMPARED SEMANTICALLY, and the reason is worth stating because the
	// first draft of this compared the bytes and failed: `response_body` is `jsonb`,
	// so Postgres re-serializes `{"id":"ast_1"}` as `{"id": "ast_1"}`. Semantic
	// equality is the property the replay contract actually needs — every field name
	// and every value survive — and the normalisation is Postgres's, not this
	// repository's.
	replay, pErr = svc.ReserveIdempotent(ctx, key, endpoint, scope, hash)
	if pErr != nil {
		t.Fatalf("the replay: %v", pErr)
	}
	if replay == nil {
		t.Fatal("a recorded response did not replay")
	}
	if replay.StatusCode != 201 {
		t.Errorf("the replay status is %d, want the recorded 201", replay.StatusCode)
	}
	if !sameJSON(t, replay.Body, `{"id":"ast_1"}`) {
		t.Errorf("the replay body is %s, want the recorded body", replay.Body)
	}

	// 5. A DIFFERENT BODY UNDER THE SAME KEY IS 409 `idempotency_key_reused` — a
	// distinct code from `conflict` because the client's action differs: pick a new
	// key, rather than retry.
	_, pErr = svc.ReserveIdempotent(ctx, key, endpoint, scope, RequestHash([]byte(`{"filename":"other.png"}`)))
	pErrExpect(t, pErr, 409, "idempotency_key_reused", "a different body under one key")

	// 6. RELEASE PUTS THE KEY BACK, because "a 500 that poisoned the key would make
	// every subsequent retry fail forever."
	svc.ReleaseIdempotent(ctx, key, endpoint, scope)
	replay, pErr = svc.ReserveIdempotent(ctx, key, endpoint, scope, hash)
	if pErr != nil || replay != nil {
		t.Errorf("after a release the key did not become reservable again: %v %v", replay, pErr)
	}
}

func TestTheLedgerIsScopedToEndpointAndPrincipal(t *testing.T) {
	// "One tenant's key must never replay another tenant's response, which would hand
	// a caller data they are not entitled to." The scope is the PRIMARY KEY, so this
	// is enforced by the schema rather than by a predicate somebody might forget.
	pg := pgtest.New(t)
	svc := New(pg.Store(), objectstore.NewMemory())
	ctx := context.Background()

	const key = "shared-key"
	hash := RequestHash([]byte(`{"a":1}`))
	alice := PrincipalScope(aliceUser, aliceAccount)
	bob := PrincipalScope(bobUser, bobAccount)

	if _, pErr := svc.ReserveIdempotent(ctx, key, "POST /v1/uploads", alice, hash); pErr != nil {
		t.Fatal(pErr)
	}
	svc.CompleteIdempotent(ctx, key, "POST /v1/uploads", alice, 201, []byte(`{"alice":true}`))

	// BOB, SAME KEY, SAME BODY: a fresh reservation, not alice's response.
	replay, pErr := svc.ReserveIdempotent(ctx, key, "POST /v1/uploads", bob, hash)
	if pErr != nil {
		t.Fatalf("bob's reserve: %v", pErr)
	}
	if replay != nil {
		t.Fatalf("bob replayed alice's response: %s", replay.Body)
	}

	// AND A DIFFERENT ENDPOINT, same principal and key: also fresh, because
	// `idempotency_keys_pk` is (key, endpoint, principal_key).
	if _, pErr := svc.ReserveIdempotent(ctx, key, "POST /v1/assets", alice, hash); pErr != nil {
		t.Fatalf("the other endpoint: %v", pErr)
	}
	if _, pErr := svc.ReserveIdempotent(ctx, key, "POST /v1/assets", alice, hash); pErr == nil {
		t.Error("the second request to the other endpoint was not told it was in flight")
	}
}

func TestKeysAreValidatedAndHashesAreStable(t *testing.T) {
	// A malformed key is a 422 and NOT silently treated as absent: "a client that
	// meant to send a key and sent a broken one would otherwise get a
	// non-idempotent request and not know why."
	for _, bad := range []string{"", "has space", "has\x00null", string(make([]byte, 300))} {
		if pErr := ValidateIdempotencyKey(bad); pErr == nil {
			t.Errorf("ValidateIdempotencyKey(%q) accepted it", bad)
		} else if pErr.Status() != 422 {
			t.Errorf("ValidateIdempotencyKey(%q) is %d, want 422", bad, pErr.Status())
		}
	}
	if pErr := ValidateIdempotencyKey("8f14e45f-ceea-467a-9b0f-01d3e2f4b5a6"); pErr != nil {
		t.Errorf("a uuid-shaped key was refused: %v", pErr)
	}
	if pErr := ValidateIdempotencyKey(string(make([]byte, MaxIdempotencyKeyLength+1))); pErr == nil {
		t.Error("an over-long key was accepted")
	}

	// The scope is BOTH halves of the principal, so two users in one account do not
	// share a key namespace.
	if PrincipalScope("u1", "a1") == PrincipalScope("u2", "a1") {
		t.Error("two users in one account share a principal scope")
	}
	// And the hash is over BYTES, so the same JSON value spelled two ways is two
	// bodies — the strict reading, because core's contract is about the request.
	a, b := RequestHash([]byte(`{"a":1,"b":2}`)), RequestHash([]byte(`{"b":2,"a":1}`))
	if a == b {
		t.Error("two differently spelled bodies hashed the same")
	}
	if RequestHash([]byte(`{"a":1,"b":2}`)) != a {
		t.Error("the hash is not stable for the same bytes")
	}
}

// ----------------------------------------------------------------------------

func pErrExpect(t *testing.T, pErr *problem.Error, status int, code problem.Code, msg string) {
	t.Helper()
	if pErr == nil {
		t.Fatalf("%s: expected %d/%s, got success", msg, status, code)
	}
	if pErr.Status() != status || pErr.Code() != code {
		t.Errorf("%s: got %d/%s (%v), want %d/%s", msg, pErr.Status(), pErr.Code(), pErr, status, code)
	}
}

func splitDots(s string) []string { return strings.Split(s, ".") }

// sameJSON reports whether two bodies are the same JSON value, whatever the
// whitespace. Used for the replay comparison, with the reason written at the call
// site: `response_body` is `jsonb` and Postgres re-serializes it.
func sameJSON(t *testing.T, a []byte, b string) bool {
	t.Helper()
	var left, right any
	if err := json.Unmarshal(a, &left); err != nil {
		t.Fatalf("%q is not JSON: %v", a, err)
	}
	if err := json.Unmarshal([]byte(b), &right); err != nil {
		t.Fatalf("%q is not JSON: %v", b, err)
	}
	return reflect.DeepEqual(left, right)
}

func isSnakeCase(s string) bool {
	for i := range len(s) {
		c := s[i]
		switch {
		case c >= 'a' && c <= 'z', c >= '0' && c <= '9', c == '_':
		default:
			return false
		}
	}
	return len(s) > 0
}

func nowFixed() time.Time {
	return time.Date(2026, 10, 4, 12, 0, 0, 0, time.UTC)
}
