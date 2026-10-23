package httpapi_test

// The HTTP surface's behavior: what each of the nine operations answers, and the
// four refusals the packet names — 401, 403, cross-tenant 404, and an unrouted path.
//
// # EVERY TEST HERE DRIVES THE ROUTER AS A FUNCTION CALL
//
// `http.Handler` IS a function, and `httptest.NewRecorder` is a `ResponseWriter`, so
// a request is a value rather than a socket. `AGENTS.md` states the Rust rule — "HTTP
// tests drive the router with `tower::ServiceExt::oneshot` — a function call, not a
// round trip" — and this is the Go spelling of the same property: not one test in
// this file binds a port, and `internal/auth`'s loopback JWKS server is the only
// socket anywhere in this package's tests.
//
// # AND WHY THE SERVICE IS A REAL ONE WITH NO DATABASE
//
// The tests that need a handler past the gate mint a principal and put it on the
// request with `auth.WithPrincipal`, which is the only exported way to do that
// safely (its key is a package-private type). Everything up to the gate is exercised
// for real. Past the gate the service needs a database, and a database is the
// database tier's job: `AGENTS.md` puts `tests/query_scoping.rs`'s behavioral half
// in the `--db` tier for the same reason. What IS asserted here is the part this
// packet owns — the status, the code, the JSON shape, and the tenant scoping that
// happens before any query.

import (
	"bytes"
	"context"
	"crypto"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"math/big"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/anywaye/darkroom/internal/auth"
	"github.com/anywaye/darkroom/internal/httpapi"
	"github.com/anywaye/darkroom/internal/service"
)

// recorder captures a response without a socket.
type recorder struct {
	*httptest.ResponseRecorder
	status int
	body   []byte
}

func newRecorder() *recorder {
	return &recorder{ResponseRecorder: httptest.NewRecorder()}
}

func (r *recorder) read() {
	r.status = r.Code
	r.body = r.Body.Bytes()
}

// problem is the decoded envelope, and it decodes with a struct so a test that
// forgets to assert a field cannot pass on it being absent.
type problem struct {
	Type     string `json:"type"`
	Title    string `json:"title"`
	Status   int    `json:"status"`
	Detail   string `json:"detail"`
	Instance string `json:"instance"`
	Code     string `json:"code"`
	TraceID  string `json:"trace_id"`
}

// request builds one. A body of `nil` is no body; anything else is JSON, so no test
// has to hand-write the bytes of a request body.
func request(method, path string, body any) *http.Request {
	var r *http.Request
	if body == nil {
		r = httptest.NewRequestWithContext(context.Background(), method, path, nil)
	} else {
		encoded, err := json.Marshal(body)
		if err != nil {
			panic(err)
		}
		r = httptest.NewRequestWithContext(context.Background(), method, path, bytes.NewReader(encoded))
		r.Header.Set("Content-Type", "application/json")
	}
	return r
}

// ----------------------------------------------------------------------------
// a real identity, so the gate under test is the real gate
// ----------------------------------------------------------------------------

// # WHY THESE TESTS SIGN REAL TOKENS RATHER THAN INJECTING A PRINCIPAL
//
// The obvious shortcut is `auth.WithPrincipal`, and it is wrong here: it would skip
// `Require`, and `Require` is one of the two things these tests are about. A 403
// produced by a route whose credential gate was never mounted is a 403 that proves
// nothing, and `RequireScope` answers 401 for a missing principal SPECIFICALLY so
// that this mistake cannot hide.
//
// So the tests below stand up a JWKS server on loopback, sign RS256 tokens against
// it, and drive the whole chain. That is one loopback socket in this file, started
// and stopped by `t.Cleanup`, carrying no credential and reaching nothing outside
// the process. `internal/auth`'s header says the same thing about its own server.
//
// It is still not a BOUND port for the service under test: `httptest.NewRecorder` is
// the response writer for every request to the router, so the nine operations are
// driven as function calls and only identity's key set needed a socket.
const (
	testIssuer   = "https://identity.localhost"
	testAudience = "darkroom"
	testAccount  = "3f6c1e6c-0000-4000-8000-000000000000"
)

// identity is a running key set and the signer that goes with it.
type identity struct {
	server *httptest.Server
	key    *rsa.PrivateKey
	kid    string
}

