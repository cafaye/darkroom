//! `openapi/v1.yaml` and the router, held to each other in both directions.
//!
//! PLAN.md §3 makes core's OpenAPI document the source of truth and MD6 has the
//! platform generating client SDKs from it, so a path in the document is a
//! method on a generated client. That makes both directions failures rather than
//! tidiness: an operation in the document the router does not serve is a client
//! that 404s in production, and a route the router serves that the document does
//! not describe is a method a generated client does not have. Neither is a thing
//! to find out about from a customer.
//!
//! ## How it can fail, and how it cannot
//!
//! It compares **paths, never counts**. A count comparison is the shape that
//! misses the change that matters: a rename leaves the count alone and a pure
//! addition breaks it, which is exactly backwards.
//!
//! It cannot pass by reading nothing. Every way either side could come back with
//! less than it should — a file that is not there, YAML that does not parse, a
//! document with no `paths:` key, a path item with no operation under it, a
//! `paths:` child that is not a path, an empty route table — is a panic whose
//! message names what it found, because a green check over nothing is worse
//! than no check.
//!
//! It reads the subject out of the manifest rather than hardcoding it. A check
//! that names its own file keeps passing after `exposes.api` moves to a
//! different one, and then it is checking a document nobody ships.
//!
//! ## Where the router's side comes from
//!
//! axum has no `Router::routes()`, so there is nothing to enumerate and a test
//! that wrote the routes out by hand would only ever fail for a name somebody
//! remembered. `http::OPERATIONS` is the route table the router is itself built
//! from, and the set the comparison uses is read out of it.
//!
//! That still leaves one gap, and the tests close it rather than assuming it
//! away: the method in a row of that table is written down, because the method a
//! `MethodRouter` answers is baked into the value `get(handler)` returns and
//! there is no accessor for it.
//! `the_router_answers_exactly_the_methods_the_route_table_declares` therefore
//! asks the router — a request with a method the table does not declare,
//! answered `405` with the `Allow` header enumerating what the router really
//! accepts. A `405`/`404` distinction is what makes that possible, and
//! `a_path_the_router_does_not_serve_is_404_and_not_405` is the control that
//! says the distinction is real.
//!
//! ## What this still cannot see, said here rather than left to be found
//!
//! The `Allow` probe reads a path *it already knows about*, so it catches a
//! method registered on a known path outside the table and it does **not** catch
//! a whole new path registered outside the table. Both halves of that sentence
//! were measured rather than reasoned, by planting a `.route()` call into
//! `router()` beside the fold and reading what each one did:
//!
//!   * `.route("/v1/assets", put(…))` — a method on a path the table knows.
//!     `the_router_answers_exactly_the_methods_the_route_table_declares` failed
//!     with `the router advertises {"GET", "PUT"} on /v1/assets and the route
//!     table declares {"GET"}`, **and the two document tests stayed green**,
//!     because the document does describe that path. Nothing else in this
//!     repository would have noticed.
//!   * `.route("/v1/brand-new", get(…))` — a path nothing knows about.
//!     All nine tests passed. A new surface route appeared in the binary and
//!     this check said nothing at all.
//!
//! So the gap is real and it is not closable from inside a test: nothing in axum
//! will enumerate the paths a `Router` holds, so a check that cannot see a path
//! cannot notice one is there. What narrows it is structural rather than a
//! guarantee: `router()` is a fold over `OPERATIONS`, so there is exactly one
//! line in this repository where a route can be registered by hand.
//!
//! ## No network, no database
//!
//! The probe authenticates, because the auth middleware runs before routing and
//! an unauthenticated request would be `401` for every path, matched or not —
//! which is the one thing that would make the probe vacuous. It then asks for a
//! method the route does not serve, so no handler runs, so the store is never
//! touched and the lazily-connected pool below is never dialled. Nothing in this
//! file opens a socket, and nothing in this file needs `TEST_DATABASE_URL`, so it
//! lives in the default tier and runs on a bare machine.

use std::collections::{BTreeMap, BTreeSet};
use std::fmt::Write as _;
use std::sync::Arc;

use axum::body::Body;
use axum::http::{header, Method, Request, StatusCode};
use darkroom::auth::{Principal, StaticTokenVerifier, SCOPE_ASSETS_READ, SCOPE_ASSETS_WRITE};
use darkroom::http::{router, AppState, OPERATIONS};
use darkroom::objectstore::InMemoryObjectStore;
use darkroom::service::Service;
use darkroom::store::Store;
use saphyr::{LoadableYamlNode, Scalar, Yaml};
use sqlx::postgres::PgPoolOptions;
use tower::ServiceExt as _;
use uuid::Uuid;

