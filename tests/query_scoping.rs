//! Tenant scoping **proved at the query layer**, against a real Postgres.
//!
//! ## The gap this file closes
//!
//! `tests/tenant_scoping.rs` reads `src/store.rs` and checks the `where` clause
//! is still written down. That is a property of the *source*. This file is a
//! property of the *database*: it builds a fixture holding two accounts' rows —
//! the same bytes, deliberately, because `unique (account_id, checksum)` makes
//! one checksum two separate assets — and then calls every account-scoped query
//! in `src/store.rs` with each account's [`Tenant`] and asserts the answer
//! contains only that account's rows.
//!
//! The failure this exists for is the common shape: **a service that scopes its
//! reads and forgets its delete.** So the four operation kinds are four
//! separate tests and the counts are asserted rather than implied.
//!
//! ## Absence, not refusal
//!
//! Every negative assertion is that a query returns `None`, `false` or an empty
//! `Vec` — never an error, and never a distinguishable "not yours". A
//! `StoreError` for another account's row is this layer's version of a 403: the
//! caller can tell it apart from absence, so it learns the row exists. The
//! handler's 404 is a *translation* of `None`; if this layer ever started
//! producing a distinguishable failure, the 404-instead-of-403 rule would hold
//! at the edge and be false underneath it.
//!
//! ## What the fixture does not do
//!
//! It does not upload bytes. Object storage is a separate tenant boundary with a
//! separate failure mode — a presigned URL whose scope is wrong — and it is
//! covered from the wire in `tests/tenant_isolation.rs`. Seeding rows directly
//! keeps these tests about *queries*: every assertion below is about what came
//! back from Postgres, and none of it depends on a codec.

mod common;

use common::*;
use darkroom::auth::Tenant;
use darkroom::domain::{Asset, AssetKind, AssetStatus, AssetVariant, VariantKind};
use darkroom::storage_key::StorageKey;
use darkroom::store::{self, Store};
use uuid::Uuid;

/// One account's rows and the keys that address them.
struct Row {
    pending: Uuid,
    ready: Uuid,
    variant: Uuid,
    storage_key: String,
    variant_key: String,
}

/// Both accounts' rows in one database, plus the checksum they deliberately
/// share.
struct Fixture {
    store: Store,
    accounts: TwoAccounts,
    a: Tenant,
    b: Tenant,
    a_rows: Row,
    b_rows: Row,
    shared_checksum: String,
}

/// A syntactically valid sha256 that nothing else in the fixture uses.
///
/// Real hex, not `repeat(32)`: `assets_account_checksum_uniq` does not validate
/// the shape, but a fixture that only works because the column is lax is a
/// fixture that stops working for a reason unrelated to the thing under test.
fn checksum(hex: char) -> String {
    hex.to_string().repeat(64)
}

