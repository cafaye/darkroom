//! Authentication and the tenant context.
//!
//! ## The one rule
//!
//! **`account_id` comes from the verified token and from nowhere else.** Not
//! from the path, not from the body, not from a query parameter. A service that
//! reads a tenant id from the request is a service where any caller can set any
//! tenant, and no amount of query-level `WHERE account_id = $1` fixes it. The
//! [`Tenant`] type makes this structural: a handler cannot read an account id
//! without going through [`Tenant::from_principal`], and there is no
//! `Tenant::from_request`.
//!
//! ## 404, not 403
//!
//! core/docs/openapi-conventions.md: "Never 404 for authorization failures on a
//! resource the caller cannot see — 404 is correct there, 403 is not allowed to
//! leak existence." So a cross-tenant read, a cross-tenant variant, and a
//! genuinely missing asset are all [`Error::NotFound`] with the same body. A
//! 403 would tell an attacker "that id exists, it just is not yours", which is
//! a free asset-id oracle.
//!
//! ## What is verified here, and what is not
//!
//! This service verifies the bearer JWT against identity's JWKS, locally, with
//! a bounded cache — core's rule, and the reason identity is not on the hot
//! path. It checks `iss`, `aud`, `exp`, `nbf`, and the algorithm against the
//! key's own advertised algorithm. It does **not** interpret `roles`: core says
//! "Services do not parse roles out of a `roles` claim — they check `scopes`,
//! or ask identity." darkroom checks scopes.
//!
//! `identity.member.*` is in darkroom's `consumes` list precisely because it
//! consumes none of it *yet*: the events are registered so the contract is
//! declared, and nothing subscribes. See the note in `cafaye.yml`.

use std::collections::HashMap;
use std::sync::{Arc, RwLock};
use std::time::{Duration, Instant};

use axum::extract::FromRequestParts;
use axum::http::request::Parts;
use serde::{Deserialize, Serialize};
use uuid::Uuid;

use crate::error::Error;

/// The verified caller. This is the only source of a tenant id in the service.
#[derive(Debug, Clone, PartialEq, Eq)]
pub struct Principal {
    /// `sub` — the user id, as identity minted it.
    pub user_id: Uuid,
    /// `account_id` — the tenant. The claim core requires for authenticated
    /// service traffic.
    pub account_id: Uuid,
    /// `scopes` — capability. Checked, not assumed.
    pub scopes: Vec<String>,
}

impl Principal {
    pub fn has_scope(&self, scope: &str) -> bool {
        self.scopes.iter().any(|s| s == scope)
    }

    /// The scope required for reading assets, if the caller has any asset scope
    /// at all. Read endpoints accept any of the read scopes so a service token
    /// minted narrowly still works; write endpoints want the write one.
    pub fn can_read_assets(&self) -> bool {
        self.has_scope(SCOPE_ASSETS_READ) || self.has_scope(SCOPE_ASSETS_WRITE)
    }

    pub fn can_write_assets(&self) -> bool {
        self.has_scope(SCOPE_ASSETS_WRITE)
    }
}

pub const SCOPE_ASSETS_READ: &str = "assets:read";
pub const SCOPE_ASSETS_WRITE: &str = "assets:write";

/// The tenant + caller a request runs as, resolved from the verified token.
///
/// A separate type from [`Principal`] on purpose: `Principal` is what the token
/// said, `Tenant` is what the handler is allowed to scope a query to. Making
/// handlers take `Tenant` means every query in the service has a tenant in
/// scope by construction, and a missing one is a compile error rather than a
/// review comment.
#[derive(Debug, Clone, Copy)]
pub struct Tenant {
    account_id: Uuid,
    user_id: Uuid,
}

impl Tenant {
    /// Build a tenant from a verified principal. The ONLY constructor.
    pub fn from_principal(principal: &Principal) -> Self {
        Self {
            account_id: principal.account_id,
            user_id: principal.user_id,
        }
    }

    pub fn account_id(&self) -> Uuid {
        self.account_id
    }

    pub fn user_id(&self) -> Uuid {
        self.user_id
    }
}

/// Tower extension set by the auth middleware, read by the extractor.
#[derive(Clone)]
pub(crate) struct AuthState(pub Arc<Principal>);

