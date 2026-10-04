// Bearer-token authentication, verified locally against identity's published
// JWKS with a bounded cache — identity is never on the hot path.
//
// The contract this implements is `openapi/v1.yaml`'s `securitySchemes.bearerAuth`,
// which is the normative statement and not a summary of it:
//
//   - Required claims: `iss`, `aud`, `sub`, `exp`, `account_id`, `scope`.
//   - Accepted algorithms are RS256 and ES256. `alg: none`, HS256, and any
//     algorithm the key does not advertise are rejected outright.
//   - `account_id` is the tenancy boundary and is taken from this token and from
//     nowhere else.
//
// `src/auth.rs` implements the same document and `README.md` documents the
// decisions in full; where the two trees differ in wording, this file cites the
// document and the Rust file is the history.
//
// # WHAT IS HAND-ROLLED, AND WHY
//
// `crypto/rsa`, `crypto/ecdsa`, `crypto/sha256`, `encoding/base64`,
// `encoding/json` and nothing else. The alternative was `golang-jwt/jwt`, and it
// was rejected on this repository's own rule: the observable contract is the SET
// OF REFUSALS, every sentence of which is `src/auth.rs`'s verbatim, and a library
// would give us its own error taxonomy with a translation layer between the token
// and the sentence a caller reads. The translation layer is where a subtle
// divergence would live.
//
// The security-critical operation is still the standard library's: `rsa.
// VerifyPKCS1v15` and `ecdsa.VerifyASN1` are what check the signatures, and there
// is no padding choice, no digest selection and no curve parsing written here
// beyond base64url and a P-256 range check.
package auth

import (
	"context"
	"crypto"
	"crypto/ecdsa"
	"crypto/rsa"
	"errors"
	"fmt"
	"log/slog"
	"math/big"
	"net/http"
	"strings"
	"time"
)

// Options is the verifier's configuration, validated at construction.
type Options struct {
	// The issuer, and nothing else: an origin such as `https://identity.localhost`.
	// Both the expected `iss` and the base the JWKS path is appended to.
	Issuer string

	// The audience every token must be addressed to.
	Audience string

	// Defaults to `{Issuer}` + [JWKSPath]. An operator who publishes keys somewhere
	// other than the issuer's own well-known path means it.
	JWKSURL string

	// Zero means [DefaultJWKSCacheTTL]. "Bounded because an unbounded cache is an
	// outage the first time a key rotates" — `src/auth.rs`, and the reason the
	// unknown-kid refetch is safe.
	JWKSCacheTTL time.Duration

	// Zero means [DefaultJWKSTimeout].
	JWKSTimeout time.Duration

	// Injected so the TTL and the claim checks are testable without sleeping. Nil
	// means `time.Now`.
	Now func() time.Time

	// The client the key-set fetch uses. Nil means a client with no overall
	// timeout, because `JWKSTimeout` is per request and an overall timeout would be
	// a second, unrelated bound on the same operation.
	Client *http.Client

	// Where key-set problems go. Nil means the default logger. A refusal NEVER
	// goes here — see [unavailable].
	Log *slog.Logger
}

// ErrNotConfigured is a configuration this verifier cannot serve. A sentinel so a
// caller can tell its own mistake from a defect in this package, which a bare
// string cannot.
var ErrNotConfigured = errors.New("auth: invalid configuration")

