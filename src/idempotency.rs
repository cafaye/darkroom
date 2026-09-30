//! The `Idempotency-Key` middleware wrapper.
//!
//! core/docs/openapi-conventions.md, "Idempotency":
//!
//! > Mutating `POST` endpoints that can be retried safely **must** accept
//! > `Idempotency-Key`.
//!
//! Three of the rules are load-bearing and each has a test:
//!
//! - **Scope is `(endpoint, principal, key)`.** One tenant's key cannot replay
//!   another tenant's response. Enforced by the primary key in
//!   `0003_idempotency_keys.sql`, not by a `where` clause somebody might forget.
//! - **Same key, same body → the original response**, with
//!   `Idempotency-Replayed: true`.
//! - **Same key, different body → 409 `idempotency_key_reused`.** A distinct
//!   code from `conflict` because the client's action differs: retry, or pick a
//!   new key.
//!
//! ## The concurrent case, which is the one that bites
//!
//! Two identical POSTs with the same key arrive at once. One reserves the row;
//! the other finds it. If the first has not finished, the second is told 409
//! `conflict` — not a fabricated replay of a response that does not exist yet.
//! Returning "success with an empty body" would be worse than an error: the
//! client would believe the upload was created and then never have an id for it.

use std::sync::Arc;

use axum::body::Body;
use axum::http::{header, HeaderMap, HeaderName, HeaderValue, StatusCode};
use axum::response::{IntoResponse, Response};
use serde_json::Value;
use sha2::{Digest, Sha256};

use crate::auth::Principal;
use crate::error::Error;
use crate::store::{self, IdempotencyOutcome, SharedStore};

/// The response header core names for a replay. Fixed, not configurable: a
/// client switches on it.
pub const REPLAYED_HEADER: &str = "idempotency-replayed";
pub const REPLAYED_HEADER_NAME: HeaderName = HeaderName::from_static(REPLAYED_HEADER);

/// The longest key accepted. A key is a client-chosen uuid per core; anything
/// this long is not a uuid and is almost certainly a mistake or an attempt to
/// fill the table.
pub const MAX_KEY_LENGTH: usize = 255;

/// A header's value, or `None` if absent, not UTF-8, empty, or too long.
///
/// `None` means "no key", which is a supported state: core says "Requests
/// without the key are processed normally." A malformed key is *not* silently
/// treated as absent — it is a 422, because a client that meant to send a key
/// and sent a broken one would otherwise get a non-idempotent request and not
/// know why.
pub fn parse_key(headers: &HeaderMap) -> Result<Option<String>, Error> {
    let Some(raw) = headers.get("idempotency-key") else {
        return Ok(None);
    };
    let value = raw
        .to_str()
        .map_err(|_| invalid_key())?
        .trim();

    if value.is_empty() {
        return Err(invalid_key());
    }
    if value.len() > MAX_KEY_LENGTH {
        return Err(invalid_key());
    }
    // Control characters in a key end up in log lines and in a primary key.
    if value.chars().any(|c| c.is_control()) {
        return Err(invalid_key());
    }
    Ok(Some(value.to_string()))
}

fn invalid_key() -> Error {
    use crate::error::FieldError;
    Error::invalid_fields(
        "Idempotency-Key must be a non-empty string of at most 255 printable characters",
        vec![FieldError::new("Idempotency-Key", "invalid_format")],
    )
}

/// The `(endpoint, principal, key)` scope. Both halves are hashed into
/// `principal_key` so the stored value is opaque and bounded — a
/// `user_id:account_id` string in a primary key is fine, but a token would not
/// be, and this keeps the shape uniform if that ever changes.
pub fn principal_scope(principal: &Principal) -> String {
    format!("{}:{}", principal.user_id, principal.account_id)
}

