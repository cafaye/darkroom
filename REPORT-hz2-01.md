# darkroom HZ-2 — the satisfy argv that named a path only one environment has

## The measurement first

`gate.yml` declares `bin/prime --db` as the gate, so that is what ran, in both
environments, with `TEST_DATABASE_URL` and `DARKROOM_DATABASE_URL` set to
kit's shared cluster on `localhost:15500`:

| environment | runs | skips | failures |
|---|---|---|---|
| worktree `wt/darkroom-hz2` | 143 passed (`--- PASS:` top-level) | 0 skipped | 0 (exit 0) |
| main checkout | 143 passed | 0 skipped | 0 (`GATE_EXIT=0`) |

Identical, and that is the honest reading: **the gate binary reads no sibling.**
`bin/prime` needs two environment variables and a reachable cluster, both
resolved the same way from anywhere. The database harness'
(`internal/darkroomdb/pgtest`) skips are URL-gated, and the one
`DARKROOM_DATABASE_URL` skip (the login-role test) did not fire because both
URLs were set.

The defect is one file over: `gate.yml`'s database requirement proved itself
with

    docker compose --project-directory . \
      -f ../kit/templates/compose/docker-compose.yml -f ./docker-compose.yml up -d --wait

`../kit` is the main checkout's answer and a worktree's nil — BRK-4R2's shape,
which `gate-check --prove` reports loudly (an unmet requirement) rather than
silently. Loud is better than silent and it is still a defect: a worker gating
in a worktree cannot prove the requirement, and the argv also relied on the
operator having `KIT_COMPOSE_DIR` set in their shell, without which kit's own
compose file resolves its initdb mount against this repository and the cluster
comes up healthy having provisioned nothing.

## The fix

`bin/stack-up` — one resolution order, used wherever kit is needed:

1. `$KIT_COMPOSE_DIR` — the operator's documented spelling. A set value is used
   as given: naming a directory is a person saying "this one".
2. `../kit` beside the repository root — the main checkout's answer.
3. kit beside the **primary worktree**, through `git worktree list --porcelain`.
4. A fetch at the ref `kit.ref` pins, into kit's own cache
   (`${XDG_CACHE_HOME:-$HOME/.cache}/cafaye/kit/<ref>`) — the CI-runner case,
   explicit and last rather than silent, at ONE pinned sha so what comes up is
   what `kit.ref` says this repository runs.

Identification is `templates/compose/docker-compose.yml`, the file the argv
passes to compose twice. The script exports `KIT_COMPOSE_DIR` from the resolved
path, which the old argv left to the operator's shell.

`gate.yml`'s satisfy is now `[bin/stack-up]` — one script the checker can RUN
and see exit, rather than a bare `docker` argv with a worktree-nil path in it.

## The proof, with the daemon faked

The cluster is live, so the script was driven with a fake `docker` earlier on
PATH, from the worktree, `KIT_COMPOSE_DIR` unset:

    bin/stack-up: kit resolved to /Users/kaka/Code/any/moon/cafaye/darkroom/../kit
    bin/stack-up: docker compose up (kit@d3d56f7a2e781186d46cdd8fcf735712940dcc97, …)
    FAKE DOCKER: compose --project-directory /Users/kaka/Code/any/moon/wt/darkroom-hz2
      -f /Users/kaka/Code/any/moon/cafaye/darkroom/../kit/templates/compose/docker-compose.yml
      -f /Users/kaka/Code/any/moon/wt/darkroom-hz2/docker-compose.yml up -d --wait

Candidate 2 missed from the worktree, candidate 3 found kit beside the primary
worktree, and the composed argv is the old command with the resolved path.

## What was not changed

- **Not kit's `templates/bin/dev.sh` dropped in as `bin/dev`** — the right
  destination and the wrong packet; `docker-compose.yml` records that as a
  separate change with its own opinions about `.env` and `db grant`. This script
  does the gate declaration's one job with the worktree-safe resolution, and a
  later `bin/dev` adoption can subsume it.
- **`bin/prime --db`'s `TEST_DATABASE_URL` / `DARKROOM_DATABASE_URL` guards** —
  already environment-independent, measured identical in both runs.
