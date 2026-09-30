# darkroom

The cafaye media service. Signed uploads, a tenant-scoped asset registry, and
derived variants — the foundation anychat's voice clips and anymark's
attachments both sit on.

One static Rust binary. It owns its own database (PLAN.md §7,
database-per-service) and never queries identity's or billing's.

- **Contract**: [`openapi/v1.yaml`](openapi/v1.yaml) — what `exposes.api` in
  [`cafaye.yml`](cafaye.yml) points at
- **Events**: `darkroom.asset.ready`, `darkroom.asset.deleted`,
  `darkroom.variant.created`
- **Conventions**: [`moon/cafaye/core/docs`](../core/docs) is normative. Where
  this repository and core disagree, core is right and this is the bug.

## Why axum

`axum` over `actix-web` because its `FromRequestParts` extractor makes the
tenant context a *type* rather than an option: a handler takes `Tenant` and
cannot compile without one, where actix's `web::Data`/extension pattern leaves
"did this handler read the authenticated principal?" as a review question.

## The upload flow, and why it is three calls

```
POST /v1/uploads ──▶ 201 { asset: pending, upload_url, storage_key, expires_in_secs }
                            │
   client PUTs bytes ───────┘   straight to object storage
                            │
POST /v1/uploads/{id}/complete ──▶ 200 { asset: ready }
                                   or 409 (object absent) / 422 (checksum mismatch)
```

The bytes never pass through this service. That is the entire point: a 1 GiB
upload is one request to the bucket, not a stream through an API process that
has to buffer it, spool it to disk, and time out on a slow client. Two tests
assert it — `the_bytes_never_pass_through_the_service` and the `StoreStats`
counters behind it.

### The presigned URL

| | |
|---|---|
| **TTL** | **900 seconds (15 minutes)**, in `objectstore::PRESIGN_TTL` |
| **Scope** | exactly one storage key, one content type, one maximum byte size |
| **Signature** | SigV4 presigning (S3) / a signed `(key, expiry)` pair (in-memory) |

Fifteen minutes rather than an hour because a presigned URL is a bearer token
for one object, and an hour is long enough for it to be found in a proxy log, a
browser history, or a `Referer` header and replayed for the rest of that hour.
The three axes of scope are all load-bearing: a URL that permits any key permits
overwriting another tenant's object; a URL with no length cap is a way to write
a 10 GiB file into a bucket someone pays for; a URL with no content-type scope
lets an HTML document be stored at an image's key.

`tests/signed_upload.rs` asserts all three, including that asset A's URL cannot
be used to write at asset B's key.

### The checksum is verified, not trusted

The client sends the sha256 at step 1. That value is a **claim**: it is written
to the `assets` row as `pending` and is never what `complete` compares against.
At step 3 the service computes sha256 over what storage actually holds and
writes *that* to the row.

A service that trusted the claim would let a client register an asset whose
checksum describes bytes it never uploaded — and that checksum is what every
downstream dedupe and every future integrity check is keyed on.

| Situation | Status | Asset ends up |
|---|---|---|
| Bytes arrived, checksum matches | `200` | `ready` |
| No object at complete time | `409` | `failed` |
| Checksum does not match the stored bytes | `422` | `failed` |
| Malformed checksum (not 64 hex) | `422` at **create** | nothing written |

`ready` and `failed` are both terminal. A failed upload is not retried into
`ready`; the client creates a new one. That keeps `UNIQUE (account_id,
checksum)` meaningful — a checksum that failed once cannot be quietly retried
into a second attempt — and it means "pending older than the presign TTL" is a
sweepable set with exactly one meaning.

## Duplicate uploads: the existing asset, 201, not 409

`UNIQUE (account_id, checksum)` holds. When the insert collides, `POST
/v1/uploads` returns **the existing asset**, with a fresh presigned URL for
*its* key, `201`, and `X-Darkroom-Duplicate: true`.

The reasoning, in the order that decided it:

1. **A 409 makes the client solve a problem it did not create.** From the
   client's side the two uploads are unrelated: a user attaching the same
   screenshot to two messages, an importer retrying a directory where one file
   was already done. A 409 means every such client implements "on conflict,
   fetch the existing asset and carry on" — and every client that has not yet
   looks like it is failing.
