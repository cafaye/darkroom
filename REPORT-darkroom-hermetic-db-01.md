# REPORT — darkroom-hermetic-db-01

**The question this report exists to answer, answered first.**

> Did the pre-change parallel run actually go red?

**Yes. It went red on all six attempts, and the set of failing tests changed every
time.** `cargo test -- --ignored` on the tree at `master` (`cceb7ce`) exited 101
six times out of six: three with cargo's default fail-fast, three with
`--no-fail-fast` so that every test binary ran. Failing distinct tests per
`--no-fail-fast` run: **12, 12 and 15**. The serial run,
`cargo test -- --ignored --test-threads=1`, exited 0 twice with 54 passed. So
`--test-threads=1` was load-bearing, and the packet did not have to soften
anything.

There is also a finding in here that the packet did not ask for and that cost a
commit: **of the three divergences this work claimed to have proven able to fire,
two were real and one was not.** §7 has the measurement and the guard that
replaced the claim.

---

## 1. What was actually true before this packet

Every one of the 54 database tests got its `Store` from `test_store()` — most
call it directly, and `tests/query_scoping.rs` and `tests/checksum_verification.rs`
reach it through a local fixture helper — and every one of them shared the single
`TEST_DATABASE_URL` and a `truncate assets, asset_variants, outbox_events,
idempotency_keys restart identity cascade` in that same database. Two concurrent
tests therefore deleted each other's fixtures mid-assert.

The symptom is the reason this packet is worth doing. It is not a lock timeout
and not a flaky count — it is an assertion failing about rows that genuinely were
not created, which reads as a product defect:

```
---- delete_removes_the_row_the_storage_object_and_emits_an_event stdout ----
thread '…' panicked at tests/api.rs:301:10:
completes: NotFound { detail: "asset not found" }

---- the_listing_pages_by_cursor_without_gaps_or_repeats stdout ----
assertion `left == right` failed: every asset is seen exactly once:
  ["2558cb63-e669-4c8a-9b5a-0d56f7f70d85", "765f426f-b7bc-4114-95ef-e8d0c922b0f7",
   "6ba853f5-eadd-45b1-b6a6-b9fc1b74c13f"]
  left: 3
 right: 7

---- the_same_bytes_in_two_accounts_are_two_assets stdout ----
assertion `left == right` failed
  left: 4
 right: 2
```

`left: 4, right: 2` is `the_same_bytes_in_two_accounts_are_two_assets` counting
**two other tests' rows as its own** — it created exactly two assets, one per
account, and read four. The deletion direction (`NotFound` above) and the
bleed direction (this) are the same bug seen from two sides.

### The red runs, summarised (pre-change tree, `cceb7ce`)

| run | command | exit | failing tests in the first (aborted) binary | binaries with a failure |
|---|---|---|---|---|
| 1 | `cargo test -- --ignored` | 101 | 5 of 10 in `tests/api.rs` | 1 |
| 2 | `cargo test -- --ignored` | 101 | 3 of 10 in `tests/api.rs` | 1 |
| 3 | `cargo test -- --ignored` | 101 | 3 of 10 in `tests/api.rs` | 1 |
| 4 | `cargo test --no-fail-fast -- --ignored` | 101 | 12 distinct | 4 |
| 5 | `cargo test --no-fail-fast -- --ignored` | 101 | 12 distinct | 5 |
| 6 | `cargo test --no-fail-fast -- --ignored` | 101 | 15 distinct | 4 |

Runs 1–3 are cargo's default: it stops at the first failing binary, so
`tests/api.rs` alone is what red is visible in. Runs 4–6 use `--no-fail-fast` so
every binary runs. The failures spread across `tests/api.rs`,
`tests/checksum_verification.rs`, `tests/idempotency.rs`, `tests/outbox.rs`,
`tests/query_scoping.rs`, `tests/signed_upload.rs` and
`tests/tenant_isolation.rs` — **which tests fail, and how many, changes every
run.** The serial baseline on the same tree, twice: `exit 0`, `54 passed`,
`13.70s` and `13.36s`.

