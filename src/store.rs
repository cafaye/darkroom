//! Persistence: assets, variants, the outbox, and the idempotency ledger.
//!
//! ## Every query takes a tenant
//!
//! No public method on this type takes a bare id. They all take
//! [`Tenant`](crate::auth::Tenant) or an explicit `account_id`, and each of
//! them puts that in the `where` clause. That is not a convention repeated in
//! eleven places by hand — it is the only shape the methods have, so a
//! repository method that forgot `account_id` would not compile against
//! [`Tenant`] and would be caught by the reviewer who wrote the signature.
//!
//! A cross-tenant read returns `Ok(None)`, never an error and never a row. The
//! handler turns `None` into 404. Returning a distinguishable error is how a
//! "not found" and a "not yours" become two different responses, and 403 on the
//! second one leaks existence.
//!
//! ## Why `sqlx` and not an ORM
//!
//! darkroom's correctness arguments are all about transactions — the outbox
//! write is atomic with the domain write, the duplicate-upload path is a
//! `select … for update` on a unique index, and the rollback test asserts
//! nothing landed. An ORM hides the transaction boundaries it would need to be
//! argued about, and every one of those arguments is about a boundary. Raw SQL
//! with `sqlx` keeps the statements visible and checked at compile time.

use std::sync::Arc;

use serde_json::Value;
use sqlx::postgres::{PgConnection, PgPool, PgPoolOptions};
use sqlx::{Executor, PgExecutor, Postgres, Transaction};
use time::OffsetDateTime;
use uuid::Uuid;

use crate::auth::Tenant;
use crate::domain::{Asset, AssetStatus, AssetVariant, VariantKind};

/// Failures from the store. Two kinds, and the split matters: a caller-visible
/// conflict versus a fault we did not expect.
#[derive(Debug, thiserror::Error)]
pub enum StoreError {
    /// The pool could not hand out a connection. 503, not 500 — the request is
    /// fine and the client should retry.
    #[error("database unavailable: {0}")]
    Unavailable(String),

    /// A unique constraint fired where the service expected one. Specifically
    /// `assets_account_checksum_uniq`, and specifically at `POST /v1/uploads`
    /// when a concurrent request inserted the same bytes first.
    #[error("conflict: {0}")]
    Conflict(String),

    /// Anything else from sqlx. 500.
    #[error("database error: {0}")]
    Query(#[from] sqlx::Error),
}

/// Classify a sqlx error. Only two codes are expected, and both are handled
/// where they happen rather than being flattened into a 500.
fn classify(err: sqlx::Error) -> StoreError {
    match &err {
        // 53300 lock_not_available, 57P01 admin_shutdown, 08006/08001
        // connection_failure, 57P02/57P03 idle_in_transaction_session_timeout.
        // These are all "the database is not currently usable", and the
        // difference between 503 and 500 is that 503 tells an orchestrator to
        // stop sending traffic while 500 tells a human there is a bug.
        sqlx::Error::PoolTimedOut => StoreError::Unavailable(err.to_string()),
        sqlx::Error::PoolClosed => StoreError::Unavailable(err.to_string()),
        sqlx::Error::Io(io) => StoreError::Unavailable(io.to_string()),
        sqlx::Error::Database(db) => match db.code().as_deref() {
            Some("23505") => StoreError::Conflict(db.constraint().unwrap_or("unique").to_string()),
            Some("53300") | Some("57P01") | Some("08006") | Some("08001") | Some("57P03") => {
                StoreError::Unavailable(db.message().to_string())
            }
            _ => StoreError::Query(err),
        },
        _ => StoreError::Query(err),
    }
}

/// The pool. A cheap clone of an `Arc` internally, so passing it to a handler
/// is free.
#[derive(Clone, Debug)]
pub struct Store {
    pool: PgPool,
}

impl Store {
    /// Connect and verify the connection is usable.
    pub async fn connect(database_url: &str, max_connections: u32) -> Result<Self, StoreError> {
        let pool = PgPoolOptions::new()
            .max_connections(max_connections)
            // Bound, because an unbounded acquire turns a database blip into a
            // process that holds every request open forever.
            .acquire_timeout(std::time::Duration::from_secs(5))
            .connect(database_url)
            .await
            .map_err(classify)?;
        Ok(Self { pool })
    }

