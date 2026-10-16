# REPORT — darkroom's substrate copy, finished at kit's twelve functions

2026-10-16. Branch `fix/substrate-drift-repair`, merged to `master` at `ce685dd`
(no push). Migration `migrations/0005_substrate_completion.sql`.

---

## 1. What was wrong, measured

darkroom's `migrations/0004_account_isolation.sql` carries kit's
`templates/database/tenancy/substrate.sql` copied verbatim, and it was copied
while the template held **eight** functions. kit added four more the next day —
`cafaye.current_user_id`, `cafaye.begin_user`,
`cafaye.protect_membership_table`, `cafaye.membership_tables` (kit commit
`537441e`, 2026-10-05 13:04) — and the copy was never compared again. kit's
checker, run against the pre-repair master (`391935b`):

```
$ cd /Users/kaka/Code/any/moon/cafaye/darkroom
$ python3 ../kit/tests/substrate_copy_check.py /Users/kaka/Code/any/moon/cafaye/darkroom
substrate copy check — template: templates/database/tenancy/substrate.sql
  12 functions, sha256 eaab289382564749

== /Users/kaka/Code/any/moon/cafaye/darkroom ==
   adopted: embedded definitions in 1 file(s): migrations/0004_account_isolation.sql
   FAIL  MISSING function: cafaye.begin_user
   FAIL  MISSING function: cafaye.current_user_id
   FAIL  MISSING function: cafaye.membership_tables
   FAIL  MISSING function: cafaye.protect_membership_table
   report: comment-only difference in cafaye.protect_credential_table (migrations/0004_account_isolation.sql:393) — SQL semantics are identical to kit's template

summary: 0 clean, 1 drifting
drift is a FAIL: the repositories above carry a copy of the substrate that is not kit's
EXIT=1
```

The one `report:` line is pre-existing and unchanged by this work: darkroom
rewrote a comment inside `protect_credential_table`; the SQL semantics match.

## 2. What changed

One new migration plus the three places that enumerated the old state:

| file | change |
|---|---|
| `migrations/0005_substrate_completion.sql` | new; 367 lines; sha256 `ed079c2cd1aab11dc497b3ca72ecfcadfa51cd9a68f272e8dfd76d13fd20ea1b` (the file the checker passed and the DB proofs applied); kit's four missing blocks byte-for-byte with their commentary, plus a darkroom header and a commented down |
| `migrations/README.md` | apply list gains `0005`; "reversible" paragraph names it; account-boundary section says what `0005` is and why it carries no calls |
| `README.md` | apply list gains `0005`; "All four migrations" → "All five" with the reason |
| `internal/darkroomdb/pgtest/pgtest.go` | two comments that said "four migrations" now say "five" |

Commits: `49b1657` on the branch; merge commit `ce685dd` on `master`
(`--no-ff`, message `merge fix/substrate-drift-repair: the substrate copy
finishes at kit's twelve functions`). Branch tree and master tree after merge
are the same object: `879cf1933dd2c5090aeab753e65525c9514808be`.

`git show --stat 49b1657`:

```
 README.md                                |   8 +-
 internal/darkroomdb/pgtest/pgtest.go     |   4 +-
 migrations/0005_substrate_completion.sql | 367 +++++++++++++++++++++++++++++++
 migrations/README.md                     |  15 +-
 4 files changed, 389 insertions(+), 5 deletions(-)
```

## 3. Verbatim provenance, and the finding that the pin cannot answer

**darkroom's `kit.ref` pins `d3d56f7` (2026-10-02 13:23), and kit's template
does not exist at that ref.** `templates/database/tenancy/substrate.sql` first
appears in kit at `e8605c9`, 2026-10-02 19:23 — six hours AFTER the pin. The
task's instruction to take the bodies "from kit's template at the ref darkroom
pins" is therefore unsatisfiable;

```
$ git -C ../kit show d3d56f7a2e781186d46cdd8fcf735712940dcc97:templates/database/tenancy/substrate.sql
fatal: path 'templates/database/tenancy/substrate.sql' exists on disk, but not in 'd3d56f7a2e781186d46cdd8fcf735712940dcc97'
EXIT=128
```

with `git ls-tree -r d3d56f7 | grep -i tenancy` empty. The pin names the compose
stack `docker-compose.yml` fetches — that is all it can name.

