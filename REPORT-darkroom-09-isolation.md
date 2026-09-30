# darkroom-09 — making tenant isolation load-bearing

The implementation was already correct. That was the finding, and it is also the
problem: `assets` and `asset_variants` carry `account_id uuid not null`, there is
a `unique (account_id, checksum)`, and all ten account-scoped queries constrain
on the column —

```sql
where id = $1 and account_id = $2
update assets where id = $1 and account_id = $2 and status = 'pending'
delete from assets where id = $1 and account_id = $2
```

— and **nothing held any of that in place.** Every existing test exercised the
queries *through* a `Tenant`, so a future edit that dropped `and account_id = $2`
from one line turned a private asset store into a shared one and every test in
the repository stayed green. This packet makes the correctness load-bearing.

> **Status: verified after an OOM restart.** The work was recovered as commit
> `recover(worker/darkroom-09-isolation)` and re-checked from a clean
> `src/store.rs` before anything below was believed. It did not pass as it
> stood: two guards hung for eleven minutes, one was satisfiable by the very
> column it exists to reject, a set comparison asserted an accident of file
> layout, and a new test failed on roughly half of all runs. All four are
> written up under "Three bugs the guards had themselves" and the two
> behavioural defects are planted-and-caught below. **Nothing in this report is
> inherited from the interrupted run's conclusions.**

## The count

| | |
|---|---|
| **Account-scoped entry points** | **25** |
| — tenant-scoped routes (`http::OPERATIONS`) | 7 |
| — service methods taking a `Tenant` | 7 |
| — account-scoped store queries | 10 |
| — the idempotency replay path | 1 |
| **Negative tests asserting A is refused B** | **32** (was 7) |
| — wire (`tests/tenant_isolation.rs`) | 12 |
| — query layer (`tests/query_scoping.rs`) | 7 |
| — idempotency ledger (`tests/idempotency.rs`) | 1 |
| — structural, no database (`tests/tenant_scoping.rs`) | 12 |
| **Operation kinds covered at the query layer** | **4 of 4** — read, list, update, delete |
| `403` responses found for an invisible resource | **0** |
| `403` responses added | **0** |

Every number here is checkable. The 7 routes are derived from `http::OPERATIONS`
and asserted in `every_tenant_scoped_route_has_a_negative_case`; the 10 queries
are derived from the `&Tenant` signatures in `src/store.rs` and asserted in
`every_tenant_scoped_query_constrains_account_id`. A reader who disagrees can
run two commands and find out.

## The enumeration, and what each entry point is

### Routes — 7, all under `/v1`, all taking the tenant from the token

| route | kind | negative case |
|---|---|---|
| `POST /v1/uploads` | write | `a_create_body_cannot_name_an_account` |
| `POST /v1/uploads/{id}/complete` | update | `a_cross_tenant_complete_does_not_fail_another_accounts_upload` |
| `GET /v1/assets` | list | `a_listing_returns_only_the_callers_own_assets` |
| `GET /v1/assets/{id}` | read | `a_cross_tenant_404_is_indistinguishable_from_a_missing_one` |
| `DELETE /v1/assets/{id}` | delete | `a_cross_tenant_delete_is_404_and_the_asset_survives` |
| `POST /v1/assets/{id}/variants` | write | `a_cross_tenant_variant_write_touches_nothing` |
| `GET /v1/assets/{id}/variants` | list | `a_cross_tenant_variant_list_is_404_not_an_empty_list` |

Two further cases cover *inputs* to a route rather than routes of their own: a
cursor replayed across accounts (`a_cursor_from_another_account_pages_only_the_
callers_own_rows`) and the same bytes in two accounts
(`the_same_bytes_in_two_accounts_are_two_assets_and_never_a_credential`).
`/healthz` and `/readyz` are the unauthenticated probes and hold nobody's data.

### Store queries — 10, every one taking a `&Tenant`

| kind | queries |
|---|---|
| read (4) | `find_asset`, `find_asset_by_checksum`, `find_storage_key`, `find_variant_storage_key` |
| list (3) | `list_assets`, `list_variants`, `list_storage_keys` |
| update (2) | `mark_ready`, `mark_failed` |
| delete (1) | `delete_asset` |