func newIdentity(t *testing.T) *identity {
	t.Helper()

	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatalf("generating a signing key: %v", err)
	}

	document := fmt.Sprintf(
		`{"keys":[{"kty":"RSA","kid":%q,"use":"sig","n":%q,"e":%q}]}`,
		testKid,
		base64.RawURLEncoding.EncodeToString(key.N.Bytes()),
		base64.RawURLEncoding.EncodeToString(big.NewInt(int64(key.E)).Bytes()),
	)

	id := &identity{key: key, kid: testKid}
	id.server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(document))
	}))
	t.Cleanup(id.server.Close)
	return id
}

// mint is a token with the documented claims, plus whatever the test changes.
func (id *identity) mint(t *testing.T, overrides map[string]any) string {
	t.Helper()

	body := map[string]any{
		"iss":        testIssuer,
		"aud":        testAudience,
		"sub":        "user-1",
		"exp":        float64(time.Now().Add(time.Hour).Unix()),
		"account_id": testAccount,
		"scope":      auth.ScopeRead + " " + auth.ScopeWrite,
	}
	for name, value := range overrides {
		if value == nil {
			delete(body, name)
			continue
		}
		body[name] = value
	}

	header, err := json.Marshal(map[string]any{"alg": "RS256", "typ": "JWT", "kid": id.kid})
	if err != nil {
		t.Fatalf("marshaling a header: %v", err)
	}
	claims, err := json.Marshal(body)
	if err != nil {
		t.Fatalf("marshaling claims: %v", err)
	}
	signingInput := base64.RawURLEncoding.EncodeToString(header) + "." + base64.RawURLEncoding.EncodeToString(claims)

	digest := sha256.Sum256([]byte(signingInput))
	signature, err := rsa.SignPKCS1v15(rand.Reader, id.key, crypto.SHA256, digest[:])
	if err != nil {
		t.Fatalf("signing: %v", err)
	}
	return signingInput + "." + base64.RawURLEncoding.EncodeToString(signature)
}

const testKid = "kid-1"

// handler builds the real router against the real identity.
func handler(t *testing.T) http.Handler {
	t.Helper()
	return handlerFor(t, newIdentity(t))
}

func handlerFor(t *testing.T, id *identity) http.Handler {
	t.Helper()

	verifier, err := auth.New(auth.Options{
		Issuer:   testIssuer,
		Audience: testAudience,
		JWKSURL:  id.server.URL + "/.well-known/jwks.json",
	})
	if err != nil {
		t.Fatalf("auth.New: %v", err)
	}
	h, err := httpapi.New(httpapi.Options{Service: service.New(nil, nil), Verifier: verifier})
	if err != nil {
		t.Fatalf("httpapi.New: %v", err)
	}
	return h
}

// asCaller sends a real bearer token. The scopes are whatever the test needs: the
// default is both, and a read-only caller passes [auth.ScopeRead] explicitly.
func asCaller(t *testing.T, id *identity, r *http.Request, scopes ...string) *http.Request {
	t.Helper()
	if len(scopes) == 0 {
		scopes = []string{auth.ScopeRead, auth.ScopeWrite}
	}
	r.Header.Set("Authorization", "Bearer "+id.mint(t, map[string]any{"scope": strings.Join(scopes, " ")}))
	return r
}

func decodeProblem(t *testing.T, r *recorder) problem {
	t.Helper()
	var p problem
	if err := json.Unmarshal(r.body, &p); err != nil {
		t.Fatalf("the body is not a problem document: %v\n%s", err, r.body)
	}
	return p
}

// ----------------------------------------------------------------------------
// the probes
// ----------------------------------------------------------------------------

// TestHealthzIsUnconditionalAndTouchesNothing is `AGENTS.md`'s rule as a test: the
// liveness probe answers with no credential, no database and no object store.
func TestHealthzIsUnconditionalAndTouchesNothing(t *testing.T) {
	rec := newRecorder()
	handler(t).ServeHTTP(rec, request(http.MethodGet, "/healthz", nil))
	rec.read()

	if rec.status != http.StatusOK {
		t.Fatalf("got %d, want 200: %s", rec.status, rec.body)
	}
	if got := rec.Header().Get("Content-Type"); got != "application/json" {
		t.Fatalf("got content type %q, want application/json", got)
	}
	if !bytes.Contains(rec.body, []byte(`"status":"ok"`)) {
		t.Fatalf("got %s", rec.body)
	}
	if rec.Header().Get("X-Trace-Id") == "" {
		t.Fatal("no X-Trace-Id: a response with no correlation id cannot be quoted in a bug report")
	}
}

