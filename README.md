# darkroom

The cafaye media service. Signed uploads, a tenant-scoped asset registry, and
derived variants — the foundation anychat's voice clips and anymark's
attachments both sit on.

One static Go binary. It owns its own database (PLAN.md §7,
database-per-service) and never queries identity's or billing's.

- **Contract**: [`openapi/v1.yaml`](openapi/v1.yaml) — what `exposes.api` in
  [`cafaye.yml`](cafaye.yml) points at
- **Events**: `darkroom.asset.ready`, `darkroom.asset.deleted`,
  `darkroom.variant.created`
- **Conventions**: [`moon/cafaye/core/docs`](../core/docs) is normative. Where
  this repository and core disagree, core is right and this is the bug.

## Why the tenant boundary is the database's

*(Historical. The Rust tree chose `axum` for its `FromRequestParts` extractor,
which made the tenant context a *type*; there is no axum here.)*

The Go tree holds the same property a different way, and the migration file is
the reason. `migrations/0004_account_isolation.sql` gives `assets` and
`asset_variants` **FORCE ROW LEVEL SECURITY**, so the predicate stopped being a
`where account_id = $2` that every query has to remember and became a POLICY:

    src/store.rs                     darkroomdb
    ------------------------------   ------------------------------------
    auth::Tenant                     uuid.UUID, from the verified token
    &mut Transaction                 the pgx.Tx InAccountTx hands the callback
    `where account_id = $1`          cafaye.begin_account($1)

`InAccountTx` is the only way to start a transaction through that package and it
makes naming the account part of starting the transaction, which is the Rust
`Tenant` rule in a form a Go signature can carry. `auth.Tenant` is still a
closed type with one constructor taking a verified `*Principal`, so an account id
from a request body cannot become a tenant anywhere either.

The mandatory half — the Rust typecheck that a handler *cannot* omit the tenant —
is not expressible in Go, so `auth.TenantOf(r)` **panics** when there is no
verified principal and every id-scoped handler calls it as its first statement.
A panic fails the test at the missing call and drops one connection in
production; a 500 written by the accessor would converge into documented
behaviour, which is the wrong direction.

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
has to buffer it, spool it to disk, and time out on a slow client. Two
`Stats` counters assert it: `TestTheClientBytesNeverPassThroughTheService`
wants `Puts == 0`, and `TestAClientsWriteNeverGoThroughPut` wants the same from
the other side of the interface.

### The presigned URL

| | |
|---|---|
| **TTL** | **900 seconds (15 minutes)**, in `objectstore.PresignTTL` |
| **Scope** | exactly one storage key, one content type, one maximum byte size |
| **Signature** | SigV4 presigning (S3) / a signed `(key, expiry)` pair (in-memory) |

Fifteen minutes rather than an hour because a presigned URL is a bearer token
for one object, and an hour is long enough for it to be found in a proxy log, a
browser history, or a `Referer` header and replayed for the rest of that hour.
The three axes of scope are all load-bearing: a URL that permits any key permits
overwriting another tenant's object; a URL with no length cap is a way to write
a 10 GiB file into a bucket someone pays for; a URL with no content-type scope
lets an HTML document be stored at an image's key.

All three axes are asserted in `internal/objectstore`, not spread across the
layers above it: `TestAPresignedPutIsScopedToOneKeyOneContentTypeAndOneLength`
covers the three together, `TestAssetAsPresignedURLCannotWriteAtAssetBsKey` is
the cross-key one — asset A's URL cannot be used to write at asset B's key — and
`TestAPresignedPutWithNoCeilingIsRefused` is the case where a leak writes an
arbitrary number of bytes into a bucket someone pays for.

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

*(Historical. That was the Rust tree's path and it is in git history; the Go
`objectstore.Store` has no method that asks a backend what checksum it recorded,
which is the property this section is about.)*

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
process, bounded by `objectstore.MaxUploadBytes` (1 GiB). That is a real cost and
it is paid on purpose. The read-count assertion is in `internal/service`'s tests
and the number is *exactly one*, so the trade cannot be quietly reversed into a
header lookup by someone who finds the read expensive.

**What it does not cost:** anything on the S3 path. S3 still verifies at
complete, against what is stored rather than what was sent, which is the
stronger of the two checks. What S3 loses is the bucket-side in-flight check
that `x-amz-checksum-sha256` gave the presigned PUT — and with it, the
requirement that a client send `x-amz-sdk-checksum-algorithm` on a header the
presigner had put in the signature. `curl -T file "$upload_url"` works again.

The SDK is also told not to add checksums on its own
(`aws.RequestChecksumCalculationWhenRequired`), because its default attaches a
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

`account_id` comes from the verified token and from nowhere else. No request
body in `openapi/v1.yaml` has an account field, so the generated
`api.CreateUploadRequest` cannot carry one — and a client that sends
`account_id` anyway gets nothing, because `internal/httpapi` builds
`service.CreateUpload` from four named fields and takes the account from
`tenant.Caller()`. (`CreateUploadRequest` is `additionalProperties: true`, so an
extra key is a valid request and refusing it would contradict the contract;
ignoring it is the correct answer.)

Two things make this structural rather than a convention repeated at seven
handler sites by hand:

- `auth.Tenant` has exactly one constructor, `auth.NewTenant`, it takes a
  verified `*Principal`, and its fields are unexported. There is no
  `auth.TenantFromRequest`, so a handler physically cannot scope a query to an
  account the caller named.
- `darkroomdb.InAccountTx` is the only way to start a transaction through that
  package, so a repository function cannot run at all without naming the
  account. That is the Go answer to the Rust rule a signature carried, and
  `AGENTS.md` is candid that it is the weaker guarantee.

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
`findAsset` in `internal/service/store.go` returns `pgx.ErrNoRows` for another
account's row and for no row at all, and `Service.find` maps that one sentinel
onto one `problem.NotFound`. A distinguishable error is the same oracle one
layer down.

`TestACrossTenantReadIsTheSame404AsAMissingOne` is the regression guard on the
service side and `TestAnUnroutedPathIsTheSameDocument` is its wire-side twin.

### What holds the predicates in place