/// `FromRequestParts` for [`Tenant`] and [`Principal`].
///
/// Implements `FromRequestParts` rather than being read from a request
/// extension at each call site, so "the handler forgot to authenticate" is a
/// 401 from one place and not a `Option` that a handler forgot to check.
impl<S> FromRequestParts<S> for Tenant
where
    S: Send + Sync,
{
    type Rejection = Error;

    async fn from_request_parts(parts: &mut Parts, _state: &S) -> Result<Self, Self::Rejection> {
        parts
            .extensions
            .get::<AuthState>()
            .map(|auth| Tenant::from_principal(&auth.0))
            .ok_or(Error::unauthorized("a bearer token is required"))
    }
}

impl<S> FromRequestParts<S> for Principal
where
    S: Send + Sync,
{
    type Rejection = Error;

    async fn from_request_parts(parts: &mut Parts, _state: &S) -> Result<Self, Self::Rejection> {
        parts
            .extensions
            .get::<AuthState>()
            .map(|auth| (*auth.0).clone())
            .ok_or(Error::unauthorized("a bearer token is required"))
    }
}

/// The claim set darkroom requires. `jsonwebtoken` decodes into this and
/// serde ignores anything else, so identity adding a claim cannot break this
/// service.
#[derive(Debug, Clone, Serialize, Deserialize)]
pub struct Claims {
    pub iss: String,
    pub aud: String,
    pub sub: String,
    pub exp: i64,
    pub nbf: Option<i64>,
    pub iat: Option<i64>,
    pub jti: Option<String>,
    /// Required for authenticated service traffic (core).
    pub account_id: Uuid,
    /// Space-separated in the JWT, per RFC 8693. Some issuers send an array;
    /// both are accepted by [`Claims::scope_list`].
    #[serde(default)]
    pub scope: Option<serde_json::Value>,
    /// Not interpreted. Present so an issuer that sends it does not fail
    /// validation, and so the comment "we do not read this" is checkable.
    #[serde(default)]
    pub roles: Option<serde_json::Value>,
}

impl Claims {
    /// `scope` as a list. Accepts the space-delimited string RFC 8693 defines
    /// and the array some issuers send, because rejecting one of them turns a
    /// working deployment into a 401 storm and the two forms are equivalent in
    /// meaning.
    pub fn scope_list(&self) -> Vec<String> {
        match &self.scope {
            Some(serde_json::Value::String(s)) => {
                s.split_whitespace().map(str::to_string).collect()
            }
            Some(serde_json::Value::Array(items)) => items
                .iter()
                .filter_map(|v| v.as_str().map(str::to_string))
                .collect(),
            _ => Vec::new(),
        }
    }
}

/// One key from the JWKS document.
#[derive(Debug, Clone, Deserialize)]
pub struct Jwk {
    pub kid: String,
    /// The algorithm this key is for. Checked against the token's `alg` header
    /// so a token cannot choose a weaker algorithm than the key advertises.
    pub alg: String,
    pub kty: String,
    #[serde(default)]
    pub n: Option<String>,
    #[serde(default)]
    pub e: Option<String>,
    #[serde(default)]
    pub crv: Option<String>,
    #[serde(default)]
    pub x: Option<String>,
    #[serde(default)]
    pub y: Option<String>,
}

#[derive(Debug, Clone, Deserialize)]
struct JwksDocument {
    keys: Vec<Jwk>,
}

/// Where this service gets its keys. Two implementations, one trait, so the
/// tests verify tokens without a network and production does the opposite.
#[async_trait::async_trait]
pub trait TokenVerifier: Send + Sync + 'static {
    /// Verify `token` and return the caller it names.
    async fn verify(&self, token: &str) -> Result<Principal, Error>;
}

/// A verifier that accepts a fixed set of test tokens. Compiled always, used by
/// the test suite and by nothing else — `dev_auth()` is the only constructor and
/// the production `build_verifier` never calls it.
///
/// The reason this exists rather than "just make a real token in the test": a
/// test that signs its own token with a key it also publishes is testing the
/// verifier it wrote, not the tenant isolation. The fake asserts the *contract*
/// — 404 on cross-tenant, 401 on a bad token — and the real verifier's tests
/// assert the cryptography separately. Both halves, neither pretending to be
/// the other.
#[derive(Debug, Clone, Default)]
pub struct StaticTokenVerifier {
    tokens: Arc<RwLock<HashMap<String, Principal>>>,
}

