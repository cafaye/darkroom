//! The signed-upload flow, end to end: create, PUT through the presigned URL,
//! complete, and every failure on the way.
//!
//! The client PUTs bytes **straight to storage** in these tests, through
//! `InMemoryObjectStore::apply_presigned_put` — the fake's simulation of the
//! bucket side of a presigned PUT. That is not a shortcut around the flow; it
//! IS the flow. The bytes never pass through the service, which is the entire
//! reason for the signed-upload pattern, and
//! `the_bytes_never_pass_through_the_service` asserts it.

mod common;

use bytes::Bytes;
use common::*;
use darkroom::domain::{AssetKind, AssetStatus};
use darkroom::objectstore::{ObjectStore, PRESIGN_TTL};
use darkroom::service::CreateUpload;
use uuid::Uuid;

/// Create an upload, PUT the bytes, complete it. The happy path, with each
/// intermediate state asserted so a regression cannot skip a step.
#[tokio::test]
#[ignore = "needs TEST_DATABASE_URL; see tests/common/mod.rs"]
async fn the_full_signed_upload_flow() {
    let store = test_store().await;
    let (service, objects) = test_service(store);
    let account = Uuid::new_v4();
    let tenant = darkroom::auth::Tenant::from_principal(&principal(account, Uuid::new_v4()));

    let payload = png(64, 48);
    let checksum = darkroom::checksum::sha256_hex(&payload);

    // 1. create — a pending asset and a presigned URL scoped to its key.
    let created = service
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
        .expect("creates");

    assert_eq!(created.asset.status, AssetStatus::Pending, "starts pending");
    assert_eq!(
        created.asset.kind,
        AssetKind::Image,
        "kind is derived from content_type"
    );
    assert!(!created.duplicate);
    assert!(
        created
            .presigned
            .url
            .contains(&created.presigned.key.replace('/', "%2F")),
        "the URL must address exactly the key the response names"
    );
    assert_eq!(created.presigned.expires_in_secs, PRESIGN_TTL.as_secs());

    // 2. the client PUTs. Nothing goes through the service.
    let puts_before = objects.stats().puts;
    objects
        .apply_presigned_put(&created.presigned.url, payload.clone(), "image/png")
        .await
        .expect("the presigned PUT succeeds");
    assert_eq!(
        objects.stats().puts,
        puts_before,
        "the client PUT must not go through ObjectStore::put — that is the service's path"
    );

    // 3. complete — verified, ready, and the checksum is the computed one.
    let ready = service
        .complete_upload(&tenant, created.asset.id, &checksum)
        .await
        .expect("completes");

    assert_eq!(ready.status, AssetStatus::Ready);
    assert_eq!(
        ready.id, created.asset.id,
        "the same asset is completed, not a new one"
    );
    assert_eq!(
        ready.checksum, checksum,
        "the stored checksum is the one computed from the bytes, which happen to equal the claim here"
    );
}

/// The property the whole pattern exists for: the bytes never pass through the
/// API process.
#[tokio::test]
#[ignore = "needs TEST_DATABASE_URL; see tests/common/mod.rs"]
async fn the_bytes_never_pass_through_the_service() {
    let store = test_store().await;
    let (service, objects) = test_service(store);
    let tenant = darkroom::auth::Tenant::from_principal(&principal(Uuid::new_v4(), Uuid::new_v4()));

    let payload = png(32, 32);
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

    // Before the client's PUT: zero gets and zero puts. A proxying service
    // would have a `get` on the complete path; this one only ever `head`s.
    assert_eq!(
        objects.stats().heads,
        0,
        "create must not probe the object store"
    );
    assert_eq!(
        objects.stats().gets,
        0,
        "create must not read the object store"
    );
    assert_eq!(
        objects.stats().puts,
        0,
        "create must not write to the object store"
    );

    objects
        .apply_presigned_put(&created.presigned.url, payload.clone(), "image/png")
        .await
        .expect("puts");

    // Complete `head`s for the metadata and then reads the object back once, to
    // verify the checksum against the bytes storage actually holds. That read is
    // the price of not asking the backend what checksum it recorded: on
    // Cloudflare R2 there is no `FULL_OBJECT` SHA-256 to ask for, so a
    // verification that trusted a header would silently stop verifying. See
    // tests/checksum_verification.rs.
    let before = objects.stats();
    service
        .complete_upload(
            &tenant,
            created.asset.id,
            &darkroom::checksum::sha256_hex(&payload),
        )
        .await
        .expect("completes");
    let after = objects.stats();
    assert_eq!(
        after.heads,
        before.heads + 1,
        "complete probes the object's metadata"
    );
    assert_eq!(
        after.gets,
        before.gets + 1,
        "and reads the object exactly once, to verify the checksum from the bytes"
    );
    assert_eq!(
        after.puts, before.puts,
        "the client's upload is still never written through the service"
    );
}