*(Historical. The Rust tree's answer was three test FILES: a structural one that
read `src/store.rs` as text with `include_str!`, and two behavioural ones. All
three are in git history; here is what replaced them and why the structural half
no longer has a file to read.)*

The Rust half that is gone is not a loss: `tests/tenant_scoping.rs` read
`src/store.rs` to prove its queries still said `account_id = $2`. There is no
query file to read — the predicate is a POLICY, and the thing that can be wrong
rather than absent is now a migration, not a forgotten clause.

What replaces it, in three layers:

| layer | tier | what it proves |
|---|---|---|
| `migrations/0004_account_isolation.sql` + `internal/darkroomdb/pgtest`'s policy guards | database | the FORCE-RLS policies exist, name a row, and cannot be unfocused by the login role |
| `internal/service`'s tests | database | read, list, update and delete return only the caller's own rows, against a two-account fixture |
| `internal/httpapi`'s drift tests | default | the document's routes are the router's routes, in both directions, and every one answers 401 without a credential |

`pgtest`'s policy guards are the structural half in its new shape: they read
`pg_get_expr` on the live policies rather than a source file, so what they hold
in place is the database's answer rather than a text file's spelling — and
`pg_get_expr` deparses `(select f())` as `( SELECT f() AS f)`, so the guard has
to know its exact shape to read it. That is stated in `pgtest`'s own tests.

`internal/service`'s cross-tenant tests seed both accounts with **the same checksum** —
`unique (account_id, checksum)` makes that two legitimate rows — because that is
the case a scoping bug hides in: an unscoped lookup by checksum does not error
and does not return nothing, it returns a row and the wrong one, while every
id-keyed test stays green.

*(The Rust guards were themselves wrong three times before they were right — a
quadratic scanner that hung two of the twelve tests for eleven minutes, a "names
a row" predicate that `account_id = $2` satisfied because `id = $` is a substring
of it, and a set comparison that asserted an accident of file layout. Those three
are why `sql_statements` was made linear and why
`naming_a_row_is_anchored_to_a_whole_identifier` existed at all, and they are in
`REPORT-darkroom-09-isolation.md`.)* The Go equivalents took the lesson rather
than the code: the structural scan is `TestEveryHelperCallInAPolicyIsWrapped`,
the predicate it uses is `isWrappedAt`, and `TestIsWrappedAt` is the table that
had to be written before the predicate was believed — its first version trimmed
whitespace on one side only and read all eight correct policies as bare.

The habit to copy: **plant a divergence, watch the guard go red, and treat
a guard that hangs or passes on broken source as a finding about the guard.**

The full enumeration, the counts per operation kind, and the five tripwires proven
able to fire are in `REPORT-darkroom-09-isolation.md`.

Three things to know before running the database tier on a busy machine, all of
which present as code defects and are not:

- **These suites write to the database they are pointed at.** Each test applies
  the migrations into a schema of its own and truncates only that schema, so two
  tests — or two checkouts — no longer destroy each other's fixtures: that used
  to happen, and the `left: 2, right: 1` recorded during
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
  `darkroom` exists. One volume recreation is the fix — `docker compose down -v`
  against the shared stack, then `up -d --wait` again — and nothing shorter
  applies a new name. (This README used to name `bin/dev db grant darkroom`.
  There is no `bin/dev` in this repository: `bin/` holds `prime`, `tier-counts`
  and `gate-self-test` and nothing else, so the command did not exist.)

## The storage boundary

One interface, `objectstore.Store`, with five methods:

```go
PresignPut(ctx, key, contentType string, maxBytes int64, ttl time.Duration) (PresignedPut, error)
Head(ctx, key string)                 (ObjectMeta, error)
Get(ctx, key string)                  ([]byte, error)
Put(ctx, key string, b []byte, contentType string) error
Delete(ctx, key string)               error
```

Everything above the line is policy — tenancy, checksum verification, status
transitions, the outbox. Everything below is transport. There is no "if s3
then" above the line and no policy below it.

Two implementations:

- **`objectstore.Memory`** (`objectstore.NewMemory`) — always compiled, used by
  every test and by `DARKROOM_OBJECT_STORE=memory`. Its presigned URLs are
  really signed and really scoped: `Memory.ApplyPresignedPut` verifies the
  signature, the key, the content type, the length cap and the expiry, and
  refuses anything outside them. A fake that merely "worked" would let a bug in
  scope or TTL ship, because nothing in the test would notice.
- **`objectstore.S3`** (`objectstore.New`) — always compiled too, because **Go
  has no feature flags here: one build is the deployment build.** The AWS SDK
  is a default dependency and always linked, so what keeps a test off the
  network is not the dependency graph but the constructors: `New` REFUSES a
  static credential and takes the SDK's own chain, so a test that needs one
  calls `NewS3ForTest` instead. `TestProductionRefusesAStaticCredential` is the
  guard on that.

Client uploads go through the presigned URL, never through `Put` — `Put` is the
service's own path, used by variant generation to store a derived image.
`TestAClientsWriteNeverGoThroughPut` asserts the client's bytes do not increment
it.

### AWS, MinIO and Cloudflare R2 are one implementation

There is no R2-specific store type, and that is the point. The services differ in
four ways, all of them *request shaping*, and request shaping is entirely below
the interface:

| | R2 | how darkroom handles it |
|---|---|---|
| region | `auto`; `us-east-1` and `""` alias to it, but SigV4 must be signed as `auto` | an R2 endpoint resolves to `auto` in `internal/objectstore/s3config.go`; a real region is **refused at startup**, naming the variable and the value |
| endpoint | `https://<ACCOUNT_ID>.r2.cloudflarestorage.com`, account-scoped, no global endpoint | always configuration, never a constant; `region=auto` with no endpoint is refused so a bucket-only config cannot resolve `s3.amazonaws.com` |
| checksums | SHA-256 is `COMPOSITE` only; `FULL_OBJECT` is offered for CRC-64/NVME | darkroom never asks: `ObjectMeta` has no checksum field and `complete` reads the bytes back |
| headers | no `x-amz-acl`, `x-amz-grant-*`, `x-amz-expected-bucket-owner`, no object lock | darkroom never sets them and has no configuration that could; `objectstore.S3Config` has no field for any of them, which is the stronger form of the same claim |

