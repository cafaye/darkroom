# core-10 — darkroom declares its gate, and names the flag that made two of them

## The finding

`darkroom` had **two gates and nothing that said so**. `mise run prime` and
`./bin/prime` are the fast loop; `.github/workflows/ci.yml:164` runs
`./bin/prime --db`; and the difference between them is the entire database
tier. darkroom is a service whose whole job is putting bytes in Postgres, its
41 database tests are `#[ignore]`d, and a bare `./bin/prime` reports them as
`ignored`, prints `==> ok` and exits **0**. core's conformance table already
recorded this as "**NO, and it already drifted**", ranked third of fifteen for
dispatch. This packet writes it down: `gate.yml` declares
`[bin/prime, --db]`, four of its five proofs are the step headers that can only
appear if all four cargo tiers ran, and `bin/gate-self-test` proves the
declaration is load-bearing against 23 breakages with two controls in front.

**Three things I found that are not about darkroom**, and that are the reason
this report is longer than a declaration usually needs. They are in their own
sections below:

1. **The obvious proof pattern is a dead floor in any crate with doc-tests.**
   Measured on a real `bin/prime --db` log: 36 `test result:` lines, and the
   last is always `Doc-tests darkroom` reporting `0 passed`, because the crate
   is `publish = false` and cargo runs the doc-test block last in all four
   tiers. The checker reads the *last* match, so a floor written as the obvious
   `([0-9]+)` reads **0 forever** and can never be satisfied. The declaration
   uses `([1-9][0-9]*)`, which is the correction and not a weakening.
2. **`gate.proof-invalid` is a proving-phase finding, so a declaration can
   promise a floor it cannot read and be green.** I wrote that breakage, ran
   it, and it stayed green. It is now a `--prove` case with the reason recorded
   in the script.
3. **core's restricted YAML reader accepts documents PyYAML refuses.** A plain
   scalar containing `": "` — which is the ordinary way to write
   `unmet: … "cargo: command not found"` — is read by
   `harness/cafaye_contract.read_yaml` and rejected by every other YAML parser
   in the fleet. Minimal reproducer in the report. My declaration quotes that
   string; the divergence is core's to fix.

## The baseline, measured before anything was written

`./bin/prime`, cold build, 2026-09-30, on an M-series Mac, rustc 1.95.0:

```
==> cargo fmt --check
==> cargo build                      Finished in 2m 06s
==> cargo clippy -D warnings         Finished in 2m 06s
==> cargo test (no database)         77 lib passed, 41 ignored across binaries
==> cargo test --features s3         89 lib passed, 9 backend-table, 9 openapi
==> ok
GATE_EXIT=0
```

`./bin/prime --db`, warm, against a Postgres started for this packet:

```
==> cargo test (no database)                     77 lib passed; 41 ignored
==> cargo test --features s3 (no database)       89 lib passed; 9 + 9
==> cargo test --ignored (against TEST_DATABASE_URL)
      api 10, checksum 7, idempotency 5, outbox 5, signed_upload 7, tenant_isolation 7  = 41 passed, 0 left ignored
==> cargo test --features s3 --ignored (against TEST_DATABASE_URL)
      api 10, checksum 7, idempotency 5, outbox 5, signed_upload 7, tenant_isolation 7  = 41 passed, 0 left ignored
==> ok
GATE_EXIT=0
```

Both are the numbers `bin/tier-counts` asserts (77 / 89 / 9 / 41 / 41) and both
match the identity it checks — the count the default run skips equals the count
the database run passes, because they are the same set of tests. **Nothing was
skipped to produce either green**: the 41 `ignored` in the first run are the
41 the second run passed, which is the whole point of the identity.

Then, with the declaration in place, on the worktree itself:

```
$ ../core/harness/bin/gate-check --prove .
OK …/darkroom-worker-core-10-darkroom: 0 failure(s), 3 warning(s) — warnings do not move the exit code
GATE_EXIT=0
```

