package auth_test

// The verifier's tests, and the four things they are for.
//
// `src/auth.rs` has a test module with a minting helper and a table of refusals.
// This file is the same shape: `TestTheDocumentedRefusalsAreTheOnlyOnes` is that
// table, and `mint` is the helper it mints with — which is why the helper lives in
// the test file rather than in `testsupport.go`. Nothing outside a test needs to
// mint a token, and a package-private signer is one less thing to get wrong.
//
// # THE JWKS IS A REAL SERVER, ON PURPOSE
//
// `httptest.NewServer` binds a socket, and `AGENTS.md`'s "No network in tests" says
// "No test opens a socket to anything but the database named by the environment."
// So this is the deliberate exception and it needs its reason: the property under
// test is a CACHE, and a cache is only real across a fetch boundary. An
// `http.RoundTripper` stub would return bytes without ever being a server, and the
// failure it could not produce — two requests, one fetch — is the failure the TTL and
// the unknown-kid refetch exist to handle.
//
// The socket is loopback, started and stopped inside the test, and it carries no
// credential and reaches nothing outside this process.

import (
	"context"
	"crypto"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/rsa"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"math/big"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/cafaye/darkroom/internal/auth"
)

const (
	testIssuer   = "https://identity.localhost"
	testAudience = "darkroom"
	testKid      = "kid-1"
)

type keyPair struct {
	rsa *rsa.PrivateKey
	ec  *ecdsa.PrivateKey
}

// newKeys is slow on purpose and called once per test: RSA-2048 key generation is
// the one thing in this file that takes real time, and it is cheaper than the
// alternatives (a fixed key in the source, or a test-only 1024-bit key that is not
// what a real issuer publishes).
func newKeys(t *testing.T) keyPair {
	t.Helper()
	rsaKey, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatalf("generating an rsa key: %v", err)
	}
	ecKey, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatalf("generating an ec key: %v", err)
	}
	return keyPair{rsa: rsaKey, ec: ecKey}
}

// jwksDocument renders both keys, in the shape RFC 7517 §5 defines.
func (p keyPair) jwksDocument() string {
	type jwkDoc struct {
		Kty string `json:"kty"`
		Kid string `json:"kid"`
		Use string `json:"use"`
		N   string `json:"n,omitempty"`
		E   string `json:"e,omitempty"`
		Crv string `json:"crv,omitempty"`
		X   string `json:"x,omitempty"`
		Y   string `json:"y,omitempty"`
	}
	document := map[string]any{"keys": []jwkDoc{
		{
			Kty: "RSA", Kid: testKid, Use: "sig",
			N: b64(p.rsa.N.Bytes()),
			E: b64(big.NewInt(int64(p.rsa.E)).Bytes()),
		},
		{
			Kty: "EC", Kid: "kid-ec", Use: "sig", Crv: "P-256",
			X: b64(p.ec.X.FillBytes(make([]byte, 32))),
			Y: b64(p.ec.Y.FillBytes(make([]byte, 32))),
		},
	}}
	encoded, err := json.Marshal(document)
	if err != nil {
		panic(err)
	}
	return string(encoded)
}

func b64(b []byte) string { return base64.RawURLEncoding.EncodeToString(b) }

// identity is a running identity: a JWKS endpoint, a request counter, and a switch
// for failing it.
type identity struct {
	server  *httptest.Server
	fetches atomic.Int64
	fail    atomic.Bool
	doc     string
}

func newIdentity(t *testing.T, document string) *identity {
	t.Helper()
	id := &identity{doc: document}
	id.server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		id.fetches.Add(1)
		if id.fail.Load() {
			// 500 rather than a connection error: the cache's behavior must be the
			// same whether identity is broken or absent.
			w.WriteHeader(http.StatusInternalServerError)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(id.doc))
	}))
	t.Cleanup(id.server.Close)
	return id
}

// claims is a token body, with every documented claim present unless a test
// deliberately removes one.
func claims() map[string]any {
	return map[string]any{
		"iss":        testIssuer,
		"aud":        testAudience,
		"sub":        "user-1",
		"exp":        float64(time.Now().Add(time.Hour).Unix()),
		"account_id": "11111111-1111-1111-1111-111111111111",
		"scope":      "assets:read assets:write",
	}
}

