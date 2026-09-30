//! Trace context and the logging subscriber.
//!
//! core/docs/openapi-conventions.md: "`trace_id` is always present and always
//! matches the `X-Trace-Id` response header. Support starts from this id."
//! Keeping the id in a task-local is what makes that true by construction
//! rather than by discipline: the error body, the response header, and every
//! `tracing` span in the request read the same value, and nothing can set one
//! without the others.
//!
//! The subscriber is `tracing` over structured fields — never a hand-rolled
//! formatter, and never a string built with `format!` at a call site, which is
//! how log lines stop being queryable.

use std::future::Future;
use std::pin::Pin;
use std::task::{Context, Poll};

use axum::http::Request;
use time::OffsetDateTime;
use time::format_description::well_known::Rfc3339;
use uuid::Uuid;

/// What a request carries for its whole lifetime.
#[derive(Debug, Clone)]
pub struct TraceContext {
    /// Echoed in the `X-Trace-Id` header and in every problem body.
    pub trace_id: String,
    /// The request path, without the query string. core: `instance` "never
    /// includes the query string, which can carry an address."
    pub instance: String,
}

tokio::task_local! {
    /// Set by [`TraceContextLayer`] for the duration of one request. Reads
    /// outside a request return a placeholder rather than panicking: a panic
    /// in an error path is a worse outcome than an unattributed log line.
    static SCOPED: TraceContext;
}

/// The current request's trace id, or `"unattributed"` outside a request.
pub fn current_trace_id() -> String {
    SCOPED
        .try_with(|ctx| ctx.trace_id.clone())
        .unwrap_or_else(|_| "unattributed".to_string())
}

/// The current request's path, or `"/"` outside a request.
pub fn current_instance() -> String {
    SCOPED
        .try_with(|ctx| ctx.instance.clone())
        .unwrap_or_else(|_| "/".to_string())
}

/// Run `future` with `ctx` installed, spawning a span that carries both fields
/// so every line logged underneath it is attributable from the log alone.
pub async fn scope<F>(ctx: TraceContext, fut: F) -> F::Output
where
    F: Future,
{
    let span = tracing::info_span!(
        "request",
        trace_id = %ctx.trace_id,
        path = %ctx.instance,
    );
    // `Instrument` attaches the span to each *poll* of the future rather than to
    // the stack. The alternative — `let _guard = span.enter()` around an `.await`
    // — is the classic Rust logging bug: the guard is `!Send`, so the compiler
    // would refuse it here, and even where it compiles it holds the span open
    // across await points that belong to other tasks.
    use tracing::Instrument as _;
    SCOPED.scope(ctx, fut.instrument(span)).await
}

/// A fresh trace id: a UUIDv4, rendered hyphenated. Not the W3C `traceparent`
/// — the 32-hex form is `TracingId::to_string` on a v4 UUID with the hyphens
/// stripped, and the 16-byte version/variant bits are already correct.
pub fn new_trace_id() -> String {
    Uuid::new_v4().to_string()
}

/// Now, in the RFC3339 form the outbox `time` column and the envelope both use.
/// core: "RFC3339 UTC, when the state change happened — not when the message was
/// queued."
pub fn now() -> OffsetDateTime {
    OffsetDateTime::now_utc()
}

/// Format an instant the way every cafaye event does.
pub fn rfc3339(at: OffsetDateTime) -> String {
    at.format(&Rfc3339)
        .unwrap_or_else(|_| "1970-01-01T00:00:00Z".to_string())
}

/// Tower layer: stamps every request with a trace id and a path before the
/// handler runs, and puts the current value back if a traceparent arrived.
#[derive(Debug, Clone, Copy, Default)]
pub struct TraceContextLayer;

impl<S> tower::Layer<S> for TraceContextLayer {
    type Service = TraceContextService<S>;

    fn layer(&self, inner: S) -> Self::Service {
        TraceContextService { inner }
    }
}

#[derive(Debug, Clone, Copy)]
pub struct TraceContextService<S> {
    inner: S,
}

/// `S::Future` has to name a concrete type, so the boxed pin is what carries
/// `scope()`'s future through the service. Boxed rather than a hand-rolled
/// state machine because there is exactly one state here and a state machine
/// would be more code to get wrong, not less.
impl<S, B> tower::Service<Request<B>> for TraceContextService<S>
where
    S: tower::Service<Request<B>, Response = axum::response::Response> + Clone + Send + 'static,
    S::Future: Send + 'static,
    B: Send + 'static,
{
    type Response = axum::response::Response;
    type Error = S::Error;
    type Future =
        Pin<Box<dyn Future<Output = Result<Self::Response, Self::Error>> + Send>>;

    fn poll_ready(&mut self, cx: &mut Context<'_>) -> Poll<Result<(), Self::Error>> {
        self.inner.poll_ready(cx)
    }

    fn call(&mut self, req: Request<B>) -> Self::Future {
        // Honour an inbound traceparent so a trace that started at the edge
        // stays one trace across services. PLAN.md §7 adopts W3C traceparent
        // "from first deploy". The value is validated to the 55-char
        // version-traceid-spanid shape before it is trusted, because this is
        // untrusted input and it is written straight into every log line.
        let inbound = req
            .headers()
            .get("traceparent")
            .and_then(|v| v.to_str().ok())
            .and_then(parse_traceparent_trace_id)
            .unwrap_or_else(new_trace_id);

        let ctx = TraceContext {
            trace_id: inbound,
            instance: req.uri().path().to_string(),
        };
        let fut = self.inner.call(req);

        Box::pin(scope(ctx, async move { fut.await }))
    }
}