The three warnings are `gate.requirement-unproven`, one per requirement, and
they are the correct answer: all three are satisfied by a command on `PATH`,
which the checker deliberately does not run. They are counted here rather than
folded into "passed", because a report that says "OK" over three unrunnable
claims is the lie this packet exists to remove.

### The same gate, with this packet's change in place

Run last, so the last thing in this report is a green over the work as it now
stands. Exit codes read from `PIPESTATUS[0]`, not from `$?` after a pipe.

```
$ ./bin/prime --db 2>&1 | tee /tmp/prime.log ; echo "PRIME_EXIT=${PIPESTATUS[0]}"
PRIME_EXIT=0

$ ./bin/tier-counts /tmp/prime.log
  ok  tier 4  unit tests                                    77
  ok  tier 4  skipped for want of a database                41
  ok  tier 4  openapi drift checks                           9
  ok  tier 4  openapi drift checks left ignored              0
  ok  tier 5  unit tests (s3 build)                         89
  ok  tier 5  R2/S3 behaviour table rows                     9
  ok  tier 5  openapi drift checks (s3 build)                9
  ok  tier 6  database tests passed (default build)         41
  ok  tier 6  database tests left ignored (default build)    0
  ok  tier 6  database tests passed (s3 build)              41
  ok  tier 6  database tests left ignored (s3 build)         0
==> tier-counts ok
TIER_COUNTS_EXIT=0
```

**11 checks ok, 0 skipped, 0 failed.** The two `left ignored` rows reading 0 are
the ones that matter most here: they are the difference between "the database
tier ran" and "the database tier ran and passed everything it selected". The 41
in "skipped for want of a database" and the 41 in each tier-6 row are the same
tests, which is the identity `bin/tier-counts` exists to hold.

## The declaration, and the four judgement calls in it

`gate.yml`, and every number in it was read out of darkroom's own output.

### 1. `command: [bin/prime, --db]`, and `miseTask: prime`, and they differ

`mise run prime` runs `bin/prime` without the flag. The declared gate is
`bin/prime --db`. The checker cross-checks the task against `entrypoint` and
compares `argv[0]`, so `bin/prime` == `bin/prime` and the task is accepted —
correctly, because it is the same file. The flag difference is stated at length
in the declaration rather than reconciled, and I want to be explicit about why
I did not reconcile it:

- Adding `[tasks.gate] run = "./bin/prime --db"` would be *adding a third
  spelling of "run the gate"* to the one repository whose `gate.yml` exists to
  stop the fleet guessing. cafaye-py's problem, by core's own table, is a task
  named `gate`. Creating one here to fix a packet about too many spellings is
  the wrong trade.
- Making the mise task itself run the database tier means a mise task that
  needs a database, which is the thing mise is not for and which would make
  `mise run prime` unusable on a laptop — the property AGENTS.md cares about.

So the declaration records the difference and the report recommends the manager
rule on it. It is the one residual defect this packet does not close.

### 2. Five proofs: four headers, one count

`cargo test` prints no suite total — one `test result:` line per test binary —
so there is no aggregate line to anchor on. Two consequences, both measured:

- The four header proofs are the `--db` check. Drop `--db` from the declared
  command, or delete the `--db` branch from `bin/prime`, and two of them are
  absent: `gate.proof-missing`, a failure. Nothing textual sees this. The
  `flag-drift` breakage in `bin/gate-self-test` exists only to prove it.
- The count proof is a **decrease-detector for the last non-empty test binary**,
  which in a `--db` run is `tests/tenant_isolation.rs` at 7. Read the comment in
  `gate.yml` before re-deriving it: the identity of that file moves if a test
  binary is added that sorts later, and the floor is a claim somebody has to
  look at — the same contract as `bin/tier-counts`'s constants. It is **not**
  the suite total, and the declaration says so.

Each header proof matches **exactly once** in a real log. Verified
mechanically, not by eye:

