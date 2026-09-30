//! `Idempotency-Key` on `POST /v1/uploads` and `POST /v1/uploads/:id/complete`.
//!
//! core/docs/openapi-conventions.md, "Idempotency", in the version that
//! matters: a client that times out and retries the same request must get the
//! same answer and must not create a second asset. These tests drive it over
//! HTTP, because the header is part of the wire contract and testing the
//! helper directly would not prove the handler honours it.

mod common;

use axum::body::Body;
use axum::http::{Request, StatusCode};
use common::*;
use darkroom::service::CreateUpload;
use tower::ServiceExt as _;

fn create_body(checksum: &str) -> String {
    format!(
        r#"{{"filename":"photo.png","content_type":"image/png","byte_size":1234,"checksum":"{checksum}"}}"#
    )
}

fn checksum_of(payload: &bytes::Bytes) -> String {
    darkroom::checksum::sha256_hex(payload)
}

/// A replay of `POST /v1/uploads` with the same key and the same body returns
/// the original response — the same asset id — and creates no second asset.
#[tokio::test]
#[ignore = "needs TEST_DATABASE_URL; see tests/common/mod.rs"]
async fn a_replayed_create_returns_the_original_and_creates_no_second_asset() {
    let store = test_store().await;
    let (service, _objects) = test_service(store.clone());
    let accounts = two_accounts();
    let (app, _v) = test_app(service, verifier_for(&accounts));

    let checksum = "d".repeat(64);
    async fn send(
        app: axum::Router,
        key: &'static str,
        checksum: String,
    ) -> (StatusCode, Option<String>, serde_json::Value) {
        let request = Request::builder()
            .method(http::Method::POST)
            .uri("/v1/uploads")
            .header("authorization", "Bearer token-a")
            .header("idempotency-key", key)
            .header("content-type", "application/json")
            .body(Body::from(create_body(&checksum)))
            .expect("builds");
        let response = app.oneshot(request).await.expect("responds");
        let status = response.status();
        let replayed = response
            .headers()
            .get("idempotency-replayed")
            .and_then(|v| v.to_str().ok())
            .map(str::to_string);
        let body = axum::body::to_bytes(response.into_body(), 1024 * 1024)
            .await
            .expect("body");
        (
            status,
            replayed,
            serde_json::from_slice::<serde_json::Value>(&body).expect("json"),
        )
    }

    let (first_status, first_replayed, first_body) =
        send(app.clone(), "key-1", checksum.clone()).await;
    assert_eq!(first_status, StatusCode::CREATED);
    assert_eq!(first_replayed, None, "a first response is not a replay");

    let (replay_status, replay_replayed, replay_body) =
        send(app.clone(), "key-1", checksum.clone()).await;
    assert_eq!(
        replay_status,
        StatusCode::CREATED,
        "a replay returns the original status"
    );
    assert_eq!(
        replay_replayed.as_deref(),
        Some("true"),
        "a replay is marked with Idempotency-Replayed: true"
    );
    assert_eq!(
        replay_body["asset"]["id"], first_body["asset"]["id"],
        "a replay must return the ORIGINAL asset id"
    );
    assert_eq!(
        replay_body["upload_url"], first_body["upload_url"],
        "and the original presigned URL, so a client that lost the first response can still upload"
    );

    // One asset, not two. The unique constraint would have caught a second row
    // anyway, but the ledger is what makes it a replay rather than a duplicate.
    let count: i64 = sqlx::query_scalar("select count(*) from assets")
        .fetch_one(store.pool())
        .await
        .expect("counts");
    assert_eq!(count, 1, "a replay must not create a second asset");
}

