//! The outbox: events land with the state change, and a rollback emits nothing.
//!
//! Two properties, and the second is the one that is easy to get wrong:
//!
//! 1. `darkroom.asset.ready`, `darkroom.asset.deleted` and
//!    `darkroom.variant.created` are inserted in the SAME transaction as the
//!    domain write, per `core/docs/event-outbox.md`.
//! 2. **A rolled-back transaction emits nothing.** Asserted here by opening a
//!    transaction, writing a domain row and an event, and rolling back — then
//!    counting rows in both tables. An outbox that survives a rollback is
//!    worse than one that can be lost: it announces a state change that never
//!    happened, and a consumer that built a thumbnail for an asset that does
//!    exist but is not `ready` has to distinguish the two.

mod common;

use common::*;
use darkroom::domain::{AssetStatus, VariantKind};
use darkroom::service::CreateUpload;
use uuid::Uuid;

async fn count(store: &darkroom::Store, table: &str) -> i64 {
    // `table` is never a caller-supplied string; every call site is a literal
    // in this file, which is the same reason the format! below is safe.
    let sql = format!("select count(*) from {table}");
    sqlx::query_scalar::<_, i64>(&sql)
        .fetch_one(store.pool())
        .await
        .unwrap_or_else(|e| panic!("counting {table}: {e}"))
}

async fn seed_ready(
    service: &darkroom::Service,
    objects: &std::sync::Arc<darkroom::objectstore::InMemoryObjectStore>,
) -> (Uuid, Uuid, Uuid) {
    let account = Uuid::new_v4();
    let user = Uuid::new_v4();
    let tenant = darkroom::auth::Tenant::from_principal(&principal(account, user));
    let payload = png(50, 50);
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
    (account, user, created.asset.id)
}

/// `darkroom.asset.ready` is emitted exactly once, when the bytes are
/// verified — not at create, when nothing is in storage yet.
#[tokio::test]
#[ignore = "needs TEST_DATABASE_URL; see tests/common/mod.rs"]
async fn ready_is_emitted_once_at_complete_and_not_at_create() {
    let store = test_store().await;
    let (service, objects) = test_service(store.clone());
    let account = Uuid::new_v4();
    let tenant = darkroom::auth::Tenant::from_principal(&principal(account, Uuid::new_v4()));

    let payload = png(24, 24);
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

    // Nothing yet. A `pending` asset is not a fact any consumer can act on:
    // the bytes are not in storage, and a consumer that built a thumbnail on
    // `created` would fail.
    assert_eq!(
        count(&store, "outbox_events").await,
        0,
        "creating a pending upload must not emit anything"
    );

    objects
        .apply_presigned_put(&created.presigned.url, payload, "image/png")
        .await
        .expect("puts");
    service
        .complete_upload(&tenant, created.asset.id, &checksum)
        .await
        .expect("completes");

    assert_eq!(count(&store, "outbox_events").await, 1, "exactly one event");
    let row = darkroom::outbox::claim_unpublished(store.pool(), 10)
        .await
        .expect("claims")
        .remove(0);
    assert_eq!(row.event_type, "darkroom.asset.ready");
    assert_eq!(row.source, "darkroom", "source is the publishing service");
    assert_eq!(
        row.subject,
        created.asset.id.to_string(),
        "subject is the entity"
    );
    assert_eq!(
        row.published_at, None,
        "unpublished until a publisher acks it"
    );
    assert_eq!(row.data["asset_id"], created.asset.id.to_string());
    assert_eq!(row.data["account_id"], account.to_string());
    assert_eq!(row.data["checksum"], checksum);
}

/// The negative case, and the one that matters: a rolled-back transaction
/// leaves no domain row AND no event. If either survives, the outbox is lying.
#[tokio::test]
#[ignore = "needs TEST_DATABASE_URL; see tests/common/mod.rs"]
async fn a_rolled_back_transaction_emits_nothing() {
    let store = test_store().await;
    let _service = darkroom::Service::new(
        std::sync::Arc::new(store.clone()),
        std::sync::Arc::new(darkroom::objectstore::InMemoryObjectStore::new()),
    );
    let account = Uuid::new_v4();
    let asset_id = Uuid::new_v4();
    let checksum = "b".repeat(64);

    // One transaction: a domain write and an event, then a rollback. This is
    // exactly the shape the service uses — `Outbox::new(&mut tx)` only accepts
    // a `&mut Transaction`, so the event cannot have been enqueued outside it.
    let mut tx = store.begin().await.expect("begins");
    darkroom::store::insert_asset_keyed(
        &mut *tx,
        &darkroom::Asset {
            id: asset_id,
            account_id: account,
            owner_user_id: Uuid::new_v4(),
            kind: darkroom::AssetKind::Image,
            original_filename: "rolled-back.png".into(),
            content_type: "image/png".into(),
            byte_size: 10,
            checksum: checksum.clone(),
            status: AssetStatus::Pending,
            metadata: serde_json::json!({}),
            created_at: darkroom::observability::now(),
            updated_at: darkroom::observability::now(),
        },
        &format!("a/{account}/rolledback/original"),
    )
    .await
    .expect("inserts");

    let event = darkroom::outbox::NewEvent::new(
        darkroom::outbox::EventType::AssetReady,
        asset_id.to_string(),
        serde_json::json!({"asset_id": asset_id.to_string()}),
    );
    darkroom::outbox::Outbox::new(&mut tx)
        .enqueue(&event)
        .await
        .expect("enqueues");

    tx.rollback().await.expect("rolls back");

    // Neither table has the row. Asserted on BOTH, because "the domain row is
    // gone" alone would still pass if the event were written outside the
    // transaction.
    assert_eq!(
        count(&store, "assets").await,
        0,
        "the domain row must be gone"
    );
    assert_eq!(
        count(&store, "outbox_events").await,
        0,
        "a rolled-back transaction must emit nothing"
    );
}