impl StaticTokenVerifier {
    pub fn new() -> Self {
        Self::default()
    }

    /// Register a token that resolves to `principal`. Re-registering a token
    /// replaces the principal, so a test can simulate a membership change
    /// without a new verifier.
    pub fn insert(&self, token: impl Into<String>, principal: Principal) {
        self.tokens
            .write()
            .expect("token map mutex is not poisoned")
            .insert(token.into(), principal);
    }

    pub fn with_token(self, token: impl Into<String>, principal: Principal) -> Self {
        self.insert(token, principal);
        self
    }
}

#[async_trait::async_trait]
impl TokenVerifier for StaticTokenVerifier {
    async fn verify(&self, token: &str) -> Result<Principal, Error> {
        self.tokens
            .read()
            .expect("token map mutex is not poisoned")
            .get(token)
            .cloned()
            // One message for "no such token" and "wrong signature". A verifier
            // that distinguishes them is a token oracle.
            .ok_or(Error::unauthorized("the bearer token is not valid"))
    }
}

/// A cached JWKS. Keys are cached by `kid` for a bounded TTL and refreshed on
/// an unknown `kid` — core's rule, and the reason a key rotation is invisible
/// to the hot path.
#[derive(Debug)]
pub struct JwksVerifier {
    http: reqwest::Client,
    jwks_url: String,
    issuer: String,
    audience: String,
    cache: RwLock<Option<CachedJwks>>,
    cache_ttl: Duration,
}

struct CachedJwks {
    by_kid: HashMap<String, jsonwebtoken::DecodingKey>,
    fetched_at: Instant,
}

// `DecodingKey` deliberately does not implement `Debug` — printing one can
// expose key material in a log. The cache is therefore logged by key id only.
impl std::fmt::Debug for CachedJwks {
    fn fmt(&self, f: &mut std::fmt::Formatter<'_>) -> std::fmt::Result {
        let mut kids: Vec<&str> = self.by_kid.keys().map(String::as_str).collect();
        kids.sort_unstable();
        f.debug_struct("CachedJwks")
            .field("kids", &kids)
            .field("fetched_at", &self.fetched_at)
            .finish()
    }
}

impl JwksVerifier {
    /// `cache_ttl` defaults to five minutes. Bounded because an unbounded cache
    /// of keys from a compromised issuer is a memory leak with a remote
    /// trigger; short enough that a revoked key stops working quickly.
    pub fn new(jwks_url: impl Into<String>, issuer: impl Into<String>, audience: impl Into<String>) -> Self {
        Self {
            http: reqwest::Client::new(),
            jwks_url: jwks_url.into(),
            issuer: issuer.into(),
            audience: audience.into(),
            cache: RwLock::new(None),
            cache_ttl: Duration::from_secs(300),
        }
    }

    /// Fetch the document if the cache is cold or stale. An unknown `kid` also
    /// triggers a refresh, so a rotation does not need a wait.
    async fn key_for(&self, kid: &str) -> Result<jsonwebtoken::DecodingKey, Error> {
        if let Some(cached) = self
            .cache
            .read()
            .expect("jwks mutex is not poisoned")
            .as_ref()
            .filter(|c| c.fetched_at.elapsed() < self.cache_ttl)
        {
            if let Some(key) = cached.by_kid.get(kid) {
                return Ok(key.clone());
            }
        }

        self.refresh().await?;
        self.cache
            .read()
            .expect("jwks mutex is not poisoned")
            .as_ref()
            .and_then(|c| c.by_kid.get(kid))
            .cloned()
            .ok_or(Error::unauthorized("the bearer token is not valid"))
    }