/// The same key with a DIFFERENT body is 409 `idempotency_key_reused`, a
/// distinct code from `conflict` because the client's action differs.
#[tokio::test]
#[ignore = "needs TEST_DATABASE_URL; see tests/common/mod.rs"]
async fn the_same_key_with_a_different_body_is_idempotency_key_reused() {
    let store = test_store().await;
    let (service, _objects) = test_service(store.clone());
    let accounts = two_accounts();
    let (app, _v) = test_app(service, verifier_for(&accounts));

    async fn send(app: axum::Router, checksum: String) -> (StatusCode, serde_json::Value) {
        let request = Request::builder()
            .method(http::Method::POST)
            .uri("/v1/uploads")
            .header("authorization", "Bearer token-a")
            .header("idempotency-key", "key-reuse")
            .header("content-type", "application/json")
            .body(Body::from(create_body(&checksum)))
            .expect("builds");
        let response = app.oneshot(request).await.expect("responds");
        let status = response.status();
        let body = axum::body::to_bytes(response.into_body(), 1024 * 1024)
            .await
            .expect("body");
        (
            status,
            serde_json::from_slice::<serde_json::Value>(&body).expect("json"),
        )
    }

    let (first_status, _) = send(app.clone(), "e".repeat(64)).await;
    assert_eq!(first_status, StatusCode::CREATED);

    let (second_status, second_body) = send(app.clone(), "f".repeat(64)).await;
    assert_eq!(second_status, StatusCode::CONFLICT);
    assert_eq!(
        second_body["code"], "idempotency_key_reused",
        "a distinct code from `conflict`: the client's action is to pick a new key, not to retry"
    );
}

/// The scope is `(endpoint, principal, key)`. One tenant's key must never
/// replay another tenant's response — that would hand a caller data they are
/// not entitled to, which is worse than a failed request.
#[tokio::test]
#[ignore = "needs TEST_DATABASE_URL; see tests/common/mod.rs"]
async fn the_key_is_scoped_to_the_principal() {
    let store = test_store().await;
    let (service, _objects) = test_service(store.clone());
    let accounts = two_accounts();
    let (app, _v) = test_app(service, verifier_for(&accounts));

    async fn send(
        app: axum::Router,
        token: &'static str,
        checksum: String,
    ) -> (StatusCode, serde_json::Value) {
        let request = Request::builder()
            .method(http::Method::POST)
            .uri("/v1/uploads")
            .header("authorization", format!("Bearer {token}"))
            .header("idempotency-key", "shared-key")
            .header("content-type", "application/json")
            .body(Body::from(create_body(&checksum)))
            .expect("builds");
        let response = app.oneshot(request).await.expect("responds");
        let status = response.status();
        let body = axum::body::to_bytes(response.into_body(), 1024 * 1024)
            .await
            .expect("body");
        (
            status,
            serde_json::from_slice::<serde_json::Value>(&body).expect("json"),
        )
    }

    let (a_status, a_body) = send(app.clone(), "token-a", "1".repeat(64)).await;
    assert_eq!(a_status, StatusCode::CREATED);

    // B uses the SAME key on the SAME endpoint with the SAME body. It is a
    // different key as far as the ledger is concerned, so this is a real create
    // — and B's create, not a replay of A's.
    let (b_status, b_body) = send(app.clone(), "token-b", "1".repeat(64)).await;
    assert_eq!(b_status, StatusCode::CREATED);
    assert_ne!(
        b_body["asset"]["id"], a_body["asset"]["id"],
        "B must not have received A's asset from a shared key"
    );
    assert_eq!(
        b_body["asset"]["account_id"],
        accounts.b_account.to_string()
    );

    let count: i64 = sqlx::query_scalar("select count(*) from assets")
        .fetch_one(store.pool())
        .await
        .expect("counts");
    assert_eq!(count, 2, "two accounts, two assets, one shared key");
}

