package httpapi_test

// The `Idempotency-Key` contract, end to end through the real router, against a
// real database.
//
// # WHAT IS PROVEN HERE AND WHERE IT LIVES
//
// `internal/service/idempotency.go` holds the DATABASE half of the ledger —
// reserve, record, release — and its own tests prove those four steps in
// isolation. Nothing in this repository proved the WIRE half: that the header is
// read, that a replay really replays, that `Idempotency-Replayed: true` is
// written, and that a reservation is released when the handler fails. Those are
// properties of `internal/httpapi`, so they are tested here, through the router,
// with the ledger in a `pgtest` schema — which is what `bin/prime --db` runs and
// what `bin/tier-counts` now counts as `database  httpapi checks`.
//
// # WHY THE REPLAY COMPARISON IS SEMANTIC AND NOT BYTE
//
// `idempotency_keys.response_body` is `jsonb`, so Postgres re-serialises what was
// stored: `{"a":1}` comes back as `{"a": 1}`, and object keys come back in jsonb's
// own order rather than the order `json.Marshal` wrote them. `internal/service`'s
// own ledger test found this the same way and says so at the call site. So this
// file asserts:
//
//   - the replay is BYTE-IDENTICAL to what the ledger holds (that is the ledger's
//     promise, and it is exact), and
//   - the replay is the same JSON VALUE as the first response (that is the
//     contract's promise, and it is what a client parses).
//
// The divergence between the two is Postgres's, not this repository's, and it is
// reported rather than hidden. A client comparing bytes rather than parsing JSON
// would see it.
//
// # NO SLEEPS, NO TIMING ASSUMPTIONS
//
// The in-flight case — two identical requests with one key at the same instant —
// is asserted by `internal/service`'s ledger test, which reserves twice in one
// goroutine and therefore needs no timing. Nothing here races two requests,
// because a test that does would be a test whose result depends on scheduling.

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"reflect"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5"

	"github.com/cafaye/darkroom/internal/auth"
	"github.com/cafaye/darkroom/internal/darkroomdb"
	"github.com/cafaye/darkroom/internal/darkroomdb/pgtest"
	"github.com/cafaye/darkroom/internal/httpapi"
	"github.com/cafaye/darkroom/internal/objectstore"
	"github.com/cafaye/darkroom/internal/service"
)

const (
	// Two accounts and two users in one of them, because `PrincipalScope` is
	// `user_id:account_id` and the isolation test has to be able to vary either
	// half independently.
	//
	// ALL FOUR ARE UUIDs, which is the schema's requirement rather than a tidiness
	// choice: `assets.owner_user_id` is a `uuid` column, so a `sub` of `"user-1"`
	// reaches `insertAsset` and fails at `invalid input syntax for type uuid` —
	// which the service reports as an `internal` 500, because a database refusing a
	// value the identity service minted is not something a client did wrong. The
	// shared `mint` in `httpapi_test.go` defaults `sub` to `"user-1"`, and every
	// test in that file stops at the credential gate, so nothing there reaches a
	// column. `sub` is therefore overridden on every request this file makes.
	idemAccountOne = "3f6c1e6c-0000-4000-8000-00000000000a"
	idemAccountTwo = "3f6c1e6c-0000-4000-8000-00000000000b"
	idemUserOne    = "7d2b9f10-0000-4000-8000-00000000000a"
	idemUserTwo    = "7d2b9f10-0000-4000-8000-00000000000b"

	// A syntactically valid sha256, which is all `POST /v1/uploads` asks for at
	// create time: the claim is recorded here and VERIFIED against the stored
	// bytes at complete, so no object needs to exist for this test.
	idemChecksum = "9f86d081884c7d659a2feaa0c55ad015a3bf4f1b2b0b822cd15d6c15b0f00a08"
)

// ledger is a test's router, its identity, and the store whose schema the ledger
// lives in.
type ledger struct {
	router http.Handler
	id     *identity
	store  *darkroomdb.Store
}