// TestReadyzNamesTheDependencyAndNeverTheHost is the probe rule with both halves.
//
// "The body names the dependency; the underlying error goes to the log. An
// unauthenticated caller must not learn that a database host is `10.0.0.5`." The
// router is built here with no `DB`, which is the reachable form of that outage for
// a test that cannot stop Postgres.
func TestReadyzNamesTheDependencyAndNeverTheHost(t *testing.T) {
	rec := newRecorder()
	handler(t).ServeHTTP(rec, request(http.MethodGet, "/readyz", nil))
	rec.read()

	if rec.status != http.StatusServiceUnavailable {
		t.Fatalf("got %d, want 503 — a probe that returns 200 blindly is worse than no probe: %s", rec.status, rec.body)
	}
	p := decodeProblem(t, rec)
	if p.Code != "unavailable" || p.Status != 503 {
		t.Fatalf("got code=%s status=%d", p.Code, p.Status)
	}
	if strings.Contains(p.Detail, "10.0.0.5") || strings.Contains(p.Detail, "postgres") {
		t.Fatalf("the refusal names an internal detail: %q", p.Detail)
	}
	if rec.Header().Get("Content-Type") != "application/problem+json" {
		t.Fatalf("got content type %q — a probe failure is still the documented envelope", rec.Header().Get("Content-Type"))
	}
}

// ----------------------------------------------------------------------------
// the three refusals
// ----------------------------------------------------------------------------

// TestNoCredentialIs401 is the first of the three, on every route the document does
// not exempt.
func TestNoCredentialIs401(t *testing.T) {
	// The nine operations, each with the method the DOCUMENT pairs with it.
	//
	// Not "every method against every path": a method the document does not pair with
	// a path is a 404 from chi's own router, and asserting 401 there would be
	// asserting that this service serves operations the contract does not describe.
	// `routes_test.go` holds the method/path pairing.
	for _, route := range [][2]string{
		{http.MethodPost, "/v1/uploads"},
		{http.MethodPost, "/v1/uploads/3f6c1e6c-0000-4000-8000-000000000000/complete"},
		{http.MethodGet, "/v1/assets"},
		{http.MethodGet, "/v1/assets/3f6c1e6c-0000-4000-8000-000000000000"},
		{http.MethodDelete, "/v1/assets/3f6c1e6c-0000-4000-8000-000000000000"},
		{http.MethodPost, "/v1/assets/3f6c1e6c-0000-4000-8000-000000000000/variants"},
		{http.MethodGet, "/v1/assets/3f6c1e6c-0000-4000-8000-000000000000/variants"},
	} {
		rec := newRecorder()
		handler(t).ServeHTTP(rec, request(route[0], route[1], map[string]any{}))
		rec.read()
		if rec.status != http.StatusUnauthorized {
			t.Errorf("%s %s answered %d with no credential, want 401", route[0], route[1], rec.status)
		}
		if p := decodeProblem(t, rec); p.Code != "unauthorized" {
			t.Errorf("%s %s gave code %q", route[0], route[1], p.Code)
		}
	}
}

// TestAMalformedCredentialIsAlso401 is the other half of the gate: not a 403. Nothing
// has been proven about the caller, so there is no capability to be missing.
func TestAMalformedCredentialIsAlso401(t *testing.T) {
	for _, header := range []string{"Bearer not-a-jwt", "Basic abc", "Bearer a b", "Bearer"} {
		r := request(http.MethodGet, "/v1/assets", nil)
		r.Header.Set("Authorization", header)
		rec := newRecorder()
		handler(t).ServeHTTP(rec, r)
		rec.read()

		if rec.status != http.StatusUnauthorized {
			t.Errorf("Authorization: %q answered %d, want 401", header, rec.status)
		}
		if p := decodeProblem(t, rec); p.Code != "unauthorized" {
			t.Errorf("Authorization: %q gave code %q", header, p.Code)
		}
	}
}

