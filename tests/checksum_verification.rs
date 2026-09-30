//! Read-back verification: the checksum decision comes from the **bytes**, on
//! every backend.
//!
//! This is the file that Cloudflare R2 exists for. R2's S3 compatibility table
//! lists SHA-256 as `COMPOSITE` only — `FULL_OBJECT` is available for CRC-64/NVME
//! and nothing else — so a design that asks the bucket "what checksum did you
//! record?" and trusts the answer has a correctness property that exists on S3
//! and silently does not exist on R2. That is the failure mode this service
//! exists to prevent, so verification here never asks.
//!
//! ## The table
//!
//! One body, run over two backends that differ only in what they hand back from
//! `get`. There is no per-backend assertion: the behaviour under test is the
//! service's, and a suite that branched on the backend would be proving the
//! branch.
//!
//! | backend | `get` returns | expected |
//! |---|---|---|
//! | honest | the bytes that were PUT | `200`, `ready` |
//! | corrupting | the same length, one byte flipped | `422`, `failed` |
//!
//! ## The cost, stated once
//!
//! Verification costs one read per completed upload. That is a real cost and it
//! is the right trade: a checksum that was not verified is a checksum that was
//! not checked. `verification_reads_the_object_back_exactly_once` is the guard —
//! it is the test that fails if someone "optimises" the read back into a header
//! lookup, and it is why the read is one and not two.

mod common;

use std::sync::Arc;

use common::*;
use darkroom::domain::AssetStatus;
use darkroom::error::Error;
use darkroom::objectstore::{ObjectStore, StoreStats};
use darkroom::service::{CreateUpload, Service};
use uuid::Uuid;

/// Which backend a row runs against. The only difference is the fault, and the
/// assertions are the same ones.
#[derive(Clone, Copy, PartialEq, Eq)]
enum Backend {
    Honest,
    Corrupting,
}

impl Backend {
    fn name(&self) -> &'static str {
        match self {
            Backend::Honest => "honest",
            Backend::Corrupting => "corrupting",
        }
    }

    /// The store for this row, wired into the service as a plain
    /// `dyn ObjectStore` — the only thing production holds, and the reason a
    /// fault can be introduced at all.
    fn store(&self) -> Arc<TestObjectStore> {
        match self {
            Backend::Honest => Arc::new(TestObjectStore::new()),
            Backend::Corrupting => Arc::new(TestObjectStore::corrupting()),
        }
    }
}

/// What a row produced, so the shared body reads as a table row rather than as a
/// pile of assertions buried in a helper.
struct Outcome {
    service: Service,
    tenant: darkroom::auth::Tenant,
    asset_id: Uuid,
    complete: Result<darkroom::domain::Asset, Error>,
}

/// The shared body. Create, the client PUTs straight to storage through the
/// presigned URL, complete. Every row of the table goes through here, so a
/// change to the verification rules is a change to one function.
async fn complete_against(backend: Backend) -> Outcome {
    let store = test_store().await;
    let objects = backend.store();
    let service = Service::new(Arc::new(store), objects.clone());

    let tenant = darkroom::auth::Tenant::from_principal(&principal(Uuid::new_v4(), Uuid::new_v4()));
    let payload = png(48, 32);
    let checksum = darkroom::checksum::sha256_hex(&payload);

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
        .unwrap_or_else(|e| panic!("the {} backend creates: {e}", backend.name()));

    // The client PUTs straight to the bucket, through the presigned URL. The
    // bytes never pass through the service, and the corruption in the second row
    // is introduced by the backend on read, not by this PUT.
    objects
        .inner()
        .apply_presigned_put(&created.presigned.url, payload, "image/png")
        .await
        .unwrap_or_else(|e| panic!("the {} backend takes the client's PUT: {e}", backend.name()));

    let complete = service
        .complete_upload(&tenant, created.asset.id, &checksum)
        .await;
    Outcome {
        service,
        tenant,
        asset_id: created.asset.id,
        complete,
    }
}

