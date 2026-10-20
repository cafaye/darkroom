# AGENTS.md

Conventions for `darkroom`, the cafaye media service. Read this before changing
anything; the house rules apply on top of it — SSH remotes for the repositories cafaye owns, and tests first: write the test, watch it fail, then implement.

## What this repository is

`github.com/cafaye/darkroom`, one static binary. It owns uploaded media: signed
uploads, a tenant-scoped asset registry, derived variants. The contracts are
owned by `cafaye/core`; CI and lint config come from `cafaye/kit`. Neither lives
here.

**ONE LANGUAGE. The Go rewrite is the whole tree.** `src/` and `tests/` were
deleted in darkroom-go-06 and they were a *gated* tree until then: for six
packets `bin/prime` ran five cargo steps beside six Go steps, because "a gate
that runs only one of two implementations stopped checking the other the day the
second one appeared." The cargo steps are REMOVED, not commented out — a gate
step that references a deleted `Cargo.toml` is a gate that does not run, and a
commented-out one is a line nobody reads.

Where this file says "the Rust tree did X", it is describing history and the
history is in git. Every such sentence now names the Go mechanism that replaced
it, because a rule with no mechanism behind it is a rule that decays.

## Layout

```
cmd/darkroom/               main, and the configuration table. The only package
                            that reads the environment.
internal/httpapi/           routes, handlers, probes, auth middleware   (the wire)
internal/service/           the upload state machine, every decision     (the rules)
internal/domain/            assets, variants, enums                      (the vocabulary)
internal/auth/              token verification, Principal, Tenant        (who is calling)
internal/objectstore/       the storage interface, in-memory + s3 (aws, r2)
internal/darkroomdb/        pgx, the outbox, the idempotency ledger      (persistence)
internal/service/outbox.go  the event envelope                          (the contract)
internal/checksum/          sha256, one-shot and streaming, and the trust rules
internal/variants/          resize + re-encode
internal/storagekey/        the only two constructors for a storage key
internal/api/               GENERATED from openapi/v1.yaml. The router.
internal/problem/           the problem envelope: code, title, status
internal/darkroomdb/pgtest  one schema per test, migrations applied into it

migrations/embed.go         the SQL, compiled in (go:embed cannot reach ../)
migrations/                 plain SQL, applied by a deploy step
openapi/v1.yaml             what exposes.api points at
bin/prime                   the gate
bin/tier-counts             the accounting check over the same log
```

**THE ROUTER IS GENERATED, AND THAT CHANGES WHAT "ADD A ROUTE" MEANS.** There is
no route table. `internal/api/api.gen.go` is `oapi-codegen` output from
`openapi/v1.yaml`, it is a `*chi.Mux`, and `httpapi.New` walks it with
`chi.Walk`. A new operation is a document edit and a `go generate` in one commit;
forgetting the handler is a COMPILE ERROR rather than a 404 in production. The
Rust tree's `http::OPERATIONS` was a hand-maintained second inventory and its own
README admitted the cost.

Dependencies point downward only. `service` knows nothing about net/http;
`domain` knows nothing about SQL; `objectstore` knows nothing about assets.
That is what lets the state machine be tested without a socket and the storage
scope without a database. **Do not add an `upward` import** — the moment
`service.rs` imports axum, the HTTP layer stops being a translation and the
test for a decision needs a request. The Go rule is the same shape:
`darkroomdb` knows nothing about HTTP, and nothing above it will import a pool.

## The two database credentials, and which is which

**`DATABASE_URL` is `darkroom`, the OWNER. `DARKROOM_DATABASE_URL` is
`darkroom_app`, the LOGIN the application uses.** Both are provisioned by
`templates/compose/postgres/initdb/10-cluster.sh` from one `KIT_POSTGRES_DATABASES`
entry, and the distinction is the whole of kit's account boundary:

- `darkroom` owns every table, runs migrations, and is subject to the RLS
  policies anyway because `assets` and `asset_variants` carry **FORCE** RLS.
