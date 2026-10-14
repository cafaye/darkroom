# HZ-2 — the sibling-read hazard, measured and fixed fleet-wide

Three worktrees, one packet. `pantry-hz2` was merged and pruned by the manager
mid-run (its main checkout now sits at the fix commit, which is why it is
measured here in the main checkout rather than a worktree); `muse-hz2` and
`darkroom-hz2` carry the unmerged branches. `conductor` was ruled out by grep
and nothing was changed there.

## The measurement table — repo × environment × runs × skips × failures

Every number below is read out of a gate run on this machine on 2026-10-10.
"Worktree" is `wt/<repo>-<name>`, "main" is `/Users/kaka/Code/any/moon/cafaye/<repo>`.
Both runs are at the same commit unless the row says otherwise.

| repo | environment | command | runs | skips | failures | exit |
|---|---|---|---|---|---|---|
| muse | worktree `muse-hz2` (after) | `./bin/prime` | 1078 passed | 4 | 0 | 0 |
| muse | main checkout @ 7e8fe89 (after) | `./bin/prime` | 1078 passed | 4 | 0 | 0 |
| muse | worktree (before, d6c937d) | `./bin/prime` | 1061 passed | 4 | 0 | 0 |
| muse | main checkout (before) | `./bin/prime` | 1061 passed | 4 | 0 | 0 |
| pantry | main checkout @ 6d977df (after merge) | `PANTRY_CAFAYE_ROOT=<workspace> ./bin/prime` | 178 passed | 0 | 0 | 0 |
| pantry | worktree (before, measured by the previous attempt) | same | 177 passed | 1 | 0 | 0 |
| pantry | main checkout (before) | same | 178 passed | 0 | 0 | 0 |
| pantry | worktree (after, from `REPORT-hz2-01.md`'s own run) | same | 179 passed | 0 | 0 | 0 |
| darkroom | worktree `darkroom-hz2` (after) | `./bin/prime --db` | 143 passed | 0 | 0 | 0 |
| darkroom | main checkout (after, previous attempt) | `./bin/prime --db` | 143 passed | 0 | 0 | 0 |
| darkroom | worktree (before) | `./bin/prime --db` | 143 passed | 0 | 0 | 0 |
| conductor | — | `bin/ci` names no sibling at all | — | — | — | — |

pantry's main-checkout "after" count is **178 passed, 0 skipped, 0 failures,
exit 0** — measured in this packet, at `6d977df`, `PANTRY_CAFAYE_ROOT` pointed
at the workspace, with the manifest-lint tier running (12 `OK` lines) and
`TestTheGeneratedTelemetryPackageIsStillGenerated` PASSING. That is the two
rows the packet asks for: the main checkout now runs the test the worktree used
to skip, and no tier reports green having run nothing.

The previous attempt recorded 179 in the worktree and its own commit explains
the difference: *"179 rather than 178 is the co-worker's new resolution-order
test landing in the tree, which is the tree and not the environment."* Confirmed
in the merged commit — `TestTheCafCheckoutResolutionOrder` is in
`cmd/pantry/datastore_declaration_test.go` at `6d977df`, so the resolver's order
is guarded by a real test rather than by a comment.

### A method warning about pantry's main checkout

pantry's main checkout **moved between two of this report's readings**: at
`6d977df` (the merged HZ-2 commit) when the gate ran, and at `7bd63a3` minutes
later, which does not carry the HZ-2 work. Anyone re-measuring pantry from the
main checkout must check `git log --oneline -1` first, or they will read the
pre-fix tree and conclude the fix does not hold. The worktree is pruned, so
there is no second environment left to compare against until a fresh one is
added.

### The four muse skips are the same four in both environments, and that is the point

`MUSE_CORE_SCHEMAS` is an environment **variable**, resolved identically from
any directory. The suite was already environment-independent before this
packet; muse's defect was in the gate *declaration*, not the suite. That is
recorded so the 4-skip row above is not later mistaken for a silent tier this
packet fixed: nothing in muse's suite skipped differently between the two
environments, ever, and nothing here moved them.

## darkroom — two sibling reads, and the second one was a surprise

### 1. `gate.yml`'s database requirement (committed by the previous attempt)

The satisfy argv was

    docker compose --project-directory . \
      -f ../kit/templates/compose/docker-compose.yml -f ./docker-compose.yml up -d --wait

`../kit` is the main checkout's answer and a worktree's nil, so
`gate-check --prove` could not satisfy the requirement from a worktree at all —
loud, via the checker's own `unmet` path, and still the BRK-4R2 shape one file
over. `bin/stack-up` replaces it with HZ-1's order: `KIT_COMPOSE_DIR`,
`../kit` beside the root, kit beside the **primary worktree** through
`git worktree list --porcelain`, and — last, named, and pinned at the ref
`kit.ref` records — a fetch for the CI-runner case. It exports
`KIT_COMPOSE_DIR` from the resolved path, which the old argv left to the
operator's shell and without which kit's compose resolves its initdb mount
against this repository and comes up healthy having provisioned nothing.

**Measured, from this worktree, with `KIT_COMPOSE_DIR` unset:**

    bin/stack-up: kit resolved to /Users/kaka/Code/any/moon/cafaye/darkroom/../kit
    bin/stack-up: docker compose up (kit@d3d56f7a2e781186d46cdd8fcf735712940dcc97, …)

Candidate 2 missed, candidate 3 found kit beside the primary worktree.

### 2. `bin/gate-self-test` — found by running it, not by reading it

The same script that guards the gate declaration answered "where is core" with
`$ROOT/../core` and nothing else. Measured from this worktree before the change,
`CAFAYE_CORE` unset, core present beside the main checkout exactly where
every document says:

    gate-self-test: core's gate checker is not at …/wt/darkroom-hz2/../core/harness/bin/gate-check
    gate-self-test: darkroom declares the gate; it does not implement the check.
    gate-self-test: set CAFAYE_CORE to a cafaye/core checkout.
    EXIT=1

That is not a skip, so `bin/tier-counts` and every floor in `bin/prime` were
blind to it: **the guard on the declaration could not run from a worktree**, in
a fleet where every worker gates in one. The manager's scan did not list it —
it was found by following the brief's own instruction to run each gate in a
worktree.

`core_candidates` now walks `CAFAYE_CORE`, `../core` beside the root, and core
beside the primary worktree through git. Candidate 3 is `..` of the primary
worktree and not the worktree root itself: a worktree is a checkout of THIS
repository, so resolving it would find a directory with no `gate-check` in it
and call that success — a found path's clothes over the same failure. There is
deliberately no fourth candidate: this script is not the gate declaration and
not where a clone belongs, and darkroom's clone fallback is `bin/stack-up`'s at
one pinned ref.

**Measured, after, from this worktree** (same tree, same cluster at
`localhost:15500`, both URLs set):

    PASS: gate-self-test — all 23 breakages went red, all 5 warning cases stayed
          green with exit 0, both controls are green, and nothing was skipped.
    exit: 0

`fresh_copy` was checked before this change rather than after: it resolves
`CHECK` from `$ROOT` once and runs the checker against copies, and each copy is
`git init`'d, so the copy's own resolution is never consulted. The change
cannot reach the cases.

## muse — the clone, and the 100% ceiling that caught the fix

gate.yml's filesystem requirement satisfied itself with

    git clone --depth 1 git@github.com:cafaye/core.git ../core

The suite was already environment-independent (1061 passed, 4 skipped,
identically in both environments, before this packet — the four are the
`MUSE_CORE_SCHEMAS`-gated parity tests and an environment variable). So muse's
copy of BRK-4R2 was loud rather than silent, and wrong twice anyway: it reached
for a network copy of something already on disk, and `../core` from a worktree
is the directory every worker's worktree shares, so two workers proving it
concurrently clone into the same place.

`src/muse/coreloc.py` is the resolution, `bin/fetch-core` is the satisfy argv,
and the order is HZ-1's: `CORE_PATH`/`CORE_SPECS_PATH`, `../core` beside the
root, then core beside the primary worktree through `git worktree
list --porcelain`. The clone remains the explicit LAST fallback for the
CI-runner case, aimed beside the primary worktree so both environments land it
in the same place, and it names itself when it fires.

**The coverage ceiling is the finding worth keeping.** The first gate run over
this branch reported

    src/muse/coreloc.py                    50      1     12      0    98%   73
    FAIL Required test coverage of 100% not reached. Total coverage: 99.96%
    ====================== 1076 passed, 4 skipped in 25.04s ======================

Line 73 is `return Path(__file__).resolve().parents[2]` in `repository_root()`,
whose only caller is `bin/fetch-core` — a script, so no test reached it. Without
the ceiling this is a green gate over a silently wrong `parents[2]`: the script
prints whatever path comes back, so a module moved one directory down resolves
a root that is not the root and the clone fallback aims beside the wrong
checkout. `test_repository_root_is_the_root_of_this_repository` closes it, and
the next run was 1078 passed, 4 skipped, 100.00%.

## pantry — merged, and what its numbers say

Pantry's fix is committed, merged, and its worktree pruned; the main checkout
sits at `6d977df`. Its report records the measured delta and the fix. Two
readers answered "where is cafe" — `bin/prime`'s manifest-lint tier (which
skipped wholesale, and blind to `PANTRY_CAFAYE_ROOT`, the variable its own Go
tier respected) and `cmd/pantry/datastore_declaration_test.go` — and
`bin/cafe-checkout` is now the one reader both call. The delta was
**177 passed / 1 skipped in the worktree against 178 / 0 in the main checkout**,
and the after-number in its worktree was 179 / 0.