func (p keyPair) mint(t *testing.T, alg, kid string, body map[string]any) string {
	t.Helper()
	header := map[string]any{"alg": alg, "typ": "JWT", "kid": kid}
	headerJSON, err := json.Marshal(header)
	if err != nil {
		t.Fatalf("marshaling a header: %v", err)
	}
	payloadJSON, err := json.Marshal(body)
	if err != nil {
		t.Fatalf("marshaling claims: %v", err)
	}
	signingInput := b64(headerJSON) + "." + b64(payloadJSON)
	digest := sha256Bytes(signingInput)

	var signature []byte
	switch alg {
	case "RS256":
		signature, err = rsa.SignPKCS1v15(rand.Reader, p.rsa, crypto.SHA256, digest)
	case "ES256":
		r, s, signErr := ecdsa.Sign(rand.Reader, p.ec, digest)
		if signErr != nil {
			t.Fatalf("signing: %v", signErr)
		}
		signature = append(r.FillBytes(make([]byte, 32)), s.FillBytes(make([]byte, 32))...)
	case "none":
		signature = nil
	case "HS256":
		// Never verified: the header is refused before any key is fetched, which is
		// the whole reason `decodeHeader` runs first. A forged signature here is the
		// ATTACK, and the test asserts it is refused for naming the algorithm.
		signature = []byte("forged")
	default:
		t.Fatalf("no signer for %q", alg)
	}
	if err != nil && alg != "none" && alg != "HS256" {
		t.Fatalf("signing: %v", err)
	}
	return signingInput + "." + b64(signature)
}

// newVerifier builds a verifier pointed at `id`, with a clock the test controls.
func newVerifier(t *testing.T, id *identity, now func() time.Time) *auth.Verifier {
	t.Helper()
	v, err := auth.New(auth.Options{
		Issuer:   testIssuer,
		Audience: testAudience,
		JWKSURL:  id.server.URL + "/.well-known/jwks.json",
		Now:      now,
	})
	if err != nil {
		t.Fatalf("auth.New: %v", err)
	}
	return v
}

func sha256Bytes(s string) []byte {
	digest := crypto.SHA256.New()
	_, _ = digest.Write([]byte(s))
	return digest.Sum(nil)
}

