# REPORT-darkroom-policy-probes-01 — packet `w41-darkroom-policy-probes-01`

Branch `worker/w10-policy-probes`, from `18f025b`. Two tests, two floors, one
planted divergence each. No executable SQL moved.

## The short version

Both absences the stale-docs report left are now measurements. The `(select …)`
wrapping has a behavioural count, and `credential_tables()` has a cross-role
comparison. Each was run red against a planted divergence before it was
believed, and each plant was reverted and verified byte-identical.

## Item 1 — the `(select …)` wrapping, measured

### The instrument, and why it is the one

The packet named two statistics tools to consider before designing around
them. Both were checked on this cluster and both are unavailable:

- **`pg_stat_user_functions`** — the view exists, but `track_functions` is
  `none` and it is a `PGC_SUSET` GUC: `set track_functions = 'pl'` as `darkroom`
  fails with `permission denied to set parameter`, and `darkroom` is not a
  superuser (`rolsuper = f`). Turning it on would need `ALTER SYSTEM` or
  `ALTER DATABASE` as a superuser, and it is a persistent change to a shared
  cluster — a side effect no test should have. Not used.
- **`pg_stat_statements`** — available in `pg_available_extensions` but not
  installed, and it requires `shared_preload_libraries` (which `darkroom` cannot
  even examine: `permission denied to examine "shared_preload_libraries"`), so
  installing it needs a cluster restart. It also counts STATEMENTS, not function
  invocations — the wrong instrument for counting how many times a function was
  called inside one statement. Not used, and for two independent reasons.

So the probe is the one kit's `tests/tenancy_test.sh` used and the one the
packet describes: a function whose call count you can observe. It is:

```sql
create or replace function probe_identity()
returns uuid language plpgsql volatile security definer as $fn$
begin update probe_counter set n = n + 1; return '11111111-…'; end $fn$;
```

- **`volatile` is load-bearing.** A non-volatile function cannot update the
  counter at all — measured as `ERROR: UPDATE is not allowed in a non-volatile
  function`. This is the one design constraint the measurement imposes, and it is
  why the probe is not `stable`.
- **`security definer`** so the count works for whichever role queries the
  fixture. With invoker rights, a role that did not own the counter would be
  refused the update and the measurement would fail for a reason that has
  nothing to do with the plan. (The test queries as `darkroom`, the counter's
  owner, so invoker rights would work — but the probe is written to be
  role-independent, which is the property the fixture is there to have.)

### The measurement, before and after

The "before" is the recorded figure in `migrations/0004`'s header — five rows,
one call wrapped, five bare — a measurement of this kit's Postgres that nothing
re-derived. The "after" is `TestTheSelectWrappingIsMeasuredNotAsserted`, which
re-derives it on every run:

| form | rows | calls |
|---|---|---|
| `using (account_id = (select probe_identity()))` | 5 | 1 |
| `using (account_id = probe_identity())` | 5 | 5 |

Measured on this cluster, PostgreSQL 17.11, and asserted by the test. The
mechanism, confirmed with `EXPLAIN (ANALYZE)`: the wrapped form plans as
`InitPlan 1` with `Filter: (account_id = (InitPlan 1).col1)` — the uncorrelated
scalar subquery is hoisted and evaluated once per statement. The bare form plans
as `Filter: (account_id = probe_identity())` — a per-candidate-row evaluation.
The InitPlan conversion is in `build_subplan` (`optimizer/plan/subselect.c`) and
keys on the subquery being uncorrelated (`parParam == NIL`); there is no
volatility test on that path, which is why the volatile probe is hoisted. The
`contain_volatile_functions` checks in that file are for CTE inlining and
ANY/EXISTS-to-join conversion, not for InitPlan hoisting — a distinction that
matters and is easy to get backwards.

The row count is asserted beside the call count under both forms: a policy that
hides every row also satisfies "the helper was called", so the fixture must
prove the query returns its five rows or the call count means nothing.

### The planted divergence, and what it does and does not prove

The divergence this test exists to catch — a planner that stops hoisting the
wrapped form — is a Postgres upgrade, and no plant on this cluster can simulate
one. What is plantable is a probe that cannot count, and the test goes red on
that:

```
the wrapped form called probe_identity 0 time(s), want 1.
the bare form called probe_identity 0 time(s), want 5.
```