- `darkroom_app` owns nothing, holds DML on the four tables and nothing else, and
  therefore **cannot** `ALTER TABLE ... DISABLE ROW LEVEL SECURITY` or
  `DROP POLICY`. A leaked connection string for it is a read-only one.

`TEST_DATABASE_URL` is the **owner** on purpose: `pgtest` creates a schema per
test, which needs `CREATE` on the database. A test suite running as the login role
could not isolate itself at all.

## The one rule RLS adds, and it is the one that surprises people

`assets` and `asset_variants` have **FORCE ROW LEVEL SECURITY**, which removes the
**owner's** exemption from their policies. So `current_account_id()` returning
NULL when no identity is set means a statement matches **NULL**, not false — and a
`DELETE` with no `begin_account` **deletes nothing and exits 0**.

That is the substrate working as designed and it is the nastiest edge in it: a
cleanup or a query that reports success and did nothing. `AGENTS.md` already
states the same rule from the other side — *"the rule is absence, not refusal"* —
and this is what it looks like in practice. **`internal/darkroomdb.InAccountTx`
sets the identity, and any statement that runs outside one is asserting rows
exist rather than that it saw them.** When a write "succeeds" and changes nothing,
check the identity before anything else.

## Rules

**Tests first.** Write the table, watch it fail, then implement until green
(PLAN.md §3). Every handler change needs a case asserting the status code
*and* the JSON shape.

**`auth.Tenant` is the only way to scope a query, and `InAccountTx` is the only
way to start a transaction.** `auth.Tenant` has exactly one constructor and it
takes a verified `*Principal`; there is no `TenantFromRequest` and adding one is
building a cross-tenant vulnerability. `InAccountTx` is what makes the second half
expressible: there is no way to reach this repository's database without naming an
account inside the transaction, because naming the account IS starting the
transaction. The Rust version of this rule was a typecheck — a repository method
that did not take a `Tenant` did not compile — and the Go version is a single
entry point rather than a property of every signature. That is a weaker
guarantee and it is worth knowing which one you are relying on.