/// Insert one `pending` and one `ready` asset for `account`, plus a thumbnail on
/// the ready one.
///
/// `at` is the timestamp of the *pending* row; the ready one is one second
/// later. The fixture interleaves the two accounts' timestamps on purpose — see
/// `a_cursor_from_another_account_returns_no_rows_of_theirs`, where an
/// interleave is what keeps the assertion from passing on an empty page.
async fn seed(
    store: &Store,
    account: Uuid,
    user: Uuid,
    shared: &str,
    at: time::OffsetDateTime,
) -> Row {
    let make = |id: Uuid, sum: String, status: AssetStatus, at: time::OffsetDateTime| Asset {
        id,
        account_id: account,
        owner_user_id: user,
        kind: AssetKind::Image,
        original_filename: "secret.png".to_string(),
        content_type: "image/png".to_string(),
        byte_size: 12,
        checksum: sum,
        status,
        metadata: serde_json::json!({}),
        created_at: at,
        updated_at: at,
    };

    // Two checksums inside one account, or the unique constraint refuses the
    // second insert. `pending` is unique to this account so the update tests
    // have a row only this account can transition; `ready` is the shared one.
    let pending_id = Uuid::new_v4();
    let pending_key = StorageKey::generate_original(account);
    store::insert_asset_keyed(
        store.pool(),
        &make(pending_id, checksum('a'), AssetStatus::Pending, at),
        pending_key.as_str(),
    )
    .await
    .expect("pending asset inserts");

    let ready_id = Uuid::new_v4();
    let ready_key = StorageKey::generate_original(account);
    store::insert_asset_keyed(
        store.pool(),
        &make(
            ready_id,
            shared.to_string(),
            AssetStatus::Ready,
            at + time::Duration::seconds(2),
        ),
        ready_key.as_str(),
    )
    .await
    .expect("ready asset inserts");

    let variant_key = StorageKey::generate_variant(account, "thumbnail");
    let variant = AssetVariant {
        id: Uuid::new_v4(),
        asset_id: ready_id,
        account_id: account,
        kind: VariantKind::Thumbnail,
        content_type: "image/webp".to_string(),
        byte_size: 8,
        width: Some(1),
        height: Some(1),
        metadata: serde_json::json!({}),
        created_at: at + time::Duration::seconds(2),
        updated_at: at + time::Duration::seconds(2),
    };
    let stored = store::upsert_variant(store.pool(), &variant, variant_key.as_str())
        .await
        .expect("variant inserts");

    Row {
        pending: pending_id,
        ready: ready_id,
        variant: stored.id,
        storage_key: ready_key.into_string(),
        variant_key: variant_key.into_string(),
    }
}

/// Two accounts, interleaved in time, each with a `pending` asset, a `ready`
/// asset carrying the *same* checksum as the other account's, and a thumbnail
/// on it.
///
/// A is newest on the `ready` row and B is newest on `ready` too, one second
/// apart, with both `pending` rows older than either. That ordering is what
/// makes the cursor test's negative assertion non-vacuous.
async fn fixture() -> Fixture {
    let store = test_store().await;
    let accounts = two_accounts();
    let shared = checksum('f');
    let base = darkroom::observability::now() - time::Duration::seconds(60);

    // A.pending  t+0
    // B.pending  t+1
    // A.ready    t+2   <- A's newest, and the cursor the test replays
    // B.ready    t+3
    let a_rows = seed(&store, accounts.a_account, accounts.a_user, &shared, base).await;
    let b_rows = seed(
        &store,
        accounts.b_account,
        accounts.b_user,
        &shared,
        base + time::Duration::seconds(1),
    )
    .await;

    // A fixture in which the two accounts are not distinguishable is worth no
    // assertion in it, and a uuid collision is not worth a debugging session.
    assert_ne!(accounts.a_account, accounts.b_account);
    assert_ne!(a_rows.ready, b_rows.ready);
    assert_ne!(a_rows.storage_key, b_rows.storage_key);
    assert_ne!(a_rows.variant, b_rows.variant);

    let a = Tenant::from_principal(&principal(accounts.a_account, accounts.a_user));
    let b = Tenant::from_principal(&principal(accounts.b_account, accounts.b_user));

    Fixture {
        store,
        accounts,
        a,
        b,
        a_rows,
        b_rows,
        shared_checksum: shared,
    }
}

/// The status of a row, read *as its own account* — the only way to see it. Used
/// to prove a cross-tenant attempt changed nothing, which says more than "the
/// call returned None".
async fn status_of(f: &Fixture, tenant: &Tenant, id: Uuid) -> Option<AssetStatus> {
    store::find_asset(f.store.pool(), tenant, id)
        .await
        .expect("reads")
        .map(|(asset, _)| asset.status)
}

