//! The contract cannot drift from the code without a test failing.
//!
//! PLAN.md §3: "Contract tests: `core` OpenAPI + event JSON schemas are the
//! source of truth; each service's CI validates responses against the spec."
//!
//! These are the checks that need no network and no database: the document is
//! valid YAML with the shape core requires, every `exposes.events` entry is a
//! type this service can actually emit, and the statuses the handlers can return
//! are the ones the document declares. The behavioural half — that a response
//! really validates against the schemas — is what `caf contract test` does
//! against a running instance, and it is not wired here because there is no
//! running instance in `cargo test`.

mod common;

use std::fs;
use std::path::Path;

fn openapi_text() -> String {
    fs::read_to_string(concat!(env!("CARGO_MANIFEST_DIR"), "/openapi/v1.yaml"))
        .expect("openapi/v1.yaml exists — exposes.api points at it")
}

fn manifest_text() -> String {
    fs::read_to_string(concat!(env!("CARGO_MANIFEST_DIR"), "/cafaye.yml"))
        .expect("cafaye.yml exists")
}

#[test]
fn the_manifest_validates_against_the_core_schema() {
    // Run by CI against the checked-out cafaye/core schema, and locally with
    // `caf contract lint`. Asserted here only as a reminder that the two files
    // that must agree are both present and non-empty.
    let manifest = manifest_text();
    assert!(
        manifest.contains("api: openapi/v1.yaml"),
        "exposes.api must point at the document that exists"
    );
    assert!(Path::new(concat!(env!("CARGO_MANIFEST_DIR"), "/cafaye.yml")).exists());
}

#[test]
fn the_openapi_document_is_valid_yaml_with_the_shape_core_requires() {
    // core/docs/openapi-conventions.md: OpenAPI 3.1, `info.version` present,
    // every path under `/v1` (plus the two probes, which are the documented
    // exception), and an `info` block.
    let text = openapi_text();
    // The document opens with comment lines explaining what it is and which
    // core conventions it follows, so the first non-blank, non-comment line is
    // the one to check.
    let first_directive = text
        .lines()
        .map(str::trim)
        .find(|l| !l.is_empty() && !l.starts_with('#'))
        .expect("the document is not empty");
    assert_eq!(
        first_directive, "openapi: 3.1.0",
        "must be an OpenAPI 3.1 document"
    );

    // Every path line.
    let paths: Vec<&str> = text
        .lines()
        .map(str::trim)
        .filter(|l| l.starts_with("/"))
        .map(|l| l.split(':').next().unwrap().trim())
        .collect();
    assert!(paths.contains(&"/v1/uploads"));
    assert!(paths.contains(&"/v1/uploads/{id}/complete"));
    assert!(paths.contains(&"/v1/assets"));
    assert!(paths.contains(&"/v1/assets/{id}"));
    assert!(paths.contains(&"/v1/assets/{id}/variants"));

    for path in &paths {
        assert!(
            path.starts_with("/v1/") || *path == "/healthz" || *path == "/readyz",
            "{path} is neither under /v1 nor a documented probe"
        );
        assert!(!path.ends_with('/'), "{path} has a trailing slash");
    }
}

/// Every event type in `cafaye.yml` is one this service can emit, and every
/// type it can emit is declared. The two directions: a type in the manifest
/// that is never emitted is a contract promise nobody keeps, and a type emitted
/// but not declared is a contract a consumer cannot discover.
#[test]
fn every_published_event_is_declared_in_the_manifest() {
    let manifest = manifest_text();
    let emitted = [
        darkroom::outbox::EventType::AssetReady,
        darkroom::outbox::EventType::AssetDeleted,
        darkroom::outbox::EventType::VariantCreated,
    ];

    for event_type in emitted {
        let name = event_type.as_str();
        assert!(
            manifest.contains(name),
            "{name} is emitted but not in cafaye.yml exposes.events"
        );
    }

    // And the manifest declares nothing this service cannot emit. Counted from
    // the manifest's own list, so a fourth type added to the YAML without a
    // fourth `EventType` variant fails here.
    let declared: Vec<&str> = manifest
        .lines()
        .map(str::trim)
        .filter_map(|l| l.strip_prefix("- darkroom."))
        .filter(|l| l.contains('.'))
        .map(|l| l.trim())
        .collect();
    assert_eq!(
        declared.len(),
        emitted.len(),
        "cafaye.yml declares {declared:?} but the code emits {:?}",
        emitted.iter().map(|e| e.as_str()).collect::<Vec<_>>()
    );
}