2. **The bytes are already there and already verified.** The existing asset
   reached `ready`, which means its checksum was checked against what storage
   holds. Handing it back gives the caller a usable asset with no second upload
   and no second copy of a file that may be a gigabyte.
3. **Re-presigning makes the response usable.** The caller gets a working URL,
   so a client that just wants to PUT and complete does the same three calls
   either way. Nothing has to know it was a duplicate.
4. **It is still one asset** — one row, one object, one bill. That is what the
   constraint was for.

What it is not is silent: the marker header and the `duplicate` body field let
a client say "already uploaded" to a user, and the log line records it.

**The one case that is a 409:** a duplicate of an asset that previously
**failed**. Its checksum was never verified against real bytes, so returning it
hands back something unusable — and the constraint still holds the row, so it
cannot become a second asset either. 409 with a message saying the bytes
previously failed is the honest answer.

**Across accounts it is two assets.** The constraint is
`(account_id, checksum)`, not `checksum` alone; a shared checksum between
tenants must not collapse into one row, or one tenant could learn from a count
that another uploaded something.

## Tenant isolation

`account_id` comes from the verified token and from nowhere else. There is no
account field in any request body — `CreateUploadBody` does not have one, so
serde drops it, and `a_create_body_cannot_name_an_account` is the regression
guard for the day someone adds one.

Two things make this structural rather than a convention repeated in eleven
places by hand:

- `auth::Tenant` has exactly one constructor, `from_principal`. There is no
  `Tenant::from_request`, so a handler physically cannot scope a query to an
  account the caller named.
- Every repository function takes a `Tenant` (or a literal `account_id` in the
  `where` clause) and puts it in the query. A repository method that forgot
  would not typecheck against `Tenant`.

**A cross-tenant read is 404, never 403**, and the body is byte-identical to the
one for an id that never existed. A 403 would be a free asset-id oracle: a
caller enumerates ids, gets 403 for the ones that exist and 404 for the ones
that don't, and has a directory of every asset on the platform. Same reasoning
for `GET /v1/assets/{id}/variants`, which returns 404 rather than an empty list
— an empty list is indistinguishable from "no variants yet", which is an
existence oracle with one bit.

`tests/tenant_isolation.rs` is the matrix; the exact statuses are in the report.

## The storage boundary

One trait, `objectstore::ObjectStore`, with five methods:

```rust
async fn presign_put(&self, key, content_type, max_bytes, ttl) -> Result<PresignedPut>;
async fn head(&self, key)                      -> Result<ObjectMeta>;
async fn get(&self, key)                       -> Result<Bytes>;
async fn put(&self, key, bytes, content_type)  -> Result<()>;
async fn delete(&self, key)                    -> Result<()>;
```

Everything above the line is policy — tenancy, checksum verification, status
transitions, the outbox. Everything below is transport. There is no "if s3
then" above the line and no policy below it.

Two implementations:

- **`InMemoryObjectStore`** — always compiled, used by every test and by
  `DARKROOM_OBJECT_STORE=memory`. Its presigned URLs are really signed and
  really scoped: `apply_presigned_put` verifies the signature, the key, the
  content type, the length cap and the expiry, and refuses anything outside
  them. A fake that merely "worked" would let a bug in scope or TTL ship,
  because nothing in the test would notice.
- **`S3ObjectStore`** — compiled only with `--features s3`, which is the
  deployment image's build. The AWS SDK is not a default dependency at all, so
  the default build **cannot construct a real object-storage client**. That is
  what makes "tests never hit the network" a property of the dependency graph
  rather than a promise in a document.

Client uploads go through the presigned URL, never through `put` — `put` is the
service's own path, used by variant generation to store a derived image. A test
asserts the client's bytes do not increment it.

## Variants

`POST /v1/assets/{id}/variants` with `kind`:

| kind | bounded to | notes |
|---|---|---|
| `thumbnail` | longest edge 256px | |
| `preview` | longest edge 1024px | |
| `web` | original dimensions | converts without resizing |