#[tokio::test]
#[ignore = "needs TEST_DATABASE_URL; see tests/common/mod.rs"]
async fn every_read_is_scoped_to_the_callers_account() {
    let f = fixture().await;
    let other_ready = f.b_rows.ready;

    // --- positive: each account reads its own --------------------------------
    let (mine, _) = store::find_asset(f.store.pool(), &f.a, f.a_rows.ready)
        .await
        .expect("reads")
        .expect("A's own asset is visible to A");
    assert_eq!(mine.account_id, f.accounts.a_account);
    let (theirs, _) = store::find_asset(f.store.pool(), &f.b, f.b_rows.ready)
        .await
        .expect("reads")
        .expect("B's own asset is visible to B");
    assert_eq!(theirs.account_id, f.accounts.b_account);

    // --- negative: A's tenant against B's rows --------------------------------
    // `None`, not an error. A `StoreError` here would be the query layer's 403.
    assert!(
        store::find_asset(f.store.pool(), &f.a, other_ready)
            .await
            .expect("no error, only absence")
            .is_none(),
        "A must not read B's asset by id"
    );
    assert!(
        store::find_storage_key(f.store.pool(), &f.a, other_ready)
            .await
            .expect("no error, only absence")
            .is_none(),
        "A must not learn B's storage key from B's asset id"
    );
    assert!(
        store::find_variant_storage_key(f.store.pool(), &f.a, other_ready, VariantKind::Thumbnail)
            .await
            .expect("no error, only absence")
            .is_none(),
        "A must not learn B's variant storage key from B's asset id"
    );
    // A checksum is a *shared* value, so the negative assertion here is NOT
    // "nothing" — the correct answer is A's own row, because that is what A
    // uploaded. What must not happen is B's row coming back, which is the leak
    // `unique (account_id, checksum)` makes possible and
    // `the_same_bytes_are_two_assets_and_neither_account_sees_the_other` states
    // in full. Asserting `is_none()` here would be asserting that scoping works
    // by breaking the feature.
    match store::find_asset_by_checksum(f.store.pool(), &f.a, &f.shared_checksum)
        .await
        .expect("reads")
    {
        Some((asset, _)) => assert_eq!(
            asset.id, f.a_rows.ready,
            "A's checksum lookup must resolve to A's own row, not B's"
        ),
        None => panic!("A must resolve its own asset from the shared checksum"),
    }

    // And a checksum nobody uploaded is `None` for both — a miss, not an error.
    let absent = checksum('e');
    assert!(
        store::find_asset_by_checksum(f.store.pool(), &f.a, &absent)
            .await
            .expect("no error, only absence")
            .is_none(),
        "a checksum with no row in A is a miss, not B's row"
    );
    assert!(
        store::find_asset_by_checksum(f.store.pool(), &f.b, &absent)
            .await
            .expect("no error, only absence")
            .is_none(),
        "and a miss in B as well"
    );

    // --- and the other direction, because scoping that only works one way is
    // --- still a leak -------------------------------------------------------
    assert!(
        store::find_asset(f.store.pool(), &f.b, f.a_rows.ready)
            .await
            .expect("no error, only absence")
            .is_none(),
        "B must not read A's asset by id"
    );
    assert!(
        store::find_storage_key(f.store.pool(), &f.b, f.a_rows.ready)
            .await
            .expect("no error, only absence")
            .is_none(),
        "B must not learn A's storage key from A's asset id"
    );
    assert!(
        store::find_variant_storage_key(
            f.store.pool(),
            &f.b,
            f.a_rows.ready,
            VariantKind::Thumbnail
        )
        .await
        .expect("no error, only absence")
        .is_none(),
        "B must not learn A's variant storage key from A's asset id"
    );
    match store::find_asset_by_checksum(f.store.pool(), &f.b, &f.shared_checksum)
        .await
        .expect("reads")
    {
        Some((asset, _)) => assert_eq!(
            asset.id, f.b_rows.ready,
            "B's checksum lookup must resolve to B's own row, not A's"
        ),
        None => panic!("B must resolve its own asset from the shared checksum"),
    }

    // --- and A's own key lookups still work, so nothing above is a
    // --- "the query is broken for everyone" pass ------------------------------
    assert_eq!(
        store::find_storage_key(f.store.pool(), &f.a, f.a_rows.ready)
            .await
            .expect("reads"),
        Some(f.a_rows.storage_key.clone()),
        "scoping must not stop A seeing its own storage key"
    );
    assert_eq!(
        store::find_variant_storage_key(
            f.store.pool(),
            &f.a,
            f.a_rows.ready,
            VariantKind::Thumbnail
        )
        .await
        .expect("reads"),
        Some(f.a_rows.variant_key.clone()),
        "scoping must not stop A seeing its own variant key"
    );
}

