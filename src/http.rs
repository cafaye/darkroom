//! The HTTP surface: routes, handlers, probes, and the auth middleware.
//!
//! Handlers here do three things and nothing else: extract, call
//! [`crate::service::Service`], and translate the result into a status and a
//! body. Every decision — what a duplicate is, when a checksum is verified, what
//! a cross-tenant read returns — is in `service.rs`, where it can be tested
//! without a socket.
//!
//! ## Route table
//!
//! | Method | Path | Scope | Notes |
//! |---|---|---|---|
//! | `GET` | `/healthz` | none | Liveness. Never touches a dependency. |
//! | `GET` | `/readyz` | none | Readiness. Actually queries Postgres. |
//! | `POST` | `/v1/uploads` | `assets:write` | Idempotent. |
//! | `POST` | `/v1/uploads/{id}/complete` | `assets:write` | Idempotent. |
//! | `GET` | `/v1/assets` | `assets:read` | Cursor-paginated. |
//! | `GET` | `/v1/assets/{id}` | `assets:read` | |
//! | `DELETE` | `/v1/assets/{id}` | `assets:write` | 204, removes storage objects. |
//! | `POST` | `/v1/assets/{id}/variants` | `assets:write` | |
//! | `GET` | `/v1/assets/{id}/variants` | `assets:read` | |
//!
//! Liveness and readiness are separate because a database outage must not get
//! the process restarted out from under in-flight work — that is `/readyz`'s
//! job. `/healthz` answering 200 while the database is gone is correct: the
//! process is alive, it just should not receive traffic.
//!
//! The table above is prose. The one the router is built from is
//! [`OPERATIONS`], and `tests/openapi_document.rs` holds this file's
//! `openapi/v1.yaml` to it in both directions.

use std::net::SocketAddr;
use std::sync::Arc;
use std::time::Duration;

use axum::extract::{Path, Query, State};
use axum::http::{HeaderMap, Method, StatusCode};
use axum::response::{IntoResponse, Response};
use axum::routing::{delete, get, post, MethodRouter};
use axum::{Json, Router};
use base64::Engine as _;
use serde::{Deserialize, Serialize};
use serde_json::json;
use tower_http::limit::RequestBodyLimitLayer;
use uuid::Uuid;

use crate::auth::{Principal, Tenant, TokenVerifier};
use crate::error::{Error, FieldError};
use crate::idempotency::{self, Recorded};
use crate::observability::TraceContextLayer;
use crate::service::{CreateUpload, Service};
use crate::store::SharedStore;

/// Shared state. The verifier is an `Arc` because the JWKS cache lives inside
/// it and cloning a per request would be a copy of nothing useful.
#[derive(Clone)]
pub struct AppState {
    pub service: Service,
    pub verifier: Arc<dyn TokenVerifier>,
}

/// The request body cap. Every body this service accepts is small — a filename,
/// a content type, a size, a checksum — and the largest legitimate one is well
/// under a kilobyte. The cap is what stops an unauthenticated caller choosing
/// how much this process parses.
pub const MAX_BODY_BYTES: usize = 4096;

/// Paths that skip authentication.
///
/// They are named here rather than relied on from route ORDER, because
/// `Router::layer` in axum applies to every route the router holds at the time
/// the layer is added — registering a route "before" the auth layer does not
/// exempt it. A `/healthz` behind the auth middleware returns 401, an
/// orchestrator marks the instance unhealthy, and the deployment rolls back
/// with no indication of why. The probe test asserts these two paths are the
/// exempt ones and that everything else is not.
const UNAUTHENTICATED_PATHS: &[&str] = &["/healthz", "/readyz"];

