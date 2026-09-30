-- 0001_assets.sql — darkroom's own tables.
--
-- DATABASE-PER-SERVICE. There is no foreign key to identity's `accounts` or
-- `users`, and no join into another service's database, ever. `account_id` and
-- `owner_user_id` are opaque UUIDs that darkroom trusts identity to have
-- minted; it does not verify they exist and it does not care. A cross-service
-- FK is a distributed constraint wearing a database constraint's clothes: it
-- couples release order, it makes one service's migration another service's
-- outage, and it cannot be enforced across two connections anyway.
--
-- The tenant boundary is `account_id`, and it is in the PRIMARY KEY scope of
-- the unique constraint below, so isolation is enforced by the database rather
-- than by remembering a WHERE clause.

create table if not exists assets (
  id                  uuid        primary key,
  account_id          uuid        not null,
  owner_user_id       uuid        not null,

  -- What the uploader said it was sending. `kind` is derived from content_type
  -- at insert time, never client-supplied: a client that could choose `kind`
  -- could get a video variant request routed through the image decoder.
  kind                text        not null,
  original_filename   text        not null,
  content_type        text        not null,
  byte_size           bigint      not null,
  -- sha256 of the bytes as they were actually stored, lowercase hex. Set at
  -- insert from the client's claim, then REPLACED by the value computed from
  -- the stored object at complete time. It is never trusted unverified.
  checksum            text        not null,

  -- The object storage key. Opaque: it is generated from the asset id and a
  -- server-side random component and is never parsed back out of a path. A
  -- client that can choose its own key can overwrite another tenant's object
  -- or write outside its own prefix, so it never gets the choice.
  storage_key         text        not null,

  status              text        not null default 'pending',
  metadata            jsonb       not null default '{}'::jsonb,

  created_at          timestamptz not null default now(),
  updated_at          timestamptz not null default now(),

  constraint assets_kind_check
    check (kind in ('image', 'audio', 'video', 'document')),
  constraint assets_status_check
    check (status in ('pending', 'ready', 'failed')),
  constraint assets_byte_size_check
    check (byte_size >= 0),
  -- The same bytes for the same account are one asset, not N. See README
  -- "Duplicate uploads" for why the loser gets the winner's row back.
  constraint assets_account_checksum_uniq
    unique (account_id, checksum)
);

-- The tenant-scoped listing query orders by (created_at desc, id) and filters on
-- account_id, so the index carries account_id first. Without it every list is
-- a sequential scan of every asset the service has ever stored.
create index if not exists assets_account_created_idx
  on assets (account_id, created_at desc, id desc);

-- storage_key is unique so a presigned URL is scoped to exactly one object: two
-- assets can never share a key, and the presign path can assert it.
create unique index if not exists assets_storage_key_uniq
  on assets (storage_key);

-- Derived records. The original row is never mutated by variant generation.
create table if not exists asset_variants (
  id            uuid        primary key,
  asset_id      uuid        not null references assets (id) on delete cascade,
  account_id    uuid        not null,
  -- The tenant is denormalised onto the variant so every query on the variants
  -- table is account-scoped by the same single index path as `assets`. Without
  -- it, listing variants is an unindexed filter on a denormalisation that only
  -- exists to make the join free.
  kind          text        not null,
  content_type  text        not null,
  byte_size     bigint      not null,
  storage_key   text        not null,
  -- Width/height of the DERIVED image. null for a non-visual variant.
  width         int         null,
  height        int         null,
  metadata      jsonb       not null default '{}'::jsonb,
  created_at    timestamptz not null default now(),
  updated_at    timestamptz not null default now(),

  constraint asset_variants_kind_check
    check (kind in ('thumbnail', 'preview', 'web')),
  -- One variant of each kind per asset: re-requesting a kind replaces it
  -- rather than accumulating rows nobody will ever read.
  constraint asset_variants_asset_kind_uniq
    unique (asset_id, kind)
);

create index if not exists asset_variants_account_created_idx
  on asset_variants (account_id, created_at desc, id desc);

create unique index if not exists asset_variants_storage_key_uniq
  on asset_variants (storage_key);