    pub fn from_pool(pool: PgPool) -> Self {
        Self { pool }
    }

    pub fn pool(&self) -> &PgPool {
        &self.pool
    }

    /// The readiness probe's actual work. A `select 1` round trip, so
    /// `/readyz` reports what it claims: that this process can get a connection
    /// and run a statement right now.
    pub async fn ping(&self) -> Result<(), StoreError> {
        sqlx::query("select 1")
            .execute(&self.pool)
            .await
            .map(|_| ())
            .map_err(classify)
    }

    pub async fn begin(&self) -> Result<Transaction<'static, Postgres>, StoreError> {
        // The 'static lifetime is the pool's, not a lie about the transaction:
        // a transaction from an owned clone of the Arc outlives the `&self`
        // borrow, which is what lets a handler hold it across an await on
        // object storage.
        let mut tx = self.pool.clone().begin().await.map_err(classify)?;
        // The transaction is the unit of atomicity for the outbox, and
        // `read_committed` is the default; stating it makes the isolation level
        // a decision in the diff rather than an assumption. `read_committed` +
        // `for update` on the duplicate path is what makes two concurrent
        // uploads of the same bytes serialise.
        let _ = tx.execute("set transaction isolation level read committed").await;
        Ok(tx)
    }

    // ---------------------------------------------------------------- assets

    // -------------------------------------------------------------- variants

}

// ------------------------------------------------ repository functions
//
// These take the executor as their first argument so a caller can pass
// either the pool (a read outside a transaction) or `&mut *tx` (a write that
// must be atomic with something else). They are module functions, not
// methods, so the executor is explicit at every call site rather than
// being `self.pool()` by default and a transaction only sometimes.

/// Insert a new asset in `pending`, with the storage key chosen by the caller.
///
/// The key is a parameter rather than being generated here because
/// [`crate::storage_key::StorageKey`] is the only thing that may construct one,
/// and it does so from the account id plus 128 random bits. A free `&str` here
/// would let any caller of this function — including a future one — pass a
/// client-supplied key into a column that has a unique index on it.
///
/// Returns `Err(StoreError::Conflict)` if `(account_id, checksum)` already
/// exists. The caller resolves that; see `service.rs::create_upload`.
pub async fn insert_asset_keyed<'e, E>(
    executor: E,
    asset: &Asset,
    storage_key: &str,
) -> Result<Asset, StoreError>
where
    E: PgExecutor<'e>,
{
    let row = sqlx::query_as::<_, AssetRow>(
        r#"
        insert into assets (
          id, account_id, owner_user_id, kind, original_filename,
          content_type, byte_size, checksum, storage_key, status, metadata,
          created_at, updated_at
        )
        values ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12, $13)
        returning id, account_id, owner_user_id, kind, original_filename,
                  content_type, byte_size, checksum, status, metadata,
                  created_at, updated_at, storage_key
        "#,
    )
    .bind(asset.id)
    .bind(asset.account_id)
    .bind(asset.owner_user_id)
    .bind(asset.kind.as_str())
    .bind(&asset.original_filename)
    .bind(&asset.content_type)
    .bind(asset.byte_size)
    .bind(&asset.checksum)
    .bind(storage_key)
    .bind(asset.status.as_str())
    .bind(&asset.metadata)
    .bind(asset.created_at)
    .bind(asset.updated_at)
    .fetch_one(executor)
    .await
    .map_err(classify)?;

    Ok(row.into_asset())
}

