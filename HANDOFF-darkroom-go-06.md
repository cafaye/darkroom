# HANDOFF — darkroom-go-06: the binary, the gate cutover, and the Rust removal

**Status: work landed and merged. One red remains, with its cause measured and its
fix written out below. Read that section first.**

Branch was `go-rewrite-06`. Four commits, all on `master` as of `535092a`:

| commit | what |
|---|---|
| `0b427f1` | `cmd/darkroom` — the binary, ported from `src/main.rs`, with its config table |
| `3d85d0d` | `bin/prime` — cargo steps removed, regenerate-and-diff added; `bin/tier-counts` — eleven constants out, four in, and a new identity |
| `f788743` | `docker/Dockerfile` — Go, `CGO_ENABLED=0`, no musl; `src/`, `tests/`, `Cargo.toml`, `Cargo.lock`, `rust-toolchain.toml` deleted |
| `c83bec0` | `mise.toml`, `gate.yml`, `ci.yml`, `README.md`, `AGENTS.md`, `.env.example`, `docker-compose.yml`, `bin/gate-self-test` |

## THE ONE RED, AND IT IS A ONE-TOKEN FIX

`bin/gate-self-test` reports **3 of 23** breakages. All three are the same root
cause and it is in `gate.yml`, not in the repository:

```yaml
# gate.yml:163 and :170 — the two countable proofs
match: 'ok[ \t]+github\.com/cafaye/darkroom/internal/service[ \t]([0-9]+)s'
minimum: 1
```

`go test -v` prints `ok  \tgithub.com/…/internal/service\t3.912s`. The capture
group `([0-9]+)s` **cannot match a decimal**, so the proof matches nothing and
core reports `gate.proof-missing` for both, which is what takes down the proving
control and the two floor breakages.

The fix, measured rather than reasoned about:

```python
re.search(r'ok[ \t]+github\.com/cafaye/darkroom/internal/service[ \t]([0-9]+)s', line)
#> None
re.search(r'ok[ \t]+github\.com/cafaye/darkroom/internal/service[ \t]([0-9]+(?:\.[0-9]+)?)s', line)
#> <re.Match; group(1) == '3.912'>
```

**This was found but not fixed — the worktree was removed from under me while I
had the fix in hand.** It is not committed, because the manager had already
merged and committing to `master` is not this packet's call. It is four
characters per line and it is the whole of the red.

### And the same paragraph is worth keeping, because the two halves disagree

The character-class spelling is **correct in one language and wrong in the
other**, which is why this took three attempts:

```
Python  re.search(r'ok[[:space:]]+github', line)   -> None   (FutureWarning: nested set)
Python  re.search(r'ok[ \t]+github', line)        -> match
awk     /^ok[[:space:]]/                            -> matches a tab
```

`bin/tier-counts` uses awk and needs `[[:space:]]`. `gate.yml` is compiled by
Python's `re`, which has no POSIX classes and does accept `\t`. Both spellings are
correct where they are. `gate.yml` says so in place.

## The three unfinished things, decided explicitly

1. **No NATS publisher loop. Still not done, deliberately.** The outbox table and
   `internal/service/outbox.go`'s insert path are here and tested; nothing moves
   rows. The Rust README's reason is kept: *"a publisher loop that cannot publish
   is worse than an absent one."*
2. **Three sweepers unwired. Still unwired, and nothing was silently deleted.**
   The Go tree has **no** sweeper at all — there is no `findStalePending` — so no
   tested-but-unscheduled function went missing. `PendingSweepAfter` is carried as
   a constant so the window a future sweeper must respect is written down.
3. **The deployment target did not build. Fixed by the rewrite.** The old
   Dockerfile ran `rustup target add x86_64-unknown-linux-musl` and copied out of
   a target its `cargo build` never produced, and `ring` needed a C toolchain for
   it. `CGO_ENABLED=0 go build` produces a statically linked stripped binary with
   no musl target and no C toolchain. Not "pending a decision about the release
   pipeline" — the decision was answered by a different language.

## Verification

```
$ ./bin/prime --db                                    EXIT=0
$ ./bin/tier-counts /tmp/prime.log                    EXIT=0
  ok  default  variant-pipeline checks               24
  ok  database  harness and role checks              15
  ok  database  service checks                       29
  ok  database  httpapi checks                       19
  ok  identity  ./internal/darkroomdb/pgtest    15 enumerated, 15 passed with a database
  ok  identity  ./internal/service              29 enumerated, 29 passed with a database
  ok  identity  ./internal/httpapi              19 enumerated, 19 passed with a database
```