```
tier-4-default-unit              matches=  1
tier-5-s3-unit                   matches=  1
tier-6-database-default-build    matches=  1
tier-6-database-s3-build         matches=  1
suite                            matches= 19  last_captured=7  floor=7
```

### 3. `selfContained: false` with three requirements, and what is *not* one

The corollary the packet warns about: `selfContained: true` is a claim that the
gate needs nothing but the repository, and this gate needs a toolchain, a
crates.io cache and a database. So it is `false`, and the three requirements are
each demonstrable:

| kind | what | satisfied by | what I actually ran |
| --- | --- | --- | --- |
| toolchain | rustc 1.95 + cargo/rustfmt/clippy | `mise install` | **ran `env -i PATH=/nonexistent ./bin/prime` → exit 127, `cargo: command not found` at `cargo fmt --check`** |
| network | crates.io, once, on a cold checkout | `cargo fetch` | **not reproduced — see below** |
| database | Postgres 17 at `$TEST_DATABASE_URL`, and it may be **empty** | `docker compose up -d postgres` | **ran; the 41 tests passed** |

The database requirement says "and it may be empty", and that is not a hope. It
is `tests/common/mod.rs:63`: `test_store()` calls
`sqlx::migrate!("./migrations")` and runs it against the pool it just opened,
with the migration files embedded at compile time. **darkroom is not identity.**
A green database tier here is a green database tier against a migrated schema,
and that is now a sentence in the declaration rather than something you have to
know about the harness.

Named in the declaration so nobody goes looking for it: **no object-storage
bucket and no credential of any kind.** The `s3` tier presigns with the real AWS
SDK and static dummy credentials, which builds a signature locally and
transmits nothing. darkroom's own CI takes no `secrets:` at all, and this
declaration introduces none — no connection string, no token, no key appears in
any file this packet added.

### 4. `timeoutSeconds: 3600`, generous on purpose

The cold `--db` run is minutes: two full compiles (2m06s each on this machine)
and, for the `s3` feature, the AWS SDK. A budget tight enough to be a bug
detector is tight enough to be a flake generator on a first run. This number
exists to stop a hung gate. The `timeout` breakage proves it fires.

## The red proof

`bin/gate-self-test`. Two controls first, because sixteen reds against a
repository that was already red prove nothing:

- **the static control** — `gate-check` on an unmodified copy: **0 failures,
  3 warnings, exit 0**.
- **the proving control** — `gate-check --prove` on an unmodified copy, which
  runs the real `bin/prime --db` and requires every declared proof at or above
  its floor. This is the only evidence that the proofs match what darkroom's
  gate actually prints.

Then, each breaking exactly one thing, in a fresh throwaway copy, asserting
**exit 1 and the named finding** — "something went red" is a much weaker claim
than "the check written for this defect is still load-bearing", and the second
is the one that decays silently.

