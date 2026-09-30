# Changelog

All notable changes to `darkroom`. The format follows
[Keep a Changelog](https://keepachangelog.com/en/1.1.0/), and this project
adheres to [Semantic Versioning](https://semver.org/spec/v2.0.0.html).

The **API version** is the `/v1` path prefix. The **document version** is
`info.version` in `openapi/v1.yaml`. They move together on a breaking change
and only `info.version` moves otherwise
([`core/docs/openapi-conventions.md`](../core/docs/openapi-conventions.md)).

## [Unreleased]

Nothing yet.

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
