# AGENTS.md

Conventions for `darkroom`, the cafaye media service. Read this before changing
anything; the house rules in `moon/PLAN.md` §1 and §3 apply on top of it.

## What this repository is

`github.com/cafaye/darkroom`, Rust 1.95, one static binary. It owns uploaded
media: signed uploads, a tenant-scoped asset registry, derived variants. The
contracts are owned by `cafaye/core`; CI and lint config come from `cafaye/kit`.
Neither lives here.

## Layout

```
src/http.rs         routes, handlers, probes, auth middleware    (the wire)
src/service.rs      the upload state machine, every decision      (the rules)
src/domain.rs       assets, variants, enums                       (the vocabulary)
src/auth.rs         token verification, Principal, Tenant         (who is calling)
src/objectstore/    the storage trait, in-memory + s3 (aws, r2)  (where bytes live)
src/store.rs        sqlx, the outbox, the idempotency ledger      (persistence)
src/outbox.rs       the event envelope                            (the contract)
src/checksum.rs     sha256, and the rules about trusting it
src/variants.rs     resize + re-encode
src/storage_key.rs  opaque key generation
migrations/         plain SQL, applied by a deploy step
openapi/v1.yaml     what exposes.api points at
bin/prime           the gate
tests/              the database suites, all #[ignore]d, plus the backend table
```

Dependencies point downward only. `service.rs` knows nothing about axum;
`domain.rs` knows nothing about SQL; `objectstore` knows nothing about assets.
That is what lets the state machine be tested without a socket and the storage
scope without a database. **Do not add an `upward` import** — the moment
`service.rs` imports axum, the HTTP layer stops being a translation and the
test for a decision needs a request.

## Rules

**Tests first.** Write the table, watch it fail, then implement until green
(PLAN.md §3). Every handler change needs a case asserting the status code
*and* the JSON shape.

**`Tenant` is the only way to scope a query.** `auth::Tenant` has exactly one
constructor and it takes a `Principal`. If you find yourself wanting
`Tenant::from_request`, you are about to build a cross-tenant vulnerability.
A repository method that does not take a `Tenant` will not typecheck, which is
the point.

**Cross-tenant is 404, not 403.** And the body must be byte-identical to the
one for an id that never existed. A 403 leaks existence; a different body is
just as much of an oracle as a different status.
`a_cross_tenant_404_is_indistinguishable_from_a_missing_one` is the regression
guard, and it covers **all five** id-scoped routes, not just `GET /v1/assets/{id}`
— a rule checked on the oldest route is a rule checked on one route.

**The rule is absence, not refusal, and the rest of the platform should copy it.**
Every service that holds another service's data has to answer "what do I say when
the caller is authenticated and the row is not theirs?" and there is only one
correct answer: the same thing you say when the row does not exist. A refusal
distinguishes the two cases, so a caller enumerating ids gets a directory of
every asset on the platform without reading a single row. This includes the
layers below the wire: `store::find_asset` returns `Ok(None)` for another
account's row, never a `StoreError`, because a distinguishable error is the same
oracle one layer down.

**The checksum is computed, never trusted.** The client's claim is written at
create and compared against at complete; the row ends up carrying the computed
value. Do not add a path that stores the claimed one.

**The checksum is computed from the bytes, never from a backend's claim about
them.** `complete` reads the object back and hashes it, on every backend, and
`ObjectStore` has no method that asks a backend what checksum it recorded. It
used to, and the property it gave existed on AWS S3 and did not exist on
Cloudflare R2 — R2 offers `FULL_OBJECT` for CRC-64/NVME only, so a `FULL_OBJECT`
sha256 never came back and the check silently stopped happening. A correctness
property may not depend on which bucket answered it. If you are tempted to make
the read conditional on a cheaper answer existing, you are rebuilding the bug:
`tests/checksum_verification.rs` asserts the read count is exactly one, and the
one read is the trade stated in the README.

**One `ObjectStore` implementation, several S3-compatible services.** R2 is not
a variant, a feature or a second type: it is an endpoint, a region, and
addressing. If a backend needs a *code* branch, the abstraction is wrong and
fixing the abstraction is in scope — that is not a reason to add
`R2ObjectStore`.

**One way to emit an event.** `Outbox::enqueue` takes a `&mut Transaction`, and
that is the entire design. A function without a transaction has no way to
publish. If you find yourself wanting an `enqueue_after_commit`, you have
described the bug `core/docs/event-outbox.md` is about.

**No event for a `pending` asset.** Nothing is in storage, so every consumer
would fail. `darkroom.asset.ready` is at complete, and that is the moment the
bytes exist.

**No event for a failure.** A consumer cannot act on "this upload did not
complete" in a way that is better served by the client receiving its 4xx.

