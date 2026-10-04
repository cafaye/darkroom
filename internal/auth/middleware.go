// The two gates, as `net/http` middleware, and where the principal goes.
//
// # WHERE THE PRINCIPAL GOES
//
// `src/auth.rs` is a `FromRequestParts` extractor, and axum REFUSES a request whose
// extractor fails before the handler runs. Go's `http.Request` has no per-request
// context bag a caller can extend without wrapping the request, so the principal
// travels in the request CONTEXT — which is what a context is for.
//
// The cost of that is stated here rather than discovered later: **a context is
// optional.** `src/auth.rs`'s `Tenant` type had exactly one constructor, took a
// verified `Principal`, and a handler could not compile without one — the whole
// tenant guarantee was the TYPE SYSTEM, and Go has no dependent types to express
// it. So darkroom splits the guarantee in two and holds each half with the
// strongest thing Go has:
//
//  1. `Tenant` is a struct with UNEXPORTED fields and one constructor, and that
//     constructor takes a verified `Principal`. It cannot be built from a string,
//     an id in a request body, or a header. That half IS expressible and
//     `tenant.go` is it.
//  2. The MANDATORY half — that a handler obtains one — is [TenantOf], which
//     PANICS rather than returning a zero `Tenant`. See that function for why a
//     panic and not a nil.
//
// A package-level `currentPrincipal` would be the alternative and it is refused by
// the same comment `../cafaye/gateway/internal/auth/middleware.go` writes: it is
// one answer shared by every in-flight request, and the bug it produces is a
// tenant seeing another tenant's identity — silent, and only reproducible under
// concurrency.
package auth

import (
	"context"
	"crypto/elliptic"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"math/big"
	"net/http"
	"strings"
)

type principalContextKey struct{}

// withPrincipal is the only writer of the principal into a request.
func withPrincipal(r *http.Request, principal *Principal) *http.Request {
	return r.WithContext(context.WithValue(r.Context(), principalContextKey{}, principal))
}

// PrincipalOf is the verified caller on a request, or nil.
//
// Nil is a real answer and not a bug: the only handlers that read it sit behind a
// gate that either sets it or answered 401, and [Verifier.RequireScope] refuses a
// missing principal rather than assuming one. It exists for exactly the two things
// [TenantOf] cannot do — reporting a principal's claims, and answering "was anyone
// authenticated" — and a handler that needs an account MUST go through [TenantOf].
func PrincipalOf(r *http.Request) *Principal {
	principal, _ := r.Context().Value(principalContextKey{}).(*Principal)
	return principal
}

// WithPrincipal returns a request carrying an already-verified principal.
//
// # WHY IT IS EXPORTED
//
// `internal/httpapi` tests its handlers by minting principals directly rather than
// running a JWKS server per test, and this is the only way to do that safely. The
// key is a package-private type, so nothing outside this package can write one: a
// `context.WithValue` with a string key would be forgeable by anything importing
// `context`, and the `Tenant` minted from it would be a signed statement about a
// caller nobody verified. The name says what it is — an already-VERIFIED principal
// — because the only honest way to export this is to refuse the reading where the
// caller mints a `Principal` themselves and hands it to this.
func WithPrincipal(r *http.Request, principal *Principal) *http.Request {
	return withPrincipal(r, principal)
}

// Refuser writes a refusal. `internal/httpapi` supplies it, so that this package
// does not depend on the package that renders the envelope — and the dependency
// runs one way, which is the direction that keeps the verifier testable without an
// HTTP surface.
type Refuser func(w http.ResponseWriter, r *http.Request, refusal *Refusal)

// Require is the bearer-token gate: 401 unless the request carries a token this
// verifier accepts.
func (v *Verifier) Require(refuse Refuser) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			token := BearerToken(r.Header.Get("Authorization"))
			if token == "" {
				refuse(w, r, unauthorized("a bearer token is required"))
				return
			}

			principal, refusal := v.Authenticate(r.Context(), token)
			if refusal != nil {
				refuse(w, r, refusal)
				return
			}

			next.ServeHTTP(w, withPrincipal(r, principal))
		})
	}
}