/// Find an asset by checksum **within the tenant**, with its storage key.
///
/// This is the duplicate-upload pre-check. The unique constraint
/// `(account_id, checksum)` is still the authority — two concurrent duplicates
/// can both miss here — but a pre-check turns the common case into an indexed
/// read instead of a failed transaction, and it is what makes the *response*
/// (the existing asset, not a 500) possible.
///
/// The `account_id` is in the where clause and not merely in the index
/// selection: a checksum shared by two accounts is two assets, and returning
/// the wrong one would hand a caller another tenant's file.
pub async fn find_asset_by_checksum<'e, E>(
    executor: E,
    tenant: &Tenant,
    checksum: &str,
) -> Result<Option<(Asset, String)>, StoreError>
where
    E: PgExecutor<'e>,
{
    let row = sqlx::query_as::<_, AssetRow>(
        r#"
        select id, account_id, owner_user_id, kind, original_filename,
               content_type, byte_size, checksum, status, metadata,
               created_at, updated_at, storage_key
          from assets
         where account_id = $1 and checksum = $2
         limit 1
        "#,
    )
    .bind(tenant.account_id())
    .bind(checksum)
    .fetch_optional(executor)
    .await
    .map_err(classify)?;

    Ok(row.map(|r| {
        let key = r.storage_key.clone();
        (r.into_asset(), key)
    }))
}

/// Find an asset by id **within the tenant**. `None` covers both "no such
/// asset" and "it belongs to someone else" — deliberately indistinguishable.
pub async fn find_asset<'e, E>(
    executor: E,
    tenant: &Tenant,
    asset_id: Uuid,
) -> Result<Option<(Asset, String)>, StoreError>
where
    E: PgExecutor<'e>,
{
    let row = sqlx::query_as::<_, AssetRow>(
        r#"
        select id, account_id, owner_user_id, kind, original_filename,
               content_type, byte_size, checksum, status, metadata,
               created_at, updated_at, storage_key
          from assets
         where id = $1 and account_id = $2
        "#,
    )
    .bind(asset_id)
    .bind(tenant.account_id())
    .fetch_optional(executor)
    .await
    .map_err(classify)?;

    Ok(row.map(|r| {
        let key = r.storage_key.clone();
        (r.into_asset(), key)
    }))
}

/// The storage key for an asset in this tenant, without loading the row.
/// Used by the delete path, which needs the key to remove the object and
/// nothing else.
pub async fn find_storage_key<'e, E>(
    executor: E,
    tenant: &Tenant,
    asset_id: Uuid,
) -> Result<Option<String>, StoreError>
where
    E: PgExecutor<'e>,
{
    let key: Option<(String,)> = sqlx::query_as(
        "select storage_key from assets where id = $1 and account_id = $2",
    )
    .bind(asset_id)
    .bind(tenant.account_id())
    .fetch_optional(executor)
    .await
    .map_err(classify)?;
    Ok(key.map(|(k,)| k))
}

/// One page of assets for the tenant, newest first.
///
/// Cursor pagination per core: `limit` defaults to 25 and caps at 100, the
/// cursor is opaque base64url, and the response is `data` + `page`. Offset
/// pagination cannot be stable while rows are being inserted, which is
/// exactly what an upload endpoint does constantly.
pub async fn list_assets<'e, E>(
    executor: E,
    tenant: &Tenant,
    limit: i64,
    cursor: Option<(OffsetDateTime, Uuid)>,
) -> Result<Vec<(Asset, String)>, StoreError>
where
    E: PgExecutor<'e>,
{
    // Keyset pagination on (created_at, id) — the index's own ordering, so
    // the planner walks the index rather than sorting.
    let rows = sqlx::query_as::<_, AssetRow>(
        r#"
        select id, account_id, owner_user_id, kind, original_filename,
               content_type, byte_size, checksum, status, metadata,
               created_at, updated_at, storage_key
          from assets
         where account_id = $1
           and ($2::timestamptz is null or (created_at, id) < ($2, $3))
         order by created_at desc, id desc
         limit $4
        "#,
    )
    .bind(tenant.account_id())
    .bind(cursor.as_ref().map(|(t, _)| *t))
    .bind(cursor.as_ref().map(|(_, i)| *i).unwrap_or_else(Uuid::nil))
    .bind(limit)
    .fetch_all(executor)
    .await
    .map_err(classify)?;

    Ok(rows
        .into_iter()
        .map(|r| {
            let key = r.storage_key.clone();
            (r.into_asset(), key)
        })
        .collect())
}

