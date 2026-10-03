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

- **The database tier runs in parallel. `--test-threads=1` is gone from
  `bin/prime`, and the reason it was there is gone with it.**

  Every `test_store()` now creates a schema named `t_` plus eight hex characters
  of a fresh uuid, applies the migrations **into that schema**, and builds its
  pool with `set search_path to <that schema>` in `after_connect`. `truncate`
  still runs and still names its tables unqualified — that is the isolation: the
  names resolve through the search path, so it empties this test's tables and
  cannot reach another's.

  The flag was a real constraint rather than caution. Six parallel runs on the
  tree before this change went red every time, and the symptom was the worst kind
  to debug — an assertion about rows that genuinely were not created, e.g.
  `NotFound { detail: "asset not found" }` on a test that had created the asset
  two lines earlier, and `left: 4, right: 2` from a test counting rows another
  test had deleted. Three runs after it went green, in both feature sets, with
  the serial run still green.

  Two properties were measured rather than assumed, and one of them corrected an
  assumption this work started with: `sqlx::migrate!` creates
  `_sqlx_migrations` unqualified, so it lands in the per-test schema — one ledger
  per schema, confirmed from the catalog — while `search_path = t_<hex>, public`
  would **not** have made the migrations no-ops, as intended. What the fallback
  would cost is quieter: a table the test's schema lacks but `public` has
  resolves to `public`'s copy, so the path is one schema and nothing else.

  `tests/schema_isolation.rs` holds the property, and splits it so the half that
  needs no database runs on a bare machine: the name's alphabet, the check on the
  two statements that interpolate one, and the stamp a `drop schema` may only act
  on. `DB_TESTS` 54 → 58, with two new constants for the new file's two halves.
  Every test that needs the database was isolated; **nothing needed its own
  exclusion**, and re-adding a suite-wide flag would undo the isolation for the
  other fifty-seven.

### Added

- **`LICENSE`: darkroom is MIT.** The repository shipped no licence file, which
  is not "unlicensed, therefore free" — it is **all rights reserved**, the
  default copyright position when a public repository grants nothing.

  `Cargo.toml` already declared `license = "MIT"` and is now backed by the grant
  itself. darkroom is a platform a consumer depends on rather than reads, so the
  licence has to leave the consumer's own situation alone: MIT does, and
  copyleft would not.

  The copyright line matches the three repositories that already shipped a
  licence exactly: `Copyright (c) 2026 cafaye`.

**Fixed: three guards that could not fire, and a test that failed on half of all
runs.** The isolation work below was recovered from an OOM restart and did not
pass as it stood. Re-running it found four defects, all of the same family — a
check that cannot fail, or one that fails for a reason unrelated to the property
it asserts — and the three guards are worth more than the feature they guard:

- **The structural scanner was quadratic and the guard hung instead of running.**
  `sql_statements` left its cursor on a closing quote rather than past it, so
  every literal bought a fresh forward scan of the rest of the unit, and
  `src/store.rs`'s doc comments — full of `"` — made the input far larger than
  the SQL. Two tests in `tests/tenant_scoping.rs` burned eleven minutes of CPU
  and were still running when the gate was killed at the machine's limit. The
  packet's central claim, that a bare machine checks the structural half, was
  **false** until the scan was linear. One character, plus a
  not-one-past-the-end case for a stray quote in prose.
- **The "a mutation must name a row" check was satisfied by `account_id` itself.**
  It asked for `sql.contains("id = $")`, and that substring occurs *inside*
  `account_id = $2`. So `update assets … where account_id = $2` — which rewrites
  every pending row an account has ever uploaded, the exact defect the check
  exists to catch — passed the whole file, and cargo reported `11 passed; 0
  failed` with that whole-account update in the source. The match is now anchored
  to a whole identifier, and `naming_a_row_is_anchored_to_a_whole_identifier`
  asserts the anchoring against the exact mutation that defeated it. An unanchored
  `contains` was not a weak test here; it was an inverted one.
- **The scoped-query set was compared as a sequence.** The derived side is source
  order and the constant is grouped by operation kind, so the assertion failed on
  a tree where both lists held the same ten names. Now a sorted set, with the
  kind grouping asserted separately by
  `the_operation_kinds_are_what_the_comment_claims` so the file's coverage claim
  is load-bearing rather than a comment that can rot.
- **`one_accounts_key_cannot_replay_another_accounts_response` compared
  uuid-ordered rows against declaration-ordered expectations**, so it failed on
  roughly half of all runs with `left: [c494acf5, …] right: [f9ff6853, …]` — a
  failure that reads exactly like a cross-tenant leak and is not one. Both sides
  are sorted now.

A guard nobody has watched fail is a guard nobody knows works, and two of these
were only found because divergences were planted in `src/store.rs` and reverted.
`bin/tier-counts` moves `TENANT_SCOPE_TESTS` 10 → 12 for the two new structural
tests.

**Tenant isolation, made load-bearing.** The implementation was already correct:
`assets` and `asset_variants` carry `account_id uuid not null`, there is a
`unique (account_id, checksum)`, and all ten account-scoped queries constrain on
the column. What was missing was anything that would notice if a future edit
dropped `and account_id = $2` from one of them — every existing test exercised
the queries *through* a `Tenant`, so all of them stayed green and a private asset
store became a shared one. Correct by construction is not a property if the
construction is never checked.