**AND `auth.TenantOf(r) PANICS WHEN THERE IS NO VERIFIED PRINCIPAL.** Not nil,
not a zero `Tenant` with an empty account (`InAccountTx` with `account_id = ''`
matches no rows under FORCE RLS while every write still exits 0 — that is the
"reports success and did nothing" failure), not a 500 written by the accessor
(that converges: the mistake becomes documented behaviour and a test asserts the
tidy error). A panic fails the test at the missing call and drops one connection
in production.

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
layers below the wire: `service.find` returns "not found" for another account's
row, never a distinct error, because a distinguishable error is the same
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
`internal/service`'s tests assert the read count is exactly one, and the one read
is the trade stated in the README.

**One `objectstore.Store` implementation, several S3-compatible services.** R2 is not
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

**The probes are exempt from auth STRUCTURALLY, and the reason matters.** The
Rust tree mounted auth with a `Router::layer`, which in axum applies to every
route the router holds — so "register the probe before the layer" does NOT exempt
it, and there is a test saying so. chi's `Use` has the opposite property: it
applies to routes registered AFTER the call and to no others. So the router is
built from three `chi.Router`s and the probe router simply has no auth middleware
to remove. The split is on the PATH (`APIV1Prefix`), not on a list of names,
because a list of names is a second inventory — and
`TestEveryUnauthenticatedRouteIsOneTheDocumentExempts` holds the set this code
exempts against the set `openapi/v1.yaml` exempts, so adding an unauthenticated
route means editing the document.

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
tenants, traverses, and is guessable. `storagekey.Generate` is the only
constructor. If you need a key shape change, change the generator and the
migration together.

**`Asset` has no `storage_key` field.** It is a server-side detail and a client
that can read it will try to build a URL from it.

**Deps need a cause stated in review.** The full list with reasoning is in
README.md. No dependency without one, and no feature flag that a test can reach
the network through.

**No `unsafe`.** There is none in this repository, and in Go the equivalent is a
`panic` on a path a caller can reach: the one that exists (`auth.TenantOf` on a
request with no principal) panics on a MISTAKE at the call site and says so in
its message. A panic that a request can trigger is not that. Anything else needs
a written justification next to it.

**A blank import is a guard no compiler holds.** `golang.org/x/image/webp`
registers itself with `image.Decode` through `import _ …`, and deleting that line
leaves a program that compiles, links, vets clean and passes every other test
before failing on the first WebP a customer uploads. The Rust tree's
`SUPPORTED_INPUT_FORMATS` guarded the equivalent risk there because a missing
Cargo feature does not compile; **in Go there is no list that can guard it**,
because the standard library exposes `image.RegisterFormat` and no reader for the
registry. `internal/variants/` holds the formats with a behavioural test carrying
a real file of each, plus a structural test that reads `variants.go` for the
three blank imports the behavioural test structurally cannot catch. Both were
found by planting the divergence, and the reasons they are two tests rather than
one are in `README.md` under "The Go dependency".

**No secrets in the repo.** Object-storage credentials are the AWS SDK's own
chain, and `objectstore.New` REFUSES a static credential so the production
constructor cannot acquire one by accident. There is no dev JWT secret in this
tree at all: `internal/auth` has one constructor and it is always on. (The Rust
tree read `DARKROOM_DEV_JWT_SECRET` at runtime rather than baking it in with
`option_env!` — a secret in the binary is a secret in the image, in the build
cache, and in `docker history`. That reasoning still holds for whatever comes
next.)

**Comments say why.** Explain the decision and the constraint, not the
mechanism. A comment restating the line below it is noise.

**Stubs stay honest.** Something not written is absent, not a `not implemented`
fake that looks finished. README's "Not done" is the source of truth.

## Tests that need a database

`go test ./...` is green on a bare machine with no Postgres and no Docker. That is
deliberate: the suites that need a database call `pgtest.Require`, which SKIPS
with a named reason, and a skip that is silent is a check that proves nothing —
which is why `bin/prime`'s `--db` tier runs them with `-v`, because `go test`
discards a passing package's output entirely, and why `bin/tier-counts` FAILS on
any `--- SKIP:` in a database tier.

The Rust tree could say "77 passed, 59 ignored" because a cargo database test is
`#[ignore]`d and libtest prints the count. Go has no such marker, so the
accounting script's identity is the one that survives: **enumerated without a
database == passed with one**, checked in both directions.

```sh
# the database is kit's shared cluster, brought up with this repository's
# compose file merged over the pinned stack. See README "Running it".
KIT_COMPOSE_DIR=<kit>/templates/compose \
docker compose --project-directory . \
  -f <kit>/templates/compose/docker-compose.yml \
  -f ./docker-compose.yml up -d --wait

./bin/prime --db                # THE GATE: everything, with the database
./bin/tier-counts /tmp/prime.log # and the counts out of the same log
```

`TEST_DATABASE_URL` comes from `.env` (see `.env.example`): the host side,
`localhost` and `KIT_POSTGRES_PORT` — **15500, not 5432** — the `darkroom`
database, and the CLUSTER's password. All three changed when darkroom joined the
shared cluster, and all three are consequences of kit's defaults rather than
choices. The service's own `DATABASE_URL`, inside the compose network, uses the
service name `postgres` in place of `localhost`.

**`--db` ALSO NEEDS `DARKROOM_DATABASE_URL`, AND REFUSES TO START WITHOUT IT.** Not
a second database — a second ROLE on the same one. `TestTheLoginRoleCanDoTheWork` is
the only test in this repository that connects as `darkroom_app`, the login role the
RLS policies apply to, and every other test in that tier connects as `darkroom`, the
owner, because the harness needs `CREATE` on the database to make a schema at all.
With `TEST_DATABASE_URL` alone the tier runs 14 of its 15 tests and the fifteenth
skips with a stated reason — an honest skip, which is what made the gap survive: the
job meant to hold this service to its contract was itself failing a floor of 15 that
nothing could satisfy, and the one test proving the role boundary works had never run
in CI anywhere. Required instead, `bin/prime` names the missing variable in under a
second, before it compiles anything. CI creates the role itself, from the same
attributes kit's `templates/compose/postgres/initdb/10-cluster.sh` uses.