/// A failed commit — the state change and the event go together, so there is
/// no way to get the event without the state or the state without the event.
#[tokio::test]
#[ignore = "needs TEST_DATABASE_URL; see tests/common/mod.rs"]
async fn the_event_and_the_state_change_are_both_present_after_a_commit() {
    let store = test_store().await;
    let (service, objects) = test_service(store.clone());
    let (_account, _user, asset_id) = seed_ready(&service, &objects).await;

    let assets = count(&store, "assets").await;
    let events = count(&store, "outbox_events").await;
    assert_eq!(assets, 1);
    assert_eq!(events, 1, "ready emitted exactly once");

    // Delete emits `darkroom.asset.deleted` in the same transaction as the row
    // removal, and removes the storage object too.
    let tenant = darkroom::auth::Tenant::from_principal(&principal(
        _account_of(&store, asset_id).await,
        Uuid::new_v4(),
    ));
    service
        .delete_asset(&tenant, asset_id)
        .await
        .expect("deletes");

    assert_eq!(count(&store, "assets").await, 0, "the row is gone");
    assert_eq!(count(&store, "outbox_events").await, 2, "ready and deleted");
    assert!(objects.is_empty(), "the storage object is gone too");

    let types: Vec<String> = darkroom::outbox::claim_unpublished(store.pool(), 10)
        .await
        .expect("claims")
        .into_iter()
        .map(|r| r.event_type)
        .collect();
    assert!(types.contains(&"darkroom.asset.ready".to_string()));
    assert!(types.contains(&"darkroom.asset.deleted".to_string()));
}

async fn _account_of(store: &darkroom::Store, asset_id: Uuid) -> Uuid {
    sqlx::query_scalar::<_, Uuid>("select account_id from assets where id = $1")
        .bind(asset_id)
        .fetch_one(store.pool())
        .await
        .expect("reads the account")
}

/// `darkroom.variant.created` is emitted with the derived record, and the
/// original is untouched.
#[tokio::test]
#[ignore = "needs TEST_DATABASE_URL; see tests/common/mod.rs"]
async fn variant_created_is_emitted_and_the_original_is_untouched() {
    let store = test_store().await;
    let (service, objects) = test_service(store.clone());
    let (account, _user, asset_id) = seed_ready(&service, &objects).await;
    let tenant = darkroom::auth::Tenant::from_principal(&principal(account, Uuid::new_v4()));

    let original_before = service.get_asset(&tenant, asset_id).await.expect("reads");
    assert_eq!(count(&store, "outbox_events").await, 1);

    let variant = service
        .create_variant(&tenant, asset_id, VariantKind::Thumbnail)
        .await
        .expect("derives");

    assert_eq!(variant.kind, VariantKind::Thumbnail);
    assert_eq!(variant.width, Some(50), "the source is 50px, so no upscale");
    assert_eq!(variant.height, Some(50));
    assert!(variant.byte_size > 0);

    // The original row is byte-for-byte unchanged. A variant must never mutate
    // its parent: the original is what a client downloads, and a thumbnail
    // that quietly re-encoded it would corrupt every existing reference.
    let original_after = service.get_asset(&tenant, asset_id).await.expect("reads");
    assert_eq!(original_before.checksum, original_after.checksum);
    assert_eq!(original_before.byte_size, original_after.byte_size);
    assert_eq!(original_before.updated_at, original_after.updated_at);
    assert_eq!(original_before.status, AssetStatus::Ready);

    assert_eq!(count(&store, "outbox_events").await, 2);
    let row = darkroom::outbox::claim_unpublished(store.pool(), 10)
        .await
        .expect("claims")
        .into_iter()
        .find(|r| r.event_type == "darkroom.variant.created")
        .expect("the variant event");
    assert_eq!(
        row.subject,
        variant.id.to_string(),
        "subject is the VARIANT, not the asset"
    );
    assert_eq!(row.data["asset_id"], asset_id.to_string());
    assert_eq!(row.data["kind"], "thumbnail");
    assert_eq!(row.data["width"], 50);
}

/// A failed upload emits nothing. A consumer cannot act on "this upload did
/// not complete" in a way that is better served by the client receiving the
/// 4xx, so there is no event for it.
#[tokio::test]
#[ignore = "needs TEST_DATABASE_URL; see tests/common/mod.rs"]
async fn a_failed_upload_emits_nothing() {
    let store = test_store().await;
    let (service, _objects) = test_service(store.clone());
    let tenant = darkroom::auth::Tenant::from_principal(&principal(Uuid::new_v4(), Uuid::new_v4()));

    let created = service
        .create_upload(
            &tenant,
            CreateUpload {
                filename: "a.png".into(),
                content_type: "image/png".into(),
                byte_size: 10,
                checksum: "c".repeat(64),
            },
        )
        .await
        .expect("creates");

    // Complete with no object ever uploaded.
    let _ = service
        .complete_upload(&tenant, created.asset.id, &"c".repeat(64))
        .await;

    assert_eq!(
        count(&store, "outbox_events").await,
        0,
        "a failure is not an event"
    );
}