```
$ TEST_DATABASE_URL=… ./bin/gate-self-test
PASS gate-self-test: the static control — the declaration is true of this tree
PASS gate-self-test: the three unrunnable requirements warn and still exit 0
PASS gate-self-test: the proving control — `bin/prime --db` ran and every
       declared proof appeared at or above its floor
PASS gate-self-test: a repository that declares no gate at all — caught by `gate.declaration-missing`
PASS gate-self-test: a gate command naming a file this repository does not have — caught by `gate.command-missing`
PASS gate-self-test: a gate entrypoint this repository does not have, while the command still does — caught by `gate.entrypoint-missing`
PASS gate-self-test: a gate nobody is allowed to execute — caught by `gate.entrypoint-not-executable`
PASS gate-self-test: a mise task that is not in the mise config — caught by `gate.task-missing`
PASS gate-self-test: a mise task that resolves to a different file than the declaration names — caught by `gate.task-unresolvable`
PASS gate-self-test: a mise task named in a repository that has no mise config — caught by `gate.task-config-missing`
PASS gate-self-test: a declaration naming a CI workflow that is not in this repository — caught by `gate.ci-missing`
PASS gate-self-test: a CI workflow that exists but never runs the gate — caught by `gate.ci-disagrees`
PASS gate-self-test: a gate that calls itself self-contained while naming three things it needs — caught by `gate.schema`
PASS gate-self-test: a requirement satisfied by a file this repository does not have — caught by `gate.requirement-path-missing`
PASS gate-self-test: a proof whose pattern does not compile, which would otherwise read as "no proof required" — caught by `gate.schema`
PASS gate-self-test: a gate command that is a bare name this machine does not have — said `gate.command-unknown` and still exited 0
PASS gate-self-test: a repository with mise tasks and a declaration that names none of them — said `gate.task-undeclared` and still exited 0
PASS gate-self-test: a declaration that says nothing about CI — said `gate.ci-undeclared` and still exited 0
PASS gate-self-test: a mise task whose run string is a pipeline, so only a shell could say what it runs — said `gate.task-unreadable` and still exited 0
PASS gate-self-test: a declaration that is entirely true about a gate that exited 0 without running anything — caught by `gate.proof-missing`
PASS gate-self-test: a gate that ran, printed its proofs, and still failed — caught by `gate.nonzero`
PASS gate-self-test: a gate that outlived the budget its own declaration gave it — caught by `gate.timeout`
PASS gate-self-test: a floor with no capture group, against a gate that ran and printed its proof — caught by `gate.proof-invalid`
PASS gate-self-test: a gate that proved fewer tests than the declaration promised — caught by `gate.floor`
PASS gate-self-test: a proof that matches nothing the real gate prints — caught by `gate.proof-missing`
PASS gate-self-test: a declaration that dropped the --db its own CI job still passes — caught by `gate.proof-missing`

PASS: gate-self-test — all 23 breakages went red, all 5 warning cases stayed
      green with exit 0, both controls are green, and nothing was skipped.
```

`edit` **fails loudly** when a recipe no longer applies. A self-test that
silently stops breaking anything is worse than no self-test, and a stale recipe
is a proof that has quietly stopped proving anything.

### The self-test found two bugs in itself, and both are worth the run

The first full run of `bin/gate-self-test` was **red: 10 of 23 failures**. After
the first fix it was **still red, 3 of 23** — which is how the second half of
this bug surfaced. Recorded here because a self-test's first run being red is the
whole reason to have written one.

**1. Eight of the cases were not proving anything, and one of them was lying
about it.** `expect_red_prove` delegated to `expect_red`, and `expect_red` ran
the checker *without* `--prove` — so the five cases behind that wrapper (the
false green, `gate.nonzero`, `gate.timeout`, `gate.floor`, the `--db` drift) ran
the **static** phase. Three more cases called `expect_red` directly for the same
reason, and those three are exactly the ones that stayed red on the second run.
All eight printed a perfectly honest-looking report about the wrong phase and
exited 0. That is the dangerous shape: not "this case is broken" but "this case
is confidently reporting on something it never looked at".

The cause is a property of the format worth writing down, and it is now a
comment above those cases: **`gate.proof-missing`, `gate.nonzero`,
`gate.timeout`, `gate.floor` and `gate.proof-invalid` do not exist in the static
phase at all.** A case written against one of them and run without `--prove`
cannot fail. Fixed by making `--prove` a flag on `expect_red` itself rather than
a second function that forwards it — the same bug class is why core's
`gate-check` never adds `|| true`. The three stand-in cases call
`expect_red --prove` directly rather than through the database-gated wrapper,
because skipping a case that *could* have run is the same sin as calling a
skipped case a pass.

**2. Two `edit` recipes had gone stale**, and the loud failure is what caught
them instead of a silent pass. `ci-undeclared` deletes the whole `ci:` block by
matching three contiguous lines, and I had put a three-line comment between
`workflow:` and `invokes:` while writing the declaration. `proof-unmeasurable`
still matched the *previous* version of the count pattern. I moved the comment
below the block — with a note in `gate.yml` saying those three lines are kept
contiguous **on purpose** so the recipe cannot quietly retire — and updated the
second recipe. Before re-running I checked all twelve recipes against the tree,
rather than discovering the next stale one at two minutes a case.

