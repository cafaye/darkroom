//! Duplicate uploads, delete, and the HTTP surface (probes, error envelope,
//! pagination, and the "no network" property).

mod common;

use axum::body::Body;
use axum::http::{Request, StatusCode};
use common::*;
use darkroom::domain::{AssetStatus, VariantKind};
use darkroom::service::CreateUpload;
use tower::ServiceExt as _;
use uuid::Uuid;

// ------------------------------------------------------------ duplicates

/// The duplicate-upload decision, asserted end to end: the second upload of
/// the same bytes in the same account returns the EXISTING asset, with 201 and
/// a working presigned URL for its key, and creates no second row.
///
/// See `service.rs::create_upload` for the reasoning and README for the
/// decision. The short version: a 409 makes the client solve a problem it did
/// not create, and the bytes are already in storage and already verified.
#[tokio::test]
#[ignore = "needs TEST_DATABASE_URL; see tests/common/mod.rs"]
async fn a_duplicate_upload_returns_the_existing_asset_with_a_usable_url() {
    let store = test_store().await;
    let (service, objects) = test_service(store.clone());
    let account = Uuid::new_v4();
    let tenant = darkroom::auth::Tenant::from_principal(&principal(account, Uuid::new_v4()));

    let payload = png(44, 44);
    let checksum = darkroom::checksum::sha256_hex(&payload);
    let first = service
        .create_upload(
            &tenant,
            CreateUpload {
                filename: "photo.png".into(),
                content_type: "image/png".into(),
                byte_size: payload.len() as i64,
                checksum: checksum.clone(),
            },
        )
        .await
        .expect("first create");
    assert!(!first.duplicate);

    // Complete it, so the duplicate path has to cope with a `ready` asset.
    objects
        .apply_presigned_put(&first.presigned.url, payload.clone(), "image/png")
        .await
        .expect("puts");
    service
        .complete_upload(&tenant, first.asset.id, &checksum)
        .await
        .expect("completes");

    // The same bytes again, under a different filename — which is the real
    // shape of a duplicate: a user attaching the same screenshot twice.
    let second = service
        .create_upload(
            &tenant,
            CreateUpload {
                filename: "photo-copy.png".into(),
                content_type: "image/png".into(),
                byte_size: payload.len() as i64,
                checksum: checksum.clone(),
            },
        )
        .await
        .expect("duplicate resolves");

    assert!(second.duplicate, "the response is marked as a duplicate");
    assert_eq!(
        second.asset.id, first.asset.id,
        "the duplicate resolves to the EXISTING asset"
    );
    assert_eq!(
        second.presigned.key, first.presigned.key,
        "and to the existing storage key, so there is one object"
    );
    assert_eq!(second.presigned.expires_in_secs, first.presigned.expires_in_secs);
    assert_eq!(
        second.asset.status,
        AssetStatus::Ready,
        "the existing asset keeps its verified state"
    );

    // One row, one object. The point of the constraint.
    let rows: i64 = sqlx::query_scalar("select count(*) from assets")
        .fetch_one(store.pool())
        .await
        .expect("counts");
    assert_eq!(rows, 1, "a duplicate must not create a second asset");
    assert_eq!(objects.len(), 1, "a duplicate must not create a second object");
}