`tenancy-check .` in the merged main checkout: **0 failures, 2 warnings**.

## conductor — ruled out, untouched

`bin/ci` runs setup, rubocop, bundler-audit, brakeman, rails test and a seed.
Grepped `bin/ script/ lib/ config/ Gemfile Rakefile` for `../core`, `../cafe`,
`../kit`: no hits. Its citations live in `.md` prose and `cafaye.yml` comments,
which the brief says to leave. Nothing was changed, and nothing was measured a
second time — there is no worktree to measure it in and no live read to make
one for.

## What was deliberately not changed, and why

- **`vendir.lock.yml`'s `cp ../core/…` and `git -C ../core rev-parse HEAD`** —
  a documented manual snapshot procedure, not a gate step, and the lock's own
  header records that a stale pin is legal and that nothing checks the bytes.
  The pantry branch already reworded its instructions to say where core is;
  growing a *check* would be a new dependency for its own sake, which the brief
  forbids and which would make the manual procedure fail for no gain.
- **muse's four `MUSE_CORE_SCHEMAS` skips** — the same four in both
  environments, before and after. They are a variable's absence, not a sibling
  read, and "fixing" them would mean writing a core checkout onto the machine.
- **`kit`, `gateway`, `identity`, `core`** — prose citations only, per the
  brief. No live read was found in either.