Addressing is forced path-style for an R2 endpoint (the account endpoint is the
whole host), and the flag is still honoured for anything else.

`internal/objectstore` runs **the same presign assertions against all of
them** — one body, five configurations, no per-backend branch — and checks the
region in the SigV4 credential scope, the signed-header list, and the absence of
every header R2 rejects. The refusals are asserted in
`cmd/darkroom`'s `TestTheR2RulesLiveInTheStore` at the layer that actually
refuses (`objectstore.New`, before any client exists), and
`TestAnS3ConfigurationIsCarriedThroughUnexamined` holds the boundary that keeps
those rules out of `cmd/darkroom/config.go`. **Nothing runs the built binary to watch it
refuse** — that is a gap, not a claim, and the R2 section below is the manual
check that stands in for it.

### Running against R2

There is nothing to build differently: one build is the deployment build. Point
the four variables at the bucket and give the service an R2 API token. There is
no secret in this repository: the token is
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

DARKROOM_OBJECT_STORE=s3 go run ./cmd/darkroom
```

A real bucket is a **manual verification step** — no test in this repository
talks to one, and none can. To confirm it works end to end:

1. **Check the startup log.** It carries `"object_store":"s3"` and the version.
   It says `s3` for R2 too, and that is correct rather than a loss: `AGENTS.md`
   says "R2 is not a variant, a feature or a second type: it is the endpoint, a
   region, and addressing," so the log naming the store rather than the vendor is
   the same claim. What to check instead is that the startup did NOT refuse: an
   R2 endpoint with a non-`auto` region exits with a message naming
   `DARKROOM_S3_REGION`, and that refusal is the whole of the R2 configuration
   rules.
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
   `aws.RequestChecksumCalculationWhenRequired` is doing its job.
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

Every variant is **JPEG**. WebP would be better and is deliberately not used,
for two reasons that are the same reason. In Rust: the `image` crate's WebP
encoder is **lossless-only** — its own docs say so and point at libwebp for
lossy — and a lossless re-encode of a 256px thumbnail saves little enough to not
be worth a C dependency and a second codec's failure modes. In Go the same
refusal is stronger: there is **no first-party WebP encoder at all**
(golang/go#45121 is open and unassigned), and `golang.org/x/image/webp` is a
pure-Go *decoder*, so "switch the variants to WebP" would mean cgo libwebp and
with it the static binary the Dockerfile builds. `image/jpeg` is lossy, pure Go,
statically linked and decodable by everything that will ever request a
thumbnail. **This is the one place a dependency decision is deferred**; it is
called out in the report.

Alpha is composited over white explicitly, as integer arithmetic:
`out = src·a + 255·(1−a)`, rounded to nearest. JPEG has no alpha channel and the
easy conversion from RGBA **drops** the alpha rather than compositing it, so a
transparent black pixel would stay black in the served image — which is why
`variants.go` walks the pixels and calls `matteOverWhite` on each channel
itself. A test caught exactly that in the Rust tree, and the Go port has the
same test plus a resized-transparent variant, because compositing correctly
and then resizing premultiplied RGBA both produce plausible images and only one
of them is right.

The original row and its object are never touched. `service.CreateVariant` reads
the original and writes a variant into a **different key namespace**, and never
opens an update against `assets`; `TestCreateAndListVariants` asserts the
variant's key differs from the original's, that the derived bytes really are in
storage, and that a re-requested kind replaces rather than accumulates.

Accepted inputs: **PNG, JPEG, GIF, WebP**. Accepted by `internal/variants/`
(decoding) and by `AssetKind` (declaring) — a decode failure is a 422 naming
`content_type`, because the bytes were checksum-verified first, so a declared
type that disagrees with them is the client's mistake.

### The Go dependency: `golang.org/x/image`

One dependency, stated here because `AGENTS.md` requires it.

**`golang.org/x/image`, for two packages.** `draw` supplies the resampling kernel
and `webp` supplies a pure-Go WebP decoder. Both are needed; neither is optional.

- **No cgo.** `chai2010/webp` and `gen2brain/webp` are the two popular WebP
  decoders in Go and both are cgo bindings to libwebp. Either would cost darkroom
  its static binary and complicate the musl build, to decode a format that only
  ever *arrives*: every variant is JPEG, so darkroom reads WebP and never writes
  it. `golang.org/x/image/webp` is first-party, pure Go, and maintained.
- **No hand-rolled image processing.** The packet's brief and this repository's
  house rules agree here. `image/jpeg`, `image/png` and `image/gif` are the
  standard library's; the resampler and the WebP decoder are `x/image`'s.
- **WebP *encoding* is not available and not needed.** See the JPEG paragraph
  above. `internal/variants/` says so in its package comment too, so nobody
  re-derives the need later.

The `webp` decoder registers itself with `image.Decode` through a **blank
import**, which is worth one sentence because it is the only guard-shaped thing
about it: deleting that line leaves a program that compiles, links, vets clean
and passes every other test, and then fails on the first WebP a customer uploads.
So the formats are held by `TestEveryAcceptedInputFormatDecodes`, which carries
a real file of each, and `TestEveryAcceptedFormatIsRegisteredInThisFile`, which
reads `variants.go` and holds the registration the behavioural test structurally
cannot. That second test exists because PNG, JPEG and GIF decoders share a
standard-library package with their *encoders*, which the test file needs — so
dropping those three blank imports leaves the behavioural test green. WebP is
the exception and not by luck: `x/image` has a decoder and no encoder, so nothing
in the repository can register the WebP decoder except the blank import. That
split is why the WebP fixtures are checked in rather than built
(`internal/variants/testdata/README.md` records the `cwebp` commands).

### The outbox dependencies: `nats.go`, and `nats-server/v2` for its tests

Two more, and `AGENTS.md` requires each to carry its cause.

**`github.com/nats-io/nats.go` — the client, and it is the whole of the
transport.** `internal/outbox/nats.go` is one file and it is the only non-test
importer. The matter is *which part* of it: JetStream's `PublishMsg`, and not
core NATS' `Publish`. Core NATS returns when the message is on the wire, not when
the broker has stored it, so marking a row published on that return would mark
rows the broker never saw — which is the failure the outbox table exists to
prevent. JetStream's is a request that blocks until the broker acks, so a nil
return from `Publish` IS the ack the loop marks the row from.

**`github.com/nats-io/nats-server/v2` — a TEST-ONLY dependency, and it buys the
one proof the fakes cannot.** A capturing publisher can prove the loop hands the
transport an envelope and marks what the transport accepted. It cannot prove the
transport puts anything onto a bus, because from the loop's point of view the fake
IS the bus. An in-process JetStream broker on a random loopback port proves it by
reading the message back out of the stream, so the message being there becomes the
BROKER'S statement rather than this package's. That is the difference between
believing a delivery happened and observing one.

It is not an exception to the no-network rule. The server is the test's own
process, on 127.0.0.1, on a port the OS chooses; no test binds a service port and
nothing here reaches anywhere outside the machine. The dependency is confined to
`_test.go` files, which is what keeps it out of the deployment binary — `go list
-deps` on `cmd/darkroom` does not name it.

The rest of its tree is transitive weight this repository does not choose
(`nkeys`, `nuid`, `jwt/v2` for the NATS auth surface, `antithesis-sdk-go` and
`go-tpm` for the server's own fault-injection build). None of it is imported, and
`go mod why -m` names the two above as the only direct edges —
`nats-server/v2` through `internal/outbox.test` and `nats.go` through
`internal/outbox`, with nothing in `cmd/darkroom`'s dependency set naming the
server at all.

## Events and the outbox

| type | subject | when |
|---|---|---|
| `darkroom.asset.ready` | the asset | Complete verified the bytes. **Not** at create. |
| `darkroom.asset.deleted` | the asset | The row and the storage objects are gone. |
| `darkroom.variant.created` | the **variant** | A derived image is readable. |

Three segments, publisher-prefixed, past tense, per
`core/docs/event-naming.md`. The first segment is `darkroom` because core
requires the publisher's own name — the brief asked for `media.asset.*` and
`cafe contract lint` rejects it. `media` is the domain; `darkroom` is the
service. A rename is a new type, so these are the names from the start.

There is exactly one way to write an event: `enqueue` in
`internal/service/outbox.go`, and it takes a `pgx.Tx` — there is no variant that
takes a pool, no queue in memory, and no second entry point. A function that
holds a transaction cannot publish outside it, and a function that does not hold
one has no way to publish at all. **No
event is emitted at `pending`** — a consumer that built a thumbnail on a
`pending` asset would find no bytes, and a consumer that notified a user would
be telling them about an upload that has not landed.

`TestARolledBackTransactionLeavesZeroOutboxRows` writes a domain row and an event
in one transaction, rolls it back, and asserts **both** tables are empty.
Asserting
only the domain row would still pass if the event were written outside the
transaction.

### The publisher, and what it costs on the bus

`internal/outbox` drains the table onto a NATS JetStream stream when `NATS_URL`
is set, and starts **nothing** when it is not — rows accumulate in
`outbox_events` and drain the moment a broker is configured, which is the whole
argument for publishing from a table rather than from the request path.

| knob | value | why |
|---|---|---|
| batch | 100 | a larger transaction holds its row locks longer; a smaller one costs more round trips |
| poll | **1s, fixed** | an exponential backoff that grows while idle leaves a completed upload's `darkroom.asset.ready` unpublished for minutes |
| `published_at` | written from the broker's **ack** | JetStream's `PublishMsg` is a request that blocks until the broker acks, so a nil return IS the ack. Core NATS returns when the message is on the wire and would mark rows published that were never delivered |
| claim | `where published_at is null order by created_at limit $1 for update skip locked` | `skip locked` is what lets two replicas run at once; a bare `for update` serialises them on the head of the table |
| subject | the **event type** | core's "no mapping table, no prefix rewriting". The stream the stack must declare is therefore **`darkroom.>`** |

**The stack must declare a stream whose subject filter covers `darkroom.>`.** A
JetStream publish to a subject no stream matches is refused, and the refusal is
correct: the row stays claimable with `attempts` climbing, which an operator
watching `select count(*) from outbox_events where published_at is null` can
see. kit's compose runs the broker with `-js` and defines no streams, so this is
the one fleet-level fact this packet imposes.

**The claim transaction sets no account.** `outbox_events` has no `account_id`
column and no policy, and `tenancy.yml` records it as deliberately unscoped, so
there is no tenant to scope the claim by — and wrapping it in `InAccountTx`
would put an identity on a transaction nothing in the database agrees with. The
claim goes through `darkroomdb.BeginTx`, the identity-free transaction
`darkroomdb.InTx` already documents `outbox_events` as being for, and the tenant
axis is held by the claim query naming ONE table. `internal/outbox/store_test.go`
reads that query as text and holds it, because a join to carry a checksum onto
the bus is exactly the edit that reads as an improvement.

**No event carries a capability.** The bus is a broadcast surface — every
consumer receives every darkroom event — so an event carries the fact and never
the identifier of whose it was, and above all never a presigned URL or a bucket
key. `account_id` is therefore not in any payload, and `internal/outbox` refuses
to publish an envelope carrying one, a storage key or an absolute URL at all.
The refusal is a FAILED publish — the row stays unpublished with `attempts`
climbing — because the alternative is the capability on the bus and the warning
in a log nobody reads.

**The loop's proof is not a database test.** The database suites SKIP with a
named reason when no PostgreSQL answers, and the `Claimer` interface exists
precisely so the drain can be driven by a fake: nineteen cases in
`internal/outbox` run with no socket and no schema, and the transport is proved
against an in-process JetStream broker of its own on a random loopback port.

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

Two details of the wire half that a client can observe:

- **`Idempotency-Replayed` is only ever `true`, never `false`.** The document types
  it as an `enum: ['true']`, so absence means "not a replay" and a client testing for
  a replay should test for the header's presence.
- **A replayed body is the ledger's, not the original response's bytes.** The row
  stores `jsonb`, and Postgres re-serialises what it was given — key order and
  spacing come back its way. The replay is byte-identical to what the ledger holds,
  which is the ledger's promise, and the same JSON *value* as the first response,
  which is the contract's. A client comparing bytes rather than parsing JSON would see
  the difference; every client parsing JSON does not.

The layer is a middleware rather than a handler concern because capturing the
response is the whole problem: a handler chooses its status in the same statement it
writes its body, so it cannot see what it produced. It sits **inside** the credential
gate — the ledger's scope is `(key, endpoint, principal)`, and an anonymous caller
must not be able to burn a key. The `endpoint` is the route (`POST /v1/uploads`),
never the request's own path, so one key reused across two completions of the same
asset is one key rather than two.

The set of idempotent operations is a hand-written map in `internal/httpapi` next to
the router, and `TestTheIdempotentRoutesAreExactlyTheDocumentOnes` holds it against
`openapi/v1.yaml` in both directions — a route the document makes idempotent that
the router does not wrap, and a route the router wraps that the document does not.
That is what makes the list acceptable rather than a second inventory. The test
matches on the parameter's header `name` and `in: header` rather than on the
component key, because `IdempotencyKey` and `Idempotency-Key` are different strings
and it is the header that a client sends.

`markReady` is also a compare-and-set (`and status = 'pending'`), so two
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

`/healthz` and `/readyz` are exempt from authentication **structurally**: the
router is built from three `chi.Router`s, and the probe router simply has no
auth middleware to remove. `internal/httpapi.New` walks the generated router
with `chi.Walk` and routes everything outside `APIV1Prefix` onto the probe
router and everything inside it onto the guarded one, so the split is on the
PATH and not on a list of names.

*(Why the Rust tree could not do this: axum's `Router::layer` applies to every
route the router holds, so "register the probes before the auth layer" does not
exempt them, and there is a test over there saying so. chi's `Use` has the
opposite property — it applies to routes registered *after* the call and to no
others — so the exemption here does not depend on ordering at all. A `/healthz`
behind the auth middleware returns 401, an orchestrator marks every instance
unhealthy, and the deployment rolls back with no indication of why.)*

`TestEveryUnauthenticatedRouteIsOneTheDocumentExempts` holds the set this code
exempts against the set `openapi/v1.yaml` exempts, so adding an unauthenticated
route means editing the document.

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

`Asset` has no serialised storage key: the field is unexported (`storageKey`)
and reachable only through `Asset.StorageKey()`. A client that can read it will
eventually try to build a URL from it.

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
psql "$DATABASE_URL" -f migrations/0004_account_isolation.sql   # no RLS without this one
psql "$DATABASE_URL" -f migrations/0005_substrate_completion.sql  # the rest of kit's copy

DARKROOM_OBJECT_STORE=memory go run ./cmd/darkroom
```