// ---------------------------------------------------------------- reading YAML

/// The keys a path item may carry besides its operations, per OpenAPI 3.1's
/// Path Item Object. They belong to the path rather than to any operation on it,
/// so they are not routes.
const PATH_ITEM_FIELDS: &[&str] = &["$ref", "summary", "description", "servers", "parameters"];

/// The method names a path item may declare. `getaway:` is not a `GET`, and a
/// key under a path that is none of these is a field of an operation's body or a
/// typo — either way not a route.
const HTTP_METHODS: &[&str] = &[
    "get", "put", "post", "delete", "options", "head", "patch", "trace",
];

fn read(relative: &str) -> String {
    let path = std::path::Path::new(env!("CARGO_MANIFEST_DIR")).join(relative);
    std::fs::read_to_string(&path)
        .unwrap_or_else(|e| panic!("{relative} has to be readable to be checked: {e}"))
}

/// One YAML document out of a file, or a panic naming the file and the reason.
fn parse<'a>(text: &'a str, origin: &str) -> Yaml<'a> {
    let mut documents = Yaml::load_from_str(text)
        .unwrap_or_else(|e| panic!("{origin} is not valid YAML, so it describes nothing: {e}"));
    assert_eq!(
        documents.len(),
        1,
        "{origin} must be exactly one YAML document, and this one has {}",
        documents.len()
    );
    documents.pop().expect("checked non-empty above")
}

/// A scalar's text. A key or a value that is not a string is a document this
/// reader does not understand, and guessing which was meant is how a check ends
/// up agreeing with something.
fn scalar<'a>(node: &'a Yaml<'_>, what: &str) -> &'a str {
    match node {
        Yaml::Value(Scalar::String(s)) => s.as_ref(),
        // Only reachable if scalar parsing is turned off, which it is not. Handled
        // rather than left to a `panic!` the reader would raise for a reason that
        // has nothing to do with the document.
        Yaml::Representation(s, ..) => s.as_ref(),
        other => panic!("{what} is not a string, it is {other:?}"),
    }
}

/// A key of `node`, or a panic. Used for every lookup, so a missing key can
/// never read as an absent value.
fn field<'a, 'y>(node: &'a Yaml<'y>, key: &str, what: &str) -> &'a Yaml<'y> {
    match node {
        Yaml::Mapping(mapping) => mapping
            .iter()
            .find(|(k, _)| scalar(k, "a mapping key") == key)
            .map(|(_, v)| v)
            .unwrap_or_else(|| {
                panic!(
                    "{what} has no `{key}` key. A reader that cannot find what it is \
                     looking for finds nothing, and a check over nothing passes."
                )
            }),
        other => panic!("{what} is not a mapping, it is {other:?}"),
    }
}

// -------------------------------------------------------------- normalising

/// The key an operation is compared under: an upper-case method and a path whose
/// every parameter segment has become `{}`.
///
/// Both sides go through this one function. A normaliser with different rules
/// for the document and the router is a normaliser that can hide the difference
/// it exists to find.
fn operation_key(method: &str, path: &str) -> (String, String) {
    let normalised = path
        .split('/')
        .map(|segment| {
            // A parameter is a whole segment on both sides: the document writes
            // `{id}`, the router writes `{id}`, and a client substitutes one
            // positionally, so renaming the parameter renames something inside
            // one route rather than adding a second one. A segment that merely
            // *contains* a brace is left alone, because that is a different
            // shape and rewriting it is the kind of rule that hides a real
            // difference.
            if segment.starts_with('{') || segment.starts_with(':') {
                "{}"
            } else {
                segment
            }
        })
        .collect::<Vec<_>>()
        .join("/");
    let trimmed = normalised.trim_end_matches('/');
    (
        method.to_ascii_uppercase(),
        if trimmed.is_empty() {
            "/".to_string()
        } else {
            trimmed.to_string()
        },
    )
}

// ---------------------------------------------------------------- the document

/// The path `cafaye.yml` publishes as this service's API. Read out of the
/// manifest so this check cannot end up reading a file the platform does not
/// ship.
fn document_path() -> String {
    let text = read("cafaye.yml");
    let manifest = parse(&text, "cafaye.yml");
    let api = field(&manifest, "exposes", "cafaye.yml");
    scalar(
        field(api, "api", "cafaye.yml `exposes`"),
        "cafaye.yml `exposes.api`",
    )
    .to_string()
}