## 2. The measurement the packet asked for: where does `_sqlx_migrations` land?

`sqlx::migrate!` records applied migrations with an **unqualified**
`create table if not exists _sqlx_migrations` (sqlx-postgres 0.8.6,
`src/migrate.rs:114`), so the ledger's schema is decided by the search path and
not by anything in sqlx. Measured from two schemas at once, through the real
harness, with `cargo test -- --nocapture`:

```
MEASURE store A: current_schema=t_f4bfe531 current_schemas=["t_f4bfe531"]
MEASURE store A: _sqlx_migrations rows = [(1, "assets"), (2, "outbox events"), (3, "idempotency keys")]
MEASURE store A: tables in current_schema = ["_sqlx_migrations", "asset_variants",
                                            "assets", "idempotency_keys", "outbox_events"]
MEASURE store B: current_schema=t_48c9ced5 current_schemas=["t_48c9ced5"]
MEASURE store B: _sqlx_migrations rows = [(1, "assets"), (2, "outbox events"), (3, "idempotency keys")]

MEASURE catalog: (schema, relation) = [("public", "_sqlx_migrations"), ("public", "assets"),
  ("t_f4bfe531", "_sqlx_migrations"), ("t_f4bfe531", "assets"),
  ("t_48c9ced5", "_sqlx_migrations"), ("t_48c9ced5", "assets"), …]
```

**Answer: one ledger per test schema**, plus the `public` ledger left behind by
the pre-change runs on this database. The measurement harness
(`tests/zz_measurement.rs`) was deleted before the commit; its assertions live in
`tests/schema_isolation.rs`.

### Two assumptions the measurement corrected

**(a) The `public` fallback does not break the migrations.** The obvious story —
and the one written into the first draft of the harness docs — was that
`search_path = t_x, public` makes `create table if not exists assets` a no-op
against the table already in `public`, leaving the per-test schema empty. It is
false: the target of unqualified DDL is the **first** schema on the path and
`if not exists` is checked there.

```
MEASURE fallback: public.assets exists and holds 2 rows
MEASURE fallback: after a full sqlx::migrate! run with search_path =
  t_b6b2a552, public, tables actually in t_b6b2a552 =
  ["_sqlx_migrations", "asset_variants", "assets", "idempotency_keys", "outbox_events"]
MEASURE fallback: this test saw 0 rows; after its truncate, public.assets holds 2
```

The fallback is still refused, for the reason it actually costs — measured with a
table that exists in `public` and not in the test's schema:

```
MEASURE no-fallback: unqualified `zz_probe` from a test pool =
  Err(42P01, relation "zz_probe" does not exist)
MEASURE no-fallback: the same table, qualified = Ok(0)
```

Without the fallback that query is an error. With it, it is another test's rows
arriving as this test's, and nothing in the suite would report it. So the path is
one schema, and `current_schemas(false)` is asserted to have length one.

**(b) sqlx's `description` is the filename with underscores turned into spaces.**
The ledger guard asserted `"outbox_events"` and went red against a harness that
was correct. Fixed to `"outbox events"`.

### Also measured, and left alone on the evidence

- **sqlx takes a database-wide advisory lock** around every migration — a hash of
  `current_database()` (`pg_advisory_lock`), so concurrent tests' DDL serialises.
  It is left **on**: two migrators can then never be inside one schema, the cost
  is milliseconds per test, and switching it off means reaching into
  `#[doc(hidden)] pub` fields of a dependency. Turning it off is a decision for
  whoever measures the wall clock and wants it.
- **Peak 17 backends** on the database during one parallel run (sampled every
  250 ms), against a `max_connections(5)` pool per test. Nowhere near a role's
  connection limit, which matters because kit's shared cluster caps each role at
  `KIT_POSTGRES_ROLE_CONNECTIONS=20`.