// New builds the verifier, or refuses the configuration.
//
// A refusal here is a refusal to START, which is the point: a typo in the issuer is
// a startup failure rather than a 503 on every request. `src/main.rs` treats a
// misconfigured auth environment as fatal and says why.
func New(options Options) (*Verifier, error) {
	issuer := strings.TrimSuffix(strings.TrimSpace(options.Issuer), "/")
	if issuer == "" {
		return nil, fmt.Errorf("%w: issuer must be a non-empty URL", ErrNotConfigured)
	}
	if scheme, _, found := strings.Cut(issuer, "://"); !found || (scheme != "https" && scheme != "http") {
		return nil, fmt.Errorf("%w: issuer must be an absolute http(s) URL, got %q", ErrNotConfigured, options.Issuer)
	}

	audience := strings.TrimSpace(options.Audience)
	if audience == "" {
		return nil, fmt.Errorf("%w: audience must be a non-empty string", ErrNotConfigured)
	}

	ttl := options.JWKSCacheTTL
	if ttl == 0 {
		ttl = DefaultJWKSCacheTTL
	}
	if ttl < 0 {
		return nil, fmt.Errorf("%w: jwks cache ttl must be positive, got %v", ErrNotConfigured, options.JWKSCacheTTL)
	}

	timeout := options.JWKSTimeout
	if timeout == 0 {
		timeout = DefaultJWKSTimeout
	}
	if timeout < 0 {
		return nil, fmt.Errorf("%w: jwks timeout must be positive, got %v", ErrNotConfigured, options.JWKSTimeout)
	}

	jwksURL := options.JWKSURL
	if jwksURL == "" {
		jwksURL = issuer + JWKSPath
	}

	now := options.Now
	if now == nil {
		now = time.Now
	}
	client := options.Client
	if client == nil {
		client = &http.Client{}
	}
	log := options.Log
	if log == nil {
		log = slog.Default()
	}

	return &Verifier{
		issuer:   issuer,
		audience: audience,
		now:      now,
		keys:     newJWKSCache(jwksURL, ttl, timeout, now, client, log),
		log:      log,
	}, nil
}

// Algorithm names, from `openapi/v1.yaml`'s `bearerAuth`: "Accepted algorithms are
// RS256 and ES256. `alg: none`, HS256, and any algorithm the key does not
// advertise are rejected outright."
//
// Both are constants rather than a set so that a reader of `acceptedAlgorithms`
// sees there are exactly two and where the second one came from. `src/auth.rs`
// accepts the same two: `Algorithm::RS256 => RS256, Algorithm::ES256 => ES256`, and
// its `map_alg` refuses everything else at key-parse time.
const (
	algRS256 = "RS256"
	algES256 = "ES256"
)

// Authenticate is the whole verifier: the header, the key set, the signature, the
// claims, or the reason.
//
// The order is load-bearing in three places, and all three are `src/auth.rs`'s:
//
//   - the header is read BEFORE any key is fetched, because `alg` and `kid` are
//     attacker-controlled and an `alg: none` token must not be able to make
//     darkroom call identity;
//   - the signature is checked BEFORE the claims, because a token that is both
//     expired and wrongly signed is a signature failure, and a caller told "token
//     has expired" for a forged token has been told something false about their
//     own credential;
//   - `exp` is checked with `exp <= now`, so a token one second past its `exp` is
//     expired. Seconds, floored: jose does `Math.floor(Date.now() / 1000)`.
func (v *Verifier) Authenticate(ctx context.Context, token string) (*Principal, *Refusal) {
	kid, alg, refusal := decodeHeader(token)
	if refusal != nil {
		return nil, refusal
	}

	key, err := v.keys.key(ctx, kid)
	if err != nil {
		// The reason goes to the log HERE, because this is the only place the
		// wrapped error exists, and it names a URL and a status that an
		// unauthenticated caller must not learn.
		v.log.Error("jwks unavailable", "error", err)
		if errors.Is(err, errUnknownKid) {
			return nil, unauthorized("token was signed by an unknown key")
		}
		return nil, unavailable()
	}

	public, err := keyOf(*key)
	if err != nil {
		v.log.Error("jwks contains an unusable key", "kid", kid, "error", err)
		// An unusable KEY is the publisher's defect and it is still a 401: the
		// caller's credential did not verify. The distinction from a fetch failure
		// matters — one is retryable and one is not, and guessing wrong either way
		// is a support ticket.
		return nil, unauthorized("token is not valid")
	}

	if err := verifySignature(public, alg, token); err != nil {
		return nil, unauthorized(err.Error())
	}

	claims, err := decodePayload(token)
	if err != nil {
		return nil, unauthorized("token is not valid")
	}

	if refusal := v.checkClaims(claims); refusal != nil {
		return nil, refusal
	}

	return principalOf(claims)
}

