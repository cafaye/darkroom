# Migrations

Plain SQL, applied in filename order. They are compiled into the binary — there is
no `migrations/` directory to lose in a distroless image — by `migrations/embed.go`
under Go. That reason is the whole of it, and it is why it survived the rewrite
from the Rust tree (which embedded the same bytes through `sqlx::migrate!`, and
which no longer exists here): a migration set read from the filesystem at boot is
a migration set a distroless image cannot have, whichever language the service is
written in. The embedding is `//go:embed *.sql`, and the reason `migrations/`
carries a `package` clause at all is that `go:embed` refuses a `..` in its
pattern — see the header of `migrations/embed.go`.

## Rules

**Migrations are a deploy step, not a boot step.** The service never runs them
at startup. Apply them with `psql` before the new binary starts.

**Never edit an applied migration.** Write a new one. A file that has run in any
environment is history; editing it means the next deploy silently does nothing
and the two versions of the service disagree about the schema.

**No up/down markers, so a down is COMMENTED SQL.** Every file here is plain SQL
and the runner in `migrations/embed.go` has no notion of a down half at all — its
`File` is a name and a body, and `Apply` executes each body in filename order.
So appending an *executable* down to a file means `psql -f` applies the up half
and then immediately undoes it. That was measured, not reasoned: `0004` shipped
with an executable down, and applying it left `relrowsecurity = false` on both
tables with `relforcerowsecurity = true` — the one catalog column that says the
policies should bite, set on a table with no policies at all. `0004`'s down is
therefore complete and reviewable as SQL inside a comment, and applying a file
never runs it.

**Every migration is reversible**, or carries a comment saying why it cannot be.
`0001`–`0003` drop only tables this service owns; `0004` drops the substrate;
`0005` drops the four functions it adds, which is back to the copy `0004` carried.

**And reversible is not free, so the cost of each is written down.** "You can undo
it" answers the wrong question; the question an operator reaching for `psql` is
asking is *what does undoing it lose that I cannot get back*. Two of these are not
symmetric with the rest, and neither is recoverable from the other side:

- **`0002`, the outbox.** A row in `outbox_events` is an event that has been
  committed but **not yet published**, and `published_at is null` is this
  repository's only definition of "not published". Dropping the table deletes
  undelivered events, and unlike a published one they **cannot be reconstructed
  from the consumer's side** — the consumer never saw them. The order that avoids
  that is the order this repository already documents everywhere else: drain the
  outbox first, then roll back the schema.
- **`0003`, the idempotency ledger.** Dropping `idempotency_keys` makes retries
  unsafe again, and **no test in this repository can observe that**, because the
  tests exercise the code path and the table is what the code path consults. A
  client whose response was lost retries and is served the request a second time:
  a second charge, a second upload, a second email. Every other table in this
  schema loses rows that describe the past; this one loses the record of what has
  already happened, which is the only thing making a repeat safe.

The general shape: the two tables that look like the least important ones to drop
are the two whose loss is the most permanent, and in both cases the damage is
invisible to everything still running.

```sh
psql "$DATABASE_URL" -f migrations/0001_assets.sql
psql "$DATABASE_URL" -f migrations/0002_outbox_events.sql
psql "$DATABASE_URL" -f migrations/0003_idempotency_keys.sql
psql "$DATABASE_URL" -f migrations/0004_account_isolation.sql
psql "$DATABASE_URL" -f migrations/0005_substrate_completion.sql
```

## The account boundary, and which tables have one

`0004_account_isolation.sql` is kit's
`templates/database/tenancy/substrate.sql` copied byte for byte plus darkroom's
calls, and it protects the **two** tables that carry an `account_id` column:
`assets` and `asset_variants`. Its header states at length why not three.

`0005_substrate_completion.sql` carries the rest of the copy. `0004` was copied
while kit's template held eight functions; kit added four more the next day —
`current_user_id`, `begin_user`, `protect_membership_table`, `membership_tables` —
and darkroom's copy was never compared against the template again. kit's
`tests/substrate_copy_check.py` is the comparison that says so, in four `FAIL
MISSING function` lines, and `0005` is the repair: the four functions with their
kit commentary, and nothing else. It is a new file rather than an edit to `0004`
because of the rule above, and it protects no table — darkroom has no membership
table for `protect_membership_table` to name, so the migration is definitions
without a call.

`outbox_events` and `idempotency_keys` have **no** `account_id` and therefore no
policy — and `darkroom_app` still receives DML on both, explicitly, in `0004`. That
is not an oversight to be fixed later: `protect_table` grants DML only as part of
*protecting* a table, so an application that adopted the substrate and stopped there
came up with a login role that could read its account-scoped tables and write
nothing at all.

`internal/darkroomdb/pgtest` asserts both halves: the boundary tests, and
`TestTheLoginRoleCanWriteEveryTableTheServiceWrites`, which is the test that would
have caught it.

## No foreign keys across services

`assets.account_id` and `assets.owner_user_id` are plain UUID columns with no
`references` clause. identity owns accounts and users; darkroom owns neither,
and a foreign key into another service's database is not a constraint — it is a
coupling of release order and a shared outage wearing a constraint's clothes.
Postgres cannot enforce it across two connections anyway, so it would be a
declaration of trust dressed as enforcement. The platform rule is
database-per-service (PLAN.md §7, "Already aligned"), and the only real foreign
key here is the one inside this database: `asset_variants.asset_id` →
`assets.id`, `on delete cascade`, because both tables are darkroom's.