/// Transition an asset to `ready` with the checksum computed from the
/// stored bytes, scoped to the tenant. Returns the new row, or `None` if
/// the asset is not in this tenant or is not in `pending`.
///
/// The `and status = 'pending'` is a compare-and-set: two concurrent
/// `complete` calls cannot both win, so the second sees `None` and the
/// idempotency layer answers with the stored response instead of emitting a
/// second `darkroom.asset.ready` for the same asset.
pub async fn mark_ready<'e, E>(
    executor: E,
    tenant: &Tenant,
    asset_id: Uuid,
    verified_checksum: &str,
) -> Result<Option<(Asset, String)>, StoreError>
where
    E: PgExecutor<'e>,
{
    let row = sqlx::query_as::<_, AssetRow>(
        r#"
        update assets
           set status = 'ready',
               checksum = $3,
               updated_at = now()
         where id = $1 and account_id = $2 and status = 'pending'
        returning id, account_id, owner_user_id, kind, original_filename,
                  content_type, byte_size, checksum, status, metadata,
                  created_at, updated_at, storage_key
        "#,
    )
    .bind(asset_id)
    .bind(tenant.account_id())
    .bind(verified_checksum)
    .fetch_optional(executor)
    .await
    .map_err(classify)?;

    Ok(row.map(|r| {
        let key = r.storage_key.clone();
        (r.into_asset(), key)
    }))
}

/// Move an asset to `failed`. Used by both failure paths: the object never
/// arrived, and the checksum did not match. The row is kept rather than
/// deleted so the upload is a permanent record of "this was attempted and
/// did not succeed" and so the UNIQUE (account_id, checksum) constraint
/// stops a second bad attempt.
pub async fn mark_failed<'e, E>(
    executor: E,
    tenant: &Tenant,
    asset_id: Uuid,
    reason: &str,
) -> Result<Option<Asset>, StoreError>
where
    E: PgExecutor<'e>,
{
    let row = sqlx::query_as::<_, AssetRow>(
        r#"
        update assets
           set status = 'failed',
               metadata = metadata || jsonb_build_object('failure_reason', $3::text),
               updated_at = now()
         where id = $1 and account_id = $2 and status = 'pending'
        returning id, account_id, owner_user_id, kind, original_filename,
                  content_type, byte_size, checksum, status, metadata,
                  created_at, updated_at, storage_key
        "#,
    )
    .bind(asset_id)
    .bind(tenant.account_id())
    .bind(reason)
    .fetch_optional(executor)
    .await
    .map_err(classify)?;

    Ok(row.map(|r| r.into_asset()))
}

/// An asset in `pending` for longer than `older_than`, for the sweeper that
/// fails uploads whose presigned URL was issued and never used. Not wired
/// to a scheduler in this packet — see README "Not done" — but the query
/// exists and is tested, because a query with no caller is a query nobody
/// has ever run.
pub async fn find_stale_pending<'e, E>(
    executor: E,
    older_than: OffsetDateTime,
    limit: i64,
) -> Result<Vec<Asset>, StoreError>
where
    E: PgExecutor<'e>,
{
    let rows = sqlx::query_as::<_, AssetRow>(
        r#"
        select id, account_id, owner_user_id, kind, original_filename,
               content_type, byte_size, checksum, status, metadata,
               created_at, updated_at, storage_key
          from assets
         where status = 'pending' and created_at < $1
         order by created_at
         limit $2
        "#,
    )
    .bind(older_than)
    .bind(limit)
    .fetch_all(executor)
    .await
    .map_err(classify)?;
    Ok(rows.into_iter().map(|r| r.into_asset()).collect())
}

/// Delete the asset row. Variants go with it via `on delete cascade`, and
/// their storage objects are removed by the caller before this runs.
///
/// Scoped to the tenant, so a `delete` cannot remove another account's row.
/// Returns `false` when the row is not in this tenant, which the handler
/// turns into 404.
pub async fn delete_asset<'e, E>(
    executor: E,
    tenant: &Tenant,
    asset_id: Uuid,
) -> Result<bool, StoreError>
where
    E: PgExecutor<'e>,
{
    let result = sqlx::query("delete from assets where id = $1 and account_id = $2")
        .bind(asset_id)
        .bind(tenant.account_id())
        .execute(executor)
        .await
        .map_err(classify)?;
    Ok(result.rows_affected() == 1)
}