#[tokio::test]
#[ignore = "needs TEST_DATABASE_URL; see tests/common/mod.rs"]
async fn every_listing_returns_only_the_callers_own_rows() {
    let f = fixture().await;

    // A sees exactly A's two assets — never B's, even though B's rows sit in
    // the same table and carry the same checksum.
    let a_assets = store::list_assets(f.store.pool(), &f.a, 100, None)
        .await
        .expect("lists");
    let mut a_ids: Vec<Uuid> = a_assets.iter().map(|(a, _)| a.id).collect();
    a_ids.sort();
    let mut expected = vec![f.a_rows.ready, f.a_rows.pending];
    expected.sort();
    assert_eq!(a_ids, expected, "A's page is A's rows and nothing else");
    for (asset, _) in &a_assets {
        // The account on the row, not just its membership: a row whose account
        // is wrong would be invisible to an id comparison.
        assert_eq!(asset.account_id, f.accounts.a_account);
    }

    let b_assets = store::list_assets(f.store.pool(), &f.b, 100, None)
        .await
        .expect("lists");
    assert_eq!(b_assets.len(), 2, "B has exactly two assets and owns both");
    for (asset, _) in &b_assets {
        assert_eq!(asset.account_id, f.accounts.b_account);
        assert!(
            asset.id != f.a_rows.ready && asset.id != f.a_rows.pending,
            "B's listing contained an A row"
        );
    }

    // --- variants ------------------------------------------------------------
    let a_variants = store::list_variants(f.store.pool(), &f.a, f.a_rows.ready)
        .await
        .expect("lists");
    assert_eq!(a_variants.len(), 1, "A's asset has one variant");
    assert_eq!(a_variants[0].id, f.a_rows.variant);
    assert_eq!(a_variants[0].account_id, f.accounts.a_account);

    // Empty, not an error: at the wire an empty list is indistinguishable from
    // "no variants yet", which is a one-bit existence oracle.
    assert!(
        store::list_variants(f.store.pool(), &f.a, f.b_rows.ready)
            .await
            .expect("lists")
            .is_empty(),
        "A must not list B's variants"
    );
    assert!(
        store::list_variants(f.store.pool(), &f.b, f.a_rows.ready)
            .await
            .expect("lists")
            .is_empty(),
        "B must not list A's variants"
    );

    // --- storage keys, the delete path's own listing -------------------------
    // Two `select`s in a `union all`, so this is the query most able to be
    // half-scoped by accident: the `assets` half constrained and the
    // `asset_variants` half not.
    let a_keys = store::list_storage_keys(f.store.pool(), &f.a, f.a_rows.ready)
        .await
        .expect("lists");
    assert_eq!(a_keys.len(), 2, "A's asset has an original and a variant");
    assert!(a_keys.contains(&f.a_rows.storage_key));
    assert!(a_keys.contains(&f.a_rows.variant_key));

    // Non-empty on the other hand, so the emptiness below is the scoping and
    // not a fixture that never had a key.
    let b_keys = store::list_storage_keys(f.store.pool(), &f.b, f.b_rows.ready)
        .await
        .expect("lists");
    assert_eq!(b_keys.len(), 2);

    assert!(
        store::list_storage_keys(f.store.pool(), &f.a, f.b_rows.ready)
            .await
            .expect("lists")
            .is_empty(),
        "A must not learn B's storage keys, or the delete path will delete them"
    );
    assert!(
        store::list_storage_keys(f.store.pool(), &f.b, f.a_rows.ready)
            .await
            .expect("lists")
            .is_empty(),
        "B must not learn A's storage keys, or the delete path will delete them"
    );
}

