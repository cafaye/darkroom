#!/usr/bin/env bash
#
# kit template — the local developer loop. Copy to bin/dev in the service repo.
#
#   bin/dev            # compose up --wait, migrate, seed an admin, print URLs
#                      #   postgres/nats/redis/the collector — the CHEAP stack,
#                      #   because the four observability backends cost up to
#                      #   130s of readiness each and nobody asked for them.
#                      #   Telemetry still goes somewhere; there is just no
#                      #   store to read it back from. For the whole stack:
#                      #     KIT_DEV_PROFILES=observability bin/dev up
#   bin/dev stack      # resolve the pinned kit ref and print where it landed
#                      #   (no side effects; reaching the resolution needs no
#                      #    docker, which is what tests/fetch_test.sh drives)
#   bin/dev live       # resolve the pinned kit ref, sync the service-owned
#                      #   live snapshot into `.kit/live/<ref>`, and print where
#                      #   it landed (`resolved: .kit/live/<ref>`). The staged-
#                      #   launcher entrypoint: a launcher resolves, reads the
#                      #   `resolved:` line, and composes from the snapshot,
#                      #   never from the cache. `stack` stays read-only; this
#                      #   is the only command besides `up` that writes the
#                      #   snapshot, and it reuses `resolve_stack` and
#                      #   `sync_live_stack` rather than a copy of either.
#   bin/dev pin <ref>  # DELIBERATELY move to another kit ref, and say what moved
#   bin/dev down       # stop the stack, keep the volumes
#   bin/dev nuke       # stop the stack and DELETE the volumes
#   bin/dev status     # what is running, and its health
#   bin/dev logs [svc] # tail the stack, or one service
#   bin/dev migrate    # run migrations, nothing else
#   bin/dev seed       # seed the local admin, nothing else
#   bin/dev db grant <name>
#                      # add one service to an ALREADY-RUNNING cluster.
#                      #   initdb only runs on a fresh volume, so naming a
#                      #   service in KIT_POSTGRES_DATABASES after the fact
#                      #   creates nothing. This PRINTS the statements to
#                      #   paste into psql; it does not run them.
#   bin/dev --help
#
# WHAT IT PROMISES
#   Exit 0 means the stack is up, healthy, migrated, and seeded. Anything else
#   is nonzero, and the failing step is named. It never leaves a half-started
#   stack behind silently: if a step fails, the message says which one and what
#   to run, and the stack it started is stopped.
#
#   IT REFUSES AN UNINTERPOLATABLE STACK BEFORE IT STARTS ANYTHING. A compose
#   file that needs a variable with no default — `KIT_POSTGRES_DATABASES` is the
#   only one kit ships — is refused in seconds, naming the variable and the line
#   to add to .env. Docker Compose does NOT do this itself: it substitutes an
#   empty string for a bare `${VAR}`, so the postgres container comes up, the
#   init script refuses, the container exits, and the only report arrives when
#   the whole readiness deadline has run out. That was 180s per attempt for one
#   missing line. `preflight_required` reads the required set out of the compose
#   files themselves rather than from a list kept here, so a service that
#   introduces its own required variable gets the same fast refusal.
#
# THE STACK IS FETCHED, NOT COPIED
#   There is no `docker-compose.yml` of kit's in this repository, and there is no
#   `otel-collector.yml`, no `tempo/`, no `loki/`, no `mimir/`, no `grafana/`.
#   `bin/dev` fetches kit at the ref named by `kit.ref` and runs
#   `templates/compose/docker-compose.yml` from that tree BESIDE your own
#   `docker-compose.yml`:
#
#     docker compose --project-directory . \
#       -f <fetched>/templates/compose/docker-compose.yml \
#       -f ./docker-compose.yml up -d --wait
#
#   Your file is the SECOND one, so it is an override: what is in it wins, and
#   everything you did not mention still comes from kit. That is the whole
#   contract, and it is why nine services stopped each carrying its own copy of
#   the same 400 lines.
#
#   FETCHED, THEN SNAPSHOTTED. `up` copies the fetched compose subtree into
#   `.kit/live/<ref>` — a directory this repository owns — and composes from
#   THERE, never from the cache. The cache is shared mutable state: fetches
#   retire generations out of it, pruning reclaims them, and anything else on
#   the machine may sweep it. A live container bind-mounted into any of that
#   stops resolving its config the moment the sweep runs. The snapshot is
#   written once per ref and never rewritten for a fixed ref (a ref is
#   immutable, so identical bytes mean nothing to do), which keeps every mount
#   source stable no matter what the cache does afterwards — including being
#   deleted outright. The snapshot is derived, not committed: it records its
#   ref in `.kit-stack-ref` like every other tree here, and carries its own
#   `.gitignore` so it is never added by accident.
#
#   `kit.ref` MUST BE A PIN. A 40-character commit sha, or a `v<semver>` tag. A
#   branch name is refused, loudly, before any network call — because a moving
#   reference is a gate that changes under you, and by the time a fetch has
#   returned it has already changed. `bin/dev pin <ref>` is how you move,
#   deliberately, and it prints the stack diff between the two refs.
#
#   WHY THE PIN IS ITS OWN FILE AND NOT A LINE IN `.env`
#   `.env` is git-ignored, because it holds a developer's port overrides and
#   whatever else is machine-specific. A pin in a git-ignored file is a pin that
#   exists on exactly one machine, which is the opposite of what a pin is for: it
#   is the thing that makes "this is what we run" a reviewable statement. So the
#   ref lives in `kit.ref` — one line, committed, whose entire content is the
#   pin — and `.env` never carries it. A first `bin/dev` on a fresh clone then
#   works with no `.env` at all, which is the whole point of not requiring one.
#
#   The service's own `docker-compose.yml` is OPTIONAL. A service with nothing
#   to add — one that only needs a database and a collector — needs no compose
#   file at all, and `bin/dev` runs the stack without one.
#
# STRICTNESS NOTES — READ BEFORE EDITING
#   - `docker compose up -d --wait`, never bare `up -d`. `--wait` blocks until
#     every service reports healthy, which is the only way "migrations ran
#     against a database that was not ready" stops being a flake you learn to
#     retry past. It is why every service in the compose template has a
#     healthcheck.
#   - NO SLEEPS ANYWHERE. Every wait is a poll on a health signal with a deadline
#     and a named timeout. A sleep is a guess about someone else's startup time,
#     and it is wrong on the machine where it matters.
#   - `bin/dev` is idempotent. Running it twice changes nothing: compose is
#     declarative, migrations are guarded, and the admin seed is an upsert. The
#     second run is as fast as the first.
#   - `nuke` is the only destructive step and it is the only one that is not the
#     default. It is spelled out rather than aliased, and it asks nothing — a
#     prompt here is a prompt someone will pipe `yes` into.
#   - Nothing is written to git by an ordinary run. `.env` is created from the
#     FETCHED `.env.example` if absent, and is git-ignored by every adopting
#     repo. The ONE file `bin/dev` writes that IS tracked is `kit.ref`, and only
#     `bin/dev pin` writes it - never a bare `bin/dev`. That asymmetry is the
#     point: the pin changes when a person says so, in a commit that says why.
#   - Connection URLs are printed, never written anywhere else. A developer
#     pasting one into a ticket is the developer's choice, not this script's.
#   - The observability profile comes DOWN, and this is the load-bearing
#     decision for the dev loop's speed. It used to come UP, and that made
#     `bin/dev` without arguments start the collector AND Tempo, Loki and
#     Grafana — three more containers than kit-02 shipped, and 130 seconds of
#     Mimir readiness nobody had asked for. A default that taxes every
#     developer's first five minutes to serve a view they did not open is a
#     default that gets switched off globally, which costs the telemetry too.
#     So the cheap stack is the default and the four backends are
#     `KIT_DEV_PROFILES=observability bin/dev up` — deliberate, one line, and
#     it says on the console which of the two you are running.
#   - IT IS STILL ONE COMMAND, and telemetry is still ON. The collector is not
#     behind the profile, so `<SERVICE>_OTEL_ENDPOINT` still has somewhere to
#     send on the default path, and the redaction allowlist still runs before
#     anything is stored. `bin/dev` prints the wall-clock it took, so "the dev
#     loop is slow" is a number rather than a feeling.
#   - NOTHING HERE WAITS ON TELEMETRY. Not `up --wait` (which gates on the
#     collector's own health, and the collector's health does not depend on
#     Tempo, Loki or Mimir), not migrate, not seed. A dev machine that cannot
#     start because an observability store is unhealthy is a dev machine that
#     teaches people to switch telemetry off, which is the opposite of the
#     intent. `tests/no_telemetry_in_readiness.sh` in kit proves the underlying
#     property against a real collector.
#   - A FETCH IS NOT A MIGRATION. `git fetch --depth 1 <url> <ref>` is one
#     command against one remote and one immutable ref; there is no branch
#     tracking, no merge and no rebase, so there is no state to get into and
#     nothing to clean up. The ref is validated BEFORE the fetch, so the failure
#     mode is a message rather than a checkout at the wrong commit.

set -euo pipefail

# Resolved BEFORE the chdir below. `usage` prints a slice of this file, and $0 is
# a relative path from wherever bin/dev was invoked — so a chdir first, then a
# `sed ... "$0"`, prints "No such file or directory" instead of the help. The
# other kit scripts get away without this because their `cd` is inside a
# function; this one runs at the top of the file.
SELF="$(cd "$(dirname "$0")" && pwd)/$(basename "$0")"

cd "$(dirname "$SELF")/.."

# How long to wait for the stack to be healthy, in seconds. A deadline, not a
# sleep: `up --wait` polls health itself, and this is the point at which we stop
# believing it and say so.
#
# 180 rather than kit-02's 120, and the extra 60 is for the observability
# profile: three more containers, each with a real initialisation (Tempo's WAL,
# Loki's schema, Grafana's migrations). The deadline has to cover the DEFAULT
# path when the profile is on, and a deadline that fires on a cold start is a
# deadline that trains people to re-run. It is deliberately NOT lowered along
# with the default: the deadline is what a developer raises on a loaded laptop,
# and it costs nothing to leave generous.
STACK_TIMEOUT="${KIT_DEV_TIMEOUT:-180}"