**THE GO HARNESS DOES THE SAME THING AND NEEDS ONE EXTRA THING.** `pgtest` gives
each test a schema named `t_<12 hex>`, applies `migrations/` into it with that
schema on the connection's `search_path`, and drops it with `cascade`. Because
`cafaye.protect_table` takes a `regclass` and `regclass` resolution follows
`search_path`, each test protects its OWN `assets` — the isolation is real rather
than a naming convention.

It serialises the MIGRATION step and nothing else, and the reason is measured
rather than designed: six concurrent tests applying `0004` failed with `tuple
concurrently updated`, because `protect_table` GRANTS DML to `darkroom_app` and
two transactions granting to the same role contend on that row. The schemas are
independent; the GRANT names a cluster-wide object no schema can privatise. Every
test still connects, seeds, queries and drops concurrently.

`DARKROOM_DATABASE_URL` is read by exactly one Go test —
`TestTheLoginRoleCanDoTheWork` — which is the only one that must prove the LOGIN
role works rather than the owner's privileges. It writes to `public` and removes
what it wrote; `pgtest.Require(t)` exists because `t.Skip` calls `runtime.Goexit`
and a test that spawns goroutines and skips inside one reports phantom
collisions instead of skipping.

These tests run against the SAME database the service uses, so the service's
per-role connection limit is spent by the service's own pool before a test
connects. `docker-compose.yml` raises `KIT_POSTGRES_ROLE_CONNECTIONS` to 20 for
that reason, and that value is applied by an init script which runs ONCE PER
VOLUME — an existing volume keeps the old limit.

**They run in parallel, and that is a property of the harness rather than a
lucky configuration.** Every `test_store()` call creates a schema named
`t_<8 hex>`, applies the migrations into it, and puts that schema on the pool's
`search_path`; `truncate` names its tables unqualified, so it empties this
test's tables and cannot reach another's. `--test-threads=1` was here because the
suites shared one set of tables, and the schemas are why it is gone — six
parallel runs before that change went red every time, three after it went green,
with the counts in `REPORT-darkroom-hermetic-db-01`.
`pgtest_test.go` holds the property, and splits it so the half that needs no
database runs on a bare machine: a name that can only be twelve characters from a
fixed alphabet, and a `drop schema` that can only fire on a schema this harness
made and that no run can still be using.

**A test that cannot be isolated does not get a serialisation back.** Adding a
serialisation flag to `bin/prime` undoes the isolation for every other test and
hides the one that needed it; such a test gets its own exclusion, named, with a
comment saying what it shares with whom. Nothing has needed one.

**A `truncate` that quietly points somewhere else is the sharpest edge in this
suite, and the guard for it exists because the divergence was planted rather than
imagined.** With `truncate` rewritten to name `public.assets` and its three
siblings, the entire database tier stayed green — the schemas hide it, because
every store is created empty and a `public` truncate cannot reach into another
schema. `pgtest_test.go` asserts both halves now: one test's
`truncate` empties its own schema AND leaves another test's rows alone, because
the first half on its own is satisfied by a `truncate` that does nothing at all.

A skip is honest; a test that silently passes without proving anything is not.
**If you add a `#[ignore]`, add the job that runs it** — CI's `gate` job calls
`bin/prime --db` and is the only reason the tenant-isolation suite is real
rather than decoration.

## The exit code is not the evidence

`bin/prime --db` exits zero when it is happy, and `go test ./...` exits zero for a
package with no test files in it. Those are the same exit code, so CI reads the
counts instead, with `bin/tier-counts`:

```sh
./bin/prime --db 2>&1 | tee /tmp/prime.log
./bin/tier-counts /tmp/prime.log
```

It asserts 24 variants / 15 harness / 39 service / 21 httpapi, **and** one
identity: the number `bin/prime` enumerated without a database equals the number
the database tiers passed with one, because they are the same tests. **Adding a
test means raising the number in `bin/tier-counts` in the same commit** — that is
the point of the constant, and a CI red that says `expected 29, got 26` is the
mechanism working, not failing.