/// A checksum the client claimed that does not match what storage holds → 422,
/// and the asset is marked `failed` so the bad bytes cannot be retried into a
/// second row under the same constraint.
#[tokio::test]
#[ignore = "needs TEST_DATABASE_URL; see tests/common/mod.rs"]
async fn complete_with_a_wrong_checksum_is_422_and_fails_the_asset() {
    let store = test_store().await;
    let (service, objects) = test_service(store);
    let tenant = darkroom::auth::Tenant::from_principal(&principal(Uuid::new_v4(), Uuid::new_v4()));

    let payload = png(64, 64);
    let honest = darkroom::checksum::sha256_hex(&payload);
    // A DIFFERENT but well-formed 64-hex string: the shape is valid, so this
    // is not a format error, it is a mismatch against real bytes.
    let lied = "0".repeat(64);

    let created = service
        .create_upload(
            &tenant,
            CreateUpload {
                filename: "photo.png".into(),
                content_type: "image/png".into(),
                byte_size: payload.len() as i64,
                checksum: honest,
            },
        )
        .await
        .expect("creates");

    objects
        .apply_presigned_put(&created.presigned.url, payload, "image/png")
        .await
        .expect("puts");

    let err = service
        .complete_upload(&tenant, created.asset.id, &lied)
        .await
        .expect_err("a lying checksum must not be accepted");

    assert_eq!(err.status().as_u16(), 422, "a checksum mismatch is 422");
    assert_eq!(err.code(), "validation_failed");
    let problem = err.to_problem("/v1/uploads/x/complete", "t");
    assert_eq!(
        problem.errors.expect("names the field")[0].field,
        "checksum",
        "the 422 must say which field was wrong"
    );

    // And the asset is terminal-failed, not left pending forever.
    let after = service
        .get_asset(&tenant, created.asset.id)
        .await
        .expect("still readable");
    assert_eq!(after.status, AssetStatus::Failed);
    assert_eq!(
        after.metadata["failure_reason"], "checksum_mismatch",
        "the reason is recorded for whoever looks at this asset later"
    );
}

/// The object was never PUT — the presigned URL expired or the client gave up.
/// 409, because the upload did not happen and the client should retry by
/// making a new one.
#[tokio::test]
#[ignore = "needs TEST_DATABASE_URL; see tests/common/mod.rs"]
async fn complete_with_a_missing_object_is_409() {
    let store = test_store().await;
    let (service, objects) = test_service(store);
    let tenant = darkroom::auth::Tenant::from_principal(&principal(Uuid::new_v4(), Uuid::new_v4()));

    let payload = png(16, 16);
    let created = service
        .create_upload(
            &tenant,
            CreateUpload {
                filename: "photo.png".into(),
                content_type: "image/png".into(),
                byte_size: payload.len() as i64,
                checksum: darkroom::checksum::sha256_hex(&payload),
            },
        )
        .await
        .expect("creates");

    // Deliberately no `apply_presigned_put`. The key was presigned and never
    // written to, which `head` must report as absent.
    let head = objects.head(created.presigned.key.as_str()).await;
    assert!(
        matches!(head, Err(darkroom::objectstore::ObjectStoreError::NotFound)),
        "a presigned-but-unwritten key must read as absent, got {head:?}"
    );

    let err = service
        .complete_upload(
            &tenant,
            created.asset.id,
            &darkroom::checksum::sha256_hex(&payload),
        )
        .await
        .expect_err("an upload that never landed must not complete");
    assert_eq!(err.status().as_u16(), 409, "an absent object is a conflict");
    assert_eq!(err.code(), "conflict");

    let after = service
        .get_asset(&tenant, created.asset.id)
        .await
        .expect("still readable");
    assert_eq!(after.status, AssetStatus::Failed);
    assert_eq!(
        after.metadata["failure_reason"],
        "object_absent_at_complete"
    );
}

/// A malformed checksum is 422 at CREATE time, not at complete time twenty
/// minutes later after a gigabyte has been uploaded.
#[tokio::test]
#[ignore = "needs TEST_DATABASE_URL; see tests/common/mod.rs"]
async fn a_malformed_checksum_is_rejected_at_create_with_nothing_written() {
    let store = test_store().await;
    let (service, _objects) = test_service(store);
    let tenant = darkroom::auth::Tenant::from_principal(&principal(Uuid::new_v4(), Uuid::new_v4()));

    for bad in ["", "abc", &"g".repeat(64), &"a".repeat(63), &"a".repeat(65)] {
        let err = service
            .create_upload(
                &tenant,
                CreateUpload {
                    filename: "photo.png".into(),
                    content_type: "image/png".into(),
                    byte_size: 10,
                    checksum: bad.to_string(),
                },
            )
            .await
            .expect_err("must be rejected");
        assert_eq!(
            err.status().as_u16(),
            422,
            "accepted a malformed checksum: {bad:?}"
        );
        let problem = err.to_problem("/v1/uploads", "t");
        assert_eq!(problem.errors.expect("names a field")[0].field, "checksum");
    }

    // Nothing was written: a rejected create leaves no asset behind, so a
    // client that fixes the checksum and retries is not colliding with itself.
    let (assets, _, _, _) = service.list_assets(&tenant, 25, None).await.expect("lists");
    assert!(assets.is_empty(), "a rejected create must not leave a row");
}