**Liveness never touches a dependency.** `/healthz` is unconditional. A
database outage must not get the process restarted out from under in-flight
uploads; that is `/readyz`'s job, and it really does `select 1`.

**The probes are exempt from auth by an explicit allow-list**, not by route
order. `Router::layer` in axum applies to every route the router holds, so
registering a route "before" the layer does not exempt it. There is a test.

**Probe failures are logged, never returned in detail.** The body names the
dependency; the underlying error goes to the log. An unauthenticated caller
must not learn that a database host is `10.0.0.5`.

**Migrations are a deploy step, not a boot step.** Nothing in `main` applies
one. Never edit an applied migration; write a new one. Every migration needs a
`Down` or a comment saying why it cannot be reversed.

**No foreign keys across services.** `account_id` and `owner_user_id` are
opaque UUIDs with no `references` clause. A cross-service FK is a release-order
coupling and a shared outage wearing a constraint's clothes, and Postgres cannot
enforce it across two connections anyway. The one real FK is
`asset_variants.asset_id` → `assets.id`, because both are ours.

**Storage keys are never client-supplied.** A client-chosen key collides across
tenants, traverses, and is guessable. `StorageKey::generate_*` is the only
constructor. If you need a key shape change, change the generator and the
migration together.

**`Asset` has no `storage_key` field.** It is a server-side detail and a client
that can read it will try to build a URL from it.

**Deps need a cause stated in review.** The full list with reasoning is in
README.md. No dependency without one, and no feature flag that a test can reach
the network through.

**No `unsafe`.** There is none in this repository. Do not add any without a
written justification next to it.

**No secrets in the repo.** Object-storage credentials are the AWS SDK's own
chain. `DARKROOM_DEV_JWT_SECRET` is read at runtime, never baked in with
`option_env!` — a secret in the binary is a secret in the image, in the build
cache, and in `docker history`.

**Comments say why.** Explain the decision and the constraint, not the
mechanism. A comment restating the line below it is noise.

**Stubs stay honest.** Something not written is absent, not a `not implemented`
fake that looks finished. README's "Not done" is the source of truth.

## Tests that need a database

`cargo test` is green on a bare machine with no Postgres and no Docker. The
integration suites are `#[ignore]`d and need `TEST_DATABASE_URL`:

```sh
docker compose up -d postgres
TEST_DATABASE_URL="postgres://darkroom:darkroom@localhost:5432/darkroom_test?sslmode=disable" \
  cargo test -- --ignored --test-threads=1
```

`--test-threads=1` is a fixture-isolation requirement, not a race workaround:
each test truncates the tables it touches, and two tests truncating
concurrently would delete each other's fixtures mid-assert.

A skip is honest; a test that silently passes without proving anything is not.
**If you add a `#[ignore]`, add the job that runs it** — CI's `gate` job calls
`bin/prime --db` and is the only reason the tenant-isolation suite is real
rather than decoration.

## The exit code is not the evidence

`bin/prime --db` exits zero when it is happy, and `cargo test` exits zero when
it ignored every test it could. Those are the same exit code, so CI reads the
counts instead, with `bin/tier-counts`:

```sh
./bin/prime --db 2>&1 | tee /tmp/prime.log
./bin/tier-counts /tmp/prime.log
```

It asserts 77 unit / 89 with s3 / 9 behaviour-table rows / 54 database twice /
10 tenant-scoping twice / 7 query-scoping, **and** one identity: the count the
default run skips equals the count the database run passes, because they are the
same tests. **Adding a test means raising the number in `bin/tier-counts` in the
same commit** — that is the point of the constant, and a CI red that says
`expected 54, got 57` is the mechanism working, not failing.

**A query's tenant predicate is checked as a property of the source, not by
convention.** `tests/tenant_scoping.rs` reads `src/store.rs` with
`include_str!` and fails if any function taking a `&Tenant` stops constraining
`account_id = $N`, if that set of functions changes, if a mutation names an
account without naming a row, or if a second query starts reading across
accounts. It is in the **default tier on purpose** — it needs no database, so
the structural half of the isolation guarantee is checked on a bare machine. The
behavioural half, a two-account fixture against real Postgres covering read,
list, update and delete, is `tests/query_scoping.rs`.

Neither file is decoration and both are pinned in `bin/tier-counts`
(`TENANT_SCOPE_TESTS`, `QUERY_SCOPING_TESTS`). A check nobody's gate runs is a
check that proves nothing, and a security check that reports zero is worse than
one that is absent: it looks like coverage.

## No object-storage credentials in CI, and none needed