**THE IDENTITY CHANGED AND THE OLD ONE IS NOT AVAILABLE IN GO.** The Rust script
asserted `skipped == passed`, which cargo can say because a database test is
`#[ignore]`d and libtest prints an `ignored` count. A Go test is not marked; it
calls `pgtest.Require` and SKIPS at runtime. Measured on this tree against no
database: `pgtest` is 4 PASS and 11 SKIP, `service` is 9 PASS and 20 SKIP — so
the skipped counts are 31 and the passed counts are 44, and the Rust identity is
not merely unavailable here, it is FALSE. The script now also fails on any
`--- SKIP:` in a database tier, because a skip there is a check that did not run
under a green `ok`.

**A tenant predicate that is WRONG rather than ABSENT is what RLS is for, and
there is no structural guard to write for it.** The Rust tree read `src/store.rs`
as text with `include_str!` and failed if a `&Tenant` query stopped saying
`account_id = $2`. There is no query file to read here — the predicate is a
POLICY, set by `migrations/0004_account_isolation.sql`. So the structural half is
`pgtest`'s policy guards, which read `pg_get_expr` on the live policies instead
of a source file: what they hold in place is the database's answer rather than a
text file's spelling. The behavioural half is `internal/service`'s two-account
fixture against real Postgres, covering read, list, update and delete, and it is
floored at 39.

**A guard has to be shown to fail, and the predicate has to be tested too.** The
first draft of the Rust `tests/tenant_scoping.rs` shipped three ways of not
guarding anything, and every one was found by running it rather than reading it:
a quadratic string scanner that hung two tests for eleven minutes instead of
failing, a "a mutation must name a row" predicate satisfied by `account_id = $2`
itself (because `id = $` is a substring of it), and a set comparison that
asserted an accident of file layout. **Any new guard here gets a planted
divergence before it is believed**, and if it hangs or passes on a broken source,
that is a finding about the guard, not about the source. The regenerate step in
`bin/prime` was planted twice for exactly this reason, and the second plant — a
document edit that changes no route — is the one the drift tests cannot see.

## No object-storage credentials in CI, and none needed

No tier needs an endpoint or a credential, which is worth being exact about.
`internal/objectstore` presigns with the real SDK and static dummy credentials;
presigning builds a URI and a signature locally and transmits nothing, so its
table runs offline against AWS, MinIO and R2 *configurations*. Adding a MinIO service container to CI would boot a bucket
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

- `objectstore.New` REFUSES a static credential and takes the AWS SDK's own
  chain, so a test that needs one calls `NewS3ForTest` and there is no way for
  the production constructor to acquire one by accident.
- HTTP tests drive the router through `httptest.NewRecorder` — the Go spelling of
  `ServiceExt::oneshot`, a function call and not a round trip.

**AND TWO PACKAGES STAND UP A LOOPBACK JWKS SERVER, WHICH IS THE ONE EXCEPTION
AND IT IS NOT AN EXCEPTION TO "NO NETWORK".** `internal/auth` and
`internal/httpapi` each run an `httptest` key-set server, because the property
under test is what the verifier does with a key it has to fetch. Neither the
service under test nor any handler binds a port: every request goes through a
recorder. The reason is in both files' headers.

The S3 store's tests presign against the real SDK with static dummy credentials,
which sends nothing: presigning builds a URI and a signature locally. That is
what lets `internal/objectstore` assert what darkroom would put on the wire for
AWS, MinIO and R2 — the credential scope, the signed headers, the absent
headers — without a bucket. A real bucket is a manual verification step and the
README says how.

The in-memory store's presigned URLs are really signed and really scoped, so a
bug in the scope or the TTL fails a test instead of shipping.

## No sleeps, no raised retries, no loosened assertions

PLAN.md §3. A test that needs an expired URL uses a zero-second TTL. A test
that needs a port that is closed connects to port 1. A test that needs a
duplicate uses a real second request. None of them waits.

## Gates