# The compose profiles to bring up. NOTHING BY DEFAULT, and this line is the
# single most consequential token in the file, so the measurement is here
# rather than in a report nobody opens.
#
# It used to read `${KIT_DEV_PROFILES-observability}`. Every developer's
# `bin/dev` therefore paid, on every cold start, for four backends that were on
# the floor of nobody's mind:
#
#   grafana  20s once and >180s another time, downloading a plugin zip on
#            FIRST BOOT — the slowest thing in the stack, and it is a network
#            call nobody asked for (see GF_INSTALL_PLUGINS_PREINSTALL_DISABLED)
#   mimir    130s of readiness budget: retries 12 x interval 10s
#   tempo    65s of readiness budget: retries 12 x interval 5s, plus a WAL init
#   loki     16s to first ready, measured, because it waits for its ring
#
# Against a 180s deadline and a 76s typical cold start (RESEARCH-fleet-velocity
# P1, measured WITH the profile partially lazy), that is the dev loop paying a
# tax for an observability stack the developer did not ask to look at — and the
# documented way to avoid it, `KIT_DEV_PROFILES= bin/dev up`, was the escape
# hatch. THE HATCH WAS THE GOOD PATH AND THE DEFAULT WAS THE TAX. That is
# backwards, so this is now the highway.
#
# WHAT DOES NOT CHANGE WITH IT, and this is the part that matters:
#
#   * THE COLLECTOR IS NOT BEHIND THE PROFILE. It runs on the cheap path, so a
#     service still has somewhere to send: `<SERVICE>_OTEL_ENDPOINT` defaults to
#     it, and a dead endpoint with no retry costs spans rather than availability.
#   * THE REDACTION ALLOWLIST STILL RUNS. `otel-collector.yml` drops
#     high-cardinality dimensions at INGEST — before anything is stored — so
#     there is no cheap path on which a `request_id` or a `user_id` becomes a
#     metric label. Turning a backend off is not how this boundary is kept.
#
# Observability is one variable, and the other end of it is now the deliberate
# spelling:
#
#     KIT_DEV_PROFILES=observability bin/dev up
#
# NOT READ FROM `.env`, and that is a decision rather than an omission. Every
# other `KIT_*` here goes through `stack_setting`, which cannot distinguish
# "unset" from "set to empty" — so a `.env` reader for this one variable would
# be a second reader of the same fact with different semantics, and a `.env`
# saying `observability` would silently re-impose the tax on every command that
# starts a stack. A developer who wants the four backends on every run puts it
# in their shell, where it is visible, rather than in a file the script cannot
# honour in the direction that matters.
KIT_DEV_PROFILES="${KIT_DEV_PROFILES-}"

# --------------------------------------------------------------------------
# the kit ref
# --------------------------------------------------------------------------
# Everything below is about ONE question: which bytes of kit is this developer
# running? A compose file cannot be `uses:`-ed, so this script is the only
# callable path there is, and the ref is the only thing standing between "always
# current" and "changes under you between two runs of the same command".

# Where kit comes from. Overridable because a self-hoster mirrors it, and
# because the test builds a local `file://` remote and needs the same code path
# rather than a mock of it.
#
# READ LAZILY by `stack_setting`, and not assigned once at the top of the file.
# The first version read these from the process environment only, so a `.env`
# naming a mirror was silently ignored and the fetch went to github anyway — from
# a service that had written down somewhere else. Ignoring a value a developer
# typed is worse than not having read the variable at all.
stack_url() { stack_setting KIT_STACK_URL "https://github.com/cafaye/kit.git"; }

# The cache. Outside the repository on purpose: a cache inside a working tree is
# a copy, and this whole packet is about the copies. One directory per ref, so
# two services on two refs cannot fight over one checkout, and an upgrade is a
# new directory rather than a mutation of the old one.
stack_home() { stack_setting KIT_STACK_HOME "${XDG_CACHE_HOME:-$HOME/.cache}/cafaye/kit"; }

# The vendored copy, for a machine with no network. See `resolve_stack` for why
# it has to DECLARE its ref rather than merely exist.
vendor_dir() { stack_setting KIT_VENDOR_DIR ".kit/stack"; }

# THE PIN'S HOME. One committed file at the service root, one line, whose entire
# content is the ref.
#
# Not `.env`, and not a default baked into this script, and the reason is the
# one above in the header: `.env` is git-ignored, so a pin kept there exists on
# one developer's machine and on no CI runner, and "one command, always current"
# becomes "one command, whatever this laptop last fetched". The name is not a
# variable — it is the contract, and anything that grades the pin reads it by
# this same name-resolution rule rather than by a glob: two readers of "where is
# the pin" that disagree are how a gate passes on a file the tool ignores.
REF_FILE="kit.ref"

# Set by `resolve_stack`, read by everything after it. Empty means "not resolved
# yet", and every consumer treats that as a bug rather than a default.
STACK_DIR=""

step() { printf '\n\033[1m== %s\033[0m\n' "$1"; }
info() { printf '   %s\n' "$1"; }
die() {
  printf '\n\033[1;31mbin/dev: %s\033[0m\n' "$1" >&2
  exit "${2:-1}"
}

# The leading comment block, found rather than counted.
#
# It was `sed -n '2,68p'` — a hardcoded line range — and the header grew past 68
# lines the moment this script learned to fetch kit, so `--help` silently
# truncated the section explaining the pin. A number in a script that has to be
# edited every time the prose above it grows is a number that eventually is not,
# and a help message that stops one paragraph early is a help message nobody
# notices is wrong. So it is derived: everything from line 2 to the first blank
# line, which is where the header ends and the code begins.
usage() { sed -n '2,/^$/p' "$SELF" | sed '$d'; }

# --------------------------------------------------------------------------
# the pin

# stack_ref — the ref this repository runs, or empty if nothing names one.
#
# TWO SOURCES, IN THIS ORDER, AND THE ORDER IS THE ARGUMENT:
#
#   1. KIT_STACK_REF in the process environment. A person overriding for ONE run
#      — `KIT_STACK_REF=$(git -C ../kit rev-parse HEAD) bin/dev up` — while
#      they are working on kit itself. It is deliberately NOT read from `.env`:
#      an override that a file could also set is two sources for one fact, and
#      the file is the one that wins silently.
#   2. `$REF_FILE`, the committed one-liner. This is the pin.
#
# Everything downstream — the cache directory, a vendored copy, a mismatch
# message - names the SAME string, because `resolve_stack` reads it exactly once
# and passes it down. A second reader of the pin would be a second answer to
# "which bytes of kit is this".
stack_ref() {
  local from_env
  from_env="$(eval "printf '%s' \"\${KIT_STACK_REF:-}\"")"
  if [ -n "$from_env" ]; then
    printf '%s' "$from_env"
    return 0
  fi
  [ -f "$REF_FILE" ] || return 0
  # First non-blank, non-comment line, trimmed. Comments are allowed because a
  # one-line file that cannot carry a note cannot carry a reason, and a pin
  # nobody wrote a reason for is a pin nobody will move deliberately.
  #
  # `|| true` and NOT omitted, and this is the second time this line has needed
  # it. A `kit.ref` holding nothing but a blank line — the single most likely
  # first-run mistake, and the one the file's own header warns about — makes
  # `grep -v '^$'` find nothing and exit 1. Under `set -o pipefail` that is the
  # PIPELINE's status, so `ref="$(stack_ref)"` fails, `set -e` aborts the script,
  # and the developer gets a non-zero exit and NO MESSAGE AT ALL. The careful
  # "your pin file does not name one" branch in `validate_ref` — the one written
  # specifically for this case — is never reached, because the script died three
  # lines earlier trying to notice.
  #
  # Silence on the most likely mistake is worse than a wrong message: it is the
  # failure where the reader learns nothing and assumes the tool is broken.
  sed -e 's/[[:space:]]*$//' -e 's/^[[:space:]]*//' "$REF_FILE" 2>/dev/null |
    grep -v '^#' | grep -v '^$' | head -1 || true
  return 0
}