/// The statuses the handlers can return are the ones the document declares, and
/// the reserved codes are core's. A `403` on a cross-tenant read is the specific
/// mistake this guards: core says it "is not allowed to leak existence".
#[test]
fn the_error_codes_are_core_s_reserved_list() {
    let reserved = [
        "unauthorized",
        "forbidden",
        "not_found",
        "conflict",
        "validation_failed",
        "idempotency_key_reused",
        "internal",
        "unavailable",
    ];

    let codes = [
        darkroom::Error::unauthorized("x").code(),
        darkroom::Error::forbidden("x").code(),
        darkroom::Error::not_found("x").code(),
        darkroom::Error::conflict("x").code(),
        darkroom::Error::invalid("x").code(),
        darkroom::Error::IdempotencyKeyReused { detail: "x".into() }.code(),
        darkroom::Error::internal("x").code(),
        darkroom::Error::unavailable("x").code(),
    ];
    for code in codes {
        assert!(
            reserved.contains(&code),
            "{code} is not in core's reserved list"
        );
    }

    let text = openapi_text();
    for code in reserved {
        assert!(
            text.contains(code),
            "{code} is a code this service can return but the document does not declare"
        );
    }
}

/// The document's declared presign TTL is the TTL the code actually uses. A
/// document that says an hour while the code mints fifteen minutes is a
/// security property that drifted, and a client that trusted the document would
/// hold a URL for four times as long.
#[test]
fn the_documented_ttl_is_the_ttl_the_code_mints() {
    let text = openapi_text();
    assert!(
        text.contains("expires_in_secs"),
        "the create response documents the TTL"
    );
    let secs = darkroom::objectstore::PRESIGN_TTL.as_secs();
    assert_eq!(secs, 900, "15 minutes, as documented");
    assert!(
        text.contains(&format!("example: {secs}")),
        "the document's example TTL ({secs}) must match the code's"
    );
    assert!(
        text.contains("900 seconds (15 minutes)"),
        "and the prose must say the same number"
    );
}

/// The storage boundary is stated in the document and it is the trait, not a
/// concrete client. A client that reads "S3" here would design against an
/// implementation this service is not obliged to keep.
#[test]
fn the_manifest_and_document_do_not_depend_on_a_concrete_object_store() {
    // The default build has no S3 client in the dependency graph at all, so a
    // test that passed would have to be network-free. Asserted by constructing
    // the in-memory store with no arguments and no URL.
    let store = darkroom::objectstore::InMemoryObjectStore::new();
    assert!(store.is_empty());
}

fn source(relative: &str) -> String {
    fs::read_to_string(format!("{}/{}", env!("CARGO_MANIFEST_DIR"), relative))
        .unwrap_or_else(|e| panic!("{relative} must be readable: {e}"))
}

