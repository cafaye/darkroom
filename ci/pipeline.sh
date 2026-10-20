#!/usr/bin/env bash
# ci/pipeline.sh — darkroom's pipeline, on the instabox backend or any other.
#
# A backend's job is only to provide an environment and call this script; the
# gate is darkroom's. Which backend ran is recorded in `ci/backend`, and
# switching is one line — `github-actions` keeps `ci/ci.yml` as
# the runner (kit's `backends/README.md` is the contract).
#
# WHERE EVERY COMMAND COMES FROM: `ci/ci.yml` IN THIS
# REPOSITORY. That file is the source of truth; when it changes, change this
# file with it.
#
#   1. THE GATE is `bin/prime --db`, with the flag — the `gate` job runs
#      `./bin/prime --db`, and `bin/prime` without it is not a gate: every
#      database test calls `pgtest.Require`, which SKIPS with a named reason
#      rather than failing. `gate.yml` declares `[bin/prime, --db]` for the
#      same reason, and its checker verifies both elements appear in a `run:`
#      body. After the gate, `./bin/tier-counts` reads the log, exactly as the
#      workflow's `the tiers actually ran` step does.
#   2. THE TOOLCHAIN is the Go pin `go.mod` declares and `mise.toml` pins
#      (`go 1.26.1`; three files, one number, with the Dockerfile's
#      `ARG GO_VERSION`). CI installs it with `actions/setup-go` and then
#      proves the pin is in force (go.mod vs `go version` vs mise.toml); here
#      `mise install` materializes it, the gate runs under `mise exec`, and
#      the same three-way proof runs below. (`actions/setup-go` in the
#      workflow is the toolchain installer, not a helper the gate calls —
#      read, and replaced here by the `mise install` below.)
#   3. NO PATH ENTRIES: no step in `ci/*.yml` appends to
#      `$GITHUB_PATH`, so `PATHS` below is empty. If a workflow ever gains one,
#      copy it there in the same order.
#
# The database tiers run IN PARALLEL with no serialisation flag — each test
# owns a schema — so unlike the Ruby services this pipeline needs no
# concurrency note for the supervisor.
#
# The `contract` job (cafaye-manifest lint via another repository's CLI) stays
# in the workflow: it checks out a second cafaye repository, which is
# explicitly out of kit's scope and out of this pipeline's.
#
# WHAT THE BACKEND PROVIDES (kit `backends/README.md`): WORKSPACE, GH_TOKEN,
# CI. Anything else this pipeline needs it declares below, and the backend
# materializes it from its own secret store.
set -euo pipefail

# ---------------------------------------------------------------------------
# identity
# ---------------------------------------------------------------------------
# The repository root is derived from this file, not from the caller's cwd. The
# agent resets the checkout to the fetched commit before calling us, and the
# pipeline does the same itself so that a hand-run is identical to a polled one.
REPO_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
cd "$REPO_DIR"
REPO_NAME="$(basename "$REPO_DIR")"

WORKSPACE="${WORKSPACE:-$(cd "$REPO_DIR/../.." && pwd)}"
BRANCH="${CI_BRANCH:-master}"
export CI="${CI:-1}"
export WORKSPACE

log() { printf '\n== %s\n' "$1"; }

log "pipeline: $REPO_NAME at $REPO_DIR"

# The gate CI names, with the flag that makes it a gate. See the header.
GATE=(./bin/prime --db)

# No `echo … >> $GITHUB_PATH` line in `ci/*.yml` — verified, so
# this stays empty rather than carrying a guess.
PATHS=()

if [ "${#PATHS[@]}" -gt 0 ]; then
  for d in "${PATHS[@]}"; do
    export PATH="$d:$PATH"
  done
fi