# --------------------------------------------------------------------------
#
# A PINNED REF IS A 40-CHARACTER COMMIT SHA, OR A v<semver> TAG.
#
# Not a branch, and not "whatever master is today". The reason is not
# fastidiousness: this is a GATE. `bin/dev` decides whether a redaction
# allowlist derived from core's schemas is in force, and a service whose gate
# resolves to a different allowlist on Tuesday than on Monday is a service
# whose telemetry boundary is not a property anyone can state. So the two moving
# forms are refused BEFORE the fetch, not corrected after it — a fetch of
# `master` succeeds, it just succeeds at something nobody chose.
#
# A tag is accepted because cutting one is a deliberate act by a person, and a
# tag that has been cut resolves to an immutable commit. An abbreviated sha is
# refused because it is a moving reference wearing a sha costume: `git fetch`
# resolves it happily and it is ambiguous across remotes.
#
# The check is `case`, not a regex, on purpose: this runs before any tooling
# beyond POSIX shell has been located, and a dependency on `grep -E` at this
# point would be a dependency in the one code path that must not have one.
validate_ref() {
  local ref="$1"
  case "$ref" in
    [0-9a-f][0-9a-f][0-9a-f][0-9a-f][0-9a-f][0-9a-f][0-9a-f][0-9a-f]*) ;;
  esac
  # The glob above matches a PREFIX, so a 7-char sha passes it. Length is
  # checked separately and exactly, which is the whole point.
  if [ "${#ref}" -eq 40 ]; then
    case "$ref" in
      *[!0-9a-f]*)
        die "$REF_FILE holds '$ref': 40 characters but not a commit sha (lowercase hex)."
        ;;
    esac
    return 0
  fi
  case "$ref" in
    v[0-9]*.[0-9]*.[0-9]*)
      # Semver's own grammar, including the optional pre-release and build
      # suffixes. `v1.0.0-rc.1` is a tag real projects cut, and a pin rule that
      # refused it would be routed around — a developer told "that is not a pin"
      # writes `master`, which is the failure this whole function exists to
      # prevent. Rejecting the suffix would make the rule worse, not stricter.
      local version="${ref#v}"
      # Drop `-pre` and `+build` before counting the dots, so `v1.0.0-rc.1` does
      # not read as a four-component version.
      local numeric="${version%%+*}"
      numeric="${numeric%%-*}"
      local major minor patch
      case "$numeric" in
        *.*.*)
          major="${numeric%%.*}"
          # `minor` and `patch` are each cut on BOTH delimiters. Cutting only the
          # leading one leaves `minor` as "0.0" for a `v0.0.0` tag, which then
          # fails the digit check below — so `v0.0.0` was refused while `v1.2.3`
          # was accepted, which is the worst possible shape for a validation
          # rule: it looks right on the examples everybody tries first.
          minor="${numeric#*.}"
          minor="${minor%%.*}"
          patch="${minor#*.}"
          patch="${patch%%.*}"
          ;;
        *)
          die "$REF_FILE holds '$ref', which is not a v<MAJOR>.<MINOR>.<PATCH> tag."
          ;;
      esac
      # Each component must be digits and nothing else. A `case` and not a
      # `[[ =~ ]]`: this function is the one code path that has to run before any
      # tooling beyond POSIX shell has been located, and an extended-regex
      # dependency there is a dependency in the one place that must not have one.
      for part in "$major" "$minor" "$patch"; do
        case "$part" in
          '' | *[!0-9]*)
            die "$REF_FILE holds '$ref', which is not a v<MAJOR>.<MINOR>.<PATCH> tag with numeric parts."
            ;;
        esac
      done
      # The suffix, if any, is semver's own: alphanumerics, dots and hyphens.
      local suffix="${version#"$numeric"}"
      case "$suffix" in
        *[!0-9A-Za-z.+-]*)
          die "$REF_FILE holds '$ref', which is not a v<MAJOR>.<MINOR>.<PATCH> tag; a pre-release or
   build suffix may contain only letters, digits, dots and hyphens."
          ;;
      esac
      return 0
      ;;
  esac
  if [ -z "$ref" ]; then
    die "no kit ref, and $REF_FILE does not name one.
   The pin is the ref of kit this repository runs, and it is committed here so that
   'one command, always current' means the same thing on every machine and in CI.
   Write one line into $REF_FILE:
     git -C /path/to/a/kit/checkout rev-parse HEAD > $REF_FILE
   Accepted: a 40-character commit sha (lowercase hex), or a v<MAJOR>.<MINOR>.<PATCH> tag.
   Refused: a branch — 'master' is a MOVING reference, and the stack you get on Tuesday
   would not be the stack you reviewed on Monday.
   To move deliberately once there is one:  bin/dev pin <ref>"
  fi
  die "$REF_FILE holds '$ref', which is not a pin.
   Accepted: a 40-character commit sha (lowercase hex), or a v<MAJOR>.<MINOR>.<PATCH> tag.
   Refused: branch names — 'master', 'main', anything without a ref suffix — because a
   branch is a MOVING reference. \`bin/dev\` would fetch whatever it points at today, and
   the stack you get on Tuesday would not be the stack you reviewed on Monday.
   To move deliberately:  bin/dev pin <ref>"
}

# --------------------------------------------------------------------------
# resolving the stack
# --------------------------------------------------------------------------
#
# FOUR SOURCES, IN THIS ORDER, AND THE ORDER IS THE ARGUMENT.
#
#   1. KIT_STACK_DIR   an explicit directory. Highest precedence because it is
#                      the only one a person typed on this run.
#   2. the cache       <KIT_STACK_HOME>/<ref>, with the ref recorded inside it.
#   3. the live        .kit/live/<ref>, ALSO with the ref recorded inside it:
#      snapshot        the tree a previous `up` rendered from, owned by this
#                      service. Below the cache because a freshly fetched tree
#                      is authoritative; above the vendored copy because it is
#                      the stack this machine actually ran. This is what lets a
#                      stack survive its cache: with the cache deleted outright
#                      and the network forbidden, `up` still resolves — to the
#                      bytes it already runs.
#   4. the vendored    .kit/stack, ALSO with the ref recorded inside it.
#      copy
#   5. a fetch         the only one that touches the network, and the only one
#                      that can fail because of the network.
#
# 2 and 3 both carry a recorded ref, and that is the load-bearing detail. A
# directory that merely CONTAINS `templates/compose/` is not a kit checkout at
# the right version; it is a directory. Accepting one silently is precisely how
# an offline loop stops matching what the team runs, so a mismatched record is
# a refusal and a missing record is a refusal, and only a matching one is used.
#
# OFFLINE IS REAL, NOT A SENTENCE. `KIT_STACK_OFFLINE=1` skips step 4 and fails
# if 1-3 all miss, naming each one. The alternative — falling back to whatever
# is in the working directory — is the one behaviour that cannot be allowed,
# because it makes `bin/dev` report success while running a stack nobody pinned.

# read_stack_ref <dir> — the ref a tree records, or empty if it records none.
# A single line, read with `head -1`, so a multi-line or binary file cannot
# become a ref by accident.
read_stack_ref() {
  [ -f "$1/.kit-stack-ref" ] || return 1
  head -1 "$1/.kit-stack-ref" 2>/dev/null | tr -d ' \t\r\n'
}

# stack_is_usable <dir> <ref> — does this tree look like kit, at that ref?
# Both halves. A tree at the wrong ref is refused, and so is a tree that is
# missing the files `up` mounts, because a fetch that succeeded and produced an
# unusable tree is a `bin/dev up` that dies four containers later on a bind-mount
# error naming a path the developer has never heard of.
#
# The kamal files are checked for the same reason, and the reason they were
# MISSING is kit-23's defect 1: a service pinned at a ref without
# `templates/kamal/` fetched a tree that passed every compose check and was
# still missing the files its adoption steps tell it to copy. The compose
# checks say nothing about the deploy half of the tree.
stack_is_usable() {
  local dir="$1" ref="$2" have
  [ -d "$dir" ] || return 1
  have="$(read_stack_ref "$dir")" || return 1
  [ "$have" = "$ref" ] || return 1
  [ -f "$dir/templates/compose/docker-compose.yml" ] &&
    [ -f "$dir/templates/compose/otel-collector.yml" ] &&
    [ -d "$dir/templates/compose/tempo" ] &&
    [ -d "$dir/templates/compose/loki" ] &&
    [ -d "$dir/templates/compose/grafana/provisioning" ] &&
    [ -f "$dir/templates/kamal/deploy.yml.erb" ] &&
    [ -f "$dir/templates/kamal/kamal-backup.yml.erb" ] &&
    [ -f "$dir/templates/kamal/drill.sh" ]
}

# --------------------------------------------------------------------------
# RETIRED GENERATIONS
# --------------------------------------------------------------------------
# The tree `fetch_stack` replaced, kept. It is kept because the destination is
# not a directory kit owns: compose bind-mounts FILES out of it into running
# containers, and on macOS a bind mount resolves its source when the container is
# CREATED.
#
# So `rm -rf "$dest"` leaves every running container working off descriptors it
# already holds, while every NEW open through the mount path fails ENOENT.
# Measured on this machine, seven containers up for three hours: the collector
# kept serving the whole time and only its healthcheck went red, because the
# healthcheck re-reads the config through the mount on every probe —
#
#   open /etc/otel/otel-collector.yml: no such file or directory
#
# — which then failed `bin/dev up` with `stack did not become healthy`, so the
# recovery path reported the fault the cache had caused.
#
# MOVING IT ASIDE IS NOT THE WHOLE FIX, and this is the half that is easy to get
# wrong: a retired generation is the SAME INODE the containers are mounted to, so
# `mv "$dest" "$retired"` followed by a later `rm -rf "$retired"` breaks the bind
# mount exactly as the `rm -rf "$dest"` did. "Clean it up later" is only true when
# the later cleanup ASKS whether anything is mounted from it — which is what
# `generation_is_mounted` below does, and why it retains on every answer it cannot
# get an answer from.
#
# What it retains is a few hundred KB per fetch: a `--depth 1` checkout of a
# repository that is almost entirely YAML.

# Where retired generations live. A SUBDIRECTORY of the cache home rather than
# siblings of the ref directory, so `ls ~/.cache/cafaye/kit` still reads as "one
# directory per ref" and nothing else has to learn a second naming scheme.
generation_home() { printf '%s/.retired\n' "$(stack_home)"; }

# How many retired generations to keep. Three, and the point of the number is
# that a re-fetch is allowed to run more than once before anything is reclaimed —
# a developer's second `bin/dev up` after a version bump is the common case, and
# pruning the generation out from under the first one is the defect this whole
# section exists to remove. Overridable, because a developer who knows their
# machine has a reason.
generation_keep() {
  local n
  n="$(stack_setting KIT_STACK_KEEP_GENERATIONS 3)"
  # A `.env` line is a developer typing, and a non-number here would make
  # `[ "$n" -gt "$keep" ]` fail the whole fetch rather than fall back. The
  # default is the value; anything that is not a positive integer IS the value.
  case "$n" in
    '' | *[!0-9]* | 0) printf '3' ;;
    *) printf '%s' "$n" ;;
  esac
}

# next_generation <ref> — a path under the retired home that nothing occupies.
# Never reuses a number, so pruning one generation can never make a later fetch
# land on it and inherit a directory that something is mounted from.
next_generation() {
  local ref="$1" home n=0
  home="$(generation_home)"
  mkdir -p "$home"
  while [ -e "$home/$ref.$n" ] || [ -L "$home/$ref.$n" ]; do
    n=$((n + 1))
    [ "$n" -lt 10000 ] || return 1
  done
  printf '%s/%s.%s' "$home" "$ref" "$n"
}

# mount_sources — every bind source the daemon currently has a container using.
# ONE `docker inspect` over every container, and not a `--filter volume=` lookup
# per generation, because compose mounts INDIVIDUAL FILES: measured on this
# machine (docker 29.4.0), `--filter volume=<the file>` matches, and
# `--filter volume=<the directory that holds it>` matches NOTHING — so a filter
# built from the generation directory would have reported every generation as
# unreferenced and deleted the one a container was mounted to.
#
# Nonzero means "cannot tell", and the caller keeps everything. That is also
# what happens when the daemon is down, or `docker` is not installed — and a
# machine that cannot answer also has no containers mounted from these paths, so
# retaining is right rather than merely cautious.
mount_sources() {
  local ids
  command -v docker >/dev/null 2>&1 || return 1
  docker info >/dev/null 2>&1 || return 1
  ids="$(docker ps -aq 2>/dev/null)" || return 1
  [ -n "$ids" ] || return 0
  # Deliberate word splitting: `$ids` is a list of container ids and docker takes
  # them as separate arguments. The alternative — one `docker inspect` per id —
  # is a spawn per container on a developer machine with a dozen running.
  # shellcheck disable=SC2086
  docker inspect --format '{{range .Mounts}}{{println .Source}}{{end}}' $ids 2>/dev/null
}

# generation_is_mounted <dir> <sources> — is anything mounted out of this tree?
# A PREFIX test rather than an equality test, because the mounted path is a file
# inside the generation, not the generation.
generation_is_mounted() {
  local gen="$1" src
  while IFS= read -r src; do
    [ -n "$src" ] || continue
    case "$src" in
      "$gen"/*) return 0 ;;
    esac
  done <<<"$2"
  return 1
}

# prune_generations <ref> — reclaim retired generations beyond `generation_keep`.
#
# Oldest first, and ONLY a generation the daemon reports nothing mounted from.
# The two halves are one rule: reclaiming space is never worth breaking a
# developer's running stack, so the conservative answer wins whenever the
# question has no cheap answer.
prune_generations() {
  local ref="$1" keep
  keep="$(generation_keep)"
  local home
  home="$(generation_home)"
  [ -d "$home" ] || return 0

  local lines="" path serial drop=0 n=0
  for path in "$home/$ref".*; do
    [ -d "$path" ] || continue
    serial="${path##*.}"
    lines="$lines$serial	$path
"
    n=$((n + 1))
  done
  # Nothing to reclaim, and the COMMON case: the cold path never reaches here, so
  # `bin/dev stack` on a fresh cache starts no docker process at all.
  [ "$n" -gt "$keep" ] || return 0
  drop=$((n - keep))

  local sources="" known=1
  sources="$(mount_sources)" || known=0
  if [ "$known" -ne 1 ]; then
    info "keeping $n retired generation(s): docker could not be asked what is mounted"
    return 0
  fi

  local i=0
  while IFS=$'\t' read -r serial path; do
    [ -n "${serial:-}" ] || continue
    if [ "$i" -lt "$drop" ]; then
      if generation_is_mounted "$path" "$sources"; then
        info "keeping retired generation $path: a container is mounted from it"
      else
        rm -rf "$path"
      fi
    fi
    i=$((i + 1))
  done < <(printf '%s' "$lines" | sort -n)
  return 0
}

# --------------------------------------------------------------------------
# THE LIVE SNAPSHOT
# --------------------------------------------------------------------------
# The tree the RUNNING stack was rendered from, owned by the service.
#
# The cache is the wrong directory to compose from, and section 8's
# retired-generation machinery is why this paragraph exists rather than
# contradicting it. Retiring keeps the old tree's INODES — a running
# container keeps serving off descriptors it already holds. What it does not
# keep is the PATH being out of the cache: the cache is shared mutable state
# (per-ref directories, retired generations, mount-aware pruning), and every
# `-f` and every bind source still points into it. Anything that removes that
# directory — a retention sweep, a cleaner, a hand — invalidates the live
# mount while the containers are still up, and the healthcheck (which re-reads
# the config through the mount on every probe) is what reports it:
#
#   open /etc/otel/otel-collector.yml: no such file or directory
#
# So `up` snapshots `$STACK_DIR/templates/compose` into `.kit/live/<ref>`
# and `compose()` runs from the snapshot whenever it is usable for the pinned
# ref. Cache replacement, pruning and outright deletion then move no byte any
# live mount resolves to.
#
# THREE PROPERTIES, each load-bearing:
#
#   - CONTENT-ADDRESSED. The snapshot records the ref in `.kit-stack-ref` and
#     is byte-compared against the resolved tree before any write. A snapshot
#     at a different ref — or a corrupt one — is never composed from, and a
#     snapshot that already IS the resolved tree is left untouched, because
#     the snapshot directory is itself bind-mounted into running containers
#     and a rewrite would churn the very mounts it exists to keep stable.
#   - BOUNDED. One directory per deliberate pin, written once per ref and
#     never rewritten for a fixed ref (a ref is immutable, so identical bytes
#     mean nothing to do). A snapshot that exists but differs at an immutable
#     ref is corruption, and it is kept aside as ONE `.stale` directory rather
#     than deleted — it may still be mounted. Siblings from older pins are
#     never reaped here: an unchanged service keeps its mounts across an
#     upgrade, so reaping the old pin's snapshot would break exactly what this
#     section exists to keep. Each generation is a few hundred KB of YAML.
#   - DERIVED, NOT COMMITTED. The snapshot carries its own `.gitignore`
#     ignoring everything beneath it, so `git add -A` can never commit a copy
#     of the fetched tree. `resolve_stack` also ACCEPTS the snapshot as a
#     resolution source (below the cache, above the vendored copy), which is
#     what lets a stack survive its cache being deleted outright.

# live_dir_for <ref> — the snapshot path for that ref, under the service root.
live_dir_for() { printf '.kit/live/%s' "$1"; }

# live_is_usable <dir> <ref> — the compose-subtree half of `stack_is_usable`.
# The snapshot holds ONLY `templates/compose` (that is everything compose
# bind-mounts), so the kamal files are deliberately NOT required here:
# requiring them would refuse a snapshot that has every byte the stack mounts.
live_is_usable() {
  local dir="$1" ref="$2" have
  [ -d "$dir" ] || return 1
  have="$(read_stack_ref "$dir")" || return 1
  [ "$have" = "$ref" ] || return 1
  [ -f "$dir/templates/compose/docker-compose.yml" ] &&
    [ -f "$dir/templates/compose/otel-collector.yml" ] &&
    [ -d "$dir/templates/compose/tempo" ] &&
    [ -d "$dir/templates/compose/loki" ] &&
    [ -d "$dir/templates/compose/grafana/provisioning" ]
}

# sync_live_stack <ref> <stack-dir> — bring the snapshot to the resolved tree.
# Called by `up` after `resolve_stack`, so SRC is always a usable tree at REF.
sync_live_stack() {
  local ref="$1" src="$2" dest tmp
  dest="$(live_dir_for "$ref")"
  # The common case — every `up` after the first on a fixed pin — touches
  # nothing. A ref is immutable, so identical bytes mean there is nothing to
  # do, and leaving the directory alone leaves every live mount alone with it.
  if live_is_usable "$dest" "$ref" &&
    diff -rq "$src/templates/compose" "$dest/templates/compose" >/dev/null 2>&1; then
    info "live stack: $dest (unchanged — the snapshot already is this ref)"
    return 0
  fi
  # Staged beside the destination, like `fetch_stack` stages beside the cache:
  # a second `bin/dev` in another terminal never sees a half-copied tree.
  tmp="$dest.tmp.$$"
  rm -rf "$tmp"
  mkdir -p "$tmp/templates/compose"
  cp -a "$src/templates/compose/." "$tmp/templates/compose/"
  printf '%s\n' "$ref" >"$tmp/.kit-stack-ref"
  # Derived, not committed. This file is created BY the sync, alongside the
  # bytes it describes, and it edits no file the service owns — so a service
  # that never ignores `.kit/` still never commits a copy of the fetched tree.
  {
    printf '%s\n' '*' '!.gitignore'
  } >"$tmp/.gitignore"
  if ! live_is_usable "$tmp" "$ref"; then
    rm -rf "$tmp"
    die "could not stage the live stack snapshot for kit@$ref from $src.
    The snapshot was left untouched; nothing was started."
  fi
  if [ -e "$dest" ] || [ -L "$dest" ]; then
    # Reachable only through corruption: at an immutable ref the bytes cannot
    # have changed, so a usable-but-different snapshot means something rewrote
    # one side. Kept aside and never deleted while mounted — it may still be
    # bind-mounted, and the retired-generations lesson applies here too.
    #
    # The naive `rm -rf "$dest.stale" && mv "$dest" "$dest.stale"` deletes a
    # generation an older stack still bind-mounts on repeated corruption. A
    # rename keeps inodes but `docker inspect` still reports the PRE-rename
    # path, so a later `rm -rf "$dest.stale"` deletes the live mount's bytes
    # (measured in m154 §2: blind prefix test + `rm -rf` of the stale). The
    # safe rule: never delete a stale that is (or may be) mounted. If docker
    # is unavailable we cannot tell, so we keep conservatively and place the
    # current dest into the next numbered slot instead.
    if [ -e "$dest.stale" ] || [ -L "$dest.stale" ]; then
      _stale_abs=""; case "$dest.stale" in /*) _stale_abs="$dest.stale" ;; *) _stale_abs="$PWD/$dest.stale" ;; esac
      _dest_abs=""; case "$dest" in /*) _dest_abs="$dest" ;; *) _dest_abs="$PWD/$dest" ;; esac
      _sources=""; _known=1; _sources="$(mount_sources 2>/dev/null)" || _known=0
      _keep=0
      if [ "$_known" -eq 0 ]; then
        _keep=1
      elif generation_is_mounted "$_stale_abs" "$_sources" || generation_is_mounted "$_dest_abs" "$_sources"; then
        _keep=1
      fi
      if [ "$_keep" -eq 1 ]; then
        _n=0; while [ -e "$dest.stale.$_n" ] || [ -L "$dest.stale.$_n" ]; do _n=$((_n + 1)); [ "$_n" -lt 10000 ] || break; done
        mv "$dest" "$dest.stale.$_n"
        info "previous live snapshot kept at $dest.stale.$_n (previous .stale still mounted or docker unavailable — not deleted)"
        mv "$tmp" "$dest"
        info "live stack: $dest (snapshotted from kit@$ref)"
        unset _stale_abs _dest_abs _sources _known _keep _n
        return 0
      else
        rm -rf "$dest.stale"
      fi
      unset _stale_abs _dest_abs _sources _known _keep
    fi
    mv "$dest" "$dest.stale"
    info "previous live snapshot kept at $dest.stale (it differed at an immutable ref — one side was rewritten)"
  fi
  mv "$tmp" "$dest"
  info "live stack: $dest (snapshotted from kit@$ref)"
}

fetch_stack() {
  # `git init` + `fetch --depth 1` + `checkout --detach FETCH_HEAD`, not
  # `git clone --branch`. Three reasons, and the third is the one that matters:
  #
  #   - `--branch` takes a BRANCH OR A TAG, and cannot take a commit sha, so
  #     cloning cannot express the stricter of the two pin forms.
  #   - a clone of a bare remote at a sha needs the remote to have `uploadpack`
  #     configured to serve it; a fetch of a sha is one request.
  #   - fetching into a directory keyed by the ref means the checkout is
  #     disposable. Delete it and the next run rebuilds it; nothing is ever
  #     half-updated, because nothing is ever updated in place.
  #
  # `--depth 1` on a repository that is mostly YAML is a few hundred KB, and it
  # is the difference between a first `bin/dev` that feels instant and one that
  # clones a history nobody reads.
  command -v git >/dev/null 2>&1 ||
    die "git is not installed, and this script needs it to fetch kit from $(stack_url)."

  local ref="$1" dest="$2" tmp
  tmp="$dest.tmp.$$"
  rm -rf "$tmp"
  mkdir -p "$tmp"
  # `git init -q` and then a remote, rather than `git clone`, for the reason in
  # the comment above. `-c advice.detachedHead=false` because a detached
  # checkout is the intended end state here and the advice is noise on every
  # first run.
  git -C "$tmp" init -q
  git -C "$tmp" remote add origin "$(stack_url)"
  if ! git -C "$tmp" fetch --quiet --depth 1 origin "$ref" 2>"$tmp/.fetch-error"; then
    local why
    why="$(tail -3 "$tmp/.fetch-error" | tr '\n' ' ')"
    rm -rf "$tmp"
    die "could not fetch kit@$ref from $(stack_url).
   $why
   Check the ref exists:  git ls-remote $(stack_url) '$ref'
   To use a ref you already have on disk, point KIT_STACK_DIR at a kit checkout,
   or vendor one:  git clone --depth 1 $(stack_url) $(vendor_dir)
                        git -C $(vendor_dir) checkout $ref
                        git -C $(vendor_dir) rev-parse HEAD > $(vendor_dir)/.kit-stack-ref"
  fi
  git -C "$tmp" -c advice.detachedHead=false checkout --quiet --detach FETCH_HEAD
  # The ref is RECORDED, and this file is what makes a cached or vendored tree
  # acceptable later. A tree without it is a directory.
  printf '%s\n' "$ref" >"$tmp/.kit-stack-ref"
  # Built beside the destination and moved into place, so a second `bin/dev` in
  # another terminal never sees a half-populated cache directory. `mv` within a
  # filesystem is atomic; a `cp -R` into place is not, and the failure it causes
  # is a bind-mount error about a file that was about to exist.
  #
  # The destination is RETIRED, never removed — see the retired-generations
  # header above for the measurement. Both moves are renames within one
  # filesystem, so there is no instant at which `$dest` is neither the old tree
  # nor the new one: that is the property the old `rm -rf` bought with the mount
  # it broke, and this keeps it without the breakage.
  if [ -e "$dest" ] || [ -L "$dest" ]; then
    local retired
    retired="$(next_generation "$ref")" ||
      die "could not find a free generation name under $(generation_home)"
    mv "$dest" "$retired"
    info "previous generation kept at $retired"
  fi
  mv "$tmp" "$dest"
  prune_generations "$ref"
}

resolve_stack() {
  local ref
  ref="$(stack_ref)"
  validate_ref "$ref"

  # 1. An explicit directory. Read through `stack_setting`, so a `KIT_STACK_DIR`
  #    in `.env` counts — the first version read `${KIT_STACK_DIR:-}` from the
  #    process environment only, so the documented offline escape hatch, written
  #    in the file a developer is told to write it in, was silently ignored and
  #    the script went to the network instead. A documented escape hatch that
  #    only works when you remember to also pass it on the command line is not an
  #    escape hatch.
  #
  #    Not checked against the ref: naming a directory is a person saying "this
  #    one", and re-litigating it with a ref check would make the override
  #    useless for exactly the case it exists for — a kit worktree with local
  #    changes, which is what a kit contributor runs.
  local named
  named="$(stack_setting KIT_STACK_DIR "")"
  if [ -n "$named" ]; then
    [ -d "$named" ] ||
      die "KIT_STACK_DIR='$named' is not a directory."
    [ -f "$named/templates/compose/docker-compose.yml" ] ||
      die "KIT_STACK_DIR='$named' has no templates/compose/docker-compose.yml.
   It must be the ROOT of a kit checkout, not its templates/ directory."
    STACK_DIR="$named"
    info "stack: $STACK_DIR (KIT_STACK_DIR — named explicitly, ref not checked)"
    return 0
  fi

  # 2. The cache, keyed by the ref.
  local cached
  cached="$(stack_home)/$ref"
  if stack_is_usable "$cached" "$ref"; then
    STACK_DIR="$cached"
    info "stack: $STACK_DIR (cache)"
    return 0
  fi

  # 3. The live snapshot: the tree a previous `up` rendered from, owned by
  #    this service and checked against the ref like the cache. A snapshot at
  #    a different ref is not this stack, so it is skipped rather than used —
  #    the same rule as every other recorded tree here.
  local live
  live="$(live_dir_for "$ref")"
  if live_is_usable "$live" "$ref"; then
    STACK_DIR="$live"
    info "stack: $STACK_DIR (live snapshot — the tree this service rendered)"
    return 0
  fi

  # 4. The vendored copy. Checked against the ref, and the check is the feature:
  # this is the offline path, and an offline path that cannot tell you which
  # version it is running is worse than no offline path.
  local vendor
  vendor="$(vendor_dir)"
  if [ -e "$vendor" ]; then
    local vendored_ref=""
    vendored_ref="$(read_stack_ref "$vendor" || true)"
    if [ -z "$vendored_ref" ]; then
      die "$vendor exists but records no ref, so there is no way to know what it is.
   A directory that contains a compose file is not a kit checkout at a known version,
   and using one is how an offline loop stops matching what the team runs. Either
   remove it, or record what it is:
     git -C $vendor rev-parse HEAD > $vendor/.kit-stack-ref"
    fi
    if [ "$vendored_ref" != "$ref" ]; then
      die "$vendor is vendored at $vendored_ref, and $REF_FILE pins $ref.
   Using it anyway would run a stack you did not pin. Move the pin deliberately —
   bin/dev pin $vendored_ref — or update the vendored copy to $ref."
    fi
    if ! stack_is_usable "$vendor" "$ref"; then
      die "$vendor records $ref but is missing files the stack mounts
   (templates/compose/{docker-compose,otel-collector}.yml, tempo/, loki/,
   grafana/provisioning/, kamal/{deploy.yml.erb,kamal-backup.yml.erb,drill.sh}).
   Re-vendor it rather than starting a stack that will die four containers later
   on a bind mount."
    fi
    STACK_DIR="$vendor"
    info "stack: $STACK_DIR (vendored, and it declares the pinned ref)"
    return 0
  fi

  # 5. The network.
  if [ "$(stack_setting KIT_STACK_OFFLINE 0)" = "1" ]; then
    die "offline (KIT_STACK_OFFLINE=1) and kit@$ref is not available locally.
   Tried, in order:
     KIT_STACK_DIR          not set
     $cached
                          not present, or not at this ref
     $live      not present (no previous up rendered this ref here)
     $vendor        not present
   Fix it with any one of:
     bin/dev pin <ref>                              # on a machine with the network, first
     git clone --depth 1 $(stack_url) $vendor
     git -C $vendor checkout $ref
     git -C $vendor rev-parse HEAD > $vendor/.kit-stack-ref
     KIT_STACK_DIR=/path/to/a/kit/checkout bin/dev   # a checkout you already have"
  fi
  info "fetching kit@$ref from $(stack_url) (first run only)"
  fetch_stack "$ref" "$cached"
  STACK_DIR="$cached"
  info "stack: $STACK_DIR (fetched)"
}

# env_value <NAME> — read one variable out of .env without sourcing the file.
#
# `set -a; . ./.env` is how print_urls reads the ports, and there it is right:
# the file is this repository's own and every line in it is a developer who ran
# the gate on a machine like this one. It is NOT right here, because this runs
# BEFORE the ref is known and the value it produces decides whether anything is
# fetched at all — a `.env` with one stray unquoted line in it would then fail
# the fetch instead of failing the stack. Sourcing a config file to read one
# variable is how a value gets to execute code; grep does not.
env_value() {
  # `${2:-.env}` and NOT `local file="$2"` in the same `local`: under `set -u` a
  # positional parameter that was never passed is an error the moment it is
  # expanded, and `local` expands all of its arguments before it assigns any of
  # them. The first version read `$2` unconditionally and died with
  # `line 423: $2: unbound variable` on the very first call — which is every
  # call, since the file argument is optional by design.
  local name="$1"
  local file="${2:-.env}"
  local line value
  [ -f "$file" ] || return 0
  line="$(grep -E "^[[:space:]]*(export[[:space:]]+)?$name=" "$file" | tail -1 || true)"
  [ -n "$line" ] || return 0
  value="${line#*=}"
  # Strip one layer of matching quotes and nothing else. `sed` rather than bash
  # parameter expansion because the expansion to strip a leading and a trailing
  # quote independently would accept `"foo'` — two different mistakes that look
  # like one.
  value="$(printf '%s' "$value" | sed -e 's/^[[:space:]]*//' -e 's/[[:space:]]*$//' -e 's/^"\(.*\)"$/\1/' -e "s/^'\(.*\)'\$/\1/")"
  printf '%s' "$value"
}