- **`bin/stack-up`'s fetch at `kit.ref`** — kept, though it never fires on this
  machine. A CI runner with no sibling checkout is a real machine and the
  fallback is what keeps `bin/prime --db` runnable there. It is last, it names
  itself, and it fetches ONE pinned sha rather than a branch.
- **darkroom's floors and `bin/tier-counts` constants** — unchanged. The
  measurement moved no test between tiers, so there is nothing to raise.
- **`bin/gate-self-test`'s planted divergence for the old satisfy argv** was
  updated, not removed: the head is now `bin/stack-up`, so the breakage still
  names a file this repository does not have. A divergence that stops firing is
  a divergence that was never a guard.

## The gate environment, both halves

| repo | worktree | main checkout |
|---|---|---|
| muse | 1078 passed, 4 skipped, 0 failures, exit 0 | 1078 passed, 4 skipped, 0 failures, exit 0 |
| pantry | 179 passed, 0 skipped, 0 failures, exit 0 (worktree, pre-merge) | 178 passed, 0 skipped, 0 failures, exit 0 (main checkout, post-merge) |
| darkroom | 143 passed, 0 skipped, 0 failures, exit 0 | 143 passed, 0 skipped, 0 failures, exit 0 |
| darkroom `gate-self-test` | 23 red, 5 green, both controls green, **exit 0** | (the OLD script exits 1 from a worktree; measured) |

`tenancy-check .` stays at **0 failures** in muse (2 warnings), darkroom
(2 warnings) and pantry (2 warnings).

## One method finding, recorded because it nearly produced a wrong number

The previous attempt's first pantry worktree run read `163 passed / 15 skipped`
because it ran concurrently with the main-checkout run, and
`tests/rls.sh:172` picks its scratch-cluster port by asking `lsof` who is
listening — a known TOCTOU. The worktree's `--serve` lost the race and its 16
data-path gates "did not run". The concurrency was the measurement tool's
defect, not the repository's. Every number in this report was taken with no
other gate running against the same cluster.