# ---------------------------------------------------------------------------
# the database
# ---------------------------------------------------------------------------
# The gate reads TWO variables from the environment the backend provides (the
# workflow's `gate` job sets both): `TEST_DATABASE_URL`, which every database
# tier queries through, and `DARKROOM_DATABASE_URL`, which names the LOGIN
# role the RLS policies name (`TestTheLoginRoleCanDoTheWork` is only meaningful
# as that role). The backend's cluster must also carry the `darkroom_app`
# login role — CI creates it with `docker exec` because a bare
# `postgres:17-alpine` service container does not. Until the kit database
# packet wires those names, a gate run that names the missing URL honestly is
# a correct result — so this declares the need and does NOT fake it with a
# default. A default URL here would point the byte-store suite at a database
# nobody provisioned and report whatever it found.
if [ -z "${TEST_DATABASE_URL:-}" ]; then
  echo "pipeline: TEST_DATABASE_URL is not set by the backend — the database tiers will SKIP naming it, and bin/tier-counts will fail the run there"
else
  echo "pipeline: TEST_DATABASE_URL is set by the backend"
fi
if [ -z "${DARKROOM_DATABASE_URL:-}" ]; then
  echo "pipeline: DARKROOM_DATABASE_URL is not set by the backend — the login-role test will SKIP naming it"
else
  echo "pipeline: DARKROOM_DATABASE_URL is set by the backend"
fi

log "the toolchain the repository pins"
# `mise install` materializes the version in mise.toml; `mise exec` runs the
# gate under it. When there is no mise, the gate runs on whatever Go this
# machine ships — and the proof below refuses it unless it is the pin.
if [ -f mise.toml ] && command -v mise >/dev/null 2>&1; then
  mise install
  GATE=(mise exec -- "${GATE[@]}")
elif [ -f mise.toml ]; then
  echo "pipeline: mise.toml is present and mise is NOT on PATH — the gate runs on the machine's own toolchain"
fi

# The workflow's pin proof, in the pipeline: the number `go.mod` declares is
# read out rather than retyped, so raising the pin stays a one-file change.
want="$(sed -n 's/^go //p' go.mod | head -1)"
got="$(go version | awk '{print $3}' | sed 's/^go//')"
if [ -z "$want" ]; then
  echo "pipeline: go.mod has no go directive. The pin is the pin; do not remove it." >&2
  exit 1
fi
if [ "$got" != "$want" ]; then
  echo "pipeline: go is $got but go.mod declares $want. The build is not pinned." >&2
  exit 1
fi
mise_pinned="$(sed -n 's/^go = "\(.*\)"$/\1/p' mise.toml)"
if [ "$mise_pinned" != "$want" ]; then
  echo "pipeline: mise.toml pins $mise_pinned but go.mod declares $want. Three files, one number." >&2
  exit 1
fi
echo "pipeline: pinned go $got"

log "the gate"
# `tee` so the counts are in the log AND in tier-counts; without `pipefail`
# this pipeline would return tee's exit code and a failing gate would read as
# a pass. The command is a list, so nothing here is re-parsed.
gate_log="${RUNNER_TEMP:-/tmp}/prime-pipeline.log"
status=0
"${GATE[@]}" 2>&1 | tee "$gate_log" || status=$?
if [ "$status" -ne 0 ]; then
  echo "pipeline: the gate exited $status — the run is red" >&2
  exit "$status"
fi

log "the tiers actually ran"
# `go test` exits zero for a package with no test files in it, so the exit
# code above is not the evidence — the counts are. Reads the log, exactly as
# the workflow's step does.
./bin/tier-counts "$gate_log"

# ---------------------------------------------------------------------------
# ship
# ---------------------------------------------------------------------------
# No chart in this tree (`chart/Chart.yaml` does not exist) and no
# `bin/publish`: there is nothing to publish, so the gate is the whole run.
# Never rebuild what the gate just tested; when this service gains something
# to ship, that block goes here.
if [ "${CI_DRY_RUN:-0}" = "1" ]; then
  log "gate passed; CI_DRY_RUN=1, not shipping"
  exit 0
fi

# ---------------------------------------------------------------------------
# the last line a log tail reads
# ---------------------------------------------------------------------------
COMMIT="$(git rev-parse --short HEAD 2>/dev/null || echo unknown)"
log "pipeline: passed $REPO_NAME at $COMMIT (nothing published)"
