# HANDOFF — darkroom-go-01

The Go substrate is built, gated and green. **This packet is complete.** The list
below is what a later packet needs, not what is unfinished — except where it says
otherwise, and nothing does.

Branch `go-rewrite-01`, six commits on top of `fde7acb`.

## What is here

- `go.mod` — `github.com/cafaye/darkroom`, `go 1.26.1`, pgx v5 only.
- `migrations/0004_account_isolation.sql` — kit's
  `templates/database/tenancy/substrate.sql` byte for byte, plus two
  `protect_table` calls, plus the grants the substrate does not give.
- `migrations/embed.go` — the SQL compiled in.
- `internal/darkroomdb/` — `Open`, `InTx`, `InAccountTx`.
- `internal/darkroomdb/pgtest/` — a schema per test, 14 tests.
- `bin/prime` — a Go tier, so the newer tree is not the unchecked one.
- `AGENTS.md`, `migrations/README.md` — updated for a repository with two trees.

## Verify

```sh
export TEST_DATABASE_URL="postgres://darkroom:cafaye@localhost:15500/darkroom?sslmode=disable"
export DARKROOM_DATABASE_URL="postgres://darkroom_app:cafaye@localhost:15500/darkroom?sslmode=disable"
go build ./... && go vet ./... && go test ./internal/darkroomdb/...
```

Without those two variables the suite SKIPS with a named reason, and prints it
under `-v`. That is a counted skip, not a pass.

## Two things the next packet will hit first

**`InAccountTx` is the only way a transaction starts, and that is load-bearing.**
A function that takes a `*pgx.Tx` from anywhere else has no identity set, and
under FORCE RLS that means its statements match NULL — a `DELETE` deletes nothing
and exits 0. This packet lost a silently no-op cleanup to exactly that. If a
later packet needs a transaction that is genuinely not account-scoped
(`outbox_events` has no policy, so a maintenance drain is legitimate), it should
be a named, separate function with a comment saying why, not a query outside
`InAccountTx`.

**`migrations/0004`'s down is commented SQL, not executable.** darkroom's
migrations carry no up/down markers, so an executable down in the same file means
`psql -f` applies the up half and immediately undoes it. That was measured: it
left `relrowsecurity = false` with `relforcerowsecurity = true` on a table with no
policies. `migrations/README.md` has the full reasoning. A packet that introduces
a migration runner with markers should introduce them for the whole directory or
none of it.

## Not decided here, and should not have been

- **`outbox_events` and `idempotency_keys` have no RLS.** Neither has an
  `account_id` column and a policy reading `using (true)` is the shape the
  substrate exists to refuse. `migrations/0004`'s header argues this at length.
  `idempotency_keys.principal_key` is `user_id:account_id` as one opaque string,
  which is a real boundary in the key; promoting it to RLS needs a schema change
  belonging to whichever packet writes the idempotency write path.
- **A migration runner.** `migrations.Apply` applies the set in order and
  deliberately does not open a transaction, because Postgres refuses
  `CREATE INDEX CONCURRENTLY` and `VACUUM` inside one. Whether deploys need
  version tracking is core's `cafaye.yml` question, not this packet's.
- **`internal/config`.** `DATABASE_URL` is read from the environment by
  whatever calls `darkroomdb.Open`; the config layer belongs with the HTTP layer
  that first needs one.

## The Rust tree

Untouched, and still gated. It is deleted by a later packet, which is the one
that makes `bin/prime`'s Go tier the only tier.