- **Parallel 9.2s against serial 17.1s** on the same machine — and even that is
  modest, and worth stating rather than dressing up: cargo runs test **binaries**
  one at a time and `tests/api.rs` alone is 5.4s of the parallel total (image
  decode and re-encode), so the wall clock is that binary's, not the threads'. The
  honest summary is "eight threads, five and a half seconds of one binary".

## 3. What changed

`tests/common/mod.rs` only, for the behaviour:

- `test_store()` creates a schema `t_` + the first eight hex characters of a
  fresh uuid, applies `sqlx::migrate!("./migrations")` **into it**, and builds the
  pool with `set search_path to <that schema>` in **`after_connect`** — every
  connection the test gets is inside the schema, not just the first one.
- `truncate()` is unchanged and now empties this test's tables, because the names
  it truncates are unqualified and resolve through that search path. Writing
  `public.assets` there would restore the exact bug, which is why the comment
  there says so.
- The schema name is **checked and quoted**, both: `is_plain_identifier` refuses
  anything that is not `t_` + eight lowercase hex characters, and `quoted` wraps
  it. Belt and braces on purpose — if the assertion is deleted the quoting cannot
  be escaped out of; if the quoting is deleted the assertion still refuses.
- A **janitor** drops test schemas older than six hours. Without it every run
  leaks a schema per test and nothing else in the harness can remove one:
  `test_store` returns a `Store`, not a guard, and dropping on drop would need a
  `Drop` impl on production code, which is out of scope. What counts as ours is
  decided in **Rust**, not in the candidate query, so the rules are unit-tested
  with no database in the picture.

`tests/schema_isolation.rs` is new: 9 tests, four with no database and five with
one. The split is the argument — the cheapest checks in the tree (a name that
cannot escape an identifier, a `drop schema` that can only fire on a schema this
harness made and that no run can still be using) should not be the ones a
developer without Docker never runs.

No change under `src/` (`git diff master -- src/` is empty). `bin/prime`,
`.github/workflows/ci.yml`, `AGENTS.md`, `README.md`, `.env.example`, `gate.yml`
and `bin/tier-counts` were updated for the flag and the counts.

### The janitor, end to end

```
323 schemas named like ours on the database
→ their comments backdated eight hours
→ one run of `cargo test --test schema_isolation -- --ignored`
→ 7 schemas remain, and all 7 are 0.00 hours old (this run's)
```

## 4. The three runs, on the final tree, with no `--test-threads`

Full per-binary output rather than a summary line, because "it was green" is not
evidence for a concurrency claim and a number is not an output.

### Run 1

```
     Running tests/api.rs
test result: ok. 10 passed; 0 failed; 0 ignored; 0 measured; 1 filtered out; finished in 5.57s
     Running tests/checksum_verification.rs
test result: ok. 7 passed; 0 failed; 0 ignored; 0 measured; 0 filtered out; finished in 0.43s
     Running tests/idempotency.rs
test result: ok. 6 passed; 0 failed; 0 ignored; 0 measured; 0 filtered out; finished in 0.41s
     Running tests/outbox.rs
test result: ok. 5 passed; 0 failed; 0 ignored; 0 measured; 0 filtered out; finished in 0.33s
     Running tests/query_scoping.rs
test result: ok. 7 passed; 0 failed; 0 ignored; 0 measured; 0 filtered out; finished in 0.41s
     Running tests/schema_isolation.rs
test result: ok. 5 passed; 0 failed; 0 ignored; 0 measured; 4 filtered out; finished in 0.50s
     Running tests/signed_upload.rs
test result: ok. 7 passed; 0 failed; 0 ignored; 0 measured; 0 filtered out; finished in 0.43s
     Running tests/tenant_isolation.rs
test result: ok. 12 passed; 0 failed; 0 ignored; 0 measured; 0 filtered out; finished in 0.69s
```

`cargo test -- --ignored` → **exit 0**, 59 passed, 0 failed, `9.19s`.

### Run 2