# stack_setting <NAME> <default> — the process environment, then `.env`, then the
# default. In that order, and the order is the contract: `KIT_STACK_DIR=… bin/dev`
# is a person overriding for one run, and `.env` is a developer recording a
# decision. A value set in the shell that `.env` also names must not be silently
# replaced by the file, or a one-shot override would be a no-op.
stack_setting() {
  local name="$1" fallback="${2:-}" value
  value="$(eval "printf '%s' \"\${$name:-}\"")"
  [ -n "$value" ] && {
    printf '%s' "$value"
    return 0
  }
  value="$(env_value "$name")"
  [ -n "$value" ] || value="$fallback"
  printf '%s' "$value"
}

# --------------------------------------------------------------------------
# preflight
# --------------------------------------------------------------------------

compose() {
  # `docker compose` is the v2 plugin; `docker-compose` is v1 and is a different
  # tool with different flags. Probe for the plugin and fail with a message that
  # says which one is missing, because "unknown command" from docker is the
  # single least helpful error in this script.
  #
  # `--profile` is applied here, on every invocation, so no call site can forget
  # it. `up` is the one that matters for the default path; `ps` and `logs` need
  # it too, or `bin/dev status` reports a healthy stack as half-absent and
  # `bin/dev logs tempo` says "no such service".
  #
  # THE EMPTY-ARRAY EXPANSION IS THE TRAP, and it is a trap because `set -u` is
  # on. macOS ships bash 3.2, where an empty array expanded as "${a[@]}" under
  # `set -u` is an "unbound variable" ERROR rather than nothing — so the escape
  # hatch (`KIT_DEV_PROFILES= bin/dev up`, the documented way to run without the
  # observability stack) died on the first line of the script with
  #
  #   bin/dev: line 120: profile_args[@]: unbound variable
  #
  # which is a spectacularly bad way to find out that a documented flag does not
  # work. The conditional expansion below is the portable form: on bash 4.4+ it
  # is identical to "${a[@]}" and on 3.2 it yields nothing.
  # `--project-directory .` is NOT optional and not tidiness. `docker compose`
  # resolves a relative path in a `-f` file against the FIRST file's directory,
  # and the first file is now kit's, fetched into a cache directory outside this
  # repository. Without this flag the service's own `build: context: .` would
  # point at kit's templates directory and the service image would be built from
  # the wrong tree — a failure that produces an image, so nothing complains.
  #
  # `KIT_COMPOSE_DIR` is exported so the base file's own `${KIT_COMPOSE_DIR:-.}`
  # mount prefixes resolve back to the composed tree, whichever directory compose
  # runs from. The default `.` in the template is what keeps a hand-copied stack
  # working, and this is the line that stops that default from being wrong in the
  # composed case.
  #
  # THE COMPOSE ROOT IS THE LIVE SNAPSHOT WHEN THERE IS ONE. `up` snapshots the
  # resolved tree into `.kit/live/<ref>` and every `-f` and every bind source
  # below follows that root — so a running stack's mounts never point into the
  # cache, and no cache replacement, pruning or deletion moves them. Computed
  # here (rather than carried in a global from `up`) so `down`, `status` and
  # `logs` resolve the same tree the running stack was rendered from; the
  # snapshot is accepted only when it records the PINNED ref and carries every
  # file compose mounts, so a stale or corrupt one falls back to the resolved
  # tree rather than being composed from.
  local root="$STACK_DIR" live_ref live_root
  live_ref="$(stack_ref)"
  if [ -n "$live_ref" ]; then
    live_root="$(live_dir_for "$live_ref")"
    if live_is_usable "$live_root" "$live_ref"; then
      root="$live_root"
    fi
  fi
  local -a base=()
  base=(--project-directory . -f "$root/templates/compose/docker-compose.yml")
  [ -f docker-compose.yml ] && base+=(-f ./docker-compose.yml)
  # The COMPOSE directory, not the kit root: that is what the base file's
  # `${KIT_COMPOSE_DIR:-.}` prefixes mean, and `.` is the directory the file
  # itself lives in. Setting it to the kit root instead resolves the mounts to
  # `<kit>/grafana/provisioning`, which does not exist, and Docker's answer to a
  # missing bind source is to create a directory — so the failure arrives four
  # containers later as `read /etc/tempo/tempo.yaml: is a directory`.
  export KIT_COMPOSE_DIR="$root/templates/compose"

  if [ -n "$KIT_DEV_PROFILES" ]; then
    if docker compose version >/dev/null 2>&1; then
      docker compose "${base[@]}" --profile "$KIT_DEV_PROFILES" "$@"
    elif command -v docker-compose >/dev/null 2>&1; then
      docker-compose "${base[@]}" --profile "$KIT_DEV_PROFILES" "$@"
    else
      die "neither 'docker compose' (v2) nor 'docker-compose' is installed" 127
    fi
  else
    if docker compose version >/dev/null 2>&1; then
      docker compose "${base[@]}" "$@"
    elif command -v docker-compose >/dev/null 2>&1; then
      docker-compose "${base[@]}" "$@"
    else
      die "neither 'docker compose' (v2) nor 'docker-compose' is installed" 127
    fi
  fi
}