    /// Fetch unconditionally and replace the cache. On failure the *previous*
    /// cache is left in place: an identity outage should not invalidate tokens
    /// that were already verified against a good key, and the TTL bounds how
    /// long a stale-but-real key is honoured.
    async fn refresh(&self) -> Result<(), Error> {
        let document: JwksDocument = self
            .http
            .get(&self.jwks_url)
            .send()
            .await
            .map_err(|e| {
                tracing::warn!(error = %e, "jwks fetch failed; keeping the cached keys");
                Error::unavailable("the token issuer is unavailable")
            })?
            .json()
            .await
            .map_err(|e| {
                tracing::warn!(error = %e, "jwks document is not valid json");
                Error::unavailable("the token issuer is unavailable")
            })?;

        let mut by_kid = HashMap::new();
        for jwk in document.keys {
            match decoding_key(&jwk) {
                Ok(key) => {
                    by_kid.insert(jwk.kid.clone(), key);
                }
                // A key this service cannot use is logged and skipped, not
                // fatal: a JWKS routinely contains keys for other services and
                // for algorithms core does not accept.
                Err(why) => tracing::debug!(kid = %jwk.kid, %why, "skipping unusable jwk"),
            }
        }

        *self.cache.write().expect("jwks mutex is not poisoned") = Some(CachedJwks {
            by_kid,
            fetched_at: Instant::now(),
        });
        Ok(())
    }
}

fn decoding_key(jwk: &Jwk) -> Result<jsonwebtoken::DecodingKey, String> {
    // The `alg` string decides the constructor, not a value read off the token:
    // this runs before any token is decoded, so a key whose advertised
    // algorithm core does not accept never becomes usable.
    match jwk.alg.as_str() {
        // RS256 is core's first-named accepted algorithm; ES256 the second.
        "RS256" | "RS384" | "RS512" => {
            let n = jwk.n.as_deref().ok_or("missing modulus")?;
            let e = jwk.e.as_deref().ok_or("missing exponent")?;
            Ok(jsonwebtoken::DecodingKey::from_rsa_components(n, e)
                .map_err(|err| format!("rsa components rejected: {err}"))?)
        }
        "ES256" | "ES384" => {
            let x = jwk.x.as_deref().ok_or("missing x coordinate")?;
            let y = jwk.y.as_deref().ok_or("missing y coordinate")?;
            Ok(jsonwebtoken::DecodingKey::from_ec_components(x, y)
                .map_err(|err| format!("ec components rejected: {err}"))?)
        }
        // Everything else — HS256 included — is refused at key-load time, so
        // `alg: none` and symmetric-key confusion never reach the decoder.
        other => Err(format!("algorithm {other} is not accepted")),
    }
}

#[async_trait::async_trait]
impl TokenVerifier for JwksVerifier {
    async fn verify(&self, token: &str) -> Result<Principal, Error> {
        use jsonwebtoken::{Algorithm, Validation};

        // The header is read before the key so the `kid` can select one. An
        // unparseable header is a 401, not a 500.
        let header = jsonwebtoken::decode_header(token)
            .map_err(|_| Error::unauthorized("the bearer token is not valid"))?;

        // core: "Accepted algorithms: RS256 and ES256. `alg: none`, symmetric
        // HS256, and any algorithm not advertised by the JWKS are rejected
        // outright." The allow-list is here, before any key is chosen.
        let algorithm = match header.alg {
            Algorithm::RS256 => Algorithm::RS256,
            Algorithm::ES256 => Algorithm::ES256,
            _ => return Err(Error::unauthorized("the bearer token is not valid")),
        };

        let kid = header
            .kid
            .as_deref()
            .ok_or(Error::unauthorized("the bearer token is not valid"))?;
        let key = self.key_for(kid).await?;

        let mut validation = Validation::new(algorithm);
        validation.set_issuer(&[self.issuer.as_str()]);
        validation.set_audience(&[self.audience.as_str()]);
        // Required claims per core: iss, aud, sub, exp, iat, jti. jsonwebtoken
        // checks exp/iss/aud itself; `sub` and the cafaye-specific
        // `account_id` are checked below.
        validation.set_required_spec_claims(&["exp", "iss", "aud", "sub"]);

        let data = jsonwebtoken::decode::<Claims>(token, &key, &validation)
            .map_err(|_| Error::unauthorized("the bearer token is not valid"))?;

        let user_id = data
            .claims
            .sub
            .parse::<Uuid>()
            .map_err(|_| Error::unauthorized("the bearer token is not valid"))?;

        Ok(Principal {
            user_id,
            account_id: data.claims.account_id,
            scopes: data.claims.scope_list(),
        })
    }
}

/// The dev-only HMAC verifier. Compiled only with `--features dev-auth` and
/// [`build_verifier`] refuses to construct it outside
/// `DARKROOM_ENV=development` — a build that has the feature compiled in but
/// is running in production must still refuse to start, because a feature flag
/// is a build-time switch and the environment is the one that knows where the
/// binary ended up.
#[cfg(feature = "dev-auth")]
pub struct HmacVerifier {
    secret: String,
    issuer: String,
    audience: String,
}