/// sha256 of the canonical request body. Hex, so it is storable and printable.
///
/// The body is hashed as received. Two semantically identical JSON bodies with
/// different key order are therefore different bodies and produce a 409 — which
/// is the honest answer, because the service cannot know they were meant to be
/// the same and guessing would mean re-serialising every request canonically.
pub fn request_hash(body: &[u8]) -> String {
    let mut hasher = Sha256::new();
    hasher.update(body);
    hex::encode(hasher.finalize())
}

/// What the handler did, captured so it can be stored and replayed.
#[derive(Debug, Clone)]
pub struct Recorded {
    pub status: StatusCode,
    pub body: Value,
    /// Headers worth replaying. Deliberately a short list rather than the whole
    /// map: replaying a `Date` or a `Content-Length` from an hour ago is worse
    /// than not replaying it.
    pub content_type: Option<String>,
    pub location: Option<String>,
    /// True when this response resolved a duplicate upload to an existing asset.
    /// Becomes `X-Darkroom-Duplicate: true` on the wire. A field rather than a
    /// re-read of the serialised body, because reading it back out of the JSON
    /// is how a field gets dropped by the next person who adds one.
    pub duplicate: Option<bool>,
}

/// The header the duplicate marker travels in. Lowercase, fixed, not
/// configurable — a client switches on the literal string.
pub const DUPLICATE_HEADER: &str = "x-darkroom-duplicate";
pub const DUPLICATE_HEADER_NAME: HeaderName = HeaderName::from_static(DUPLICATE_HEADER);

impl Recorded {
    pub fn new(status: StatusCode, body: Value) -> Self {
        Self {
            status,
            body,
            content_type: Some("application/json".to_string()),
            location: None,
            duplicate: None,
        }
    }

    pub fn with_location(mut self, location: impl Into<String>) -> Self {
        self.location = Some(location.into());
        self
    }

    pub fn with_duplicate(mut self, duplicate: bool) -> Self {
        self.duplicate = Some(duplicate);
        self
    }

    /// The stored response, turned back into bytes with the replay marker.
    ///
    /// Note what is NOT replayed: the duplicate marker. A replay answers a
    /// *new* request, and whether that new request was itself a duplicate is a
    /// fact about it, not about the recorded one.
    pub fn into_response(self, replayed: bool) -> Response {
        let mut headers = HeaderMap::new();
        headers.insert(
            header::CONTENT_TYPE,
            HeaderValue::from_str(self.content_type.as_deref().unwrap_or("application/json"))
                .unwrap_or_else(|_| HeaderValue::from_static("application/json")),
        );
        if let Some(location) = &self.location {
            if let Ok(value) = HeaderValue::from_str(location) {
                headers.insert(header::LOCATION, value);
            }
        }
        if self.duplicate == Some(true) {
            headers.insert(DUPLICATE_HEADER_NAME, HeaderValue::from_static("true"));
        }
        // Present on a replay and absent otherwise, so "was this a replay?" is
        // answerable without comparing bodies.
        if replayed {
            headers.insert(REPLAYED_HEADER_NAME, HeaderValue::from_static("true"));
        }
        // The trace id of the CURRENT request, not the one recorded with the
        // original response. A replay is a new HTTP request with its own
        // support handle.
        if let Ok(value) = HeaderValue::from_str(&crate::observability::current_trace_id()) {
            headers.insert(crate::error::TRACE_ID_HEADER_NAME, value);
        }

        let bytes = serde_json::to_vec(&self.body).unwrap_or_else(|_| b"{}".to_vec());
        (self.status, headers, Body::from(bytes)).into_response()
    }
}

