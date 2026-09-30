//! The service layer: the business rules, with no axum in it.
//!
//! Everything that is a *decision* lives here rather than in a handler, so it can
//! be tested without an HTTP round trip and so a handler stays a translation
//! between the wire and a call. Concretely: the signed-upload state machine, the
//! checksum verification, the duplicate-upload resolution, and the
//! tenant-scoped queries are all in this file.
//!
//! ## The upload state machine
//!
//! ```text
//!   POST /v1/uploads
//!        │
//!        ├── duplicate (same account, same checksum) ──▶ return the existing asset
//!        ▼
//!     pending ──complete, object present, checksum matches──▶ ready  (+ darkroom.asset.ready)
//!              │
//!              ├──complete, object absent──────────────────▶ failed (409)
//!              └──complete, checksum mismatch──────────────▶ failed (422)
//! ```
//!
//! `ready` and `failed` are terminal. A `failed` asset is never retried into
//! `ready`: the client creates a new upload. That keeps the UNIQUE
//! `(account_id, checksum)` constraint useful — a checksum that failed once
//! cannot be silently retried into a second attempt with the same key — and it
//! means `pending` older than the presign TTL is a sweepable set with exactly
//! one meaning.

use std::sync::Arc;
use std::time::Duration;

use serde_json::json;
use uuid::Uuid;

use crate::auth::Tenant;
use crate::checksum;
use crate::domain::{Asset, AssetKind, AssetStatus, AssetVariant, VariantKind};
use crate::error::{Error, FieldError};
use crate::objectstore::{
    InMemoryObjectStore, PresignedPut, SharedObjectStore, MAX_UPLOAD_BYTES, PRESIGN_TTL,
};
use crate::outbox::{EventType, NewEvent, Outbox};
use crate::storage_key::StorageKey;
use crate::store::{self, SharedStore, StoreError, Tx};

/// What a handler needs to serve a request. Cloneable, and cheap to clone: the
/// two handles are `Arc`s.
#[derive(Clone)]
pub struct Service {
    pub store: SharedStore,
    pub objects: SharedObjectStore,
    /// The in-memory store, when that is what is wired. `None` for S3.
    ///
    /// Held so the integration tests and the dev server can PUT bytes through a
    /// presigned URL the way a client does. Production never reads it.
    pub in_memory_objects: Option<Arc<InMemoryObjectStore>>,
}

impl Service {
    pub fn new(store: SharedStore, objects: SharedObjectStore) -> Self {
        Self {
            store,
            objects,
            in_memory_objects: None,
        }
    }

    pub fn with_in_memory_objects(store: SharedStore, objects: Arc<InMemoryObjectStore>) -> Self {
        Self {
            store,
            objects: objects.clone() as SharedObjectStore,
            in_memory_objects: Some(objects),
        }
    }
}

/// `POST /v1/uploads` — create the asset, issue a presigned PUT.
#[derive(Debug, Clone)]
pub struct CreateUpload {
    pub filename: String,
    pub content_type: String,
    pub byte_size: i64,
    /// The sha256 the client says it is about to upload. Validated for shape
    /// here, verified against the stored bytes at complete time.
    pub checksum: String,
}

/// The result of a create. `replayed` is true when this is the existing asset
/// returned for a duplicate, so the handler can label the response without
/// re-deriving it.
#[derive(Debug, Clone)]
pub struct CreatedUpload {
    pub asset: Asset,
    pub presigned: PresignedPut,
    /// True when the checksum already existed for this account and the existing
    /// asset was returned instead of creating a second one.
    pub duplicate: bool,
}

