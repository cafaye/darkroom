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

### The checksum is verified by reading the object back, not trusted

The client sends the sha256 at step 1. That value is a **claim**: it is written
to the `assets` row as `pending` and is never what `complete` compares against.
At step 3 the service **reads the object back and hashes it**, and writes *that*
to the row.

A service that trusted the claim would let a client register an asset whose
checksum describes bytes it never uploaded — and that checksum is what every
downstream dedupe and every future integrity check is keyed on.

| Situation | Status | Asset ends up |
|---|---|---|
| Bytes arrived, checksum matches | `200` | `ready` |
| No object at complete time | `409` | `failed` |
| Measured size ≠ declared size | `422` | `failed` |
| Checksum does not match the stored bytes | `422` | `failed` |
| Malformed checksum (not 64 hex) | `422` at **create** | nothing written |

`ready` and `failed` are both terminal. A failed upload is not retried into
`ready`; the client creates a new one. That keeps `UNIQUE (account_id,
checksum)` meaningful — a checksum that failed once cannot be quietly retried
into a second attempt — and it means "pending older than the presign TTL" is a
sweepable set with exactly one meaning.

#### The read costs one request, and that is the point

`complete` does `head` (existence, measured size) then `get` (the bytes) then
hashes. **It never asks the backend what checksum it recorded**, and that used
to be how it worked. The old path was:

```rust
put_object().checksum_algorithm(ChecksumAlgorithm::Sha256)  // ask S3 to record one
head_object().checksum_mode(ChecksumMode::Enabled)          // ask for it back
if let Some(recorded) = meta.checksum { compare(claim, recorded) }
else { /* read the object and hash it — the "fallback" */ }
```