**And the one that does not:** `find_stale_pending` reads every account's rows,
has no `Tenant`, and is correct — it is the stale-upload sweeper, and a sweeper
scoped to one account is a sweeper that leaves every other account's abandoned
uploads in `pending` forever. It is safe because no request path can reach it,
and both halves of that are asserted: the signature, and the absence of the name
from `src/service.rs` and `src/http.rs`. This was the only deliberate exception
and it is now a closed set rather than a habit.

`insert_asset_keyed` and `upsert_variant` write an entity that carries
`account_id`, so there is nothing to scope a read by; both are covered by
`account_id` in their insert.

## What the three files do

The work splits along one line: *is the scoping still written down* is a property
of the source, and *does the scoping actually hold* is a property of Postgres.
Neither can substitute for the other — a `where` clause that names `account_id`
and a `where` clause that is correct are the same string in a file and different
queries in a database — so they are three files, not one, and each says so.

### `tests/tenant_scoping.rs` — the default tier, no database

Reads `src/store.rs` with `include_str!`, so a failure is a fact about the source
that built the binary rather than a grep somebody has to remember to run, and
derives its expectations from the code rather than from a maintained list. Five
properties, each load-bearing alone:

1. **Every `&Tenant` query constrains `account_id = $N`.** Derived from the
   *signature*, so dropping the clause from `delete_asset` — which does not change
   its signature — fails here.
2. **That set is exactly the ten named.** A new scoped query that forgot the
   column is a new name and fails; so does a removal, because a query nobody can
   find is a query nobody has ever run.
3. **A mutating scoped query names a row, not just an account.** `update assets
   set status = 'failed' where account_id = $2` is scoped and still a defect — it
   rewrites every row an account has. Necessary and not sufficient. The predicate
   is anchored, and the anchoring is itself tested — see the guard's own bug
   below.
4. **Exactly one query reads across accounts, and no request path reaches it.**
5. **`principal_scope` carries the account**, because the idempotency ledger
   stores whole response bodies.
6. **The four/two/one operation-kind breakdown is asserted**, not described, so
   the file's headline coverage claim cannot rot into prose.

It is in the **default tier on purpose**: the structural half of the isolation
guarantee should be checked on a machine with no Postgres and no Docker, because
that is where it will be run most often and it is the half that catches the edit
before a fixture is built. Pinned as `TENANT_SCOPE_TESTS=12` in both default
tiers — a security check that reports zero is worse than one that is absent,
because it looks like coverage.

### `tests/query_scoping.rs` — the behavioural half, against Postgres

A two-account fixture, four operation kinds as four separate tests, and the
count asserted rather than implied. The failure this exists for is the common
shape: **a service that scopes its reads and forgets its delete.** `delete_asset`
is a single `where` clause, a cross-tenant delete is data loss rather than
disclosure, and no read-path test in the repository would have noticed.

Three deliberate choices, each earning its place:

- **Both accounts share a checksum.** `unique (account_id, checksum)` makes that
  two legitimate rows, and it is the case a scoping bug hides in: an unscoped
  lookup by checksum does not error and does not return nothing, it returns a row
  and the *wrong* one, while every id-keyed test stays green. A scoping check
  that never puts the same bytes in two accounts has not tested the interesting
  half.
- **"Changed nothing" is asserted, not "returned None".** A cross-tenant
  `mark_ready` that missed its scoping and then lost a second race on the
  `status = 'pending'` guard would still return `None` — and have advanced
  somebody else's upload. So each negative write re-reads the row *as its owner*
  and asserts the status, and for `mark_failed` the `failure_reason` in the
  metadata separately, because a write that missed its scoping and its status
  guard would still have left the reason behind.
- **A mutation names a row, not an account.** Covered structurally in the default
  tier and behaviourally here, because the second is what a `delete … where
  account_id = $2` typo would look like.

Also covered: a cursor replayed across accounts (the keyset predicate and the
tenant predicate are separate clauses, and a regression dropping the second pages
out of one account and into the other, silently, as a 200); `list_storage_keys`
(two `select`s in a `union all`, so the query most able to be half-scoped — the
`assets` half constrained and the `asset_variants` half not); and
`find_variant_storage_key`.

### `tests/tenant_isolation.rs` — over the wire, 7 → 12 cases