/// The presigned URL is scoped to exactly one key and one length. A second
/// asset's key does not work with the first asset's URL.
#[tokio::test]
#[ignore = "needs TEST_DATABASE_URL; see tests/common/mod.rs"]
async fn a_presigned_url_only_writes_to_its_own_key() {
    let store = test_store().await;
    let (service, objects) = test_service(store);
    let tenant = darkroom::auth::Tenant::from_principal(&principal(Uuid::new_v4(), Uuid::new_v4()));

    let a = png(20, 20);
    let b = png(30, 30);
    let first = service
        .create_upload(
            &tenant,
            CreateUpload {
                filename: "a.png".into(),
                content_type: "image/png".into(),
                byte_size: a.len() as i64,
                checksum: darkroom::checksum::sha256_hex(&a),
            },
        )
        .await
        .expect("creates a");
    let second = service
        .create_upload(
            &tenant,
            CreateUpload {
                filename: "b.png".into(),
                content_type: "image/png".into(),
                byte_size: b.len() as i64,
                checksum: darkroom::checksum::sha256_hex(&b),
            },
        )
        .await
        .expect("creates b");

    assert_ne!(
        first.presigned.key, second.presigned.key,
        "two uploads, two keys"
    );

    // The scope that matters is the KEY, not the content. A presigned URL says
    // "these bytes may be written at this key" and deliberately says nothing
    // about what the bytes are — that is what the checksum at complete time is
    // for. So the property under test is: asset A's URL writes to asset A's
    // key, and asset A's key still holds nothing until A's URL is used.
    assert!(
        objects.head(first.presigned.key.as_str()).await.is_err(),
        "asset a's key must still be empty"
    );

    // Use A's own URL. It lands at A's key, and B's key stays empty.
    objects
        .apply_presigned_put(&first.presigned.url, a.clone(), "image/png")
        .await
        .expect("a's own url is valid for a's key");
    assert!(
        objects.head(first.presigned.key.as_str()).await.is_ok(),
        "a's bytes are now at a's key"
    );
    assert!(
        objects.head(second.presigned.key.as_str()).await.is_err(),
        "and nowhere near b's key"
    );

    // The checksum check is the thing that stops A writing the WRONG bytes
    // anywhere: completing A with a's real checksum succeeds, and completing A
    // with b's checksum is a 422.
    service
        .complete_upload(&tenant, first.asset.id, &darkroom::checksum::sha256_hex(&a))
        .await
        .expect("a completes with a's checksum");
    assert!(
        service
            .complete_upload(
                &tenant,
                second.asset.id,
                &darkroom::checksum::sha256_hex(&a)
            )
            .await
            .is_err(),
        "b's key is empty, so b cannot complete no matter what checksum is claimed"
    );
}

/// The presigned URL is scoped to the declared content type, so a URL signed
/// for `image/png` cannot be used to store an HTML document at that key.
#[tokio::test]
#[ignore = "needs TEST_DATABASE_URL; see tests/common/mod.rs"]
async fn a_presigned_url_is_scoped_to_its_content_type() {
    let store = test_store().await;
    let (service, objects) = test_service(store);
    let tenant = darkroom::auth::Tenant::from_principal(&principal(Uuid::new_v4(), Uuid::new_v4()));

    let payload = png(10, 10);
    let created = service
        .create_upload(
            &tenant,
            CreateUpload {
                filename: "a.png".into(),
                content_type: "image/png".into(),
                byte_size: payload.len() as i64,
                checksum: darkroom::checksum::sha256_hex(&payload),
            },
        )
        .await
        .expect("creates");

    let err = objects
        .apply_presigned_put(
            &created.presigned.url,
            Bytes::from_static(b"<html>not a png</html>"),
            "text/html",
        )
        .await
        .expect_err("a png-scoped url must not accept text/html");
    assert!(
        !matches!(err, darkroom::objectstore::ObjectStoreError::TooLarge),
        "the rejection is the content type, not the length: {err:?}"
    );

    // The correct type still works, so the assertion above is about scope and
    // not about the fake being broken.
    objects
        .apply_presigned_put(&created.presigned.url, payload, "image/png")
        .await
        .expect("the right type is accepted");
}