// verifySignature is the compact-JWS check for one of the two accepted algorithms.
//
// The algorithm is taken from the token's OWN header, which is why the accepted
// set is a closed switch rather than a table: a verifier that honoured whatever
// `alg` said is the thing this whole package exists to refuse.
func verifySignature(key *publicKey, alg, token string) error {
	parts := strings.Split(token, ".")
	if len(parts) != 3 {
		return errString("token is not valid")
	}

	signature, err := base64URL(parts[2])
	if err != nil || len(signature) == 0 {
		return errString("token is not valid")
	}
	digest := sha256Sum(parts[0] + "." + parts[1])

	switch {
	case alg == algRS256 && key.rsa != nil:
		// RFC 7518 §3.3: RS256 is `RSASSA-PKCS1-v1_5` over `SHA-256`. PKCS#1 v1.5
		// and not PSS, because the algorithm is named in the header and honouring
		// PSS would honour the claim this file exists to refuse.
		// `rsa.VerifyPKCS1v15` is what checks this, and `rsa.PublicKey.Size` is
		// derived from `N` by the standard library on first use — setting it by
		// hand would produce a verification that fails rather than one that passes.
		if err := rsa.VerifyPKCS1v15(key.rsa, crypto.SHA256, digest, signature); err != nil {
			// jose's `ERR_JWS_SIGNATURE_VERIFICATION_FAILED` says one thing about
			// every way the signature can be wrong, and so does this: a caller who
			// could tell "your key is not ours" from "your token was truncated" is
			// probing an oracle.
			return errString("token signature does not verify")
		}
	case alg == algES256 && key.ec != nil:
		// RFC 7518 §3.4: ES256 is ECDSA over P-256 with the RAW r||s concatenation,
		// not a DER SEQUENCE. 64 bytes, and the split is big-endian on each half.
		if len(signature) != 64 {
			return errString("token signature does not verify")
		}
		r := new(big.Int).SetBytes(signature[:32])
		s := new(big.Int).SetBytes(signature[32:])
		if !ecdsa.Verify(key.ec, digest, r, s) {
			return errString("token signature does not verify")
		}
	default:
		// The header named RS256 and the key is an EC key, or the reverse. That is
		// a publisher's mismatch and it is a signature failure rather than an
		// algorithm failure: telling an unauthenticated caller WHICH of the two
		// went wrong is a free oracle for choosing the other.
		return errString("token signature does not verify")
	}
	return nil
}

// errString is a refusal reason as an error, because `verifySignature` is the one
// place in this file whose failure is a sentence rather than a status.
type errString string

func (e errString) Error() string { return string(e) }

// decodeHeader is the protected header, read before anything else.
//
// It runs before any key is fetched, which is the whole point of doing it first: a
// malformed token costs no network, and `alg: none` or `alg: HS256` never reaches
// the fetch.
func decodeHeader(token string) (kid, alg string, refusal *Refusal) {
	segments := strings.Split(token, ".")
	if len(segments) != 3 {
		// jose accepts three (a JWS) or five (a JWE) segments and answers
		// `ERR_JWE_INVALID` for the second; this service has no JWE key and the
		// refusal for one is the refusal for a malformed token.
		return "", "", unauthorized("token is malformed")
	}

	raw, err := base64URL(segments[0])
	if err != nil {
		return "", "", unauthorized("token is malformed")
	}

	var header struct {
		Alg string `json:"alg"`
		Kid string `json:"kid"`
	}
	if err := jsonUnmarshal(raw, &header); err != nil {
		return "", "", unauthorized("token is malformed")
	}

	if header.Alg != algRS256 && header.Alg != algES256 {
		// The offending `alg` IS echoed, and it is safe to echo: it is one of the
		// things the CALLER chose, and naming it is what lets them tell a stale
		// token from a token this deployment will never accept. What is not safe is
		// echoing an EXPECTED algorithm, which is the platform's configuration.
		return "", "", unauthorized("token algorithm '" + header.Alg + "' is not accepted")
	}
	if header.Kid == "" {
		return "", "", unauthorized("token header names no key")
	}
	return header.Kid, header.Alg, nil
}