**All five migrations, in order.** This used to list three and omit
`0004_account_isolation.sql`, which is the one that makes the tenant boundary the
database's — a database built from the first three answers every query, correctly,
for every account. `0005_substrate_completion.sql` was the second omission, and
kit's `tests/substrate_copy_check.py` is what found it: kit added four functions
to its template the day after `0004` was copied, and this copy stayed at eight
until `0005`.

`DARKROOM_OBJECT_STORE=memory` is the default, and the binary warns at startup
that uploads are lost on restart. There is no `dev-auth` build flag: the Go tree
has no HMAC verifier, so `DARKROOM_ENV` gates nothing and `auth.New` is the only
verifier. A development deployment needs a real JWKS endpoint.

`$DATABASE_URL` in that shell is the one from `.env`, which is the HOST-side
URL — `localhost` and `KIT_POSTGRES_PORT` (15500). The service's own
`DATABASE_URL`, inside the compose network, uses the service name `postgres`
instead. See `docker-compose.yml` for why the two differ and what the password
now is.

Migrations are a **deploy step, not a boot step**. Nothing in `main` applies
one, because two replicas racing to deploy would deadlock on `CREATE TABLE`.

## Deploying it

darkroom ships a Helm chart (`chart/`), a publish workflow
(`.github/workflows/publish.yml`) and a `bin/dev` wrapper, so it deploys the
same way the rest of the fleet does: an image plus a chart, pulled by Argo CD
from `oci://ghcr.io/cafaye/charts/darkroom`.