// newLedger builds the real router over a real service, a real ledger schema and
// a real signing identity. `pgtest.New` SKIPS rather than fails when no database
// answers, so every test below is a counted skip and not a pass.
func newLedger(t *testing.T) *ledger {
	t.Helper()

	pg := pgtest.New(t)
	store := pg.Store()
	id := newIdentity(t)
	verifier, err := auth.New(auth.Options{
		Issuer:   testIssuer,
		Audience: testAudience,
		JWKSURL:  id.server.URL + "/.well-known/jwks.json",
	})
	if err != nil {
		t.Fatalf("auth.New: %v", err)
	}
	router, err := httpapi.New(httpapi.Options{
		Service:  service.New(store, objectstore.NewMemory()),
		Verifier: verifier,
	})
	if err != nil {
		t.Fatalf("httpapi.New: %v", err)
	}
	return &ledger{router: router, id: id, store: store}
}

// create builds a valid `POST /v1/uploads` body. The filename is a parameter
// because "same key, different body" has to be a difference the DOCUMENT can see.
func create(filename string) map[string]any {
	return map[string]any{
		"filename":     filename,
		"content_type": "image/png",
		"byte_size":    2048,
		"checksum":     idemChecksum,
	}
}

// keyed is a request carrying `key`, as the principal described by `as`.
//
// `sub` and `account_id` are both defaulted to this file's UUIDs and `as` overrides
// either, so a caller cannot accidentally inherit the shared `mint`'s non-uuid
// `"user-1"` and get a 500 that says nothing about idempotency.
func (l *ledger) keyed(t *testing.T, method, path string, body any, key string, as ...map[string]any) *http.Request {
	t.Helper()

	r := request(method, path, body)
	claims := map[string]any{
		"sub":        idemUserOne,
		"account_id": idemAccountOne,
		"scope":      auth.ScopeRead + " " + auth.ScopeWrite,
	}
	for _, override := range as {
		for name, value := range override {
			claims[name] = value
		}
	}
	r.Header.Set("Authorization", "Bearer "+l.id.mint(t, claims))
	if key != "" {
		r.Header.Set("Idempotency-Key", key)
	}
	return r
}

// do drives the router as a function call and reads the response.
func (l *ledger) do(t *testing.T, r *http.Request) *recorder {
	t.Helper()

	rec := newRecorder()
	l.router.ServeHTTP(rec, r)
	rec.read()
	return rec
}

// row is one row of the ledger, read back the way the next request will read it.
type row struct {
	endpoint    string
	principal   string
	statusCode  int
	requestHash string
	body        []byte
}

// rows reads the ledger. `idempotency_keys` has no `account_id` and therefore no
// RLS policy, so this is `InTx` and not `InAccountTx` — the same choice
// `internal/service/idempotency.go` makes and explains.
func (l *ledger) rows(t *testing.T) []row {
	t.Helper()

	var out []row
	err := l.store.InTx(context.Background(), func(tx pgx.Tx) error {
		got, err := tx.Query(context.Background(), `select endpoint, principal_key, status_code, request_hash, response_body
			from idempotency_keys order by endpoint, principal_key, key`)
		if err != nil {
			return err
		}
		defer got.Close()
		for got.Next() {
			var one row
			if err := got.Scan(&one.endpoint, &one.principal, &one.statusCode, &one.requestHash, &one.body); err != nil {
				return err
			}
			out = append(out, one)
		}
		return got.Err()
	})
	if err != nil {
		t.Fatalf("reading the ledger: %v", err)
	}
	return out
}

// assetCount is how many assets the account owns, through the service's own
// transaction entry point rather than through the list handler — the list handler
// belongs to another packet in this wave and this test must not depend on it.
func (l *ledger) assetCount(t *testing.T, account string) int {
	t.Helper()

	count := 0
	err := l.store.InAccountTx(context.Background(), account, func(tx pgx.Tx) error {
		return tx.QueryRow(context.Background(), `select count(*) from assets`).Scan(&count)
	})
	if err != nil {
		t.Fatalf("counting assets for %s: %v", account, err)
	}
	return count
}