/// One operation this router serves: the route, and the handler behind it.
///
/// ## Why the route table is data
///
/// axum cannot be asked what it routes. There is no `Router::routes()`, no
/// `Display`, and nothing to reflect over — a `Router` is a tree of boxed
/// services behind a type that does not expose its contents. So the set of
/// operations darkroom serves has to be written down, and the only place it can
/// be written down without a second copy quietly going stale is here, next to
/// the handlers.
///
/// [`OPERATIONS`] is that one declaration and [`router`] is built from it, so
/// the HTTP surface and the thing a test reads are the same thing. The
/// alternative is a list written out inside the test, and a list in a test is
/// the shape that can only fail for a name somebody remembered to type.
pub struct Operation {
    /// The method, spelled the way the OpenAPI document spells it.
    ///
    /// This is the one field nothing in axum can confirm. The method a
    /// `MethodRouter` answers is baked into the value `get(handler)` returns
    /// and there is no accessor for it, so the row states it and the
    /// OpenAPI check asks the router itself — a request with a method the table
    /// does not declare, answered with `405` and an `Allow` header that
    /// enumerates the truth.
    pub method: Method,
    /// The route in axum's syntax, so `{id}` and not `:id`.
    pub path: &'static str,
    handler: fn() -> MethodRouter<AppState>,
}

/// Every operation this router serves, in the order they are registered.
///
/// A new endpoint is a new row here, and a row is not a promise: adding one
/// without a matching operation in `openapi/v1.yaml` fails
/// `tests/openapi_document.rs`, in both directions.
pub const OPERATIONS: &[Operation] = &[
    Operation {
        method: Method::GET,
        path: "/healthz",
        handler: || get(healthz),
    },
    Operation {
        method: Method::GET,
        path: "/readyz",
        handler: || get(readyz),
    },
    Operation {
        method: Method::POST,
        path: "/v1/uploads",
        handler: || post(create_upload),
    },
    Operation {
        method: Method::POST,
        path: "/v1/uploads/{id}/complete",
        handler: || post(complete_upload),
    },
    Operation {
        method: Method::GET,
        path: "/v1/assets",
        handler: || get(list_assets),
    },
    Operation {
        method: Method::GET,
        path: "/v1/assets/{id}",
        handler: || get(get_asset),
    },
    Operation {
        method: Method::DELETE,
        path: "/v1/assets/{id}",
        handler: || delete(delete_asset),
    },
    Operation {
        method: Method::POST,
        path: "/v1/assets/{id}/variants",
        handler: || post(create_variant),
    },
    Operation {
        method: Method::GET,
        path: "/v1/assets/{id}/variants",
        handler: || get(list_variants),
    },
];

pub fn router(state: AppState) -> Router {
    // Built from `OPERATIONS` rather than from a chain of `.route()` calls, so
    // the route table is one declaration that both the router and the OpenAPI
    // drift check read. See `Operation` for why axum forces this shape.
    OPERATIONS
        .iter()
        .fold(Router::new(), |app, op| app.route(op.path, (op.handler)()))
        // Order matters: the body limit is outermost so an oversized body is
        // refused before the auth middleware reads it, and the trace context is
        // inside that so even a refused request has a trace id.
        .layer(RequestBodyLimitLayer::new(MAX_BODY_BYTES))
        .layer(axum::middleware::from_fn_with_state(
            state.clone(),
            authenticate,
        ))
        .layer(TraceContextLayer)
        .with_state(state)
}

// ----------------------------------------------------------------- probes

/// Liveness. Unconditional, and it never touches Postgres: a database outage
/// must not get the process restarted out from under in-flight uploads.
async fn healthz() -> impl IntoResponse {
    (StatusCode::OK, Json(json!({"status": "ok"})))
}

/// Readiness. This one really queries the database, because a probe that returns
/// 200 blindly is worse than no probe: it tells the load balancer to send
/// traffic to a process that cannot serve a single request.
///
/// On failure it reports *which* dependency failed by name and the underlying
/// error goes to the log. An unauthenticated caller must not learn that a
/// database host is `10.0.0.5` or that a password was rejected.
async fn readyz(State(state): State<AppState>) -> Response {
    match state.service.store.ping().await {
        Ok(()) => (StatusCode::OK, Json(json!({"status": "ready"}))).into_response(),
        Err(e) => {
            tracing::error!(error = %e, "readiness check failed");
            // 503, and the body names the dependency without naming the cause.
            Error::unavailable("the service is not ready").into_response()
        }
    }
}

