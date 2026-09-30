# Migrations

Plain SQL, applied in filename order by `sqlx::migrate!`, which embeds them in
the binary — there is no `migrations/` directory to lose in a distroless image.

## Rules

**Migrations are a deploy step, not a boot step.** The service never runs them
at startup. Apply them with `sqlx-cli` or `psql` before the new binary starts.

**Never edit an applied migration.** Write a new one. A file that has run in any
environment is history; editing it means the next deploy silently does nothing
and the two versions of the service disagree about the schema.

**Every migration is reversible**, or carries a comment saying why it cannot be.
All four here are: `down` drops only tables this service owns.

```sh
psql "$DATABASE_URL" -f migrations/0001_assets.sql
psql "$DATABASE_URL" -f migrations/0002_outbox_events.sql
```

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