The load-bearing change is that the byte-identical-404 guard now covers **all
five** id-scoped routes instead of only `GET /v1/assets/{id}`. A rule checked on
the oldest route is a rule checked on one route, and the route somebody added
most recently is where it would be dropped.

New cases: cross-tenant `complete` proving A's upload is still `pending`
afterwards; cross-tenant variant write proving nothing moved **including the
outbox** (a `darkroom.variant.created` naming A's asset that B caused is a
message on somebody else's bus, and one that inserted a row and then rolled back
leaves the event as its only trace); the variant listing as 404 rather than an
empty array; a cursor across accounts; and the same bytes in two accounts never
yielding a presigned URL scoped to the other one's storage key — a presigned URL
is a write credential, so that leak would be a write, not a read.

## Absence, not refusal

**A resource the caller cannot see does not exist, and every layer below the wire
says so the same way.** Stated in `src/store.rs` and `AGENTS.md` because it is
the rule the platform should copy: every service that reads another service's
data has to answer "what do I say when the caller is authenticated and the row is
not theirs?", and there is one correct answer — the same thing you say when the
row does not exist. A `403` is a confirmation: it tells the caller the row is
real and somebody else owns it, which is a smaller leak than the row and a
perfectly good way to enumerate the platform.

This holds below the wire too, which is the part that was not written down:
`store::find_asset` returns `Ok(None)` for another account's row, **never a
`StoreError`**, because a distinguishable error is the same oracle one layer
down. Every negative assertion in `tests/query_scoping.rs` is `None`, `false` or
empty, and the `.expect("no error, only absence")` in each is there to make the
point that a *distinguishable* failure would be a defect, not a diagnostic.

And where emptiness would itself answer the question, the answer is not empty:
`GET /v1/assets/{id}/variants` returns 404 for another account's asset rather
than `[]`, because an empty list is indistinguishable from "no variants yet" and
that is an existence oracle with one bit.

### On `403`

**No `403` was found for a resource the caller cannot see, and none was added.**
The one `403` in the service is `require_scope`, which is a capability failure —
the token authenticated and lacks `assets:read` — and it is correct for two
reasons: there is no existence to leak (the caller never got as far as naming a
resource), and core's convention draws the line in exactly that place.
`a_token_without_the_scope_is_403_and_anonymous_is_401` already stated it and
still does. The distinction is load-bearing in both directions: a `403` for
*tenancy* would be a finding to fix, and a `404` for *capability* would throw
away a distinction the caller is entitled to. `403` for a resource the caller
cannot see was treated as a finding throughout, and none was present.

## Findings

1. **The `Forbidden` variant's doc comment described the anti-pattern, and it
   was the one real hazard found. Fixed.** `src/error.rs` read: *"The caller
   authenticated and holds the scope, but the account in the token is not
   theirs."* That sentence states, as the variant's purpose, exactly the thing
   the rule forbids — a 403 for a resource the caller cannot see. It then
   contradicted itself two clauses later, because the rest of the comment was
   already correct.

   The implementation was never affected: `Forbidden` has one constructor,
   `Error::forbidden`, called from one place, `http::require_scope`. So nothing
   was broken today. But a comment is what a future author reads before writing
   `if caller.account != asset.account { return forbidden() }`, and this one
   invited it while the following sentence forbade it. The rewritten comment
   states the capability case positively, names the tenancy case as `NotFound`,
   and says why the two differ in kind rather than in degree: a missing scope is
   a fact about the *caller*, which they already know, so reporting it leaks
   nothing; a foreign resource is a fact about *somebody else*, and reporting it
   is the leak. It also records that the single-constructor property is what
   makes a 403 incapable of becoming a tenancy response by accident.

   **No behaviour changed.** This is a doc comment, and the check that it now
   says the right thing is the one already in
   `a_token_without_the_scope_is_403_and_anonymous_is_401`.

2. **`find_variant_storage_key` and `find_storage_key` are correct and uncalled.**
   Both are properly scoped, so nothing is broken, and both are now covered by
   negative cases in `tests/query_scoping.rs` — an uncalled query that is *tested*
   is defensible, an uncalled query that is neither tested nor called is
   decoration. `find_variant_storage_key` is the weaker of the two: the delete
   path uses `list_storage_keys`, which gets both the original and the variants in
   one query, so nothing needs it today. Removing it is a one-line change and a
   separate decision; leaving it scoped and tested is the safe half.