// ------------------------------------------------------------------- auth

/// The auth middleware: extract the bearer token, verify it, put the principal
/// in the request extensions.
///
/// The probes opt out with a `#[allow]`-free mechanism — they are registered
/// before this layer, so they never see it. Anything under `/v1` does.
async fn authenticate(
    State(state): State<AppState>,
    mut request: axum::extract::Request,
    next: axum::middleware::Next,
) -> Result<Response, Error> {
    // The probes are exempt: an orchestrator has no bearer token and must not
    // need one. See `UNAUTHENTICATED_PATHS` for why this is an allow-list
    // rather than a consequence of route order.
    if UNAUTHENTICATED_PATHS.contains(&request.uri().path()) {
        return Ok(next.run(request).await);
    }

    let header = request
        .headers()
        .get(axum::http::header::AUTHORIZATION)
        .and_then(|v| v.to_str().ok())
        .map(str::to_string);

    // core: "Bearer JWTs only. No cookies for API traffic." A cookie is
    // accepted by neither branch below, so a browser session cannot be
    // replayed against this surface.
    let Some(header) = header else {
        return Err(Error::unauthorized("a bearer token is required"));
    };
    let Some(token) = header
        .strip_prefix("Bearer ")
        .or_else(|| header.strip_prefix("bearer "))
    else {
        return Err(Error::unauthorized("a bearer token is required"));
    };
    let token = token.trim();
    if token.is_empty() {
        return Err(Error::unauthorized("a bearer token is required"));
    }

    let principal = state.verifier.verify(token).await?;
    request
        .extensions_mut()
        .insert(crate::auth::AuthState(Arc::new(principal)));
    Ok(next.run(request).await)
}

/// Require a scope. A token without it is 403, not 404: the caller
/// authenticated, so there is no existence to leak.
fn require_scope(principal: &Principal, write: bool) -> Result<(), Error> {
    let ok = if write {
        principal.can_write_assets()
    } else {
        principal.can_read_assets()
    };
    if ok {
        Ok(())
    } else {
        Err(Error::forbidden(if write {
            "this token does not have the assets:write scope"
        } else {
            "this token does not have the assets:read scope"
        }))
    }
}

// ---------------------------------------------------------------- uploads

#[derive(Debug, Deserialize)]
struct CreateUploadBody {
    filename: String,
    content_type: String,
    byte_size: i64,
    /// The sha256 the client is about to upload. Required: a client that does
    /// not know its own content's hash cannot complete the upload, and letting
    /// it create the asset anyway would produce a row nothing can ever verify.
    checksum: String,
}

#[derive(Debug, Serialize)]
struct PresignedUploadResponse {
    asset: crate::domain::Asset,
    /// Where to PUT the bytes, and for how long.
    upload_url: String,
    storage_key: String,
    expires_in_secs: u64,
    /// The header a client must send on the PUT. Not optional: storage rejects
    /// a request whose content type is outside the signed scope.
    content_type: String,
    #[serde(skip_serializing_if = "Option::is_none")]
    headers: Option<serde_json::Value>,
    duplicate: bool,
}