```
     Running tests/api.rs
test result: ok. 10 passed; 0 failed; 0 ignored; 0 measured; 1 filtered out; finished in 5.50s
     Running tests/checksum_verification.rs
test result: ok. 7 passed; 0 failed; 0 ignored; 0 measured; 0 filtered out; finished in 0.41s
     Running tests/idempotency.rs
test result: ok. 6 passed; 0 failed; 0 ignored; 0 measured; 0 filtered out; finished in 0.41s
     Running tests/outbox.rs
test result: ok. 5 passed; 0 failed; 0 ignored; 0 measured; 0 filtered out; finished in 0.37s
     Running tests/query_scoping.rs
test result: ok. 7 passed; 0 failed; 0 ignored; 0 measured; 0 filtered out; finished in 0.44s
     Running tests/schema_isolation.rs
test result: ok. 5 passed; 0 failed; 0 ignored; 0 measured; 4 filtered out; finished in 0.64s
     Running tests/signed_upload.rs
test result: ok. 7 passed; 0 failed; 0 ignored; 0 measured; 0 filtered out; finished in 0.40s
     Running tests/tenant_isolation.rs
test result: ok. 12 passed; 0 failed; 0 ignored; 0 measured; 0 filtered out; finished in 0.69s
```

`cargo test -- --ignored` → **exit 0**, 59 passed, 0 failed, `9.30s`.

### Run 3

```
     Running tests/api.rs
test result: ok. 10 passed; 0 failed; 0 ignored; 0 measured; 1 filtered out; finished in 5.37s
     Running tests/checksum_verification.rs
test result: ok. 7 passed; 0 failed; 0 ignored; 0 measured; 0 filtered out; finished in 0.47s
     Running tests/idempotency.rs
test result: ok. 6 passed; 0 failed; 0 ignored; 0 measured; 0 filtered out; finished in 0.44s
     Running tests/outbox.rs
test result: ok. 5 passed; 0 failed; 0 ignored; 0 measured; 0 filtered out; finished in 0.34s
     Running tests/query_scoping.rs
test result: ok. 7 passed; 0 failed; 0 ignored; 0 measured; 0 filtered out; finished in 0.47s
     Running tests/schema_isolation.rs
test result: ok. 5 passed; 0 failed; 0 ignored; 0 measured; 4 filtered out; finished in 0.59s
     Running tests/signed_upload.rs
test result: ok. 7 passed; 0 failed; 0 ignored; 0 measured; 0 filtered out; finished in 0.42s
     Running tests/tenant_isolation.rs
test result: ok. 12 passed; 0 failed; 0 ignored; 0 measured; 0 filtered out; finished in 0.71s
```

`cargo test -- --ignored` → **exit 0**, 59 passed, 0 failed, `9.25s`.

(Binary names and lines are verbatim from the three run logs. The four binaries
that report `0 passed` with every one of their tests filtered out — and so appear
nowhere above — are `contract.rs`, `openapi_document.rs`, `storage_backends.rs`
and `tenant_scoping.rs`: they hold no `#[ignore]`d test, so `--ignored` filters
all of theirs out. `tests/schema_isolation.rs` reports `4 filtered out`, which is
its four no-database cases. The doc-test block reports `0 passed` last and is
counted by `bin/tier-counts` too, contributing zero.)

### The other two runs the packet asks for

```
cargo test --features s3 -- --ignored          exit 0   59 passed  0 failed
cargo test -- --ignored --test-threads=1       exit 0   59 passed  0 failed   17.10s
```

Serial is still green, and is now **slower than parallel**, which is the honest
way round: with a schema per test there is nothing for the flag to serialise, so
a developer who passes it is choosing the slower of two correct runs. That
asymmetry is deliberate — a flag that costs nothing gets added back by anyone, for
any reason.

`./bin/prime --db` also ran the tier twice more, once per feature set, and both
were green; `bin/tier-counts` on that log:

```
  ok  tier 4  unit tests                             77
  ok  tier 4  skipped for want of a database         59
  ok  tier 4/5  schema-isolation checks (default build) 4
  ok  tier 4/5  schema-isolation checks needing a database (default build) 5
  ok  tier 4/5  schema-isolation checks (s3 build)   4
  ok  tier 4/5  schema-isolation checks needing a database (s3 build) 5
  ok  tier 6  query-scoping cases                    7
  ok  tier 6  schema-isolation cases                 5
  ok  tier 6  schema-isolation cases left ignored    0
  ok  tier 6  database tests passed (default build)  59
  ok  tier 6  database tests passed (s3 build)       59
==> tier-counts ok
```

The identity the exit code cannot: **59 skipped by the default run == 59 passed
by the database run**, both feature sets.

## 5. The canary, and a finding about the canary the packet specified

The packet asks for a temporary test that writes a row, sleeps long enough for
another test's `truncate`, and confirms the suite goes red with it. Written
exactly that way it is **not a canary**. Measured on the pre-change harness:

```
attempt 1, old harness + sleep-based canary   exit 101   canary FAILED
      (and it failed on the WRONG assertion — `the canary wrote its row:
       left: 5, right: 1`, i.e. it was counting other tests' rows)
attempt 2, old harness + sleep-based canary   exit 101   canary PASSED   ←
      (scoped to its own asset id, so the only remaining way to go red is its
       own row being deleted — and it did not happen inside three seconds)
```

Three seconds is a bet that some sibling test has reached its own `truncate`
inside the window, and on a warm eight-core machine it often has not: every
sibling may already have truncated before the canary wrote anything. Green is
also the answer that matters, because a switch that is right about half the time
proves nothing in either direction.

So the canary was rebuilt as **two tests that hand off through two flags**. The
writer publishes its row; the truncator waits for that flag and only then
truncates; the writer waits for the truncator's flag before it reads. The
ordering is constructed rather than waited for, both waits time out at ten
seconds and **fail** if the other half never arrived, and every count is scoped
to the writer's own asset id — an unscoped `select count(*) from assets` goes red
on the old harness by counting other tests' rows, which is a real symptom but not
the one the pair is there to produce.

```
pre-change harness + canary    exit 101   zz_canary_a_… FAILED
  another test's truncate deleted the row this test had already written
  and was still holding
    left: 0
   right: 1
  (that run: 16 other tests also failed, so the canary is not the only symptom —
   it is the one that names the mechanism)

this harness + canary          exit 0     3 runs out of 3, 60 passed each
```

**So the parallel database tier can still go red, on demand, and by removing the
thing this packet added rather than by waiting for the right second.** Both
canary tests were deleted before the commit. Their job was to be the switch, not
to be a fixture.

## 6. Item-by-item against "what done is"

| # | requirement | result |
|---|---|---|
| 1 | `cargo test -- --ignored`, no flag, green three times | **met** — three runs in §4, exit 0, 59 passed each, plus two more inside `./bin/prime --db` |
| 2 | `--test-threads=1` still passes | **met** — exit 0, 59 passed |
| 3 | an un-isolatable test gets its own named exclusion, not a suite-wide flag | **no test needed one**; the rule is written into `bin/prime`, `AGENTS.md` and commit `d129142` so a future one is handled as the packet says rather than by re-adding the flag |
| 4 | `bin/tier-counts` still reports its tiers, counts right | **met** — `DB_TESTS` 54 → **59**, three constants touched (`SCHEMA_ISOLATION_TESTS=4`, `SCHEMA_ISOLATION_DB_TESTS=5`); the ignored count inside that one file is asserted to be 5 rather than 0, which is the only place in the tree where an ignored count is a constant rather than a failure |
| 5 | the tier-6 comment in `.github/workflows/ci.yml` updated | **met**, and extended with what CI would *not* notice: it provisions one postgres service, so a quietly re-serialised suite would pass on a laptop's eight cores and fail there for reasons the workflow cannot report |

Out of scope and honoured: `TEST_DATABASE_URL`'s shape is unchanged, CI still
provisions one database, sqlx is still sqlx, **nothing under `src/` changed**
(`git diff master -- src/` is empty), and no test was made less thorough.