/// Extract the 32-hex trace id from a `traceparent` header, if it is
/// well-formed. Returns `None` for anything else, so a malformed inbound value
/// is replaced rather than propagated into logs.
fn parse_traceparent_trace_id(value: &str) -> Option<String> {
    // 00-<32 hex>-<16 hex>-<2 hex>: version, trace-id, parent-id, flags.
    let mut parts = value.split('-');
    let version = parts.next()?;
    let trace_id = parts.next()?;
    let parent_id = parts.next()?;
    let flags = parts.next()?;
    if parts.next().is_some() {
        return None;
    }
    if version.len() != 2
        || trace_id.len() != 32
        || parent_id.len() != 16
        || flags.len() != 2
        || !trace_id.chars().all(|c| c.is_ascii_hexdigit())
        || !parent_id.chars().all(|c| c.is_ascii_hexdigit())
        || !flags.chars().all(|c| c.is_ascii_hexdigit())
        // A trace-id of all zeroes is the spec's "invalid" sentinel.
        || trace_id.chars().all(|c| c == '0')
    {
        return None;
    }
    Some(trace_id.to_string())
}

/// Install the process-wide subscriber. Called once from `main`, never from a
/// test — a test that installs a global subscriber fights every other test for
/// the same global.
pub fn init_tracing(default_level: &str) {
    use tracing_subscriber::{EnvFilter, fmt, prelude::*};

    // RUST_LOG wins; the argument is the floor so a container that sets nothing
    // still gets structured output at the configured level.
    let filter = EnvFilter::try_from_default_env()
        .unwrap_or_else(|_| EnvFilter::new(default_level));

    // JSON in production, human-readable locally, chosen by whether stdout is a
    // terminal — a log pipeline wants JSON and a person at a terminal does not.
    let json_output = std::env::var("DARKROOM_LOG_FORMAT")
        .map(|v| v == "json")
        .unwrap_or_else(|_| !console_is_terminal());

    let registry = tracing_subscriber::registry().with(filter);

    if json_output {
        registry
            .with(
                fmt::layer()
                    .json()
                    .with_current_span(true)
                    .with_target(true),
            )
            .try_init()
            .ok();
    } else {
        registry
            .with(fmt::layer().with_target(true))
            .try_init()
            .ok();
    }
}

/// `try_init` already swallows a double install, so this only has to answer
/// "is anyone watching".
fn console_is_terminal() -> bool {
    // std has no isatty. Reading /dev/tty by hand is worse than the shell
    // check, and the environment variable is what every container runtime
    // already sets. Absent means a pipe, which is the right default for CI.
    std::env::var("DARKROOM_LOG_FORMAT").is_ok_and(|v| v == "human")
        || std::env::var("TERM").is_ok_and(|term| !term.is_empty() && term != "dumb")
}

#[cfg(test)]
mod tests {
    use super::*;

    #[test]
    fn traceparent_trace_id_is_extracted() {
        assert_eq!(
            parse_traceparent_trace_id("00-4bf92f3577b34da6a3ce929d0e0e4736-00f067aa0ba902b7-01")
                .as_deref(),
            Some("4bf92f3577b34da6a3ce929d0e0e4736")
        );
    }

    #[test]
    fn malformed_traceparent_is_rejected_not_propagated() {
        // This is untrusted input that lands in every log line of the request,
        // so a value that is not exactly the spec's shape is discarded and a
        // fresh id is generated rather than being written through.
        for bad in [
            "",
            "00-4bf92f3577b34da6a3ce929d0e0e4736-00f067aa0ba902b7",       // too few
            "00-4bf92f3577b34da6a3ce929d0e0e4736-00f067aa0ba902b7-01-extra", // too many
            "0-4bf92f3577b34da6a3ce929d0e0e4736-00f067aa0ba902b7-01",        // short version
            "00-ZZZ92f3577b34da6a3ce929d0e0e4736-00f067aa0ba902b7-01",      // non-hex
            "00-00000000000000000000000000000000-00f067aa0ba902b7-01",      // all-zero trace id
            "00-4bf92f3577b34da6a3ce929d0e0e4736-00f067aa0ba902-01",        // short parent id
        ] {
            assert_eq!(
                parse_traceparent_trace_id(bad),
                None,
                "accepted a malformed traceparent: {bad:?}"
            );
        }
    }

    #[tokio::test]
    async fn scope_makes_the_context_readable_inside_and_absent_outside() {
        let ctx = TraceContext {
            trace_id: "trace-xyz".into(),
            instance: "/v1/assets".into(),
        };
        let inside = scope(ctx, async { (current_trace_id(), current_instance()) }).await;
        assert_eq!(inside, ("trace-xyz".into(), "/v1/assets".into()));

        // Outside a request the accessors degrade instead of panicking. An
        // error path that panics is worse than an unattributed log line.
        assert_eq!(current_trace_id(), "unattributed");
        assert_eq!(current_instance(), "/");
    }

    #[test]
    fn rfc3339_round_trips_utc() {
        let at = time::OffsetDateTime::parse(
            "2026-09-30T04:19:00Z",
            &Rfc3339,
        )
        .expect("parses");
        assert_eq!(rfc3339(at), "2026-09-30T04:19:00Z");
    }
}