# --------------------------------------------------------------------------
# preflight: required compose interpolations
# --------------------------------------------------------------------------
#
# WHAT THIS IS AND WHY IT EXISTS, MEASURED.
#
# `templates/compose/docker-compose.yml` interpolates one variable with NO
# default:
#
#     KIT_POSTGRES_DATABASES: ${KIT_POSTGRES_DATABASES}
#
# and every other `${KIT_*}` in that file carries a `:-` default. The reason that
# one has none is written out in the compose file itself: a default of
# `${KIT_POSTGRES_DATABASES:-courier}` provisions ONE FLEET SERVICE's database for
# everybody who adopts kit, and an EMPTY default is refused by
# `initdb/10-cluster.sh`, which is the correct outcome. So the absence of a
# default is load-bearing and this check may not add one back.
#
# Docker Compose does NOT treat a bare `${VAR}` as an error. It substitutes the
# EMPTY STRING and logs one warning line, so the container is created, the init
# script runs, and `10-cluster.sh` refuses at its `:?` guard — and the postgres
# container exits 1. `up -d --wait` then waits out the whole
# `STACK_TIMEOUT` (180s by default) for a container that already died, and only
# then reports a failure whose message names a healthcheck. On the fleet that is
# the 900 seconds in the packet: not one missing variable being expensive to
# notice, but the notice arriving only after the readiness deadline has expired,
# five times over.
#
# So the cost was never the check; it was WHERE the check happened. This runs
# after the stack resolves (the compose file has to exist to be read) and BEFORE
# the first `docker compose` call, which is the earliest point at which the
# answer is available. It is seconds, and it names the variable.
#
# THE LIST IS DERIVED, NOT RESTATED. Every `${NAME}` with no `:-`/`-`/`:?`/`?`
# operator, in BOTH compose files — kit's base and the service's own override,
# because a service may introduce its own required variable and would otherwise
# discover it the same 180 seconds later. A list written out here would be a
# second copy of a fact that lives in the compose file, and it would go stale in
# exactly the way AGENTS.md's "a property stated in two places is a property
# asserted in zero times" rule describes. There is deliberately no
# `KIT_POSTGRES_DATABASES` in this file: the base compose file says which
# variables are required, and this reads that answer.
#
# EMPTY COUNTS AS MISSING, and that is not the same check. `templates/compose/
# .env.example` ships `KIT_POSTGRES_DATABASES=` with nothing after it, and
# `ensure_env` copies that example to `.env` on a fresh clone — so the most likely
# state of a new checkout is the variable PRESENT AND EMPTY. Compose substitutes
# an empty string for that too, so a presence check would pass on exactly the
# input that fails.
#
# `grep -o` and `sort -u` only: the pin validation above runs before any tooling
# beyond POSIX shell has been located, and a dependency in a preflight is a
# dependency in the path whose entire job is to fail before anything expensive
# happens.
required_compose_vars() {
  local file
  for file in "$@"; do
    [ -f "$file" ] || continue
    grep -o '\${[A-Za-z_][A-Za-z0-9_]*}' "$file" 2>/dev/null |
      sed -e 's/^\${//' -e 's/}$//'
  done | sort -u
}