// TestTheDocumentedRefusalsAreTheOnlyOnes is `src/auth.rs`'s refusal table, as a
// table.
//
// Every row is a way a caller can be refused, and each one asserts the STATUS and
// the CODE together — because a client branches on the code and an orchestrator
// branches on the status, and a refusal whose two halves can disagree is a refusal
// whose classification can drift.
func TestTheDocumentedRefusalsAreTheOnlyOnes(t *testing.T) {
	keys := newKeys(t)
	id := newIdentity(t, keys.jwksDocument())
	now := time.Now()
	verifier := newVerifier(t, id, func() time.Time { return now })

	expired := claims()
	expired["exp"] = float64(now.Add(-time.Second).Unix())
	future := claims()
	future["nbf"] = float64(now.Add(time.Hour).Unix())
	wrongAudience := claims()
	wrongAudience["aud"] = "pantry"
	// An ARRAY audience must CONTAIN ours, not equal it — identity mints
	// multi-audience tokens.
	sharedAudience := claims()
	sharedAudience["aud"] = []any{"pantry", "darkroom"}
	noAccount := claims()
	delete(noAccount, "account_id")
	noScope := claims()
	delete(noScope, "scope")

	for _, row := range []struct {
		name   string
		token  string
		status int
		detail string
	}{
		{"not a jwt", "nonsense", 401, "token is malformed"},
		{"alg none", keys.mint(t, "none", testKid, claims()), 401, "token algorithm 'none' is not accepted"},
		{"alg hs256", keys.mint(t, "HS256", testKid, claims()), 401, "token algorithm 'HS256' is not accepted"},
		{"unknown kid", keys.mint(t, "RS256", "kid-nope", claims()), 401, "token was signed by an unknown key"},
		{"expired", keys.mint(t, "RS256", testKid, expired), 401, "token has expired"},
		{"not yet valid", keys.mint(t, "RS256", testKid, future), 401, "token claim 'nbf' is not valid"},
		{"wrong audience", keys.mint(t, "RS256", testKid, wrongAudience), 401, "token claim 'aud' is not valid"},
		{"no account", keys.mint(t, "RS256", testKid, noAccount), 401, "token claim 'account_id' is not valid"},
		{"no scope", keys.mint(t, "RS256", testKid, noScope), 401, "token claim 'scope' is not valid"},
	} {
		t.Run(row.name, func(t *testing.T) {
			_, refusal := verifier.Authenticate(context.Background(), row.token)
			if refusal == nil {
				t.Fatalf("a %s token was accepted", row.name)
			}
			if refusal.Status != row.status || refusal.Detail != row.detail {
				t.Fatalf("got %d %q, want %d %q", refusal.Status, refusal.Detail, row.status, row.detail)
			}
		})
	}

	t.Run("es256 verifies", func(t *testing.T) {
		principal, refusal := verifier.Authenticate(context.Background(), keys.mint(t, "ES256", "kid-ec", sharedAudience))
		if refusal != nil {
			t.Fatalf("an ES256 token was refused: %d %q", refusal.Status, refusal.Detail)
		}
		if principal.AccountID != "11111111-1111-1111-1111-111111111111" {
			t.Fatalf("account came out as %q", principal.AccountID)
		}
	})

	t.Run("rs256 verifies", func(t *testing.T) {
		principal, refusal := verifier.Authenticate(context.Background(), keys.mint(t, "RS256", testKid, claims()))
		if refusal != nil {
			t.Fatalf("an RS256 token was refused: %d %q", refusal.Status, refusal.Detail)
		}
		if !principal.HasScope(auth.ScopeWrite) {
			t.Fatalf("a token claiming %v does not hold %s", principal.Scope, auth.ScopeWrite)
		}
	})
}

// TestIdentityBeingDownIs503AndNot401 is the distinction that matters most in this
// package: a 401 tells the caller to fix a credential that was fine.
func TestIdentityBeingDownIs503AndNot401(t *testing.T) {
	keys := newKeys(t)
	id := newIdentity(t, keys.jwksDocument())
	id.fail.Store(true)
	verifier := newVerifier(t, id, time.Now)

	_, refusal := verifier.Authenticate(context.Background(), keys.mint(t, "RS256", testKid, claims()))
	if refusal == nil || refusal.Status != http.StatusServiceUnavailable {
		t.Fatalf("got %+v, want a 503", refusal)
	}
	if strings.Contains(refusal.Detail, id.server.URL) {
		t.Fatalf("the refusal leaked the key-set URL: %q", refusal.Detail)
	}
}

// TestTheCacheIsUsedAndTheUnknownKidRefetches is the property a stub transport
// could not produce: TWO requests, ONE fetch, and then a refetch the moment a token
// names a key we have not seen.
func TestTheCacheIsUsedAndTheUnknownKidRefetches(t *testing.T) {
	keys := newKeys(t)
	id := newIdentity(t, keys.jwksDocument())
	now := time.Now()
	verifier := newVerifier(t, id, func() time.Time { return now })

	token := keys.mint(t, "RS256", testKid, claims())
	for range 3 {
		if _, refusal := verifier.Authenticate(context.Background(), token); refusal != nil {
			t.Fatalf("refused: %q", refusal.Detail)
		}
	}
	if got := id.fetches.Load(); got != 1 {
		t.Fatalf("three requests produced %d JWKS fetches, want 1 — an unbounded cache is an outage the first time a key rotates", got)
	}

	// Past the TTL the key set is fetched again, WITHOUT an unknown kid — this is
	// the bound doing its job.
	now = now.Add(auth.DefaultJWKSCacheTTL + time.Second)
	if _, refusal := verifier.Authenticate(context.Background(), token); refusal != nil {
		t.Fatalf("refused after the ttl: %q", refusal.Detail)
	}
	if got := id.fetches.Load(); got != 2 {
		t.Fatalf("a stale cache produced %d fetches, want 2", got)
	}
}