The `s3` tier needs no endpoint and no credential, which is worth being exact
about. `tests/storage_backends.rs` presigns with the real SDK and static dummy
credentials; presigning builds a URI and a signature locally and transmits
nothing, so the nine-row table runs offline against AWS, MinIO and R2
*configurations*. Adding a MinIO service container to CI would boot a bucket
that nothing in the suite ever speaks to — a green checkmark on nothing. What
that table cannot prove, and does not claim, is that R2 *accepts* the URL; the
README's "Running against R2" is the manual procedure.

## The toolchain is pinned

`rust-toolchain.toml` is the pin, and CI asserts `rustc --version` against it
rather than trusting it. It is the same release as `mise.toml`, `Cargo.toml`'s
`rust-version` and the Dockerfile's `RUST_VERSION`; if you raise one, raise
them in the same commit. kit's shared job calls
`dtolnay/rust-toolchain@stable` with no `toolchain:` input, so *that* job runs
on floating stable regardless of `versions:` — which is why the pin is enforced
in the `gate` job and not only declared in the shared one.


## No network in tests

No test opens a socket to anything but the database named by the environment.
Two structural reasons, not two promises:

- The AWS SDK is not a default dependency, so the default build **cannot
  construct a real object-storage client**.
- HTTP tests drive the router with `tower::ServiceExt::oneshot` — a function
  call, not a round trip.

The S3 backend's tests presign against the real SDK with static dummy
credentials, which sends nothing: presigning builds a URI and a signature
locally. That is what lets `tests/storage_backends.rs` assert what darkroom
would put on the wire for AWS, MinIO and R2 — the credential scope, the signed
headers, the absent headers — without a bucket. A real bucket is a manual
verification step and the README says how.

The in-memory store's presigned URLs are really signed and really scoped, so a
bug in the scope or the TTL fails a test instead of shipping.

## No sleeps, no raised retries, no loosened assertions

PLAN.md §3. A test that needs an expired URL uses a zero-second TTL. A test
that needs a port that is closed connects to port 1. A test that needs a
duplicate uses a real second request. None of them waits.

## Gates

```sh
./bin/prime          # fmt, build, clippy, test, and test --features s3
./bin/prime --db     # + the ignored tests against Postgres, both feature sets
cargo fmt --check
cargo clippy --all-targets --all-features -- -D warnings
cargo build --all-targets --all-features   # the feature-gated paths
cargo llvm-cov --fail-under-lines 50       # the coverage floor, and only this
./bin/gate-self-test # is gate.yml still true of this repository
```

**`./bin/prime --db` is the gate, and `mise run prime` is not.** `gate.yml`
declares the former, and the flag is in the declaration rather than implied by
it: without `--db` the 41 database tests report as `ignored` and the run still
prints `==> ok` and exits 0. If you are landing a change, run the declared gate.
`gate.yml` is core's format — read it before changing it, and read what it says
about this repository's own requirements.

`bin/gate-self-test` needs a Postgres at `$TEST_DATABASE_URL` for the cases that
run the gate, and it reports a skip as a failure rather than a pass.

All green before a commit lands. The `--all-features` build matters: `s3` and
`dev-auth` are behind features, so a default build compiling clean says nothing
about them. **So does the `cargo test --features s3` tier**, for the same
reason in the other direction: `cargo test` does not compile the R2
configuration rules, the presigned-URL table or the startup refusals at all, and
a test nobody's gate runs is a test that proves nothing.

The coverage floor is deliberately loose and deliberately not in `bin/prime`:
instrumenting is a different build, and folding it into the gate would make
every developer run twice. It measures the default suite only — not the s3
build, not the database tier — and README says so where a reader will look.


## Adding an endpoint

1. `service.rs` — the rule, in a method that takes a `Tenant`. Tests first.
2. `http.rs` — the handler, extracting and translating, with nothing decided,
   **and a row in `http::OPERATIONS`**, which is the table `router()` is built
   from. There is no other way to register a route.
3. `openapi/v1.yaml` — the path, the errors, and `info.version` if anything
   else in the document moved.
4. A test asserting the status code, the JSON shape, and the anonymous case.
5. A row in README's endpoint table.
6. A new constant in `bin/tier-counts` if the new test lives in its own file, so
   a file that stops running fails the gate instead of reporting zero.
7. `./bin/prime`.

## Adding an event

1. A variant on `outbox::EventType`, and the test asserting three segments and
   the publisher prefix.
2. `cafaye.yml` `exposes.events` — and the test that counts both directions.
3. The catalog row in `core/docs/event-naming.md` **and** a payload schema in
   `core/schemas/events/`, which is a core change, not this repository's.
4. Enqueue it in the same transaction as the domain write.
5. A test that a rolled-back transaction emits nothing.