#[tokio::test]
#[ignore = "needs TEST_DATABASE_URL; see tests/common/mod.rs"]
async fn an_update_from_another_account_changes_nothing() {
    let f = fixture().await;

    // --- mark_ready ----------------------------------------------------------
    // A tries to complete B's pending upload. `None` — and B's row is *still
    // pending*, which is the assertion that matters: the compare-and-set is
    // `where … and status = 'pending'`, so a scoping regression here does not
    // fail loudly, it silently completes the wrong upload.
    assert!(
        store::mark_ready(f.store.pool(), &f.a, f.b_rows.pending, &f.shared_checksum)
            .await
            .expect("no error, only absence")
            .is_none(),
        "A must not transition B's pending asset to ready"
    );
    assert_eq!(
        status_of(&f, &f.b, f.b_rows.pending).await,
        Some(AssetStatus::Pending),
        "B's upload must still be pending after A tried to complete it"
    );
    // And B's checksum was not rewritten, which is the same claim about the
    // column rather than about the status.
    let (b_row, _) = store::find_asset(f.store.pool(), &f.b, f.b_rows.pending)
        .await
        .expect("reads")
        .expect("B's row is still there");
    assert_eq!(
        b_row.checksum,
        checksum('a'),
        "A's cross-tenant mark_ready must not write its checksum into B's row"
    );

    // --- mark_failed ---------------------------------------------------------
    // The other update, and the more dangerous one: a cross-tenant `mark_failed`
    // needs no object in storage, no checksum and no race. It is a pure `where`
    // clause, which makes it the update most likely to be missing one.
    assert!(
        store::mark_failed(f.store.pool(), &f.a, f.b_rows.pending, "cross_tenant")
            .await
            .expect("no error, only absence")
            .is_none(),
        "A must not fail B's pending asset"
    );
    assert_eq!(
        status_of(&f, &f.b, f.b_rows.pending).await,
        Some(AssetStatus::Pending),
        "B's upload must still be pending after A tried to fail it"
    );

    // The reason too: a `mark_failed` that missed its scoping would still leave
    // the status alone if it also missed the `pending` guard, so the metadata
    // is checked separately rather than inferred from the status.
    let (b_pending, _) = store::find_asset(f.store.pool(), &f.b, f.b_rows.pending)
        .await
        .expect("reads")
        .expect("B's row is still there");
    assert!(
        !b_pending
            .metadata
            .as_object()
            .is_some_and(|m| m.contains_key("failure_reason")),
        "A's cross-tenant mark_failed wrote a failure_reason into B's row: {:?}",
        b_pending.metadata
    );

    // --- the positive half ---------------------------------------------------
    // A suite that only asserts denials passes on a service that refuses
    // everyone, so both accounts must still be able to write their own.
    //
    // The checksum passed is the `pending` row's **own** declared checksum, not
    // the shared one, because `mark_ready` writes the verified checksum into the
    // column and `unique (account_id, checksum)` would then hold two A rows with
    // the same checksum. That is the constraint doing its job, and it is worth
    // stating because the failure it produces is a `Conflict`, not a scoping
    // bug — a reader hitting it would otherwise go looking in the wrong place.
    let a_own = checksum('a');
    let (ready, _) = store::mark_ready(f.store.pool(), &f.a, f.a_rows.pending, &a_own)
        .await
        .expect("no error")
        .expect("A can complete A's own upload");
    assert_eq!(ready.status, AssetStatus::Ready);
    assert_eq!(ready.account_id, f.accounts.a_account);
    // The row now carries the computed checksum, not the one it was created
    // with — the compare-and-set replaced it, which is the service's rule.
    assert_eq!(ready.checksum, a_own);

    let failed = store::mark_failed(f.store.pool(), &f.b, f.b_rows.pending, "object_absent")
        .await
        .expect("no error")
        .expect("B can fail B's own upload");
    assert_eq!(failed.status, AssetStatus::Failed);
    assert_eq!(failed.account_id, f.accounts.b_account);

    // And B's write did not touch A's pending row.
    assert_eq!(
        status_of(&f, &f.a, f.a_rows.pending).await,
        Some(AssetStatus::Ready),
        "B's own mark_failed left A's row exactly as A had set it"
    );
}