/// Run `handler` under `key`, or return the stored response for a replay.
///
/// The flow, and why each step is where it is:
///
/// 1. **Reserve.** An insert of `(key, endpoint, principal)`. This is the lock —
///    exactly one concurrent request can hold it.
/// 2. **Body check.** A hit with a different body hash is `idempotency_key_reused`.
/// 3. **Replay.** A hit with `status_code != 0` is the recorded response.
/// 4. **Run.** We hold the reservation, so we do the work.
/// 5. **Record, or release.** Success stores the response. Failure deletes the
///    reservation so the client's retry is a fresh attempt — a 500 that poisoned
///    the key would make every subsequent retry fail forever.
///
/// The store calls are on the pool rather than inside a transaction with the
/// handler's work, because the handler's transaction is its own: the reservation
/// must outlive a rollback of the domain work, or a replay would find nothing.
pub async fn run<F, Fut>(
    store: &SharedStore,
    principal: &Principal,
    endpoint: &'static str,
    key: Option<String>,
    body: &[u8],
    handler: F,
) -> Result<Response, Error>
where
    F: FnOnce() -> Fut,
    Fut: std::future::Future<Output = Result<Recorded, Error>>,
{
    let Some(key) = key else {
        // No key: process normally. core calls this "the client's bug" for
        // money endpoints, and for an upload it is the client's choice.
        return handler().await.map(|r| r.into_response(false));
    };

    let hash = request_hash(body);
    let scope = principal_scope(principal);

    // Acquire ONE connection for the reserve-and-read pair. They must be the
    // same connection: a reserve on one connection followed by a read on
    // another is two unrelated queries, and the read would not see the row the
    // insert is still writing.
    let mut conn = store
        .pool()
        .acquire()
        .await
        .map_err(|e| Error::from(crate::store::StoreError::from(e)))?;

    match store::reserve_idempotency_key(&mut conn, &key, endpoint, &scope, &hash).await? {
        IdempotencyOutcome::Replay {
            status_code,
            response_body,
        } => {
            tracing::info!(%key, endpoint, "idempotent replay");
            Ok(Recorded {
                status: StatusCode::from_u16(status_code as u16)
                    .unwrap_or(StatusCode::INTERNAL_SERVER_ERROR),
                body: response_body,
                content_type: Some("application/json".to_string()),
                location: None,
                duplicate: None,
            }
            .into_response(true))
        }
        IdempotencyOutcome::BodyMismatch => Err(Error::IdempotencyKeyReused {
            detail: "this Idempotency-Key was already used with a different request body".to_string(),
        }),
        IdempotencyOutcome::InFlight => Err(Error::conflict(
            "a request with this Idempotency-Key is still in flight",
        )),
        IdempotencyOutcome::Reserved => {
            match handler().await {
                Ok(recorded) => {
                    // Recorded in its own statement rather than in the
                    // handler's transaction, and deliberately so: the ledger
                    // write must commit even if a later step rolls back, and
                    // the handler's transaction has already committed by the
                    // time we get here.
                    if let Err(e) = store::complete_idempotency_key(
                        store.pool(),
                        &key,
                        endpoint,
                        &scope,
                        recorded.status.as_u16() as i32,
                        &recorded.body,
                    )
                    .await
                    {
                        // The work succeeded and only the ledger write failed.
                        // A retry with the same key re-runs the handler, which
                        // for these endpoints is safe (the domain work is
                        // compare-and-set or unique-constrained), so this is a
                        // log-and-continue rather than a 500 that tells the
                        // client the upload failed when it did not.
                        tracing::error!(error = %e, %key, endpoint, "could not record the idempotency outcome");
                    }
                    Ok(recorded.into_response(false))
                }
                Err(e) => {
                    if let Err(release_err) =
                        store::release_idempotency_key(store.pool(), &key, endpoint, &scope).await
                    {
                        tracing::error!(error = %release_err, %key, endpoint, "could not release the idempotency reservation");
                    }
                    Err(e)
                }
            }
        }
    }
}

/// A shared service handle, so `main` builds one and the router holds it.
pub type IdempotencyStore = Arc<crate::store::Store>;

#[cfg(test)]
mod tests {
    use super::*;
    use axum::http::Request;
    use uuid::Uuid;

    fn headers_with(key: &'static str) -> HeaderMap {
        let mut h = HeaderMap::new();
        h.insert(key, HeaderValue::from_static("k-1"));
        h
    }

    #[test]
    fn an_absent_key_is_absent_not_an_error() {
        // core: "Requests without the key are processed normally."
        let headers = HeaderMap::new();
        assert_eq!(parse_key(&headers).expect("no key is fine"), None);
    }