## 7. Planted divergences — two of three fired, and the third is why §3 has five database cases

Commit `7213712` claimed three divergences had been planted and reverted, each
caught in the tier named. Two were. The third was asserted, not measured, and
when it was finally planted the **entire database tier stayed green**:

```
divergence 1  truncate → public.assets, public.asset_variants,
              public.outbox_events, public.idempotency_keys restart identity cascade

  cargo test --no-fail-fast -- --ignored   →  exit 0, 0 failed
  claimed catcher: one_tests_truncate_cannot_delete_anothers_rows
  actual result:  … that test PASSED
```

Why the schemas hid it, which is the finding:

- `one_tests_truncate_cannot_delete_anothers_rows` still passes, because a
  `public` truncate **cannot reach into another schema**. The direction it guards
  is the one a qualified truncate happens to get right.
- Every other test gets a schema created empty a line earlier, so a truncate aimed
  at the wrong place deletes nothing observable.
- **`test_store`'s own truncate on entry deletes nothing at all on this tree.**
  The packet kept `truncate` because "a test that reuses its own store across
  steps still needs it" — true in principle, and no test does that today, which
  is exactly why nothing noticed.

A `truncate` that quietly empties the **service's** tables is the sharpest edge in
this suite: `TEST_DATABASE_URL` is the same database the service uses, and on
kit's shared cluster that is the running service's own data.

So there is now a guard for it, asserting **both** halves, because the first half
alone is satisfied by a `truncate` that does nothing at all:

```
tests/schema_isolation.rs
  truncate_empties_this_tests_schema_and_only_that
    A writes, B writes, truncate(&a)
    A's rows are gone          ← the half nothing was checking
    B's rows are still there   ← the half the other test checked
```

Re-planted against that guard, the same divergence:

```
---- truncate_empties_this_tests_schema_and_only_that stdout ----
assertion `left == right` failed: truncate did not empty this test's own schema.
Either it names something other than this test's tables — public.assets being
the one that matters, since that is the service's own database — or it does
nothing, which leaves a test that reuses its store across steps reading the rows
it wrote in the previous step.
  left: 1
 right: 0
```

`exit 101`, and that was the **only** failing test in the run — which is the
finding stated exactly: before the guard the divergence was invisible; now it is
one red test.

### The two that did fire

| divergence | result | caught by |
|---|---|---|
| the janitor filtering on the name alone (no stamp, no age) | **all 5 database cases fail.** A name-only janitor drops sibling tests' LIVE schemas mid-run, and the loudest failure is not the janitor test at all: `reads current_schema(): ColumnDecode { UnexpectedNullError }` — a sibling's schema was dropped, so the path named a schema that is gone | the stamp assertions and the six-hour age check |
| `set search_path` once on ONE connection instead of in `after_connect` | **exit 101, 8 binaries failing, 40 distinct tests failing**, including both path guards. Worth naming what it does rather than only that it fails: the connection without the path resolves `truncate` to `public`, so this divergence does not merely weaken isolation, it points the suite's deletes at the service's own tables | `current_schemas(false)` asserted to have length one, on every fetch |

### And a fifth defect, in a guard written by this packet

The janitor test planted a **fixed** name, `t_zzzz`. A failed run of that test
left the schema behind — the cleanup below the assertion never executes — and the
next run died on `42P06 schema already exists`. Found by running the test after
a run of it had failed, which is the only way it is findable. The name is now
unique per run and still not one `is_plain_identifier` accepts, which is the
property the test is about. A clean failure rather than a false pass, but a flake
is a flake.

## 8. Gate

```
./bin/prime --db          exit 0   (fmt, build, clippy -D warnings --all-features,
                                   cargo test, cargo test --features s3,
                                   cargo test -- --ignored, and the same with s3)
./bin/tier-counts         exit 0   22 checks, all ok
./bin/gate-self-test      exit 0   all 23 breakages red, all 5 warning cases
                                   green with exit 0, both controls green,
                                   nothing skipped
cargo test (no TEST_DATABASE_URL)  exit 0   the tier that must be green on a
                                   bare machine; tests/schema_isolation.rs
                                   reports 4 passed, 5 ignored there, which is
                                   the point of splitting that file
```