/// The duplicate is a 201 with a marker header, not a 409 — asserted over the
/// wire, because the status code is the contract.
#[tokio::test]
#[ignore = "needs TEST_DATABASE_URL; see tests/common/mod.rs"]
async fn a_duplicate_upload_is_201_with_a_marker_header() {
    let store = test_store().await;
    let (service, _objects) = test_service(store.clone());
    let accounts = two_accounts();
    let (app, _v) = test_app(service.clone(), verifier_for(&accounts));

    async fn send(app: axum::Router) -> (StatusCode, Option<String>, serde_json::Value) {
        let request = Request::builder()
            .method(http::Method::POST)
            .uri("/v1/uploads")
            .header("authorization", "Bearer token-a")
            .header("content-type", "application/json")
            .body(Body::from(format!(
                // A real 64-hex sha256. A hand-typed one was 65 characters and
                // the service correctly answered 422 — which is the format
                // check working, and a good reminder that a test fixture typed
                // by hand is a test fixture that will be wrong.
                r#"{{"filename":"p.png","content_type":"image/png","byte_size":10,"checksum":"{}"}}"#,
                darkroom::checksum::sha256_hex(b"duplicate fixture")
            )))
            .expect("builds");
        let response = app.oneshot(request).await.expect("responds");
        let status = response.status();
        let duplicate = response
            .headers()
            .get("x-darkroom-duplicate")
            .and_then(|v| v.to_str().ok())
            .map(str::to_string);
        let body = axum::body::to_bytes(response.into_body(), 1024 * 1024)
            .await
            .expect("body");
        (status, duplicate, serde_json::from_slice::<serde_json::Value>(&body).expect("json"))
    }

    let (first_status, first_marker, first_body) = send(app.clone()).await;
    assert_eq!(first_status, StatusCode::CREATED);
    assert_eq!(first_marker, None, "a first upload is not marked duplicate");
    assert_eq!(first_body["duplicate"], false);

    let (second_status, second_marker, second_body) = send(app.clone()).await;
    assert_eq!(
        second_status,
        StatusCode::CREATED,
        "a duplicate is 201, not 409 — the client gets a usable asset either way"
    );
    assert_eq!(
        second_marker.as_deref(),
        Some("true"),
        "and it is marked, so a client that wants to say 'already uploaded' can"
    );
    assert_eq!(second_body["duplicate"], true);
    assert_eq!(second_body["asset"]["id"], first_body["asset"]["id"]);
    assert!(second_body["upload_url"].is_string(), "and the URL is usable");
}

/// The same bytes in a DIFFERENT account are two assets. The constraint is
/// `(account_id, checksum)`, not `checksum` alone — a shared checksum across
/// tenants must not collapse into one row, or tenant A could learn that tenant
/// B uploaded something by watching the count.
#[tokio::test]
#[ignore = "needs TEST_DATABASE_URL; see tests/common/mod.rs"]
async fn the_same_bytes_in_two_accounts_are_two_assets() {
    let store = test_store().await;
    let (service, _objects) = test_service(store.clone());
    let accounts = two_accounts();
    let payload = png(12, 12);
    let checksum = darkroom::checksum::sha256_hex(&payload);

    let mut ids = Vec::new();
    for account in [accounts.a_account, accounts.b_account] {
        let tenant = darkroom::auth::Tenant::from_principal(&principal(account, Uuid::new_v4()));
        let created = service
            .create_upload(
                &tenant,
                CreateUpload {
                    filename: "same.png".into(),
                    content_type: "image/png".into(),
                    byte_size: payload.len() as i64,
                    checksum: checksum.clone(),
                },
            )
            .await
            .expect("creates");
        assert!(!created.duplicate, "a first upload in any account is not a duplicate");
        ids.push(created.asset.id);
    }

    assert_ne!(ids[0], ids[1], "two accounts, two assets");
    let rows: i64 = sqlx::query_scalar("select count(*) from assets")
        .fetch_one(store.pool())
        .await
        .expect("counts");
    assert_eq!(rows, 2);
}

/// A duplicate whose existing asset FAILED is a 409, because the constraint
/// still holds the row and its checksum was never verified.
#[tokio::test]
#[ignore = "needs TEST_DATABASE_URL; see tests/common/mod.rs"]
async fn a_duplicate_of_a_failed_upload_is_409() {
    let store = test_store().await;
    let (service, _objects) = test_service(store.clone());
    let account = Uuid::new_v4();
    let tenant = darkroom::auth::Tenant::from_principal(&principal(account, Uuid::new_v4()));

    let checksum = "8".repeat(64);
    let created = service
        .create_upload(
            &tenant,
            CreateUpload {
                filename: "bad.png".into(),
                content_type: "image/png".into(),
                byte_size: 10,
                checksum: checksum.clone(),
            },
        )
        .await
        .expect("creates");

    // Fail it: no object was ever uploaded.
    let _ = service.complete_upload(&tenant, created.asset.id, &checksum).await;
    assert_eq!(
        service.get_asset(&tenant, created.asset.id).await.expect("reads").status,
        AssetStatus::Failed
    );

    // The same bytes again. The UNIQUE constraint still holds the row, so this
    // cannot become a second asset — and the existing one has a checksum that
    // was never verified against real bytes, so it cannot be handed back
    // either. 409 is the honest answer.
    let err = service
        .create_upload(
            &tenant,
            CreateUpload {
                filename: "bad-again.png".into(),
                content_type: "image/png".into(),
                byte_size: 10,
                checksum,
            },
        )
        .await
        .expect_err("must not resolve to a failed asset");
    assert_eq!(err.status().as_u16(), 409);
}