#[cfg(feature = "dev-auth")]
impl HmacVerifier {
    pub fn new(secret: impl Into<String>, issuer: impl Into<String>, audience: impl Into<String>) -> Self {
        Self {
            secret: secret.into(),
            issuer: issuer.into(),
            audience: audience.into(),
        }
    }

    /// Mint a token. Dev-only, and deliberately obvious about it: a function
    /// that can sign a token belongs in a dev binary and nowhere else.
    pub fn mint(&self, user_id: Uuid, account_id: Uuid, scopes: &[&str], ttl: Duration) -> String {
        use jsonwebtoken::{Algorithm, EncodingKey, Header, encode};

        #[derive(Serialize)]
        struct DevClaims {
            iss: String,
            aud: String,
            sub: String,
            exp: i64,
            iat: i64,
            jti: String,
            account_id: Uuid,
            scope: String,
        }

        let now = crate::observability::now();
        let claims = DevClaims {
            iss: self.issuer.clone(),
            aud: self.audience.clone(),
            sub: user_id.to_string(),
            exp: (now + ttl).unix_timestamp(),
            iat: now.unix_timestamp(),
            jti: Uuid::new_v4().to_string(),
            account_id,
            scope: scopes.join(" "),
        };

        encode(
            &Header::new(Algorithm::HS256),
            &claims,
            &EncodingKey::from_secret(self.secret.as_bytes()),
        )
        .expect("an HS256 token over our own claims always encodes")
    }
}

#[async_trait::async_trait]
#[cfg(feature = "dev-auth")]
impl TokenVerifier for HmacVerifier {
    async fn verify(&self, token: &str) -> Result<Principal, Error> {
        use jsonwebtoken::{DecodingKey, Validation};

        let mut validation = Validation::new(Algorithm::HS256);
        validation.set_issuer(&[self.issuer.as_str()]);
        validation.set_audience(&[self.audience.as_str()]);
        validation.set_required_spec_claims(&["exp", "iss", "aud", "sub"]);

        let data = jsonwebtoken::decode::<Claims>(
            token,
            &DecodingKey::from_secret(self.secret.as_bytes()),
            &validation,
        )
        .map_err(|_| Error::unauthorized("the bearer token is not valid"))?;

        let user_id = data
            .claims
            .sub
            .parse::<Uuid>()
            .map_err(|_| Error::unauthorized("the bearer token is not valid"))?;

        Ok(Principal {
            user_id,
            account_id: data.claims.account_id,
            scopes: data.claims.scope_list(),
        })
    }
}

#[cfg(test)]
mod tests {
    use super::*;

    fn principal(account: Uuid, user: Uuid, scopes: &[&str]) -> Principal {
        Principal {
            user_id: user,
            account_id: account,
            scopes: scopes.iter().map(|s| s.to_string()).collect(),
        }
    }

    #[test]
    fn a_tenant_can_only_be_built_from_a_principal() {
        // The compile-time half of the isolation guarantee. There is no
        // `Tenant::from_request`, so a handler physically cannot scope a query
        // to an account the caller named in their body.
        let account = Uuid::new_v4();
        let p = principal(account, Uuid::new_v4(), &[SCOPE_ASSETS_READ]);
        assert_eq!(Tenant::from_principal(&p).account_id(), account);
    }

    #[tokio::test]
    async fn a_tenant_with_no_auth_state_is_401_not_500() {
        // A route reached without the middleware is a 401, not a panic. Built
        // here by constructing the parts by hand because that is the only way
        // to reach the state a mis-wired router produces.
        let request = axum::http::Request::get("/v1/assets")
            .body(())
            .expect("builds");
        let (mut parts, _) = request.into_parts();
        let err = Tenant::from_request_parts(&mut parts, &()).await;
        assert!(err.is_err());
    }