One bound, not two, because there is no correct answer to "which axis wins" for
a 4000×100 panorama — one number means a panorama and a portrait both come out
bounded by their longest edge. **Never upscales**: a 100×80 image asked for as a
256px thumbnail stays 100×80, because upscaling costs bytes, adds no
information, and produces a thumbnail bigger than its source.

Every variant is **JPEG**. WebP would be better and is deliberately not used:
the `image` crate's WebP encoder is **lossless-only** — its own docs say so and
point at libwebp for lossy — and a lossless re-encode of a 256px thumbnail
saves little enough to not be worth a C dependency and a second codec's failure
modes. `image`'s JPEG encoder is lossy, pure Rust, and decodable by everything
that will ever request a thumbnail. **This is the one place a dependency
decision is deferred**; it is called out in the report.

Alpha is composited over white explicitly, in `u16`. `DynamicImage::to_rgb8`
*drops* the alpha channel rather than compositing it, so a transparent black
pixel would stay black in the served image. A test caught exactly that.

The original row and its object are never touched. `variant_created_is_emitted_
and_the_original_is_untouched` asserts the original's checksum, byte size and
`updated_at` are all unchanged after a variant is derived.

## Events and the outbox

| type | subject | when |
|---|---|---|
| `darkroom.asset.ready` | the asset | Complete verified the bytes. **Not** at create. |
| `darkroom.asset.deleted` | the asset | The row and the storage objects are gone. |
| `darkroom.variant.created` | the **variant** | A derived image is readable. |

Three segments, publisher-prefixed, past tense, per
`core/docs/event-naming.md`. The first segment is `darkroom` because core
requires the publisher's own name — the brief asked for `media.asset.*` and
`caf contract lint` rejects it. `media` is the domain; `darkroom` is the
service. A rename is a new type, so these are the names from the start.

There is exactly one way to write an event: `Outbox::enqueue`, which takes a
`&mut Transaction`. A function that holds a transaction cannot publish outside
it, and a function that does not hold one has no way to publish at all. **No
event is emitted at `pending`** — a consumer that built a thumbnail on a
`pending` asset would find no bytes, and a consumer that notified a user would
be telling them about an upload that has not landed.

`a_rolled_back_transaction_emits_nothing` writes a domain row and an event in
one transaction, rolls it back, and asserts **both** tables are empty. Asserting
only the domain row would still pass if the event were written outside the
transaction.

**The publisher loop is not in this packet.** It needs a NATS client and a
deployment, neither of which exists yet, and a publisher loop that cannot
publish is worse than an absent one. The table and the insert path — the half
that carries the correctness guarantee — are here and tested.

## Idempotency

`POST /v1/uploads` and `POST /v1/uploads/{id}/complete` honour
`Idempotency-Key`, scoped to `(endpoint, principal, key)`.

| Case | Result |
|---|---|
| Same key, same body | The original response, `Idempotency-Replayed: true` |
| Same key, different body | `409 idempotency_key_reused` |
| Same key, different principal | A different key — a real create, not a replay |
| Concurrent, same key, first still running | `409 conflict` — not a fabricated empty replay |
| No key | Processed normally |

The ledger is a table, not a cache: the scope includes the principal and the
request may land on any replica, and "idempotent for a retry that hits the same
box" is the worst possible property for something whose purpose is surviving a
timeout. A failed handler **releases** its reservation, so a client that got a
503 and retried gets a real attempt rather than a permanent 409.

`mark_ready` is also a compare-and-set (`and status = 'pending'`), so two
concurrent completes cannot both win and the second emits no second
`darkroom.asset.ready`.

## Endpoints

| Method | Path | Scope | |
|---|---|---|---|
| `GET` | `/healthz` | none | Liveness. Never touches a dependency. |
| `GET` | `/readyz` | none | Readiness. Really runs `select 1`. |
| `POST` | `/v1/uploads` | `assets:write` | Idempotent. 201 + presigned URL. |
| `POST` | `/v1/uploads/{id}/complete` | `assets:write` | Idempotent. 200 / 409 / 422. |
| `GET` | `/v1/assets` | `assets:read` | Cursor-paginated. |
| `GET` | `/v1/assets/{id}` | `assets:read` | |
| `DELETE` | `/v1/assets/{id}` | `assets:write` | 204. Objects removed. |
| `POST` | `/v1/assets/{id}/variants` | `assets:write` | 201. |
| `GET` | `/v1/assets/{id}/variants` | `assets:read` | |

