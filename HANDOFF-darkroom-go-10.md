# HANDOFF-darkroom-go-10 — the README described a service that no longer exists

`README.md` only. Five commits: `6eec63b`, `2653720`, `097f274`, `4395e92`,
`f70d707`. No Go source was touched.

## What is done and verified

Every symbol the README points at for a **current** code path now names
something that exists in this tree, in Go. Each rename was checked with a
`grep` for the definition; the full table is in the packet report.

The three claims that were **false about a missing thing**, all now restated:

| was | now |
|---|---|
| "`store::find_stale_pending` exists and is tested" | not built; only `service.PendingSweepAfter` is here |
| "`truncate` still runs, and names its tables unqualified" | there is no `truncate` in `pgtest`; `drop schema … cascade` is the reaper |
| "Running it" listed 3 migrations | 4 — omitting `0004_account_isolation.sql` gave a working service with **no tenant boundary** |

That third one is the only finding here that would have shipped a security
hole, and it came out of the same pass as the rest.

## What is half-done, and why

The **non-README** files carry the identical drift this packet was about. Named,
not fixed — the brief put every file except `README.md` out of scope, and two
other workers were editing Go source in this wave.

1. **`internal/service/outbox.go:23`** — "`claimUnpublished` exists for the
   publisher that a later packet adds, and it is the only read path in this
   file." `grep -rn claimUnpublished --include='*.go'` returns **this comment
   and nothing else**. This is the same error as `find_stale_pending`: a
   function that was never written, described as existing. It matters more than
   the README's did — this is a source comment another packet will read.
2. **`migrations/0004_account_isolation.sql:157`** — "`bin/dev down -v`".
   `bin/` holds `prime`, `tier-counts`, `gate-self-test` and nothing else.
3. **`cafaye.yml:7-9`** — "`exposes.api` still points at the same
   `openapi/v1.yaml` because the packet that introduces the Go HTTP layer is
   not this one." The Go HTTP layer is here: `internal/httpapi`, the generated
   `*chi.Mux`, `chi.Walk`.
4. **`migrations/README.md:5`** — "the Rust tree this repository still carries"
   (`src/` was deleted in darkroom-go-06), and line 12 tells you to apply
   migrations "with `psql` or `sqlx-cli`". `sqlx-cli` is Rust.
5. **`.github/workflows/ci.yml:30-41`** — describes `bin/prime`'s tiers in cargo
   terms as present tense: "Tier 6 is `cargo test -- --ignored`", "a run that
   stops at tier 5", and names `tests/common/mod.rs` /
   `tests/schema_isolation.rs`, neither of which exists.

## Your first move

Item 1, and it is a comment-only edit in
`internal/service/outbox.go`. Take it alone: it is one file, no test changes, and
it is the last place in the tree where a reader is told a function exists and
will not find it.

Items 2-5 are prose in configuration and SQL. If a wave is running, group them
into one commit with the reason stated once, the way the README's own
`FIXED BY THE REWRITE` bullet states its reason.

## One thing that is not stale, and looks like it

The README's tier table (`24` / `15` / `29` / `19`) and `bin/prime`'s ten
`assert_go_suite` floors are **correct as of `f70d707`**. They will need raising
when the `Idempotency-Replayed` and `?order=asc` packets land their tests —
that is the documented rule, not drift, and this packet deliberately did not
pre-empt it.