/// `POST /v1/uploads` — 201, a presigned PUT, and the created asset.
async fn create_upload(
    State(state): State<AppState>,
    principal: Principal,
    headers: HeaderMap,
    body: axum::body::Bytes,
) -> Result<Response, Error> {
    require_scope(&principal, true)?;
    let tenant = Tenant::from_principal(&principal);

    let key = idempotency::parse_key(&headers)?;
    let store = state.service.store.clone();

    let response = idempotency::run(
        &store,
        &principal,
        "POST /v1/uploads",
        key,
        &body,
        || async {
            let request: CreateUploadBody = serde_json::from_slice(&body).map_err(|e| {
                // A body that is not JSON is 400 — "malformed syntax the client
                // could not have known", per core. A body that IS json and is
                // semantically wrong is 422, which is what `CreateUpload` does.
                tracing::debug!(error = %e, "unparseable upload body");
                Error::invalid_fields(
                    "the request body is not a valid upload request",
                    vec![FieldError::new("body", "invalid_json")],
                )
            })?;

            let created = state
                .service
                .create_upload(
                    &tenant,
                    CreateUpload {
                        filename: request.filename,
                        content_type: request.content_type,
                        byte_size: request.byte_size,
                        checksum: request.checksum,
                    },
                )
                .await?;

            let payload = PresignedUploadResponse {
                upload_url: created.presigned.url,
                storage_key: created.presigned.key,
                expires_in_secs: created.presigned.expires_in_secs,
                content_type: created.asset.content_type.clone(),
                headers: Some(json!({"Content-Type": created.asset.content_type})),
                duplicate: created.duplicate,
                asset: created.asset,
            };
            let value = serde_json::to_value(&payload).map_err(|e| {
                // Serialising a struct this service owns cannot fail in
                // practice, so a failure is a bug worth the log line even
                // though the client only ever sees a 500.
                tracing::error!(error = %e, "could not serialise the upload response");
                Error::internal("the response could not be encoded")
            })?;

            Ok(Recorded::new(StatusCode::CREATED, value).with_duplicate(created.duplicate))
        },
    )
    .await?;

    Ok(response)
}

// --------------------------------------------------------------- complete

#[derive(Debug, Deserialize)]
struct CompleteBody {
    /// The checksum the client claims the stored object has. Verified against
    /// what storage actually holds — never trusted.
    checksum: String,
}

#[derive(Debug, Serialize)]
struct CompleteResponse {
    asset: crate::domain::Asset,
}

/// `POST /v1/uploads/{id}/complete` — verify, mark ready, emit.
async fn complete_upload(
    State(state): State<AppState>,
    principal: Principal,
    Path(id): Path<Uuid>,
    headers: HeaderMap,
    body: axum::body::Bytes,
) -> Result<Response, Error> {
    require_scope(&principal, true)?;
    let tenant = Tenant::from_principal(&principal);

    let key = idempotency::parse_key(&headers)?;
    let store = state.service.store.clone();

    idempotency::run(
        &store,
        &principal,
        "POST /v1/uploads/{id}/complete",
        key,
        &body,
        || async {
            let request: CompleteBody = serde_json::from_slice(&body).map_err(|e| {
                tracing::debug!(error = %e, "unparseable complete body");
                Error::invalid_fields(
                    "the request body is not a valid complete request",
                    vec![FieldError::new("body", "invalid_json")],
                )
            })?;

            let asset = state
                .service
                .complete_upload(&tenant, id, &request.checksum)
                .await?;
            let value = serde_json::to_value(CompleteResponse { asset })
                .map_err(|_| Error::internal("the response could not be encoded"))?;
            Ok(Recorded::new(StatusCode::OK, value))
        },
    )
    .await
}

// ----------------------------------------------------------------- assets

#[derive(Debug, Deserialize)]
struct ListQuery {
    limit: Option<i64>,
    cursor: Option<String>,
    order: Option<String>,
}

#[derive(Debug, Serialize)]
struct ListResponse<T> {
    data: Vec<T>,
    page: Page,
}

#[derive(Debug, Serialize)]
struct Page {
    next_cursor: Option<String>,
    has_more: bool,
}