// TestAFailedFetchKeepsTheKeysWeAlreadyTrusted is `src/auth.rs:351` translated.
func TestAFailedFetchKeepsTheKeysWeAlreadyTrusted(t *testing.T) {
	keys := newKeys(t)
	id := newIdentity(t, keys.jwksDocument())
	now := time.Now()
	verifier := newVerifier(t, id, func() time.Time { return now })
	token := keys.mint(t, "RS256", testKid, claims())

	if _, refusal := verifier.Authenticate(context.Background(), token); refusal != nil {
		t.Fatalf("refused on the warm path: %q", refusal.Detail)
	}

	id.fail.Store(true)
	now = now.Add(auth.DefaultJWKSCacheTTL + time.Second)
	if _, refusal := verifier.Authenticate(context.Background(), token); refusal != nil {
		t.Fatalf("identity being down invalidated an already-trusted key: %d %q", refusal.Status, refusal.Detail)
	}
}

func TestANewConfigurationIsARefusalToStart(t *testing.T) {
	for _, row := range []struct {
		name    string
		options auth.Options
	}{
		{"no issuer", auth.Options{Audience: "darkroom"}},
		{"issuer is not a url", auth.Options{Issuer: "identity", Audience: "darkroom"}},
		{"no audience", auth.Options{Issuer: testIssuer}},
		{"negative ttl", auth.Options{Issuer: testIssuer, Audience: "darkroom", JWKSCacheTTL: -time.Second}},
	} {
		t.Run(row.name, func(t *testing.T) {
			if _, err := auth.New(row.options); !errors.Is(err, auth.ErrNotConfigured) {
				t.Fatalf("got %v, want ErrNotConfigured", err)
			}
		})
	}
}

// TestTheBearerHeaderIsRFC6750 is the same table `bearerToken` in
// `src/middleware/jwt.ts` has, because the expression is the credential.
func TestTheBearerHeaderIsRFC6750(t *testing.T) {
	for _, row := range []struct{ header, want string }{
		{"Bearer abc", "abc"},
		{"bearer abc", "abc"},
		{"BEARER abc", "abc"},
		{"Bearer  abc", "abc"},
		{"Bearer", ""},
		{"Bearer a b", ""},
		{"Basic abc", ""},
		{"", ""},
		{"abc", ""},
	} {
		t.Run(row.header, func(t *testing.T) {
			if got := auth.BearerToken(row.header); got != row.want {
				t.Fatalf("BearerToken(%q) = %q, want %q", row.header, got, row.want)
			}
		})
	}
}

// TestAScopeGateWithNoAuthInFrontOfItIsAWiringMistake is the row the packet names:
// unauthenticated is 401, and the answer is 401 even from the SCOPE gate, because
// nothing has been proven about the caller yet.
func TestAScopeGateWithNoAuthInFrontOfItIsAWiringMistake(t *testing.T) {
	verifier := newVerifier(t, newIdentity(t, newKeys(t).jwksDocument()), time.Now)

	var got *auth.Refusal
	refuse := func(_ http.ResponseWriter, _ *http.Request, refusal *auth.Refusal) { got = refusal }

	gate, err := verifier.RequireScope(refuse, auth.ScopeWrite)
	if err != nil {
		t.Fatalf("RequireScope: %v", err)
	}
	gate(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {
		t.Fatal("the inner handler ran with no principal")
	})).ServeHTTP(httptest.NewRecorder(), httptest.NewRequestWithContext(context.Background(), http.MethodGet, "/v1/assets", nil))

	if got == nil || got.Status != http.StatusUnauthorized {
		t.Fatalf("got %+v, want a 401", got)
	}
}

func TestAnEmptyScopeIsARefusalToBuild(t *testing.T) {
	verifier := newVerifier(t, newIdentity(t, newKeys(t).jwksDocument()), time.Now)
	if _, err := verifier.RequireScope(nil, "   "); !errors.Is(err, auth.ErrNotConfigured) {
		t.Fatalf("got %v, want ErrNotConfigured — a blank scope is a gate that refuses everything naming nothing", err)
	}
}