// checkClaims verifies the claims `openapi/v1.yaml`'s `bearerAuth` names as
// required: `iss`, `aud`, `sub`, `exp`, `account_id`, `scope`.
//
// `account_id` and `scope` are checked HERE rather than by the gate, and that is a
// decision rather than an omission: the document lists both as REQUIRED CLAIMS, so
// a token without either is not a credential this service accepts at all and the
// answer is 401 at the point every other claim is checked. The gateway's verifier
// treats `account_id` as optional because a gateway routes on `sub` as well; darkroom
// cannot, because the account IS the tenancy boundary and a request with no account
// cannot be scoped to anything.
func (v *Verifier) checkClaims(claims map[string]any) *Refusal {
	iss, ok := claims["iss"].(string)
	if !ok || iss != v.issuer {
		return claimRefusal("iss")
	}

	if !audienceHolds(claims["aud"], v.audience) {
		return claimRefusal("aud")
	}

	if sub, ok := claims["sub"].(string); !ok || sub == "" {
		return claimRefusal("sub")
	}

	// `scope` presence, not `scope` membership: a token with no scope claim has not
	// proved it may do anything, and the gate's 403 needs a token to be about.
	if _, ok := claims["scope"]; !ok {
		return claimRefusal("scope")
	}

	// The account, required. See the block comment.
	if account, ok := claims["account_id"].(string); !ok || account == "" {
		return claimRefusal("account_id")
	}

	now := float64(v.now().Unix())

	if raw, present := claims["exp"]; present {
		seconds, ok := raw.(float64)
		if !ok {
			return claimRefusal("exp")
		}
		if seconds <= now {
			return unauthorized("token has expired")
		}
	}

	// `nbf` AFTER `exp`. `src/auth.rs` checks them in the order jose does, and a
	// token that is both not-yet-valid and already expired is an EXPIRED token:
	// telling a caller "not yet valid" about a credential that will never be valid
	// again sends them round the same loop.
	if raw, present := claims["nbf"]; present {
		seconds, ok := raw.(float64)
		if !ok {
			return claimRefusal("nbf")
		}
		if seconds > now {
			return claimRefusal("nbf")
		}
	}

	return nil
}

// claimRefusal is `refusalOf`'s `ERR_JWT_CLAIM_VALIDATION_FAILED` branch: "token
// claim '<name>' is not valid". The name is one of six fixed strings, and naming it
// is what lets a caller tell a clock problem from an addressing problem.
func claimRefusal(claim string) *Refusal {
	return unauthorized(fmt.Sprintf("token claim '%s' is not valid", claim))
}

// audienceHolds is jose's `checkAudiencePresence`: a string `aud` must EQUAL the
// expected audience, and an array `aud` must CONTAIN it.
//
// Containment and not equality is a real distinction: a token minted for
// `["darkroom", "admin"]` is addressed to darkroom, and refusing it would break a
// multi-audience token identity issues for several services at once. Anything that
// is neither a string nor an array of strings fails, which is what stops
// `aud: {"darkroom": true}` from being an audience.
func audienceHolds(raw any, expected string) bool {
	switch value := raw.(type) {
	case string:
		return value == expected
	case []any:
		for _, member := range value {
			text, ok := member.(string)
			if !ok {
				return false
			}
			if text == expected {
				return true
			}
		}
	}
	return false
}

// principalOf is the claims a handler is allowed to see, or the reason there are
// none.
func principalOf(claims map[string]any) (*Principal, *Refusal) {
	scopeText, ok := claims["scope"].(string)
	if !ok {
		// `checkClaims` established the claim is PRESENT. A present, non-string
		// `scope` is a publisher's defect and the refusal is the same one jose
		// raises for a claim of the wrong type.
		return nil, claimRefusal("scope")
	}

	// `strings.Fields` rather than `strings.Split(" ")`, because a scope claim
	// separated by a tab or a double space is a well-formed claim and an empty
	// member is not a scope. Same as `src/auth.rs`.
	scopes := strings.Fields(scopeText)
	if len(scopes) == 0 {
		return nil, claimRefusal("scope")
	}

	return &Principal{
		Sub:       claims["sub"].(string),
		AccountID: claims["account_id"].(string),
		Scope:     scopes,
		Claims:    claims,
	}, nil
}