The bytes were taken from kit's template **as the checker reads it**: the
checker (`../kit/tests/substrate_copy_check.py`) reads `KIT_ROOT`'s working
tree, which at the time of the copy was clean and exactly `origin/master`
`46d08f2`; the template file's sha256 is
`eaab2893825647495ee0db770841ac60fe855791b148423430ca44db1f8334c2`, and it has
been byte-unchanged since `537441e` (`git diff 537441e 46d08f2 --
templates/database/tenancy/substrate.sql` is empty; `sha256` of both the
working-tree file and the `46d08f2` blob agree). The migration's own header
records this instead of a `kit.ref` ref, because the pin cannot.

Timeline, measured from both repositories:

- 2026-10-02 13:23 `d3d56f7` — darkroom's `kit.ref` pin (no template in kit yet)
- 2026-10-02 19:23 `e8605c9` — template appears, 4 functions
- 2026-10-02 23:09 `236f593` — 8 functions
- 2026-10-04 13:49 `67051e4` — still 8; **2026-10-04 21:03 darkroom's `898f0fc` copies it**
- 2026-10-05 13:04 `537441e` — 12 functions, the CALLER scope
- 2026-10-16 this repair

So `0004`'s header sentence "the kit body below is byte-identical to the
template it came from" was true the moment it was written, and the **set**
`0004`+`0005` is what makes it true of the template today.

### Byte-proof of the four blocks

Extracted from both files with the checker's own parser and compared as raw
bytes (signature and body, no normalization):

```
current_user_id:          sig-bytes-equal=True body-bytes-equal=True body-len=792/792
begin_user:               sig-bytes-equal=True body-bytes-equal=True body-len=170/170
protect_membership_table: sig-bytes-equal=True body-bytes-equal=True body-len=4000/4000
membership_tables:        sig-bytes-equal=True body-bytes-equal=True body-len=2105/2105
ALL-FOUR-BYTES-EQUAL
```

The four blocks are in kit's order (`current_user_id`, `begin_user`,
`protect_membership_table`, `membership_tables`). No reorder was needed or
made: the only substrate dependency in the four is
`protect_membership_table` → `protect_table`, which `0004` defines, and
`0005` runs after `0004` by filename. The commentary came with each block; it
was not trimmed or rewritten.

## 4. Acceptance, each measured

### 4.1 checker after — clean

Run at the master path that the acceptance names, after the merge:

```
$ python3 ../kit/tests/substrate_copy_check.py /Users/kaka/Code/any/moon/cafaye/darkroom
substrate copy check — template: templates/database/tenancy/substrate.sql
  12 functions, sha256 eaab289382564749

== /Users/kaka/Code/any/moon/cafaye/darkroom ==
   adopted: embedded definitions in 2 file(s): migrations/0004_account_isolation.sql, migrations/0005_substrate_completion.sql
   report: comment-only difference in cafaye.protect_credential_table (migrations/0004_account_isolation.sql:393) — SQL semantics are identical to kit's template
   PASS  every one of kit's functions is present and its SQL semantics match

summary: 1 clean, 0 drifting
CHECKER_MASTER_EXIT=0
```

### 4.2 control — the checker reds, then greens again

In the worktree, the `begin_user` comment+function block was removed from
`0005` with a byte-offset deletion (offset 6996–7673), the checker was run,
and the file was restored from a pristine copy:

```
   adopted: embedded definitions in 2 file(s): migrations/0004_account_isolation.sql, migrations/0005_substrate_completion.sql
   FAIL  MISSING function: cafaye.begin_user
   report: comment-only difference in cafaye.protect_credential_table ...
summary: 0 clean, 1 drifting
CONTROL_EXIT=1

$ cp /tmp/0005_pristine.sql migrations/0005_substrate_completion.sql
$ shasum -a 256 migrations/0005_substrate_completion.sql /tmp/0005_pristine.sql
ed079c2cd1aab11dc497b3ca72ecfcadfa51cd9a68f272e8dfd76d13fd20ea1b  migrations/0005_substrate_completion.sql
ed079c2cd1aab11dc497b3ca72ecfcadfa51cd9a68f272e8dfd76d13fd20ea1b  /tmp/0005_pristine.sql

$ python3 ../kit/tests/substrate_copy_check.py .
   PASS  every one of kit's functions is present and its SQL semantics match
summary: 1 clean, 0 drifting
RESTORED_EXIT=0
```