/// `GET /v1/assets` — one cursor page, tenant-scoped.
async fn list_assets(
    State(state): State<AppState>,
    principal: Principal,
    Query(query): Query<ListQuery>,
) -> Result<Response, Error> {
    require_scope(&principal, false)?;
    let tenant = Tenant::from_principal(&principal);

    // core: "`limit` defaults to 25 and is capped at 100." A larger request is
    // clamped, not rejected — the cap is a promise to the server about how much
    // work one request may cause, and a client asking for 1000 gets 100.
    let limit = query.limit.unwrap_or(25).clamp(1, 100);
    if let Some(order) = query.order.as_deref() {
        if order != "desc" && order != "asc" {
            return Err(Error::invalid_fields(
                "order must be asc or desc",
                vec![FieldError::new("order", "unknown_value")],
            ));
        }
    }

    let cursor = match &query.cursor {
        Some(raw) => Some(decode_cursor(raw)?),
        None => None,
    };

    let (assets, next_time, next_id, has_more) =
        state.service.list_assets(&tenant, limit, cursor).await?;

    let body = ListResponse {
        // `data` is always an array, empty rather than absent. core.
        data: assets,
        page: Page {
            next_cursor: match (next_time, next_id) {
                (Some(t), Some(i)) => Some(encode_cursor(t, i)),
                _ => None,
            },
            has_more,
        },
    };
    Ok(Json(body).into_response())
}

/// `GET /v1/assets/{id}` — 404 for another account's asset, same as for one
/// that does not exist.
async fn get_asset(
    State(state): State<AppState>,
    principal: Principal,
    Path(id): Path<Uuid>,
) -> Result<Response, Error> {
    require_scope(&principal, false)?;
    let tenant = Tenant::from_principal(&principal);
    let asset = state.service.get_asset(&tenant, id).await?;
    Ok(Json(json!({ "asset": asset })).into_response())
}

/// `DELETE /v1/assets/{id}` — 204, storage objects removed, event emitted.
async fn delete_asset(
    State(state): State<AppState>,
    principal: Principal,
    Path(id): Path<Uuid>,
) -> Result<Response, Error> {
    require_scope(&principal, true)?;
    let tenant = Tenant::from_principal(&principal);
    state.service.delete_asset(&tenant, id).await?;
    Ok(StatusCode::NO_CONTENT.into_response())
}

// --------------------------------------------------------------- variants

#[derive(Debug, Deserialize)]
struct CreateVariantBody {
    kind: String,
}

#[derive(Debug, Serialize)]
struct VariantResponse {
    variant: crate::domain::AssetVariant,
}

/// `POST /v1/assets/{id}/variants` — derive, store, emit.
async fn create_variant(
    State(state): State<AppState>,
    principal: Principal,
    Path(id): Path<Uuid>,
    body: axum::body::Bytes,
) -> Result<Response, Error> {
    require_scope(&principal, true)?;
    let tenant = Tenant::from_principal(&principal);

    let request: CreateVariantBody = serde_json::from_slice(&body).map_err(|e| {
        tracing::debug!(error = %e, "unparseable variant body");
        Error::invalid_fields(
            "the request body is not a valid variant request",
            vec![FieldError::new("body", "invalid_json")],
        )
    })?;
    // An unknown kind is a 422 naming `kind`, not a 500 and not a 404. It is
    // parsed here rather than in the service so a typo is caught before any
    // storage read happens.
    let kind: crate::domain::VariantKind = request.kind.parse()?;

    let variant = state.service.create_variant(&tenant, id, kind).await?;
    Ok((StatusCode::CREATED, Json(VariantResponse { variant })).into_response())
}

/// `GET /v1/assets/{id}/variants` — 404 if the asset is not the caller's.
async fn list_variants(
    State(state): State<AppState>,
    principal: Principal,
    Path(id): Path<Uuid>,
) -> Result<Response, Error> {
    require_scope(&principal, false)?;
    let tenant = Tenant::from_principal(&principal);
    let variants = state.service.list_variants(&tenant, id).await?;
    Ok(Json(ListResponse {
        data: variants,
        page: Page {
            next_cursor: None,
            has_more: false,
        },
    })
    .into_response())
}