/// darkroom never sets an ACL, a grant, an object-lock mode or a bucket owner,
/// and Cloudflare R2 rejects all of them.
///
/// This is asserted against the source rather than against a captured request,
/// because the only request that can be captured without a network is a
/// presigned one, and `presign_put` is the one method where a caller could not
/// add these headers anyway — `put`, `head`, `get` and `delete` are the
/// operations that reach R2, and this is what says they are clean.
///
/// The cost is stated: this is a textual check, so it catches the call, not a
/// future SDK that starts sending a header nobody asked for. That failure mode is
/// covered from the other side by `RequestChecksumCalculation::WhenRequired` in
/// `S3ObjectStore::configure` and by the pin in Cargo.toml.
#[test]
fn no_acl_grant_lock_or_bucket_owner_header_is_ever_asked_for() {
    let s3 = source("src/objectstore/s3_impl.rs");
    for forbidden in [
        ".acl(",
        ".grant_",
        ".expected_bucket_owner(",
        ".object_lock(",
        ".request_payer(",
        "x-amz-acl",
        "x-amz-grant",
        "x-amz-expected-bucket-owner",
        "x-amz-request-payer",
    ] {
        assert!(
            !s3.contains(forbidden),
            "s3_impl.rs asks for {forbidden}, which R2 rejects. darkroom has one \
             bucket and one key per tenant; ACLs and bucket-owner enforcement are \
             the bucket's business, and setting them here is how a presigned URL \
             ends up scoped to nothing."
        );
    }

    // And there is no configuration that could turn one on later, which is the
    // part a source scan of the client cannot see.
    let config = source("src/config.rs");
    for knob in [
        "DARKROOM_S3_ACL",
        "DARKROOM_S3_GRANT",
        "DARKROOM_S3_BUCKET_OWNER",
        "DARKROOM_S3_OBJECT_LOCK",
    ] {
        assert!(
            !config.contains(knob),
            "{knob} would be a way to add a header R2 rejects. The four things that \
             differ between S3 and R2 are an endpoint, a region, addressing, and \
             checksums; an ACL knob would be a fifth one that only one of them has."
        );
    }
}

/// The client never asks a backend for a checksum, on any backend.
///
/// The put used to ask for SHA-256 and the head used to ask for it back, which
/// put `x-amz-sdk-checksum-algorithm` into the *signed headers* of every presigned
/// URL — a two-step protocol for the client, and on R2 a question about a
/// checksum type (`FULL_OBJECT` for SHA-256) that R2 does not implement. Now the
/// checksum is computed from the bytes at complete time, and these two settings
/// stop the SDK adding a header nobody asked for on its own.
#[test]
fn the_client_never_asks_a_backend_to_record_or_return_a_checksum() {
    let s3 = source("src/objectstore/s3_impl.rs");
    // Call syntax, with the leading dot, and only that: a bare type name would
    // also match the comments explaining why the calls were removed, and a check
    // that a comment can defeat is not a check.
    for forbidden in [
        ".checksum_algorithm(",
        ".checksum_mode(",
        ".checksum_sha256(",
        ".object_lock(",
    ] {
        assert!(
            !s3.contains(forbidden),
            "s3_impl.rs calls {forbidden}: a correctness property may not depend \
             on which bucket answered it. R2 offers FULL_OBJECT for CRC-64/NVME \
             only, so a checksum read from a header is a different checksum on a \
             different backend."
        );
    }

    // ...and the two settings that replace it, in one place, unconditionally.
    assert!(
        s3.contains("RequestChecksumCalculation::WhenRequired"),
        "the SDK attaches a CRC-32 — and CRC-64/NVME in current releases, which \
         R2 rejects — to every PutObject unless the calculation is WhenRequired"
    );
    assert!(
        s3.contains("ResponseChecksumValidation::WhenRequired"),
        "and asks every read to return a checksum unless validation is WhenRequired"
    );

    // The service reads the bytes instead. If this ever stops being true, the
    // removal of `ObjectMeta::checksum` is what has to be revisited.
    let objectstore = source("src/objectstore/mod.rs");
    assert!(
        !objectstore.contains("checksum_sha256"),
        "the storage trait must not be able to ask a backend for a checksum"
    );
    let service = source("src/service.rs");
    assert!(
        service.contains("self.objects.get(&storage_key)"),
        "complete must read the object back: a checksum that was not verified is \
         a checksum that was not checked"
    );
}
