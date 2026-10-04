package auth

import (
	"context"
	"crypto/ecdsa"
	"crypto/rsa"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"sync"
	"time"
)

// The JWKS fetch, its bounded cache, and the two rules that make a key rotation
// survivable without a redeploy.
//
// # WHY IT IS HAND-ROLLED
//
// `src/auth.rs` hand-rolls this too, and the Rust README's reason applies verbatim:
// the observable contract is the set of refusals, and a library's error taxonomy
// would sit between the token and the sentence a caller reads. What IS standard
// library is everything security-critical: `rsa.VerifyPKCS1v15` and
// `ecdsa.VerifyASN1` are what check the signatures, and there is no padding choice,
// no digest selection and no curve parsing here beyond base64url and a `P-256`
// range check.
//
// # WHY THE CACHE IS BOUNDED AND WHY A FAILURE KEEPS THE OLD KEYS
//
// `src/auth.rs`: "cache_ttl defaults to five minutes. Bounded because an unbounded
// cache is an outage the first time a key rotates." Two properties are carried over
// because they are the same two problems:
//
//   - The TTL is what makes an UNKNOWN `kid` able to trigger a refetch. A token
//     naming a key we have not seen is the only signal that identity rotated, and
//     that signal is worthless unless it can move the cache. So `keysFor` refetches
//     on an unknown kid even inside a live window.
//   - A failed fetch LEAVES THE PREVIOUS CACHE IN PLACE. identity being down must
//     not invalidate tokens that were already verified against a good key. The
//     bound that makes this safe is the TTL itself: without it, "keep the old keys"
//     would mean "keep the old keys until identity comes back", which is precisely
//     the outage the bound exists to prevent.

// DefaultJWKSCacheTTL is five minutes, and it is the number the Rust tree
// hard-codes at `src/auth.rs:321`. One number in two trees is a decision to keep;
// a second copy of it elsewhere in this repository would be a third that drifts.
const DefaultJWKSCacheTTL = 5 * time.Minute

// DefaultJWKSTimeout bounds ONE fetch. Not an overall client timeout: the budget is
// per request, and a second, unrelated bound on the same operation is a bound
// nobody reasoned about.
const DefaultJWKSTimeout = 5 * time.Second

// JWKSPath is where a key set lives, appended to the issuer.
const JWKSPath = "/.well-known/jwks.json"

// jwk is one key from the document. `kty` is required by RFC 7517 §4.1 and `alg`
// is OPTIONAL — a publisher that omits it has not limited the set, so the only
// thing that limits an algorithm is what the TOKEN says, and `decodeHeader` is the
// only reader of that. Core: "Accepted algorithms: RS256 and ES256. `alg: none`,
// symmetric algorithms, and any algorithm the key does not advertise are rejected
// outright" — and `alg` on the KEY is not consulted for that, because an
// asymmetric key is the thing that makes `alg: HS256` impossible to honour.
type jwk struct {
	Kty string `json:"kty"`
	Kid string `json:"kid"`
	Use string `json:"use"`
	// RSA
	N string `json:"n"`
	E string `json:"e"`
	// EC
	Crv string `json:"crv"`
	X   string `json:"x"`
	Y   string `json:"y"`
}

// usableFor is whether this key can verify a token naming `kid`.
//
// `use` absent or `"sig"` both mean a verification key, per RFC 7517 §4.2. A key
// that says `"enc"` is an ENCRYPTION key and verifying with it would be accepting a
// token nobody asked us to accept.
func (k jwk) usableFor(kid string) bool {
	if k.Kid == "" || k.Kid != kid {
		return false
	}
	return k.Use == "" || k.Use == "sig"
}

// jwksCache is one key set and the moment it was fetched. One per Verifier, and a
// Verifier is one per process: two verifiers would fetch the key set twice and
// could disagree about whether a rotation has happened.
type jwksCache struct {
	url     string
	ttl     time.Duration
	timeout time.Duration
	client  *http.Client
	now     func() time.Time
	log     *slog.Logger

	mu      sync.Mutex
	current []jwk
	fetched time.Time
	cold    bool
}

func newJWKSCache(url string, ttl, timeout time.Duration, now func() time.Time, client *http.Client, log *slog.Logger) *jwksCache {
	return &jwksCache{url: url, ttl: ttl, timeout: timeout, client: client, now: now, log: log, cold: true}
}