#[tokio::test]
#[ignore = "needs TEST_DATABASE_URL; see tests/common/mod.rs"]
async fn a_delete_from_another_account_removes_nothing() {
    let f = fixture().await;

    // The one the brief names: a service that scopes reads and forgets deletes.
    // This is data loss rather than disclosure, it is a single `where` clause,
    // and no read-path test in the repository would notice it.
    assert!(
        !store::delete_asset(f.store.pool(), &f.a, f.b_rows.ready)
            .await
            .expect("no error, only a false"),
        "A must not delete B's asset"
    );

    let (survivor, key) = store::find_asset(f.store.pool(), &f.b, f.b_rows.ready)
        .await
        .expect("reads")
        .expect("B's asset survived A's delete");
    assert_eq!(survivor.account_id, f.accounts.b_account);
    assert_eq!(key, f.b_rows.storage_key);
    assert_eq!(
        store::list_variants(f.store.pool(), &f.b, f.b_rows.ready)
            .await
            .expect("lists")
            .len(),
        1,
        "B's variant survived too: the cascade follows the row, not the account"
    );

    // The other direction, then A's own delete still working.
    assert!(
        !store::delete_asset(f.store.pool(), &f.b, f.a_rows.ready)
            .await
            .expect("no error, only a false"),
        "B must not delete A's asset"
    );
    assert!(
        store::find_asset(f.store.pool(), &f.a, f.a_rows.ready)
            .await
            .expect("reads")
            .is_some(),
        "A's asset survived B's delete"
    );
    assert!(
        store::delete_asset(f.store.pool(), &f.a, f.a_rows.ready)
            .await
            .expect("no error"),
        "A must still be able to delete A's own asset"
    );
    assert!(
        store::find_asset(f.store.pool(), &f.a, f.a_rows.ready)
            .await
            .expect("reads")
            .is_none(),
        "A's own delete removed it"
    );
    assert!(
        store::find_asset(f.store.pool(), &f.b, f.b_rows.ready)
            .await
            .expect("reads")
            .is_some(),
        "B's asset survived every delete above"
    );
}

#[tokio::test]
#[ignore = "needs TEST_DATABASE_URL; see tests/common/mod.rs"]
async fn the_same_bytes_are_two_assets_and_neither_account_sees_the_other() {
    // The case a scoping bug hides in. `unique (account_id, checksum)` makes the
    // identical checksum legitimately two rows, so an unscoped lookup by checksum
    // does not error and does not return nothing — it returns a row, and the
    // wrong one. Every id-keyed test above would stay green if this one did.
    let f = fixture().await;

    let a_hit = store::find_asset_by_checksum(f.store.pool(), &f.a, &f.shared_checksum)
        .await
        .expect("reads")
        .expect("A resolves its own asset from the shared checksum");
    let b_hit = store::find_asset_by_checksum(f.store.pool(), &f.b, &f.shared_checksum)
        .await
        .expect("reads")
        .expect("B resolves its own asset from the shared checksum");

    assert_eq!(a_hit.0.id, f.a_rows.ready);
    assert_eq!(b_hit.0.id, f.b_rows.ready);
    assert_ne!(
        a_hit.0.id, b_hit.0.id,
        "the same bytes in two accounts are two rows, not one"
    );
    assert_eq!(a_hit.0.account_id, f.accounts.a_account);
    assert_eq!(b_hit.0.account_id, f.accounts.b_account);
    // The storage keys differ too, so a leaked key names a *different* object
    // rather than repeating a harmless answer.
    assert_ne!(a_hit.1, b_hit.1);
}