### What you need on the box

1. **A Postgres database** — one database and one login role for darkroom,
   following kit's single-cluster rule (owner role for migrations, a
   `darkroom_app` login role for the process). Create them once:
   ```sh
   psql "$ADMIN_URL" -c "CREATE ROLE darkroom WITH LOGIN PASSWORD '...' NOSUPERUSER NOCREATEDB;"
   psql "$ADMIN_URL" -c "CREATE DATABASE darkroom OWNER darkroom;"
   ```
2. **The five migrations**, applied as a deploy step (never at boot):
   ```sh
   for f in migrations/000{1..5}_*.sql; do psql "$DATABASE_URL" -f "$f"; done
   ```
3. **A secret named `darkroom-secrets`** holding at least
   `DARKROOM_DATABASE_URL` (the login role's URL). Add `DARKROOM_ISSUER`,
   `DARKROOM_AUDIENCE`, and either `AWS_*`/R2 credentials or set
   `DARKROOM_OBJECT_STORE=memory` (uploads are lost on restart).
4. **A bucket** if `DARKROOM_OBJECT_STORE=s3` (Cloudflare R2, S3 or MinIO).
   See "Running against R2" above for the configuration rules.

### Installing

```sh
helm upgrade --install darkroom ./chart -n darkroom \
  -f deploy/local/values/darkroom.yaml   # or your own values
```

The chart is internal by default (`ingress.enabled: false`); darkroom is
reached through the gateway, not directly. For a local cluster smoke test,
see `deploy/local/values/darkroom.yaml`.

### Repeatability check

This procedure was exercised end to end against a fresh `k3d` cluster:
image built and imported, namespace and secret created, migrations applied,
`helm upgrade --install` run twice (idempotent), and `/healthz` and `/readyz`
returned `{"status":"ok"}` and `{"status":"ready"}`. The values file in
`deploy/local/` is the reproducible input; change only that file per
environment.

## Tests

```sh
./bin/prime          # fmt, build, vet, test, regenerate-and-diff — no database needed
./bin/prime --db     # + the database tiers, against TEST_DATABASE_URL and
                     #   DARKROOM_DATABASE_URL. It refuses to start without both.
```

`go test ./...` alone is green on a bare machine with no Postgres and no Docker.
The database tests call `pgtest.Require`, which SKIPS with a named reason when no
database answers, and **a skipped test proves nothing** — which is why
`bin/prime --db` and the CI `gate` job exist, and why the comment is in both.

There is a third tier, and it exists because the generated router is the surface:
`go generate ./internal/api/` plus a diff. `internal/api/api.gen.go` is built
from `openapi/v1.yaml` and `internal/httpapi.New` WALKS that file to register its
routes, so a stale generated file answers the old routes under the new document's
promises. Without the regenerate step nothing in the gate would notice.

### Reading the tiers, because the exit code does not

`bin/prime --db` exits zero in three situations where it has verified nothing:
the database tiers never ran, they ran and touched nothing, and a suite was
emptied. None of those change the exit code, so the counts are what is left to
read:

```sh
./bin/prime --db 2>&1 | tee /tmp/prime.log
./bin/tier-counts /tmp/prime.log
```

| step | command | what it must report |
|---|---|---|
| default | `go test -v ./internal/variants/` | 24 `--- PASS:` rows |
| database | `go test -v ./internal/darkroomdb/...` | 15 `--- PASS:` rows, 0 skipped |
| database | `go test -v ./internal/service/...` | 39 `--- PASS:` rows, 0 skipped |
| database | `go test -v ./internal/httpapi/` | 34 `--- PASS:` rows |
| default | `go test -list '.*'` | ten packages, floored in `bin/prime` itself |

One of those is an identity rather than a constant: **the number of tests
`bin/prime` enumerates without a database must equal the number the database
tiers pass with one.** The Rust script could say `skipped == passed`, because a
cargo database test is `#[ignore]`d and libtest prints an `ignored` count. A Go
test is not marked; it skips at runtime, so that identity is not merely
unavailable but FALSE here — measured against no database, `pgtest` is 4 PASS and
11 SKIP and `service` is 9 PASS and 20 SKIP, so the skipped counts are 31 and the
passed counts are 44. The enumerating-equals-passing version is true, is in the
same log, and is checked in both directions.

A tier reporting `--- SKIP:` where a pass was expected fails the script outright:
a skip there is a check that did not run under a green `ok`.

Adding a test means raising the number in `bin/tier-counts` in the same commit.
That is the point of the constant: it is a claim somebody has to look at.

**Which** number moves is a question rather than a matter of taste, and
`bin/tier-counts`'s header works it through. In the Rust tree a new file under
`tests/` was a separate test binary and so was in none of the four constants —
which is why this tree pins each package in `bin/prime`'s `assert_go_suite`
rather than relying on one total. Without those floors a package whose tests
were deleted one at a time would report `[no test files]` and print `ok`, while
every other number still read correctly.

### One schema per test, which is why the database tiers run in parallel

There is no test-serialising flag anywhere in this repository, and there was one
in `bin/prime` until each `pgtest.New` started giving its test a schema of its
own. `internal/darkroomdb/pgtest` builds a name of `SchemaPrefix` (`t_`) plus
exactly `SchemaSuffixLen` (12) hex characters — four from a hash of `t.Name()` so
a failure names the test that made the schema, and eight from `crypto/rand` so
two cases of one table-driven test cannot collide — applies `migrations/` **into
that schema**, and puts that schema on the pool's `search_path`, so every
connection the test gets is inside it.

**There is no `truncate` in this harness.** This paragraph used to say one
"still runs, naming its tables unqualified", which was the Rust tree's harness:
there, tests shared one set of tables and a `truncate` was how a test got a clean
one. Here each test starts from a schema that did not exist a moment earlier, so
there is nothing to empty, and the only unqualified-SQL hazard that matters is
gone with it. What reaps a test is `drop schema … cascade`, and
`droppable(schema)` refuses anything whose name is not `t_` plus twelve characters
from the fixed alphabet — the guard is a prefix test plus a length test, because
`drop schema` is unconditional SQL and this function is the only thing standing
between a bug in `uniqueSuffix` and a developer's `public`.

Two reasons it is a schema and not something else, both still live:

- **Not a database per test.** `TEST_DATABASE_URL` names one database, and
  changing its shape is a different decision than this one.
- **Not a mutex.** That is `--test-threads=1` under another name, and it costs
  the whole suite to protect one test. The migration step serialises behind
  `pg_advisory_xact_lock(hashtext('darkroom.pgtest.migrate'))` — a Postgres lock,
  not a process one, because `go test ./...` runs several packages as separate
  processes and six concurrent `0004` applications failed with
  `tuple concurrently updated` on a loaded machine. Everything else — connecting,
  seeding, querying, dropping — runs concurrently.

*(The Rust tree's third reason is history and does not apply here: `Service` there
held a transaction open across an `await` on object storage, so a test's own
writes were not visible inside one. In this tree `CompleteUpload` does its `Head`,
its `Get` and its hashing BEFORE `InAccountTx`, and the transaction covers only
`markReady` and the `enqueue` beside it.)*

The search path is the one schema and **not** `t_<hex>, public`, and the reason
was measured rather than reasoned about: the fallback does not break the
migrations (unqualified DDL targets the first schema on the path, and
`if not exists` is checked there). What it costs is quieter — a table this
schema does not have but `public` does resolves to `public`'s copy, so the same
query returns `relation does not exist` without the fallback and another test's
rows with it.

`internal/darkroomdb/pgtest`'s `guard_test.go` and `pgtest_test.go` hold all of
it, and split it so the half that
needs no Postgres runs on a bare machine: the name's alphabet, the check on the
two statements that interpolate one, and the stamp a `drop schema` is only
allowed to act on. The counts and the proofs are in
`REPORT-darkroom-hermetic-db-01.md`.

### `openapi/v1.yaml` and the router, held to each other

*(Historical. This section described `tests/openapi_document.rs` comparing a
hand-maintained `http::OPERATIONS` table with the document. In the Go tree there
is no table to maintain and no such file — see below, which is the stronger
version.)*

`internal/httpapi/routes_test.go` compares the published document with the routes
the service actually serves, **in both directions**: an operation the document
describes and the router does not serve is a generated client that 404s in
production, and a route the router serves that the document does not describe is
a method the generated client does not have. It follows courier's
`test/courier_web/openapi_document_test.exs`, which is the same tripwire shape
pantry's has fired three times against.

It compares **keys, never counts** — a count comparison passes on a rename and
fails on a pure addition, which is backwards. The key is `(method, path)`, and the
method being part of it is exactly what the Rust tree could not get: there, "the
method a `MethodRouter` answers is baked into the value `get(handler)` returns
and there is no accessor for it", so the drift check could only compare paths.
Both of its readers raise rather than under-read: `declaredRoutes` fails if the
document yields zero operations, `servedRoutes` fails if `chi.Walk` finds none,
and a green check over nothing is worse than no check. The document is **parsed
as YAML, not scanned** for `operationId:` lines, because a scan reads lines
without knowing they sit under a path and a method.

**THE ROUTER'S SIDE IS NOW THE DOCUMENT, NOT A TABLE — AND THE REASON IS WORTH
THE WHOLE SECTION.** The Rust tree's problem was that axum cannot be asked what
it routes — there is no `Router::routes()` and nothing to reflect over — so
`src/http.rs` carried a hand-maintained `http::OPERATIONS` table of
`(method, path, handler)` that `router()` folded over. A hand-maintained
inventory is a second copy that goes stale silently, and the Rust README admits
the cost: a planted `.route()` on a new path left all nine drift tests green.

There is no table here. `internal/api/api.gen.go` is generated from the document
by `oapi-codegen`, it is a `*chi.Mux`, and `internal/httpapi.New` walks THAT
router with `chi.Walk` and re-registers every route it finds. So:

- there is no path list in this repository to forget;
- a new `operationId` is a new method on the generated `ServerInterface`, and not
  implementing it **fails the build** rather than 404ing in production;
- a route that is not in the document cannot be reached, because the only thing
  that registers routes is code derived from the document.

And `bin/prime` regenerates and diffs on every run, so the generated file is the
document rather than a stale copy of it.

**What this closes, which is why the walk is the whole answer.** In the Rust tree
the router-side probe read a path it already knew about, so it caught a method
registered on a known path outside the table and did **not** catch a whole new
path registered outside it. Both halves were measured by planting a `.route()`
call beside the fold: a planted `put` on `/v1/assets` turned the method test red
while both document tests stayed green, and a planted `get` on `/v1/brand-new`
left all nine tests green. "Nothing in axum enumerates the paths a `Router`
holds, so that gap is not closable from inside a test." `chi.Walk` enumerates
them, so here there is one inventory and neither gap has a shape.

The other half of that is asking the router directly: a path it does not know is
`404`, and `TestAnUnroutedPathIsTheSameDocument` is the control that says the
distinction is real — it requires `404`, code `not_found` and
`application/problem+json` from `/v1/nope`, from `/`, and from `POST /healthz`,
which is a verb the document does not pair with that path.

**`/healthz` and `/readyz` are in the document**, under a `probes` tag with
`security: []`, and there is no omission list at all: the document's own
`security: []` IS the exemption. courier excludes the probes; excluding them here
would mean deleting correct documentation to satisfy a carve-out.
`TestEveryUnauthenticatedRouteIsOneTheDocumentExempts` reads that out of the YAML
in **both** directions — nothing is served without a gate unless the document says
so, and nothing the document exempts is missing from the router, so a rename
cannot hide behind a stale carve-out.

This check needs neither a database nor a socket, so it is in the default tier
and runs on a bare machine. It reaches the document through a hardcoded
`../../openapi/v1.yaml` rather than through `exposes.api` in `cafaye.yml` — the
Rust tree read the service's own pointer and this one does not, which is a real
difference and a small gap: the two files could be pointed at different
documents without this test noticing.

### The coverage gate

Coverage is not in `bin/prime` and is not the same thing. It is kit's
`coverage-fail-under` input, set to **50**, against **53.73%** measured line
coverage on rustc 1.95.0 (60.95% regions, 55.85% functions). A floor rather
than a ratchet: it fails if coverage collapses and stays out of the way of a
legitimate change. `--fail-under-lines 50` was measured green and
`--fail-under-lines 54` was measured red, so the gate can actually fail — and
kit's default of 0 fails nothing, which by kit's own rule is not a gate.

Three things it does not measure, stated rather than implied:

- the database tier, because kit's coverage step runs the default `go test
  ./...`, and the tests that need a database SKIP in it;
- `internal/service` and `internal/httpapi`, which are largely absent from the
  default suite for that same reason;
- `cmd/darkroom`, at ~0%, because a binary's `main` is never called by a test.

**THE FLOOR IS NOW A MEASURED GO NUMBER.** `go test ./... -coverprofile` against
the default (no-database) suite reports 54.6% line coverage, and `ci.yml`'s
`coverage-fail-under` is 50 — a floor that can actually fail, measured on
Go 1.26.1 rather than carried over from the Rust tree. The database tier and
`cmd/darkroom` are still excluded (see above), so the real number is higher; the
measurement is the floor's honest lower bound, not a claim about the whole
service.

No test in this repository opens a socket to anything but the database named by
the environment and the JWKS server two packages stand up for their own verifier.
HTTP tests drive the router through `httptest.NewRecorder`, which is the Go
spelling of `ServiceExt::oneshot` — a function call rather than a network round
trip. The S3 backend's
tests presign against the real SDK with static dummy credentials, which is a
local operation — it builds a URI and a signature and sends nothing.

No sleeps anywhere. The one test that needs an expired presign URL uses a
zero-second TTL and asserts the fake refuses it — not a one-second sleep, which
would be a flake waiting for a loaded CI box.

## Not done, and why

- **Streaming verification for large objects.** `complete` reads the object
  through `objectstore.Store.Get` and hashes it, so a 1 GiB upload is buffered
  in the API process at complete time. It is released as soon as it is hashed,
  and the hash itself is already available in a streaming form
  (`checksum.SHA256HexStreaming`, tested equivalent to the one-shot path), but
  the interface hands over `[]byte` and the read is what it is. The honest
  options are a sixth interface method that streams, or a lower
  `objectstore.MaxUploadBytes`; both are contract changes and neither belongs
  in the change that made R2 work.
- **A live R2/S3 integration test.** `internal/objectstore/integration_test.go`
  now contains `TestIntegrationRoundTrip`, which does a presign/put/head/get/hash/
  delete round trip against a real bucket. It is guarded by
  `DARKROOM_TEST_S3_BUCKET/ACCESS/SECRET` and skips on every default `go test`
  run, so no credential reaches CI. What is still manual is pointing it at a
  bucket and reading the result; nothing in the suite runs it automatically.
- **~~CI does not build the deployment target.~~ FIXED BY THE REWRITE.** The Rust
  `docker/Dockerfile` ran `rustup target add x86_64-unknown-linux-musl` and then
  copied out of `target/x86_64-unknown-linux-musl/release/darkroom` while its
  `cargo build` had no `--target`, so the file it copied was not there; and
  `ring` needed a C toolchain for that target, so a glibc runner could not link
  it either. The Go `docker/Dockerfile` is `CGO_ENABLED=0 go build`, which
  produces a statically linked stripped binary with no musl target, no C
  toolchain and no `rustup`. There is no equivalent of the missing `--target`
  because there is no equivalent of the problem. It is not "pending a decision
  about the release pipeline": the decision was answered by a different language.
- **The remaining bullet above is what a green badge here does not currently
  mean:** that a real bucket accepts what darkroom signs. The deployment target
  is no longer one of them.
- **Nothing about the outbox is outstanding any more, and the entry that used to
  be here said the publisher loop needed a NATS client and a deployment. kit's
  BRK-1 installed one and BRK-5 built the loop.** What is still missing is
  operational rather than structural: no stream is declared by kit's compose, so
  `darkroom.>` has to be added by whoever owns the broker before an event
  leaves the table. The failure until then is loud — `attempts` climbing against
  `published_at is null` — which is the direction a misconfiguration should fail
  in.
- **The stale-`pending` sweeper.** `darkroomdb.FailStalePending` and
  `service.FailStalePending` now exist and are tested (per account, since the
  `assets` table is account-scoped under FORCE RLS): a `pending` asset older
  than `service.PendingSweepAfter` (twice the presign TTL) is marked `failed`.
  What is NOT here yet is the cross-account driver: a sweep over every account
  needs a BYPASSRLS role or a SECURITY DEFINER function, which the substrate does
  not provision. Until it does, the sweeper is called per account by whoever
  owns the scheduler. `service.PendingSweepAfter` still lives in
  `internal/service` so the cutoff is not duplicated.
- **~~The idempotency-key retention sweep.~~ FIXED.** 24 hours per core;
  `darkroomdb.DeleteExpiredIdempotencyKeys` and
  `service.DeleteExpiredIdempotencyKeys` now exist and are tested, deleting rows
  whose `created_at` is older than the cutoff. The table carries no `account_id`
  and no RLS, so the sweep runs as the login role without an account context.
  The index (`idempotency_keys_created_idx`) makes it a range scan.
- **A concurrent-replay test.** Two identical keyed requests arriving at the same
  instant is the case the ledger's own unit test covers by reserving twice in one
  goroutine. End to end through the router it would need real concurrency, and the
  honest options are a barrier or a sleep — a result that depends on scheduling.
  A flaky test is worse than an absent one, so it is absent and named here.
- **~~The two `oapi-codegen` binding refusals that bypass this service's
  envelope.~~ FIXED.** `httpapi.New` now passes `bindingErrorHandler` as
  `api.ChiServerOptions.ErrorHandlerFunc`, so a malformed or duplicated
  `Idempotency-Key` (and any other binding failure) is answered as
  `application/problem+json` with code `validation_failed`, like every other
  refusal. `internal/httpapi` tests cover the uniform envelope.
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
  event-declaration checks are here; `cafe contract test` against a live
  darkroom is not wired into CI because there is no deployed instance yet.

## License

**The grant is the Business Source Licence 1.1.** See [LICENSE](LICENSE), which
carries the parameters that make it this repository's: Licensor `Cafaye`,
Licensed Work `darkroom`, Additional Use Grant `None`, Change Date
`2030-10-10`, Change Licence `Apache-2.0` (`LICENSE:3-9`). Free to run, free to
self-host, never to offer a competing service — and on the Change Date it
becomes Apache-2.0 whether or not anyone asks, because that flip is written
inside the licence text rather than promised somewhere else.

**It was MIT.** This repository shipped MIT from the day it acquired a licence
file (`CHANGELOG.md:222`), and the reasoning recorded then still holds: darkroom
is a platform a consumer depends on rather than reads, so the grant has to
leave the consumer's own situation alone — MIT does that, and a copyleft licence
would make every downstream inherit an obligation, which is the opposite of
what a service registry is for. What changed is which grant the fleet puts on a
**service**, not that reasoning. OPS-D2 draws the line at the adoption job
versus the running job — framework permissive, services under BSL — and
darkroom is on the service side of it. This is a deliberate move DOWN a
permission ladder, taken out loud here rather than by drift, because a grant
that quietly contradicts the fleet's stated policy is the thing that makes
every other licence claim in the platform untrustworthy.

**Anyone who already holds a copy keeps the grant it was published under.** An
MIT copy of this repository is still an MIT copy; nothing above re-licenses
what someone already took, and the BSL grant governs copies obtained from here
on. That is the whole of the obligation owed to existing holders, and it is a
matter of the grant they accepted rather than a courtesy.

**Where the machine-readable declaration lives, and where it does not.**
`info.license` in `openapi/v1.yaml:84-92` — the document `cafaye.yml`'s
`exposes.api` points at — carries `BUSL-1.1` with the SPDX identifier and page.
`cafaye.yml` itself carries no licence field and cannot: core's manifest schema
sets `additionalProperties: false` with no licence property, so a `licence:` or
a `license:` key there is rejected by `cafe contract lint`, which CI runs against
this exact file (`.github/workflows/ci.yml:303-307`). Both spellings were added
to a copy of the manifest and both failed with `declares unknown key`, so the
manifest states the grant and the reason in a comment (`cafaye.yml:25-43`)
rather than breaking the schema to make a field agree. Adding the field is
core's packet: the schema and cafe's vendored pin of it
(`cafe/internal/contract/schemas/manifest-0.2.json`, pinned to core `3340e0f`)
have to move together, and neither file lives in this repository.