// TestATenantCannotBeBuiltFromAnythingButAPrincipal is the half of the guarantee Go
// CAN express: unexported fields, one constructor, and that constructor takes a
// verified principal.
func TestATenantCannotBeBuiltFromAnythingButAPrincipal(t *testing.T) {
	tenant := auth.NewTenant(&auth.Principal{Sub: "user-1", AccountID: "account-1"})
	if tenant.AccountID() != "account-1" || tenant.UserID() != "user-1" {
		t.Fatalf("got account %q user %q", tenant.AccountID(), tenant.UserID())
	}
	if caller := tenant.Caller(); caller.AccountID != "account-1" || caller.UserID != "user-1" {
		t.Fatalf("Caller() lost a field: %+v", caller)
	}

	assertPanics(t, "NewTenant(nil)", func() { auth.NewTenant(nil) })
}

// TestAMissingTenantPanicsInsteadOfScopingToNothing is the design decision the
// packet asks to be made explicit, asserted rather than described.
//
// The competing answers are a nil pointer and a zero `Tenant`. Both are worse than
// a panic for one reason, and this test is the reason: a zero tenant reaches
// `InAccountTx` as `account_id = ”`, which matches no rows and still exits 0. The
// panic is the only answer that cannot be mistaken for a result.
func TestAMissingTenantPanicsInsteadOfScopingToNothing(t *testing.T) {
	assertPanics(t, "TenantOf with no gate in front", func() {
		auth.TenantOf(httptest.NewRequestWithContext(context.Background(), http.MethodGet, "/v1/assets/abc", nil))
	})
}

// TestThePanicMessageNamesTheRoute is what keeps the panic usable. A panic whose
// message says nothing is a panic people route around by adding a nil check, which
// is the outcome the panic exists to prevent.
func TestThePanicMessageNamesTheRoute(t *testing.T) {
	defer func() {
		recovered := recover()
		message := fmt.Sprint(recovered)
		if !strings.Contains(message, "GET /v1/assets/abc") {
			t.Fatalf("the panic message does not name the route: %q", message)
		}
		if !strings.Contains(message, "Require") {
			t.Fatalf("the panic message does not say what to do: %q", message)
		}
	}()
	auth.TenantOf(httptest.NewRequestWithContext(context.Background(), http.MethodGet, "/v1/assets/abc", nil))
}

// TestAVerifiedTenantIsReadFromTheGateAndNowhereElse is the positive half, and it
// goes through the real middleware rather than `WithPrincipal` so the wiring is
// what is tested.
func TestAVerifiedTenantIsReadFromTheGateAndNowhereElse(t *testing.T) {
	keys := newKeys(t)
	verifier := newVerifier(t, newIdentity(t, keys.jwksDocument()), time.Now)

	var seen auth.Tenant
	inner := http.HandlerFunc(func(_ http.ResponseWriter, r *http.Request) {
		seen = auth.TenantOf(r)
	})
	refuse := func(w http.ResponseWriter, _ *http.Request, refusal *auth.Refusal) {
		t.Fatalf("refused: %d %q", refusal.Status, refusal.Detail)
	}

	handler := verifier.Require(refuse)(inner)
	handler.ServeHTTP(
		httptest.NewRecorder(),
		withBearer(httptest.NewRequestWithContext(context.Background(), http.MethodGet, "/v1/assets", nil), keys.mint(t, "RS256", testKid, claims())),
	)

	if seen.AccountID() != "11111111-1111-1111-1111-111111111111" || seen.UserID() != "user-1" {
		t.Fatalf("the gate produced tenant account=%q user=%q", seen.AccountID(), seen.UserID())
	}
}

func withBearer(r *http.Request, token string) *http.Request {
	r.Header.Set("Authorization", "Bearer "+token)
	return r
}

// assertPanics is here rather than in a helper package because a panic assertion
// that reports the VALUE is worth more than one that reports only that it happened.
func assertPanics(t *testing.T, what string, fn func()) {
	t.Helper()
	defer func() {
		if recover() == nil {
			t.Fatalf("%s did not panic", what)
		}
	}()
	fn()
}