That is fine on AWS S3 and it is **not a property at all on Cloudflare R2**.
R2's S3 compatibility table
([`developers.cloudflare.com/r2/api/s3/api`](https://developers.cloudflare.com/r2/api/s3/api/))
offers `FULL_OBJECT` for CRC-64/NVME only and `COMPOSITE` for SHA-256, so the
`FULL_OBJECT` sha256 never comes back. A check whose presence depends on which
bucket answered it is a check that can stop happening, which is the one failure
mode this service exists to prevent. So the fallback became the only path, and
`ObjectMeta` lost its `checksum` field so the dependency cannot be
reintroduced by accident.

**What it costs:** one read of the object per completed upload, in the API
process, bounded by `MAX_UPLOAD_BYTES` (1 GiB). That is a real cost and it is
paid on purpose. `tests/checksum_verification.rs` asserts the number is
*exactly one*, so the trade cannot be quietly reversed into a header lookup by
someone who finds the read expensive.

**What it does not cost:** anything on the S3 path. S3 still verifies at
complete, against what is stored rather than what was sent, which is the
stronger of the two checks. What S3 loses is the bucket-side in-flight check
that `x-amz-checksum-sha256` gave the presigned PUT — and with it, the
requirement that a client send `x-amz-sdk-checksum-algorithm` on a header the
presigner had put in the signature. `curl -T file "$upload_url"` works again.

The SDK is also told not to add checksums on its own
(`RequestChecksumCalculation::WhenRequired`), because its default attaches a
CRC-32 — and CRC-64/NVME in current AWS SDK releases, which R2 rejects — to
every `PutObject` and `UploadPart` that nobody asked about.

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

### Absence, not refusal

**A cross-tenant read is 404, never 403**, and the body is byte-identical to the
one for an id that never existed. A 403 would be a free asset-id oracle: a
caller enumerates ids, gets 403 for the ones that exist and 404 for the ones
that don't, and has a directory of every asset on the platform. Same reasoning
for `GET /v1/assets/{id}/variants`, which returns 404 rather than an empty list
— an empty list is indistinguishable from "no variants yet", which is an
existence oracle with one bit.

This is the rule the rest of the platform should copy. Every service that holds
another service's data has to answer "what do I say when the caller is
authenticated and the row is not theirs?", and there is one correct answer: the
same thing you say when the row does not exist. It holds below the wire too —
`store::find_asset` returns `Ok(None)` for another account's row, never a
`StoreError`, because a distinguishable error is the same oracle one layer down.

### What holds the predicates in place

`store.rs` was correct by construction and **held in place by nothing**. Every
existing test exercised the queries *through* a `Tenant`, so all of them stayed
green if a future edit dropped `and account_id = $2` from one of the eleven
lines. Three files now make the correctness load-bearing:

| file | tier | what it proves |
|---|---|---|
| `tests/tenant_scoping.rs` | default, **no database** | the `account_id` predicate is still written down, for every query that takes a `&Tenant` |
| `tests/query_scoping.rs` | database | read, list, update and delete return only the caller's own rows, against a two-account fixture |
| `tests/tenant_isolation.rs` | database | the same, over the wire: every tenant-scoped route, and the exact status and body |

`tests/tenant_scoping.rs` reads `src/store.rs` with `include_str!` and fails
when a `&Tenant` query stops constraining the column, when the set of such
queries changes, when a mutation names an account without naming a row, when a
second query starts reading across accounts, and when a route in
`http::OPERATIONS` has no negative case. It is in the default tier on purpose:
the structural half of the isolation guarantee should be checked on a machine
with no Postgres and no Docker, because that is where it will be run most often
and it is the half that catches the edit before a fixture is built.

`tests/query_scoping.rs` seeds both accounts with **the same checksum** —
`unique (account_id, checksum)` makes that two legitimate rows — because that is
the case a scoping bug hides in: an unscoped lookup by checksum does not error
and does not return nothing, it returns a row and the wrong one, while every
id-keyed test stays green.

These guards were themselves wrong three times before they were right — a
quadratic scanner that hung two of the twelve tests for eleven minutes, a
"names a row" predicate that `account_id = $2` satisfied because `id = $` is a
substring of it, and a set comparison that asserted an accident of file layout.
Each is written up in `REPORT-darkroom-09-isolation.md`, and each is the reason
`sql_statements` is linear and `naming_a_row_is_anchored_to_a_whole_identifier`
exists. The habit to copy: **plant a divergence, watch the guard go red, and treat
a guard that hangs or passes on broken source as a finding about the guard.**

The full enumeration, the counts per operation kind, and the five tripwires proven
able to fire are in `REPORT-darkroom-09-isolation.md`.

Three things to know before running the database tier on a busy machine, all of
which present as code defects and are not:

- **These suites write to the database they are pointed at.** Each test applies
  the migrations into a schema of its own and truncates only that schema, so two
  tests — or two checkouts — no longer destroy each other's fixtures: that used
  to happen, and the `left: 2, right: 1` from `tests/api.rs` recorded during
  darkroom-09 was another worktree's run rather than a leak. What is left is that
  the schemas are created in, and reaped from, the database `TEST_DATABASE_URL`
  names, so a run against a database holding data you want leaves a schema per
  test beside it. Point it at a database you are happy to have that in.
- **The host port is `KIT_POSTGRES_PORT` (15500), not 5432.** darkroom joined
  kit's shared postgres rather than running its own, and the published port
  belongs to kit. It moves with its VARIABLE and with nothing else: a `ports:`
  entry in a service's compose file is APPENDED to the fetched stack's rather
  than substituted for it, so writing `5432:5432` leaves postgres listening on
  15500 *and* 5432. If the suite fails with `role "darkroom" does not exist`,
  check `KIT_POSTGRES_PORT` before reading it as a missing migration — you are
  probably connected to a different database.
- **The cluster provisions ONCE PER VOLUME.** `KIT_POSTGRES_DATABASES` is read
  by an init script that runs only against a fresh `postgres-data` volume, so a
  developer who already has one gets a healthy cluster that has never been told
  `darkroom` exists. `bin/dev db grant darkroom`, or one volume recreation, is
  the fix; nothing shorter applies a new name.

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

### AWS, MinIO and Cloudflare R2 are one implementation

There is no `R2ObjectStore`, and that is the point. The services differ in four
ways, all of them *request shaping*, and request shaping is entirely below the
trait:

| | R2 | how darkroom handles it |
|---|---|---|
| region | `auto`; `us-east-1` and `""` alias to it, but SigV4 must be signed as `auto` | an R2 endpoint resolves to `auto` in `config.rs`; a real region is **refused at startup**, naming the variable and the value |
| endpoint | `https://<ACCOUNT_ID>.r2.cloudflarestorage.com`, account-scoped, no global endpoint | always configuration, never a constant; `region=auto` with no endpoint is refused so a bucket-only config cannot resolve `s3.amazonaws.com` |
| checksums | SHA-256 is `COMPOSITE` only; `FULL_OBJECT` is offered for CRC-64/NVME | darkroom never asks: `ObjectMeta` has no checksum field and `complete` reads the bytes back |
| headers | no `x-amz-acl`, `x-amz-grant-*`, `x-amz-expected-bucket-owner`, no object lock | darkroom never sets them and has no configuration that could; a source-level invariant in `tests/contract.rs` says so |

Addressing is forced path-style for an R2 endpoint (the account endpoint is the
whole host), and the flag is still honoured for anything else.

`tests/storage_backends.rs` runs **the same presign assertions against all of
them** — one body, five configurations, no per-backend branch — and checks the
region in the SigV4 credential scope, the signed-header list, and the absence of
every header R2 rejects. The startup refusals are tested against the real
binary, not only against the parser.

### Running against R2

Build with the feature, point the four variables at the bucket, and give the
service an R2 API token. There is no secret in this repository: the token is
read by the AWS SDK's own chain from the environment.

```sh
export DARKROOM_OBJECT_STORE=s3
export DARKROOM_S3_BUCKET=darkroom-media
export DARKROOM_S3_ENDPOINT=https://<ACCOUNT_ID>.r2.cloudflarestorage.com
# DARKROOM_S3_REGION is optional for R2 and defaults to `auto`.
# Setting it to `us-east-1` is accepted (R2 aliases it) and signed as `auto`.
# Setting it to anything else refuses startup.

# Credentials: the AWS SDK's own chain. An R2 API token has an Access Key ID
# and a Secret Access Key, so the environment pair is enough.
export AWS_ACCESS_KEY_ID=<R2_ACCESS_KEY_ID>
export AWS_SECRET_ACCESS_KEY=<R2_SECRET_ACCESS_KEY>

cargo run --features s3
```

A real bucket is a **manual verification step** — no test in this repository
talks to one, and none can. To confirm it works end to end:

1. **Check the startup log.** It carries `object_store=r2` (not `s3`) and the
   version. If it says `s3`, the endpoint is not an R2 host and R2's rules were
   not applied.
2. **Check a presigned URL.** `POST /v1/uploads` returns `upload_url`. It must
   look like
   `https://<ACCOUNT_ID>.r2.cloudflarestorage.com/darkroom-media/a/<id>/original?…X-Amz-Credential=…%2Fauto%2Fs3%2Faws4_request…&X-Amz-SignedHeaders=content-type%3Bhost…`.
   `auto` in the credential scope and `content-type;host` as the *only* signed
   headers are the two things that are easy to get wrong and impossible to fix
   later.
3. **Upload and complete.** `curl -X PUT --data-binary @file "$upload_url"`
   with no extra headers — if that needs a checksum header, something is wrong.
   Then `POST /v1/uploads/{id}/complete` with the same sha256: `200`, and
   `GET /v1/assets/{id}` shows `status: ready` with the checksum you computed.
4. **Derive a variant.** `POST /v1/assets/{id}/variants` exercises the service's
   own `PutObject`, which is the operation the SDK's default checksum headers
   break against R2. A `200` here is the evidence that
   `RequestChecksumCalculation::WhenRequired` is doing its job.
5. **In the R2 dashboard**, the object is there and its size matches. R2 does
   not store a `FULL_OBJECT` SHA-256 for you, and darkroom no longer expects
   one — the row's checksum came from reading the object back.

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

darkroom's `docker-compose.yml` is an OVERRIDE, not a copy. It is merged with
the stack that `kit.ref` pins, and the `postgres:` service it used to carry
lives in that stack now — this repository names its own database and its own
NOSUPERUSER role in one key and nothing else.

```sh
mise install
cp .env.example .env          # no secrets in this repo

# bring up kit's stack with darkroom's file merged over it.
# KIT_COMPOSE_DIR is NOT optional — without it compose resolves kit's initdb
# mount against THIS repository and silently provisions no databases at all.
KIT_COMPOSE_DIR=<kit>/templates/compose \
docker compose --project-directory . \
  -f <kit>/templates/compose/docker-compose.yml \
  -f ./docker-compose.yml up -d --wait

psql "$DATABASE_URL" -f migrations/0001_assets.sql
psql "$DATABASE_URL" -f migrations/0002_outbox_events.sql
psql "$DATABASE_URL" -f migrations/0003_idempotency_keys.sql

cargo run                                     # in-memory object store
cargo run --features dev-auth                 # + the HMAC token verifier
```

`$DATABASE_URL` in that shell is the one from `.env`, which is the HOST-side
URL — `localhost` and `KIT_POSTGRES_PORT` (15500). The service's own
`DATABASE_URL`, inside the compose network, uses the service name `postgres`
instead. See `docker-compose.yml` for why the two differ and what the password
now is.

Migrations are a **deploy step, not a boot step**. Nothing in `main` applies
one, because two replicas racing to deploy would deadlock on `CREATE TABLE`.

## Tests

```sh
./bin/prime          # fmt, build, clippy, test — no database needed
./bin/prime --db     # + the ignored tests, against TEST_DATABASE_URL
```

`cargo test` alone is green on a bare machine with no Postgres and no Docker.
The database tests are `#[ignore]`d, and **a skipped test proves nothing** —
which is why `bin/prime --db` and the CI `gate` job exist, and why the comment
is in both.

There is a third tier, `cargo test --features s3`, and it exists because
`cargo test` does not compile the `s3` feature at all. Without it the R2
configuration rules, the presigned-URL table and the startup refusals would
compile in nobody's gate and prove nothing — the same "skipped test proves
nothing" argument, one level up. `--db` runs the ignored suites with and without
the feature, because the deployment build is a different build of the library.

### Reading the tiers, because the exit code does not

`bin/prime --db` exits zero in three situations where it has verified nothing:
tier 6 never ran, tier 6 ran and touched nothing, and tier 5 never compiled the
`s3` feature. None of those change the exit code, so the counts are what is
left to read:

```sh
./bin/prime --db 2>&1 | tee /tmp/prime.log
./bin/tier-counts /tmp/prime.log
```

| tier | command | what it must report |
|---|---|---|
| 4 | `cargo test` | 77 lib unit tests, 58 skipped for want of a database, 9 OpenAPI drift checks, 12 tenant-scoping checks, 4 schema-isolation checks |
| 5 | `cargo test --features s3` | 89 lib unit tests, 9 R2/S3 behaviour-table rows, 9 OpenAPI drift checks, 12 tenant-scoping checks |
| 6 | `cargo test -- --ignored` | 58 passed, 0 left ignored, 7 of them query-scoping, 4 of them schema-isolation |
| 6 | `cargo test --features s3 -- --ignored` | 58 passed, 0 left ignored |

One of those is an identity rather than a constant: **the count the default run
skips must equal the count the database run passes**, because they are the same
set of tests. A test that is `#[ignore]`d without the database tier running it
breaks it in one direction; a test added and never `#[ignore]`d breaks it in the
other. Neither can reach master as a green badge.

Adding a test means raising the number in `bin/tier-counts` in the same commit.
That is the point of the constant: it is a claim somebody has to look at.

**Which** number moves is a question rather than a matter of taste, and
`bin/tier-counts`'s header works it through. A new file under `tests/` is a
separate test binary, so it is in none of the four original constants — the first
two count `unittests src/lib.rs`, the third counts `#[ignore]`d tests, and the
fourth is scoped to `storage_backends.rs`. Raising `UNIT_TESTS` for it would have
made that number a lie, so each default-tier file is pinned in its own right
instead: `OPENAPI_DRIFT_TESTS=9`, asserted in tiers 4 and 5. Without that pin a
file whose tests were deleted one at a time would report `0 passed` while every
other number still read correctly.

### One schema per test, which is why tier 6 runs in parallel

There is no `--test-threads=1` anywhere in this repository, and there was one in
`bin/prime` until each `test_store()` started giving its test a schema of its
own. `tests/common/mod.rs` creates `t_` plus eight hex characters of a fresh
uuid, applies `sqlx::migrate!("./migrations")` **into that schema**, and builds
the pool with `set search_path to <that schema>` in `after_connect`, so every
connection the test gets is inside it. `truncate` still runs, and still names its
tables unqualified — that is the isolation, not an oversight: the names resolve
through the search path, so it empties this test's tables and cannot reach
another's.

Three reasons it is a schema and not something else, because all three were
available:

- **Not a database per test.** `TEST_DATABASE_URL` names one database, and
  changing its shape is a different decision than this one.
- **Not a transaction rolled back per test.** `Service` holds a transaction open
  across an `await` on object storage — that is the outbox rule — so a test's own
  writes are not visible inside one, which a rollback-based harness cannot do.
- **Not a mutex.** That is `--test-threads=1` under another name, and it costs
  the whole suite to protect one test.

The search path is the one schema and **not** `t_<hex>, public`, and the reason
was measured rather than reasoned about: the fallback does not break the
migrations (unqualified DDL targets the first schema on the path, and
`if not exists` is checked there). What it costs is quieter — a table this
schema does not have but `public` does resolves to `public`'s copy, so the same
query returns `relation does not exist` without the fallback and another test's
rows with it.

`tests/schema_isolation.rs` holds all of it, and splits it so the half that
needs no Postgres runs on a bare machine: the name's alphabet, the check on the
two statements that interpolate one, and the stamp a `drop schema` is only
allowed to act on. The counts and the proofs are in
`REPORT-darkroom-hermetic-db-01.md`.

### `openapi/v1.yaml` and the router, held to each other

`tests/openapi_document.rs` compares the published document with the routes the
service actually serves, **in both directions**: an operation the document
describes and the router does not serve is a generated client that 404s in
production, and a route the router serves that the document does not describe is
a method the generated client does not have. It follows courier's
`test/courier_web/openapi_document_test.exs`, which is the same tripwire shape
pantry's has fired three times against.

It compares **paths, never counts** — a count comparison passes on a rename and
fails on a pure addition, which is backwards. Both of its readers raise rather
than under-read: an unparseable document, a missing `paths:` key, a path item
with no operation under it, or an empty route table is a failure, because a
green check over nothing is worse than no check.

**The router's side is a route table, not a list in a test.** axum cannot be
asked what it routes — there is no `Router::routes()` and nothing to reflect
over — so `http::OPERATIONS` in `src/http.rs` is a single table of
`(method, path, handler)` and `router()` is a fold over it. One declaration, read
by both the router and the check. The alternative, a route list written out
inside the test, is the shape that can only fail for a name somebody remembered
to type.

One thing the table cannot state, because the method a `MethodRouter` answers is
baked into the value `get(handler)` returns with no accessor for it, is closed by
asking the router: a request with an undeclared verb is answered `405` with an
`Allow` header enumerating the truth, and a path the router does not know is
`404`. That `405`/`404` distinction is what makes the probe possible, and
`a_path_the_router_does_not_serve_is_404_and_not_405` is the control that says
the distinction is real.

**What it cannot see, stated plainly.** The probe reads a path it already knows
about, so it catches a method registered on a known path outside the table — and
it does **not** catch a whole new path registered outside the table. Both halves
were measured by planting a `.route()` call beside the fold: a planted `put` on
`/v1/assets` turned the method test red while both document tests stayed green,
and a planted `get` on `/v1/brand-new` left all nine tests green. Nothing in
axum enumerates the paths a `Router` holds, so that gap is not closable from
inside a test. What narrows it is structural rather than a guarantee: `router()`
is a fold over `OPERATIONS`, so there is one line in this repository where a
route can be registered by hand.

**`/healthz` and `/readyz` are in the document**, under a `probes` tag with
`security: []`, so the omission list is empty. courier excludes them; excluding
them here would mean deleting correct documentation to satisfy a carve-out. The
list is still a closed list keyed by method *and* path rather than a prefix
match, it is empty rather than absent, and two tests hold it: one fails if a
first omission appears, and one fails if an omission stops naming something the
router serves, so a rename cannot hide behind a stale carve-out.

This check needs neither a database nor a socket, so it is in the default tier
and runs on a bare machine. It reads its subject out of `exposes.api` in
`cafaye.yml` rather than hardcoding a filename, so it cannot end up checking a
document the platform does not ship.

### The coverage gate

Coverage is not in `bin/prime` and is not the same thing. It is kit's
`coverage-fail-under` input, set to **50**, against **53.73%** measured line
coverage on rustc 1.95.0 (60.95% regions, 55.85% functions). A floor rather
than a ratchet: it fails if coverage collapses and stays out of the way of a
legitimate change. `--fail-under-lines 50` was measured green and
`--fail-under-lines 54` was measured red, so the gate can actually fail — and
kit's default of 0 fails nothing, which by kit's own rule is not a gate.

Three things it does not measure, stated rather than implied:

- the `--features s3` build, because kit's coverage step runs with default
  features — so `objectstore/s3_impl.rs` is not in the picture at all;
- the database tier, because `cargo llvm-cov` runs the same `cargo test` that
  ignores the 58 database tests — which is why `store.rs` reports 0.54%;
- `main.rs`, at 0%, because a binary's `main` is never called by a test.

What it does catch is the default suite ceasing to run: the only tests
`cargo llvm-cov` executes are the 77 lib unit tests and the 30 non-ignored
integration tests — 1 in `api.rs`, 8 in `contract.rs`, 9 in
`openapi_document.rs` and 12 in `tenant_scoping.rs` — so if those stop running
the number falls off a cliff.

No test in this repository opens a socket to anything but the database named by
the environment. HTTP tests drive the router with `tower::ServiceExt::oneshot`,
which is a function call rather than a network round trip. The S3 backend's
tests presign against the real SDK with static dummy credentials, which is a
local operation — it builds a URI and a signature and sends nothing.

No sleeps anywhere. The one test that needs an expired presign URL uses a
zero-second TTL and asserts the fake refuses it — not a one-second sleep, which
would be a flake waiting for a loaded CI box.

## Not done, and why

- **Streaming verification for large objects.** `complete` reads the object
  through `ObjectStore::get` and hashes it, so a 1 GiB upload is buffered in the
  API process at complete time. It is released as soon as it is hashed, and the
  hash itself is already available in a streaming form
  (`checksum::sha256_hex_streaming`, tested equivalent to the one-shot path), but
  the trait hands over `Bytes` and the read is what it is. The honest options are
  a sixth trait method that streams, or a lower `MAX_UPLOAD_BYTES`; both are
  contract changes and neither belongs in the change that made R2 work.
- **A live R2 integration test.** Nothing in this repository can reach a real
  bucket, and adding something that could would put a credential in CI. What is
  tested is configuration, the credential scope, the signed-header list and the
  headers R2 rejects; what is not is R2 accepting them. The "Running against R2"
  section above is the manual procedure, and the variant-generation step in it is
  the one that catches an SDK-level default. A **local** S3-compatible double
  (MinIO) would close the round-trip half of this without a credential, but it
  needs a test that actually speaks to it — nothing in the suite does today, so
  adding the service container alone would be a green checkmark on nothing.
- **CI does not build the deployment target.** `docker/Dockerfile` runs
  `rustup target add x86_64-unknown-linux-musl` and then copies out of
  `target/x86_64-unknown-linux-musl/release/darkroom` — but its `cargo build`
  has no `--target`, so the file it copies is not there and the image build
  fails. Two problems rather than one: the missing flag, and the fact that
  `ring` needs a C toolchain for that target, so a glibc `ubuntu` runner cannot
  link it either. Nothing in CI would have caught either. Fixing it means
  choosing a musl-capable build image, which is a decision about the release
  pipeline rather than a fix that belongs in a CI packet.
- **The two bullets above are what a green badge here does not currently mean:**
  that a real bucket accepts what darkroom signs, and that the target the image
  ships is the target the compiler can produce.
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

## License

MIT. See [LICENSE](LICENSE). `Cargo.toml` declares the same thing in its
`license` field.

darkroom is a platform a consumer depends on rather than reads, so the licence
has to leave the consumer's own situation alone. MIT does; a copyleft licence
would make every downstream inherit an obligation, which is the opposite of
what a service registry is for.