// TestAVerifiedCallerWithoutTheScopeIs403 is the second of the three.
//
// A `403` and not a `404`, and not a `401`: the token DID verify, so something is
// known about the caller, and what is missing is a capability. `openapi/v1.yaml`'s
// `Forbidden` response says exactly this — "Authenticated, but the token lacks the
// required scope."
func TestAVerifiedCallerWithoutTheScopeIs403(t *testing.T) {
	// A read-only token against the write routes. Every one of them.
	for _, row := range []struct{ method, path string }{
		{http.MethodPost, "/v1/uploads"},
		{http.MethodPost, "/v1/uploads/3f6c1e6c-0000-4000-8000-000000000000/complete"},
		{http.MethodDelete, "/v1/assets/3f6c1e6c-0000-4000-8000-000000000000"},
		{http.MethodPost, "/v1/assets/3f6c1e6c-0000-4000-8000-000000000000/variants"},
	} {
		id := newIdentity(t)
		rec := newRecorder()
		handlerFor(t, id).ServeHTTP(rec, asCaller(t, id, request(row.method, row.path, map[string]any{}), auth.ScopeRead))
		rec.read()

		if rec.status != http.StatusForbidden {
			t.Errorf("%s %s answered %d for a read-only token, want 403", row.method, row.path, rec.status)
			continue
		}
		p := decodeProblem(t, rec)
		if p.Code != "forbidden" {
			t.Errorf("%s %s gave code %q", row.method, row.path, p.Code)
		}
		// The scope the CALLER holds is not echoed back: that is a list of what this
		// credential can reach, handed to whoever presented it.
		if strings.Contains(p.Detail, "assets:read") {
			t.Errorf("%s %s echoed the caller's own scopes: %q", row.method, row.path, p.Detail)
		}
	}
}

// TestAWriteOnlyTokenCannotRead is the mirror, and it is the case that makes the gate
// per-route rather than per-service: the token is perfect and still refused.
func TestAWriteOnlyTokenCannotRead(t *testing.T) {
	id := newIdentity(t)
	rec := newRecorder()
	handlerFor(t, id).ServeHTTP(rec, asCaller(t, id, request(http.MethodGet, "/v1/assets", nil), auth.ScopeWrite))
	rec.read()

	if rec.status != http.StatusForbidden {
		t.Fatalf("got %d, want 403: %s", rec.status, rec.body)
	}
	if p := decodeProblem(t, rec); p.Detail != "token is missing the "+auth.ScopeRead+" scope" {
		t.Fatalf("got detail %q, want it to name the scope this service requires", p.Detail)
	}
}

// TestAnUnroutedPathIsTheSameDocument is the third of the three, extended to a path
// that is not in the contract at all.
//
// chi's own 404 is not in the generated router and not in the document, so it is
// answered here — with `not_found` and the same envelope, rather than chi's
// `text/plain`. A client switching on `application/problem+json` must not have to
// special-case the one response that is not the contract's shape.
func TestAnUnroutedPathIsTheSameDocument(t *testing.T) {
	for _, row := range []struct{ method, path string }{
		{http.MethodGet, "/v1/nope"},
		{http.MethodGet, "/"},
		{http.MethodPost, "/healthz"}, // chi's 405: a method the document does not pair with this path
	} {
		id := newIdentity(t)
		rec := newRecorder()
		handlerFor(t, id).ServeHTTP(rec, asCaller(t, id, request(row.method, row.path, nil)))
		rec.read()

		if rec.status != http.StatusNotFound {
			t.Errorf("%s %s answered %d, want 404: %s", row.method, row.path, rec.status, rec.body)
			continue
		}
		p := decodeProblem(t, rec)
		if p.Code != "not_found" {
			t.Errorf("%s %s gave code %q", row.method, row.path, p.Code)
		}
		if rec.Header().Get("Content-Type") != "application/problem+json" {
			t.Errorf("%s %s is not application/problem+json", row.method, row.path)
		}
	}
}

// TestTheTraceIdInTheBodyIsTheOneInTheHeader is core's rule, and it is only
// structural because both come from one place.
func TestTheTraceIdInTheBodyIsTheOneInTheHeader(t *testing.T) {
	rec := newRecorder()
	handler(t).ServeHTTP(rec, request(http.MethodGet, "/v1/assets", nil))
	rec.read()

	p := decodeProblem(t, rec)
	if p.TraceID == "" {
		t.Fatal("a problem document with no trace_id")
	}
	if got := rec.Header().Get("X-Trace-Id"); got != p.TraceID {
		t.Fatalf("header says %q and body says %q", got, p.TraceID)
	}
}