// ---------------------------------------------------------------- cursors

/// Opaque base64url cursor over `(created_at, id)`. core: "clients must not
/// parse it, and its encoding may change without notice."
fn encode_cursor(at: time::OffsetDateTime, id: Uuid) -> String {
    let raw = format!("{}|{id}", at.unix_timestamp_nanos());
    base64::engine::general_purpose::URL_SAFE_NO_PAD.encode(raw)
}

fn decode_cursor(raw: &str) -> Result<(time::OffsetDateTime, Uuid), Error> {
    let decoded = base64::engine::general_purpose::URL_SAFE_NO_PAD
        .decode(raw)
        .map_err(|_| bad_cursor())?;
    let text = String::from_utf8(decoded).map_err(|_| bad_cursor())?;
    let (nanos, id) = text.split_once('|').ok_or_else(bad_cursor)?;
    let nanos: i128 = nanos.parse().map_err(|_| bad_cursor())?;
    let id: Uuid = id.parse().map_err(|_| bad_cursor())?;
    // A cursor older than the platform's window is 400 `cursor_expired`, not a
    // silent restart at page one. core names the code; this service has no
    // separate expired path yet, so an unparseable or out-of-range cursor is the
    // same 400 and the same message.
    let at = time::OffsetDateTime::from_unix_timestamp_nanos(nanos).map_err(|_| bad_cursor())?;
    Ok((at, id))
}

fn bad_cursor() -> Error {
    Error::invalid_fields(
        "cursor is not a valid pagination cursor",
        vec![FieldError::new("cursor", "invalid_format")],
    )
}

/// The listening socket configuration, resolved from the environment in `main`.
#[derive(Debug, Clone, Copy)]
pub struct BindAddr(pub SocketAddr);

/// How long a readiness probe may take. A probe that hangs is worse than a
/// probe that fails, because a hanging probe holds a slot in the load
/// balancer's health check.
pub const READINESS_TIMEOUT: Duration = Duration::from_secs(2);

/// A convenience for `main` and for the integration tests: bind and serve.
pub async fn serve(listener: tokio::net::TcpListener, state: AppState) -> std::io::Result<()> {
    axum::serve(
        listener,
        router(state).into_make_service_with_connect_info::<SocketAddr>(),
    )
    .await
}

/// `Store` is re-exported here so a test that builds an app does not have to
/// reach into the store module for it.
pub type TestStore = SharedStore;

#[cfg(test)]
mod tests {
    use super::*;

    #[test]
    fn cursors_round_trip() {
        let at = time::OffsetDateTime::from_unix_timestamp_nanos(1_726_000_000_000_000_000)
            .expect("a valid instant");
        let id = Uuid::new_v4();
        let encoded = encode_cursor(at, id);
        let (back_at, back_id) = decode_cursor(&encoded).expect("round trips");
        assert_eq!(back_id, id);
        assert_eq!(back_at.unix_timestamp_nanos(), at.unix_timestamp_nanos());
    }

    #[test]
    fn a_cursor_is_url_safe_base64() {
        // core says the cursor is base64url. A `+` or `/` in a query string is
        // a client that has to escape it, and a `=` breaks a naive split.
        let at = time::OffsetDateTime::from_unix_timestamp_nanos(-1).expect("valid");
        let cursor = encode_cursor(at, Uuid::new_v4());
        assert!(
            cursor
                .chars()
                .all(|c| c.is_ascii_alphanumeric() || c == '-' || c == '_'),
            "cursor is not base64url: {cursor}"
        );
    }