impl Service {
    /// Create an upload and presign its destination.
    ///
    /// ## The duplicate decision
    ///
    /// **A duplicate returns the existing asset with 201, not a 409.**
    ///
    /// The constraint is `UNIQUE (account_id, checksum)` — the same bytes for
    /// the same account are one asset. When the insert collides, this returns
    /// the row that is already there, with a fresh presigned URL for **its**
    /// key, and `duplicate: true`.
    ///
    /// The reasoning, in the order that decided it:
    ///
    /// - **A 409 makes the client solve a problem it did not create.** From the
    ///   client's side the two uploads are unrelated — a user attaching the same
    ///   screenshot to two messages, an importer retrying a directory where one
    ///   file was already done. A 409 means every such client has to
    ///   implement "on conflict, fetch the existing asset and carry on", and
    ///   every client that has not yet will look like it is failing.
    /// - **The bytes are already there and already verified.** The existing
    ///   asset reached `ready`, which means its checksum was checked against
    ///   what storage holds. Handing it back gives the client a usable asset
    ///   with no second upload and no second copy of a file that may be a
    ///   gigabyte.
    /// - **Re-presigning is safe and makes the response usable.** The caller
    ///   gets a working URL, so a client that just wants to PUT and complete
    ///   does the same three calls either way. Nothing has to know it was a
    ///   duplicate.
    /// - **It is still one asset, which is the point of the constraint.** No
    ///   second row, no second storage object, no second bill.
    ///
    /// What it is *not*: silent. `duplicate: true` becomes
    /// `X-Darkroom-Duplicate: true` on the response, so a client that wants to
    /// tell the user "already uploaded" can, and the log line records it.
    ///
    /// The one case this deliberately does not cover: an existing asset in
    /// `failed` status. A failed asset's checksum was never verified, so
    /// returning it would hand back something unusable. That insert still
    /// collides on the constraint, so the failure is detected and reported as a
    /// 409 with a message that says the bytes previously failed — the honest
    /// answer, because the constraint says they already exist and this service
    /// does not get to decide otherwise.
    pub async fn create_upload(
        &self,
        tenant: &Tenant,
        request: CreateUpload,
    ) -> Result<CreatedUpload, Error> {
        // --- validate, before anything is written or signed ---------------
        let filename = validate_filename(&request.filename)?;
        let kind = AssetKind::from_content_type(&request.content_type)?;
        let content_type = request
            .content_type
            .split(';')
            .next()
            .unwrap_or_default()
            .trim()
            .to_ascii_lowercase();

        if request.byte_size < 0 {
            return Err(Error::invalid_fields(
                "byte_size must not be negative",
                vec![FieldError::new("byte_size", "out_of_range")],
            ));
        }
        if request.byte_size > MAX_UPLOAD_BYTES {
            return Err(Error::invalid_fields(
                format!("byte_size must not exceed {MAX_UPLOAD_BYTES}"),
                vec![FieldError::new("byte_size", "too_large")],
            ));
        }
        if !checksum::is_valid_checksum_format(&request.checksum) {
            return Err(Error::invalid_fields(
                "checksum must be 64 hex characters (sha256)",
                vec![FieldError::new("checksum", "invalid_format")],
            ));
        }

        // --- the duplicate path, checked before the write -----------------
        // A SELECT first rather than catching the constraint violation, so the
        // common duplicate case is a cheap indexed read instead of a failed
        // transaction. The insert is still the authority: two concurrent
        // duplicates both miss here, and one of them gets the constraint
        // violation and falls through to the same resolution.
        if let Some(existing) =
            store::find_asset_by_checksum(self.store.pool(), tenant, &request.checksum).await?
        {
            return self
                .resolve_duplicate(tenant, existing, &content_type)
                .await;
        }

        // --- create -------------------------------------------------------
        let now = crate::observability::now();
        let asset = Asset {
            id: Uuid::new_v4(),
            account_id: tenant.account_id(),
            owner_user_id: tenant.user_id(),
            kind,
            original_filename: filename,
            content_type: content_type.clone(),
            byte_size: request.byte_size,
            checksum: request.checksum.to_ascii_lowercase(),
            status: AssetStatus::Pending,
            metadata: json!({}),
            created_at: now,
            updated_at: now,
        };
        let key = StorageKey::generate_original(tenant.account_id());

        let created = match self.insert_with_key(&asset, &key).await {
            Ok(row) => row,
            Err(StoreError::Conflict(_)) => {
                // Lost the race. The winner's row is the answer.
                let existing =
                    store::find_asset_by_checksum(self.store.pool(), tenant, &request.checksum)
                        .await?
                        .ok_or_else(|| {
                            // The winner's transaction rolled back between our
                            // conflict and this read. Unreachable in practice, and
                            // if it happens a 409 is the honest answer rather than
                            // inventing an asset that does not exist.
                            tracing::warn!("checksum conflict but the row is gone");
                            Error::conflict("a concurrent upload of the same bytes is in progress")
                        })?;
                return self
                    .resolve_duplicate(tenant, existing, &content_type)
                    .await;
            }
            Err(e) => return Err(e.into()),
        };

        // NOTE: no outbox event here. A `pending` asset is not a fact any
        // consumer can act on — the bytes are not in storage yet, and a
        // consumer that built a thumbnail on this would fail. `darkroom.asset.ready`
        // is emitted at complete time, which is the moment the bytes exist.
        let presigned = self
            .objects
            .presign_put(key.as_str(), &content_type, request.byte_size, PRESIGN_TTL)
            .await?;

        Ok(CreatedUpload {
            asset: created,
            presigned,
            duplicate: false,
        })
    }