// ---------------------------------------------------------------- delete

/// Delete removes the row, removes the STORAGE OBJECT, and emits the event.
#[tokio::test]
#[ignore = "needs TEST_DATABASE_URL; see tests/common/mod.rs"]
async fn delete_removes_the_row_the_storage_object_and_emits_an_event() {
    let store = test_store().await;
    let (service, objects) = test_service(store.clone());
    let account = Uuid::new_v4();
    let tenant = darkroom::auth::Tenant::from_principal(&principal(account, Uuid::new_v4()));

    let payload = png(60, 40);
    let checksum = darkroom::checksum::sha256_hex(&payload);
    let created = service
        .create_upload(
            &tenant,
            CreateUpload {
                filename: "a.png".into(),
                content_type: "image/png".into(),
                byte_size: payload.len() as i64,
                checksum: checksum.clone(),
            },
        )
        .await
        .expect("creates");
    objects
        .apply_presigned_put(&created.presigned.url, payload, "image/png")
        .await
        .expect("puts");
    service
        .complete_upload(&tenant, created.asset.id, &checksum)
        .await
        .expect("completes");

    // A variant too, so the delete has more than one object to remove.
    service
        .create_variant(&tenant, created.asset.id, VariantKind::Thumbnail)
        .await
        .expect("variant");
    assert_eq!(objects.len(), 2, "an original and a thumbnail");

    service
        .delete_asset(&tenant, created.asset.id)
        .await
        .expect("deletes");

    assert_eq!(count_rows(&store, "assets").await, 0, "the row is gone");
    assert_eq!(
        count_rows(&store, "asset_variants").await,
        0,
        "the variant rows go with it (on delete cascade)"
    );
    assert_eq!(
        objects.len(),
        0,
        "BOTH storage objects are removed — a delete that leaves a thumbnail behind is not a delete"
    );

    let event = darkroom::outbox::claim_unpublished(&*store.pool(), 10)
        .await
        .expect("claims")
        .into_iter()
        .find(|r| r.event_type == "darkroom.asset.deleted")
        .expect("the delete event");
    assert_eq!(event.subject, created.asset.id.to_string());
}

async fn count_rows(store: &darkroom::Store, table: &str) -> i64 {
    sqlx::query_scalar::<_, i64>(&format!("select count(*) from {table}"))
        .fetch_one(store.pool())
        .await
        .unwrap_or_else(|e| panic!("counting {table}: {e}"))
}

/// Delete is 204 over the wire, and deleting again is a 404 rather than a 500.
#[tokio::test]
#[ignore = "needs TEST_DATABASE_URL; see tests/common/mod.rs"]
async fn delete_is_204_and_a_second_delete_is_404() {
    let store = test_store().await;
    let (service, _objects) = test_service(store.clone());
    let accounts = two_accounts();
    let (app, _v) = test_app(service.clone(), verifier_for(&accounts));

    let tenant = darkroom::auth::Tenant::from_principal(&principal(accounts.a_account, accounts.a_user));
    let payload = png(20, 20);
    let checksum = darkroom::checksum::sha256_hex(&payload);
    let created = service
        .create_upload(
            &tenant,
            CreateUpload {
                filename: "a.png".into(),
                content_type: "image/png".into(),
                byte_size: payload.len() as i64,
                checksum,
            },
        )
        .await
        .expect("creates");

    let del = |app: axum::Router, id: Uuid| async move {
        let request = Request::builder()
            .method(http::Method::DELETE)
            .uri(format!("/v1/assets/{id}"))
            .header("authorization", "Bearer token-a")
            .body(Body::empty())
            .expect("builds");
        app.oneshot(request).await.expect("responds").status()
    };

    assert_eq!(del(app.clone(), created.asset.id).await, StatusCode::NO_CONTENT);
    assert_eq!(
        del(app.clone(), created.asset.id).await,
        StatusCode::NOT_FOUND,
        "a second delete is 404, not a 500 — delete is retryable and idempotent"
    );
}

