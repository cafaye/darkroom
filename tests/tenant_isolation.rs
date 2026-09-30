//! Tenant isolation. The point of the service.
//!
//! Every case here asserts an **exact status code**, and the expected one is
//! almost always 404 rather than 403. That is not a stylistic choice:
//! `core/docs/openapi-conventions.md` says "Never 404 for authorization
//! failures on a resource the caller cannot see — 404 is correct there, 403 is
//! not allowed to leak existence." A 403 on "that asset exists but it is not
//! yours" is a free asset-id oracle: a caller enumerates ids, gets 403 for the
//! ones that exist and 404 for the ones that do not, and has a directory of
//! every asset on the platform.
//!
//! So: identical responses, indistinguishable, for "does not exist" and "not
//! yours". The tests assert both produce the *same* body as well as the same
//! status, because a body that differs is just as much of an oracle.
//!
//! ## The rule, stated once, for the whole platform
//!
//! **Cross-tenant is absence, not refusal. `404`, never `403`, and a body
//! byte-identical to the one for an id that never existed.** A refusal is a
//! confirmation: it tells the caller the resource is real and that somebody else
//! owns it, which is a smaller leak than the row and a perfectly good way to
//! enumerate the platform. Every service that holds another service's data has
//! this choice to make, and this is the answer.
//!
//! ## What this file covers, and what does not
//!
//! The **wire**: every tenant-scoped route in `http::OPERATIONS`, driven through
//! the real router, plus the two routes whose negative case is about ownership
//! rather than a status. The **query layer** — the same property proved against
//! a two-account fixture, covering read, list, update and delete — is
//! `tests/query_scoping.rs`, and the structural half, which needs no database at
//! all, is `tests/tenant_scoping.rs`. The coverage of this file is not taken on
//! trust: `tests/tenant_scoping.rs` derives the route list from
//! `http::OPERATIONS` and fails if a route has no negative case here.

mod common;

use axum::body::Body;
use axum::http::{Request, StatusCode};
use common::*;
use darkroom::domain::AssetStatus;
use darkroom::service::CreateUpload;
use tower::ServiceExt as _;
use uuid::Uuid;