```sh
./bin/prime          # fmt, build, vet, test, regenerate-and-diff — no database
./bin/prime --db     # + the database tiers against Postgres. THIS IS THE GATE.
gofmt -l .            # listed, then asserted empty: -w rewrites and exits 0
go build ./... && go build -o bin/darkroom ./cmd/darkroom
go vet ./...          # not golangci-lint: a linter is a dep to version and argue about
go generate ./internal/api/   # regenerate the router FROM the document
./bin/tier-counts /tmp/prime.log   # the counts out of a --db run
./bin/gate-self-test  # is gate.yml still true of this repository
```

`go build the binary` is a SEPARATE step from `go build ./...` and it exists for
a reason worth keeping: a `cmd/darkroom` that was deleted is not a build failure,
it is a silent absence, and `./...` reports a package that is simply not there as
success. That is the Rust `src/main.rs` shape — a library can outlive its binary
and `cargo build` stays green.

**ONE TREE, AND THE CARGO STEPS ARE GONE FROM IT.** They were five steps beside
six Go steps while both existed, and they are REMOVED rather than left commented
out or left running. Both of those are worse than absence: a commented-out
`cargo test` is a line nobody reads, and a LIVE one in a repository with no
`Cargo.toml` exits 127 and takes `set -e` and the whole gate down with it.

**AND A GO SUITE THAT LOSES ITS TESTS IS STILL GREEN, WHICH IS WHY ONE GO STEP
IS NOT `go test`.** `go test` prints `ok` for a package with no test files and
discards the count from a package that passes, so an emptied file or a build tag
that stopped matching is a green gate reporting zero. `bin/prime` enumerates
every suite with `go test -list` and fails below a floor — cmd/darkroom 16,
objectstore 21, checksum 6, storagekey 4, pgtest 17, domain 7, problem 7,
service 49, auth 12, httpapi 34, **outbox 19**. Adding a test means raising the
number in the same commit, which is the point of the constant: *"a security check
that reports zero is worse than one that is absent: it looks like coverage."*

**AND A FLOOR IS `-lt`, SO A COUNT THAT IS TOO HIGH PASSES IT — which is how the
service identity drifted for two packets without a single test failing.** 39 was
the number the tenancy packet inherited and did not raise, so the tree carried 47
while `bin/tier-counts` said `expected 39, log says 47` and `bin/prime` said 47
is above 39. Both were green on the same run. The two figures are the same number
on purpose, and BRK-5 measured and raised them to 49 (the tenancy packet's eight
plus its own two payload guards) after the asymmetry was found by running
`bin/tier-counts` rather than by reading it.

**AND A GENERATED FILE THAT DRIFTS IS ALSO GREEN, WHICH IS WHY THE REGENERATE
STEP IS NOT OPTIONAL.** `internal/api/api.gen.go` is built from
`openapi/v1.yaml` and the router IS that file, so a stale one answers the old
routes under the new document's promises. `bin/prime` regenerates through the
module cache and diffs, and it does not skip when it cannot run: a missing
generator fails the gate rather than passing it.

**`./bin/prime --db` is the gate, and `mise run prime` is not.** `gate.yml`
declares the former, and the flag is in the declaration rather than implied by
it: without `--db` the 44 database tests SKIP at runtime, the run still prints
`==> ok` and exits 0, and `bin/tier-counts` reports zero. If you are landing a
change, run the declared gate.
`gate.yml` is core's format — read it before changing it, and read what it says
about this repository's own requirements.

`bin/gate-self-test` needs a Postgres at `$TEST_DATABASE_URL` for the cases that
run the gate, and it reports a skip as a failure rather than a pass.

All green before a commit lands. The feature-flag argument is GONE and it is
worth remembering why it mattered: `s3` and `dev-auth` were behind cargo features,
so a default build compiling clean said nothing about them, and the gate needed a
second tier to compile them at all. Go has no feature flags here — one build is
the deployment build — so the whole class of "the gate did not compile that path"
is gone with them. The replacement is the regenerate step, which covers the one
build variant that remains: the generated file.

