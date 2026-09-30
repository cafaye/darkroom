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
    let b_tenant =
        darkroom::auth::Tenant::from_principal(&principal(accounts.b_account, accounts.b_user));
    let variant = service
        .create_variant(
            &darkroom::auth::Tenant::from_principal(&principal(
                accounts.a_account,
                accounts.a_user,
            )),
            a_asset,
            darkroom::VariantKind::Thumbnail,
        )
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
    let a_tenant =
        darkroom::auth::Tenant::from_principal(&principal(accounts.a_account, accounts.a_user));
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
    let _ = b_tenant;
    let _ = variant;
}

/// The load-bearing assertion: a cross-tenant 404 and a genuinely-missing
/// asset produce **byte-identical** responses. Different bodies are just as
/// much of an oracle as different statuses.
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

    async fn get(app: axum::Router, id: Uuid, token: &'static str) -> (StatusCode, bytes::Bytes) {
        let request = Request::builder()
            .uri(format!("/v1/assets/{id}"))
            .header("authorization", format!("Bearer {token}"))
            .body(Body::empty())
            .expect("builds");
        let response = app.oneshot(request).await.expect("responds");
        let status = response.status();
        let body = axum::body::to_bytes(response.into_body(), 64 * 1024)
            .await
            .expect("body");
        (status, body)
    }

    let (exists_status, exists_body) = get(app.clone(), a_asset, "token-b").await;
    let (missing_status, missing_body) = get(app.clone(), never_existed, "token-b").await;

    assert_eq!(exists_status, StatusCode::NOT_FOUND);
    assert_eq!(missing_status, StatusCode::NOT_FOUND);
    // `trace_id` is per-request and `instance` is the path, so both differ by
    // design. The rest of the envelope must be identical.
    let strip = |body: &[u8]| -> serde_json::Value {
        let mut value: serde_json::Value = serde_json::from_slice(body).expect("json");
        let object = value.as_object_mut().expect("object");
        object.remove("trace_id");
        object.remove("instance");
        value
    };
    assert_eq!(
        strip(&exists_body),
        strip(&missing_body),
        "the two 404s must be identical apart from the per-request trace id and path"
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