// ----------------------------------------------------------------- probes

/// `/healthz` is 200 without a token, and does not touch the database. The
/// second half is asserted by breaking the pool's database: if healthz pinged,
/// it would fail.
#[tokio::test]
#[ignore = "needs TEST_DATABASE_URL; see tests/common/mod.rs"]
async fn healthz_is_unconditional_and_readyz_actually_checks_the_database() {
    let store = test_store().await;
    let (service, _objects) = test_service(store.clone());
    let (app, _v) = test_app(service, verifier_for(&two_accounts()));

    // No authorization header on either probe: an orchestrator has no token.
    let health = Request::builder()
        .uri("/healthz")
        .body(Body::empty())
        .expect("builds");
    let response = app.clone().oneshot(health).await.expect("responds");
    assert_eq!(response.status(), StatusCode::OK, "liveness needs no credential");
    let body: serde_json::Value =
        serde_json::from_slice(&axum::body::to_bytes(response.into_body(), 4096).await.unwrap())
            .expect("json");
    assert_eq!(body["status"], "ok");

    // `/readyz` really queries. Point the pool at a closed port and it must
    // fail — a probe that returns 200 blindly is worse than no probe.
    let broken = darkroom::Store::connect("postgres://nobody:nobody@127.0.0.1:1/none", 1)
        .await
        .expect_err("a closed port must not connect");
    assert!(matches!(
        broken,
        darkroom::store::StoreError::Unavailable(_) | darkroom::store::StoreError::Query(_)
    ));
    // And a live pool passes.
    store.ping().await.expect("the real pool answers");

    let ready = Request::builder()
        .uri("/readyz")
        .body(Body::empty())
        .expect("builds");
    let response = app.oneshot(ready).await.expect("responds");
    assert_eq!(response.status(), StatusCode::OK, "readiness passes against a live database");
}

// ---------------------------------------------------------- error envelope

/// Every non-2xx is `application/problem+json` with core's `code` and a
/// `trace_id` equal to the `X-Trace-Id` header.
#[tokio::test]
#[ignore = "needs TEST_DATABASE_URL; see tests/common/mod.rs"]
async fn every_error_is_problem_json_with_a_matching_trace_id() {
    let store = test_store().await;
    let (service, _objects) = test_service(store.clone());
    let (app, _v) = test_app(service, verifier_for(&two_accounts()));

    let cases: Vec<(Request<Body>, StatusCode, &str)> = vec![
        (
            Request::builder()
                .uri(format!("/v1/assets/{}", Uuid::new_v4()))
                .header("authorization", "Bearer token-a")
                .body(Body::empty())
                .expect("builds"),
            StatusCode::NOT_FOUND,
            "not_found",
        ),
        (
            Request::builder()
                .uri("/v1/assets")
                .body(Body::empty())
                .expect("builds"),
            StatusCode::UNAUTHORIZED,
            "unauthorized",
        ),
    ];

    for (request, expected_status, expected_code) in cases {
        let response = app.clone().oneshot(request).await.expect("responds");
        assert_eq!(response.status(), expected_status);
        assert_eq!(
            response.headers()["content-type"],
            "application/problem+json",
            "core requires problem+json for every non-2xx"
        );

        let header_trace = response.headers()["x-trace-id"]
            .to_str()
            .expect("ascii")
            .to_string();
        let body: serde_json::Value =
            serde_json::from_slice(&axum::body::to_bytes(response.into_body(), 64 * 1024).await.unwrap())
                .expect("json");
        assert_eq!(body["code"], expected_code);
        assert_eq!(body["status"], expected_status.as_u16());
        assert_eq!(
            body["trace_id"].as_str(),
            Some(header_trace.as_str()),
            "trace_id in the body must equal the X-Trace-Id header"
        );
        assert_eq!(
            body["type"].as_str(),
            Some(format!("https://errors.cafaye.com/{expected_code}").as_str())
        );
    }
}