3. **No other tenancy hazard was found.** No unscoped query, no route missing a
   case, no place where a cross-tenant request returned something
   distinguishable from absence. The implementation was as correct as the packet
   said; the only thing standing between it and a future leak was a comment that
   described the wrong thing.

## Proven able to fire

A guard nobody has watched fail is a guard nobody knows works. Four
divergences were planted in `src/store.rs` and reverted. Every one was caught,
and the messages below are the real output, not a paraphrase.

| # | planted | caught by | tier |
|---|---|---|---|
| 1 | `delete_asset` lost `and account_id = $2` | `every_tenant_scoped_query_constrains_account_id` **and** `exactly_one_query_reads_across_accounts_and_it_is_the_sweeper` | default, **no database** |
| 2 | `mark_failed` lost `and account_id = $2` | `every_tenant_scoped_query_constrains_account_id`; and `an_update_from_another_account_changes_nothing` + the sweep behaviourally | default + database |
| 3 | a mutation scoped to the account but naming no row | `a_mutating_scoped_query_names_one_row_and_never_a_whole_account` | default |
| 4 | `find_asset_by_checksum` lost `account_id = $1` | the same two structural guards; and `every_read_is_scoped_to_the_callers_account` + `the_same_bytes_…` + the sweep behaviourally | default + database |
| 5 | a second unscoped read beside `find_stale_pending` | `exactly_one_query_reads_across_accounts_and_it_is_the_sweeper` | default |

Divergences 1 and 4 were each planted, caught, and reverted during this
re-verification, not inherited from the first draft — the recovered work was
re-measured from a clean `src/store.rs` rather than trusted.

Divergence 1, verbatim, from the default tier with **no Postgres running**:

```
test result: FAILED. 10 passed; 2 failed; 0 ignored
---- every_tenant_scoped_query_constrains_account_id ----
store::delete_asset takes a &Tenant but its SQL does not constrain account_id,
so account A could read, update or delete account B's rows. This file exists
for exactly that line.
---- exactly_one_query_reads_across_accounts_and_it_is_the_sweeper ----
these read tenant data with no account_id predicate:
  ["find_stale_pending", "delete_asset"]
```

and the same divergence caught behaviourally, against real Postgres:

```
test a_delete_from_another_account_removes_nothing ... FAILED
A must not delete B's asset
```

That behavioural failure is the important one: **B's asset was actually
deleted** in the test database. The vulnerability is not hypothetical, the
default tier catches the edit before it happens, and the database tier
demonstrates what it would have cost.

Divergence 4 is the one that proves the shared-checksum fixture earns its place.
Dropping `account_id = $1` from `find_asset_by_checksum` produced:

```
test every_read_is_scoped_to_the_callers_account ... FAILED
test the_same_bytes_in_two_accounts_are_two_assets_and_neither_account_sees_the_other ... FAILED
test no_scoped_query_ever_returns_a_row_under_the_wrong_account ... FAILED
test result: FAILED. 4 passed; 3 failed
```

An unscoped checksum lookup does not error and does not return nothing — it
returns a row, and the wrong account's. A read that resolves the wrong tenant's
asset is a *write* leak one step later, because the caller then PUTs bytes at
the storage key the response handed back.

Two results worth stating because they are not what the plan predicted:

- **Two independent guards caught divergence 1**, not one. The derived
  "exactly one query reads across accounts" set catches a `delete` losing its
  predicate, because a `delete … where id = $1` is also a read of the table from
  the analysis's point of view. That redundancy is why the sweeper test exists
  rather than being folded into the first one.
- **Divergence 3 was caught by a guard the plan did not know was needed.**
  Scoping an `update` to `where account_id = $2` — with the tenant predicate
  present and correct — is scoped and is still a defect, because it rewrites
  every row in the account. No `account_id`-presence check can see that, and the
  "names a row" rule exists only because that case was thought about while
  writing the file. **Then the guard for that case turned out to be
  satisfiable by `account_id` itself**, which is the second of the three bugs
  above and the reason the anchoring now has its own test.

### Three bugs the guards had themselves

This is the part of the packet worth the most, and all three were found by
**running** the guards rather than reading them. Every one has the same shape: a
check that cannot fail, or a check that fails for a reason unrelated to the
property it exists to assert.