**`tests/tenant_scoping.rs` — the structural half, in the default tier.** It reads
`src/store.rs` with `include_str!`, so the check is a fact about the source that
built the binary rather than a grep somebody has to remember to run, and it
derives its expectations from the code rather than from a maintained list. Five
properties: every function taking a `&Tenant` constrains `account_id = $N`;
that set is exactly the ten named in the file; every mutating scoped query names
a **row** as well as an account, because `where account_id = $2` alone is scoped
and still a defect; exactly one query reads across accounts, and no request-path
module can reach it; `principal_scope` carries the account, because the
idempotency ledger stores whole response *bodies* and an unscoped replay hands
one account another's response; and the four-reads / two-updates / one-delete
breakdown is asserted rather than described. It needs no database, so the
structural half of the isolation guarantee is now checked on a bare machine.

**`tests/query_scoping.rs` — the behavioural half, against Postgres.** A
two-account fixture covering read, list, update and delete, which is the shape a
service is usually broken in: it scopes its reads and forgets its delete. Both
accounts deliberately share a **checksum** — `unique (account_id, checksum)`
makes that two legitimate rows — because that is where a scoping bug hides: an
unscoped lookup by checksum does not error and does not return nothing, it
returns a row and the wrong one, while every id-keyed test stays green. Also
covers a cross-tenant `mark_failed` (the failure path is a bare `where` clause,
so it is the easiest of the three writes to get wrong), a cross-tenant
`list_storage_keys` (two `select`s in a `union all`, so the query most able to be
half-scoped), and a cursor replayed from one account into another.

**Every tenant-scoped route now has a negative case, and the enumeration is
derived rather than trusted.** `tests/tenant_isolation.rs` grew from 7 to 12
cases; the coverage is not taken on faith, because `tests/tenant_scoping.rs`
builds the route list from `http::OPERATIONS` — the table the router is built
from — and fails if a route has no case, if a case has no route, or if a case
names a test that is not there. The byte-identical-404 guard now covers all five
id-scoped routes rather than only `GET /v1/assets/{id}`: a rule checked on the
oldest route is a rule checked on one route, and the route somebody added most
recently is where it would be dropped. New cases: a cross-tenant `complete`
proving A's upload is still `pending` afterwards (a 404 on its own is compatible
with "did the work, then reported 404"), a cross-tenant variant write proving
nothing moved including the outbox, a variant listing that is 404 rather than an
empty array, a cursor replayed across accounts, and the same bytes in two
accounts never yielding a presigned URL scoped to the other one's storage key.

**One account's `Idempotency-Key` cannot replay another account's response**
(`tests/idempotency.rs`). The ledger is the only table here that stores a whole
HTTP response body, and `reserve_idempotency_key` hands it back on a replay, so
an unscoped replay is not a status-code leak — it is another account's asset id
and presigned upload URL. Previously covered only by a unit test on the scope
string.

**Fixed: the `Forbidden` variant's doc comment described the anti-pattern.**
`src/error.rs` documented 403 as "the caller authenticated and holds the scope,
but the account in the token is not theirs" — stating, as the variant's purpose,
exactly the thing this packet exists to prevent, and contradicting itself two
clauses later. No behaviour was wrong: `Error::forbidden` has one constructor
called from one place, `http::require_scope`, so a 403 cannot become a tenancy
response by accident. But a comment is what a future author reads before writing
`if caller.account != asset.account { return forbidden() }`, and this one invited
it. It now states the capability case positively, names the tenancy case as
`NotFound`, and explains why the two differ in kind rather than in degree — a
missing scope is a fact about the caller, which they already know, while a
foreign resource is a fact about somebody else, and reporting it is the leak.

**The rule, stated once in the module docs for the platform to copy:**
`src/store.rs` and `AGENTS.md` now carry *absence, not refusal* — a resource the
caller cannot see does not exist, and every layer below the wire says so the same
way. A 403 is a confirmation; it tells a caller the row is real and somebody else
owns it, which is a smaller leak than the row and a perfectly good way to
enumerate the platform. This includes below the wire: `store::find_asset` returns
`Ok(None)`, never a `StoreError`, because a distinguishable error is the same
oracle one layer down.

`bin/tier-counts` gains `TENANT_SCOPE_TESTS=12` and `QUERY_SCOPING_TESTS=7`, and
`DB_TESTS` moves 41 → 54 for the 13 new `#[ignore]`d cases. The skipped-equals-run
identity is now 54 == 54.

**Two environment notes, because both read as code defects and are not.** Port
5432 on this machine is already bound by a host Postgres and another container, so
`docker compose up -d postgres` starts a container whose documented URL then
connects to *somebody else's* database and fails with `role "darkroom" does not
exist` — which reads like a missing migration. And these tests truncate the tables
they touch, so a checkout sharing a database with another worktree destroys each
other's fixtures mid-assert: three `tests/api.rs` failures observed during
re-verification were another checkout's `--db` run. Run the database tier against
this worktree's own Postgres. Relatedly, `./bin/prime --db | tail` reports exit 0
on a run that failed `fmt`, `clippy` and three suites — the `gate.yml` note about
pipelines under zsh, met again in practice.

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