`cargo llvm-cov --fail-under-lines 50` was not run: it is kit's CI job's floor,
it is deliberately not in `bin/prime`, and this change adds test files rather than
library code. `cargo clippy --all-targets --all-features -- -D warnings` did run,
inside `bin/prime`.

## 9. Environment notes, which are not code defects

- The measurements used a **private `postgres:17-alpine` on port 15999 with CI's
  exact credentials** (`darkroom`/`darkroom`/`darkroom_test`), not kit's shared
  cluster on 15500, for two reasons. The shared cluster caps each role at
  `KIT_POSTGRES_ROLE_CONNECTIONS=20`, and a concurrency packet measured against an
  artificial connection ceiling would be measuring the wrong thing — peak was 17
  backends, uncomfortably close to that 20. And `REPORT-darkroom-09-isolation.md`
  records two checkouts on one database destroying each other's fixtures
  mid-assert, which is the hazard this packet is about and is not worth
  reproducing on purpose. The local cluster's shape is unchanged by this work.
- The measurement database's `public` schema holds the tables and the ledger the
  pre-change runs left behind, and the divergence-3 run truncated them (that is
  what a `search_path` on one connection does). Nothing in this tree reads them
  any more; the guard that proves it queries the catalog through a pool with no
  schema on its path and asserts each `t_<hex>` has its own.
- Another session was working in `cafaye/kit` on this machine during the packet.
  No shared state was touched: this worktree is `wt-m39-darkroom-` prefixed, the
  database is a private container, and `git worktree list` shows only this
  worktree's branch moved. Nothing was pushed.

## 10. What a successor should know

1. **The `--test-threads=1` story is settled in both directions.** Six red runs
   before, five green after, a canary that goes red on demand when the isolation
   is removed, and serial that is now the slower of two correct runs. If a future
   test cannot be isolated it gets its own exclusion — the packet's rule, now
   written into `bin/prime`.
2. **A `truncate` aimed at the wrong schema is invisible, and that took a
   planted divergence to find.** Nothing held it before this report's last commit,
   because every store is created empty and a `public` truncate cannot reach into
   another schema. `tests/schema_isolation.rs` now asserts both halves; if you
   touch `truncate`, re-plant `public.assets` and watch for one red test.
3. **Every run leaves a schema behind and the janitor is what removes it.** Six
   hours is the threshold, it was chosen for two stated reasons (no live run has
   a schema that old; a database used weekly does not accumulate them forever),
   and it is the one piece of this change whose constant nobody has measured
   against a real usage pattern. If it is wrong, it is wrong in the safe
   direction — schemas accumulate.
4. **A `drop schema` in this tree is the sharpest edge in the test suite.** It
   fires on schemas matching `^t_` in `TEST_DATABASE_URL`'s database. Two rules
   hold it, both unit-tested without a database: the name's alphabet and the
   stamp's shape and age. Do not add a third `drop` without adding a rule.
5. **sqlx's migration lock is still on, deliberately.** It serialises concurrent
   tests' DDL database-wide. Measured cost: nothing visible (the tier is 9.2s
   parallel against 17.1s serial, and `tests/api.rs` dominates both). Turning it
   off needs `#[doc(hidden)] pub` fields and a reason.
6. **Do not add `public` back to the search path.** The reason is not the one you
   will guess — the migrations survive it. The reason is §2(a): a table this
   schema lacks and `public` has resolves to `public`'s copy instead of erroring.
7. **A guard written here has already failed twice.** The `t_zzzz` plant was a
   fixed name (a flake waiting for a failed run), and the `truncate` guard was
   missing entirely. The pattern that found both was the same: plant the
   divergence, run it, and treat "the suite stayed green" as a finding about the
   guard rather than as good news.