// TestAKeyedUploadIsRecordedAndItsReplayReturnsTheOriginal is done-means 1 and 2
// together, because the second is only observable once the first has run.
func TestAKeyedUploadIsRecordedAndItsReplayReturnsTheOriginal(t *testing.T) {
	pg := newLedger(t)

	// 1. THE FIRST REQUEST RUNS THE HANDLER AND IS RECORDED.
	first := pg.do(t, pg.keyed(t, http.MethodPost, "/v1/uploads", create("photo.png"), "key-1", map[string]any{"account_id": idemAccountOne}))
	if first.status != http.StatusCreated {
		t.Fatalf("the first request answered %d, want 201: %s", first.status, first.body)
	}
	if got := first.Header().Get("Idempotency-Replayed"); got != "" {
		t.Fatalf("the first response carries Idempotency-Replayed: %q. The document's header is an enum of ['true'] — present only on a replay", got)
	}

	recorded := pg.rows(t)
	if len(recorded) != 1 {
		t.Fatalf("the ledger holds %d rows, want 1: a keyed request that is not recorded cannot be replayed: %+v", len(recorded), recorded)
	}
	if recorded[0].endpoint != "POST /v1/uploads" {
		t.Errorf("the ledger recorded endpoint %q, want the document's `POST /v1/uploads`", recorded[0].endpoint)
	}
	if recorded[0].principal != service.PrincipalScope(idemUserOne, idemAccountOne) {
		t.Errorf("the ledger recorded principal %q, want user_id:account_id of the caller", recorded[0].principal)
	}
	if recorded[0].statusCode != http.StatusCreated {
		t.Errorf("the ledger recorded status %d, want the 201 the client received", recorded[0].statusCode)
	}
	if !sameValue(t, recorded[0].body, first.body) {
		t.Errorf("the ledger recorded %s, which is not the response the client received (%s)", recorded[0].body, first.body)
	}

	// 2. THE SAME KEY AND THE SAME BODY REPLAYS.
	second := pg.do(t, pg.keyed(t, http.MethodPost, "/v1/uploads", create("photo.png"), "key-1", map[string]any{"account_id": idemAccountOne}))
	if second.status != first.status {
		t.Fatalf("the replay answered %d and the original answered %d. A replay that changed the status would be a different response to the same request", second.status, first.status)
	}
	if got := second.Header().Get("Idempotency-Replayed"); got != "true" {
		t.Fatalf("the replay carries Idempotency-Replayed: %q, want `true` — this is the header the document declares and nothing wrote before this packet", got)
	}
	if !sameValue(t, second.body, first.body) {
		t.Fatalf("the replay body is %s and the original was %s: a retry must receive the same response", second.body, first.body)
	}
	// Byte-identical to what the LEDGER holds, which is the exact half. See the
	// file header for why this is not a byte comparison against the first response:
	// `response_body` is `jsonb` and Postgres re-serialises it.
	if len(pg.rows(t)) != 1 {
		t.Errorf("the replay added a second ledger row; a replay must not reserve again")
	}

	// 3. AND IT CREATES NO SECOND ASSET. The replay bypassed the handler entirely,
	// so this is the service's row count rather than the duplicate path's answer.
	if got := pg.assetCount(t, idemAccountOne); got != 1 {
		t.Errorf("the account owns %d assets after a replay, want 1", got)
	}
}

// TestAnIdempotencyKeyReusedWithADifferentBodyIs409 is done-means 3: the same key with a
// different body is `idempotency_key_reused` and NOT a `conflict`, because the
// client's action differs — pick a new key, rather than retry.
func TestAnIdempotencyKeyReusedWithADifferentBodyIs409(t *testing.T) {
	pg := newLedger(t)

	first := pg.do(t, pg.keyed(t, http.MethodPost, "/v1/uploads", create("photo.png"), "key-2", map[string]any{"account_id": idemAccountOne}))
	if first.status != http.StatusCreated {
		t.Fatalf("the first request answered %d, want 201: %s", first.status, first.body)
	}

	second := pg.do(t, pg.keyed(t, http.MethodPost, "/v1/uploads", create("other.png"), "key-2", map[string]any{"account_id": idemAccountOne}))
	if second.status != http.StatusConflict {
		t.Fatalf("the second request answered %d, want 409: %s", second.status, second.body)
	}
	p := decodeProblem(t, second)
	if p.Code != "idempotency_key_reused" {
		t.Fatalf("got code %q, want idempotency_key_reused: %s", p.Code, second.body)
	}
	if p.Status != http.StatusConflict {
		t.Errorf("the body's status is %d, want 409 — core: always matches the HTTP status", p.Status)
	}
	if second.Header().Get("Idempotency-Replayed") != "" {
		t.Error("a refused replay carries Idempotency-Replayed; nothing was replayed")
	}
}