Both directions are shown: red names exactly the removed function; restore
returns the file to the same sha256 and the checker to exit 0.

### 4.3 `./bin/prime` — exit 0

Worktree (pre-merge), full run ending `==> ok`:

```
AFTER_PRIME_EXIT=0
```

Master path (post-merge), same:

```
PRIME_MASTER_EXIT=0
```

### 4.4 `./bin/prime --db` — exit 0

Database: disposable `darkroom_substrate_a4dc418b` on the shared cluster
(`127.0.0.1:15500`), dropped at the end of this work (see §7). Worktree run
(pre-merge; the merged tree is the same object, `879cf193…`):

```
==> go test -v ./internal/darkroomdb/... (against TEST_DATABASE_URL)
ok  github.com/cafaye/darkroom/internal/darkroomdb/pgtest     2.386s
==> go test -v ./internal/service/... (against TEST_DATABASE_URL)
ok  github.com/cafaye/darkroom/internal/service               3.958s
==> go test -v ./internal/httpapi/ (against TEST_DATABASE_URL; the Idempotency-Key ledger)
ok  github.com/cafaye/darkroom/internal/httpapi               6.325s
==> ok
PRIME_DB_EXIT=0
SKIP count: 0
```

A post-merge `--db` run at the master path was also made; its result is the
same `==> ok` with `PRIME_DB_MASTER_EXIT=0`.

**Role note, with the measurement that forced it.** The task suggested role
`cafaye` for the test connection; `cafaye` is the cluster superuser
(`rolsuper = t`), and Postgres superusers bypass RLS, which is exactly what
darkroom's three-way-denial tests measure. Measured on a scratch table with
`force row level security` in the disposable database:

```
--- as darkroom (owner, no identity) ---  0
--- as cafaye (superuser, no identity) --- 1
```

So `TEST_DATABASE_URL` ran as the non-superuser `darkroom` in a database owned
by `darkroom` — the same shape CI uses — and `DARKROOM_DATABASE_URL` as
`darkroom_app`. URLs (password elided deliberately):

```
TEST_DATABASE_URL=postgres://darkroom:<pw>@127.0.0.1:15500/darkroom_substrate_a4dc418b?sslmode=disable
DARKROOM_DATABASE_URL=postgres://darkroom_app:<pw>@127.0.0.1:15500/darkroom_substrate_a4dc418b?sslmode=disable
```

No host `psql` exists on this machine; every `psql` in this report ran inside
the cluster container (`docker exec … psql -h 127.0.0.1 -U <role>`), over the
same TCP/SCRAM path the tests use. No password was ever printed or written to
a file.

### 4.5 applied, and the twelve functions are there

```
$ for f in 0001_assets 0002_outbox_events 0003_idempotency_keys 0004_account_isolation 0005_substrate_completion; do
    docker exec -i cafaye-postgres-1 sh -c "PGPASSWORD=\$POSTGRES_PASSWORD psql -w -h 127.0.0.1 -U darkroom -d $DB -v ON_ERROR_STOP=1 -f -" < migrations/$f.sql
  done
APPLY(as darkroom) migrations/0001_assets.sql exit=0 ERRORS=0
APPLY(as darkroom) migrations/0002_outbox_events.sql exit=0 ERRORS=0
APPLY(as darkroom) migrations/0003_idempotency_keys.sql exit=0 ERRORS=0
APPLY(as darkroom) migrations/0004_account_isolation.sql exit=0 ERRORS=0
APPLY(as darkroom) migrations/0005_substrate_completion.sql exit=0 ERRORS=0

$ psql -c "select proname from pg_proc p join pg_namespace n on n.oid=p.pronamespace where n.nspname='cafaye' order by 1"
          proname
---------------------------
 begin_account
 begin_credential
 begin_user
 credential_tables
 current_account_id
 current_credential_digest
 current_user_id
 membership_tables
 protect_credential_table
 protect_membership_table
 protect_table
 unprotected_tables
(12 rows)
```

### 4.6 idempotent — 0004 then 0005, twice

Second pass, same database, `-v ON_ERROR_STOP=1`:

```
REAPPLY(pass2) migrations/0004_account_isolation.sql exit=0 ERRORS=0
REAPPLY(pass2) migrations/0005_substrate_completion.sql exit=0 ERRORS=0
$ psql -tAc "select count(*) from pg_proc p join pg_namespace n on n.oid=p.pronamespace where n.nspname='cafaye'"
12
```