/// A replayed `complete` returns the original response and does not emit a
/// second `darkroom.asset.ready`. The `and status = 'pending'` compare-and-set
/// in `mark_ready` is what makes this hold even without the ledger.
#[tokio::test]
#[ignore = "needs TEST_DATABASE_URL; see tests/common/mod.rs"]
async fn a_replayed_complete_returns_the_original_and_emits_one_event() {
    let store = test_store().await;
    let (service, objects) = test_service(store.clone());
    let accounts = two_accounts();
    let (app, _v) = test_app(service.clone(), verifier_for(&accounts));

    let tenant =
        darkroom::auth::Tenant::from_principal(&principal(accounts.a_account, accounts.a_user));
    let payload = png(28, 28);
    let checksum = checksum_of(&payload);
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

    let send = |app: axum::Router, checksum: String| async move {
        let request = Request::builder()
            .method(http::Method::POST)
            .uri(format!("/v1/uploads/{}/complete", created.asset.id))
            .header("authorization", "Bearer token-a")
            .header("idempotency-key", "complete-1")
            .header("content-type", "application/json")
            .body(Body::from(format!(r#"{{"checksum":"{checksum}"}}"#)))
            .expect("builds");
        let response = app.oneshot(request).await.expect("responds");
        let status = response.status();
        let replayed = response
            .headers()
            .get("idempotency-replayed")
            .and_then(|v| v.to_str().ok())
            .map(str::to_string);
        let body = axum::body::to_bytes(response.into_body(), 1024 * 1024)
            .await
            .expect("body");
        (
            status,
            replayed,
            serde_json::from_slice::<serde_json::Value>(&body).expect("json"),
        )
    };

    let (first_status, first_replayed, first_body) = send(app.clone(), checksum.clone()).await;
    assert_eq!(first_status, StatusCode::OK);
    assert_eq!(first_replayed, None);

    let (replay_status, replay_replayed, replay_body) = send(app.clone(), checksum.clone()).await;
    assert_eq!(replay_status, StatusCode::OK);
    assert_eq!(replay_replayed.as_deref(), Some("true"));
    assert_eq!(replay_body["asset"]["id"], first_body["asset"]["id"]);

    // Exactly one event. A second `darkroom.asset.ready` for the same asset
    // would make a consumer's thumbnail work run twice, and a consumer that
    // trusts `at-least-once` dedupe on the envelope id would see a NEW id.
    let events: i64 = sqlx::query_scalar(
        "select count(*) from outbox_events where event_type = 'darkroom.asset.ready'",
    )
    .fetch_one(store.pool())
    .await
    .expect("counts");
    assert_eq!(
        events, 1,
        "a replayed complete must not emit a second event"
    );
}

/// A request with no key is processed normally — that is a supported state, not
/// an error.
#[tokio::test]
#[ignore = "needs TEST_DATABASE_URL; see tests/main.rs"]
async fn a_request_without_a_key_is_processed_normally() {
    let store = test_store().await;
    let (service, _objects) = test_service(store.clone());
    let accounts = two_accounts();
    let (app, _v) = test_app(service, verifier_for(&accounts));

    let request = Request::builder()
        .method(http::Method::POST)
        .uri("/v1/uploads")
        .header("authorization", "Bearer token-a")
        .header("content-type", "application/json")
        .body(Body::from(create_body(&"9".repeat(64))))
        .expect("builds");
    let response = app.oneshot(request).await.expect("responds");
    assert_eq!(response.status(), StatusCode::CREATED);
}

/// The ledger is tenant data, and it is the only table in this service that
/// stores a whole HTTP **response body**. A replay hands that body back, so a
/// replay that ignored the account would not leak a status code — it would hand
/// account B account A's asset id and A's presigned upload URL.
///
/// ## What is being defended
///
/// The scope is `(endpoint, principal, key)` and `principal` is
/// `user_id:account_id`. The primary key in `0003_idempotency_keys.sql` is what
/// enforces it, so a cross-tenant replay cannot even collide with A's row — it
/// reserves its own and runs the handler. This test drives it over HTTP because
/// the header is part of the wire contract, and because the interesting failure
/// is not "the query is wrong" but "the whole request is answered from a row
/// that is not yours".
#[tokio::test]
#[ignore = "needs TEST_DATABASE_URL; see tests/common/mod.rs"]
async fn one_accounts_key_cannot_replay_another_accounts_response() {
    let store = test_store().await;
    let (service, _objects) = test_service(store.clone());
    let accounts = two_accounts();
    let (app, _v) = test_app(service, verifier_for(&accounts));

    // The identical body for both accounts, deliberately. Same bytes, two
    // accounts, is two assets — `unique (account_id, checksum)` — so the two
    // responses *should* differ, and the only honest way for B's to differ is
    // that B's was not A's.
    let checksum = "c".repeat(64);
    let body = create_body(&checksum);

    async fn send(
        app: axum::Router,
        token: &'static str,
        key: &'static str,
        body: &str,
    ) -> (StatusCode, Option<String>, serde_json::Value) {
        let request = Request::builder()
            .method(http::Method::POST)
            .uri("/v1/uploads")
            .header("authorization", format!("Bearer {token}"))
            .header("idempotency-key", key)
            .header("content-type", "application/json")
            .body(Body::from(body.to_string()))
            .expect("builds");
        let response = app.oneshot(request).await.expect("responds");
        let status = response.status();
        let replayed = response
            .headers()
            .get("idempotency-replayed")
            .and_then(|v| v.to_str().ok())
            .map(str::to_string);
        let bytes = axum::body::to_bytes(response.into_body(), 1024 * 1024)
            .await
            .expect("body");
        (
            status,
            replayed,
            serde_json::from_slice::<serde_json::Value>(&bytes).expect("json"),
        )
    }

    let (a_status, a_replayed, a_body) = send(app.clone(), "token-a", "shared-key", &body).await;
    assert_eq!(a_status, StatusCode::CREATED);
    assert_eq!(a_replayed, None, "a first response is not a replay");
    assert_eq!(
        a_body["asset"]["account_id"],
        accounts.a_account.to_string()
    );

    // B sends the same key and the same body. Not a replay, and emphatically not
    // A's response.
    let (b_status, b_replayed, b_body) = send(app.clone(), "token-b", "shared-key", &body).await;
    assert_eq!(
        b_status,
        StatusCode::CREATED,
        "B's request is a fresh one, not a replay of A's"
    );
    assert_eq!(
        b_replayed, None,
        "B must not be told this is a replay: that header would confirm the key \
         exists in the ledger"
    );
    assert_ne!(
        b_body["asset"]["id"], a_body["asset"]["id"],
        "B received A's asset id"
    );
    assert_ne!(
        b_body["upload_url"], a_body["upload_url"],
        "B received A's presigned URL — a credential for A's storage key"
    );
    assert_eq!(
        b_body["asset"]["account_id"],
        accounts.b_account.to_string()
    );

    // A's own replay still works, so the guard is the account and not a
    // regression that broke replaying for everyone.
    let (again_status, again_replayed, again_body) =
        send(app.clone(), "token-a", "shared-key", &body).await;
    assert_eq!(again_status, StatusCode::CREATED);
    assert_eq!(again_replayed.as_deref(), Some("true"));
    assert_eq!(again_body["asset"]["id"], a_body["asset"]["id"]);

    // Two rows in the ledger, not one shared row and not one overwritten. A
    // single row would mean the scope dropped the account and B's write had
    // clobbered A's.
    let rows: i64 = sqlx::query_scalar("select count(*) from idempotency_keys")
        .fetch_one(store.pool())
        .await
        .expect("counts");
    assert_eq!(rows, 2, "one ledger row per (endpoint, principal, key)");

    // And two assets, one per account. Compared as a **set**, and the reason is
    // worth stating because this exact bug shipped in the first draft of this
    // test and failed on roughly half of all runs.
    //
    // The query says `order by account_id`, and Postgres orders uuid by its raw
    // bytes, so the result is "whichever account id happens to be smaller" —
    // which is A and B in a coin flip. The expectation was written in "A then B"
    // declaration order, so a green run was a run where A's uuid happened to sort
    // first, and a red one read as `left: [a, b] right: [b, a]`: a failure that
    // looks precisely like a cross-tenant leak and is not one.
    //
    // Both sides are sorted before comparison, so the assertion is about *which*
    // accounts hold a row and never about the order two random v4s came out in.
    // The sibling test in `tenant_isolation.rs` carried the same comment and had
    // already learned this; a rule that only one file knows is a rule the next
    // file re-learns by failing.
    let mut accounts_seen: Vec<String> = sqlx::query_scalar("select account_id::text from assets")
        .fetch_all(store.pool())
        .await
        .expect("counts");
    let mut expected = vec![
        accounts.a_account.to_string(),
        accounts.b_account.to_string(),
    ];
    accounts_seen.sort();
    expected.sort();
    assert_eq!(
        accounts_seen, expected,
        "the same bytes are one asset per account, and neither account can see \
         the other's"
    );
}