### The four the packet names

| packet | breakage | caught by |
| --- | --- | --- |
| `entrypoint` names a file that does not exist | `entrypoint: bin/prime` → `bin/not-the-gate` | `gate.entrypoint-missing` |
| `miseTask` resolves to a *different* file | `[tasks.prime].run` → `./bin/only-lints` | `gate.task-unresolvable` |
| the `proof` regex matches nothing the gate prints | `0 ignored;` → `999999 ignored;`, **real gate run** | `gate.proof-missing` |
| `ci.workflow` is not in the repository | → `.github/workflows/gate.yml` | `gate.ci-missing` |

…plus `gate.ci-disagrees` from a workflow that exists and no longer runs the
gate, the false green (a declaration entirely true about a gate that exits 0
having done nothing), `gate.floor`, `gate.nonzero`, `gate.timeout`,
`gate.proof-invalid`, and the `--db` drift.

### The one that is not string matching

Breakage: replace `bin/prime` with a three-line script that runs nothing,
prints nothing and exits 0. Every string in the declaration about that copy is
still true — the command exists, the entrypoint is executable, the mise task
resolves to it, CI calls it — and the repository is ungated. **The only thing
that catches it is asking the gate to say what it did.** That is `gate.proof`,
and a checker that only compared strings would have passed it along with every
other breakage in this file.

### Warnings, and the promise they make

Four breakages produce a `warn`, and `expect_warn` asserts the exit code is
still **0** in every one. Promote a warning to a failure and the checker is red
on a laptop and green on CI — the same defect in a new place. Plus one asserted
property on the control itself: the three `gate.requirement-unproven` warnings
that every run of this repository prints, and the run still exits 0.

### A skip is not a pass

Without `$TEST_DATABASE_URL` the **three** cases that run darkroom's real gate
are SKIPPED, printed, counted separately, and the script **exits 1**. kit's rule,
applied verbatim: a proof nobody ran is not a proof, and calling a half-run
self-test green would be the false green it exists to catch. The three stand-in
cases do **not** skip — they need the gate to have run, not a database, and
skipping a case that could have run is the same sin. **On this machine there
were 0 skips.**

## Three things about core's checker, not about darkroom

### The obvious count pattern is a dead floor in any crate with doc-tests

Measured on the real `--db` log:

```
zero allowed   ([0-9]+)       36 matches, last = 0   ("Doc-tests darkroom")
zero excluded  ([1-9][0-9]*)  19 matches, last = 7   ("tests/tenant_isolation.rs")
```