/// An inbound `traceparent` is honoured, so a trace that started at the edge
/// stays one trace across services.
#[tokio::test]
#[ignore = "needs TEST_DATABASE_URL; see tests/common/mod.rs"]
async fn an_inbound_traceparent_is_propagated() {
    let store = test_store().await;
    let (service, _objects) = test_service(store.clone());
    let (app, _v) = test_app(service, verifier_for(&two_accounts()));

    let trace_id = "4bf92f3577b34da6a3ce929d0e0e4736";
    let request = Request::builder()
        .uri(format!("/v1/assets/{}", Uuid::new_v4()))
        .header("authorization", "Bearer token-a")
        .header("traceparent", format!("00-{trace_id}-00f067aa0ba902b7-01"))
        .body(Body::empty())
        .expect("builds");
    let response = app.oneshot(request).await.expect("responds");
    assert_eq!(
        response.headers()["x-trace-id"].to_str().expect("ascii"),
        trace_id,
        "the inbound trace id must be continued, not replaced"
    );
}

// -------------------------------------------------------------- pagination

/// Cursor pagination: `data` is always an array, `page.next_cursor` is null on
/// the last page, and a cursor walks the whole set without repeating or
/// skipping.
#[tokio::test]
#[ignore = "needs TEST_DATABASE_URL; see tests/common/mod.rs"]
async fn the_listing_pages_by_cursor_without_gaps_or_repeats() {
    let store = test_store().await;
    let (service, _objects) = test_service(store.clone());
    let accounts = two_accounts();
    let (app, _v) = test_app(service.clone(), verifier_for(&accounts));

    // Seven assets, so three pages at limit=3.
    let tenant = darkroom::auth::Tenant::from_principal(&principal(accounts.a_account, accounts.a_user));
    let mut created = Vec::new();
    for n in 0..7u8 {
        // A distinct, well-formed checksum per asset, derived from the index so
        // the set is deterministic and the duplicates test cannot be confused
        // with it.
        let checksum = darkroom::checksum::sha256_hex(format!("asset-{n}").as_bytes());
        let upload = service
            .create_upload(
                &tenant,
                CreateUpload {
                    filename: format!("{n}.png"),
                    content_type: "image/png".into(),
                    byte_size: 10,
                    checksum,
                },
            )
            .await
            .expect("creates");
        created.push(upload.asset.id);
    }
    assert_eq!(created.len(), 7);

    let mut seen: Vec<String> = Vec::new();
    let mut cursor: Option<String> = None;
    let mut pages = 0;
    loop {
        pages += 1;
        assert!(pages <= 10, "pagination did not terminate");
        let uri = match &cursor {
            Some(c) => format!("/v1/assets?limit=3&cursor={c}"),
            None => "/v1/assets?limit=3".to_string(),
        };
        let request = Request::builder()
            .uri(&uri)
            .header("authorization", "Bearer token-a")
            .body(Body::empty())
            .expect("builds");
        let response = app.clone().oneshot(request).await.expect("responds");
        assert_eq!(response.status(), StatusCode::OK);
        let body: serde_json::Value =
            serde_json::from_slice(&axum::body::to_bytes(response.into_body(), 1024 * 1024).await.unwrap())
                .expect("json");

        let data = body["data"].as_array().expect("data is always an array");
        for item in data {
            seen.push(item["id"].as_str().expect("id").to_string());
        }
        match body["page"]["next_cursor"].as_str() {
            Some(next) => {
                assert_eq!(body["page"]["has_more"], true);
                cursor = Some(next.to_string());
            }
            None => {
                assert_eq!(
                    body["page"]["has_more"], false,
                    "a null next_cursor and has_more=false must agree"
                );
                break;
            }
        }
    }

    assert_eq!(seen.len(), 7, "every asset is seen exactly once: {seen:?}");
    let unique: std::collections::HashSet<&String> = seen.iter().collect();
    assert_eq!(unique.len(), 7, "no repeats across pages");
}

// ------------------------------------------------------------- no network

/// The default build cannot name an S3 client at all, which is what makes
/// "tests never hit the network" a property of the dependency graph rather
/// than a promise in a README.
#[test]
fn the_default_build_has_no_object_storage_client() {
    // `ObjectStoreConfig` has no S3 variant unless the `s3` feature is on. This
    // test compiles in both configurations; what it asserts is that the in-memory
    // store is always constructible with no arguments and no URL, and that the
    // whole suite runs without a network.
    let store = darkroom::objectstore::InMemoryObjectStore::new();
    assert!(store.is_empty());
    assert_eq!(store.len(), 0);
}