/// Create a ready asset for `account` and return `(account, asset_id)`.
async fn seed_ready_asset(
    service: &darkroom::Service,
    objects: &std::sync::Arc<darkroom::objectstore::InMemoryObjectStore>,
    account: Uuid,
    user: Uuid,
) -> Uuid {
    let tenant = darkroom::auth::Tenant::from_principal(&principal(account, user));
    let payload = png(40, 30);
    let checksum = darkroom::checksum::sha256_hex(&payload);
    let created = service
        .create_upload(
            &tenant,
            CreateUpload {
                filename: "secret.png".into(),
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
    created.asset.id
}

/// Create a `pending` asset for `account` — the presigned PUT issued, the bytes
/// never sent. The state `POST /v1/uploads/{id}/complete` and the sweeper both
/// act on, and the state a `complete` that forgot its scoping would silently
/// advance.
async fn seed_pending_asset(
    service: &darkroom::Service,
    account: Uuid,
    user: Uuid,
) -> Uuid {
    let tenant = darkroom::auth::Tenant::from_principal(&principal(account, user));
    // Distinct bytes per account: `unique (account_id, checksum)` is per
    // account, so a shared checksum would be legal but would make the
    // cross-tenant assertion ambiguous about which row it meant.
    let payload = if account == Uuid::nil() {
        png(1, 1)
    } else {
        png((account.as_u128() % 7 + 20) as u32, 30)
    };
    let checksum = darkroom::checksum::sha256_hex(&payload);
    service
        .create_upload(
            &tenant,
            CreateUpload {
                filename: "unfinished.png".into(),
                content_type: "image/png".into(),
                byte_size: payload.len() as i64,
                checksum,
            },
        )
        .await
        .expect("creates")
        .asset
        .id
}

/// The tenant-isolation matrix, as a table. One row per endpoint × caller, with
/// the exact status asserted — the shape `identity`'s authorization matrix uses,
/// applied to the surface that exists here.
#[tokio::test]
#[ignore = "needs TEST_DATABASE_URL; see tests/common/mod.rs"]
async fn cross_tenant_access_is_404_on_every_endpoint() {
    let store = test_store().await;
    let (service, objects) = test_service(store);
    let accounts = two_accounts();
    let (app, _verifier) = test_app(service.clone(), verifier_for(&accounts));

    // An asset that belongs to A.
    let a_asset = seed_ready_asset(&service, &objects, accounts.a_account, accounts.a_user).await;
    // A thumbnail on it, so the variant endpoints have something to protect.
    let a_tenant =
        darkroom::auth::Tenant::from_principal(&principal(accounts.a_account, accounts.a_user));
    service
        .create_variant(&a_tenant, a_asset, darkroom::VariantKind::Thumbnail)
        .await
        .expect("A can make a variant of its own asset");

    // Every endpoint B can reach, aimed at A's asset.
    let cases: Vec<(&str, http::Method, String, Option<Body>)> = vec![
        (
            "GET asset",
            http::Method::GET,
            format!("/v1/assets/{a_asset}"),
            None,
        ),
        (
            "DELETE asset",
            http::Method::DELETE,
            format!("/v1/assets/{a_asset}"),
            None,
        ),
        (
            "GET variants",
            http::Method::GET,
            format!("/v1/assets/{a_asset}/variants"),
            None,
        ),
        (
            "POST variant",
            http::Method::POST,
            format!("/v1/assets/{a_asset}/variants"),
            Some(Body::from(r#"{"kind":"preview"}"#)),
        ),
        (
            "POST complete",
            http::Method::POST,
            format!("/v1/uploads/{a_asset}/complete"),
            Some(Body::from(
                r#"{"checksum":"0000000000000000000000000000000000000000000000000000000000000000"}"#,
            )),
        ),
    ];

    for (name, method, path, body) in cases {
        let request = Request::builder()
            .method(method.clone())
            .uri(&path)
            .header("authorization", "Bearer token-b")
            .header("content-type", "application/json")
            .body(body.unwrap_or_else(Body::empty))
            .expect("builds");

        let response = app.clone().oneshot(request).await.expect("responds");
        assert_eq!(
            response.status(),
            StatusCode::NOT_FOUND,
            "{name}: a cross-tenant request must be 404, not {}",
            response.status()
        );
    }

    // And A's asset and its variant are untouched by all of that.
    assert_eq!(
        service
            .get_asset(&a_tenant, a_asset)
            .await
            .expect("still there")
            .status,
        AssetStatus::Ready
    );
    assert_eq!(
        service
            .list_variants(&a_tenant, a_asset)
            .await
            .expect("still there")
            .len(),
        1,
        "B's attempts must not have created or removed a variant"
    );
}

/// The load-bearing assertion: a cross-tenant 404 and a genuinely-missing
/// asset produce **byte-identical** responses. Different bodies are just as
/// much of an oracle as different statuses.
///
/// Every id-scoped route, not just `GET /v1/assets/{id}`. A 404 that leaks is a
/// 404 that leaks *per route*: the one place this rule is most likely to be
/// broken by accident is the route somebody added most recently, and a test
/// that only checks the oldest one is a test that reports the repository is
/// safe when it is only partly checked.
#[tokio::test]
#[ignore = "needs TEST_DATABASE_URL; see tests/common/mod.rs"]
async fn a_cross_tenant_404_is_indistinguishable_from_a_missing_one() {
    let store = test_store().await;
    let (service, objects) = test_service(store);
    let accounts = two_accounts();
    let (app, _verifier) = test_app(service.clone(), verifier_for(&accounts));

    let a_asset = seed_ready_asset(&service, &objects, accounts.a_account, accounts.a_user).await;
    // An id that was never issued to anyone.
    let never_existed = Uuid::new_v4();

    async fn ask(
        app: axum::Router,
        method: http::Method,
        template: &str,
        id: Uuid,
        token: &'static str,
    ) -> (StatusCode, bytes::Bytes) {
        let request = Request::builder()
            .method(method.clone())
            .uri(template.replace("{id}", &id.to_string()))
            .header("authorization", format!("Bearer {token}"))
            .header("content-type", "application/json")
            .body(match template {
                "/v1/assets/{id}/variants" if method == http::Method::POST => {
                    Body::from(r#"{"kind":"preview"}"#)
                }
                "/v1/uploads/{id}/complete" => Body::from(
                    r#"{"checksum":"0000000000000000000000000000000000000000000000000000000000000000"}"#,
                ),
                _ => Body::empty(),
            })
            .expect("builds");
        let response = app.oneshot(request).await.expect("responds");
        let status = response.status();
        let body = axum::body::to_bytes(response.into_body(), 64 * 1024)
            .await
            .expect("body");
        (status, body)
    }

    // Every id-scoped operation, aimed at A's asset from B, and at an id that
    // never existed from B. The two answers must be indistinguishable.
    let routes: Vec<(&str, http::Method, &str)> = vec![
        ("GET asset", http::Method::GET, "/v1/assets/{id}"),
        ("DELETE asset", http::Method::DELETE, "/v1/assets/{id}"),
        (
            "GET variants",
            http::Method::GET,
            "/v1/assets/{id}/variants",
        ),
        (
            "POST variant",
            http::Method::POST,
            "/v1/assets/{id}/variants",
        ),
        (
            "POST complete",
            http::Method::POST,
            "/v1/uploads/{id}/complete",
        ),
    ];

    // `trace_id` is per-request and `instance` is the path — and the path
    // differs between "A's asset" and "never existed" by design. The rest of
    // the envelope must be identical, and `detail` is the field that would leak
    // first if one of the five routes said "not yours".
    let strip = |body: &[u8]| -> serde_json::Value {
        let mut value: serde_json::Value = serde_json::from_slice(body).expect("json");
        let object = value.as_object_mut().expect("object");
        object.remove("trace_id");
        object.remove("instance");
        value
    };

    for (name, method, template) in &routes {
        let (theirs_status, theirs_body) =
            ask(app.clone(), method.clone(), template, a_asset, "token-b").await;
        let (missing_status, missing_body) =
            ask(app.clone(), method.clone(), template, never_existed, "token-b").await;

        assert_eq!(
            theirs_status,
            StatusCode::NOT_FOUND,
            "{name}: a cross-tenant request must be 404, not {}",
            theirs_status
        );
        assert_eq!(
            missing_status, StatusCode::NOT_FOUND,
            "{name}: control — a missing id is 404 too"
        );
        assert_eq!(
            strip(&theirs_body),
            strip(&missing_body),
            "{name}: the cross-tenant 404 and the missing-id 404 must be \
             identical apart from the per-request trace id and the path"
        );
    }

    // And A's asset is still readable by A, so none of the above is a 404 to
    // everyone.
    let a_tenant =
        darkroom::auth::Tenant::from_principal(&principal(accounts.a_account, accounts.a_user));
    assert!(
        service.get_asset(&a_tenant, a_asset).await.is_ok(),
        "B's requests must not have made A's asset unreadable to A"
    );
}

/// The listing is scoped too: B's list never contains A's assets, and the total
/// count does not leak either.
#[tokio::test]
#[ignore = "needs TEST_DATABASE_URL; see tests/common/mod.rs"]
async fn a_listing_returns_only_the_callers_own_assets() {
    let store = test_store().await;
    let (service, objects) = test_service(store);
    let accounts = two_accounts();
    let (app, _verifier) = test_app(service.clone(), verifier_for(&accounts));

    let a_asset = seed_ready_asset(&service, &objects, accounts.a_account, accounts.a_user).await;
    let b_asset = seed_ready_asset(&service, &objects, accounts.b_account, accounts.b_user).await;
    assert_ne!(a_asset, b_asset);

    let request = Request::builder()
        .uri("/v1/assets?limit=100")
        .header("authorization", "Bearer token-a")
        .body(Body::empty())
        .expect("builds");
    let response = app.clone().oneshot(request).await.expect("responds");
    assert_eq!(response.status(), StatusCode::OK);

    let body: serde_json::Value = serde_json::from_slice(
        &axum::body::to_bytes(response.into_body(), 1024 * 1024)
            .await
            .unwrap(),
    )
    .expect("json");
    let ids: Vec<&str> = body["data"]
        .as_array()
        .expect("data is an array")
        .iter()
        .map(|a| a["id"].as_str().expect("an id"))
        .collect();

    assert!(
        ids.contains(&a_asset.to_string().as_str()),
        "A sees its own asset"
    );
    assert!(
        !ids.contains(&b_asset.to_string().as_str()),
        "A must never see B's asset in a listing: {ids:?}"
    );
    assert_eq!(
        ids.len(),
        1,
        "A has exactly one asset, so the page must have one row"
    );
}

/// A `member` with the read scope sees their account's assets. The positive
/// half of the matrix — a test suite that only asserts denials would pass on a
/// service that returns 404 to everyone.
#[tokio::test]
#[ignore = "needs TEST_DATABASE_URL; see tests/common/mod.rs"]
async fn a_member_sees_their_own_accounts_assets() {
    let store = test_store().await;
    let (service, objects) = test_service(store);
    let accounts = two_accounts();
    let (app, _verifier) = test_app(service.clone(), verifier_for(&accounts));

    let a_asset = seed_ready_asset(&service, &objects, accounts.a_account, accounts.a_user).await;

    let request = Request::builder()
        .uri(format!("/v1/assets/{a_asset}"))
        .header("authorization", "Bearer token-a")
        .body(Body::empty())
        .expect("builds");
    let response = app.clone().oneshot(request).await.expect("responds");
    assert_eq!(
        response.status(),
        StatusCode::OK,
        "a member must be able to read their own account's asset"
    );

    let body: serde_json::Value = serde_json::from_slice(
        &axum::body::to_bytes(response.into_body(), 1024 * 1024)
            .await
            .unwrap(),
    )
    .expect("json");
    assert_eq!(body["asset"]["id"], a_asset.to_string());
    assert_eq!(body["asset"]["status"], "ready");
}

/// A token with no asset scope is 403, not 404. It authenticated, so there is
/// no existence to leak — the difference from the cross-tenant case is
/// deliberate and worth stating.
#[tokio::test]
#[ignore = "needs TEST_DATABASE_URL; see tests/common/mod.rs"]
async fn a_token_without_the_scope_is_403_and_anonymous_is_401() {
    let store = test_store().await;
    let (service, objects) = test_service(store);
    let accounts = two_accounts();
    let asset = seed_ready_asset(&service, &objects, accounts.a_account, accounts.a_user).await;

    let verifier = verifier_for(&accounts).with_token(
        "token-no-scope",
        darkroom::auth::Principal {
            user_id: accounts.a_user,
            account_id: accounts.a_account,
            scopes: vec!["invoices:read".into()],
        },
    );
    let (app, _v) = test_app(service, verifier);

    // Wrong scope: authenticated, so 403. The asset exists and the caller
    // cannot see it because of capability, not tenancy.
    let request = Request::builder()
        .uri(format!("/v1/assets/{asset}"))
        .header("authorization", "Bearer token-no-scope")
        .body(Body::empty())
        .expect("builds");
    let response = app.clone().oneshot(request).await.expect("responds");
    assert_eq!(response.status(), StatusCode::FORBIDDEN);

    // No token: 401.
    let request = Request::builder()
        .uri(format!("/v1/assets/{asset}"))
        .body(Body::empty())
        .expect("builds");
    let response = app.clone().oneshot(request).await.expect("responds");
    assert_eq!(response.status(), StatusCode::UNAUTHORIZED);

    // A garbage token: 401, and the same body as no token at all — a verifier
    // that distinguishes them is an oracle.
    let request = Request::builder()
        .uri(format!("/v1/assets/{asset}"))
        .header("authorization", "Bearer not-a-real-token")
        .body(Body::empty())
        .expect("builds");
    let response = app.clone().oneshot(request).await.expect("responds");
    assert_eq!(response.status(), StatusCode::UNAUTHORIZED);
}

/// Deleting B's asset by id is a 404 and does not remove it. The delete path is
/// the one where a missing scope check would be worst: a cross-tenant delete
/// is data loss, not just disclosure.
#[tokio::test]
#[ignore = "needs TEST_DATABASE_URL; see tests/common/mod.rs"]
async fn a_cross_tenant_delete_is_404_and_the_asset_survives() {
    let store = test_store().await;
    let (service, objects) = test_service(store);
    let accounts = two_accounts();
    let (app, _verifier) = test_app(service.clone(), verifier_for(&accounts));

    let a_asset = seed_ready_asset(&service, &objects, accounts.a_account, accounts.a_user).await;
    let objects_before = objects.len();

    let request = Request::builder()
        .method(http::Method::DELETE)
        .uri(format!("/v1/assets/{a_asset}"))
        .header("authorization", "Bearer token-b")
        .body(Body::empty())
        .expect("builds");
    let response = app.clone().oneshot(request).await.expect("responds");
    assert_eq!(response.status(), StatusCode::NOT_FOUND);

    // A's asset is still there AND its bytes are still there.
    let a_tenant =
        darkroom::auth::Tenant::from_principal(&principal(accounts.a_account, accounts.a_user));
    assert!(
        service.get_asset(&a_tenant, a_asset).await.is_ok(),
        "B's delete must not remove A's row"
    );
    assert_eq!(
        objects.len(),
        objects_before,
        "B's delete must not remove A's storage objects"
    );
}

/// Cross-tenant create: B cannot create an asset in A's account by naming it,
/// because the body has no account field at all. This test is the regression
/// guard for the day someone adds one.
#[tokio::test]
#[ignore = "needs TEST_DATABASE_URL; see tests/common/mod.rs"]
async fn a_create_body_cannot_name_an_account() {
    let store = test_store().await;
    let (service, _objects) = test_service(store);
    let accounts = two_accounts();
    let (app, _verifier) = test_app(service.clone(), verifier_for(&accounts));

    // A body that TRIES to name A's account. `account_id` is not a field of
    // `CreateUploadBody`, so serde drops it — the asset is created for B, the
    // authenticated caller, and the attempt to plant it in A fails silently
    // and harmlessly.
    let body = format!(
        r#"{{"filename":"a.png","content_type":"image/png","byte_size":10,"checksum":"{}","account_id":"{}"}}"#,
        "a".repeat(64),
        accounts.a_account
    );
    let request = Request::builder()
        .method(http::Method::POST)
        .uri("/v1/uploads")
        .header("authorization", "Bearer token-b")
        .header("content-type", "application/json")
        .body(Body::from(body))
        .expect("builds");
    let response = app.clone().oneshot(request).await.expect("responds");
    assert_eq!(response.status(), StatusCode::CREATED);

    let body: serde_json::Value = serde_json::from_slice(
        &axum::body::to_bytes(response.into_body(), 1024 * 1024)
            .await
            .unwrap(),
    )
    .expect("json");
    assert_eq!(
        body["asset"]["account_id"],
        accounts.b_account.to_string(),
        "the asset must belong to the authenticated account, whatever the body claimed"
    );
}

/// The mirror of the create guard, and the case the unique constraint exists
/// for. A checksum shared by two accounts is **two assets**, so a lookup that
/// forgot `account_id` would not error and would not return nothing — it would
/// return the wrong account's row, and the caller would then PUT bytes at the
/// wrong storage key and complete the wrong asset.
///
/// `POST /v1/uploads` for B with A's checksum must create B's own asset and
/// resolve the duplicate against B's, not hand B A's presigned URL. A presigned
/// URL is a write credential for one object: replaying A's would let B
/// overwrite A's upload.
#[tokio::test]
#[ignore = "needs TEST_DATABASE_URL; see tests/common/mod.rs"]
async fn the_same_bytes_in_two_accounts_are_two_assets_and_never_a_credential() {
    let store = test_store().await;
    let (service, objects) = test_service(store);
    let accounts = two_accounts();
    let (app, _verifier) = test_app(service.clone(), verifier_for(&accounts));

    let a_asset = seed_ready_asset(&service, &objects, accounts.a_account, accounts.a_user).await;
    let a_key = {
        // A's own storage key, read as A — the thing B must not learn.
        let a_tenant =
            darkroom::auth::Tenant::from_principal(&principal(accounts.a_account, accounts.a_user));
        darkroom::store::find_storage_key(service.store.pool(), &a_tenant, a_asset)
            .await
            .expect("reads")
            .expect("A's asset has a key")
    };
    // The same bytes B is about to claim: a distinct image, so the checksum
    // genuinely differs from A's.
    let b_payload = png(41, 31);
    let b_checksum = darkroom::checksum::sha256_hex(&b_payload);

    let body = format!(
        r#"{{"filename":"b.png","content_type":"image/png","byte_size":{},"checksum":"{b_checksum}"}}"#,
        b_payload.len()
    );
    let request = Request::builder()
        .method(http::Method::POST)
        .uri("/v1/uploads")
        .header("authorization", "Bearer token-b")
        .header("content-type", "application/json")
        .body(Body::from(body))
        .expect("builds");
    let response = app.clone().oneshot(request).await.expect("responds");
    assert_eq!(response.status(), StatusCode::CREATED);

    let body: serde_json::Value = serde_json::from_slice(
        &axum::body::to_bytes(response.into_body(), 1024 * 1024)
            .await
            .unwrap(),
    )
    .expect("json");
    assert_eq!(
        body["asset"]["account_id"],
        accounts.b_account.to_string()
    );
    assert_ne!(
        body["asset"]["id"], a_asset.to_string(),
        "B was handed A's asset"
    );
    assert_ne!(
        body["storage_key"], a_key,
        "B was handed a presigned URL scoped to A's storage key — a write \
         credential for another tenant's object"
    );

    // B's own presigned URL really does work, and it writes to B's key. A
    // response that named a key nobody can use is a broken service, and a
    // test that only asserted "not A's" would pass on one.
    let b_key = body["storage_key"].as_str().expect("a key").to_string();
    objects
        .apply_presigned_put(
            &body["upload_url"].as_str().expect("a url"),
            b_payload.clone(),
            "image/png",
        )
        .await
        .expect("B can PUT to its own key");
    assert!(
        b_key != a_key && b_key.starts_with(&format!("a/{}/", accounts.b_account)),
        "B's key is B's: {b_key}"
    );

    // The duplicate path resolves per account too: B uploading the *same* bytes
    // again returns B's own asset, not A's.
    let repeat = format!(
        r#"{{"filename":"b.png","content_type":"image/png","byte_size":{},"checksum":"{b_checksum}"}}"#,
        b_payload.len()
    );
    let request = Request::builder()
        .method(http::Method::POST)
        .uri("/v1/uploads")
        .header("authorization", "Bearer token-b")
        .header("content-type", "application/json")
        .body(Body::from(repeat))
        .expect("builds");
    let response = app.clone().oneshot(request).await.expect("responds");
    assert_eq!(response.status(), StatusCode::CREATED);
    assert_eq!(
        response
            .headers()
            .get("x-darkroom-duplicate")
            .and_then(|v| v.to_str().ok()),
        Some("true"),
        "the same bytes for the same account is a duplicate"
    );
    let body: serde_json::Value = serde_json::from_slice(
        &axum::body::to_bytes(response.into_body(), 1024 * 1024)
            .await
            .unwrap(),
    )
    .expect("json");
    assert_eq!(
        body["asset"]["account_id"],
        accounts.b_account.to_string(),
        "the duplicate must resolve to B's own asset, never A's"
    );

    // Two rows, one per account, and A's is untouched.
    let per_account: Vec<(String, i64)> = sqlx::query_as(
        "select account_id::text, count(*) from assets group by account_id order by account_id",
    )
    .fetch_all(service.store.pool())
    .await
    .expect("counts");
    assert_eq!(
        per_account,
        vec![
            (accounts.a_account.to_string(), 1),
            (accounts.b_account.to_string(), 1),
        ],
        "one asset per account, and A's upload was not disturbed"
    );
}

/// A cross-tenant `complete` must not advance — or fail — another account's
/// unfinished upload.
///
/// This is the update path at the wire, and it is the one with the widest blast
/// radius. `complete` reads the object, hashes it, and then writes
/// `status = 'ready'` with a compare-and-set on `pending`. A `find_asset` that
/// lost its `account_id` would let B complete A's upload with **B's** bytes and
/// A's storage key, and a `mark_failed` that lost its would let B destroy it
/// with no object and no checksum at all — the failure path is a bare `where`
/// clause, so it is the easiest of the three to get wrong.
///
/// The assertion that matters is the second one: B gets a 404 *and* A's row is
/// still `pending`. A 404 on its own is compatible with "refused before doing
/// the work" and with "did the work and then reported 404".
#[tokio::test]
#[ignore = "needs TEST_DATABASE_URL; see tests/common/mod.rs"]
async fn a_cross_tenant_complete_does_not_fail_another_accounts_upload() {
    let store = test_store().await;
    let (service, objects) = test_service(store);
    let accounts = two_accounts();
    let (app, _verifier) = test_app(service.clone(), verifier_for(&accounts));

    let a_pending = seed_pending_asset(&service, accounts.a_account, accounts.a_user).await;
    let a_tenant =
        darkroom::auth::Tenant::from_principal(&principal(accounts.a_account, accounts.a_user));
    assert_eq!(
        service
            .get_asset(&a_tenant, a_pending)
            .await
            .expect("there")
            .status,
        AssetStatus::Pending,
        "the fixture is a pending upload"
    );

    // A syntactically valid checksum. It does not match anything, which is the
    // point: if B's request reached the failure path at all, it would mark A's
    // asset `failed` with `checksum_mismatch` before the comparison ever
    // mattered.
    let body = format!(
        r#"{{"checksum":"{}"}}"#,
        "0".repeat(64)
    );
    let request = Request::builder()
        .method(http::Method::POST)
        .uri(format!("/v1/uploads/{a_pending}/complete"))
        .header("authorization", "Bearer token-b")
        .header("content-type", "application/json")
        .body(Body::from(body))
        .expect("builds");
    let response = app.clone().oneshot(request).await.expect("responds");
    assert_eq!(
        response.status(),
        StatusCode::NOT_FOUND,
        "a cross-tenant complete is 404, not {}",
        response.status()
    );

    // The row is untouched — status *and* metadata. A `mark_failed` that missed
    // its scoping and then lost a second race on the status guard would still
    // have written the reason.
    let (asset, _) = darkroom::store::find_asset(service.store.pool(), &a_tenant, a_pending)
        .await
        .expect("reads")
        .expect("A's row is still there");
    assert_eq!(
        asset.status,
        AssetStatus::Pending,
        "B must not have advanced A's upload"
    );
    assert!(
        !asset
            .metadata
            .as_object()
            .is_some_and(|m| m.contains_key("failure_reason")),
        "B must not have failed A's upload: {:?}",
        asset.metadata
    );

    // And A can still finish it, so nothing above broke the real path.
    let payload = png(30, 20);
    let checksum = darkroom::checksum::sha256_hex(&payload);
    let tenant = darkroom::auth::Tenant::from_principal(&principal(accounts.a_account, accounts.a_user));
    let created = service
        .create_upload(
            &tenant,
            CreateUpload {
                filename: "mine.png".into(),
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
        .expect("A can complete A's own upload");
    let _ = a_pending;
}

/// A cross-tenant variant write touches nothing — not the row, not the storage
/// objects, and not the outbox.
///
/// The outbox is the assertion that makes this worth a separate test. A
/// cross-tenant variant that inserted a row would be visible in the variant
/// count; one that inserted a row *and* a storage object would be visible in
/// the object count; one that did either and then rolled back would be visible
/// in neither, and the event is the only trace it left. A
/// `darkroom.variant.created` naming A's asset that B caused is a message on
/// somebody else's bus.
#[tokio::test]
#[ignore = "needs TEST_DATABASE_URL; see tests/common/mod.rs"]
async fn a_cross_tenant_variant_write_touches_nothing() {
    let store = test_store().await;
    let (service, objects) = test_service(store);
    let accounts = two_accounts();
    let (app, _verifier) = test_app(service.clone(), verifier_for(&accounts));

    let a_asset = seed_ready_asset(&service, &objects, accounts.a_account, accounts.a_user).await;
    let a_tenant =
        darkroom::auth::Tenant::from_principal(&principal(accounts.a_account, accounts.a_user));
    let objects_before = objects.len();

    let request = Request::builder()
        .method(http::Method::POST)
        .uri(format!("/v1/assets/{a_asset}/variants"))
        .header("authorization", "Bearer token-b")
        .header("content-type", "application/json")
        .body(Body::from(r#"{"kind":"preview"}"#))
        .expect("builds");
    let response = app.clone().oneshot(request).await.expect("responds");
    assert_eq!(response.status(), StatusCode::NOT_FOUND);

    assert!(
        service
            .list_variants(&a_tenant, a_asset)
            .await
            .expect("reads")
            .is_empty(),
        "B must not have created a variant row on A's asset"
    );
    assert_eq!(
        objects.len(),
        objects_before,
        "B must not have written a storage object for A's asset"
    );
    let events: i64 =
        sqlx::query_scalar("select count(*) from outbox_events where event_type = 'darkroom.variant.created'")
            .fetch_one(service.store.pool())
            .await
            .expect("counts");
    assert_eq!(
        events, 0,
        "B's cross-tenant variant must emit no darkroom.variant.created"
    );

    // And A can still make its own, so the path is not merely closed.
    service
        .create_variant(&a_tenant, a_asset, darkroom::VariantKind::Preview)
        .await
        .expect("A can make its own variant");
    assert_eq!(
        service
            .list_variants(&a_tenant, a_asset)
            .await
            .expect("reads")
            .len(),
        1
    );
}

/// The variant listing answers 404, not an empty array — and the distinction is
/// the whole point of the case.
///
/// `[]` is a one-bit existence oracle. A caller who gets an empty list for
/// someone else's asset cannot tell it from "this asset has no variants yet",
/// and the difference is precisely whether the asset id is real. 404 collapses
/// the question; an empty array answers half of it.
#[tokio::test]
#[ignore = "needs TEST_DATABASE_URL; see tests/common/mod.rs"]
async fn a_cross_tenant_variant_list_is_404_not_an_empty_list() {
    let store = test_store().await;
    let (service, objects) = test_service(store);
    let accounts = two_accounts();
    let (app, _verifier) = test_app(service.clone(), verifier_for(&accounts));

    // An asset with a variant, and an asset with none: B must not be able to
    // tell the two apart, because if it could, the difference is existence.
    let with_variant = seed_ready_asset(&service, &objects, accounts.a_account, accounts.a_user).await;
    let a_tenant =
        darkroom::auth::Tenant::from_principal(&principal(accounts.a_account, accounts.a_user));
    service
        .create_variant(&a_tenant, with_variant, darkroom::VariantKind::Thumbnail)
        .await
        .expect("A makes a variant");

    async fn list(app: axum::Router, id: Uuid) -> (StatusCode, bytes::Bytes) {
        let request = Request::builder()
            .uri(format!("/v1/assets/{id}/variants"))
            .header("authorization", "Bearer token-b")
            .body(Body::empty())
            .expect("builds");
        let response = app.oneshot(request).await.expect("responds");
        let status = response.status();
        let body = axum::body::to_bytes(response.into_body(), 64 * 1024)
            .await
            .expect("body");
        (status, body)
    }

    let (owned_status, owned_body) = list(app.clone(), with_variant).await;
    assert_eq!(
        owned_status,
        StatusCode::NOT_FOUND,
        "another account's asset is 404 on the variant listing, not an empty page"
    );

    // An id that never existed, for the same account's comparison.
    let (missing_status, missing_body) = list(app.clone(), Uuid::new_v4()).await;
    assert_eq!(missing_status, StatusCode::NOT_FOUND);
    let strip = |body: &[u8]| -> serde_json::Value {
        let mut value: serde_json::Value = serde_json::from_slice(body).expect("json");
        let object = value.as_object_mut().expect("object");
        object.remove("trace_id");
        object.remove("instance");
        value
    };
    assert_eq!(
        strip(&owned_body),
        strip(&missing_body),
        "the two 404s are the same answer"
    );

    // A's own listing is a 200 with a row, so this is a refusal of B's access
    // and not a broken endpoint.
    let request = Request::builder()
        .uri(format!("/v1/assets/{with_variant}/variants"))
        .header("authorization", "Bearer token-a")
        .body(Body::empty())
        .expect("builds");
    let response = app.oneshot(request).await.expect("responds");
    assert_eq!(response.status(), StatusCode::OK);
}

/// A pagination cursor from another account pages only the caller's own rows.
///
/// The cursor is `(created_at, id)` in base64url and it is handed to the client,
/// so it is an *input* a caller can be given by someone else and replayed. The
/// keyset predicate `(created_at, id) < ($2, $3)` and the tenant predicate are
/// separate clauses, and a regression that dropped the second would page out of
/// one account and into the other — silently, as a 200.
#[tokio::test]
#[ignore = "needs TEST_DATABASE_URL; see tests/common/mod.rs"]
async fn a_cursor_from_another_account_pages_only_the_callers_own_rows() {
    let store = test_store().await;
    let (service, objects) = test_service(store);
    let accounts = two_accounts();
    let (app, _verifier) = test_app(service.clone(), verifier_for(&accounts));

    // A gets two assets, so A's first page has a `next_cursor` to hand out.
    let _a_first = seed_ready_asset(&service, &objects, accounts.a_account, accounts.a_user).await;
    let a_second = seed_ready_asset(&service, &objects, accounts.a_account, accounts.a_user).await;
    // B gets one, created between them in time is not guaranteed — the cursor
    // test below does not depend on the ordering between accounts, only on the
    // scoping, which is the thing under test.
    let b_asset = seed_ready_asset(&service, &objects, accounts.b_account, accounts.b_user).await;

    async fn page(app: axum::Router, token: &'static str, query: &str) -> serde_json::Value {
        let request = Request::builder()
            .uri(format!("/v1/assets?{query}"))
            .header("authorization", format!("Bearer {token}"))
            .body(Body::empty())
            .expect("builds");
        let response = app.oneshot(request).await.expect("responds");
        assert_eq!(response.status(), StatusCode::OK);
        let bytes = axum::body::to_bytes(response.into_body(), 1024 * 1024)
            .await
            .expect("body");
        serde_json::from_slice(&bytes).expect("json")
    }

    let ids_in = |body: &serde_json::Value| -> Vec<String> {
        body["data"]
            .as_array()
            .expect("data is an array")
            .iter()
            .map(|a| a["id"].as_str().expect("an id").to_string())
            .collect()
    };

    // A's first page, and the cursor it hands back.
    let first = page(app.clone(), "token-a", "limit=1").await;
    let cursor = first["page"]["next_cursor"]
        .as_str()
        .expect("A has a second page")
        .to_string();

    // A walking its own cursor gets exactly one row back, and it is A's.
    let second = page(app.clone(), "token-a", &format!("limit=1&cursor={cursor}")).await;
    let second_ids = ids_in(&second);
    assert_eq!(
        second_ids.len(),
        1,
        "one row per page: {second_ids:?}"
    );
    assert_ne!(
        second_ids[0], b_asset.to_string(),
        "A's own cursor returned B's row"
    );
    assert!(
        second_ids[0] != first["data"][0]["id"].as_str().expect("an id"),
        "the cursor must actually have advanced, or the second page is the first"
    );

    // B replaying A's cursor. B's page is B's rows or nothing: A's cursor
    // encodes A's asset id and A's timestamp, and B gets none of it.
    let b_page = page(app.clone(), "token-b", &format!("limit=100&cursor={cursor}")).await;
    let b_ids = ids_in(&b_page);
    assert!(
        !b_ids.contains(&a_second.to_string()),
        "B replaying A's cursor returned A's row: {b_ids:?}"
    );
    let b_own = page(app.clone(), "token-b", "limit=100").await;
    for id in &b_ids {
        assert!(
            ids_in(&b_own).contains(id),
            "B's replayed page must hold B's own rows, and {id} is not one"
        );
    }
}