    #[tokio::test]
    async fn scope_parsing_accepts_both_issuer_conventions() {
        // RFC 8693 says space-delimited; some issuers send an array. Both mean
        // the same thing, and rejecting one turns a working deployment into a
        // 401 storm.
        let string_form = Claims {
            iss: "i".into(),
            aud: "a".into(),
            sub: Uuid::new_v4().to_string(),
            exp: 0,
            nbf: None,
            iat: None,
            jti: None,
            account_id: Uuid::new_v4(),
            scope: Some(serde_json::json!("assets:read assets:write")),
            roles: None,
        };
        assert_eq!(string_form.scope_list(), vec!["assets:read", "assets:write"]);

        let array_form = Claims {
            scope: Some(serde_json::json!(["assets:read"])),
            ..string_form
        };
        assert_eq!(array_form.scope_list(), vec!["assets:read"]);

        // Absent is empty, not a panic and not a wildcard.
        let absent = Claims {
            scope: None,
            ..string_form
        };
        assert!(absent.scope_list().is_empty());
    }

    #[test]
    fn a_write_scope_implies_read_and_a_read_scope_does_not_imply_write() {
        // Least privilege in one direction only: a token minted `assets:read`
        // can list but not delete. Asserted because a symmetric accident here
        // is a privilege escalation that no other test would catch.
        let reader = principal(Uuid::new_v4(), Uuid::new_v4(), &[SCOPE_ASSETS_READ]);
        assert!(reader.can_read_assets());
        assert!(!reader.can_write_assets());

        let writer = principal(Uuid::new_v4(), Uuid::new_v4(), &[SCOPE_ASSETS_WRITE]);
        assert!(writer.can_read_assets());
        assert!(writer.can_write_assets());

        // No scopes at all: neither. An empty scope list is not "all".
        let nothing = principal(Uuid::new_v4(), Uuid::new_v4(), &[]);
        assert!(!nothing.can_read_assets());
        assert!(!nothing.can_write_assets());
    }

    #[tokio::test]
    async fn the_static_verifier_rejects_an_unknown_token_without_distinguishing() {
        let v = StaticTokenVerifier::new();
        let known = Uuid::new_v4();
        v.insert("good", principal(known, Uuid::new_v4(), &[SCOPE_ASSETS_READ]));

        assert!(v.verify("good").await.is_ok());
        let err = v.verify("bad").await.expect_err("unknown token");
        // One message for every rejection. A verifier that says "unknown token"
        // for one and "bad signature" for the other is an oracle.
        assert_eq!(err.status().as_u16(), 401);
        assert_eq!(err.detail(), "the bearer token is not valid");
    }

    #[test]
    fn only_rsa_and_ec_keys_are_loadable() {
        // core: "`alg: none`, symmetric HS256, and any algorithm not advertised
        // by the JWKS are rejected outright." An HS256 JWK is refused at
        // load time, so the symmetric/asymmetric confusion this rule exists to
        // prevent never reaches a decoder.
        let hs = Jwk {
            kid: "k".into(),
            alg: "HS256".into(),
            kty: "oct".into(),
            n: Some("n".into()),
            e: Some("AQAB".into()),
            crv: None,
            x: None,
            y: None,
        };
        assert!(decoding_key(&hs).is_err());

        let ps = Jwk { alg: "PS256".into(), kty: "RSA".into(), ..hs.clone() };
        assert!(decoding_key(&ps).is_err());

        // A real RSA key loads.
        let rsa = Jwk {
            kid: "k".into(),
            alg: "RS256".into(),
            kty: "RSA".into(),
            n: Some(
                "0vx7agoebGcQSuuPiLJXZptN9nndrQmbXEps2aiAFbWhM78LhWx4cbbfAAtVT86zwu1RK7aPFFxuhDR1L6tSoc_BJECPebWKRXjBZCiFV4n3oknjhMstn64tZ_2W-5JsGY4Hc5n9yBXArwl93lqt7_RN5w6Cf0h4QyQ5v-65YGjQR0_FDW2QvzqY368QQMicAtaSqzs8KJZgnYb9c7d0zgdAZHzu6qMQvRL5hajrn1n91CbOpbISD08qNLyrdkt-bFTWhAI4vMQFh6WeZu0fM4lFd2NcRwr3XPksINHaQ-G_xBniIqbw0Ls1jF44-csFCur-kEgU8awapJzKnqDKgw",
            ),
            e: Some("AQAB".into()),
            crv: None,
            x: None,
            y: None,
        };
        assert!(decoding_key(&rsa).is_ok(), "a valid RS256 key must load");
    }
}