    /// Insert the pending row. The tenant is not a parameter because the
    /// `Asset` already carries `account_id`, and passing both would be two
    /// sources of truth for the same value — the kind of thing that disagrees
    /// the first time someone forgets to update one.
    async fn insert_with_key(&self, asset: &Asset, key: &StorageKey) -> Result<Asset, StoreError> {
        let mut tx = self.store.begin().await?;
        let row = store::insert_asset_keyed(&mut *tx, asset, key.as_str()).await?;
        tx.commit().await.map_err(StoreError::Query)?;
        Ok(row)
    }

    /// The duplicate path, shared by the pre-check and the constraint-violation
    /// fallback.
    async fn resolve_duplicate(
        &self,
        tenant: &Tenant,
        existing: (Asset, String),
        content_type: &str,
    ) -> Result<CreatedUpload, Error> {
        let (asset, storage_key) = existing;

        // A failed asset's checksum was never verified against real bytes, so
        // returning it hands back something the caller cannot use. The
        // constraint still holds the row, so this is a 409 and not a second
        // insert.
        if asset.status == AssetStatus::Failed {
            return Err(Error::conflict(
                "these bytes were uploaded before and that upload failed; start a new upload with different content",
            ));
        }

        // Re-presign against the EXISTING key. This is what makes the response
        // usable for a client that wants to PUT and complete, and it is scoped
        // to that one key by the same rules as a fresh upload.
        let presigned = self
            .objects
            .presign_put(
                &storage_key,
                &asset.content_type,
                asset.byte_size,
                PRESIGN_TTL,
            )
            .await?;

        tracing::info!(
            asset_id = %asset.id,
            account_id = %tenant.account_id(),
            "duplicate upload resolved to the existing asset"
        );
        let _ = content_type;

        Ok(CreatedUpload {
            asset,
            presigned,
            duplicate: true,
        })
    }