/// Row one: storage holds the bytes the client sent, so complete verifies and
/// the row carries the checksum computed from those bytes.
#[tokio::test]
#[ignore = "needs TEST_DATABASE_URL; see tests/common/mod.rs"]
async fn verification_succeeds_when_storage_holds_the_bytes_that_were_uploaded() {
    let outcome = complete_against(Backend::Honest).await;
    let ready = outcome.complete.expect("the honest row verifies");
    assert_eq!(ready.status, AssetStatus::Ready);
    assert_eq!(ready.id, outcome.asset_id, "the same asset, not a new one");
    assert_eq!(
        ready.checksum,
        darkroom::checksum::sha256_hex(&png(48, 32)),
        "the stored checksum is computed from the bytes, not copied from the claim"
    );
}

/// Row two: storage does not hold those bytes, and the same body catches it.
///
/// The name says what it protects rather than which row it is, because this is
/// the assertion the whole packet exists for.
#[tokio::test]
#[ignore = "needs TEST_DATABASE_URL; see tests/common/mod.rs"]
async fn a_corrupted_object_is_caught_by_read_back_verification() {
    let outcome = complete_against(Backend::Corrupting).await;
    let err = outcome
        .complete
        .expect_err("bytes that are not the bytes the client sent must not verify");

    assert_eq!(err.status().as_u16(), 422, "a mismatch is 422");
    assert_eq!(err.code(), "validation_failed");
    assert_eq!(
        err.to_problem("/v1/uploads/x/complete", "t")
            .errors
            .expect("names the field")[0]
            .field,
        "checksum",
        "the 422 must say which field was wrong"
    );

    // Terminal, with the reason recorded. The `UNIQUE (account_id, checksum)` row
    // keeps the checksum, so these bytes cannot be retried into a second asset
    // behind a lie.
    let after = outcome
        .service
        .get_asset(&outcome.tenant, outcome.asset_id)
        .await
        .expect("still readable");
    assert_eq!(after.status, AssetStatus::Failed);
    assert_eq!(after.metadata["failure_reason"], "checksum_mismatch");
}

/// Metadata cannot reveal the corruption, which is the only reason a read-back is
/// worth its cost. Asserted directly so a future change to the fault cannot
/// quietly turn row two into a size-mismatch test.
#[tokio::test]
#[ignore = "needs TEST_DATABASE_URL; see tests/common/mod.rs"]
async fn a_store_can_lie_in_metadata_and_still_be_caught_by_the_bytes() {
    let store = test_store().await;
    let objects = Backend::Corrupting.store();
    let service = Service::new(Arc::new(store), objects.clone());
    let tenant = darkroom::auth::Tenant::from_principal(&principal(Uuid::new_v4(), Uuid::new_v4()));

    let payload = png(64, 64);
    let claimed = darkroom::checksum::sha256_hex(&payload);
    let created = service
        .create_upload(
            &tenant,
            CreateUpload {
                filename: "photo.png".into(),
                content_type: "image/png".into(),
                byte_size: payload.len() as i64,
                checksum: claimed.clone(),
            },
        )
        .await
        .expect("creates");
    objects
        .inner()
        .apply_presigned_put(&created.presigned.url, payload.clone(), "image/png")
        .await
        .expect("puts");

    // Everything `head` can say is correct.
    let meta = objects.head(&created.presigned.key).await.expect("head");
    assert_eq!(meta.byte_size, payload.len() as i64, "size is right");
    assert_eq!(meta.content_type, "image/png", "content type is right");
    assert!(
        objects.head(&created.presigned.key).await.is_ok(),
        "and the object is plainly present"
    );

    // Only the bytes disagree — and they disagree on read, with the length
    // intact, so a size check is not what catches it.
    let read_back = objects.get(&created.presigned.key).await.expect("get");
    assert_eq!(read_back.len(), payload.len(), "the length is unchanged");
    assert_ne!(
        darkroom::checksum::sha256_hex(&read_back),
        claimed,
        "the fault must actually be corrupting, or this file proves nothing"
    );

    assert_eq!(
        service
            .complete_upload(&tenant, created.asset.id, &claimed)
            .await
            .expect_err("metadata is not evidence")
            .status()
            .as_u16(),
        422
    );
}