// TestARequestWithNoIdempotencyKeyIsProcessedNormally is done-means 4. The document says it
// outright — "Requests without the key are processed normally" — so the proof is
// that the ledger is EMPTY afterwards: an unkeyed request reserves nothing.
func TestARequestWithNoIdempotencyKeyIsProcessedNormally(t *testing.T) {
	pg := newLedger(t)

	for range 2 {
		rec := pg.do(t, pg.keyed(t, http.MethodPost, "/v1/uploads", create("photo.png"), "", map[string]any{"account_id": idemAccountOne}))
		if rec.status != http.StatusCreated {
			t.Fatalf("an unkeyed request answered %d, want 201 — it must behave exactly as it did before this packet: %s", rec.status, rec.body)
		}
		if got := rec.Header().Get("Idempotency-Replayed"); got != "" {
			t.Errorf("an unkeyed response carries Idempotency-Replayed: %q", got)
		}
	}

	if rows := pg.rows(t); len(rows) != 0 {
		t.Errorf("two unkeyed requests wrote %d ledger rows, want 0: no key means no reservation", len(rows))
	}
	// Two uploads of the same checksum in one account are a duplicate, which the
	// service answers 201 with `X-Darkroom-Duplicate`. Asserted so this test says
	// what "as it did today" actually was, rather than only what it is not.
	if got := pg.assetCount(t, idemAccountOne); got != 1 {
		t.Errorf("the account owns %d assets, want 1 — the second identical unkeyed request resolved to the duplicate", got)
	}
}

// TestAnEmptyIdempotencyKeyIs422RatherThanAbsent is done-means 5, and the refusal
// happens before any query — which is the point: "a client that meant to send a key
// and sent a broken one would otherwise get a non-idempotent request and not know
// why."
//
// IT RUNS OVER A REAL SERVICE rather than the `handlerFor` helper, because the
// difference IS the answer: with nothing built, an empty key falls through to the
// handler and reaches `service.CreateUpload` with no store behind it, which is a
// panic rather than a status. The assertion below is about a 422, so the test needs
// a handler that can reach one either way.
func TestAnEmptyIdempotencyKeyIs422RatherThanAbsent(t *testing.T) {
	pg := newLedger(t)
	r := asCaller(t, pg.id, request(http.MethodPost, "/v1/uploads", create("photo.png")))
	r.Header["Idempotency-Key"] = []string{""}

	rec := newRecorder()
	pg.router.ServeHTTP(rec, r)
	rec.read()

	if rec.status != http.StatusUnprocessableEntity {
		t.Fatalf("an empty Idempotency-Key answered %d, want 422: %s", rec.status, rec.body)
	}
	if rec.Header().Get("Content-Type") != "application/problem+json" {
		t.Errorf("got content type %q, want application/problem+json", rec.Header().Get("Content-Type"))
	}
	p := decodeProblem(t, rec)
	if p.Code != "validation_failed" {
		t.Fatalf("got code %q, want validation_failed", p.Code)
	}
	if !mentionsField(t, rec.body, "idempotency-key") {
		t.Errorf("the refusal does not name the header it is about: %s", rec.body)
	}
	if rec.Header().Get("Idempotency-Replayed") != "" {
		t.Error("a refused request carries Idempotency-Replayed")
	}
}

// TestARepeatedIdempotencyKeyHeaderIs422 is the same reasoning for a second spelling of the
// same header: `openapi/v1.yaml` types the key as ONE string, and the generated
// binder's own answer for two values is a bare `text/plain` 400, which is not
// this service's envelope. So the wire layer refuses it as a broken key.
func TestARepeatedIdempotencyKeyHeaderIs422(t *testing.T) {
	pg := newLedger(t)
	r := asCaller(t, pg.id, request(http.MethodPost, "/v1/uploads", create("photo.png")))
	r.Header["Idempotency-Key"] = []string{"key-a", "key-b"}

	rec := newRecorder()
	pg.router.ServeHTTP(rec, r)
	rec.read()

	if rec.status != http.StatusUnprocessableEntity {
		t.Fatalf("two Idempotency-Key headers answered %d, want 422: %s", rec.status, rec.body)
	}
	if rec.Header().Get("Content-Type") != "application/problem+json" {
		t.Errorf("got content type %q — every refusal in this service is the one envelope", rec.Header().Get("Content-Type"))
	}
}