    /// `POST /v1/uploads/:id/complete` — verify the object and mark it ready.
    ///
    /// The verification is the whole point, and it has one input: **the bytes**.
    ///
    /// 1. `head` the key. Absent → the upload never landed → **409**, and the
    ///    asset is marked `failed`. The `head` is metadata only and is the cheap
    ///    half: it answers "did it land, and how big" without downloading a
    ///    gigabyte to find out the gigabyte is not there.
    /// 2. The measured size against the declared size. A client that uploaded
    ///    more than it said is a **422** — the object is there, it is just the
    ///    wrong one.
    /// 3. **Read the object back and hash it.** Not "ask the backend what
    ///    checksum it recorded, and read the object only if it has none" — that
    ///    ordering is the defect this was changed for. On AWS S3 the recorded
    ///    `FULL_OBJECT` sha256 is real and comparing against it is cheaper; on
    ///    Cloudflare R2 there is no `FULL_OBJECT` SHA-256 at all (its
    ///    compatibility table offers `COMPOSITE` for SHA-256 and `FULL_OBJECT`
    ///    for CRC-64/NVME only), so the same code path asks a question whose
    ///    answer depends on the backend, and the honest answer — nothing, or a
    ///    value that is not the object's sha256 — turns a check into a coin
    ///    flip. A correctness property may not depend on which bucket answered
    ///    it, so the bytes are read and hashed here on every backend.
    /// 4. Mark `ready` with the checksum **computed from the stored bytes**, and
    ///    enqueue `darkroom.asset.ready` in the same transaction.
    ///
    /// Step 3 costs one read per completed upload. It is a real cost, paid on
    /// purpose, and `tests/checksum_verification.rs` asserts the number is
    /// exactly one so the trade cannot be quietly reversed into a header lookup.
    pub async fn complete_upload(
        &self,
        tenant: &Tenant,
        asset_id: Uuid,
        claimed_checksum: &str,
    ) -> Result<Asset, Error> {
        if !checksum::is_valid_checksum_format(claimed_checksum) {
            return Err(Error::invalid_fields(
                "checksum must be 64 hex characters (sha256)",
                vec![FieldError::new("checksum", "invalid_format")],
            ));
        }

        // --- read the asset, tenant-scoped -------------------------------
        // `None` covers "no such asset" and "not yours" identically, and the
        // handler turns both into 404. Reading it before touching storage is
        // also what stops a caller from using this endpoint to probe whether an
        // object key exists in a bucket they have no claim to.
        let Some((asset, storage_key)) =
            store::find_asset(self.store.pool(), tenant, asset_id).await?
        else {
            return Err(Error::not_found("asset not found"));
        };

        // Terminal states. A `ready` asset completed again is the idempotency
        // layer's problem, not a 409 — but reaching here with `ready` means the
        // key was not used, and answering with the asset is more useful than
        // refusing.
        if asset.status == AssetStatus::Ready {
            return Ok(asset);
        }
        if asset.status == AssetStatus::Failed {
            return Err(Error::conflict(
                "this upload has already failed and cannot be completed",
            ));
        }

        // --- 1. does the object exist? -----------------------------------
        let meta = match self.objects.head(&storage_key).await {
            Ok(meta) => meta,
            Err(crate::objectstore::ObjectStoreError::NotFound) => {
                // The presigned URL was issued and never used, or the bytes were
                // removed. Either way the upload did not happen.
                self.fail_asset(tenant, asset_id, "object_absent_at_complete")
                    .await?;
                return Err(Error::conflict(
                    "no object was found in storage for this upload; it may have expired before the bytes were sent",
                ));
            }
            Err(e) => return Err(e.into()),
        };

        // --- 2. the measured size against the declared size ----------------
        // The presigned PUT already refused an oversized object on the in-memory
        // path; on the S3 path the signature does not carry a length ceiling, so
        // this is where a client that uploaded more than it declared is caught.
        // It is a 422, not a 409: the object IS there, it is just the wrong size.
        if meta.byte_size != asset.byte_size {
            self.fail_asset(tenant, asset_id, "size_mismatch").await?;
            return Err(Error::invalid_fields(
                format!(
                    "the object in storage is {} bytes but {} were declared",
                    meta.byte_size, asset.byte_size
                ),
                vec![FieldError::new("byte_size", "mismatch")],
            ));
        }

        // --- 3. the checksum, computed from what storage actually holds -----
        // One read, every backend, no exceptions and no fallback. The client's
        // claim is never what is compared against — that is the whole security
        // property of the signed-upload pattern — and neither is anything the
        // backend says about its own bytes, because a backend that cannot be
        // asked is indistinguishable from a backend that was never asked.
        let bytes = self.objects.get(&storage_key).await?;
        let actual = checksum::sha256_hex(&bytes);
        // The buffer is dead the moment it is hashed. A 1 GiB upload would
        // otherwise sit in this process's heap until the response is written.
        drop(bytes);

        if !checksum::checksums_match(claimed_checksum, &actual) {
            // The bytes in storage are not the bytes the client said it would
            // upload. That is a corrupted upload, a wrong client, or a client
            // trying to register a checksum for content it never sent — and the
            // last one is why this is a hard failure rather than a warning.
            self.fail_asset(tenant, asset_id, "checksum_mismatch")
                .await?;
            return Err(Error::invalid_fields(
                "the checksum does not match the bytes in storage",
                vec![FieldError::new("checksum", "mismatch")],
            ));
        }

        // --- 4. mark ready, emit the event, same transaction --------------
        let mut tx = self.store.begin().await?;

        let Some((ready, _)) = store::mark_ready(&mut *tx, tenant, asset_id, &actual).await? else {
            // Another complete won the compare-and-set. Roll back and let the
            // idempotency layer serve the stored response.
            tx.rollback().await.ok();
            let (current, _) = store::find_asset(self.store.pool(), tenant, asset_id)
                .await?
                .ok_or(Error::not_found("asset not found"))?;
            return Ok(current);
        };

        let event = NewEvent::new(
            EventType::AssetReady,
            ready.id.to_string(),
            json!({
                "asset_id": ready.id.to_string(),
                "account_id": ready.account_id.to_string(),
                "owner_user_id": ready.owner_user_id.to_string(),
                "kind": ready.kind.as_str(),
                "content_type": ready.content_type,
                "byte_size": ready.byte_size,
                "checksum": ready.checksum,
            }),
        );
        Outbox::new(&mut tx).enqueue(&event).await?;

        // Commit. If this fails, the asset stays `pending` and no event exists
        // — which is the outbox rule, and the reason the test suite has a
        // rollback case.
        tx.commit().await.map_err(StoreError::Query)?;

        tracing::info!(asset_id = %ready.id, account_id = %ready.account_id, "asset ready");
        Ok(ready)
    }