/// Every operation `openapi/v1.yaml` describes, keyed by [`operation_key`].
fn document_operations() -> Operations {
    let relative = document_path();
    let text = read(&relative);
    let document = parse(&text, &relative);
    let paths = field(&document, "paths", &relative);

    let items = match paths {
        Yaml::Mapping(mapping) => mapping,
        other => panic!(
            "{relative} has a `paths:` key that is {other:?} rather than a mapping of \
             paths to operations. Skipping it would leave this check comparing an \
             empty document against a real router and calling it agreement."
        ),
    };

    let mut found = Operations::new();
    for (key, item) in items.iter() {
        let path = scalar(key, "a key under `paths:`").to_string();
        if PATH_ITEM_FIELDS.contains(&path.as_str()) || path.starts_with("x-") {
            continue;
        }
        assert!(
            path.starts_with('/'),
            "{relative} has `{path}` directly under `paths:`, and a key there is a \
             path starting with `/` or a field of the Path Item Object \
             ({}). This reader refuses the document rather than guessing which was meant.",
            PATH_ITEM_FIELDS.join(", ")
        );

        let children = match item {
            Yaml::Mapping(mapping) => mapping,
            other => panic!(
                "{relative} describes `{path}` as {other:?}, which has no operations \
                 under it. A path item a client cannot generate a method from is not \
                 a path item, and skipping it would leave this check agreeing with a \
                 document that says nothing about this route."
            ),
        };

        let methods: Vec<String> = children
            .iter()
            .filter_map(|(k, _)| {
                let name = scalar(k, "a key under a path item").to_ascii_lowercase();
                HTTP_METHODS.contains(&name.as_str()).then_some(name)
            })
            .collect();
        assert!(
            !methods.is_empty(),
            "{relative} describes `{path}` with no operation under it. Every key of a \
             path item that is not a method is either a field of the path or a field \
             of an operation, so there is nothing here a generated client could call."
        );

        for method in methods {
            insert(
                &mut found,
                Found {
                    method: method.to_ascii_uppercase(),
                    path: operation_key(&method, &path).1,
                    origin: format!("{relative} `{path}`"),
                },
            );
        }
    }
    found
}

// -------------------------------------------------------------- the route table

/// Every operation [`OPERATIONS`] declares, keyed by [`operation_key`].
fn declared_operations() -> Operations {
    assert!(
        !OPERATIONS.is_empty(),
        "http::OPERATIONS is empty, so this service serves nothing. A router-reading \
         check that finds no routes agrees with a document-reading check that finds \
         no paths, and two empty sets always agree."
    );
    let mut found = Operations::new();
    for operation in OPERATIONS {
        let (method, path) = operation_key(operation.method.as_str(), operation.path);
        insert(
            &mut found,
            Found {
                method,
                path,
                origin: format!("http::OPERATIONS `{}`", operation.path),
            },
        );
    }
    found
}

// ------------------------------------------------------------------ exclusions

/// A route the router serves that the document deliberately does not describe.
struct Omission {
    method: &'static str,
    path: &'static str,
    why: &'static str,
}

/// darkroom's declared omissions: **none**.
///
/// courier excludes `/healthz` and `/readyz`, and that is the right call there —
/// its document is a customer-facing menu and its header says the probes are
/// out. darkroom's document already describes both, under a `probes` tag, with
/// `security: []` and a description that says who consumes them, so excluding
/// them here would mean deleting correct documentation to satisfy a carve-out.
/// `tests/contract.rs` independently requires every path in the document to be
/// either under `/v1` or one of the two probes, so the two files agree about
/// which paths exist.
///
/// This list is empty rather than absent, and that is the point: it is a closed
/// list, keyed by method *and* path, and `the_omissions_are_the_probes_and_nothing_else`
/// fails if it grows, so the first omission is a change somebody reads.
/// `a_declared_omission_still_names_an_operation_the_router_serves` fails if an
/// entry stops naming anything, so a rename cannot hide behind a stale carve-out.
const OMISSIONS: &[Omission] = &[];

// -------------------------------------------------------------------- the diff

/// One operation, keyed by [`operation_key`] and carrying the spelling its
/// source used, so a failure can name `/v1/assets/{id}` rather than the
/// parameter-erased `/v1/assets/{}`.
#[derive(Debug, Clone, PartialEq, Eq, PartialOrd, Ord)]
struct Found {
    method: String,
    path: String,
    origin: String,
}