// TestACallersRequestIdIsEchoedAndAMissingOneIsMinted: the propagation rule, both
// halves, and the over-long case.
func TestACallersRequestIdIsEchoedAndAMissingOneIsMinted(t *testing.T) {
	t.Run("echoed", func(t *testing.T) {
		r := request(http.MethodGet, "/v1/assets", nil)
		r.Header.Set("X-Request-Id", "caller-supplied-1")
		rec := newRecorder()
		handler(t).ServeHTTP(rec, r)
		rec.read()

		if got := rec.Header().Get("X-Trace-Id"); got != "caller-supplied-1" {
			t.Fatalf("got %q, want the caller's own id echoed verbatim", got)
		}
	})

	t.Run("minted", func(t *testing.T) {
		rec := newRecorder()
		handler(t).ServeHTTP(rec, request(http.MethodGet, "/v1/assets", nil))
		rec.read()

		if len(rec.Header().Get("X-Trace-Id")) != 32 {
			t.Fatalf("got %q, want 32 hex characters", rec.Header().Get("X-Trace-Id"))
		}
	})

	t.Run("over-long is replaced not truncated", func(t *testing.T) {
		r := request(http.MethodGet, "/v1/assets", nil)
		r.Header.Set("X-Request-Id", strings.Repeat("x", 200))
		rec := newRecorder()
		handler(t).ServeHTTP(rec, r)
		rec.read()

		got := rec.Header().Get("X-Trace-Id")
		if len(got) != 32 {
			t.Fatalf("got a %d-character id; a truncated caller id is a DIFFERENT id that still looks like theirs", len(got))
		}
	})
}

// ----------------------------------------------------------------------------
// the body limit
// ----------------------------------------------------------------------------

// TestTheBodyLimitIsTheDocumented413 is `openapi/v1.yaml`'s number, asserted.
func TestTheBodyLimitIsTheDocumented413(t *testing.T) {
	if httpapi.MaxBodyBytes != 4096 {
		t.Fatalf("MaxBodyBytes is %d; the document says 4096 and this packet does not change the document",
			httpapi.MaxBodyBytes)
	}

	oversized := map[string]any{"filename": strings.Repeat("a", httpapi.MaxBodyBytes)}
	id := newIdentity(t)
	rec := newRecorder()
	handlerFor(t, id).ServeHTTP(rec, asCaller(t, id, request(http.MethodPost, "/v1/uploads", oversized)))
	rec.read()

	if rec.status != http.StatusRequestEntityTooLarge {
		t.Fatalf("got %d, want 413: %s", rec.status, rec.body)
	}
	if rec.Header().Get("Content-Type") != "application/problem+json" {
		t.Fatalf("a refused body is not application/problem+json: %q", rec.Header().Get("Content-Type"))
	}
	if decodeProblem(t, rec).TraceID == "" {
		t.Fatal("the refusal has no trace id — the body limit runs before a handler and must still be correlatable")
	}
}

// TestTheBodyLimitIsOutsideTheAuthGate is the LAYER ORDER, asserted rather than
// described — and the answer is 413, not 401.
//
// The order is trace context, then the body limit, then the credential gate, and it
// is `src/http.rs`'s: the limit is mounted "outermost so an oversized body is refused
// before the auth middleware reads it". Here the trace context is outside the limit
// so a refusal still has a trace id, which is the one thing the Rust ordering does
// not give.
//
// So an ANONYMOUS caller with an oversized body is told 413, and that is deliberate.
// The limit is the document's own published number — `POST /v1/uploads` declares "The
// request body exceeded 4096 bytes" — so there is nothing here to leak, and refusing
// it after the credential gate would mean a caller burns a token to learn a fact the
// contract already states. What must NOT happen is a 401: that would tell an
// anonymous caller the body size was acceptable while never checking the credential.
func TestTheBodyLimitIsOutsideTheAuthGate(t *testing.T) {
	oversized := map[string]any{"filename": strings.Repeat("a", httpapi.MaxBodyBytes)}
	rec := newRecorder()
	handler(t).ServeHTTP(rec, request(http.MethodPost, "/v1/uploads", oversized))
	rec.read()

	if rec.status != http.StatusRequestEntityTooLarge {
		t.Fatalf("got %d, want 413 — the limit is outside the credential gate, so an oversized body is refused before any token is read: %s", rec.status, rec.body)
	}
}