**`gofmt -l` AND NOT `gofmt -w`.** The second rewrites the files and exits 0
whatever it found, so a gate built on it reports green having silently reformatted
the commit. `-l` lists and still exits 0, so it is paired with an explicit
emptiness test — the same reasoning as the exit-code rule at the top of
`bin/prime`, one level down.

## Every guard here gets a planted divergence

A guard that has never been shown red is a guard nobody can rely on. The Go
database guards were each planted against the real cluster before being believed,
and two of them caught bugs in the packet that introduced them:

| planted                                          | caught by                                    |
|--------------------------------------------------|----------------------------------------------|
| `grant truncate … to darkroom_app`                 | `TestTheLoginRoleCanWriteEveryTable…`        |
| a migration that unfocuses `assets`                | `TestEveryProtectedTableIsForced` **and** `TestTheOwnerCannotRead…` |
| a migration replacing a wrapped call with a bare one | `TestEveryHelperCallInAPolicyIsWrapped`     |
| a migration creating a `for all` policy            | `TestNoPolicyIsForAll`                       |
| `droppable` fed `public`, `""`, `t_`, `t_ZZZ…`    | `TestDroppableIsThePlantedDivergence…`       |

Three things went wrong while writing them and all three are recorded in the
code, because each is a way a guard passes on a broken source:

- **`isWrappedAt` was wrong twice.** Go's `regexp` is RE2 and has no lookbehind,
  so the first version's `(?<!…)` **panicked** and took the package down. The
  replacement trimmed whitespace on the wrong side and read all eight correct
  policies as bare. `TestIsWrappedAt`'s table caught the second version.
- **A planted row was not a divergence.** `TestDroppable…`'s "PLANTED: too long"
  was `t_0123456789ab` — byte-identical to the REAL row two lines below it — so
  it asserted `false` for a name the harness builds.
- **`pg_get_expr` is not the text `protect_table` wrote.** It deparses
  `(select f())` as `( SELECT f() AS f)`, so matching the written form failed on
  every correct policy. Reading the deparse is what makes the check structural;
  knowing its exact shape is what makes it correct.

The coverage floor lives in CI, not in `bin/prime`: instrumenting is a different
build, and folding it into the gate would make every developer run twice. **Its
50 IS THE RUST TREE'S MEASURED NUMBER, CARRIED ACROSS.** It has not been
re-measured for Go, and the honest statement is that it is a floor which may or
may not be reachable. It measures the default suite only — not the database tier
— and README says so where a reader will look.


## Adding an endpoint

1. `internal/service` — the rule, in a method that takes a `service.Caller`.
   Tests first.
2. `openapi/v1.yaml` — the path, the operationId, the errors, and
   `info.version` if anything else in the document moved.
3. `go generate ./internal/api/` — **in the same commit.** The generated
   `ServerInterface` gains a method, and `internal/httpapi` does not implement it
   until you do, so forgetting the handler is a COMPILE ERROR. Then the handler,
   extracting and translating, with nothing decided. There is no route table to
   add a row to; the router walks the generated file.
4. A test asserting the status code, the JSON shape, and the anonymous case.
5. A row in README's endpoint table.
6. A new constant in `bin/prime`'s `assert_go_suite` and in `bin/tier-counts`
   if the new test lives in its own package, so a package that stops running
   fails the gate instead of reporting zero.
7. `./bin/prime --db`.

Step 1's rule survives translation, and it is the one worth restating: a function
that does not take an account, or that takes a transaction from somewhere other
than `InAccountTx`, is doing the thing the Rust `Tenant` typecheck prevented.

## Adding an event

1. A variant on `service.EventType`, and the test asserting three segments and
   the publisher prefix.
2. `cafaye.yml` `exposes.events` — and the test that counts both directions.
3. The catalog row in `core/docs/event-naming.md` **and** a payload schema in
   `core/schemas/events/`, which is a core change, not this repository's.
4. Enqueue it in the same transaction as the domain write.
5. A test that a rolled-back transaction emits nothing.

## Secrets
- No `.env` files with values. Use `cafe secret set/get/list` (dev) and `cafe dev` (injects). See `docs/secrets.md`.
