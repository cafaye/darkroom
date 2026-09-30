# Changelog

All notable changes to `darkroom`. The format follows
[Keep a Changelog](https://keepachangelog.com/en/1.1.0/), and this project
adheres to [Semantic Versioning](https://semver.org/spec/v2.0.0.html).

The **API version** is the `/v1` path prefix. The **document version** is
`info.version` in `openapi/v1.yaml`. They move together on a breaking change
and only `info.version` moves otherwise
([`core/docs/openapi-conventions.md`](../core/docs/openapi-conventions.md)).

## [Unreleased]

### Added

**`gate.yml`: the gate is declared, so it no longer has to be guessed.** What
"run the gate" means in this repository was discoverable only by getting it
wrong: `mise run prime` runs `bin/prime`, CI runs `bin/prime --db`, and the
difference is the entire database tier, because the database tests are
`#[ignore]`d and a bare `./bin/prime` reports them as `ignored`, prints `==> ok`
and exits 0. Nothing in the repository said so in a form a machine or a new
reader could check. `gate.yml` is core's format
([`../core/schemas/gate.schema.json`](../core/schemas/gate.schema.json), checked
by `../core/harness/gate_check.py`) and it states the argv, the mise task, the
entrypoint, the three external requirements with how to satisfy each, the CI
invocation — flag included — and five proofs, four of which are the step headers
that can only appear if all four cargo tiers ran.

`bin/gate-self-test` is the other half: two controls and twenty-three breakages,
each breaking exactly one thing in a throwaway copy and asserting core's checker
goes red **and names the finding it expects**. The four cases the packet names
are in it — an entrypoint that is not a file, a mise task that resolves
somewhere else, a proof pattern that matches nothing the gate prints, a CI
workflow that does not invoke the gate — and so are the false green, the floor,
the timeout, and the drift this declaration exists to record: dropping `--db`
from the declared command is invisible to every static check and is caught only
by running the gate.

**`openapi/v1.yaml` and the router are now held to each other by a test, in
both directions.** `tests/openapi_document.rs` compares the published document
with the routes the service actually serves. An operation the document describes
and the router does not serve is a generated client that 404s in production; a
route the router serves that the document does not describe is a method the
generated client does not have. It follows courier's
`test/courier_web/openapi_document_test.exs`, which is the same tripwire shape
pantry's has fired three times against.

It compares **paths, never counts** — a count comparison passes on a rename and
fails on a pure addition, which is backwards — and both of its readers raise
rather than under-read, because a green check over nothing is worse than no
check.

**The router's side is a route table the router is built from, not a list in a
test.** axum cannot be asked what it routes: there is no `Router::routes()` and
nothing to reflect over, so the set of operations has to be written down
somewhere. It is `http::OPERATIONS` in `src/http.rs` — a table of
`(method, path, handler)` — and `router()` is a fold over it, so the HTTP
surface and the thing the test reads are one declaration.

The one field the table cannot state is the method, because the method a
`MethodRouter` answers is baked into the value `get(handler)` returns with no
accessor for it. That is closed by asking the router: a request with an
undeclared verb is answered `405` with an `Allow` header enumerating the truth,
and a path the router does not know is `404`.

Proven able to fire, not assumed to. Four divergences planted and reverted:
an operation added to the document, a row added to the route table, a `put`
registered on a known path outside the table (caught by the `Allow` probe while
both document tests stayed green), and a `get` on a brand-new path outside the
table (**not** caught — all nine tests stayed green, which is the gap below,
measured rather than argued). `tests/contract.rs` is 8/8 green under a planted
document divergence, so this check is the only thing in the repository that
notices.

`/healthz` and `/readyz` are in the document under a `probes` tag with
`security: []`, so the omission list is empty — courier excludes them, and
excluding them here would mean deleting correct documentation to satisfy a
carve-out. The list is still a closed list keyed by method *and* path rather than
a prefix match, and two tests hold it: one fails if a first omission appears, one
fails if an omission stops naming something the router serves.

Needs neither a database nor a socket, so it is in the default tier and runs on
a bare machine. It reads its subject out of `exposes.api` in `cafaye.yml` rather
than hardcoding a filename.

**`bin/tier-counts` — the gate's own accounting.** `bin/prime --db` exits zero
in three situations where it has verified nothing: tier 6 never ran, tier 6 ran
and touched nothing, and tier 5 never compiled the `s3` feature. None of those
change the exit code, so the counts are what is left to read.

`bin/tier-counts` takes a captured run and asserts 77 unit, 89 with `s3`, 9
R2/S3 behaviour-table rows, and 41 database tests in each feature set — plus one
identity that is not a constant-to-constant comparison: **the count the default
run skips must equal the count the database run passes**, because they are the
same set of tests. A test `#[ignore]`d without the database tier running it
breaks it in one direction; a test added and never ignored breaks it in the
other. Neither reaches master as a green badge. Adding a test means raising the
number in the same commit, which is the point of the constant.

A new file under `tests/` is a separate test binary, so it is in none of those
four constants — the first two count `unittests src/lib.rs`, the third counts
`#[ignore]`d tests, and the fourth is scoped to `storage_backends.rs` — and
raising `UNIT_TESTS` for one would have made that number a lie. Each default-tier
file is now pinned in its own right instead: `OPENAPI_DRIFT_TESTS=9`, asserted in
tiers 4 and 5. Without that pin, a file whose tests were deleted one at a time
would report `0 passed` while every other number still read correctly.

Proven able to fail, not assumed to: six mutations of a real green log, each
producing a specific message and a non-zero exit.

**`rust-toolchain.toml`.** The workflow has promised this file since
darkroom-01. It did not exist, and the `rustup show active-toolchain ||
rustup toolchain install` that stood in for it succeeds on every runner, so the
`||` branch could never fire and the toolchain was whatever the image shipped
that month. Now pinned to `1.95.0` — the same release as `mise.toml`,
`Cargo.toml`'s `rust-version` and the Dockerfile's `RUST_VERSION` — with
`clippy` and `rustfmt` as components and `x86_64-unknown-linux-gnu` as the
target CI compiles on.

### Changed

**`router()` is a fold over `http::OPERATIONS` instead of a chain of
`.route()` calls.** The chain it replaced registered the same nine routes in the
same order, so the served surface is unchanged — the table is the old chain as
data. What changed is that there is now one declaration of the route table which
both the router and `tests/openapi_document.rs` read, instead of a chain in one
file and a route list in a test. There is exactly one line in `src/http.rs` where
a route can be registered by hand, and it is the fold.

**`saphyr` is a dev-dependency**, for reading `openapi/v1.yaml` in that check.
Dev-only, so it cannot reach the binary, the image or a running service. Pure
Rust with no `unsafe` in its tree, where `serde_yaml` is a transpiled C library
and this repository has no `unsafe` and wants none — a C-derived parser arriving
behind a dev-dependency would be the same decision wearing a smaller hat.
`default-features = false` drops `encoding`, because the only YAML this reads is
a UTF-8 file this repository wrote. No runtime dependency was added, so no
README dependency-table entry is owed.

**CI calls `cafaye/kit/.github/workflows/ci.reusable.yml@master`** for the
shared half (`language: rust`, coverage floor 50 against 53.73% measured line
coverage), and keeps two jobs that kit cannot own. `gate` runs `bin/prime --db`
against a `postgres:17-alpine` service — the database tier is the reason this
packet exists, and it cannot run without an environment kit's rust job has no
way to provide. `contract` checks out `cafaye/caf` and runs their tool, which is
a step that reaches into another cafaye repository and is out of kit's scope by
name.

The `build` job is gone. `gate` is a strict superset of it: `bin/prime` already
ran fmt, build, `--all-features` clippy, `cargo test` and
`cargo test --features s3`, and `gate` adds the database tier, the tier
accounting and a `git diff --exit-code Cargo.lock` guard. The global
`RUSTFLAGS: -D warnings` is gone with it — it was set in CI and not locally, and
a gate that is stricter in CI than on a laptop is a gate the two can disagree
about.

**No object-storage credential, and no MinIO service, in CI.** The `s3` tier
needs neither. `tests/storage_backends.rs` presigns with the real SDK and static
dummy credentials; presigning builds a URI and a signature locally and transmits
nothing, so the nine-row table runs offline. A MinIO container would boot a
bucket nothing in the suite speaks to — a green checkmark on nothing. The
limitation is now written down instead: the table proves the request darkroom
would sign, not that R2 accepts it, and the README's manual procedure covers
that half.

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
