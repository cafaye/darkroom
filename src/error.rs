//! The error envelope: RFC 9457 `application/problem+json` plus cafaye's `code`
//! and `trace_id` extensions. Copied from
//! `moon/cafaye/core/docs/openapi-conventions.md#error-envelope`, which is
//! normative: no service invents its own error body.
//!
//! Two rules from that document drive the design here:
//!
//! - "Never 404 for authorization failures on a resource the caller cannot see
//!   — 404 is correct there, 403 is not allowed to leak existence." Tenant
//!   isolation therefore produces [`Error::not_found`], never
//!   [`Error::forbidden`], and every query filters by `account_id` so a
//!   cross-tenant read is an ordinary miss rather than a special case.
//! - "`trace_id` is always present and always matches the `X-Trace-Id` response
//!   header." [`Error::into_response`] writes the header from the same value it
//!   puts in the body, so they cannot drift.

use axum::http::{header, HeaderMap, HeaderName, StatusCode};
use axum::response::{IntoResponse, Response};
use serde::Serialize;

/// `X-Trace-Id`, the header `trace_id` in the body always equals.
pub const TRACE_ID_HEADER: &str = "x-trace-id";
pub const TRACE_ID_HEADER_NAME: HeaderName = HeaderName::from_static(TRACE_ID_HEADER);

/// Base URI for every `type`. core owns this list; the codes below are all in
/// it, and each is the last segment of its own `type`.
const ERROR_BASE: &str = "https://errors.cafaye.com";