// key is THE key for a token naming `kid`, fetching if the cache is cold, stale, or
// has never seen that key id.
//
// The unknown-kid refetch is the whole rotation story and it is why the cache is
// keyed by kid rather than holding one document: a rotation publishes a new kid,
// tokens start naming it, and the first one to arrive is what moves the cache. The
// cost is one fetch per unknown kid, which an unauthenticated caller can trigger —
// so the TTL is what bounds it back down, and a fetch that fails is what bounds it
// further (a kid that does not exist is fetched once per TTL).
func (c *jwksCache) key(ctx context.Context, kid string) (*jwk, error) {
	c.mu.Lock()
	defer c.mu.Unlock()

	if !c.cold && c.now().Sub(c.fetched) < c.ttl {
		if found, ok := selectByKid(c.current, kid); ok {
			return found, nil
		}
		// Unknown kid inside a live window: fall through and refetch. The comment
		// above is the reason this is not "not found".
	}

	if err := c.refreshLocked(ctx); err != nil {
		// `src/auth.rs:351`: "On failure the *previous* cache is left in place". The
		// token was signed by a key this process already trusted, and identity being
		// down must not invalidate it.
		if !c.cold {
			if found, ok := selectByKid(c.current, kid); ok {
				return found, nil
			}
		}
		return nil, err
	}

	found, ok := selectByKid(c.current, kid)
	if !ok {
		return nil, errUnknownKid
	}
	return found, nil
}

// selectByKid returns the ONE key with this id, and refuses an ambiguous set.
//
// Two keys with one `kid` is a publisher that has published an ambiguous set, and
// "token is not valid" is the only answer that does not tell an unauthenticated
// caller which of two keys to try. This is jose's `ERR_JWKS_MULTIPLE_MATCHING_KEYS`
// translated, and `src/auth.rs` makes the same distinction through `jsonwebtoken`.
//
// Zero keys is ALSO `false` rather than a distinct answer here, because whether a
// kid is absent from a set we just refreshed or absent from a set that never loaded
// is one answer to the caller: identity does not have that key.
func selectByKid(set []jwk, kid string) (*jwk, bool) {
	var found []jwk
	for _, key := range set {
		if key.usableFor(kid) {
			found = append(found, key)
		}
	}
	if len(found) != 1 {
		return nil, false
	}
	return &found[0], true
}

// errUnknownKid is a caller that named a key identity that does not exist. It is
// the one thing worth SAYING — the caller can go and look at their issuer — and it
// is 401, not 503: identity answered, and its answer was "I have never heard of
// that key".
var errUnknownKid = errors.New("auth: no key in the set matches the token's kid")

// refreshLocked fetches unconditionally and REPLACES the cache only on success.
//
// Locked by the CALLER rather than by itself, because `key` holds one lock across
// the read-the-cache-and-maybe-refetch decision, and a second `Lock` here would
// deadlock rather than narrow a window.
func (c *jwksCache) refreshLocked(ctx context.Context) error {
	ctx, cancel := context.WithTimeout(ctx, c.timeout)
	defer cancel()

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, c.url, nil)
	if err != nil {
		return fmt.Errorf("auth: building the jwks request: %w", err)
	}
	req.Header.Set("Accept", "application/json")

	resp, err := c.client.Do(req)
	if err != nil {
		return fmt.Errorf("auth: fetching the jwks document: %w", err)
	}
	defer func() {
		_, _ = io.Copy(io.Discard, resp.Body)
		_ = resp.Body.Close()
	}()

	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("auth: the jwks document answered %d", resp.StatusCode)
	}

	// Bounded, because the document is fetched from a URL an operator configured
	// and an unbounded body is a memory cost nobody asked for. 1 MiB is far above
	// any real key set.
	const maxJWKSBytes = 1 << 20
	body, err := io.ReadAll(io.LimitReader(resp.Body, maxJWKSBytes))
	if err != nil {
		return fmt.Errorf("auth: reading the jwks document: %w", err)
	}

	var document struct {
		Keys []jwk `json:"keys"`
	}
	if err := json.Unmarshal(body, &document); err != nil {
		return fmt.Errorf("auth: the jwks document is not valid json: %w", err)
	}

	c.current = document.Keys
	c.fetched = c.now()
	c.cold = false

	// Logged by COUNT and never by key material. `src/auth.rs:293`: the cache "is
	// logged by key id only" — a key id is public, an exponent is not, and a log
	// line is the last place either belongs.
	if c.log != nil {
		c.log.Info("jwks refreshed", "keys", len(c.current))
	}
	return nil
}

// publicKey is a JWK as something the standard library verifies against.
type publicKey struct {
	rsa *rsa.PublicKey
	ec  *ecdsa.PublicKey
}

// keyOf turns a JWK into a key, or refuses it.
//
// The bounds checks are the point rather than decoration, and each one turns a
// malformed document into a refusal at the place the refusal is meaningful instead
// of an overflow or a panic later:
//   - a modulus that is even, or not above 2^32, is not an RSA modulus;
//   - an exponent outside `[3, 2^31-1]` is refused on LENGTH, so a JWK carrying a
//     deliberately enormous `e` cannot overflow into a negative `int`;
//   - a curve other than `P-256` is refused, because ES256 is P-256 and accepting
//     P-384 under the name ES256 would be verifying a signature over a different
//     curve than the algorithm names.
func keyOf(key jwk) (*publicKey, error) {
	switch key.Kty {
	case "RSA":
		return rsaKeyOf(key)
	case "EC":
		return ecKeyOf(key)
	default:
		// `oct` is a SYMMETRIC key. A JWKS that publishes one is how `alg: HS256`
		// gets accepted by a verifier that reads this file, and refusing here is
		// the cheaper half of refusing it: the header check already refused an
		// `alg` that is not RS256 or ES256, so this is the second door.
		return nil, fmt.Errorf("auth: unsupported key type %q", key.Kty)
	}
}