// TestAFailedRequestReleasesItsIdempotencyReservation is done-means 6.
//
// The proof is a retry, not a row count: a second request with the same key
// after a failure would be `409 conflict` if the reservation had survived (the
// body differs, so a surviving row would be `409 idempotency_key_reused`) and
// `201` if it was released. "a 500 that poisoned the key would make every
// subsequent retry fail forever."
func TestAFailedRequestReleasesItsIdempotencyReservation(t *testing.T) {
	pg := newLedger(t)

	// A 422 from the service: `byte_size` negative is refused by the state machine.
	bad := create("photo.png")
	bad["byte_size"] = -1
	first := pg.do(t, pg.keyed(t, http.MethodPost, "/v1/uploads", bad, "key-3", map[string]any{"account_id": idemAccountOne}))
	if first.status != http.StatusUnprocessableEntity {
		t.Fatalf("the invalid request answered %d, want 422: %s", first.status, first.body)
	}

	// THE SAME KEY, NOW WITH A BODY THE SERVICE ACCEPTS. If the reservation were
	// still held this is a 409; a released key is a fresh attempt.
	second := pg.do(t, pg.keyed(t, http.MethodPost, "/v1/uploads", create("photo.png"), "key-3", map[string]any{"account_id": idemAccountOne}))
	if second.status != http.StatusCreated {
		t.Fatalf("the retry answered %d, want 201: a failure that kept its reservation makes every retry a 409. %s", second.status, second.body)
	}
	if got := second.Header().Get("Idempotency-Replayed"); got != "" {
		t.Errorf("the retry is marked as a replay: %q", got)
	}
}

// TestAMalformedBodyReleasesItsIdempotencyReservationToo is the same rule one layer out: a
// 400 from `decodeJSON` is a failure, and it must not poison the key either.
func TestAMalformedBodyReleasesItsIdempotencyReservationToo(t *testing.T) {
	pg := newLedger(t)

	malformed := asCaller(t, pg.id, request(http.MethodPost, "/v1/uploads", nil))
	malformed.Header.Set("Content-Type", "application/json")
	malformed.Header.Set("Idempotency-Key", "key-4")
	rec := newRecorder()
	pg.router.ServeHTTP(rec, malformed)
	rec.read()
	if rec.status != http.StatusBadRequest {
		t.Fatalf("a body-less request answered %d, want 400: %s", rec.status, rec.body)
	}

	second := pg.do(t, pg.keyed(t, http.MethodPost, "/v1/uploads", create("photo.png"), "key-4", map[string]any{"account_id": idemAccountOne}))
	if second.status != http.StatusCreated {
		t.Fatalf("the retry after a malformed body answered %d, want 201: %s", second.status, second.body)
	}
}

// TestAReplayIsScopedToThePrincipal is done-means 7, in all three directions the
// scope has: a second ACCOUNT, a second USER in the same account, and the
// original principal still replaying its own.
//
// "One tenant's key must never replay another tenant's response, which would hand
// a caller data they are not entitled to."
func TestAReplayIsScopedToThePrincipal(t *testing.T) {
	pg := newLedger(t)
	const key = "shared-key"

	body := create("photo.png")
	alice := map[string]any{"account_id": idemAccountOne}
	// Same account, different user. `PrincipalScope` is `user_id:account_id`, so
	// two users in one account are two principals and do not share a namespace.
	carol := map[string]any{"account_id": idemAccountOne, "sub": "user-2"}
	bob := map[string]any{"account_id": idemAccountTwo}

	first := pg.do(t, pg.keyed(t, http.MethodPost, "/v1/uploads", body, key, alice))
	if first.status != http.StatusCreated {
		t.Fatalf("alice answered %d, want 201: %s", first.status, first.body)
	}

	for _, row := range []struct {
		name string
		as   map[string]any
	}{
		{"another account", bob},
		{"another user in the same account", carol},
	} {
		t.Run(row.name, func(t *testing.T) {
			other := pg.do(t, pg.keyed(t, http.MethodPost, "/v1/uploads", body, key, row.as))
			if other.status != http.StatusCreated {
				t.Fatalf("answered %d, want 201 — a shared key must be a fresh reservation, not alice's replay: %s", other.status, other.body)
			}
			if got := other.Header().Get("Idempotency-Replayed"); got != "" {
				t.Fatalf("this principal was handed alice's response: Idempotency-Replayed: %q", got)
			}
			if sameValue(t, other.body, first.body) {
				t.Fatalf("this principal received alice's body verbatim: %s", other.body)
			}
		})
	}

	// AND ALICE STILL REPLAYS HER OWN. Three reservations now exist — one per
	// principal — and the ledger's primary key is what makes that possible.
	if rows := pg.rows(t); len(rows) != 3 {
		t.Errorf("the ledger holds %d rows, want 3 (one per principal)", len(rows))
	}
	again := pg.do(t, pg.keyed(t, http.MethodPost, "/v1/uploads", body, key, alice))
	if again.Header().Get("Idempotency-Replayed") != "true" {
		t.Errorf("alice's own replay is not marked as one: %s", again.body)
	}
	// ONE asset, and this is the assertion that makes the three-reservations claim
	// mean something: the LEDGER's scope is the principal while the ASSET's scope is
	// the account. Carol and alice are two principals holding two reservations on
	// one key, and both uploads carry the same checksum, so `assets`'s
	// `unique (account_id, checksum)` resolves carol's to alice's existing row —
	// which is the service's duplicate decision, and it is why alice receiving
	// carol's replay would have been a cross-user data leak rather than a
	// duplicated upload. Bob's account is unaffected by either.
	if got := pg.assetCount(t, idemAccountOne); got != 1 {
		t.Errorf("alice's account owns %d assets, want 1 — a second user in one account uploading the same checksum resolves to the duplicate", got)
	}
	if got := pg.assetCount(t, idemAccountTwo); got != 1 {
		t.Errorf("bob's account owns %d assets, want 1 — bob's own upload, and nothing of alice's", got)
	}
}