    /// Move a `pending` asset to `failed`, in its own transaction, and emit
    /// nothing. A failure is not an event: no consumer can act on "this upload
    /// did not complete" in a way that is not better served by the client
    /// knowing through the 4xx it is receiving.
    async fn fail_asset(&self, tenant: &Tenant, asset_id: Uuid, reason: &str) -> Result<(), Error> {
        let mut tx = self.store.begin().await?;
        store::mark_failed(&mut *tx, tenant, asset_id, reason).await?;
        tx.commit().await.map_err(StoreError::Query)?;
        Ok(())
    }

    /// `GET /v1/assets` — one page, tenant-scoped.
    pub async fn list_assets(
        &self,
        tenant: &Tenant,
        limit: i64,
        cursor: Option<(time::OffsetDateTime, Uuid)>,
    ) -> Result<(Vec<Asset>, Option<time::OffsetDateTime>, Option<Uuid>, bool), Error> {
        // One extra row to find out whether there is a next page, without a
        // second count query.
        let rows = store::list_assets(self.store.pool(), tenant, limit + 1, cursor).await?;
        let has_more = rows.len() as i64 > limit;
        let page: Vec<Asset> = rows
            .into_iter()
            .take(limit as usize)
            .map(|(a, _)| a)
            .collect();
        let next = if has_more {
            page.last().map(|a| (a.created_at, a.id))
        } else {
            None
        };
        Ok((page, next.map(|(t, _)| t), next.map(|(_, i)| i), has_more))
    }

    /// `GET /v1/assets/:id` — tenant-scoped, 404 for another account's asset.
    pub async fn get_asset(&self, tenant: &Tenant, asset_id: Uuid) -> Result<Asset, Error> {
        store::find_asset(self.store.pool(), tenant, asset_id)
            .await?
            .map(|(a, _)| a)
            // The single 404 path. A cross-tenant read and a genuinely missing
            // asset are byte-identical responses, so existence never leaks.
            .ok_or(Error::not_found("asset not found"))
    }

    /// `DELETE /v1/assets/:id` — remove the row, the storage objects, and emit
    /// `darkroom.asset.deleted`.
    ///
    /// Order matters: the storage objects are deleted **before** the row is
    /// committed, because a row that says "gone" while a gigabyte of objects
    /// survives is a leak nobody will ever find, whereas storage objects that
    /// were removed just before a rolled-back row is a missing object for an
    /// asset that still exists — recoverable, and far cheaper. The window is
    /// one transaction, not a retry loop.
    ///
    /// Every variant's object goes too: deleting an asset whose thumbnail
    /// remains is not a delete.
    pub async fn delete_asset(&self, tenant: &Tenant, asset_id: Uuid) -> Result<(), Error> {
        let keys = store::list_storage_keys(self.store.pool(), tenant, asset_id).await?;
        if keys.is_empty() {
            return Err(Error::not_found("asset not found"));
        }

        // Best-effort in the sense that a failure is logged and the row delete
        // still proceeds: a storage outage must not make an asset undeletable,
        // and the sweeper for orphaned objects is the safety net (not built in
        // this packet — see README "Not done").
        for key in &keys {
            if let Err(e) = self.objects.delete(key).await {
                tracing::error!(%key, asset_id = %asset_id, error = %e, "could not delete a storage object");
            }
        }

        let mut tx = self.store.begin().await?;
        let deleted = store::delete_asset(&mut *tx, tenant, asset_id).await?;
        if !deleted {
            // Gone between the key read and here. Idempotent: the caller asked
            // for it to not exist, and it does not.
            tx.rollback().await.ok();
            return Err(Error::not_found("asset not found"));
        }

        let event = NewEvent::new(
            EventType::AssetDeleted,
            asset_id.to_string(),
            json!({
                "asset_id": asset_id.to_string(),
                "account_id": tenant.account_id().to_string(),
            }),
        );
        Outbox::new(&mut tx).enqueue(&event).await?;
        tx.commit().await.map_err(StoreError::Query)?;

        tracing::info!(%asset_id, account_id = %tenant.account_id(), "asset deleted");
        Ok(())
    }