#[tokio::test]
#[ignore = "needs TEST_DATABASE_URL; see tests/common/mod.rs"]
async fn a_cursor_from_another_account_returns_no_rows_of_theirs() {
    // A pagination cursor is `(created_at, id)` in base64url and it comes back
    // to the client, so it is an account-scoped *input* like any other: a caller
    // can be handed one and replay it. The keyset predicate
    // `(created_at, id) < ($2, $3)` and the tenant predicate are separate
    // clauses, and a regression that dropped the second would page straight out
    // of one account and into the other.
    let f = fixture().await;

    // A's newest page, and the cursor it hands back. In the fixture's ordering
    // that is A's `ready` row, and exactly one of A's two rows is older.
    let first = store::list_assets(f.store.pool(), &f.a, 1, None)
        .await
        .expect("lists");
    assert_eq!(first.len(), 1);
    assert_eq!(
        first[0].0.id, f.a_rows.ready,
        "A's newest row is A's ready row"
    );
    let cursor = (first[0].0.created_at, first[0].0.id);

    // A walking its own cursor sees its own next row — non-empty, so the
    // negative assertion below is about scoping and not about an empty page.
    let second = store::list_assets(f.store.pool(), &f.a, 10, Some(cursor))
        .await
        .expect("lists");
    assert_eq!(
        second.iter().map(|(a, _)| a.id).collect::<Vec<_>>(),
        vec![f.a_rows.pending],
        "A's second page is A's one older row"
    );

    // B replays A's cursor. The cursor encodes A's asset id and A's timestamp,
    // and B still gets nothing of A's: B's older `pending` row and nothing else.
    let b_page = store::list_assets(f.store.pool(), &f.b, 10, Some(cursor))
        .await
        .expect("lists");
    assert_eq!(
        b_page.iter().map(|(a, _)| a.id).collect::<Vec<_>>(),
        vec![f.b_rows.pending],
        "B replaying A's cursor must get B's own older row and nothing of A's"
    );
    for (asset, _) in &b_page {
        assert_eq!(asset.account_id, f.accounts.b_account);
    }
}