impl Found {
    fn label(&self) -> String {
        format!("{} {}", self.method, self.path)
    }

    fn key(&self) -> (String, String) {
        (self.method.clone(), self.path.clone())
    }
}

type Operations = BTreeMap<(String, String), Found>;

fn insert(found: &mut Operations, operation: Found) {
    let key = operation.key();
    if let Some(existing) = found.get(&key) {
        panic!(
            "two operations both read as {}: {} and {}. They normalise onto one \
             route, so a check that could not see the collision would be blind to it.",
            operation.label(),
            existing.origin,
            operation.origin
        );
    }
    found.insert(key, operation);
}

/// Where the two sets disagree, in three parts.
struct Drift {
    /// The document has it and the router does not. The direction that 404s a
    /// client generated from the document.
    documented_not_served: Vec<Found>,
    /// The ones no declared omission covers. This is what a test fails on; the
    /// difference between it and `served_not_documented` is a declared choice
    /// rather than an oversight.
    unexplained: Vec<Found>,
    /// The declared omissions, so a carve-out reads as a decision somebody made.
    omitted: Vec<(Found, &'static str)>,
}

fn drift() -> Drift {
    let document = document_operations();
    let router = declared_operations();
    let omissions: BTreeMap<(String, String), &'static str> = OMISSIONS
        .iter()
        .map(|o| (operation_key(o.method, o.path), o.why))
        .collect();

    // Every operation the router serves that the document does not describe,
    // split by whether a declared omission covers it. The split is the whole
    // point of `OMISSIONS`: a route missing from the document is a failure, and
    // the only thing that turns it into a choice is a closed list somebody
    // reads and this test pins to a size.
    let mut omitted = Vec::new();
    let mut unexplained = Vec::new();
    for (key, found) in &router {
        if document.contains_key(key) {
            continue;
        }
        match omissions.get(key) {
            Some(why) => omitted.push((found.clone(), *why)),
            None => unexplained.push(found.clone()),
        }
    }

    Drift {
        documented_not_served: document
            .iter()
            .filter(|(key, _)| !router.contains_key(*key))
            .map(|(_, found)| found.clone())
            .collect(),
        unexplained,
        omitted,
    }
}

/// The message a failure prints, built to be actionable on its own: every
/// offending operation is named, each says which side has it, and the two ways
/// to fix it are spelled out with the file to change.
fn describe(diff: &Drift) -> String {
    let mut out = String::new();

    if !diff.documented_not_served.is_empty() {
        let _ = writeln!(
            out,
            "the document describes {} the router does not serve.\n",
            count(&diff.documented_not_served, "operation")
        );
        out.push_str(
            "  An endpoint in the menu that answers 404 is a product configured against a\n  \
             route that does not exist, and they find out at their outage.\n\n  Offending \
             operations:\n",
        );
        for operation in &diff.documented_not_served {
            let _ = writeln!(
                out,
                "    {}  (from {})",
                operation.label(),
                operation.origin
            );
        }
        out.push_str(
            "\n  To fix this, do one of the two things:\n    \
             1. The route is real and shipping — add a row to `http::OPERATIONS` in\n       \
             src/http.rs, which is what the router is built from.\n    \
             2. The operation is not shipping — delete it from the document.\n",
        );
    }

    if !diff.unexplained.is_empty() {
        let _ = writeln!(
            out,
            "the router serves {} the document does not describe.\n",
            count(&diff.unexplained, "operation")
        );
        out.push_str(
            "  A route nobody documented is a route the next generated client will not\n  \
             have, so the surface grows and the contract does not.\n\n  Offending \
             operations:\n",
        );
        for operation in &diff.unexplained {
            let _ = writeln!(
                out,
                "    {}  (from {})",
                operation.label(),
                operation.origin
            );
        }
        out.push_str(
            "\n  To fix this, do one of the two things:\n    \
             1. The route is part of darkroom's public surface — document it in\n       \
             openapi/v1.yaml.\n    \
             2. The route is not meant to be public — add it to `OMISSIONS` in this\n       \
             file, with the reason. `the_omissions_are_the_probes_and_nothing_else`\n       \
             then fails until somebody reads that the list grew.\n",
        );
    }

    if !diff.omitted.is_empty() {
        let _ = writeln!(
            out,
            "\nDeclared omissions — the router serves these and the document does not, on purpose:"
        );
        for (operation, why) in &diff.omitted {
            let _ = writeln!(out, "  {} — {why}", operation.label());
        }
    }

    let _ = write!(
        out,
        "\n`cafaye.yml` declares `exposes.api`, which is the file this check reads, and \
         PLAN.md MD6 has SDK generation reading that file, so every operation in it is \
         a method on a generated client. This check is \
         `tests/openapi_document.rs`; the route table it reads is `http::OPERATIONS` in \
         src/http.rs."
    );

    out
}

fn count(operations: &[Found], noun: &str) -> String {
    if operations.len() == 1 {
        format!("1 {noun}")
    } else {
        format!("{} {noun}s", operations.len())
    }
}

// ---------------------------------------------------------- asking the router

/// The token the probe authenticates with. The probe needs a credential
/// because the auth middleware runs *before* routing, so an unauthenticated
/// request is `401` whether or not a route matched — and a probe that cannot
/// tell those apart is not a probe.
const TOKEN: &str = "openapi-drift-probe";

fn app() -> axum::Router {
    let verifier = StaticTokenVerifier::new().with_token(
        TOKEN,
        Principal {
            user_id: Uuid::nil(),
            account_id: Uuid::nil(),
            scopes: vec![SCOPE_ASSETS_READ.into(), SCOPE_ASSETS_WRITE.into()],
        },
    );
    let state = AppState {
        service: Service::new(
            Arc::new(Store::from_pool(
                PgPoolOptions::new()
                    .acquire_timeout(std::time::Duration::from_millis(1))
                    .connect_lazy("postgres://invalid:invalid@127.0.0.1:1/none")
                    .expect("a lazy pool never dials"),
            )),
            Arc::new(InMemoryObjectStore::new()),
        ),
        verifier: Arc::new(verifier),
    };
    router(state)
}

fn authenticated(method: Method, path: &str) -> Request<Body> {
    Request::builder()
        .method(method)
        .uri(path)
        .header(header::AUTHORIZATION, format!("Bearer {TOKEN}"))
        .body(Body::empty())
        .expect("builds")
}

/// A method the route table does not declare anywhere.
///
/// Chosen from the table rather than hardcoded, because a hardcoded one is a
/// verb some future endpoint could start using — and then this would be asking
/// the router a question it answers directly.
fn probe_verb() -> Method {
    ["TRACE", "PATCH", "PUT", "OPTIONS"]
        .into_iter()
        .find(|verb| !OPERATIONS.iter().any(|op| op.method.as_str() == *verb))
        .expect(
            "http::OPERATIONS declares every method this probe could ask with, so there \
             is no verb left to ask about one the table does not already say",
        )
        .parse()
        .expect("a literal method parses")
}

/// Every method the router says it accepts on `path`, read out of the `Allow`
/// header it returns for one it does not.
async fn methods_the_router_advertises(
    app: &axum::Router,
    path: &str,
    verb: &Method,
) -> BTreeSet<String> {
    let response = app
        .clone()
        .oneshot(authenticated(verb.clone(), path))
        .await
        .expect("the router responds");
    assert_eq!(
        response.status(),
        StatusCode::METHOD_NOT_ALLOWED,
        "{path} answered {} for {verb}, a method http::OPERATIONS does not declare, so \
         the router does not serve the path the route table names. A path the router \
         does not know at all would be 404, which is the control in \
         `a_path_the_router_does_not_serve_is_404_and_not_405`.",
        response.status()
    );
    let allow = response
        .headers()
        .get(header::ALLOW)
        .expect("a 405 from axum names the methods it would have accepted")
        .to_str()
        .expect("a header value built from ASCII method names");
    allow
        .split(',')
        .map(|method| method.trim().to_ascii_uppercase())
        .collect()
}

// ------------------------------------------------------------------- the tests

#[test]
fn the_manifest_names_the_document_this_check_reads() {
    // Without this the check would happily read a file `exposes.api` no longer
    // points at, and pass on a document the platform does not ship.
    assert_eq!(
        document_path(),
        "openapi/v1.yaml",
        "cafaye.yml `exposes.api` is the file this check reads, so if it moved, this \
         check moved with it rather than reading whatever is left behind"
    );
}

#[test]
fn the_document_was_read_and_it_is_not_empty() {
    // Without this the two tests below compare an empty set against an empty set
    // and go green having checked nothing. The reader panics on most ways of
    // under-reading; this catches the rest.
    let document = document_operations();
    assert!(
        !document.is_empty(),
        "the document was read as describing no operations at all. A check over none \
         passes, and a green check over nothing is worse than no check."
    );
}

#[test]
fn the_router_was_read_and_it_is_not_empty() {
    let router = declared_operations();
    assert!(
        !router.is_empty(),
        "http::OPERATIONS was read as serving no operations at all. A router-reading \
         check that finds nothing agrees with a document-reading check that finds \
         nothing."
    );
}

#[test]
fn every_operation_the_document_describes_is_one_the_router_serves() {
    // The dangerous direction, and the one that 404s a customer: an operation in
    // the menu that answers 404 is a product configured against a route that does
    // not exist.
    let diff = drift();
    assert!(diff.documented_not_served.is_empty(), "{}", describe(&diff));
}

#[test]
fn every_operation_the_router_serves_is_one_the_document_describes() {
    // The other direction. A route nobody documented is a route the next
    // generated client will not have.
    let diff = drift();
    assert!(diff.unexplained.is_empty(), "{}", describe(&diff));
}

#[test]
fn a_declared_omission_still_names_an_operation_the_router_serves() {
    // The stale-exclusion guard. If `/healthz` were renamed or removed, an
    // omission naming it would still be in the list while matching nothing — and
    // a carve-out that matches nothing is a hole waiting for the next route to
    // fall into it.
    let router = declared_operations();
    for omission in OMISSIONS {
        let key = operation_key(omission.method, omission.path);
        assert!(
            router.contains_key(&key),
            "the omission for {} {} names an operation the router does not serve. \
             Either the route was renamed or removed, in which case the omission has \
             to go with it, or the omission is hiding a route that belongs in the \
             document.",
            omission.method,
            omission.path
        );
    }
}

#[test]
fn the_omissions_are_the_probes_and_nothing_else() {
    // Not a route list — a bound. An omission set that grows is a document losing
    // its coverage quietly, and pinning it here is what makes growth a change
    // somebody reads rather than a diff line that scrolls past. darkroom's set is
    // empty, and this is the test that says so.
    let declared: Vec<(String, String)> = OMISSIONS
        .iter()
        .map(|o| operation_key(o.method, o.path))
        .collect();
    assert!(
        declared.is_empty(),
        "openapi/v1.yaml describes every route the router serves, the two probes \
         included, so the omission list is empty. OMISSIONS now names {declared:?} — \
         if that is deliberate, say why in the document's header as well as here, and \
         update this test to match. A carve-out that is not in the document is a \
         silent one."
    );
}

#[tokio::test]
async fn the_router_answers_exactly_the_methods_the_route_table_declares() {
    // The method in a row of `http::OPERATIONS` is written down, because axum
    // will not say which one a `MethodRouter` answers. So this asks it. A path
    // the router knows is answered `405` for an unknown verb and names what it
    // would have accepted; a path it does not know is `404`. Comparing the whole
    // `Allow` set against the whole declared set makes both a wrong method and a
    // method registered outside the table a failure.
    let app = app();
    let verb = probe_verb();
    let declared = declared_operations();

    let mut paths: Vec<String> = declared
        .values()
        .map(|operation| operation.path.clone())
        .collect();
    paths.sort();
    paths.dedup();

    for path in paths {
        let expected: BTreeSet<String> = declared
            .values()
            .filter(|operation| operation.path == path)
            .map(|operation| operation.method.clone())
            .collect();
        let mut advertised = methods_the_router_advertises(&app, &path, &verb).await;

        // axum answers a `HEAD` request from the `GET` handler, so `Allow` for a
        // GET route is `GET,HEAD`. HEAD is implied by GET rather than declared,
        // and a row declaring it would be the one case where it is not.
        if !expected.contains("HEAD") {
            advertised.remove("HEAD");
        }

        assert_eq!(
            advertised, expected,
            "the router advertises {advertised:?} on {path} and the route table \
             declares {expected:?}. Either a row in `http::OPERATIONS` names a method \
             the router does not answer, or a method was registered on this path \
             outside the table — which is exactly the drift a list written out inside \
             a test would not have noticed."
        );
    }
}

#[tokio::test]
async fn a_path_the_router_does_not_serve_is_404_and_not_405() {
    // The control for the test above, without which that test could be green for
    // the wrong reason: a router that answered `405` to everything would satisfy
    // every `405` assertion in it.
    let app = app();
    let response = app
        .oneshot(authenticated(probe_verb(), "/v1/no-such-operation"))
        .await
        .expect("the router responds");
    assert_eq!(
        response.status(),
        StatusCode::NOT_FOUND,
        "a path the router does not serve must be 404, or the 405 the other test reads \
         means nothing. Got {}.",
        response.status()
    );
}