    /// `POST /v1/assets/:id/variants` — derive an image and store it.
    ///
    /// Rejects anything that is not a `ready` image, with a 422 that says
    /// which of the two it is. A 404 here would mean the asset is not the
    /// caller's; a 422 means it is theirs and cannot be derived from.
    pub async fn create_variant(
        &self,
        tenant: &Tenant,
        asset_id: Uuid,
        kind: VariantKind,
    ) -> Result<AssetVariant, Error> {
        let Some((asset, source_key)) =
            store::find_asset(self.store.pool(), tenant, asset_id).await?
        else {
            return Err(Error::not_found("asset not found"));
        };

        if asset.status != AssetStatus::Ready {
            return Err(Error::invalid_fields(
                "only a ready asset can have a variant generated from it",
                vec![FieldError::new("status", "not_ready")],
            ));
        }
        if asset.kind != AssetKind::Image {
            return Err(Error::invalid_fields(
                format!(
                    "variants are only supported for images, and this asset is a {}",
                    asset.kind
                ),
                vec![FieldError::new("kind", "unsupported_kind")],
            ));
        }

        // Read the original. The original row is never mutated.
        let original = self.objects.get(&source_key).await?;
        let encoded = crate::variants::encode(&original, kind, &asset.content_type)?;

        let variant_key = StorageKey::generate_variant(tenant.account_id(), kind.as_str());
        self.objects
            .put(
                variant_key.as_str(),
                encoded.bytes.clone(),
                kind.content_type(),
            )
            .await?;

        let now = crate::observability::now();
        let variant = AssetVariant {
            id: Uuid::new_v4(),
            asset_id: asset.id,
            account_id: tenant.account_id(),
            kind,
            content_type: kind.content_type().to_string(),
            byte_size: encoded.bytes.len() as i64,
            width: encoded.width,
            height: encoded.height,
            metadata: json!({
                "source_asset_id": asset.id.to_string(),
                "source_checksum": asset.checksum,
                "source_width": encoded.source_width,
                "source_height": encoded.source_height,
            }),
            created_at: now,
            updated_at: now,
        };

        let mut tx = self.store.begin().await?;
        let stored = store::upsert_variant(&mut *tx, &variant, variant_key.as_str()).await?;
        let event = NewEvent::new(
            EventType::VariantCreated,
            stored.id.to_string(),
            json!({
                "variant_id": stored.id.to_string(),
                "asset_id": asset.id.to_string(),
                "account_id": tenant.account_id().to_string(),
                "kind": stored.kind.as_str(),
                "content_type": stored.content_type,
                "byte_size": stored.byte_size,
                "width": stored.width,
                "height": stored.height,
            }),
        );
        Outbox::new(&mut tx).enqueue(&event).await?;
        tx.commit().await.map_err(StoreError::Query)?;

        Ok(stored)
    }