#[tokio::test]
#[ignore = "needs TEST_DATABASE_URL; see tests/common/mod.rs"]
async fn no_scoped_query_ever_returns_a_row_under_the_wrong_account() {
    // The sweep. Every account-scoped query in `src/store.rs`, run against both
    // accounts, with one assertion over everything they returned.
    //
    // The count matters. A sweep that quietly stopped calling a query — because
    // it was renamed, or because an `if` grew around it — would pass this file's
    // assertions and prove less than it reads. The count is what turns that back
    // into a failure, and
    // `tests/tenant_scoping.rs::every_scoped_query_has_a_case_in_the_database_suite`
    // is the other half: it fails when a *new* scoped query appears in
    // `src/store.rs` with no case here.
    let f = fixture().await;
    let mut seen: Vec<Uuid> = Vec::new();

    for (label, tenant, own, other, expected) in [
        ("A", &f.a, &f.a_rows, &f.b_rows, f.accounts.a_account),
        ("B", &f.b, &f.b_rows, &f.a_rows, f.accounts.b_account),
    ] {
        // Per-account, and *per round*. Asserting ownership over a cumulative
        // `seen` would make the B round demand that A's rows belong to B — which
        // is what the first version of this test did, and it failed on its own
        // fixture for that reason. A sweep whose bookkeeping is wrong fails on
        // the second account and gets read as a leak, which is worse than not
        // having it: it teaches a reader to ignore it.
        let mut round: Vec<Uuid> = Vec::new();

        // --- the positive reads --------------------------------------------
        if let Some((asset, _)) = store::find_asset(f.store.pool(), tenant, own.ready)
            .await
            .expect("reads")
        {
            assert_eq!(asset.account_id, expected);
            round.push(asset.id);
        }
        if let Some((asset, _)) =
            store::find_asset_by_checksum(f.store.pool(), tenant, &f.shared_checksum)
                .await
                .expect("reads")
        {
            assert_eq!(asset.account_id, expected);
            round.push(asset.id);
        }
        for (asset, _) in store::list_assets(f.store.pool(), tenant, 100, None)
            .await
            .expect("lists")
        {
            assert_eq!(
                asset.account_id, expected,
                "{label}'s listing returned another account's row"
            );
            round.push(asset.id);
        }
        for variant in store::list_variants(f.store.pool(), tenant, own.ready)
            .await
            .expect("lists")
        {
            assert_eq!(variant.account_id, expected);
            round.push(variant.id);
        }

        // --- the negative probe of every kind --------------------------------
        assert!(store::find_asset(f.store.pool(), tenant, other.ready)
            .await
            .expect("reads")
            .is_none());
        assert!(store::find_storage_key(f.store.pool(), tenant, other.ready)
            .await
            .expect("reads")
            .is_none());
        assert!(store::find_variant_storage_key(
            f.store.pool(),
            tenant,
            other.ready,
            VariantKind::Thumbnail
        )
        .await
        .expect("reads")
        .is_none());
        assert!(store::list_variants(f.store.pool(), tenant, other.ready)
            .await
            .expect("lists")
            .is_empty());
        assert!(
            store::list_storage_keys(f.store.pool(), tenant, other.ready)
                .await
                .expect("lists")
                .is_empty()
        );
        assert!(
            store::mark_ready(f.store.pool(), tenant, other.pending, &f.shared_checksum)
                .await
                .expect("updates")
                .is_none()
        );
        assert!(
            store::mark_failed(f.store.pool(), tenant, other.pending, "cross_tenant")
                .await
                .expect("updates")
                .is_none()
        );
        assert!(!store::delete_asset(f.store.pool(), tenant, other.pending)
            .await
            .expect("deletes"));

        // Everything this round handed back belongs to this account. The oracle
        // is test-local and knows the fixture layout, which is exactly what a
        // sweep needs and what production code never has.
        for id in &round {
            assert_eq!(
                id_owner(id, &f),
                expected,
                "{label} returned row {id}, which is not {label}'s"
            );
        }
        seen.extend(round);
    }

    // Five positive reads per account — the row by id, the row by checksum, both
    // rows in the listing, and the one variant — so ten pushes covering six
    // distinct rows. A sweep that quietly stopped calling a query would pass
    // every assertion above and prove less than it reads; the count is what
    // turns that back into a failure.
    assert_eq!(seen.len(), 10, "five positive reads per account: {seen:?}");
    let mut distinct = seen.clone();
    distinct.sort();
    distinct.dedup();
    assert_eq!(
        distinct.len(),
        6,
        "four assets and two variants: {distinct:?}"
    );
    for id in [f.a_rows.ready, f.a_rows.pending, f.a_rows.variant] {
        assert!(seen.contains(&id), "A's row {id} was never reached");
    }
    for id in [f.b_rows.ready, f.b_rows.pending, f.b_rows.variant] {
        assert!(seen.contains(&id), "B's row {id} was never reached");
    }
}

/// The account that owns `id` in the fixture. A test-local oracle: it knows the
/// layout, which is what the sweep needs and what production code never does.
fn id_owner(id: &Uuid, f: &Fixture) -> Uuid {
    if *id == f.a_rows.ready || *id == f.a_rows.pending || *id == f.a_rows.variant {
        f.accounts.a_account
    } else {
        f.accounts.b_account
    }
}