`/healthz` and `/readyz` are exempt from authentication by an explicit path
allow-list, **not** by route order — axum's `Router::layer` applies to every
route the router holds, so registering the probes "before" the auth layer does
not exempt them. A `/healthz` behind the auth middleware returns 401, an
orchestrator marks every instance unhealthy, and the deployment rolls back with
no indication of why. A test caught exactly this.

## Storage keys

```
a/{account_id}/{128 random bits}/original
a/{account_id}/{128 random bits}/variant-{kind}
```

Never client-supplied, and the reason is not tidiness: a client-chosen key
collides across tenants, a client-chosen key traverses (`../../../other`), and
a predictable key is a capability — if the key were `{account_id}/{asset_id}`,
anyone who guessed an asset id could presign a PUT over the *original*. The
randomness is what makes the presigned URL the only way to write there. The
account prefix is for bucket lifecycle rules, not authorisation.

`Asset` has no `storage_key` field. A client that can read it will eventually
try to build a URL from it.

## Running it

```sh
mise install
cp .env.example .env          # no secrets in this repo
docker compose up -d postgres
psql "$DATABASE_URL" -f migrations/0001_assets.sql
psql "$DATABASE_URL" -f migrations/0002_outbox_events.sql
psql "$DATABASE_URL" -f migrations/0003_idempotency_keys.sql

cargo run                                     # in-memory object store
cargo run --features dev-auth                 # + the HMAC token verifier
```

Migrations are a **deploy step, not a boot step**. Nothing in `main` applies
one, because two replicas racing to deploy would deadlock on `CREATE TABLE`.

## Tests

```sh
./bin/prime          # fmt, build, clippy, test — no database needed
./bin/prime --db     # + the ignored tests, against TEST_DATABASE_URL
```

`cargo test` alone is green on a bare machine with no Postgres and no Docker.
The database tests are `#[ignore]`d, and **a skipped test proves nothing** —
which is why `bin/prime --db` and the CI `test-with-database` job exist, and why
the comment is in both.

No test in this repository opens a socket to anything but the database named by
the environment. HTTP tests drive the router with `tower::ServiceExt::oneshot`,
which is a function call rather than a network round trip.

No sleeps anywhere. The one test that needs an expired presign URL uses a
zero-second TTL and asserts the fake refuses it — not a one-second sleep, which
would be a flake waiting for a loaded CI box.

## Not done, and why

- **The outbox publisher loop.** Needs a NATS client and a deployment. The
  table and the insert path — the half with the correctness guarantee — are
  here; a publisher loop that cannot publish is worse than an absent one.
- **The stale-`pending` sweeper.** `store::find_stale_pending` exists and is
  tested; nothing calls it on a schedule yet. `PENDING_SWEEP_AFTER` is twice
  the presign TTL so a client mid-PUT is never swept.
- **The idempotency-key retention sweep.** 24 hours per core; the index makes
  it a range scan when it is written. Until then the table grows.
- **An orphaned-object sweeper.** If storage deletion fails during
  `DELETE /v1/assets/{id}`, the row is still removed and the object leaks. That
  is the deliberate trade (a row that says "gone" over a surviving gigabyte is
  worse), and it needs a reconciler.
- **Audio and video transcoding, and thumbnails for non-images.** Out of scope
  for this packet. The `kind` enum and the variant pipeline are built so both
  are additive, but only the image path is implemented.
- **A CDN or any public read URL.** Serving bytes is a different service's job;
  this one hands out storage keys and presigned writes only.
- **Malware scanning.** Named in the brief as out of scope. It belongs on the
  `ready` transition as a consumer of `darkroom.asset.ready`, not inline in the
  request path.
- **Lossy WebP variants.** See above: a dependency decision, deliberately
  deferred rather than taken silently.
- **Contract tests against a running instance.** The document-shape and
  event-declaration checks are here; `caf contract test` against a live
  darkroom is not wired into CI because there is no deployed instance yet.