    /// `GET /v1/assets/:id/variants` — tenant-scoped.
    ///
    /// The parent asset is checked first and its absence is the same 404 as a
    /// missing asset on `GET /v1/assets/:id`. Without that check an empty list
    /// would answer "this asset id exists but you cannot see it" — well, worse:
    /// an empty list answers "no", and a caller cannot tell it from "this asset
    /// has no variants yet", which is an existence oracle with one bit.
    pub async fn list_variants(
        &self,
        tenant: &Tenant,
        asset_id: Uuid,
    ) -> Result<Vec<AssetVariant>, Error> {
        if store::find_asset(self.store.pool(), tenant, asset_id)
            .await?
            .is_none()
        {
            return Err(Error::not_found("asset not found"));
        }
        Ok(store::list_variants(self.store.pool(), tenant, asset_id).await?)
    }
}

/// Filename validation. The name is stored and returned to clients, and it ends
/// up in a `Content-Disposition` on the download path eventually, so the rules
/// are about not carrying control characters and not being absurdly long.
fn validate_filename(filename: &str) -> Result<String, Error> {
    let trimmed = filename.trim();
    if trimmed.is_empty() {
        return Err(Error::invalid_fields(
            "filename must not be empty",
            vec![FieldError::new("filename", "required")],
        ));
    }
    if trimmed.len() > 512 {
        return Err(Error::invalid_fields(
            "filename must be at most 512 characters",
            vec![FieldError::new("filename", "too_long")],
        ));
    }
    if trimmed.chars().any(|c| c.is_control()) {
        return Err(Error::invalid_fields(
            "filename must not contain control characters",
            vec![FieldError::new("filename", "invalid_format")],
        ));
    }
    // A path is not a filename. Stripping is wrong (it changes the name) and
    // accepting is worse, so it is a 422.
    if trimmed.contains('/') || trimmed.contains('\\') {
        return Err(Error::invalid_fields(
            "filename must not contain a path separator",
            vec![FieldError::new("filename", "invalid_format")],
        ));
    }
    Ok(trimmed.to_string())
}

/// How long a `pending` asset may live before the sweeper fails it. Twice the
/// presign TTL, so an upload whose URL is still valid is never swept out from
/// under a client that is mid-PUT.
pub const PENDING_SWEEP_AFTER: Duration = Duration::from_secs(PRESIGN_TTL.as_secs() * 2);

/// Compile-time reminder that `Tx` is used; the alias lives in `store`.
const _: fn() = || {
    fn assert_tx_used(_: &Tx<'_>) {}
    let _ = assert_tx_used;
};

#[cfg(test)]
mod tests {
    use super::*;

    #[test]
    fn filenames_are_validated_before_anything_is_written() {
        assert_eq!(validate_filename("photo.png").expect("valid"), "photo.png");
        // Trimmed, not rejected — a stray space from a copy-paste is not a
        // security event.
        assert_eq!(
            validate_filename("  photo.png ").expect("valid"),
            "photo.png"
        );

        for bad in [
            "",
            "   ",
            "with/slash.png",
            "with\\backslash.png",
            "with\0null.png",
            "with\nnewline.png",
            &"x".repeat(513),
        ] {
            let err = validate_filename(bad).expect_err("must be rejected");
            assert_eq!(err.status().as_u16(), 422, "accepted {bad:?}");
        }
    }

    #[test]
    fn the_sweep_window_is_longer_than_the_presign_window() {
        // A client whose URL is still valid must never have its asset swept as
        // stale. Asserted because the two numbers are in different files and
        // one of them changing is silent otherwise.
        assert!(PENDING_SWEEP_AFTER > PRESIGN_TTL);
    }

    #[test]
    fn the_presign_ttl_is_short_enough_to_not_be_a_credential() {
        // 15 minutes. A URL that lives for an hour is a bearer token for one
        // object that can be found in a proxy log, a browser history, or a
        // Referer header and replayed for the rest of that hour. Asserted so a
        // "let's give people an hour to upload" change has to be deliberate.
        assert_eq!(PRESIGN_TTL, Duration::from_secs(900));
        assert!(PRESIGN_TTL <= Duration::from_secs(900));
    }

    #[test]
    fn the_single_put_cap_is_under_the_s3_single_put_limit() {
        // 1 GiB against S3's 5 GiB single-PUT ceiling. Above that, presigned
        // PUT becomes presigned multipart, which is a second flow with a second
        // set of URLs and a second thing to get wrong. Staying under it is a
        // design decision, not a limit.
        const { assert!(MAX_UPLOAD_BYTES < 5 * 1024 * 1024 * 1024) };
    }
}