/// The one read per completed upload.
///
/// This is the test that fails if verification goes back to asking the backend for
/// a checksum. It is also the honest statement of what verification costs:
/// `head` for the metadata, `get` for the bytes, and never a `put` — the client's
/// upload still does not pass through this process.
#[tokio::test]
#[ignore = "needs TEST_DATABASE_URL; see tests/common/mod.rs"]
async fn verification_reads_the_object_back_exactly_once() {
    let store = test_store().await;
    let objects = Backend::Honest.store();
    let service = Service::new(Arc::new(store), objects.clone());
    let tenant = darkroom::auth::Tenant::from_principal(&principal(Uuid::new_v4(), Uuid::new_v4()));

    let payload = png(24, 24);
    let checksum = darkroom::checksum::sha256_hex(&payload);
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

    assert_eq!(
        objects.stats(),
        StoreStats {
            presigns: 1,
            ..StoreStats::default()
        },
        "create mints a URL and nothing else: no probe, no read, no write"
    );

    objects
        .inner()
        .apply_presigned_put(&created.presigned.url, payload, "image/png")
        .await
        .expect("puts");
    let before = objects.stats();

    service
        .complete_upload(&tenant, created.asset.id, &checksum)
        .await
        .expect("completes");

    let after = objects.stats();
    assert_eq!(
        after.heads,
        before.heads + 1,
        "one metadata probe, for existence and the measured size"
    );
    assert_eq!(
        after.gets,
        before.gets + 1,
        "exactly one read-back: the verification itself. A store that recorded a \
         checksum and a client that believed it is the R2 bug, and this is the \
         assertion that fails when somebody puts it back."
    );
    assert_eq!(
        after.puts, before.puts,
        "and the client's bytes are still never written through the service"
    );
}