# preflight_required — refuse a stack whose compose files cannot be
# interpolated, naming every missing variable and how to set it.
#
# `stack_setting "$name" ""` rather than a bare `${!name}`: the two sources it
# reads are the two compose itself reads (the process environment and `.env`), and
# a check that consulted a third source would disagree with the value the
# container gets.
preflight_required() {
  local -a files missing=()
  files=("$STACK_DIR/templates/compose/docker-compose.yml")
  [ -f docker-compose.yml ] && files+=(./docker-compose.yml)

  local var value
  while IFS= read -r var; do
    [ -n "$var" ] || continue
    value="$(stack_setting "$var" "")"
    [ -n "$value" ] || missing+=("$var")
  done <<EOF
$(required_compose_vars "${files[@]}")
EOF

  [ "${#missing[@]}" -eq 0 ] && return 0

  local count="${#missing[@]}"
  printf '\n\033[1;31mbin/dev: %s required by the compose file %s not set:\033[0m\n' \
    "$count" "$( [ "$count" -eq 1 ] && printf 'is' || printf 'are' )" >&2
  local m
  for m in "${missing[@]}"; do
    printf '  %s\n' "$m" >&2
  done
  printf '\n' >&2
  printf 'Docker Compose substitutes an empty string for a bare ${VAR} rather than\n' >&2
  printf 'refusing it, so nothing here has started yet and nothing will. The stack\n' >&2
  printf 'would come up with that value empty and fail later, minutes in.\n\n' >&2
  printf 'Set it in .env, which is the only file a developer edits:\n\n' >&2
  printf '  %s=<value>\n' "${missing[0]}" >&2
  [ "$count" -gt 1 ] && printf '  …and one line for each of the names above.\n\n' >&2
  case " ${missing[*]} " in
    *' KIT_POSTGRES_DATABASES '*)
      printf '\nFor KIT_POSTGRES_DATABASES, that value is the COMMA-separated list of\n' >&2
      printf 'the services sharing this one Postgres cluster. Each name becomes a\n' >&2
      printf 'database and an owner role of its own, and the database is the boundary\n' >&2
      printf 'between them (templates/compose/docker-compose.yml, "THE DECLARATION"):\n\n' >&2
      printf '  KIT_POSTGRES_DATABASES=alpha,beta\n\n' >&2
      printf 'One service needs its own name alone; a developer working across two\n' >&2
      printf 'services on one laptop lists both, and they get one container and two\n' >&2
      printf 'databases. Add a name later without wiping the volume: bin/dev db grant <name>\n\n' >&2
      ;;
  esac
  exit 1
}

# --------------------------------------------------------------------------
# .env
# --------------------------------------------------------------------------

ensure_env() {
  [ -f .env ] && return 0

  # The template comes from the FETCHED tree, not from this repository. That is
  # the point: a service carries no copy of it, so a new `${KIT_*}` in a kit
  # release reaches a service that has not been touched, instead of a service
  # whose committed `.env.example` is nine releases behind. A service's own
  # `.env` — git-ignored, and the only file a developer edits — is where its
  # port overrides and its own variables live.
  #
  # A service that already committed a `.env.example` still wins: `cp` refuses
  # to overwrite, and the explicit branch below says so, because a developer's
  # committed example is a deliberate thing and silently replacing it on a
  # first `bin/dev` would be a surprise with a merge conflict behind it.
  if [ -f "$STACK_DIR/templates/compose/.env.example" ]; then
    cp "$STACK_DIR/templates/compose/.env.example" .env
    info "created .env from the pinned kit ref's .env.example — edit it if a port collides"
    return 0
  fi
  # Only reachable if the pinned ref has no template, which is a broken kit rather
  # than a broken service. Said as such, because the alternative — silently
  # writing a three-line .env and letting every variable default — is a stack
  # that comes up and is not the one anybody reviewed.
  die "the pinned kit ref ($STACK_DIR) has no templates/compose/.env.example.
   That is a defect in kit, not in this service: bin/dev has no template to
   create .env from. Check out the ref and see:
     git -C '$STACK_DIR' ls templates/compose/.env.example"
}

# --------------------------------------------------------------------------
# the leftover .env.example
# --------------------------------------------------------------------------
#
# A service that adopted the copy-then-edit model has a committed
# `.env.example` beside its `.env`. `ensure_env` above no longer reads it — the
# fetched template wins — so that file is now DEAD, and a dead file in a
# repository is worse than a missing one: it looks authoritative and it is nine
# releases out of date.
#
# The first version of this check reported every unset `KIT_*` as drift. That is
# wrong now, and the reason is the packet itself: a fresh service's `.env`
# SHOULD contain almost nothing, because every unset variable resolves to kit's
# default and that default is the documented, reviewed one. Flagging it told
# nine developers their stack was wrong when it was exactly right.
warn_stale_env_example() {
  [ -f .env.example ] || return 0
  step "a .env.example here is no longer read"
  info "bin/dev takes every KIT_* default from the pinned kit ref, and composes fall"
  info "back to those defaults for anything your .env does not set. So this file is"
  info "now dead weight that will read as authoritative and will not be."
  info "  git rm .env.example"
  info "Keep .env itself: it is where your overrides live. Your kit pin is $REF_FILE."
}

# --------------------------------------------------------------------------
# steps
# --------------------------------------------------------------------------

up() {
  # Resolve FIRST, before `ensure_env`. The order is not cosmetic:
  # `ensure_env` copies the `.env.example` out of the FETCHED tree, so it cannot
  # run until the tree is fetched, and a ref is only known once the ref
  # variable has been read. Doing it the other way round is how a `.env` ends up
  # seeded from whatever stale copy happened to be in the working directory.
  step "resolving kit"
  resolve_stack
  # Snapshot BEFORE anything composes: from here on every `-f` and every bind
  # source resolves into `.kit/live/<ref>`, owned by this service, and no
  # cache lifecycle afterwards can move a byte a running container resolves
  # to. After `ensure_env` so the `.env` the message below tells the developer
  # to edit is about the stack, not the snapshot.
  sync_live_stack "$(stack_ref)" "$STACK_DIR"
  ensure_env
  warn_stale_env_example

  # BEFORE the first `docker compose` call, and after `ensure_env` so that the
  # `.env` the message tells the developer to edit actually exists. `up` is the
  # only step that starts containers, so it is the only step that needs it — a
  # `bin/dev status` against a misconfigured service should still report what is
  # running rather than refuse to look.
  preflight_required

  step "starting the stack from kit@$(stack_ref) (deadline: ${STACK_TIMEOUT}s)"
  # The wall-clock is measured here and printed at the end, because "the dev
  # loop is slow" is a claim everyone makes and nobody measures, and the whole
  # point of shipping five more containers is that it is not slow. A number in
  # the output is the only thing that settles the argument, and it is also what
  # makes a regression visible in a commit message.
  local started elapsed
  started=$(date +%s)

  # --wait blocks on health, not on "the container exists". A postgres that is
  # running but has not finished initdb answers nothing, and the migration step
  # below would fail against it.
  if ! KIT_TIMEOUT="$STACK_TIMEOUT" compose up -d --wait --wait-timeout "$STACK_TIMEOUT"; then
    printf '\n' >&2
    printf 'bin/dev: the stack did not become healthy in %ss.\n' "$STACK_TIMEOUT" >&2
    printf 'What is unhealthy:\n' >&2
    compose ps >&2 2>&1 | sed 's/^/  /' >&2
    printf '\nIts logs:\n' >&2
    compose logs --tail 40 >&2 2>&1 | sed 's/^/  /' >&2
    printf '\nRaise the deadline with KIT_DEV_TIMEOUT=300 bin/dev, or run bin/dev logs.\n' >&2
    die "stack did not become healthy — nothing further was run, so nothing is half-migrated" 1
  fi

  step "migrating"
  migrate

  step "seeding the local admin"
  seed

  print_urls

  elapsed=$(( $(date +%s) - started ))
  step "up in ${elapsed}s"
  # WHICH STACK YOU GOT, and the difference is not cosmetic: one of these two
  # is four containers lighter and several minutes faster to become healthy, so
  # a run that prints nothing about it leaves a developer guessing whether the
  # slowness they are about to investigate is theirs or the profile's.
  if [ -n "$KIT_DEV_PROFILES" ]; then
    info "observability backends are ON (compose profile: $KIT_DEV_PROFILES)"
    info "for the cheap stack instead:  bin/dev up   (no profiles, no backends)"
  else
    info "the cheap stack: postgres/nats/redis + the collector, no observability"
    info "backends. Telemetry is STILL ON — the collector is not behind the"
    info "profile, and it still drops high-cardinality dimensions at ingest."
    info "For traces/logs/metrics to read back:  KIT_DEV_PROFILES=observability bin/dev up"
    info "For the single-pane ClickStack view instead:  KIT_DEV_PROFILES=clickstack bin/dev up"
  fi
}

migrate() {
  # Each language migrates differently; the repo's own command is the only one
  # that is correct, and every cafaye service has one under a bin/ or script/
  # entry. Probed in a fixed order and the first match wins, so this works
  # without kit having to know six migration systems.
  if [ -x bin/migrate ]; then
    bin/migrate
  elif [ -x bin/rails ] && [ -f config/application.rb ]; then
    bin/rails db:prepare
  elif [ -x bin/ecto.setup ]; then
    mix ecto.create
    mix ecto.migrate
  elif [ -f Cargo.toml ] && [ -x bin/prime ]; then
    # Rust services own their migrations as SQL applied by the app; the primer
    # is the supported entry point and runs them.
    bin/prime --fast
    info "no bin/migrate: ran bin/prime --fast — apply migrations in your own task if you have one"
  else
    die "no migration command found (looked for bin/migrate, bin/rails, bin/ecto.setup). Add bin/migrate and re-run."
  fi
}