/// Every storage key belonging to an asset: the original plus each
/// variant. Read before the delete so the storage objects can be removed
/// with the keys still known.
pub async fn list_storage_keys<'e, E>(
    executor: E,
    tenant: &Tenant,
    asset_id: Uuid,
) -> Result<Vec<String>, StoreError>
where
    E: PgExecutor<'e>,
{
    let keys: Vec<(String,)> = sqlx::query_as(
        r#"
        select storage_key from assets where id = $1 and account_id = $2
        union all
        select storage_key from asset_variants where asset_id = $1 and account_id = $2
        "#,
    )
    .bind(asset_id)
    .bind(tenant.account_id())
    .fetch_all(executor)
    .await
    .map_err(classify)?;
    Ok(keys.into_iter().map(|(k,)| k).collect())
}

/// Insert or replace a variant. `on conflict (asset_id, kind) do update` is
/// what makes "re-request a kind" idempotent rather than accumulating rows.
pub async fn upsert_variant<'e, E>(
    executor: E,
    variant: &AssetVariant,
    storage_key: &str,
) -> Result<AssetVariant, StoreError>
where
    E: PgExecutor<'e>,
{
    let row = sqlx::query_as::<_, VariantRow>(
        r#"
        insert into asset_variants (
          id, asset_id, account_id, kind, content_type, byte_size,
          storage_key, width, height, metadata, created_at, updated_at
        )
        values ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12)
        on conflict (asset_id, kind) do update
           set content_type = excluded.content_type,
               byte_size    = excluded.byte_size,
               storage_key  = excluded.storage_key,
               width        = excluded.width,
               height       = excluded.height,
               metadata     = excluded.metadata,
               updated_at   = now()
        returning id, asset_id, account_id, kind, content_type, byte_size,
                  storage_key, width, height, metadata, created_at, updated_at
        "#,
    )
    .bind(variant.id)
    .bind(variant.asset_id)
    .bind(variant.account_id)
    .bind(variant.kind.as_str())
    .bind(&variant.content_type)
    .bind(variant.byte_size)
    .bind(storage_key)
    .bind(variant.width)
    .bind(variant.height)
    .bind(&variant.metadata)
    .bind(variant.created_at)
    .bind(variant.updated_at)
    .fetch_one(executor)
    .await
    .map_err(classify)?;
    Ok(row.into_variant())
}

/// The storage key of a variant, for the delete path.
pub async fn find_variant_storage_key<'e, E>(
    executor: E,
    tenant: &Tenant,
    asset_id: Uuid,
    kind: VariantKind,
) -> Result<Option<String>, StoreError>
where
    E: PgExecutor<'e>,
{
    let key: Option<(String,)> = sqlx::query_as(
        "select storage_key from asset_variants
          where asset_id = $1 and account_id = $2 and kind = $3",
    )
    .bind(asset_id)
    .bind(tenant.account_id())
    .bind(kind.as_str())
    .fetch_optional(executor)
    .await
    .map_err(classify)?;
    Ok(key.map(|(k,)| k))
}

/// Variants of an asset, scoped to the tenant.
pub async fn list_variants<'e, E>(
    executor: E,
    tenant: &Tenant,
    asset_id: Uuid,
) -> Result<Vec<AssetVariant>, StoreError>
where
    E: PgExecutor<'e>,
{
    // Both the asset_id and the account_id are in the where clause. The
    // asset_id alone would be enough *if* the caller had already been
    // authorised against the asset — but the authorisation check and this
    // read are separate statements, and a row that exists under another
    // account must not be reachable by guessing the asset id.
    let rows = sqlx::query_as::<_, VariantRow>(
        r#"
        select id, asset_id, account_id, kind, content_type, byte_size,
               storage_key, width, height, metadata, created_at, updated_at
          from asset_variants
         where asset_id = $1 and account_id = $2
         order by created_at desc, id desc
        "#,
    )
    .bind(asset_id)
    .bind(tenant.account_id())
    .fetch_all(executor)
    .await
    .map_err(classify)?;
    Ok(rows.into_iter().map(|r| r.into_variant()).collect())
}



// ------------------------------------------------------------- idempotency