/// A missing object is still a 409 and still costs no read. The `head` exists so
/// the "the upload never landed" answer does not have to download a gigabyte to
/// discover the gigabyte is not there.
#[tokio::test]
#[ignore = "needs TEST_DATABASE_URL; see tests/common/mod.rs"]
async fn an_absent_object_is_a_conflict_and_downloads_nothing() {
    let store = test_store().await;
    let objects = Backend::Corrupting.store();
    let service = Service::new(Arc::new(store), objects.clone());
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

    // No PUT. The key was presigned and never written to.
    let err = service
        .complete_upload(
            &tenant,
            created.asset.id,
            &darkroom::checksum::sha256_hex(&payload),
        )
        .await
        .expect_err("an upload that never landed must not complete");
    assert_eq!(err.status().as_u16(), 409);
    assert_eq!(err.code(), "conflict");
    assert_eq!(objects.stats().heads, 1, "one probe, and no read");
    assert_eq!(objects.stats().gets, 0, "nothing to read, so nothing read");

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

/// Read-back verification must not disturb duplicate resolution, which is keyed
/// on the *claimed* checksum and runs before storage is ever read.
///
/// The read happens at complete; the duplicate decision happens at create. A
/// verification change that moved hashing earlier — to reject a bad checksum at
/// create, say — would silently turn a duplicate into a fresh row and break
/// `UNIQUE (account_id, checksum)`. This is the regression guard for that.
#[tokio::test]
#[ignore = "needs TEST_DATABASE_URL; see tests/common/mod.rs"]
async fn read_back_verification_does_not_break_duplicate_detection() {
    let store = test_store().await;
    let objects = Backend::Honest.store();
    let service = Service::new(Arc::new(store), objects.clone());
    let tenant = darkroom::auth::Tenant::from_principal(&principal(Uuid::new_v4(), Uuid::new_v4()));

    let payload = png(40, 40);
    let byte_size = payload.len() as i64;
    let checksum = darkroom::checksum::sha256_hex(&payload);
    let create = || CreateUpload {
        filename: "photo.png".into(),
        content_type: "image/png".into(),
        byte_size,
        checksum: checksum.clone(),
    };

    let first = service
        .create_upload(&tenant, create())
        .await
        .expect("creates");
    assert!(!first.duplicate);
    objects
        .inner()
        .apply_presigned_put(&first.presigned.url, payload.clone(), "image/png")
        .await
        .expect("puts");
    service
        .complete_upload(&tenant, first.asset.id, &checksum)
        .await
        .expect("completes");

    // Same bytes, same account: the existing asset, with a usable URL for its own
    // key.
    let reads_before_duplicate = objects.stats().gets;
    let second = service
        .create_upload(&tenant, create())
        .await
        .expect("resolves");
    assert!(second.duplicate, "the second create is a duplicate");
    assert_eq!(second.asset.id, first.asset.id, "and it is the same asset");
    assert_eq!(second.presigned.key, first.presigned.key);
    assert_eq!(
        objects.stats().gets,
        reads_before_duplicate,
        "resolving a duplicate must not read the object: the decision is the \
         claim matching the row, and hashing early would make this a second row"
    );

    // And it completes. One row, one object, one bill.
    let ready = service
        .complete_upload(&tenant, second.asset.id, &checksum)
        .await
        .expect("the duplicate's own complete still verifies");
    assert_eq!(ready.status, AssetStatus::Ready);
    assert_eq!(ready.checksum, checksum);

    let (assets, _, _, _) = service.list_assets(&tenant, 25, None).await.expect("lists");
    assert_eq!(
        assets.len(),
        1,
        "still exactly one asset for these bytes in this account"
    );
}

/// The other half of the interaction: a checksum that failed verification still
/// holds the `UNIQUE (account_id, checksum)` row, so the same bytes cannot be
/// registered again. A read that proved the bytes were wrong must not also
/// release the claim on them.
#[tokio::test]
#[ignore = "needs TEST_DATABASE_URL; see tests/common/mod.rs"]
async fn a_verification_failure_still_holds_the_duplicate_constraint() {
    let store = test_store().await;
    let objects = Backend::Corrupting.store();
    let service = Service::new(Arc::new(store), objects.clone());
    let tenant = darkroom::auth::Tenant::from_principal(&principal(Uuid::new_v4(), Uuid::new_v4()));

    let payload = png(52, 52);
    let byte_size = payload.len() as i64;
    let checksum = darkroom::checksum::sha256_hex(&payload);
    let create = || CreateUpload {
        filename: "photo.png".into(),
        content_type: "image/png".into(),
        byte_size,
        checksum: checksum.clone(),
    };

    let created = service
        .create_upload(&tenant, create())
        .await
        .expect("creates");
    objects
        .inner()
        .apply_presigned_put(&created.presigned.url, payload, "image/png")
        .await
        .expect("puts");
    assert!(
        service
            .complete_upload(&tenant, created.asset.id, &checksum)
            .await
            .is_err(),
        "the corrupting store cannot verify"
    );

    // The same bytes again. The asset is `failed`, its checksum was never
    // verified, and the constraint still holds the row: 409, not a second row.
    let err = service
        .create_upload(&tenant, create())
        .await
        .expect_err("a duplicate of a failed upload is a conflict");
    assert_eq!(err.status().as_u16(), 409);
    assert_eq!(err.code(), "conflict");
    assert!(
        err.detail().contains("failed"),
        "the message must say the earlier upload failed, got: {}",
        err.detail()
    );

    let (assets, _, _, _) = service.list_assets(&tenant, 25, None).await.expect("lists");
    assert_eq!(assets.len(), 1, "still one row for these bytes");
}
