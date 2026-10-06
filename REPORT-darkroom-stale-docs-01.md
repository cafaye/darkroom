# REPORT-darkroom-stale-docs-01 — packet `w8-stale-docs`

Branch `worker/w8-stale-docs`, from `2a43344`. Prose only: no Go source, no
test, no floor, no executable SQL.

## The short version: the five were already fixed before this packet started

Every one of the packet's five was corrected by a commit already on this
branch's history. `git log --oneline master..HEAD` was **empty** at the start —
HEAD was `2a43344`, identical to `master`. The five are not drift to fix; they
are drift that was fixed, three days of work ago, by four different packets:

| # | site | fixed by | what that commit did |
|---|---|---|---|
| 1 | `internal/service/outbox.go` | `bd53145`, refined `68c476d` | the `claimUnpublished` comment now says the function does not exist, names `[countOutbox]` as the file's only read path, and records that the Rust tree's real `claim_unpublished` was deliberately not ported |
| 2 | `migrations/0004_account_isolation.sql` | `331daf2` | `bin/dev down -v` → the three routes that actually re-apply a set. Also fixed `StorageKey::generate_*` and the `sqlx::migrate!` justification |
| 3 | `cafaye.yml` | `8385496`, `892728e` | `exposes.api` now says the Go HTTP layer IS here and the router is generated from this exact document |
| 4 | `migrations/README.md` | `8385496`, `869299c` | "the Rust tree this repository still carries" → the Rust tree, which no longer exists here; `sqlx-cli` gone |
| 5 | `.github/workflows/ci.yml` | `8385496`, `58c7339`, `2a170d7` | the cargo-shaped tier list is gone |

So the honest result of this packet is: **verify the five, then grep past them.**
The verification is below; the grep found four real things the handoff never
reached, and those are the commit.

## Grep, before and after

Run over the whole tree, `.git` excluded, nothing else. Historical records
(`HANDOFF-*`, `REPORT-*`, `CHANGELOG`) are listed but not counted as live
prose — they are history and history is supposed to name the old thing.

### BEFORE (`2a43344`)

```
tests/tenancy_test.sh      3 hits outside HANDOFF/REPORT/CHANGELOG
tests/schema_isolation     4 hits outside HANDOFF/REPORT/CHANGELOG
tests/common               0
find_stale_pending         1 hits outside HANDOFF/REPORT/CHANGELOG
claimUnpublished           2 hits outside HANDOFF/REPORT/CHANGELOG
bin/dev down               0
sqlx-cli                   0
StorageKey::generate       0
```

The five the packet named are already at zero. `tests/tenancy_test.sh` — **not
in the packet's list, and the same defect class as its item 2** — is at 3.

### AFTER

Every remaining hit, whole tree:

```
tests/tenancy_test.sh   migrations/0004_account_isolation.sql:157  "had both in `tests/tenancy_test.sh`, deleted with the rest of `tests/`"
                        migrations/0004_account_isolation.sql:453  "the Rust `tests/tenancy_test.sh` that counted the calls is gone with"
                        migrations/0004_account_isolation.sql:502  "`tests/tenancy_test.sh` used to measure both roles on every run"
tests/common            .github/workflows/ci.yml:265               "the `tests/common/mod.rs` tier list this file used to carry"
                        + HANDOFF / REPORT only
tests/schema_isolation  internal/darkroomdb/pgtest/pgtest.go       ×4, all "the Rust harness in …"
                        + CHANGELOG / HANDOFF / REPORT only
find_stale_pending      README.md:971                              "the README used to say otherwise: it named … and called it 'tested'"
                        + REPORT / HANDOFF only
claimUnpublished        internal/service/outbox.go:33, :35         the correction itself, quoting the wrong sentence it replaced
                        + HANDOFF only
bin/dev down            HANDOFF-darkroom-go-10.md only
sqlx-cli                HANDOFF-darkroom-go-10.md only
StorageKey::generate    none
```

**Every live hit is now past tense.** Not one claims something exists.

A second sweep, stronger than the packet's: every backticked path-shaped token
in `*.go`/`*.md`/`*.sql`/`*.yml`/`*.sh` tested with `[ -e ]`. 50 distinct
missing paths, each one checked by hand:

- **`src/*` and `tests/*`** — all correctly framed as history ("the Go spelling
  of `src/config.rs`", "the Rust harness in `tests/schema_isolation.rs`").
- **`templates/*`** — kit's tree, which is a different repository. Correct.
- **`internal/httpapi.New`, `internal/problem.Problem`, `internal/config`,
  `internal/equivalence`, `migrations/0004`** — Go **package paths and
  identifiers**, not directories. `internal/config` and `internal/equivalence`
  are named as things that do *not* exist; the rest are `pkg.Symbol` forms the
  `[ -e ]` test cannot see past the dot. No finding.
- **`bin/dev`, `bin/not-the-gate`, `bin/prime-go`** — all three already handled:
  `bin/not-the-gate` and `bin/prime-go` are planted divergences in guard tests,
  and `bin/dev` is called out at `README.md:314` and `docker-compose.yml:10` as
  absent by name.

## What this packet fixed

### 1. `migrations/0004_account_isolation.sql` — three present-tense pointers to a deleted script

Same defect as the packet's item 2, same file, three lines from its fix. The
replacement is what is true, not a name swap, because **the old sentence made
two different claims and only one of them is still made by anything**:

- `TestEveryHelperCallInAPolicyIsWrapped` (`internal/darkroomdb/pgtest`) does
  run on every database tier and does hold the wrapped form — by deparsing live
  policies with `pg_get_expr`. What it holds is the **spelling**. Nothing in this
  tree counts calls.
- `tests/tenancy_test.sh` compared the deparsed audit across **both roles**,
  because `pg_get_expr` drops the schema from any name the reader's `search_path`
  resolves. `credential_tables()` now reads `pg_depend` OIDs, so there is no
  spelling for a `search_path` to change. That is a real weakening of the
  per-run check and the price of deleting the script — recorded as a loss, not
  rounded off.

On item 2's own question — what should the reader actually do instead of
`bin/dev down -v` — note that kit's `templates/bin/dev.sh` spells the
volume-deleting command **`nuke`**, not `down -v`; `down` keeps volumes and
takes no `-v`. So the old text named a flag combination that does not exist even
in the script it was borrowed from. The current text names the three routes that
re-apply a set, which is what actually re-runs this file.

### 2 & 3. `ci.yml` and `bin/tier-counts` — two hand-kept copies of a number, both stale

`ci.yml:253` said `15 harness + 29 service + 19 httpapi`. `bin/tier-counts`'s
header table said `21` httpapi. The constants they describe are `39` and `34`.

This is why the packet calls item 5 "worse than the rest: not a comment but a
file a machine reads" — and the drift had simply moved to the numbers inside
it. Three later packets each raised a floor in the same commit that added a
test, which is the rule these files exist to enforce; none returned to the
copies. `AGENTS.md` on `http::OPERATIONS`: "a hand-maintained second inventory
and its own README admitted the cost." A restated number is a second inventory
whether or not it is in a table.

**No floor moved.** All four constants and all ten `bin/prime` floors are
untouched. Verified against a real `--db` run, below.

## Verification

The gate was run for real, not inferred. This needed a database first:

- The cluster on `15500` had `KIT_POSTGRES_DATABASES=identity` — darkroom's
  database and **neither of its two roles** existed, so
  `TestTheLoginRoleCanDoTheWork` failed to connect.
- Provisioned exactly as kit's `templates/compose/postgres/initdb/10-cluster.sh`
  and `.github/workflows/ci.yml` do: `darkroom` (owner) and `darkroom_app`
  (login) as separate `CREATE ROLE` calls, `GRANT darkroom_app TO darkroom`, the
  database, the `REVOKE ALL … FROM PUBLIC` boundary, and the per-role timeouts.
- Applied `migrations/0001`–`0004` to `public` as the deploy step does, since
  that test writes to the service's own schema. Confirmed
  `relrowsecurity`/`relforcerowsecurity` both true on `assets` and
  `asset_variants`, false on the two that carry no `account_id`.

```
$ ./bin/prime --db                      → ==> ok
$ grep -c -- '--- SKIP:' prime-db2.log  → 0
$ grep -c -- '--- FAIL'  prime-db2.log  → 0

$ ./bin/tier-counts prime-db2.log
  ok  default  variant-pipeline checks               24
  ok  database  harness and role checks              15
  ok  database  service checks                       39
  ok  database  httpapi checks                       34
  ok  identity  ./internal/darkroomdb/pgtest         15 enumerated, 15 passed with a database
  ok  identity  ./internal/service                   39 enumerated, 39 passed with a database
  ok  identity  ./internal/httpapi                   34 enumerated, 34 passed with a database
  ==> tier-counts ok        exit 0

$ ./bin/gate-self-test   → PASS, all 23 breakages red, 5 warnings green, nothing skipped
```

The 34/34 identity is the one worth noting: it is an **equality**, not a floor,
so it fails in both directions. The corrected comment now says `39` and `34`,
and the run says `39` and `34`.

**Nothing executable moved.** All four migration files hash identically with
`--` lines stripped, before and after. Every added line in `0004` starts with
`--`. The `ci.yml` diff is comments only; the workflow re-parses as YAML, `jobs`
is still `[ci, gate, contract]`, and the step's `run` string is byte-identical.

**On `ci.yml` specifically, since a worker cannot observe CI running:** the file
runs in `gate.yml`'s `gate` job. Nothing functional changed — the edited step is
a YAML comment above an unchanged `run: ./bin/tier-counts "$RUNNER_TEMP/prime.log"`.
The comment now also names the four constants it mirrors, so the next reader who
raises a floor knows there is a copy here. That is the property, not the digits.

## Found, not fixed — left for a packet with a database budget

Neither is drift; both are absences.

1. **No behavioural count of the `(select …)` wrapping.** The `1 call` against
   `5 calls` figure is a recorded measurement of this kit's Postgres, and
   nothing re-derives it. `TestEveryHelperCallInAPolicyIsWrapped` holds the
   spelling, which catches a bare call but not a regression in Postgres's
   planner. Closing it needs a probe function and a five-row fixture — a real
   test, not a comment fix, and it belongs to whoever owns the harness.
2. **No cross-role comparison of `credential_tables()`.** The mechanism is now
   `search_path`-independent by construction, so this is a much weaker gap than
   it was, but it is still an absent check rather than a passing one.

## Also stale, deliberately untouched

- **`README.md` tier table.** The packet and `HANDOFF-darkroom-go-10.md` both say
  leave it. Confirmed it does not restate the counts at all — the one stale
  figure it used to carry was fixed in `go-10`.
- **Historical `cargo`/`sqlx` prose** in `bin/prime`, `bin/tier-counts`,
  `bin/gate-self-test`, `docker/Dockerfile`, `internal/objectstore/s3.go`. All
  past tense, all explaining why a Go mechanism replaced the Rust one, which is
  what `AGENTS.md` requires of every such sentence.