// TestTheIdempotencyKeyIsScopedToTheEndpoint proves the third leg of the primary key: the
// same principal and the same key on a different operation is a different
// reservation, so a client that reuses one key across operations does not get
// another operation's response.
func TestTheIdempotencyKeyIsScopedToTheEndpoint(t *testing.T) {
	pg := newLedger(t)
	as := map[string]any{"account_id": idemAccountOne}

	first := pg.do(t, pg.keyed(t, http.MethodPost, "/v1/uploads", create("photo.png"), "key-5", as))
	if first.status != http.StatusCreated {
		t.Fatalf("the upload answered %d, want 201: %s", first.status, first.body)
	}

	// `POST /v1/uploads/{id}/complete` with the same key: the same key, a
	// different endpoint, and a fresh reservation.
	second := pg.do(t, pg.keyed(t, http.MethodPost, "/v1/uploads/3f6c1e6c-0000-4000-8000-000000000001/complete",
		map[string]any{"checksum": idemChecksum}, "key-5", as))
	if second.Header().Get("Idempotency-Replayed") != "" {
		t.Errorf("a different endpoint replayed the upload's response: %s", second.body)
	}
	for _, one := range pg.rows(t) {
		if one.endpoint == "POST /v1/uploads" && one.statusCode != http.StatusCreated {
			t.Errorf("the upload's recorded status is %d, want 201", one.statusCode)
		}
	}
}

// TestAnUnauthenticatedIdempotencyRequestReservesNothing is the LAYER ORDER for this
// layer: the credential gate is outside the ledger, so an anonymous caller cannot
// burn a key or read a recorded response.
func TestAnUnauthenticatedIdempotencyRequestReservesNothing(t *testing.T) {
	pg := newLedger(t)

	rec := newRecorder()
	r := request(http.MethodPost, "/v1/uploads", create("photo.png"))
	r.Header.Set("Idempotency-Key", "key-6")
	pg.router.ServeHTTP(rec, r)
	rec.read()

	if rec.status != http.StatusUnauthorized {
		t.Fatalf("an anonymous keyed request answered %d, want 401: %s", rec.status, rec.body)
	}
	if rows := pg.rows(t); len(rows) != 0 {
		t.Errorf("an anonymous request wrote %d ledger rows, want 0: the gate is outside this layer", len(rows))
	}
}