The binary, against the real cluster:

```
healthz=200  {"status":"ok"}
readyz=200   {"status":"ready"}
/v1/assets with no credential = 401
SIGTERM -> "darkroom: shutting down" grace=30s -> "darkroom: stopped"
```

The image, built and run:

```
docker build -> EXIT=0
docker run -p 18080:8080 darkroom-go06:probe
  {"status":"ok"}     healthz=200
  {"status":"ready"}  readyz=200
  /v1/assets no credential = 401
$ docker run darkroom-go06:probe        # the image's own defaults
  ERROR darkroom: exiting error="darkroom: configuration:
    DARKROOM_S3_BUCKET is required when DARKROOM_OBJECT_STORE=s3"
  EXIT=1
```

`CGO_ENABLED=0 GOOS=linux go build -trimpath -ldflags="-s -w"` →
`ELF 64-bit LSB executable, ARM aarch64, **statically linked**, stripped`.

## What the gate is now

`bin/prime` lost five cargo steps and gained a regenerate-and-diff. The cargo
steps are **removed**, not commented out: a commented-out step is a line nobody
reads, and a live one in a repo with no `Cargo.toml` exits 127 and takes `set -e`
and the whole gate down.

`bin/prime`'s floors, every number read out of this tree with `go test -list`:

```
cmd/darkroom 7, objectstore 21, checksum 6, storagekey 4, pgtest 15,
domain 7, problem 7, service 29, auth 12, httpapi 19, variants 24
```

`auth` and `httpapi` had **no floor at all** before — they were unmapped while
`src/http.rs` was the tree under test, which is precisely the unmapped-suite
failure the step exists to catch. They are the two most important packages in the
tree.

### The identity changed, and the old one is false here

The Rust script asserted `skipped == passed`. It cannot translate: a Go test is
not `#[ignore]`d, it calls `pgtest.Require` and skips at runtime. Measured
against no database:

```
./internal/darkroomdb/pgtest    4 PASS, 11 SKIP, 15 total
./internal/service              9 PASS, 20 SKIP, 29 total
```

Skipped counts are 31; passed counts are 44. Asserting the Rust identity would
assert something false. What is true and is in one log: **enumerated without a
database == passed with one**, checked in both directions, plus a hard failure on
any `--- SKIP:` inside a `--db` tier.

## Four guards, each shown red

| planted | caught by |
|---|---|
| `DATABASE_URL` made a fallback for the login role | `TestDATABASEURLIsNotAFallbackForTheLoginRole` |
| a document edit that changes **no route** | `bin/prime`'s regenerate step, `EXIT=1` — the drift tests **cannot** see this one |
| a `--db` tier reporting skips | `tier-counts` — *"a check that did not run"* |
| a floor raised past reality, and `internal/auth` emptied | `bin/prime` — *"a green go test with no tests is a check that proves nothing"* |

## Deliberately not done

- **`Idempotency-Replayed` is still unwritten** (packet 05's finding, carried).
- **`GET /v1/assets?order=asc` is still ignored**, recorded in `ListAssets`.
- **No cross-tenant 404 test end to end through the router.** Asserted where it is
  decided — `internal/service` and `service.find` — and it needs a database.
  Packet 05 named this as the next packet's work and `bin/prime` now carries a
  step header for `internal/httpapi` in the `--db` branch precisely so that
  adding one is a line rather than a redesign.
- **The coverage floor of 50 is the Rust tree's measured number, carried across.**
  It has not been re-measured for Go. Stated in `ci.yml`, `README.md` and
  `AGENTS.md` rather than quietly kept as if it were current.
- **The Dockerfile's `go mod download` line is unverified on this machine** — its
  Docker has no egress to `proxy.golang.org`. The build was verified with the
  dependency layer vendored and every other line identical. A networked builder
  still has to prove that one line.
- **No dev-auth verifier.** `internal/auth` has one constructor and it is always
  on; `DARKROOM_ENV` is read, logged, and gates nothing. A behaviour change from
  the Rust binary, stated rather than discovered by a developer.
- **`DARKROOM_DB_MAX_CONNECTIONS` is not read.** `darkroomdb.Open` owns the pool
  size. Asserted inert by a test rather than promised in a comment.