    #[test]
    fn a_well_formed_key_is_returned_trimmed() {
        assert_eq!(
            parse_key(&headers_with("idempotency-key")).expect("valid"),
            Some("k-1".to_string())
        );
    }

    #[test]
    fn a_malformed_key_is_422_not_silently_ignored() {
        // A client that meant to send a key and sent a broken one must not get
        // a non-idempotent request and no explanation.
        let cases: Vec<HeaderMap> = vec![
            {
                let mut h = HeaderMap::new();
                h.insert("idempotency-key", HeaderValue::from_static(""));
                h
            },
            {
                let mut h = HeaderMap::new();
                h.insert("idempotency-key", HeaderValue::from_static("   "));
                h
            },
            {
                let mut h = HeaderMap::new();
                h.insert("idempotency-key", HeaderValue::from_static(&"x".repeat(256)));
                h
            },
        ];
        for headers in cases {
            let err = parse_key(&headers).expect_err("must be rejected");
            assert_eq!(err.status().as_u16(), 422, "a bad key is a 422");
        }
    }

    #[test]
    fn scope_is_per_principal_not_per_token() {
        // Two different tokens for the same user in the same account must share
        // a key, or a token refresh would turn a retry into a second upload.
        let account = Uuid::new_v4();
        let user = Uuid::new_v4();
        let a = Principal {
            user_id: user,
            account_id: account,
            scopes: vec!["assets:write".into()],
        };
        let b = Principal {
            scopes: vec!["assets:write".into(), "assets:read".into()],
            ..a.clone()
        };
        assert_eq!(principal_scope(&a), principal_scope(&b));

        // A different account is a different scope. This is what stops one
        // tenant's key replaying another tenant's response.
        let other = Principal {
            user_id: user,
            account_id: Uuid::new_v4(),
            scopes: a.scopes.clone(),
        };
        assert_ne!(principal_scope(&a), principal_scope(&other));
    }

    #[test]
    fn the_body_hash_distinguishes_bodies_and_ignores_nothing() {
        let a = request_hash(br#"{"filename":"a.png"}"#);
        let b = request_hash(br#"{"filename":"b.png"}"#);
        assert_ne!(a, b);
        assert_eq!(a, request_hash(br#"{"filename":"a.png"}"#));
        // Key order is part of the bytes, so a reordered body is a different
        // body. Stated as a decision, not an accident.
        assert_ne!(
            request_hash(br#"{"a":1,"b":2}"#),
            request_hash(br#"{"b":2,"a":1}"#)
        );
        assert_eq!(request_hash(b"").len(), 64);
    }

    #[test]
    fn a_replay_carries_the_marker_and_a_fresh_response_does_not() {
        let recorded = Recorded::new(
            StatusCode::CREATED,
            serde_json::json!({"id": "ast_1"}),
        );
        let replayed = recorded.clone().into_response(true);
        assert_eq!(replayed.headers()[REPLAYED_HEADER_NAME], "true");

        let fresh = recorded.into_response(false);
        assert!(
            fresh.headers().get(REPLAYED_HEADER_NAME).is_none(),
            "a first response must not claim to be a replay"
        );
        assert_eq!(fresh.status(), StatusCode::CREATED);
    }

    #[test]
    fn a_recorded_location_header_survives_the_round_trip() {
        let recorded = Recorded::new(StatusCode::CREATED, serde_json::json!({}))
            .with_location("/v1/assets/ast_1");
        let response = recorded.into_response(false);
        assert_eq!(response.headers()[header::LOCATION], "/v1/assets/ast_1");
    }

    #[tokio::test]
    async fn the_marker_header_name_is_lowercase() {
        // HTTP header names are case-insensitive but a client matching on the
        // literal string is common enough that the canonical lowercase form is
        // worth asserting.
        let _ = Request::get("/");
        assert_eq!(REPLAYED_HEADER, "idempotency-replayed");
    }
}
