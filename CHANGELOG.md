# Changelog

All notable changes to `darkroom`. The format follows
[Keep a Changelog](https://keepachangelog.com/en/1.1.0/), and this project
adheres to [Semantic Versioning](https://semver.org/spec/v2.0.0.html).

The **API version** is the `/v1` path prefix. The **document version** is
`info.version` in `openapi/v1.yaml`. They move together on a breaking change
and only `info.version` moves otherwise
([`core/docs/openapi-conventions.md`](../core/docs/openapi-conventions.md)).

## [Unreleased]

### Changed

**The checksum is verified by reading the object back, on every backend.**
`POST /v1/uploads/{id}/complete` used to ask the backend what checksum it had
recorded — `put_object().checksum_algorithm(SHA256)` on the way in,
`head_object().checksum_mode(ENABLED)` on the way out — and compare the client's
claim against that answer, reading the object only when the backend had none to
give. That answer exists on AWS S3. It does not exist on Cloudflare R2, whose S3
compatibility table offers `FULL_OBJECT` for CRC-64/NVME only and `COMPOSITE`
for SHA-256, so on R2 the `FULL_OBJECT` sha256 never came back and the
correctness property silently stopped being enforced.

`complete` now always reads the object and hashes it, and `ObjectStore` never
asks a backend for a checksum. This costs one read per completed upload and
buys a verification that does not depend on which bucket answered; the count is
pinned at exactly one by a test so the trade cannot be reversed quietly.

The wire also changes, deliberately:

- The presigned PUT no longer carries `x-amz-checksum-algorithm` in its signed
  headers, so `curl -T file "$upload_url"` works again. A client no longer has to
  send a checksum header to use the URL.
- The S3 client sets `RequestChecksumCalculation::WhenRequired` and
  `ResponseChecksumValidation::WhenRequired`, so the SDK stops attaching
  CRC-32 — and CRC-64/NVME in current AWS SDK releases, which R2 rejects — to
  `PutObject` and `UploadPart` that nobody asked about.

`aws-sdk-s3` is now pinned to `=1.150.0`, with the reason in `Cargo.toml`.

### Removed

**`ObjectMeta::checksum`.** A library-type contract change, not a wire change:
the OpenAPI document is untouched and `info.version` does not move. The field
was the sha256 "the backend itself recorded", and a field that is `Some` on one
backend and `None` on another is a field whose absence silently disables a
check. Removing it makes the regression unexpressible rather than merely
discouraged. `objectstore::base64_to_hex` went with it, and the in-memory
store's stored checksum.

### Added

**Cloudflare R2 as configuration.** One `ObjectStore` implementation, one
client, two shapes of the same five operations. An R2 endpoint resolves to
region `auto` (with `us-east-1` and the empty value normalised to it), forces
path-style addressing, and **refuses startup** if paired with a real region or
if `auto` is configured without an endpoint — both with a message naming the
variable and the fix. `Config::describe()` reports `r2` rather than `s3` so the
startup log says which bucket is being signed for.

`tests/storage_backends.rs` runs one presign assertion body over AWS S3, an
S3-compatible endpoint and three R2 configurations, and asserts the SigV4
credential scope, the signed-header list, and the absence of every header R2
rejects. The startup refusals are asserted against the real binary.

`bin/prime` gains a `cargo test --features s3` tier: the default `cargo test` does
not compile the feature at all, so without it the R2 tests were decoration.

## [0.1.0] — 2026-09-30

First cut. The foundation anychat's voice clips and anymark's attachments both
sit on: signed uploads, a tenant-scoped asset registry, derived variants.

### Added

**The upload flow.** `POST /v1/uploads` creates a `pending` asset and returns a
presigned PUT; the client writes bytes straight to object storage;
`POST /v1/uploads/{id}/complete` verifies the object and marks it `ready`. The
bytes never pass through the service.

- Presigned URLs are **short-lived (900 s)** and scoped on three axes: one
  storage key, one content type, one maximum byte size.
- The client's checksum is a **claim** at create and is **verified against what
  storage holds** at complete. The row ends up carrying the computed value.
- Failure paths: no object at complete → `409`, checksum mismatch → `422`,
  malformed checksum → `422` at create with nothing written. `ready` and
  `failed` are both terminal.

**The asset registry.** `GET /v1/assets` (cursor-paginated), `GET
/v1/assets/{id}`, `DELETE /v1/assets/{id}` (204; the storage objects and every
variant's are removed with the row). `UNIQUE (account_id, checksum)` holds.

**Duplicate uploads resolve to the existing asset** with `201` and
`X-Darkroom-Duplicate: true`, not a `409` — the client gets a usable asset and
a working URL either way, and it is still one row and one object. The
exception is a duplicate of a *failed* asset, which is a `409`: its checksum
was never verified, and the constraint still holds the row.

**Tenant isolation.** Every query filters on `account_id` from the verified
token; `Tenant` has no constructor that accepts one from a request. A
cross-tenant read is a `404` with a body byte-identical to the one for an id
that never existed.

**Variants.** `POST /v1/assets/{id}/variants` with `thumbnail` (256 px),
`preview` (1024 px) and `web` (re-encode, no resize). Never upscales; the
original row and object are never touched. All JPEG.

**Events.** `darkroom.asset.ready`, `darkroom.asset.deleted`,
`darkroom.variant.created` — each inserted into `outbox_events` in the same
transaction as the state change it describes.

**Idempotency.** `Idempotency-Key` on both POSTs, scoped to
`(endpoint, principal, key)`. Same key and body replays the original response
with `Idempotency-Replayed: true`; a different body is `409
idempotency_key_reused`; a failed handler releases its reservation.

**Probes.** `/healthz` never touches a dependency. `/readyz` really queries the
database. Both are exempt from authentication.

**Storage behind a trait.** `ObjectStore` with an in-memory implementation
(always compiled, really signed and really scoped) and an S3 one behind
`--features s3`. The default build cannot construct a real object-storage
client, so "tests never hit the network" is a property of the dependency graph.

**Contract.** `cafaye.yml` (validates against core's manifest schema),
`openapi/v1.yaml`, three plain-SQL migrations, and `AGENTS.md`.

### Notes for the reviewer

- **Event types are `darkroom.*`, not the brief's `media.*`.** core requires
  the first segment to be the publishing service's own name, and
  `caf contract lint` enforces it. A rename is a new type, never a repurpose,
  so these are the names from the start.
- **Variants are JPEG, not WebP.** The `image` crate's WebP encoder is
  lossless-only — its own docs say so and point at libwebp for lossy. Lossy
  WebP is a dependency decision and is deliberately deferred rather than taken
  silently.
- **The outbox publisher loop is not here.** The table and the insert path are,
  and the rollback case is tested. A loop that cannot publish is worse than an
  absent one.
- **Toolchain is 1.95, not kit's placeholder 1.84.** kit's template says its
  numbers are placeholders and that a service pins what it deploys on.

[Unreleased]: https://github.com/cafaye/darkroom/compare/v0.1.0...HEAD
[0.1.0]: https://github.com/cafaye/darkroom/releases/tag/v0.1.0