seed() {
  # The admin user is what stops the first five minutes of a new checkout being
  # "log in as whom?". An upsert, not an insert: running this twice must not
  # create two admins or fail on a unique constraint.
  if [ -x bin/seed ]; then
    bin/seed
  elif [ -x bin/rails ] && [ -f config/application.rb ]; then
    bin/rails db:seed
  else
    info "no bin/seed: skipping the admin seed"
  fi
}

# db_grant <name> — the statements that add one service to a RUNNING cluster.
#
# WHY THIS EXISTS, AND WHY IT PRINTS RATHER THAN RUNS.
#
# `docker-entrypoint-initdb.d` runs once per volume, so a service named after the
# cluster was first provisioned never gets a database. The documented answer was
# `bin/dev down -v && bin/dev up`, which is correct and destroys the developer's
# data. Four files in this repository — templates/database/README.md,
# DECISIONS.md, the init script and the compose file — told the reader to run
# `bin/dev db grant <name>` instead, and no such command existed. A promise to a
# command that is not there is the same defect DECISIONS.md itself was created to
# fix, one layer down: the reader is told the way out exists and then finds that
# it does not.
#
# It PRINTS rather than executes, and that is the decision rather than a
# limitation. These are `CREATE ROLE` and `CREATE DATABASE` — a role and a
# database, on a cluster a developer's service is about to be handed credentials
# for. A command whose failure mode is "half-provisioned, and the service's
# migrations then run against a database that was never created" is not something
# a wrapper should perform on a stack it did not start. So this prints the exact
# statements, in the order the init script uses, validated, and the reader pastes
# them into `psql`. The alternative — a `--yes` flag — is a second thing to get
# right in the one command whose mistake is a stray superuser role.
#
# It is the same boundary the init script applies, not a smaller one: the REVOKE
# and the per-role limits are load-bearing for isolation, and a database created
# here without them is a database every role in the cluster can walk into.
db_grant() {
  local svc="${1:-}"
  if [ -z "$svc" ]; then
    die "usage: bin/dev db grant <service-name>"
  fi
  # The same validator the init script uses, and for the same reason: the name is
  # pasted unquoted into identifiers, and a service name is a value from a `.env`
  # file rather than a trusted literal. Silently mangling an identifier here is
  # how one service ends up owning another's database.
  case "$svc" in
    [!a-z_]* | *[!a-z0-9_]*)
      die "'$svc' is not a bare identifier. It must match [a-z_][a-z0-9_]*, and it has to be the SAME name as your database, your owner role and your application_name."
      ;;
  esac

  # Read the cluster's own settings rather than assuming the defaults, so the
  # connection line printed below is the one that works on THIS machine.
  # shellcheck disable=SC1091
  [ -f .env ] && { set -a && . ./.env && set +a; }
  local port="${KIT_POSTGRES_PORT:-15500}"
  local user="${KIT_POSTGRES_USER:-cafaye}"
  local pass="${KIT_POSTGRES_PASSWORD:-cafaye}"
  local admin_db="${KIT_POSTGRES_DB:-cafaye_platform}"

  cat <<SQL