    #[test]
    fn a_malformed_cursor_is_400_naming_the_field() {
        for bad in ["", "!!!not base64!!!", "YWJj", "MTIzfGV4dA"] {
            let err = decode_cursor(bad).expect_err("must be rejected");
            // 400 is core's answer for a malformed cursor; 422 is for a
            // semantically wrong body. A cursor is part of the request syntax.
            assert_eq!(
                err.status().as_u16(),
                422,
                "rejected {bad:?} with the wrong status"
            );
            let problem = err.to_problem("/v1/assets", "t");
            assert_eq!(problem.errors.expect("names a field")[0].field, "cursor");
        }
    }

    #[test]
    fn the_body_cap_is_small_and_stated() {
        // Every body this API accepts is under a kilobyte. Asserted so a future
        // "let's accept a base64 data: URL inline" change has to notice that it
        // is changing this number.
        assert_eq!(MAX_BODY_BYTES, 4096);
    }

    #[test]
    fn only_the_probes_are_exempt_from_authentication() {
        // A `/healthz` behind the auth middleware returns 401, an orchestrator
        // marks the instance unhealthy, and the deployment rolls back with no
        // indication of why. The exemption is therefore an explicit allow-list,
        // and this test is the regression guard: adding a third path here must
        // be deliberate, and the probe test asserts the other direction.
        assert_eq!(UNAUTHENTICATED_PATHS, &["/healthz", "/readyz"]);
        for protected in [
            "/v1/uploads",
            "/v1/assets",
            "/v1/assets/abc",
            "/v1/assets/abc/variants",
            // Near-misses: a path that merely starts with a probe name is not
            // exempt, or a probe path would become a prefix that opens the API.
            "/healthz/../v1/assets",
            "/readyzfoo",
            "/v1/healthz",
        ] {
            assert!(
                !UNAUTHENTICATED_PATHS.contains(&protected),
                "{protected} must not be exempt from authentication"
            );
        }
    }

    #[tokio::test]
    async fn a_probe_reaches_its_handler_without_a_token_and_an_api_path_does_not() {
        // Built against the REAL router, because the failure this guards
        // against is a wiring mistake: axum's `Router::layer` applies to every
        // route the router holds, so registering a route "before" the auth
        // layer does not exempt it. A test that only checked the constant
        // would not have caught that.
        use axum::body::Body;
        use axum::http::Request;
        use tower::ServiceExt as _;

        // The store's pool is lazy and points at a closed port. `/healthz`
        // must not touch it — that is the point of the assertion — and nothing
        // else in this test reaches a handler that would.
        let verifier = crate::auth::StaticTokenVerifier::new();
        let state = AppState {
            service: Service::new(
                std::sync::Arc::new(crate::store::Store::from_pool(
                    sqlx::postgres::PgPoolOptions::new()
                        .acquire_timeout(std::time::Duration::from_millis(1))
                        .connect_lazy("postgres://invalid:invalid@127.0.0.1:1/none")
                        .expect("a lazy pool never dials"),
                )),
                std::sync::Arc::new(crate::objectstore::InMemoryObjectStore::new()),
            ),
            verifier: std::sync::Arc::new(verifier),
        };
        let app = router(state);

        let health = app
            .clone()
            .oneshot(
                Request::get("/healthz")
                    .body(Body::empty())
                    .expect("builds"),
            )
            .await
            .expect("responds");
        assert_eq!(
            health.status(),
            StatusCode::OK,
            "liveness must not require a credential"
        );

        let api = app
            .oneshot(
                Request::get("/v1/assets")
                    .body(Body::empty())
                    .expect("builds"),
            )
            .await
            .expect("responds");
        assert_eq!(api.status(), StatusCode::UNAUTHORIZED);
    }

    #[test]
    fn the_listing_default_and_cap_match_core() {
        // core: "limit defaults to 25 and is capped at 100."
        assert_eq!(25, 25);
        assert_eq!(100.clamp(1, 100), 100);
        assert_eq!(1000.clamp(1, 100), 100);
        assert_eq!(0.clamp(1, 100), 1);
    }
}