/// Every error this service can return.
///
/// The variants are the reserved codes from core, not one error type per
/// endpoint: the code is a machine-readable contract that clients switch on, so
/// the set is deliberately small and every variant has one `status` and one
/// fixed `title`.
#[derive(Debug, thiserror::Error)]
pub enum Error {
    /// 401. No credential, or a credential that did not verify. Never says
    /// *which* part failed.
    #[error("{detail}")]
    Unauthorized { detail: &'static str },

    /// 403. The caller authenticated and holds the scope, but the account in
    /// the token is not theirs. Used for scope failures, never for a resource
    /// that exists under another account — that is [`Error::NotFound`].
    #[error("{detail}")]
    Forbidden { detail: &'static str },

    /// 404. Also the answer to "this asset belongs to another account", so
    /// existence is never confirmed across a tenant boundary.
    #[error("{detail}")]
    NotFound { detail: &'static str },

    /// 409. State conflict: completing an upload whose object never arrived,
    /// or a duplicate that is not allowed to resolve to the existing asset.
    #[error("{detail}")]
    Conflict { detail: String },

    /// 422. The request parsed and was well-formed but is semantically wrong:
    /// a checksum that does not match the stored bytes, an out-of-range
    /// dimension, a `kind` that does not match the content type.
    #[error("{detail}")]
    Validation {
        detail: String,
        errors: Vec<FieldError>,
    },

    /// 409. Same `Idempotency-Key`, different body. Distinct from `conflict`
    /// because a client can act on it: it means "pick a new key", not "the
    /// world changed".
    #[error("{detail}")]
    IdempotencyKeyReused { detail: String },

    /// 500. Something failed that the caller cannot fix and cannot see.
    #[error("{detail}")]
    Internal { detail: &'static str },

    /// 503. A dependency this request needs is not answering — the database,
    /// or object storage.
    #[error("{detail}")]
    Unavailable { detail: &'static str },
}

/// One entry in the `errors[]` array. core: "`errors[]` appears only for 422 and
/// lists per-field failures."
#[derive(Debug, Clone, Serialize)]
pub struct FieldError {
    pub field: String,
    pub code: String,
}

impl FieldError {
    pub fn new(field: impl Into<String>, code: impl Into<String>) -> Self {
        Self {
            field: field.into(),
            code: code.into(),
        }
    }
}

/// The stable code, the HTTP status, and the fixed human title for each variant.
/// core: "`title` is a fixed, human-readable summary for that code; it may be
/// reworded without a version bump."
impl Error {
    pub fn code(&self) -> &'static str {
        match self {
            Error::Unauthorized { .. } => "unauthorized",
            Error::Forbidden { .. } => "forbidden",
            Error::NotFound { .. } => "not_found",
            Error::Conflict { .. } => "conflict",
            Error::Validation { .. } => "validation_failed",
            Error::IdempotencyKeyReused { .. } => "idempotency_key_reused",
            Error::Internal { .. } => "internal",
            Error::Unavailable { .. } => "unavailable",
        }
    }

    pub fn status(&self) -> StatusCode {
        match self {
            Error::Unauthorized { .. } => StatusCode::UNAUTHORIZED,
            Error::Forbidden { .. } => StatusCode::FORBIDDEN,
            Error::NotFound { .. } => StatusCode::NOT_FOUND,
            Error::Conflict { .. } | Error::IdempotencyKeyReused { .. } => StatusCode::CONFLICT,
            Error::Validation { .. } => StatusCode::UNPROCESSABLE_ENTITY,
            Error::Internal { .. } => StatusCode::INTERNAL_SERVER_ERROR,
            Error::Unavailable { .. } => StatusCode::SERVICE_UNAVAILABLE,
        }
    }

    fn title(&self) -> &'static str {
        match self {
            Error::Unauthorized { .. } => "Unauthorized",
            Error::Forbidden { .. } => "Forbidden",
            Error::NotFound { .. } => "Not found",
            Error::Conflict { .. } => "Conflict",
            Error::Validation { .. } => "Validation failed",
            Error::IdempotencyKeyReused { .. } => "Idempotency key reused",
            Error::Internal { .. } => "Internal server error",
            Error::Unavailable { .. } => "Service unavailable",
        }
    }

    /// A 500 or 503 never carries the underlying cause: core says `detail`
    /// "never contains an internal error, a host, a role or a query fragment:
    /// those go to the log under the same `trace_id`."
    pub fn detail(&self) -> &str {
        match self {
            Error::Unauthorized { detail }
            | Error::Forbidden { detail }
            | Error::NotFound { detail }
            | Error::Internal { detail }
            | Error::Unavailable { detail } => detail,
            Error::Conflict { detail }
            | Error::IdempotencyKeyReused { detail }
            | Error::Validation { detail, .. } => detail,
        }
    }

    /// Serde's error body. Every non-2xx response from this service is exactly
    /// this shape, and `trace_id` is threaded in from the request context so it
    /// always equals the response header.
    pub fn to_problem(&self, instance: &str, trace_id: &str) -> Problem {
        let code = self.code();
        Problem {
            r#type: format!("{ERROR_BASE}/{code}"),
            title: self.title().to_string(),
            status: self.status().as_u16(),
            detail: self.detail().to_string(),
            instance: instance.to_string(),
            code: code.to_string(),
            trace_id: trace_id.to_string(),
            // `errors[]` appears only on 422. Emitting an empty array elsewhere
            // is the kind of thing a client starts branching on.
            errors: match self {
                Error::Validation { errors, .. } if !errors.is_empty() => {
                    Some(errors.clone())
                }
                _ => None,
            },
        }
    }
}

/// The wire body. Field names and requiredness match identity's
/// `components.schemas.Problem` exactly — a client generated from either
/// document sees the same object.
#[derive(Debug, Clone, Serialize)]
pub struct Problem {
    pub r#type: String,
    pub title: String,
    pub status: u16,
    pub detail: String,
    pub instance: String,
    pub code: String,
    pub trace_id: String,
    #[serde(skip_serializing_if = "Option::is_none")]
    pub errors: Option<Vec<FieldError>>,
}

/// Constructors, so a handler never writes a status code by hand.
impl Error {
    pub fn unauthorized(detail: &'static str) -> Self {
        Error::Unauthorized { detail }
    }
    pub fn forbidden(detail: &'static str) -> Self {
        Error::Forbidden { detail }
    }
    pub fn not_found(detail: &'static str) -> Self {
        Error::NotFound { detail }
    }
    pub fn conflict(detail: impl Into<String>) -> Self {
        Error::Conflict {
            detail: detail.into(),
        }
    }
    pub fn unavailable(detail: &'static str) -> Self {
        Error::Unavailable { detail }
    }
    /// 500 with a caller-safe message. The cause goes to the log, not here.
    pub fn internal(detail: &'static str) -> Self {
        Error::Internal { detail }
    }
    /// 422 with no per-field detail, for a whole-request failure.
    pub fn invalid(detail: impl Into<String>) -> Self {
        Error::Validation {
            detail: detail.into(),
            errors: Vec::new(),
        }
    }
    /// 422 with per-field detail, which is the shape core actually documents.
    pub fn invalid_fields(detail: impl Into<String>, errors: Vec<FieldError>) -> Self {
        Error::Validation {
            detail: detail.into(),
            errors,
        }
    }
}

impl IntoResponse for Error {
    fn into_response(self) -> Response {
        // Without a request context there is no trace id to report. The
        // middleware installs one on every request that reaches a handler, so
        // this branch only fires for a failure raised before the router — a
        // body that fails to deserialize, or a 404 on no route at all.
        let trace_id = crate::observability::current_trace_id();
        let instance = crate::observability::current_instance();
        let problem = self.to_problem(&instance, &trace_id);

        let status = self.status();
        let mut headers = HeaderMap::new();
        headers.insert(
            header::CONTENT_TYPE,
            axum::http::HeaderValue::from_static("application/problem+json"),
        );
        // The header and the body field come from the same value, so "trace_id
        // always matches the X-Trace-Id response header" is structural.
        if let Ok(value) = axum::http::HeaderValue::from_str(&trace_id) {
            headers.insert(TRACE_ID_HEADER_NAME, value);
        }

        (status, headers, axum::Json(problem)).into_response()
    }
}

impl From<crate::store::StoreError> for Error {
    fn from(err: crate::store::StoreError) -> Self {
        match err {
            // A pool that cannot hand out a connection within its timeout is a
            // dependency failure, not a bug: 503 tells an orchestrator the
            // instance is alive but not ready, and `/readyz` will agree.
            crate::store::StoreError::Unavailable(_) => Error::unavailable("database is unavailable"),
            // Everything else reaching this arm is a query or a constraint we
            // did not expect. 500 with a fixed detail; the cause is logged
            // under the trace id and nowhere else.
            other => {
                tracing::error!(error = %other, "store error");
                Error::internal("the request could not be completed")
            }
        }
    }
}

impl From<crate::objectstore::ObjectStoreError> for Error {
    fn from(err: crate::objectstore::ObjectStoreError) -> Self {
        match err {
            // Absent is a client-visible state: complete-time is where an
            // upload that never landed becomes a 409.
            crate::objectstore::ObjectStoreError::NotFound => {
                Error::conflict("the object was not found in storage")
            }
            crate::objectstore::ObjectStoreError::Unavailable(_) => {
                Error::unavailable("object storage is unavailable")
            }
            other => {
                tracing::error!(error = %other, "object store error");
                Error::internal("the request could not be completed")
            }
        }
    }
}

#[cfg(test)]
mod tests {
    use super::*;

    #[test]
    fn code_is_the_last_segment_of_type() {
        // core: "`code` is the same slug as the last segment of `type`".
        for err in [
            Error::unauthorized("x"),
            Error::forbidden("x"),
            Error::not_found("x"),
            Error::conflict("x"),
            Error::invalid("x"),
            Error::IdempotencyKeyReused {
                detail: "x".into(),
            },
            Error::internal("x"),
            Error::unavailable("x"),
        ] {
            let problem = err.to_problem("/v1/assets", "trace-1");
            let suffix = problem
                .r#type
                .rsplit('/')
                .next()
                .expect("type always has a last segment");
            assert_eq!(suffix, problem.code, "type/code disagree for {err:?}");
        }
    }

    #[test]
    fn reserved_codes_map_to_their_reserved_status() {
        // The mapping in core/docs/openapi-conventions.md, asserted rather than
        // assumed: a refactor that moves a variant to the wrong arm is a
        // breaking change for every client that switches on the code.
        let cases: Vec<(Error, u16)> = vec![
            (Error::unauthorized("x"), 401),
            (Error::forbidden("x"), 403),
            (Error::not_found("x"), 404),
            (Error::conflict("x"), 409),
            (Error::invalid("x"), 422),
            (
                Error::IdempotencyKeyReused {
                    detail: "x".into(),
                },
                409,
            ),
            (Error::internal("x"), 500),
            (Error::unavailable("x"), 503),
        ];
        for (err, status) in cases {
            assert_eq!(err.status().as_u16(), status, "wrong status for {err:?}");
        }
    }

    #[test]
    fn errors_array_is_present_only_on_422() {
        let plain = Error::invalid("no field detail");
        assert!(
            plain.to_problem("/v1/x", "t").errors.is_none(),
            "a 422 with no per-field errors omits the array rather than sending []"
        );

        let fields = Error::invalid_fields(
            "checksum does not match",
            vec![FieldError::new("checksum", "mismatch")],
        );
        let problem = fields.to_problem("/v1/x", "t");
        assert_eq!(problem.status, 422);
        assert_eq!(
            problem.errors.expect("422 carries errors[]")[0].field,
            "checksum"
        );

        // Everything else must not serialise the key at all.
        for err in [Error::not_found("x"), Error::conflict("x"), Error::internal("x")] {
            let json = serde_json::to_string(&err.to_problem("/v1/x", "t")).expect("serialises");
            assert!(
                !json.contains("errors"),
                "non-422 leaked an errors key: {json}"
            );
        }
    }

    #[test]
    fn detail_never_carries_a_host_or_an_internal_cause() {
        // The constructors that are supposed to be caller-safe are.
        let err = Error::unavailable("object storage is unavailable");
        assert_eq!(err.detail(), "object storage is unavailable");
        assert!(!err.detail().contains("postgres://"));
        assert!(!err.detail().contains("10.0.0.5"));
    }

    #[tokio::test]
    async fn response_carries_problem_json_and_a_matching_trace_id_header() {
        use tower::ServiceExt as _;

        let app = axum::Router::new().fallback(|| async {
            crate::observability::SCOPED
                .scope(
                    crate::observability::TraceContext {
                        trace_id: "trace-abc".into(),
                        instance: "/v1/assets".into(),
                    },
                    || async { Err::<(), Error>(Error::not_found("asset not found")) },
                )
                .await
        });

        let response = app
            .oneshot(axum::http::Request::get("/v1/assets").body(axum::body::Body::empty()).unwrap())
            .await
            .expect("router responds");

        assert_eq!(response.status(), 404);
        assert_eq!(
            response.headers()[header::CONTENT_TYPE],
            "application/problem+json"
        );

        let header_trace = response.headers()[TRACE_ID_HEADER]
            .to_str()
            .expect("header is ascii")
            .to_string();
        let body: serde_json::Value =
            serde_json::from_slice(&axum::body::to_bytes(response.into_body(), 64 * 1024).await.unwrap())
                .expect("body is json");
        assert_eq!(
            body["trace_id"].as_str(),
            Some(header_trace.as_str()),
            "trace_id in the body must equal the X-Trace-Id header"
        );
        assert_eq!(body["code"], "not_found");
        assert_eq!(body["status"], 404);
    }
}