The checker reads the **last** match (core's own `docs/gate.md` says so), and
cargo always runs the doc-test block last. For any crate with no doctests, a
floor written the obvious way is 0 forever. That is not a darkroom quirk; it is
true of `pantry` and of every other Rust service in the fleet, and the format
has no way to say "the last *meaningful* line" other than by the author knowing
it. Two options for the manager, and I am not choosing between them here:

- **(a)** document it, the way `gate.yml` documents it, and let every Rust
  repository find it. Cheap, and it fails loudly (`gate.floor`) so nobody gets a
  silent wrong answer.
- **(b)** let a proof's `match` opt out of "last match wins". That is a change
  to `gate_check.prove()` and to the schema, and it is core's to make.

**Cost of flipping to (b):** one optional field, one branch in `prove()`, one
breakage in `gate_self_test.sh`. **Cost of (a):** nothing but the paragraph that
is already in `gate.yml`.

### A floor with no capture group is green until somebody runs the gate

`gate.proof-invalid` for "a floor and no single group to read it from" is
raised in `prove()`, not in `validate()`. I wrote that breakage expecting the
static phase to catch it, ran it, and **it stayed green** — the declaration was
well formed, the pattern compiled, the minimum was an integer, and none of that
is false. The case is now `--prove`-only in the script, with the reason
recorded next to it. core's own self-test has the same case (its breakage 22)
and does assert it, but with `--prove`, which is consistent; the thing worth
saying is that the *static* half of the format has no opinion about a floor it
cannot read, and a reader of `gate.schema` might assume otherwise.

### core's YAML reader is more permissive than PyYAML

Minimal reproducer, run in this packet:

```yaml
      satisfy:
        command: [docker, ps]
        unmet: it says "cargo: command not found"
```

```
core's harness/bin/gate-check   →  read fine, went on to the checks (2 failures, 2 warnings)
PyYAML safe_load                →  ScannerError: mapping values are not allowed here
```

A plain YAML scalar may not contain `": "`. core's `read_yaml` is a hand-rolled
restricted reader (`harness/cafaye_contract.py:868`) and it accepts it. So a
`gate.yml` can be **true** for `gate.declaration-unreadable` and unreadable by
every other YAML tool in the fleet — a linter, a YAML-aware editor, the next
thing that wants to read it. `gate.declaration-unreadable` is a failure that
does not fire.

**This is core's to fix and I did not touch core.** The one-line fix on my side
was to quote the string, which is why `gate.yml` has an odd-looking quoted
`unmet:` with a comment saying why. The fix on core's side is a decision about
whether the restricted reader is meant to be a *subset* of YAML or a different
dialect, and that is the manager's.

## What I did not change, and why

- **`bin/prime`.** Not one byte. Adding a summary line to the gate so a proof
  could read a suite total would have been the tidier declaration, and it is
  still a change to the gate made by a packet whose job is to *declare* the
  gate. The brief says do not modify the gate's behaviour to make it pass, and
  a line added only to satisfy a proof is that. The cost is stated in
  `gate.yml`: the count proof is a decrease-detector for one test binary rather
  than a suite total.
- **`mise.toml`.** See judgement call 1. Adding a task would have been a third
  spelling.
- **`.github/workflows/ci.yml`.** No new job, no new step. Wiring
  `bin/gate-self-test` into CI needs a checkout of `cafaye/core`, which is a
  cross-repository dependency darkroom's pipeline does not have outside the
  `contract` job, and it is a change to CI's shape that this packet was not
  asked for. **So the red proof is not yet run by CI** — it is run by me, and
  its output is below. That is a real gap, and it is the first thing I would
  fix next.
- **`bin/tier-counts`.** Its four constants are the suite-wide counts and are
  correct. `gate.yml` does not duplicate them and must not: one file counting
  the suite is the whole discipline.
- **`README.md`.** The `### Reading the tiers` section is right and I left it.
  `AGENTS.md` gained one line, because that is the file the rules live in and a
  script nobody can find is a proof nobody runs.

## What I could not verify

Everything here is a claim I did **not** check against a real run.

- **`docker compose up -d postgres`, the declared `satisfy.command` for the
  database, never ran.** An unrelated Postgres already held port 5432 on this
  machine, so the compose file could not bind. I started a container **by name**
  (`darkroom-core10-pg`, `postgres:17-alpine`, published on 55432) and pointed
  `$TEST_DATABASE_URL` at it. The requirement is therefore demonstrated by
  *running a Postgres of the declared image and version*, not by running the
  declared command. The declaration names the documented command because that is
  what this repository's own README, `AGENTS.md` and `tests/common/mod.rs` all
  tell a developer to run, and a requirement satisfied by something the
  repository does not document would be a worse claim. I removed the container
  by name afterwards; nothing else was touched.
- **The cold-checkout crates.io failure was not reproduced.** The `network`
  requirement is asserted from `Cargo.lock`'s ~200 pinned crates and from the
  fact that this machine's `~/.cargo/registry` is already populated. My first
  attempt to demonstrate it (an empty `CARGO_HOME` with `--offline`) made mise
  decide the toolchain was missing and start a rustup install; I killed it,
  removed the temporary cargo home, and verified `cargo 1.95.0` /
  `rustc 1.95.0` and `mise ls rust` were intact. I did not retry, so the
  `unmet` text for that requirement is a *shape* I believe rather than a
  message I have seen. If it is wrong, the failure mode is a confusing cargo
  error rather than a false green, so the cost of being wrong here is low.
- **The toolchain requirement's `satisfy.command` is `mise install`, and I ran
  neither `mise install` nor a run with the toolchain genuinely absent.** The
  *unmet* side I did demonstrate, with `env -i PATH=/nonexistent ./bin/prime`
  (exit 127, `cargo: command not found`); the satisfied side is a machine that
  already has rustc 1.95. A missing-toolchain run would mean uninstalling the
  pin on a machine I do not own.
- **GitHub Actions was not run, and cannot be from here.** `gate.ci-disagrees`
  asks one question of the workflow text — does any `run:` body mention every
  element of the declared argv — and answered yes for `[bin/prime, --db]`. I
  did **not** evaluate Actions: a step can branch on an event, be filtered by a
  path, or sit in a job that does not run on a branch, and none of that is
  visible in the file. So "darkroom's CI passes" is a claim about YAML, not a
  green build. The `gate` job's Postgres service, the `--health-cmd` wait and
  the `cargo` cache behaviour are all unverified by me.
- **`bin/prime --db` on `ubuntu-24.04`.** Every number in this report is from an
  `aarch64-apple-darwin` machine. `postgres:17-alpine` is what CI runs, so the
  engine matches, but the timings (2m06s per compile) do not transfer and the
  `timeoutSeconds: 3600` budget was sized from them plus a lot of headroom.
- **The proving breakages' cost is machine- and path-dependent.** Each runs the
  real gate in a copy at a different absolute path, which is a different cargo
  unit, so `darkroom` itself recompiles even though the ~200 dependencies are
  shared through `CARGO_TARGET_DIR`. That is why the header mentions
  `DARKROOM_GATE_TARGET_DIR`. It is a speed property, not a correctness one, and
  I did not measure a fully cold run of `bin/gate-self-test`.
- **`shellcheck`.** Not installed here, so `bin/gate-self-test` has not been
  linted. `bash -n` passes. The script is `set -uo pipefail` with no `set -e`,
  deliberately: each case's failure has to be counted, not to abort the run.
- **The three `gate.requirement-unproven` warnings are warnings by design and I
  did not try to remove them.** All three requirements are satisfied by a bare
  name on `PATH`. A repository-relative `satisfy.command` is checked for
  existence and produces no warning, and none of darkroom's three requirements
  is satisfied by a file in this repository, so there was nothing to point at.

## What I recommend the manager rule on

1. **The `mise run prime` / `bin/prime --db` split.** The fastest fix that adds
   no spelling is one line in `AGENTS.md` and `README.md` saying which is the
   gate — which is half of what this packet did. The complete fix is a
   `mise.toml` task, which is a *third* spelling, so it needs a decision rather
   than a patch.
2. **Whether `gate.proof` should read the last match.** Today it does, and for a
   Rust crate that is the doc-test line. See "Two things about core's checker".
3. **Whether `read_yaml` should be a subset of YAML or its own dialect.**
   `gate.declaration-unreadable` currently means "core's reader refused it", not
   "it is not YAML", and the remediation text tells a reader to run the checker
   — which will not reproduce their parser's error.

## Files

| file | what |
| --- | --- |
| `gate.yml` | the declaration. Five proofs, three requirements, every number measured. |
| `bin/gate-self-test` | two controls and 23 breakages, each naming the finding it expects. |
| `AGENTS.md` | one line in **Gates**: the declared gate is `--db`, and `mise run prime` is not. |
| `CHANGELOG.md` | the entry, under **Unreleased → Added**. |

Nothing outside this worktree was modified. No merge, no push, no remote.