func rsaKeyOf(key jwk) (*publicKey, error) {
	modulus, err := base64URL(key.N)
	if err != nil || len(modulus) == 0 {
		return nil, errors.New("auth: the rsa key has no modulus")
	}
	exponent, err := base64URL(key.E)
	// Four bytes is 2^32 and `rsa.checkPub` refuses an exponent outside
	// `[2, 2^31-1]`, so anything longer is refused here rather than truncated.
	if err != nil || len(exponent) == 0 || len(exponent) > 4 {
		return nil, errors.New("auth: the rsa key has no usable exponent")
	}

	e := 0
	for _, b := range exponent {
		e = e<<8 | int(b)
	}
	if e < 3 || e%2 == 0 {
		return nil, errors.New("auth: the rsa exponent is not a public exponent")
	}

	n := bigFromBytes(modulus)
	if n.Bit(0) == 0 || n.BitLen() < 33 {
		return nil, errors.New("auth: the rsa modulus is not a public modulus")
	}

	return &publicKey{rsa: &rsa.PublicKey{N: n, E: e}}, nil
}

func ecKeyOf(key jwk) (*publicKey, error) {
	if key.Crv != "P-256" {
		return nil, fmt.Errorf("auth: curve %q is not P-256", key.Crv)
	}
	x, err := base64URL(key.X)
	if err != nil || len(x) != 32 {
		return nil, errors.New("auth: the ec key has no usable x coordinate")
	}
	y, err := base64URL(key.Y)
	if err != nil || len(y) != 32 {
		return nil, errors.New("auth: the ec key has no usable y coordinate")
	}

	pub := &ecdsa.PublicKey{Curve: p256(), X: bigFromBytes(x), Y: bigFromBytes(y)}
	// `IsOnCurve` rather than trusting the coordinates: a point off the curve makes
	// `ecdsa.VerifyASN1` return false for reasons that are indistinguishable from a
	// bad signature, and a key we cannot place is a key we should have refused.
	if !pub.Curve.IsOnCurve(pub.X, pub.Y) {
		return nil, errors.New("auth: the ec point is not on P-256")
	}
	return &publicKey{ec: pub}, nil
}

// Verifier is the built verifier. One per process.
type Verifier struct {
	issuer   string
	audience string
	now      func() time.Time
	keys     *jwksCache
	log      *slog.Logger
}

// Principal is what a verified request carries. Handlers read this and nothing
// else.
//
// Ported field for field from `Principal` in `src/auth.rs`, including the one the
// Rust comment explains: the single place that decides what an account is is the
// single place that read the token.
type Principal struct {
	// `sub`. The user, recorded as `owner_user_id`. It NEVER scopes a query.
	Sub string

	// `account_id`. THE TENANCY BOUNDARY. `openapi/v1.yaml`'s `bearerAuth`
	// description: "`account_id` is the tenancy boundary and is taken from this
	// token and from nowhere else. There is no account field in any request body."
	AccountID string

	// The `scope` claim, split on whitespace. Empty when the claim is absent —
	// never everything. "A scope gate that treated 'no scopes' as 'all scopes'
	// would be a gate with no gate", and that sentence is `src/auth.rs`'s.
	Scope []string

	// The remaining claims, unfiltered.
	Claims map[string]any
}

// HasScope is one scope question, asked the one place.
func (p *Principal) HasScope(required string) bool {
	for _, held := range p.Scope {
		if held == required {
			return true
		}
	}
	return false
}

// Refusal is why a caller does not get a principal.
//
// A status and a code rather than an `error`, because the two ARE the answer: a
// 401 and a 503 are different decisions about different subjects, and a refusal
// that had to be re-classified by its message would be a refusal whose
// classification could drift.
type Refusal struct {
	Status int
	Code   string
	Detail string
}

func unauthorized(detail string) *Refusal {
	return &Refusal{Status: http.StatusUnauthorized, Code: "unauthorized", Detail: detail}
}

func forbidden(detail string) *Refusal {
	return &Refusal{Status: http.StatusForbidden, Code: "forbidden", Detail: detail}
}

// unavailable is identity being down. NOT 401, and the reason is worth the
// sentence: a 401 tells the caller to fix a credential that was fine. "The
// underlying error goes to the log" — `AGENTS.md`, verbatim, for the probes, and
// the same rule here because the audience is just as unauthenticated.
func unavailable() *Refusal {
	return &Refusal{
		Status: http.StatusServiceUnavailable,
		Code:   "unavailable",
		Detail: "the signing keys could not be retrieved",
	}
}