// RequireScope is 403 for a verified caller without this scope. Mount it after
// [Verifier.Require].
//
// A route with the scope gate and no auth in front of it is a WIRING MISTAKE and
// not an authorization decision: nothing has been proven about the caller yet, so
// the answer is 401 and not 403. `RequireScope` therefore refuses a missing
// principal with a 401 of its own rather than assuming one — which is also what
// makes this function usable in isolation, and what
// `TestAScopeGateWithNoAuthInFrontOfItIsAWiringMistake` asserts.
//
// It returns an error rather than a handler for an empty scope, so a route table
// with a blank scope is a REFUSAL TO BUILD rather than a gate that refuses
// everything with a message naming nothing. `src/auth.rs` validates in the same
// place for the same reason.
func (v *Verifier) RequireScope(refuse Refuser, scope string) (func(http.Handler) http.Handler, error) {
	required := strings.TrimSpace(scope)
	if required == "" {
		return nil, fmt.Errorf("%w: scope must be a non-empty string", ErrNotConfigured)
	}

	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			principal := PrincipalOf(r)
			if principal == nil {
				refuse(w, r, unauthorized("a bearer token is required"))
				return
			}
			if !principal.HasScope(required) {
				// The scope that is missing is darkroom's own configuration and is
				// safe to name. The scopes the caller DOES hold are not echoed back:
				// that would be a list of what this credential can reach, handed to
				// whoever presented it.
				refuse(w, r, forbidden("token is missing the "+required+" scope"))
				return
			}
			next.ServeHTTP(w, r)
		})
	}, nil
}

// The two scopes, from `openapi/v1.yaml`'s `bearerAuth.x-required-scopes`:
//
//	read:  [assets:read]
//	write: [assets:write]
//
// Constants rather than literals at nine call sites, because the document puts them
// in one place and a handler spelling `assets:read` by hand is a ninth place to
// update when it changes.
const (
	ScopeRead  = "assets:read"
	ScopeWrite = "assets:write"
)

// BearerToken is RFC 6750 §2.1, and it is the same expression `bearerToken` in
// `src/middleware/jwt.ts` uses: `/^bearer +(\S+)$/i`.
//
// Case-insensitive scheme, and exactly one token after it — so `Bearer a b` is not
// a credential and `Bearer` alone is not a credential.
func BearerToken(authorization string) string {
	fields := strings.Fields(strings.TrimSpace(authorization))
	if len(fields) != 2 {
		return ""
	}
	if !strings.EqualFold(fields[0], "bearer") {
		return ""
	}
	return fields[1]
}

// The remaining helpers, gathered here because each is used by more than one file
// and each has a decision in it.

func base64URL(segment string) ([]byte, error) {
	// RFC 7515 §2: JWT segments are base64url WITHOUT padding. `RawURLEncoding` is
	// the encoding that matches the spec; `URLEncoding` would reject every token
	// whose length happens not to need padding, which is a subset rather than the
	// whole and is the kind of failure that looks like an issuer problem.
	return base64.RawURLEncoding.DecodeString(segment)
}

func sha256Sum(signingInput string) []byte {
	digest := sha256.Sum256([]byte(signingInput))
	return digest[:]
}

func jsonUnmarshal(data []byte, into any) error { return json.Unmarshal(data, into) }

// decodePayload is the claims set, and it refuses anything that is not a top-level
// JSON OBJECT. jose raises `ERR_JWT_INVALID` for the second case; unmarshalling a
// bare array into a map is an error here for the same reason.
func decodePayload(token string) (map[string]any, error) {
	parts := strings.Split(token, ".")
	if len(parts) != 3 {
		return nil, errors.New("auth: token is malformed")
	}
	raw, err := base64URL(parts[1])
	if err != nil {
		return nil, err
	}

	var claims map[string]any
	if err := json.Unmarshal(raw, &claims); err != nil {
		return nil, err
	}
	if claims == nil {
		return nil, errors.New("auth: a JWT claims set must be a top-level JSON object")
	}
	return claims, nil
}

func bigFromBytes(b []byte) *big.Int { return new(big.Int).SetBytes(b) }

// p256 returns the curve, named rather than inlined because `elliptic.P256` has
// returned the same singleton since Go 1.21 and there is exactly one use.
func p256() elliptic.Curve { return elliptic.P256() }