- **The string scanner was quadratic, and the guard hung instead of running.**
  `sql_statements` left its cursor sitting *on* a closing quote rather than past
  it, so every literal in the text bought a fresh forward scan of everything
  after it. `src/store.rs` carries doc comments containing `"` characters, and
  because a unit runs to the next `pub async fn` it swallows the following
  function's prose — so the input was much larger than the SQL. Two tests spent
  **eleven minutes** of CPU and were still running when the gate was killed at
  the machine's own limit. A hanging guard is worse than a missing one: it gets
  deleted rather than fixed, and it burns a CI slot. The fix is one character,
  `&rest[len..]` to `&rest[len + 1..]`, with the unterminated-literal case
  handled so a stray quote in prose cannot index one past the end.

  This is worth stating plainly: the guard that exists to make tenant scoping
  load-bearing **could not be executed at all** until it was fixed. The packet's
  central claim — that a bare machine checks the structural half — was false
  until the scan was linear.

- **The "names a row" predicate was satisfied by `account_id` itself.** The
  mutation guard asked for `sql.contains("id = $")`, and the substring `id = $`
  occurs *inside* `account_id = $2`, one character in. So
  `update assets set status = 'failed' where account_id = $2` — which rewrites
  every pending row an account has ever uploaded, the exact defect that guard
  exists to catch — **passed the entire file**, and `cargo test` reported
  `11 passed; 0 failed` while a whole-account update sat in the source.

  Found by mutation, not by argument, and it is the single strongest argument in
  this report for testing the guards. An unanchored `contains` here was not a
  weak test, it was an inverted one. The match is now anchored to a whole
  identifier, and `naming_a_row_is_anchored_to_a_whole_identifier` asserts the
  anchoring against the exact mutation that defeated the old version — so the
  next refactor of that helper fails rather than silently re-opening the hole.

- **The scoped-query set was compared as a sequence.** The derived side is the
  order `src/store.rs` declares its functions in; the constant groups them by
  operation kind. `find_asset_by_checksum` precedes `find_asset` in the source
  and follows it in the constant, so the assertion failed on a tree where **both
  lists held the same ten names**. It is now compared as a sorted set, and the
  kind grouping the constant exists to document is asserted on its own by
  `the_operation_kinds_are_what_the_comment_claims` — so the coverage claim is
  load-bearing instead of being a comment that can rot.

A fourth defect was not in a guard but in a new test, and it is the one that
would have shipped a permanently-flaky gate:

- **`one_accounts_key_cannot_replay_another_accounts_response` compared
  uuid-ordered rows against declaration-ordered expectations.** The query says
  `order by account_id`; Postgres orders uuid by raw bytes, so the result is
  "whichever account id is smaller" — a coin flip between A and B. The
  expectation was written "A then B", so the test failed on **roughly half of all
  runs**, and the failure read
  `left: [c494acf5, f9ff6853] right: [f9ff6853, c494acf5]`: a shape that looks
  precisely like a cross-tenant leak and is not one. Both sides are now sorted.
  The sibling test in `tenant_isolation.rs` already carried the comment explaining
  this; a rule that only one file knows is a rule the next file re-learns by
  failing.

## What was not done, and why

- **No abstraction was invented.** The brief allows a `(account_id, id) -> Option`
  helper and says a single test could then cover the class. `store.rs` already has
  that shape: every scoped query takes a `&Tenant` and returns `Option` for a
  miss. Adding a helper would have been a second way to do the same thing, and
  the class was covered by deriving the existing shape instead — which is
  stronger, because a derived expectation cannot go stale.
- **No new `403`s, and no `403` converted to a `404` that was correct as a
  `403`.** See above.
- **No sleeps, no raised retries, no loosened assertions.** The cursor and
  duplicate cases use real second requests and a real interleaved fixture rather
  than waiting for a timestamp; the `mark_failed` and `mark_ready` cases assert
  the row afterwards rather than asserting a timing window.
- **`find_variant_storage_key` was not removed.** It is correctly scoped and now
  has negative cases, which is the defensible half of "uncalled query"; deleting
  a `pub` function is a separate decision and not this packet's to make.

## The environment, and executed versus skipped