bin/dev db grant $svc — add one service to a cluster that is already running.

  docker-entrypoint-initdb.d only runs on a FRESH volume, so naming this service
  in KIT_POSTGRES_DATABASES and running \`bin/dev up\` will not create anything.
  (And \`bin/dev down -v && bin/dev up\` DOES work, by deleting your data.)

Paste this into the cluster. It is the same provisioning the init script performs,
in the same order, and every statement in it is load-bearing:

  psql -h localhost -p $port -U $user -d $admin_db   # password: $pass

  CREATE ROLE "$svc" LOGIN NOSUPERUSER NOCREATEDB NOCREATEROLE NOREPLICATION
    PASSWORD '$pass';
  CREATE DATABASE "$svc" OWNER "$svc";

  -- the LOGIN role, which owns nothing. A login role that owns its tables can
  -- ALTER TABLE ... DISABLE ROW LEVEL SECURITY and DROP POLICY, so the account
  -- boundary in templates/database/tenancy/ needs this second role to be real.
  -- It exists whether or not this service has adopted those templates, which is
  -- why it is here and not in a migration.
  CREATE ROLE "${svc}_app" LOGIN NOSUPERUSER NOCREATEDB NOCREATEROLE NOREPLICATION
    PASSWORD '$pass';
  GRANT "${svc}_app" TO "$svc";

  -- the boundary. Without this line EVERY role in the cluster can CONNECT to
  -- $svc, and isolation becomes "nobody happened to grant you SELECT".
  REVOKE ALL ON DATABASE "$svc" FROM PUBLIC;
  GRANT CONNECT, TEMPORARY ON DATABASE "$svc" TO "$svc";
  GRANT CONNECT, TEMPORARY ON DATABASE "$svc" TO "${svc}_app";

  -- blast radius, so this service cannot take the connections the other eight need
  ALTER ROLE "$svc" CONNECTION LIMIT ${KIT_POSTGRES_ROLE_CONNECTIONS:-10};
  ALTER ROLE "$svc" SET statement_timeout = '${KIT_POSTGRES_STATEMENT_TIMEOUT_MS:-15000}';
  ALTER ROLE "$svc" SET idle_in_transaction_session_timeout = '${KIT_POSTGRES_IDLE_IN_TRANSACTION_TIMEOUT_MS:-30000}';
  ALTER ROLE "${svc}_app" SET statement_timeout = '${KIT_POSTGRES_STATEMENT_TIMEOUT_MS:-15000}';
  ALTER ROLE "${svc}_app" SET idle_in_transaction_session_timeout = '${KIT_POSTGRES_IDLE_IN_TRANSACTION_TIMEOUT_MS:-30000}';

Then, in that database:

  psql -h localhost -p $port -U $user -d $svc
  REVOKE CREATE ON SCHEMA public FROM PUBLIC;   -- PG15+; already the default, stated so it is true
SQL

  # The extension step is conditional and cluster-wide, so it is reported rather
  # than assumed: the admin role has to create them, and a service role cannot.
  if [ -n "${KIT_POSTGRES_EXTENSIONS:-}" ]; then
    cat <<SQL

  And in EACH database above, as the admin role — a service role cannot:
  CREATE EXTENSION IF NOT EXISTS ${KIT_POSTGRES_EXTENSIONS%%,*} [ , … per name in KIT_POSTGRES_EXTENSIONS ];
SQL
  fi

  cat <<SQL

  Then add the name to KIT_POSTGRES_DATABASES in your .env, so a FRESH volume
  provisions it without any of this. The statements above are for the volume you
  already have.
SQL
}

print_urls() {
  # Read from .env rather than hardcoding, so the printed URL is the URL that
  # actually works on this machine. A printed URL that does not resolve is worse
  # than none.
  # shellcheck disable=SC1091
  set -a && . ./.env && set +a

  # Every port read from .env, never hardcoded — including the observability
  # ones. A printed URL that does not resolve is worse than none, and the whole
  # point of kit's claimed port block is that these are NOT the well-known
  # numbers, so hardcoding them here would print the one address that is wrong.
  #
  # AND ONLY IF THAT PROFILE IS ACTUALLY ON. Printing a URL for a backend that
  # `compose ps` will not list is a printed lie: a developer pastes 15900 into a
  # browser on the default path, gets connection refused, and concludes the
  # stack is broken. So the observability block is conditional on
  # KIT_DEV_PROFILES — which is the whole point of the default having flipped.
  local pg_port="${KIT_POSTGRES_PORT:-15500}"
  local nats_port="${KIT_NATS_CLIENT_PORT:-15600}"
  local redis_port="${KIT_REDIS_PORT:-15800}"
  local pg_user="${KIT_POSTGRES_USER:-cafaye}"
  local pg_pass="${KIT_POSTGRES_PASSWORD:-cafaye}"
  local pg_db="${KIT_POSTGRES_DB:-cafaye_platform}"

  step "ready"
  cat <<URLS
   postgres     postgresql://$pg_user:$pg_pass@localhost:$pg_port/$pg_db
   nats         nats://localhost:$nats_port
   redis        redis://localhost:$redis_port

   otel (otlp)  http://otel-collector:${KIT_OTEL_HTTP_PORT:-4318}   (compose network only)

   service      ${KIT_DEV_SERVICE_URL:-http://localhost:3000}
URLS

  if [ -n "$KIT_DEV_PROFILES" ]; then
    if [ "$KIT_DEV_PROFILES" = "clickstack" ]; then
      cat <<URLS

   hyperdx      http://localhost:${KIT_CLICKSTACK_PORT:-15910}        (traces, logs, metrics, errors — one pane)
URLS
    else
      cat <<URLS

   grafana      http://localhost:${KIT_GRAFANA_PORT:-15000}        (traces, logs, errors)
   tempo        http://localhost:${KIT_TEMPO_PORT:-15900}
   loki         http://localhost:${KIT_LOKI_PORT:-15901}
URLS
    fi
  else
    cat <<URLS

   (no observability backends: this run started the cheap stack. For grafana,
   tempo and loki:  KIT_DEV_PROFILES=observability bin/dev up.  For the
   single-pane ClickStack view:  KIT_DEV_PROFILES=clickstack bin/dev up)
URLS
  fi

  cat <<URLS

   Nothing leaves this machine unless you point it somewhere. To use your own
   backend instead of the ones above, set <SERVICE>_OTEL_ENDPOINT in your own
   .env — that is the only contract, and the shipped collector is just its
   default value. Unset the variable and the exporter is a genuine no-op: no
   queue, no retry loop, no warning per request, no dial at boot.
URLS
}

status() {
  step "stack status"
  resolve_stack
  compose ps
}

logs() {
  resolve_stack
  if [ "${1:-}" = "" ]; then
    compose logs -f
  else
    compose logs -f "$1"
  fi
}

down() {
  step "stopping the stack (volumes kept)"
  resolve_stack
  # `down` without -v on purpose: your local data is the thing you are not
  # throwing away by typing `bin/dev down`.
  #
  # AND IT STOPS THE WHOLE PROJECT, NOT THE PART THIS SHELL'S VARIABLE NAMES.
  # Measured, and it is the mirror of the `print_urls` fix: the observability
  # backends are behind a profile, and `docker compose down` without that
  # profile does not touch them. So after
  #
  #     KIT_DEV_PROFILES=observability bin/dev up
  #     bin/dev down                          # the cheap path, variable unset
  #
  # `down` exits 0, prints "stopping the stack", and leaves tempo, loki and
  # grafana RUNNING AND HEALTHY. You have to type `down` a second time with the
  # variable set. A teardown that reports success while three containers and
  # their memory limits are still up is the same defect as a printed URL that
  # does not resolve, and it is worse: this one costs memory on the machine.
  #
  # The profiles come from `compose config --profiles`, so the list is the
  # PROJECT's own and a profile added to the compose file needs nothing here.
  # A hardcoded `observability` would be a second copy of a name that already
  # lives in the compose file, which is the ratchet this file keeps avoiding.
  local -a pf=()
  while IFS= read -r p; do
    [ -n "$p" ] && pf+=(--profile "$p")
  done < <(compose config --profiles 2>/dev/null || true)
  compose "${pf[@]+"${pf[@]}"}" down
}

nuke() {
  # The only destructive step. Spelled out, never the default, never aliased.
  step "stopping the stack and DELETING its volumes"
  resolve_stack
  info "postgres, nats and redis data in this stack are gone after this."
  # Same reason as `down`, and it matters MORE here: a `nuke` that leaves the
  # observability containers running has removed the volumes they were writing
  # to, so Docker recreates those volumes empty on the next start and the
  # developer is left with a half-deleted stack rather than a clean one.
  local -a pf=()
  while IFS= read -r p; do
    [ -n "$p" ] && pf+=(--profile "$p")
  done < <(compose config --profiles 2>/dev/null || true)
  compose "${pf[@]+"${pf[@]}"}" down --volumes --remove-orphans
}

# --------------------------------------------------------------------------
# stack: resolve, and say where it landed
# --------------------------------------------------------------------------

# This exists because the whole packet turns on "which bytes of kit is this",
# and a fact that only exists inside a function that also brings up eight
# containers cannot be checked. `bin/dev stack` is the same resolution with no
# side effects, and `tests/fetch_test.sh` drives it rather than `up` — the
# fetch path is then provable without docker in the loop.
#
# The `resolved: <path>` line is an INTERFACE, not progress output. It is the
# first machine-readable line the script emits, and the test parses it, so a
# reformatting of the human-facing `step`/`info` lines cannot silently break the
# check. Printed to stdout, with the human lines above it.
stack_cmd() {
  step "resolving kit"
  resolve_stack
  printf 'resolved: %s\n' "$STACK_DIR"
  printf 'ref: %s\n' "$(stack_ref)"
  printf 'compose files:\n'
  printf '  %s (kit, pinned)\n' "$STACK_DIR/templates/compose/docker-compose.yml"
  [ -f docker-compose.yml ] &&
    printf '  %s (this service — an override, not a fork)\n' "$PWD/docker-compose.yml"
  if [ -f docker-compose.yml ]; then
    step "merged by docker compose"
    # Rendered, not described. This is the check the packet asks for — a compose
    # file that has never been `docker compose config`-validated is a YAML file —
    # and it is here so a developer sees what the two files actually became
    # rather than trusting the prose above.
    compose config 2>&1 | sed 's/^/  /' || true
  else
    info "this service adds no compose file; the pinned stack is the whole thing"
  fi
}

# --------------------------------------------------------------------------
# live: resolve, snapshot into the service-owned tree, and say where it landed
# --------------------------------------------------------------------------

# The staged-launcher entrypoint. Site's staged `bin/launch` calls
# `templates/bin/dev.sh stack` and then composes using the shared cache
# directly — so anything that retires, prunes or deletes that cache moves a
# byte a live mount resolves to. `up` already snapshots into
# `.kit/live/<ref>` and composes from there, but `stack` is intentionally
# read-only and `up` also starts containers, migrates and seeds. A launcher
# that only stages needs the snapshot without the side effects, so this is
# the smallest Kit-owned command that does exactly that: `resolve_stack`
# (the same five-source resolution `stack` and `up` share), then
# `sync_live_stack` (the same content-addressed snapshot `up` writes), then
# the same machine-readable `resolved: <path>` line `stack` prints — except
# the path is ALWAYS the service-owned `.kit/live/<ref>`, never the cache.
#
# No `compose config` here on purpose. `stack` renders the merged config for
# a human; a launcher parses `^resolved: ` and composes itself, so config
# output on stdout is noise on the line it reads.
live_cmd() {
  step "resolving kit"
  resolve_stack
  sync_live_stack "$(stack_ref)" "$STACK_DIR"
  local dest
  dest="$(live_dir_for "$(stack_ref)")"
  printf 'resolved: %s\n' "$dest"
  printf 'ref: %s\n' "$(stack_ref)"
}

# --------------------------------------------------------------------------
# pin: the deliberate upgrade
# --------------------------------------------------------------------------
#
# THE UPGRADE IS A COMMAND, NOT AN EDIT, and the reason is the diff.
#
# Moving the pin in `$REF_FILE` is the one edit in a service repository that changes
# every container it starts, so the interesting question is never "can I write
# this sha" — it is "what changes if I do". `bin/dev pin` prints the stack
# diff between the old ref and the new one, and refuses to write anything until
# it has.
#
# It writes the pin and nothing else. It does not touch `.env` at all, does not
# run compose, and does not upgrade the images: the containers that come up next
# are a separate, visible step, and folding them in would make "what changed"
# answerable only after it had already happened.
pin_cmd() {
  local new="${1:-}"
  [ -n "$new" ] || die "bin/dev pin needs a ref:  bin/dev pin v0.4.0   |   bin/dev pin <40-char sha>"
  validate_ref "$new"

  local old
  old="$(stack_ref)"

  # The same-ref case FIRST, before anything is fetched. Pinning to the ref you
  # are already on is not a move, it is a no-op, and answering it by fetching two
  # copies of the same commit is both slow and a lie about what happened.
  if [ "$old" = "$new" ]; then
    step "already pinned to $new"
    info "nothing to do. The stack is unchanged."
    return 0
  fi

  local new_dir old_dir="" old_cache
  new_dir="$(stack_home)/$new"
  old_cache="$(stack_home)/$old"
  if stack_is_usable "$old_cache" "$old"; then
    old_dir="$old_cache"
  elif [ -n "$(stack_setting KIT_STACK_DIR "")" ] && [ -d "$(stack_setting KIT_STACK_DIR "")" ]; then
    old_dir="$(stack_setting KIT_STACK_DIR "")"
  fi

  # FETCH BOTH SIDES, or say plainly that it could not.
  #
  # The first version only fetched the NEW ref, inside the branch that already
  # needed the old one on disk — so on a machine that had never run `bin/dev` it
  # printed "the current ref is not on this machine, so the diff cannot be computed
  # here" and wrote the pin anyway. That is the command's entire reason for
  # existing: it is the one place a developer sees what they are about to change,
  # and a first `bin/dev pin` — the first use, and the use most likely to BE the
  # upgrade — was the one case where it could not.
  #
  # The cache is keyed by ref, so fetching both is two directories that do not
  # touch each other. Offline is honoured rather than attempted:
  # `KIT_STACK_OFFLINE=1` says the diff needs the network and still writes the
  # pin, because refusing to record a move somebody has already decided on because
  # the laptop is on a train would be a worse answer than saying so.
  local offline
  offline="$(stack_setting KIT_STACK_OFFLINE 0)"
  step "what changes between $old and $new"
  if [ -z "$old_dir" ] && [ "$offline" != "1" ]; then
    info "fetching the current ref so the diff is real rather than a claim"
    fetch_stack "$old" "$old_cache" || true
    stack_is_usable "$old_cache" "$old" && old_dir="$old_cache"
  fi
  if [ ! -d "$new_dir" ] && [ "$offline" != "1" ]; then
    fetch_stack "$new" "$new_dir" || true
  fi

  if [ -z "$old_dir" ] || [ ! -d "$new_dir" ]; then
    info "the stack diff could NOT be computed: one of the two refs is not on this"
    info "machine. The pin is still written — refusing to record a move you have"
    info "already decided on is not this command's job — but read what you are"
    info "pinning before you commit it:"
    info "  git -C <a kit checkout> diff --stat $old $new -- templates/compose"
  else
    # Two real directories, and `diff -r` over the one subtree that IS the stack.
    # A diff of the whole repository would be the classifier's job and not this
    # script's: what a developer needs here is which CONTAINERS change, because
    # that is what the move costs them.
    local changed
    changed="$(diff -rq "$old_dir/templates/compose" "$new_dir/templates/compose" 2>/dev/null || true)"
    if [ -z "$changed" ]; then
      info "the compose stack is IDENTICAL between the two refs."
    else
      info "changed under templates/compose:"
      printf '%s\n' "$changed" | sed 's/^/    /'
    fi
  fi

  step "writing the pin"
  # ONE file, replaced whole, and the replacement is written before the old one
  # is moved away. A `mv` within a filesystem is atomic, so a `bin/dev pin` that
  # is killed mid-write leaves the previous pin intact rather than a file with
  # half a sha in it — and half a sha is a pin the validator refuses, which would
  # present as "your stack stopped working" for a reason that happened once, in
  # a crash, and left no trace.
  #
  # The file is COMMITTED, so the write is visible in `git status` and the bump
  # is a reviewable line in a pull request. That is the whole reason the pin is
  # not in `.env`: a pin nobody can review is a pin nobody moves deliberately,
  # and a pin nobody moves deliberately is one that goes stale.
  local tmp
  tmp="$REF_FILE.new.$$"
  {
    # `printf '%s\n' TEXT` and NOT `printf 'TEXT\n'`. In a single-quoted shell
    # string `\n` is two characters, so the string runs on past the closing quote
    # and the next line is parsed as code — which is exactly what happened here,
    # and it left the emitted `kit.ref` reading
    #   # ...a 40-character commit# sha, or a v<semver> tag...
    # with two comment lines run together by a missing newline. The format string
    # and the text are separate arguments, so there is no escape to get wrong.
    printf '%s\n' '# The kit ref this repository runs. One line: a 40-character commit'
    printf '%s\n' '# sha, or a v<semver> tag. NEVER a branch.'
    printf '%s\n' '# Move it with:  bin/dev pin <ref>   (it prints the stack diff first)'
    printf '%s\n' "$new"
  } >"$tmp"
  mv "$tmp" "$REF_FILE"
  info "$REF_FILE now pins kit@$new"
  info "Commit it. The stack a repository runs is a reviewed statement, not a"
  info "value that happens to be on this laptop."
  info "Nothing has been started. Run \`bin/dev\` to bring the stack up on the new ref."
}

# --------------------------------------------------------------------------

main() {
  case "${1:-up}" in
    up)
      # The stack is resolved inside `up`, and `require_files` is GONE: the six
      # files it demanded — docker-compose.yml of kit's, otel-collector.yml,
      # grafana, tempo, loki, mimir — are exactly the copy this packet removes.
      # Requiring them would require the drift back.
      #
      # `ensure_env` is inside `up` for the same reason: it copies the fetched
      # tree's `.env.example`, so it runs after the fetch.
      #
      # A service's own `docker-compose.yml` is optional, and `compose` already
      # omits it when absent.
      up
      ;;
    stack) stack_cmd ;;
    live) live_cmd ;;
    pin)
      shift
      pin_cmd "${1:-}"
      ;;
    down) down ;;
    nuke) nuke ;;
    status) status ;;
    logs)
      shift
      logs "${1:-}"
      ;;
    migrate) migrate ;;
    seed) seed ;;
    db)
      shift
      case "${1:-}" in
        grant)
          shift
          db_grant "${1:-}"
          ;;
        *) die "usage: bin/dev db grant <service-name>" ;;
      esac
      ;;
    -h | --help | help) usage ;;
    *)
      printf 'bin/dev: unknown command: %s\n\n' "$1" >&2
      usage >&2
      exit 2
      ;;
  esac
}

main "$@"