Both halves fail, which is the proof that the test is sensitive to the probe
actually counting. The bare half (want 5) is the control that demonstrates
sensitivity to per-row evaluation — the same blindness a planner regression
would produce. The plant was reverted and the test is green.

**What is left, honestly:** the test cannot be shown red against the regression
it is named for, because that regression is not a code change. The control half
is the evidence that the probe can see per-row evaluation, and the wrapped half
is what would report more than 1 if the planner ever stopped hoisting. A guard
that has never been shown to fail is a claim; this one has been shown to fail on
the only divergence this cluster can produce, and the limit is recorded rather
than rounded off.

## Item 2 — `credential_tables()` across roles

### The check

`TestCredentialTablesAnswersEveryRoleTheSame` builds a credential table with the
substrate's own entry point — `cafaye.protect_credential_table('api_keys',
'token_digest')` — and then asks `cafaye.credential_tables()` four times:

| role | search_path | why |
|---|---|---|
| owner (`darkroom`) | default | the role every other test in this package uses |
| `darkroom_app` | default | the login role a request holds, and the one that has been wrong before |
| owner | `cafaye, public` | the old bug's condition, reproduced |
| `darkroom_app` | `cafaye, public` | both |

All four must return the identical, non-empty answer: exactly one row, the
fixture. The "exactly one" is also the negative control — `assets` is protected
by the same migration and has no resolve policy, so if it appeared the answer
would be two rows and the test would fail.

### The planted divergence

The old query matched the deparsed policy expression against the schema-qualified
function name. `pg_get_expr` drops the schema from any name the reader's
`search_path` resolves, so with `cafaye` on the path the match found nothing.
The plant reconstructs that mechanism:

```sql
… and pg_get_expr(pol.polqual, pol.polrelid) like '%cafaye.current\_credential\_digest%' escape '\'
```

Against the current OID-based query the test is green. Against the plant it goes
red on exactly the two `cafaye`-on-path calls:

```
cafaye.credential_tables() as "" under search_path "cafaye, public" returned [], want [t_….api_keys (token_digest)].
cafaye.credential_tables() as "darkroom_app" under search_path "cafaye, public" returned [], want [t_….api_keys (token_digest)].
```

The two default-path calls stay green, which is what makes the red specific: the
test is not merely detecting a broken query, it is detecting the exact
role-and-path-dependent failure the old bug had. The plant was reverted and the
migration is byte-identical to its pre-packet state (`git diff` empty).

## Verification

```
$ ./bin/prime --db                      → ==> ok
$ ./bin/tier-counts /tmp/prime-w10.log
  ok  default  variant-pipeline checks               24
  ok  database  harness and role checks              17
  ok  database  service checks                       39
  ok  database  httpapi checks                       34
  ok  identity  ./internal/darkroomdb/pgtest         17 enumerated, 17 passed with a database
  ok  identity  ./internal/service                   39 enumerated, 39 passed with a database
  ok  identity  ./internal/httpapi                   34 enumerated, 34 passed with a database
  ==> tier-counts ok

$ grep -c -- '--- SKIP:' /tmp/prime-w10.log   → 0
$ grep -c -- '--- FAIL'  /tmp/prime-w10.log   → 0

$ ./bin/gate-self-test   → PASS, all 23 breakages red, 5 warnings green, nothing skipped
```

The pgtest floor moves 15 → 17 in `bin/prime`, `bin/tier-counts` (constant and
header table) and `.github/workflows/ci.yml`. The enumerated-equals-passed
identity holds for both new tests. The migration files are byte-identical before
and after (`git diff` empty on `migrations/`).

## What this packet is not

- **Not a comment fix.** Both items were left because they needed tests. The
  comments in `migrations/0004` that record the `1 call` against `5 calls`
  figure and the `search_path`-independence of `credential_tables()` are now
  backed by a measurement and a check respectively, and the comments say so.
- **Not a rewrite of the policy substrate.** The probe and the fixture are built
  by the tests, around what exists. `protect_table` and `protect_credential_table`
  are untouched.
- **Not the coverage floors.** The README tier table and the `bin/prime` floors
  are the documented rule. The floors moved because the tests moved them, in the
  same commit, in all three copies.