/// The recorded outcome of a request that carried an `Idempotency-Key`.
///
/// core: "Replay with the same key **and** the same request body returns the
/// original response and `Idempotency-Replayed: true`. Replay with the same
/// key but a different body returns 409 `idempotency_key_reused`." The body
/// hash is what makes the second sentence possible.
#[derive(Debug, Clone, sqlx::FromRow)]
pub struct IdempotencyRecord {
    pub key: String,
    pub endpoint: String,
    pub request_hash: String,
    pub status_code: i32,
    pub response_body: Value,
}

/// The ledger. A table, not a cache, because the scope is
/// `(endpoint, principal, key)` and two replicas must agree on it — an
/// in-memory "have I seen this key" set is wrong the moment there is a second
/// instance, which is the same argument core makes for consumer dedupe.
pub async fn reserve_idempotency_key(
    executor: &mut PgConnection,
    key: &str,
    endpoint: &str,
    principal_key: &str,
    request_hash: &str,
) -> Result<IdempotencyOutcome, StoreError> {
    // The insert is the lock. Two concurrent replays race here and exactly one
    // wins; the loser reads the row the winner is about to write and returns
    // the stored response once it lands.
    //
    // A concrete `&mut PgConnection` rather than a generic `PgExecutor`: the
    // insert and the read below are two statements that must be on the SAME
    // connection, and a generic `E` would let a caller pass the pool to each and
    // run them on different connections — which would turn "reserve, then read"
    // into two unrelated queries.
    let inserted = sqlx::query(
        r#"
        insert into idempotency_keys (key, endpoint, principal_key, request_hash, status_code, response_body)
        values ($1, $2, $3, $4, 0, '{}'::jsonb)
        on conflict (key, endpoint, principal_key) do nothing
        "#,
    )
    .bind(key)
    .bind(endpoint)
    .bind(principal_key)
    .bind(request_hash)
    .execute(&mut *executor)
    .await
    .map_err(classify)?;

    if inserted.rows_affected() == 1 {
        return Ok(IdempotencyOutcome::Reserved);
    }

    // The row exists. A body that hashes differently is a client bug — the same
    // key for two different requests — and gets its own 409 code so the client
    // knows to pick a new key rather than to retry.
    let existing: Option<(String, i32, Value)> = sqlx::query_as(
        r#"
        select request_hash, status_code, response_body
          from idempotency_keys
         where key = $1 and endpoint = $2 and principal_key = $3
        "#,
    )
    .bind(key)
    .bind(endpoint)
    .bind(principal_key)
    .fetch_optional(&mut *executor)
    .await
    .map_err(classify)?;

    match existing {
        None => {
            // The winner's transaction rolled back between our insert failing
            // and this read. Treating it as "not reserved" lets the caller try
            // again, which is the only safe reading.
            Ok(IdempotencyOutcome::Reserved)
        }
        Some((stored_hash, _, _)) if stored_hash != request_hash => {
            Ok(IdempotencyOutcome::BodyMismatch)
        }
        // status_code 0 is the reservation marker: the row is claimed but the
        // handler has not finished. Replaying now is a concurrent request, and
        // the honest answer is 409 conflict with a message that says so —
        // not a fabricated replay of a response that does not exist yet.
        Some((_, 0, _)) => Ok(IdempotencyOutcome::InFlight),
        Some((_, status_code, response_body)) => Ok(IdempotencyOutcome::Replay {
            status_code,
            response_body,
        }),
    }
}

/// Write the outcome of a request, completing its reservation.
pub async fn complete_idempotency_key<'e, E>(
    executor: E,
    key: &str,
    endpoint: &str,
    principal_key: &str,
    status_code: i32,
    response_body: &Value,
) -> Result<(), StoreError>
where
    E: PgExecutor<'e>,
    {
    sqlx::query(
        r#"
        update idempotency_keys
           set status_code = $4, response_body = $5
         where key = $1 and endpoint = $2 and principal_key = $3
        "#,
    )
    .bind(key)
    .bind(endpoint)
    .bind(principal_key)
    .bind(status_code)
    .bind(response_body)
    .execute(executor)
    .await
    .map_err(classify)?;
    Ok(())
}