// TestAnOversizedBodyIsStill413ThroughTheIdempotencyLedger is the body limit, seen from
// inside this layer. The ledger reads the body to hash it, and a read that hits
// the ceiling must leave the handler able to answer the DOCUMENT's 413 rather
// than a truncated parse.
func TestAnOversizedBodyIsStill413ThroughTheIdempotencyLedger(t *testing.T) {
	pg := newLedger(t)

	// VALID JSON that is over the limit, and a length the layer above cannot know:
	// `MaxBytesReader` is what fails, exactly as the chunked case `decodeJSON`
	// documents. Both halves matter — a body of bare `a` bytes would be refused as
	// malformed syntax (a 400 from `decodeJSON`) before the reader ever hit the
	// ceiling, which would make this test pass for the wrong reason.
	padded := create("photo.png")
	padded["filename"] = strings.Repeat("a", httpapi.MaxBodyBytes)
	oversized := asCaller(t, pg.id, request(http.MethodPost, "/v1/uploads", padded))
	oversized.Header.Set("Idempotency-Key", "key-7")
	encoded, err := json.Marshal(padded)
	if err != nil {
		t.Fatalf("marshalling the oversized body: %v", err)
	}
	if int64(len(encoded)) <= httpapi.MaxBodyBytes {
		t.Fatalf("the oversized body is %d bytes, which is not over the %d limit — this test would prove nothing", len(encoded), httpapi.MaxBodyBytes)
	}
	oversized.Body = newReadCloser(encoded)
	oversized.ContentLength = -1
	rec := newRecorder()
	pg.router.ServeHTTP(rec, oversized)
	rec.read()

	if rec.status != http.StatusRequestEntityTooLarge {
		t.Fatalf("a keyed oversized request answered %d, want 413: %s", rec.status, rec.body)
	}
	// NO ROW AT ALL, and this is the sharpest thing the ledger has to get right.
	//
	// The layer reads the body to hash it, and here that read FAILS partway through
	// the ceiling. Reserving on a hash of half a body would be a reservation whose
	// `request_hash` no retry could ever match — so every subsequent request under
	// that key would be a 409 `idempotency_key_reused` until the 24-hour sweep, for a
	// client that never sent a broken body. So an unreadable body reserves nothing
	// and falls through to the handler, which answers the document's 413 itself.
	if rows := pg.rows(t); len(rows) != 0 {
		t.Fatalf("the ledger holds %d rows for a body that could not be read, want 0: %+v", len(rows), rows)
	}

	// AND THE KEY IS STILL USABLE, which is the observable consequence of reserving
	// nothing rather than something.
	retry := pg.do(t, pg.keyed(t, http.MethodPost, "/v1/uploads", create("photo.png"), "key-7", map[string]any{"account_id": idemAccountOne}))
	if retry.status != http.StatusCreated {
		t.Fatalf("the retry after a 413 answered %d, want 201 — an unreadable body must not poison the key: %s", retry.status, retry.body)
	}
}

// ----------------------------------------------------------------------------
// helpers
// ----------------------------------------------------------------------------

// newReadCloser is a body whose length the request does not know.
//
// `decodeJSON`'s own header calls this the CHUNKED case: `bodyLimit` refuses an
// oversized body up front only when `ContentLength` is over the limit, and with an
// unknown length the refusal falls to `MaxBytesReader` failing inside the handler.
// A body the request already knows the length of would be refused by the layer above
// the ledger, and this test is about the other half.
func newReadCloser(b []byte) io.ReadCloser {
	return io.NopCloser(bytes.NewReader(b))
}

// sameValue reports whether two bodies are the same JSON value. See the file
// header: `response_body` is `jsonb`, so byte comparison against the ORIGINAL
// response is not available and semantic comparison is the contract.
func sameValue(t *testing.T, a, b []byte) bool {
	t.Helper()

	var left, right any
	if err := json.Unmarshal(a, &left); err != nil {
		t.Fatalf("%q is not JSON: %v", a, err)
	}
	if err := json.Unmarshal(b, &right); err != nil {
		t.Fatalf("%q is not JSON: %v", b, err)
	}
	return reflect.DeepEqual(left, right)
}

// mentionsField reports whether a problem document's `errors` names `field`. The
// document's `Problem` is the generated struct and this file does not depend on
// its spelling, so the assertion is on the bytes.
func mentionsField(t *testing.T, body []byte, field string) bool {
	t.Helper()

	var decoded struct {
		Errors []struct {
			Field string `json:"field"`
		} `json:"errors"`
	}
	if err := json.Unmarshal(body, &decoded); err != nil {
		t.Fatalf("the body is not a problem document: %v\n%s", err, body)
	}
	if len(decoded.Errors) == 0 {
		t.Fatalf("the refusal names no field: %s", body)
	}
	for _, one := range decoded.Errors {
		if one.Field == field {
			return true
		}
	}
	return false
}