Both files are `create or replace` + `if exists`/`if not exists` throughout;
the second pass converges to the same twelve.

## 5. Extra measurements (beyond acceptance)

**The four functions execute, not merely exist.** On PG 17.11, in the
disposable database, a scratch membership table was run through
`cafaye.protect_membership_table('substrate_smoke.memberships','user_id','darkroom_app')`:

- five policies created: `…_cafaye_select/insert/update/delete` plus
  `…_cafaye_members`;
- `select * from cafaye.membership_tables()` returned exactly
  `substrate_smoke | memberships | user_id` — the by-OID audit sees it;
- as `darkroom_app`: no identity → `0` rows; `cafaye.begin_user(userA)` → the
  one userA row; `insert` under only a caller identity → `ERROR: new row
  violates row-level security policy for table "memberships"` (discovery, not
  authorship); `cafaye.begin_account(accountA)` → the one accountA row.

The scratch schema was dropped afterwards and `membership_tables()` returns 0
rows again.

**Fixture learning, reported rather than smoothed over.** The first app-side
attempt failed with `ERROR: permission denied for schema substrate_smoke`
because the scratch schema had no `USAGE` grant for `darkroom_app`.
`protect_membership_table` grants `usage on schema cafaye` (so the login role
can call `begin_user`) but deliberately not usage on the table's own schema;
in a real deployment that table lives in the service schema and the service's
grant already exists. The fixture was fixed with one explicit
`grant usage on schema substrate_smoke to darkroom_app`, not by changing the
substrate.

**Harness hygiene.** After the full `--db` run, the disposable database
contained only the `cafaye` and `public` schemas — `pgtest` left no `t_*`
schemas behind.

## 6. Fleet constraints honored

1. `0004` was not touched;
   `git diff --stat 391935b 60db1fb -- migrations/0004_account_isolation.sql`
   prints nothing. The repair is a new file.
2. The four blocks are the template's bytes, commentary included (§3 proof).
   No reorder was needed; the one dependency and why filename order satisfies
   it are stated in the file's header.
3. No up/down markers; `0005`'s down is commented SQL, with the same measured
   reason `0004` records (an executable down is applied by `psql -f` right
   after the up half).
4. Migrations were not wired into startup; they are applied by hand / by the
   deploy step / by `pgtest` exactly as before.
5. No credential printed or written: all passwords came from the container's
   own environment through command substitution; URLs in this report elide
   the password.
6. Worktree on a branch, tested, merged to `master`, worktree and branch
   removed; nothing pushed.

## 7. Worktree / branch / merge / cleanup state

- Worktree `/Users/kaka/Code/any/moon/wt/substrate-drift-darkroom`, branch
  `fix/substrate-drift-repair` at `49b1657`; removed and branch deleted after
  the merge.
- `master` is at the merge commit `ce685dd` (plus this report's commit); the
  branch and master trees were identical (`879cf193…`).
- **Not pushed.** `origin/master` is still `391935b`; master is ahead by the
  merge (and the report commit) with nothing sent.
- The shared cluster was read and used read-only except for the disposable
  database `darkroom_substrate_a4dc418b`, which was created for this work,
  recreated once under the `darkroom` owner (see §4.4), and dropped at the
  end. Nothing else was created or dropped. The shared `darkroom` database
  was not modified — **it still carries the 8-function `0004` until a deploy
  applies `0005`, which is by design (migrations are a deploy step).**

## 8. What was NOT done / remains unproven

- The four functions are **installed but not used**: no Go code calls
  `begin_user` or `current_user_id`, and darkroom has no membership table, so
  no committed test exercises them. The smoke test in §5 is evidence from
  this session, not a regression test in the repository. This was a
  deliberate scope boundary — darkroom's tests do not gain a fixture table it
  does not have.
- `kit.ref` was **not** updated. Re-pinning is a decision about which compose
  stack darkroom runs, separate from this repair; see §3 for why the pin
  could not be the source of the bytes.
- `0005` has not been applied to any deployed database; only to the
  disposable one. Deployment state is not in the repository.
- The comment-only difference in `protect_credential_table` remains (by the
  checker's design it is a report, not a failure) and was not "fixed" here.