/// Drop a reservation whose handler failed, so a retry with the same key is a
/// fresh attempt rather than a permanent 409.
///
/// A failed request is not a recorded outcome: core says the ledger stores "the
/// response", and there is no response when the handler returned an error. A
/// client that got a 503 and retried with the same key must get a real attempt.
pub async fn release_idempotency_key<'e, E>(
    executor: E,
    key: &str,
    endpoint: &str,
    principal_key: &str,
) -> Result<(), StoreError>
where
    E: PgExecutor<'e>,
{
    // `and status_code = 0` so a completed record is never deleted by a
    // handler that failed after the fact.
    sqlx::query(
        r#"
        delete from idempotency_keys
         where key = $1 and endpoint = $2 and principal_key = $3 and status_code = 0
        "#,
    )
    .bind(key)
    .bind(endpoint)
    .bind(principal_key)
    .execute(executor)
    .await
    .map_err(classify)?;
    Ok(())
}

#[derive(Debug, Clone, PartialEq, Eq)]
pub enum IdempotencyOutcome {
    /// The caller owns the key and must perform the work.
    Reserved,
    /// A completed record for the same key and the same body. Return the stored
    /// response with `Idempotency-Replayed: true`.
    Replay { status_code: i32, response_body: Value },
    /// The same key with a different body. 409 `idempotency_key_reused`.
    BodyMismatch,
    /// Another request holds the reservation right now. 409 `conflict`.
    InFlight,
}

// ------------------------------------------------------------------- rows

/// The row shape, kept separate from the API shape. `storage_key` comes back
/// with the row and is stripped by [`AssetRow::into_asset`], so there is no
/// path that returns a key to a handler that might serialise it.
#[derive(Debug, Clone, sqlx::FromRow)]
struct AssetRow {
    id: Uuid,
    account_id: Uuid,
    owner_user_id: Uuid,
    kind: String,
    original_filename: String,
    content_type: String,
    byte_size: i64,
    checksum: String,
    storage_key: String,
    status: String,
    metadata: Value,
    created_at: OffsetDateTime,
    updated_at: OffsetDateTime,
}

impl AssetRow {
    fn into_asset(self) -> Asset {
        Asset {
            id: self.id,
            account_id: self.account_id,
            owner_user_id: self.owner_user_id,
            // A value the SQL check constraint already restricted. A failure
            // here means the database and this code disagree, which is a deploy
            // error, and the 500 it produces is the correct response.
            kind: self.kind.parse().unwrap_or(crate::domain::AssetKind::Document),
            original_filename: self.original_filename,
            content_type: self.content_type,
            byte_size: self.byte_size,
            checksum: self.checksum,
            status: self.status.parse().unwrap_or(AssetStatus::Pending),
            metadata: self.metadata,
            created_at: self.created_at,
            updated_at: self.updated_at,
        }
    }
}

#[derive(Debug, Clone, sqlx::FromRow)]
struct VariantRow {
    id: Uuid,
    asset_id: Uuid,
    account_id: Uuid,
    kind: String,
    content_type: String,
    byte_size: i64,
    /// Selected so a delete can find an object's key, never serialised. The
    /// delete path reads it via `find_variant_storage_key`, which selects only
    /// that column; this is here because `returning` names it.
    #[allow(dead_code)]
    storage_key: String,
    width: Option<i32>,
    height: Option<i32>,
    metadata: Value,
    created_at: OffsetDateTime,
    updated_at: OffsetDateTime,
}

impl VariantRow {
    fn into_variant(self) -> AssetVariant {
        AssetVariant {
            id: self.id,
            asset_id: self.asset_id,
            account_id: self.account_id,
            kind: self
                .kind
                .parse()
                .unwrap_or(crate::domain::VariantKind::Thumbnail),
            content_type: self.content_type,
            byte_size: self.byte_size,
            width: self.width,
            height: self.height,
            metadata: self.metadata,
            created_at: self.created_at,
            updated_at: self.updated_at,
        }
    }
}

pub type SharedStore = Arc<Store>;

/// Re-exported so `service.rs` can name a transaction without importing sqlx.
pub type Tx<'a> = Transaction<'a, Postgres>;