The database tier is environment-gated on **`TEST_DATABASE_URL`** and needs a
Postgres; `docker compose up -d postgres` or any reachable instance will do. The
counts, reported separately because a CI job that silently skips the hard part is
worse than no CI:

| tier | command | executed | skipped |
|---|---|---|---|
| 4 | `cargo test` | 12 tenant-scoping checks, no database needed | 54 `#[ignore]`d for want of `TEST_DATABASE_URL` |
| 5 | `cargo test --features s3` | the same 12, in the deployment build | 54 |
| 6 | `cargo test -- --ignored` | 54 passed, 7 of them query-scoping | 0 |
| 6 | `cargo test --features s3 -- --ignored` | 54 passed | 0 |

`bin/tier-counts` reads those numbers out of a `bin/prime --db` log and fails if
the default run's skip count and the database run's pass count ever disagree —
they are the same set of tests, so 54 == 54 is an identity and not two plausible
numbers.

`bin/gate-self-test` also passes on this tree: 23 planted breakages all go red,
5 warning cases stay green with exit 0, both controls are green, and nothing was
skipped. Worth running after a packet that adds a tier, because a declaration
whose proof no longer matches what the gate prints calls itself self-contained
and the failure is silent. `./bin/gate-self-test`, run against the same database, is green with
**nothing skipped** — and it reports a skip as a failure, so the 5 database-gated
cases in it were genuinely executed rather than passed over.

### The gate on this machine, and one environment finding

`./bin/prime --db` exits 0 and `bin/tier-counts` reports `77 unit, 89 with s3,
54 database x2, 9 backend-table rows, 9 openapi drift x2, 12 tenant scoping x2,
7 query scoping`.

Getting there needed a Postgres, and **`docker compose up -d postgres` does not
work on this machine** — port 5432 is already bound by a host Postgres (pid 845)
and by another container, so the compose container starts and then the URL in
`docker-compose.yml` connects to somebody else's database. The connection fails
with `role "darkroom" does not exist`, which reads like a missing migration and
is actually a port collision. Two further properties of the machine, recorded
because both cost time and neither is visible from the repository:

- **A test that shares a database with another worktree is not reliable.** The
  suite truncates the tables it touches, so two checkouts running the same
  database destroy each other's fixtures mid-assert. Four `tests/api.rs`
  failures — `left: 2, right: 1`, "the row is gone", `left: 4, right: 2`, and a
  missing `x-darkroom-duplicate` header — were somebody else's `--db` run, not
  defects: the same suite is green on its own, twice. The two causes were an
  orphaned `bin/prime --db` left behind by a killed run, and a core
  `gate_check.py --prove` in this worktree. `darkroom09-pg` on port 15543 is
  this worktree's own instance and the database tier is green on it. Worth
  naming because each of those assertions is a *correct* assertion failing for a
  reason that has nothing to do with the code under test — and a reader who
  believed it would go looking for a delete bug that does not exist.
- **`cargo fmt --check` piped into `tail` reports success while failing.** The
  recovered work was unformatted, and `./bin/prime --db … | tail -40` printed
  `PRIME EXIT=0` on the same run that failed `fmt`, `clippy` and three suites.
  That is this repository's own `gate.yml` note about `… | tail` under zsh, met
  again in practice. Every exit code quoted in this report was taken from
  `$pipestatus[1]` or from an unpiped run.

## The numbers a reader can check

```sh
# the 7 tenant-scoped routes, from the table the router is built from
grep -c 'path: "/v1' src/http.rs

# the 10 account-scoped queries, from the &Tenant signatures
grep -c 'tenant: &Tenant' src/store.rs

# the 12 structural checks — the tier that needs no database
grep -c '^#\[test\]' tests/tenant_scoping.rs

# the 7 query-layer cases
grep -c '#\[ignore' tests/query_scoping.rs

# the 12 wire cases
grep -c '#\[ignore' tests/tenant_isolation.rs

# the gate, and the counts behind it
./bin/prime --db > /tmp/prime.log 2>&1
./bin/tier-counts /tmp/prime.log
```

Note the two greps that count `fn` rather than `#[test]`: the file has six
helper functions as well as its tests, so `grep -c '^\s*async fn\|^fn '`
returns 20 and looks like a disagreement that is not one.
