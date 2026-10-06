-- 0004_account_isolation.sql — the account boundary, enforced by Postgres.
--
-- THE LAYOUT OF THIS FILE IS DELIBERATE AND IT IS IDENTITY'S, because identity is
-- the fleet's Go adopter and this is a Go rewrite: darkroom's header, then kit's
-- `templates/database/tenancy/substrate.sql` COPIED VERBATIM, then darkroom's
-- calls. `identity/migrations/00016_account_isolation.sql` is identical in shape.
--
-- "COPIED VERBATIM, NOT ADAPTED, NOT TRIMMED, NOT REIMPLEMENTED" is identity's
-- sentence and it is the rule rather than the phrasing. The substrate is the
-- fleet's shared definition of the account boundary, and a service that edited its
-- copy would be a service whose boundary is its own private opinion. kit's
-- `templates/database/tenancy/README.md` "Adopting this" step 1 says to copy it;
-- this is that copy, and the kit body below is byte-identical to the template it
-- came from. Everything the substrate decides is argued in its own comments and
-- none of it is restated here. The three things a reader of THIS file needs and
-- cannot get from the copy are below.
--
-- WHY DARKROOM NEEDS IT. Until this migration the tenant boundary was entirely in
-- Rust: `account_id` was a column, `src/store.rs` carried `WHERE account_id = $1`
-- in every statement, and `tests/tenant_scoping.rs` asserted the predicate was
-- present by reading the source as text. That guard is real and it is the right
-- guard, and it is a guard against a FORGOTTEN predicate rather than against a
-- WRITTEN one — the string `account_id = $` is exactly what it looks for. RLS
-- goes on top of a predicate that is still there and is still load-bearing:
-- kit's substrate says under "what this file does not prove" that the queries
-- must keep carrying their own `where account_id = ?`, because the predicate is
-- what makes the query indexable and what catches a mistake in the policy
-- expression. The Go rewrite keeps it.
--
-- THE THREE EXISTING MIGRATIONS ARE NOT RENUMBERED. darkroom's applied files are
-- `0001`, `0002`, `0003`, applied in filename order. This is `0004`, it follows
-- them, and nothing was renumbered — `migrations/README.md` "Never edit an
-- applied migration" is about editing, and this file edits none of the three.
--
-- THE TWO CALLS AT THE BOTTOM ARE THE WHOLE OF WHAT THIS MIGRATION DECIDES, and
-- `cafaye.protect_table` is the ONLY entry point for an ordinarily scoped table.
--
-- WHY TWO CALLS AND NOT THREE, stated here because it is the one place where a
-- reader's count disagrees with the packet brief and the brief is wrong.
-- darkroom has FOUR tables across its three migrations, and two of them carry an
-- `account_id` column:
--
--   assets, asset_variants     account_id NOT NULL -> protected below
--   outbox_events              NO account column
--   idempotency_keys           NO account column
--
-- `outbox_events` and `idempotency_keys` are NOT account-scoped and cannot be made
-- so by a policy, because `protect_table` needs a real `account_id` column to
-- predicate on, and a policy that reads `using (true)` is precisely the shape the
-- substrate's own header exists to refuse: a policy that permits everything is
-- indistinguishable, in `pg_policies`, from no policy at all, and it is read as
-- protection by whoever audits it next.
--
--   outbox_events     is the publisher's own queue. It holds every tenant's events
--                      and the publisher reads all of them by design; it is not
--                      tenant data, it is darkroom's mail. A per-account policy on
--                      it would break the publisher and an unconditional one would
--                      claim a protection it does not have. It is left unprotected,
--                      and the honest way to say that is in this file rather than
--                      implied by an absence.
--
--   idempotency_keys carries `principal_key`, which is `user_id:account_id` as one
--                      opaque string (migrations/0003_idempotency_keys.sql). The
--                      account IS in there and it is deliberately NOT a column: the
--                      primary key is `(key, endpoint, principal_key)` and splitting
--                      the principal to index on it would make the ledger wider than
--                      the thing it is a ledger FOR. Its tenant safety is the
--                      predicate in the key — a replay is scoped to
--                      `(endpoint, principal)`, so one tenant's key cannot replay
--                      another tenant's response. That is a real boundary and it is
--                      a boundary in the key rather than in a policy, and
--                      promoting it to RLS needs a schema change belonging to
--                      whichever packet writes the idempotency write path — not to
--                      a substrate migration that would then have to be un-applied.
--
-- `cafaye.unprotected_tables()` is what makes the above auditable rather than a
-- claim: it reports every table with an `account_id` column that no `protect_table`
-- call named, so an unprotected account-scoped table cannot be added quietly
-- later. Zero rows out of two account-scoped tables is the answer this migration
-- intends, and `internal/darkroomdb` asserts it.

-- kit template — the ACCOUNT boundary inside one service's database.
--
--     psql "$MIGRATIONS_DATABASE_URL" -f substrate.sql
--
-- WHAT THIS IS. One schema, one GUC, one setter, one function that protects a
-- table, and one sweep that says whether the service is still isolated. Postgres
-- enforces the account boundary; the application does not have to remember to.
--
-- WHY IT EXISTS HERE AND NOT IN EACH SERVICE. Across the nine account-scoped
-- services there is not one `ROW LEVEL SECURITY`, not one `CREATE POLICY` and not
-- one non-owner login role. Tenancy is enforced entirely by hand-written
-- `WHERE account_id = ?` in six languages, which means it is one forgotten
-- predicate per query per service and the whole suite stays green. A boundary
-- that lives in application code is only as strong as the least recently
-- reviewed query. This is the same trade kit already makes with the database
-- itself, where the boundary is `REVOKE` and not good intentions.
--
-- THE TWO ROLES, AND WHY ONE OF THEM IS THE POINT.
--
--   <service>       the owner. LOGIN, owns the database and every table, runs
--                   migrations. This is the role kit's cluster already
--                   provisions and a service already uses, so adopting this
--                   changes nothing about how a service builds its schema.
--   <service>_app   the LOGIN the application uses. It owns nothing, so it
--                   cannot `ALTER TABLE ... DISABLE ROW LEVEL SECURITY`, drop a
--                   policy, or truncate around one. It is granted DML and
--                   nothing else.
--
-- FORCE ROW LEVEL SECURITY is still required, and it is required for the OWNER.
-- Postgres exempts a table's owner from its own policies; `FORCE` is what removes
-- that exemption. Without it a table whose policies are all in place still reads
-- as fully protected while the role that owns it reads every account's rows, and
-- nothing reports that: the policies are there, `relrowsecurity` is true, and
-- the owner walks straight past them. It is the one setting on this page that
-- Supabase's row-level-security guide does not document and that no lint in this
-- fleet checks for, which is why `templates/database/tenancy/isolation.sql`
-- asserts it by RUNNING the denials as the owner rather than by grepping for it.
--
-- THE CONSEQUENCE, STATED HERE BECAUSE IT WILL SURPRISE SOMEONE. Once a table is
-- protected, the OWNER reads zero rows from it too — no policy names the owner,
-- and FORCE removed the exemption that used to let it through. That is the
-- correct posture and it is also the adoption cost: a migration that has to read
-- or backfill rows either sets the identity (`set local role <service>_app` plus
-- `begin_account`) or goes through a `SECURITY DEFINER` function. A migration
-- that silently reads zero rows fails immediately, which is the good direction,
-- but it is a change to how a service writes backfills and it is worth knowing
-- before the first one runs rather than after.
--
-- THE `(select ...)` WRAPPING IS NOT STYLE.
--
--   using (      cafaye.current_account_id() = account_id )      -- per ROW
--   using ( (select cafaye.current_account_id()) = account_id )  -- per STATEMENT
--
-- Postgres evaluates a bare function call in a policy qualifier once per
-- candidate row and hoists an uncorrelated scalar subquery into an InitPlan that
-- runs once per statement. Measured on this kit, five rows, one call:
--
--     using (account_id = (select probe_id()))   ->  1 call
--     using (account_id = probe_id())            ->  5 calls
--
-- and `TestEveryHelperCallInAPolicyIsWrapped` in `internal/darkroomdb/pgtest`
-- re-checks it on every run, because a comment saying so is not a measurement
-- and this is the sort of difference that is invisible until the table is large,
-- which is the worst time to find it. `protect_table` only ever writes the
-- wrapped form. A hand-written policy that writes the bare form is correct and
-- slow, and nothing in a normal run will ever notice.
--
-- WHAT THAT TEST IS, PRECISELY, because the two are not the same check and the
-- older sentence here conflated them. It deparses the live policies with
-- `pg_get_expr` and asserts the helper call sits inside a `(select …)`, so what
-- holds on every run is the SPELLING. It does not COUNT calls, and nothing in
-- this tree does: `1 call` against `5 calls` above is a measurement of this
-- kit's Postgres, recorded because it is the reason the wrapped form is the one
-- written rather than because anything re-derives it. A behavioural count needs
-- a probe function and a fixture of rows, and neither exists here. The Rust tree
-- had both in `tests/tenancy_test.sh`, deleted with the rest of `tests/`; this
-- file used to name that script as though the count were still being taken on
-- every run, and a reader who went looking for it found nothing.
--
-- WHAT IS NOT HERE. No JWT parsing, no token format, no session store, and no
-- `auth.uid()`. How a service turns the credential in its hand into an account
-- is that service's own business — identity issues opaque session tokens and
-- gateway verifies JWKS bearer JWTs, and a template that picked one of those would
-- be wrong for the other. The seam is `cafaye.begin_account/1`, and a service
-- calls it once per request from whatever it already authenticates.
--
-- Idempotent, deliberately: this is a migration, migrations get re-run — by a
-- deploy step applying the set again, by `pgtest` applying the whole set into
-- every test's throwaway schema, and by hand when someone re-applies a file
-- whose half of the schema is already there — and `protect_table` drops and
-- recreates its policies so a re-run converges rather than failing on a
-- duplicate name.

-- Its own schema, and not `public`, so `cafaye` is never part of a sweep over
-- the service's tables and never collides with a name the service chose.
create schema if not exists cafaye;

-- THE ONE SEAM.
--
--   returns NULL when no account has been set  ->  every policy reads zero rows
--   raises      when the value is not a uuid   ->  a plumbing bug, loudly
--
-- The NULL is the load-bearing half and it is deliberate. "No identity" and "an
-- identity that owns none of these rows" are indistinguishable by construction,
-- which is core's `docs/tenancy.md` D33 arriving from the other end: a policy that
-- raised on an absent identity would tell a caller its request was not
-- authenticated, and a caller can be made to believe that about somebody else's
-- request.
--
-- The raise is the other half, and it is not a contradiction. An ABSENT identity
-- is a legitimate state that must read as nothing. A MALFORMED one is a bug in
-- the service's own plumbing, and reading nothing would report it as a service
-- with no data, which is the worst possible way to find out.
create or replace function cafaye.current_account_id()
returns uuid
language plpgsql
stable
as $caf$
declare
  raw text;
begin
  -- `missing_ok = true` is what makes an unset GUC NULL rather than an error.
  -- A dotted custom GUC needs no CREATE to be read: Postgres treats the
  -- placeholder as set-to-empty, which is the second case below.
  raw := current_setting('cafaye.account_id', true);
  if raw is null or raw = '' then
    return null;
  end if;
  return raw::uuid;
exception
  when invalid_text_representation then
    raise exception 'cafaye.account_id is set to % which is not a uuid', raw
      using errcode = '22023',
            hint = 'cafaye.begin_account/1 takes a uuid. A request that reaches the '
                   'database with a malformed account has a bug in whatever '
                   'authenticated it, and this is the cheapest place to find it.';
end
$caf$;

-- SET THE IDENTITY, ONCE PER REQUEST, TRANSACTION-LOCAL.
--
-- `is_local = true` is the whole design. Every one of kit's six drivers holds a
-- POOL, and a session-level account variable on a pooled connection is the worst
-- shape this thing can take: the connection goes back to the pool still carrying
-- tenant A's identity, tenant B is handed it, and tenant B reads tenant A's rows.
-- Transaction-local cannot outlive the request, so a leak needs the caller to
-- forget to open a transaction, which is a different bug with a different
-- symptom.
--
-- A NULL argument CLEARS rather than failing, because "this worker has no
-- account" is a legitimate call and it goes through here rather than through a
-- raw `set_config`, so there is exactly one place in a service's codebase that
-- writes this GUC.
--
-- THE AUTOCOMMIT CONSEQUENCE, STATED RATHER THAN HIDDEN. Outside an explicit
-- transaction, `is_local = true` expires at the end of the statement that set it,
-- so a caller reaching the database in autocommit reads zero rows. That cannot
-- leak and it is loud rather than silent — a service that suddenly sees none of
-- its own data fails its first integration test — and `isolation.sql` runs every
-- case inside an explicit transaction for exactly this reason.
create or replace function cafaye.begin_account(p_account uuid)
returns void
language plpgsql
as $caf$
begin
  if p_account is null then
    perform set_config('cafaye.account_id', '', true);
    return;
  end if;
  perform set_config('cafaye.account_id', p_account::text, true);
end
$caf$;

-- ===========================================================================
-- CREDENTIAL RESOLUTION — the one query whose subject the boundary does not
-- know yet. Read this before changing anything below; MD24 is the decision and
-- its cost, and DECISIONS.md is where the alternatives are recorded as dead.
-- ===========================================================================
--
-- THE PROBLEM, IN ONE SENTENCE. A scoped token, an invitation token and a
-- client id all resolve by "find the row by an unguessable value and learn the
-- account from it", and the account is WHAT THE QUERY IS FOR — so a query with
-- no identity cannot be scoped by an identity it has not computed yet. Measured
-- by `identity` against its real protected `api_keys` table, as the OWNER:
--
--     no identity (the state a token request is in)      0 rows
--     identity = the credential's own account            1 row
--     identity = another tenant's identity               0 rows
--
-- The first line is the finding, and it is the quietest one available: the token
-- does not resolve, `ErrNoRows` becomes "not found", and the HTTP layer answers
-- 401. Every machine credential, refused as though it did not exist.
--
-- WHY NOT THE OTHER THREE ANSWERS. Each is measured or read rather than
-- assumed, and each is written down in `DECISIONS.md` (MD24):
--
--   * A `BYPASSRLS` role — Supabase's answer, and right for Supabase's
--     topology: `service_role` is `nologin noinherit bypassrls` granted only to
--     `authenticator`, and what bounds it is who may assume it plus its object
--     grants. kit has one per-request role and no separate service layer, so a
--     bypass role here is a bypass over the WHOLE DATABASE held by the role
--     every request already authenticates as.
--   * A `SECURITY DEFINER` function — does not work under FORCE. FORCE applies
--     to the definer, and only BYPASSRLS skips RLS; no service role has one.
--   * Dropping FORCE — MD23's measurement: 1 row with, 3 without, on the one
--     table holding every machine credential.
--
-- WHAT IS HERE INSTEAD. One extra `for select` policy, whose qualifier is the
-- value the CALLER PRESENTED, read from a transaction-local GUC:
--
--     using (token_digest = (select cafaye.current_credential_digest()))
--
-- The predicate is in the POLICY and not only in the caller's query, and that is
-- the whole design rather than a detail. RLS policies combine permissively, so a
-- policy that said merely "a credential session may select this table" would hand
-- the caller a table-wide SELECT and let it browse every key in the database
-- with the one query this mechanism exists to prevent. Because the policy
-- carries the same predicate the query carries, the honest semantics are:
--
--     A resolution session may read exactly the credential row whose digest it
--     presented, and nothing else in the database.
--
-- `select * from api_keys` inside one of these sessions returns THAT row, not
-- the table. `isolation.sql` asserts it as `credential/resolution-cannot-browse`
-- and asserts the other half — that the mechanism did not become a general
-- bypass — as `credential/resolution-does-not-open-another-table`.
--
-- WHAT IT CANNOT DO, all five, so nobody has to find them:
--
--   1. It cannot tell a secret from a label. It widens a read to the row whose
--      value you presented, and whether that value is unguessable is the
--      SERVICE's knowledge. `oidc_clients.client_id` is an identifier, not a
--      secret, and this is the wrong mechanism for it. The invariant that keeps
--      the mechanism from becoming a general bypass is exactly that one: you can
--      only widen a read to a row you could already name.
--   2. It cannot write. `for select`, so every write policy still demands an
--      account identity: a resolution session cannot mint a credential.
--   3. It does not open any other table. It is a policy on a named table, and
--      `account_users` still reads zero rows in a resolution session.
--   4. It is transaction-local, like `begin_account/1`, and that is not tidiness
--      — the six drivers hold pools, and a session-level digest survives the
--      pool and hands one resolution's credential to the next connection.
--   5. It is not audited at runtime. The GUC leaves nothing behind; what IS
--      auditable is the SCOPE, and `credential_tables()` below is the one query
--      that answers "which tables in this database can be read with no account".
--      A claim this file cannot honour would be a comment pretending to be a
--      control, so it is not made.
--
-- TWO SEAMS, NOT ONE. `begin_credential/1` is a second transaction-local GUC and
-- the cost of that is real and is why the substrate has no third. It is set in
-- exactly one function, so a service has exactly one place that writes it.

-- The digest the session is resolving. NULL when none is set, and the NULL is
-- load-bearing for the same reason `current_account_id()`'s is: with no digest
-- set, `token_digest = NULL` is never true, so a table carrying only a resolve
-- policy plus an account policy reads nothing at all rather than raising. A
-- credential table is still fully protected without a digest — this policy only
-- ever ADDS the one row whose digest is present.
create or replace function cafaye.current_credential_digest()
returns text
language plpgsql
stable
as $caf$
declare
  raw text;
begin
  -- `missing_ok = true` makes an unset GUC NULL rather than an error; the second
  -- case is Postgres treating an unregistered dotted placeholder as set-to-empty.
  raw := current_setting('cafaye.credential_digest', true);
  if raw is null or raw = '' then
    return null;
  end if;
  return raw;
end
$caf$;

-- OPEN THE RESOLUTION, ONCE, TRANSACTION-LOCAL. The twin of `begin_account/1`,
-- and a NULL argument clears rather than failing, for the same reason.
--
-- THE CALLER PASSES THE VALUE IT ALREADY COMPUTED. A service hashes the token it
-- was handed — that hash is how it was going to query the table anyway — and
-- hands the digest to Postgres as the statement that scopes the read. Postgres
-- does not learn the secret, does not learn how it was derived, and gains no
-- ability to compute one: it is given a string it will compare against a column.
create or replace function cafaye.begin_credential(p_digest text)
returns void
language plpgsql
as $caf$
begin
  if p_digest is null then
    perform set_config('cafaye.credential_digest', '', true);
    return;
  end if;
  perform set_config('cafaye.credential_digest', p_digest, true);
end
$caf$;

-- PROTECT ONE CREDENTIAL TABLE. The one call a credential table's migration
-- makes, and it is ONE call: it protects the table exactly as `protect_table`
-- does — same four policies, same ENABLE, same FORCE, same index on
-- `account_id` — and then adds the fifth. There is no `protect_credential_table`
-- that only adds the resolve policy to a table somebody forgot to protect,
-- because a substrate with two entry points for one table is a substrate where
-- the one nobody reads is the one that ships.
--
--   select cafaye.protect_credential_table('api_keys', 'token_digest');
--
-- The digest column is named in the migration and nowhere else, and naming it
-- IS the declaration: it is the column whose value a caller must already hold to
-- resolve a credential on this table, and the README says what makes a value
-- suitable (it has to be unguessable, or this mechanism is the wrong one).
create or replace function cafaye.protect_credential_table(
  p_table regclass,
  p_digest_column text,
  p_login_role text default null
)
returns void
language plpgsql
as $caf$
declare
  login_role text := coalesce(p_login_role, current_user || '_app');
  base text;
  qual text;
begin
  -- 0. THE COLUMN NAME IS A BARE IDENTIFIER. `%I` quotes it, so this is not
  --    about injection; it is about a typo becoming a policy nobody notices,
  --    which is the failure a database-boundary file exists to make loud.
  if p_digest_column is null
     or p_digest_column !~ '^[A-Za-z_][A-Za-z0-9_$]*$' then
    raise exception 'cafaye.protect_credential_table(%, %): the digest column must be one bare column name, and % is not one.', p_table, coalesce(p_digest_column, '<null>'), coalesce(p_digest_column, 'null')
      using errcode = 'undefined_column',
            hint = 'The second argument is the column a caller presents to resolve a credential on this table — `token_digest` for a hashed API key, `token` for a hashed invitation. It is a column of this table and nothing else.';
  end if;

  -- 1. AND IT EXISTS, before `protect_table` runs, so a misspelt column does not
  --    half-install a table.
  if not exists (
    select 1 from pg_attribute
    where attrelid = p_table and attname = p_digest_column and not attisdropped
  ) then
    raise exception 'cafaye.protect_credential_table(%, %): the table has no % column.', p_table, login_role, p_digest_column
      using errcode = 'undefined_column',
            hint = 'A credential table resolves by the column a caller presents. Name the one this table has; if it has none, this table is not resolved by an unguessable value and does not want this mechanism.';
  end if;

  -- 2. THE SAME PROTECTION AS EVERY OTHER ACCOUNT-SCOPED TABLE. This calls the
  --    other function rather than repeating it, so a fix to one is a fix to both
  --    and there is no second copy of ENABLE/FORCE to drift.
  perform cafaye.protect_table(p_table, p_login_role);

  base := (select c.relname from pg_class c where c.oid = p_table::regclass::oid);

  -- 3. THE INDEX ON THE DIGEST COLUMN, for the reason `protect_table` creates one
  --    on `account_id`: a policy is a filter on every candidate row, and this one
  --    filters on the column the resolution query is keyed by.
  execute format('create index if not exists %I on %s (%I)',
                 base || '_cafaye_' || p_digest_column || '_idx', p_table, p_digest_column);

  -- 4. ONE POLICY, FOR ONE COMMAND, FOR THE TWO NAMED ROLES, WITH THE PREDICATE
  --    THE CALLER'S OWN QUERY CARRIES.
  --
  --    `for select` and nothing else is what makes item 2 of "what it cannot do"
  --    true: there is no `insert`/`update`/`delete` counterpart, so a resolution
  --    session meets the ordinary account policies on the write side and a NULL
  --    identity there is `42501`.
  --
  --    The `(select ...)` wrapping is the same InitPlan discipline as every other
  --    policy in this file, for the same measured reason: 1 call wrapped against
  --    5 bare on five rows, measured on this kit's Postgres and written down in
  --    the header of this file rather than re-taken per run.
  --    `TestEveryHelperCallInAPolicyIsWrapped` holds the wrapped SPELLING on every
  --    run; the Rust `tests/tenancy_test.sh` that counted the calls is gone with
  --    the rest of `tests/`, and nothing counts them.
  qual := format('%I = (select cafaye.current_credential_digest())', p_digest_column);

  --    DROP before CREATE, so a re-run converges and a corrected qualifier cannot
  --    survive one.
  execute format('drop policy if exists %I on %s', base || '_cafaye_resolve', p_table);
  execute format('create policy %I on %s for select to %I, %I using (%s)',
                 base || '_cafaye_resolve', p_table, current_user, login_role, qual);

  -- 5. USAGE on `cafaye`, for the same reason `protect_table` grants it: without
  --    it the login role cannot resolve `cafaye.begin_credential/1` and every
  --    resolution fails with `permission denied for schema cafaye`.
  if to_regnamespace('cafaye') is not null then
    execute format('grant usage on schema cafaye to %I', login_role);
  end if;
end
$caf$;

-- WHICH TABLES CAN BE READ WITHOUT AN ACCOUNT. The audit for item 5 above: one
-- query, and not a code search.
--
-- It reads the POLICIES rather than a list, for the reason the sweep reads
-- `pg_class`: a table this function could only know about because
-- `protect_credential_table` created it would be satisfied by a mechanism that
-- records nothing. The policy name and the digest reference are both required,
-- so a hand-written policy that merely mentions the digest does not appear here
-- and a policy named like this one with a different qualifier does not either.
--
-- It answers the SCOPE question and only that one. It cannot answer "who
-- resolved what", and this file does not pretend otherwise: the digest is
-- transaction-local and gone by the time anything could look at it.
--
-- NOTHING HERE READS A DEPARSED EXPRESSION, AND THE REASON IS THE READER.
-- `pg_get_expr` is a pretty-printer, and it drops the schema from a name the
-- READER can resolve — which is that reader's `search_path`, and `"$user"` is a
-- search_path entry. So the policy this file writes deparses as
--
--     (token_digest = ( SELECT cafaye.current_credential_digest() AS …))
--
-- for a role with no schema of its own, and as
--
--     (token_digest = ( SELECT current_credential_digest() AS …))
--
-- for the role named after this platform, whose `"$user"` schema IS `cafaye`.
-- An audit written against that text therefore returned NOTHING in that session
-- and the whole answer in every other one — same database, same policy — and
-- "no table here can be resolved without an account" is exactly what an operator
-- auditing credentials as the cluster's own admin role would then be told.
-- `tests/tenancy_test.sh` used to measure both roles on every run and require the
-- answers to be equal. It is gone with the rest of `tests/`, and NOTHING REPLACES
-- IT — which is the one honest gap in this paragraph, so it is stated rather than
-- rounded off. The query below reads `pg_depend` OIDs rather than a deparsed
-- expression, so there is no spelling for a `search_path` to change and equal
-- answers are a property of the MECHANISM rather than a comparison somebody runs;
-- "see WHAT THAT COSTS" is where the two extra catalogs and the one dependency
-- join that buys are written down. That is a real weakening of what is checked
-- per run and it is the price of deleting the script that checked it.
--
-- The paragraph this one replaces warned that "an audit query that returns an
-- empty column forever looks exactly like an audit query that found nothing". The
-- trap was never the regexp's strictness; it was reading a spelling at all. So
-- the two facts this query needs are read from DEPENDENCIES, which are OIDs:
--
--   * `cafaye.current_credential_digest()` is called by the policy's own `using`
--     expression, and `CREATE POLICY` records that in `pg_depend` with
--     `classid = pg_policy` and `refclassid = pg_proc`. Matching it through
--     `pg_proc` JOIN `pg_namespace` ON `proname` is by OID rather than by
--     spelling, so the answer is the same for every role and every `search_path`.
--   * the COLUMN it is compared against is a reference to a column of the
--     policy's OWN table, recorded as a `pg_depend` row onto that table with the
--     column's `attnum` in `refobjsubid`. Measured on 16.15 and 17: one `pg_proc`
--     row with `deptype = 'n'`, one `pg_class` row with `refobjsubid > 0`, and
--     one `pg_class` row with `refobjsubid = 0` and `deptype = 'a'` — the last
--     is the whole relation, which is why the column join requires
--     `refobjsubid > 0` rather than joining the table.
--
-- WHAT THAT COSTS, all of it, so none of it is a surprise:
--
--   1. Two catalogs more (`pg_proc`, `pg_depend`) and one dependency join. This
--      is a query a human runs once; it is sub-millisecond per row over a whole
--      database and nothing calls it per request.
--   2. It depends on `CREATE POLICY` recording dependencies — which is what
--      stops a policy outliving the function it calls, so the fact is the
--      catalog's rather than a convention of this file. Measured on 16.15 and
--      17.x; NOT measured on 15, kit's floor, because no 15 image was available
--      on this machine and an unmeasured version is not a verified one.
--   3. `pronargs = 0` is part of the match. The substrate's digest function takes
--      no arguments, and an overload of the same name is not that function.
--   4. A qualifier naming MORE THAN ONE column of its own table cannot be
--      narrowed to one, because nothing structural says which of them the digest
--      is compared to. That row is still reported, with `'(unresolved)'` in the
--      column: a table named with an unresolved column is an audit finding, and a
--      table absent from the audit is the silence this function exists to
--      prevent. The deparsing version reported the same policy with a NULL
--      column, which is neither an answer nor a complaint.
create or replace function cafaye.credential_tables()
returns table (table_schema text, table_name text, digest_column text)
language sql
stable
as $caf$
  -- (1) The digest function, by OID. `pronargs = 0` is part of the identity and
  --     not decoration — see cost 3 above.
  with digest_fn as (
    select fn.oid
      from pg_proc fn
      join pg_namespace fns on fns.oid = fn.pronamespace
     where fns.nspname = 'cafaye'
       and fn.proname = 'current_credential_digest'
       and fn.pronargs = 0
  ),

  -- (2) The policies in the substrate's own naming that call it, with the columns
  --     of THEIR OWN table that the call sits beside. `cd.refobjid = pol.polrelid`
  --     is what keeps a qualifier that reads another table's column out: the
  --     audit is about what THIS table resolves by.
  resolve as (
    select pol.oid as policy_oid,
           pol.polrelid,
           att.attname as column_name
      from pg_policy pol
      join pg_depend fd
        on fd.classid = 'pg_policy'::regclass
       and fd.objid = pol.oid
       and fd.refclassid = 'pg_proc'::regclass
       and fd.deptype = 'n'
      join digest_fn df on df.oid = fd.refobjid
      left join pg_depend cd
        on cd.classid = 'pg_policy'::regclass
       and cd.objid = pol.oid
       and cd.refclassid = 'pg_class'::regclass
       and cd.refobjid = pol.polrelid
       and cd.refobjsubid > 0
      left join pg_attribute att
        on att.attrelid = pol.polrelid
       and att.attnum = cd.refobjsubid
       and not att.attisdropped
     where pol.polname like '%!_cafaye!_resolve' escape '!'
       and pol.polcmd = 'r'
  )

  -- (3) One row per policy. The column is reported only when the qualifier names
  --     exactly one of the table's own columns, and `'(unresolved)'` otherwise —
  --     cost 4 above. `min()` is never the answer; it is what a single-column
  --     qualifier reduces to.
  select n.nspname::text,
         c.relname::text,
         case when g.columns_named = 1 then g.column_name
              else '(unresolved)' end
    from (select policy_oid,
                 polrelid,
                 count(distinct column_name) as columns_named,
                 min(column_name)            as column_name
            from resolve
           group by policy_oid, polrelid) g
    join pg_class c on c.oid = g.polrelid
    join pg_namespace n on n.oid = c.relnamespace
   order by n.nspname, c.relname
$caf$;

-- PROTECT ONE ACCOUNT-SCOPED TABLE. The only thing a migration calls.
--
--   select cafaye.protect_table('assets');                 -- the owner's role
--   select cafaye.protect_table('assets', 'darkroom_app'); -- explicit login role
--
-- Everything it does is required and everything it does is idempotent. There is
-- no `protect_table_lite`, and there is no way to ask it for a policy without
-- FORCE, because a template that offers the weaker version is a template the
-- weaker version gets chosen from.
create or replace function cafaye.protect_table(
  p_table regclass,
  p_login_role text default null
)
returns void
language plpgsql
as $caf$
declare
  login_role text := coalesce(p_login_role, current_user || '_app');
  qual text := 'account_id = (select cafaye.current_account_id())';
  base text;
  seq_name text;
begin
  -- 0. THE LOGIN ROLE HAS TO EXIST, and saying so by name is the difference
  --    between a diagnosable failure and a confusing one. Postgres reports a
  --    missing role in `create policy` as `role "courier_app" does not exist`,
  --    six lines into a function, with the table named by the context and the
  --    fix not. This is the check that turns that into one sentence.
  if not exists (select 1 from pg_roles where rolname = login_role and rolcanlogin) then
    raise exception 'cafaye.protect_table(%, %): % is not a LOGIN role on this cluster.', p_table, login_role, login_role
      using errcode = 'undefined_object',
            hint = 'kit''s cluster provisions <service> and <service>_app. Either the _app role has not been provisioned — this service was added to KIT_POSTGRES_DATABASES before it existed, and the init script only runs on a fresh volume — or pass the login role explicitly as the second argument.';
  end if;

  -- 1. THE COLUMN, OR STOP. A table with no `account_id` cannot be scoped by
  --    account, and protecting it anyway would create policies whose predicate
  --    cannot be evaluated — which Postgres resolves as "not true", i.e. a table
  --    nobody can read, which is a much harder failure to diagnose than this one.
  if not exists (
    select 1 from pg_attribute
    where attrelid = p_table and attname = 'account_id' and not attisdropped
  ) then
    raise exception 'cafaye.protect_table(%, %): the table has no account_id column, so there is nothing to scope it by.', p_table, login_role
      using errcode = 'undefined_column',
            hint = 'Either the column is spelled something else — write the policy yourself and name the fleet''s key — or this table is not account-scoped and should not be protected. A service holding no customer rows is core''s honest zero and needs none of this.';
  end if;

  base := (select c.relname from pg_class c where c.oid = p_table::regclass::oid);

  -- 2. THE INDEX, IN THE SAME FUNCTION, BECAUSE A POLICY IS A FILTER ON EVERY
  --    ROW AND AN UNINDEXED ONE IS A SEQUENTIAL SCAN. Postgres evaluates the
  --    policy against each candidate row, so an account-scoped table read
  --    through its primary key still has to filter on `account_id`, and without
  --    this index that is a full scan behind a primary-key lookup. Named
  --    deterministically so a re-run converges rather than making a second one.
  execute format('create index if not exists %I on %s (account_id)', base || '_cafaye_account_id_idx', p_table);

  -- 3. REVOKE FROM PUBLIC BEFORE ENABLING. A table's default privileges come
  --    from its owner and from whatever has been granted on it; PUBLIC is not
  --    among them unless somebody granted it. Revoking is one statement, and it
  --    means the answer to "who can read this" is not "whoever the last
  --    migration remembered".
  execute format('revoke all on table %s from public', p_table);

  -- 4. ENABLE, THEN FORCE. Two statements, and the second is the one that is in
  --    nobody's blog post.
  --
  --    Without FORCE a table's OWNER is exempt from its policies. Every service
  --    in this fleet runs its migrations as its own role, so without FORCE the
  --    role that owns the table reads every account's rows while the policies
  --    read as though they were in place. `pg_class.relforcerowsecurity` is the
  --    only catalog that says otherwise, and `isolation.sql` asserts it by
  --    running the denials AS THE OWNER — which is the only assertion that
  --    cannot be satisfied by a policy that exists.
  execute format('alter table %s enable row level security', p_table);
  execute format('alter table %s force row level security', p_table);

  -- 5. FOUR POLICIES, ONE PER COMMAND, ALL NAMED, FOR TWO NAMED ROLES.
  --
  --    Naming the roles with `to <owner>, <login>` rather than leaving it to PUBLIC
  --    is not tidiness: an unnamed policy is evaluated for every role including the
  --    ones that should never reach the table, and the role filter is what stops
  --    the evaluation early. It is also the first thing Supabase's guide tells you
  --    to do and the first thing everybody skips.
  --
  --    The OWNER is in the list, and that is not a convenience. FORCE removes the
  --    owner's exemption, so without an owner policy the owner reads NOTHING from
  --    a protected table — no rows, including its own — and a service that runs
  --    background work and migrations as that role discovers it by seeing empty
  --    result sets. Naming it means the owner's reads are ACCOUNT-SCOPED rather
  --    than absent: a migration sets an identity and reads exactly one account's
  --    rows, which is the same rule every other request obeys and the only version
  --    of "the owner may read" that is not "the owner may read everything".
  --
  --    One policy per command rather than one `for all` is what keeps a later
  --    grant additive — a policy added for one command cannot widen another. The
  --    `for all` shape with `using (true)` and `with check (...)` is the one that
  --    silently permits an INSERT nobody meant to permit, and it is the shape
  --    every "permissive RLS policy" lint is written to flag.
  --
  --    `insert` has no `using` — an INSERT has no existing row to filter — and
  --    its `with check` is what stops a row being created in somebody else's
  --    account, which is the write-side twin of the read denial.
  --
  --    DROP before CREATE so the function is re-runnable, and so a policy whose
  --    definition has been corrected here cannot survive a re-run.
  execute format('drop policy if exists %I on %s', base || '_cafaye_select', p_table);
  execute format('drop policy if exists %I on %s', base || '_cafaye_insert', p_table);
  execute format('drop policy if exists %I on %s', base || '_cafaye_update', p_table);
  execute format('drop policy if exists %I on %s', base || '_cafaye_delete', p_table);

  execute format('create policy %I on %s for select to %I, %I using (%s)', base || '_cafaye_select', p_table, current_user, login_role, qual);
  execute format('create policy %I on %s for insert to %I, %I with check (%s)', base || '_cafaye_insert', p_table, current_user, login_role, qual);
  execute format('create policy %I on %s for update to %I, %I using (%s) with check (%s)', base || '_cafaye_update', p_table, current_user, login_role, qual, qual);
  execute format('create policy %I on %s for delete to %I, %I using (%s)', base || '_cafaye_delete', p_table, current_user, login_role, qual);

  -- 6. AND THE LOGIN ROLE GETS DML AND NOTHING ELSE. No ownership, no DDL, no
  --    CREATE on the schema, no ability to change a policy. A grant that gives a
  --    login role more than it needs is a grant that has to be re-examined every
  --    time somebody adds a privilege, and this is the one place that list lives.
  --
  --    USAGE on `cafaye` is in the list and is not optional: without it the login
  --    role cannot resolve `cafaye.begin_account/1` at all, so the first thing a
  --    service does after adopting this is a `permission denied for schema
  --    cafaye` on every request.
  --
  --    USAGE ON A SEQUENCE is in the list for the same reason and it is the one
  --    that is INVISIBLE until a service writes a row. A table's INSERT reads its
  --    sequence before it writes the row, and a sequence is a separate object with
  --    its own grants — the table grant above does not carry it. Without this line
  --    a protected table with a `bigserial` column is UNINSERTABLE by the login
  --    role:
  --
  --        ERROR:  permission denied for sequence notification_preferences_id_seq
  --
  --    which is SQLSTATE 42501 — the code the policies themselves raise. An
  --    assertion that checks the code alone cannot tell a working boundary from a
  --    table nobody can write to. `templates/database/tenancy/isolation.sql`'s
  --    `cafaye_observed` returns `sqlstate` and nothing else, so it is exposed to
  --    exactly that; what has kept it out of reach is that its probe table is
  --    `id uuid … default gen_random_uuid()`, which is why 39 assertions have been
  --    green through this defect. `reports/substrate-sequence-grant/` is where the
  --    message is required, and it is where the two are told apart.
  execute format('grant usage on schema public to %I', login_role);
  if to_regnamespace('cafaye') is not null then
    execute format('grant usage on schema cafaye to %I', login_role);
  end if;
  execute format('grant select, insert, update, delete on table %s to %I', p_table, login_role);

  -- 7. AND USAGE ON EVERY SEQUENCE THIS TABLE'S OWN COLUMNS OWNED, AND NO OTHER.
  --
  --    `USAGE` is the whole grant and it is the whole need: it is what `nextval`
  --    and `currval` check, so it is the minimum that makes an INSERT into a
  --    serial column work. It is deliberately NOT `select` (which would let the
  --    login role read `last_value` and reason about how many rows other tables
  --    hold) and NOT `update` (which would let it `setval` and forge ids). A
  --    blanket `grant all on all sequences in schema public` also makes the
  --    insert work, and hands the same role control of every sequence in the
  --    database — including the ones behind tables it must not touch. The
  --    narrowing is the point of the loop.
  --
  --    FOUND IN THE CATALOG, NOT BY NAME. `<column>_id_seq` is this migration's
  --    convention and is not the next service's; a table with two serial columns
  --    has two sequences and neither is the table's; `serial4`, `bigserial`,
  --    `identity` and a hand-written `DEFAULT nextval(…)` differ in spelling and
  --    in nothing else. `pg_depend` is the edge Postgres itself records for all of
  --    them — `deptype` `a` for `serial` and `i` for `identity` — so reading it
  --    covers every spelling rather than the one this migration happened to use.
  --    The loop deliberately does NOT filter on `deptype`.
  --
  --    MEASURED, because it was the premise this line was written under and the
  --    measurement says otherwise: on PostgreSQL 17.11 and 18.6 a protected table
  --    whose id is `generated always as identity` IS insertable by a role holding
  --    no privilege on the sequence at all, while that same role's explicit
  --    `nextval()` on the sequence is refused — the identity value is fetched on a
  --    path with no ACL check. So the `bigserial` case was the lockout and this one
  --    was not, and the grant is here anyway for two reasons that do not depend on
  --    that: it is `USAGE` on the table's OWN sequence and nothing wider, and a
  --    substrate whose correctness rests on an undocumented executor detail is one
  --    minor version from the same lockout. Do not read this loop as a claim that
  --    `identity` was broken; read it as a claim that this file does not have to
  --    be re-derived per spelling.
  --
  --    `FOR … IN SELECT` rather than `SELECT … INTO`, so an empty result set is a
  --    loop that does not run rather than a `no rows` exception. Most tables in
  --    any service have no serial column at all, and `protect_table` already
  --    refuses a table it cannot scope by name — a sequence loop that RAISED on
  --    the tables that need no loop would be a quieter bug than the one it fixes.
  --
  --    Schema-qualified, because a bare relation name in a `GRANT` is resolved
  --    through the CALLER's `search_path` — and the caller of a migration is a
  --    role whose `search_path` this function does not control.
  for seq_name in
    select format('%I.%I', n.nspname, s.relname)
      from pg_class s
      join pg_namespace n on n.oid = s.relnamespace
      join pg_depend d on d.classid = 'pg_class'::regclass
                       and d.refclassid = 'pg_class'::regclass
                       and d.objid = s.oid
                       and d.refobjid = p_table::regclass
      join pg_attribute a on a.attrelid = d.refobjid
                         and a.attnum = d.refobjsubid
     where s.relkind = 'S'
  loop
    execute format('grant usage on sequence %s to %I', seq_name, login_role);
  end loop;
end
$caf$;

-- SWEEP: every account-scoped table in the schemas THIS SUBSTRATE OWNS,
-- protected or not.
--
-- Returns one row per table that is NOT, so a caller can report all of them
-- rather than the first. It returns rather than raising because a caller that
-- gets rows can print them, and a caller that gets an exception has to parse the
-- message. This is the query that answers "is this service isolated?".
--
-- THE COLUMN IS THE DEFINITION OF ACCOUNT-SCOPED, and it is a definition rather
-- than a list because a list is a thing to remember. A table that grows an
-- `account_id` column and no policies appears here on the next run, which is the
-- moment before it matters.
--
-- THE SCOPE IS THE SCHEMAS THE SUBSTRATE WAS APPLIED IN, and that is the whole
-- difference between this function and a scan of every table in the database.
-- The property being asserted is "every account-scoped table THE SERVICE
-- INSTALLED is protected", and its input is the set of tables the substrate was
-- applied to — a whole-database scan is one way to compute that set, and it is
-- the way that breaks every adopter.
--
-- MEASURED, on identity: its test helper builds a private fixture schema per
-- test by cloning tables with `LIKE … INCLUDING ALL`, and `LIKE` does not copy
-- row-level security. Each fixture therefore carries an `account_id` column and
-- no policies. A whole-database sweep named every one of them, so a correct
-- database produced a red proof — and the count moved with how many neighbouring
-- tests were mid-flight (5 on one run, 21 on the next), which is what says it
-- was a reach rather than a defect. A sweep that cannot stay green on a correct
-- database is a sweep nobody reads, and one that goes red for somebody else's
-- fixtures is a sweep an adopter has to work around, which is worse.
--
-- THE SCOPE IS DERIVED, NOT RECORDED, and the difference is the reason an
-- adopter needs no migration to get the fix. There is no registry table, because
-- a registry would have to be created by the NEW substrate and every database
-- that has applied the OLD one does not have it — so the fix would only arrive
-- with a migration, and the proofs it is meant to repair are the thing that
-- decides whether the migration is needed. Instead the scope is read back out of
-- what the substrate ALREADY wrote: `protect_table` names its policies
-- `<table>_cafaye_<command>` and creates an index beside them, so every schema
-- the substrate was applied in is discoverable from the catalog, with nothing
-- new to install and nothing to remember to update.
--
-- Extension-owned tables are excluded (`pg_depend.deptype = 'e'`): pgvector,
-- PostGIS and friends ship their own tables with their own grants, and reporting
-- a table this substrate cannot protect would train the reader to ignore the
-- sweep.
--
-- TEMPORARY TABLES ARE NOT EXCLUDED, and that is deliberate. A temp table with an
-- `account_id` column and no policies is genuinely unprotected, it is exactly
-- the shape a service's test fixture has, and `isolation.sql` plants one on
-- purpose as its sweep's control. It is safe to INCLUDE this file's own session's
-- temporary schema for the reason a fixture schema is not: temporary objects are
-- per-session, so no other session — not another test, not another service — can
-- put a table in scope by creating one. `pg_temp` is the alias; the real schema
-- is named `pg_temp_N`, which is why this is a prefix test and not an equality.
create or replace function cafaye.unprotected_tables()
returns table (table_schema text, table_name text, why text)
language sql
stable
as $caf$
  with scoped(nspname) as (
    -- (1) The substrate's own schema. It holds functions rather than rows, so
    --     this contributes nothing today; it is here so that a table added to
    --     `cafaye` later cannot be quietly unowned by the exclusion this query
    --     used to carry.
    select 'cafaye'::name
    -- (2) This session's temporary schema, where `isolation.sql` plants its
    --     control table. Per-session, so it can never carry another session's.
    union
    select n.nspname
      from pg_namespace n
     where n.nspname = 'pg_temp' or left(n.nspname, 8) = 'pg_temp_'
    -- (3) Every schema the substrate was applied IN, read back from the policies
    --     `protect_table` wrote. A schema qualifies when it holds at least one
    --     table this substrate protected, which is what makes the scope the
    --     service's own tables rather than a list somebody maintains: a new
    --     account-scoped table added NEXT MONTH to a schema already in scope is
    --     named on the next run, with nothing having been updated anywhere.
    union
    select n.nspname
      from pg_namespace n
      join pg_class c on c.relnamespace = n.oid
      join pg_policy p on p.polrelid = c.oid
     where c.relkind = 'r'
       and p.polname ~ '_cafaye_(select|insert|update|delete)$'
  )
  select n.nspname::text,
         c.relname::text,
         case
           when not c.relrowsecurity then 'row level security is not enabled'
           when not c.relforcerowsecurity then 'row level security is not FORCED, so the table owner bypasses it'
           when not exists (select 1 from pg_policy p where p.polrelid = c.oid) then 'row level security is enabled with no policy, so every row is hidden'
           else ''
         end
  from pg_class c
  join pg_namespace n on n.oid = c.relnamespace
  left join pg_depend d on d.objid = c.oid and d.deptype = 'e'
  where c.relkind = 'r'
    and d.objid is null
    and n.nspname in (select s.nspname from scoped s)
    and exists (
      select 1 from pg_attribute a
      where a.attrelid = c.oid and a.attname = 'account_id' and not a.attisdropped
    )
    and (
      not c.relrowsecurity
      or not c.relforcerowsecurity
      or not exists (select 1 from pg_policy p where p.polrelid = c.oid)
    )
  order by n.nspname, c.relname
$caf$;

-- ---------------------------------------------------------------------------
-- A login role that OWNS an account-scoped table can switch that table's
-- policies off, and no amount of FORCE stops it: FORCE is about the owner being
-- SUBJECT to policies, not about the owner being unable to REMOVE them. So the
-- second half of the design is a role that owns nothing, and that claim has
-- nothing to sweep — it is a fact about one role and one table, so it is asserted
-- about the role the application actually logs in as rather than generalised into
-- a query that would have to guess which role that is. `isolation.sql` asserts
-- it three ways, and the two that matter are the ones no catalog reports: the
-- login role is refused `ALTER TABLE ... DISABLE ROW LEVEL SECURITY`, and it is
-- refused `DROP POLICY`.
--
-- WHAT THIS FILE DOES NOT PROVE, because every item below is a way the boundary
-- can be right in the database and absent from the service.
--
--   * That the service SETS the identity. `begin_account/1` is one call in one
--     place, and nothing here can tell a service that never calls it from one
--     that calls it in every request. A service that forgets reads zero rows,
--     which is safe and extremely visible, so this is a diagnosis rather than a
--     leak.
--
--   * That the service's QUERIES still carry their own `where account_id = ?`.
--     They should. RLS is defence in depth, not a licence to delete the
--     predicate: the predicate is what makes the query indexable, and it is what
--     a mistake in the policy expression gets caught by. core's
--     `schemas/tenant-isolation.schema.json` still requires the predicate and this
--     file does not change that.
--
--   * That the login role is not a MEMBER of the owner role. The cluster grants
--     `<service>_app` TO `<service>` so a migration and `tests/isolation_test.sh`
--     can impersonate the weaker role; the reverse membership would undo the
--     entire design and is worth asserting in the services that adopt this.
--
--   * That a TABLE is account-scoped. The sweep defines that as "has an
--     `account_id` column", which is the fleet's own vocabulary (D7), and it
--     misses a service that spells it `tenant_id` and protects it by hand. A
--     service with no account-scoped tables at all is the honest zero, and
--     core's `tenancy.honest-zero` finding is what says so out loud.
--
--   * That a SCHEMA the substrate was never applied to holds no account-scoped
--     table. The sweep's scope is the schemas it protected, plus its own and
--     `pg_temp`, so customer rows kept in a schema no `protect_table` call ever
--     named are outside it. That is the trade, and it is a deliberate one: the
--     alternative reports every other session's test fixture, which is how a
--     correct database ends up with a red proof and how an adopter ends up
--     carrying a workaround for a bug that was upstream. A service whose account
--     tables span schemas the substrate has not met is a service that has not
--     protected them, and `protect_table` is one call per table.
-- ---------------------------------------------------------------------------

-- ============================================================================
-- DARKROOM'S CALLS. Everything above this line is kit's, byte for byte.
-- ============================================================================

-- The two account-scoped tables. For each one `protect_table` creates: ENABLE,
-- FORCE, four per-command policies (select/insert/update/delete) each naming the
-- login role, an index on `account_id`, USAGE on `cafaye`, and DML to the login
-- role.
--
-- The login role is `darkroom_app` — the role kit's
-- `templates/compose/postgres/initdb/10-cluster.sh` provisions for every entry in
-- `KIT_POSTGRES_DATABASES` (`GRANT :"app" TO :"svc"`), on the SHARED cluster. It
-- is named HERE rather than defaulted, because the default is NULL and a NULL
-- login role protects the table for nobody: the policies would be written with no
-- role in their `to` clause and `darkroom_app` would hold no grant. Spelling it
-- means a typo is a migration failure rather than a table that reads as protected
-- and is unreachable.
select cafaye.protect_table('assets', 'darkroom_app');
select cafaye.protect_table('asset_variants', 'darkroom_app');

-- ============================================================================
-- DARKROOM'S GRANTS TO THE LOGIN ROLE, on the two tables no policy protects.
--
-- `protect_table` grants DML to `darkroom_app` as part of protecting a table, so
-- the two calls above are all the grants the account-scoped tables need. These two
-- are the rest, and they are HERE rather than in a later packet because without
-- them the application cannot write anything, which is a fact measured on the
-- shared cluster rather than reasoned:
--
--   $ psql … -Atc "select has_table_privilege('darkroom_app','outbox_events','INSERT')"
--   false
--
-- `outbox_events` and `idempotency_keys` have no `account_id` and so no policy and
-- no grant, and `protect_table` is the only thing in the substrate that grants
-- anything. An application that adopts the tenancy templates and does not add
-- these two lines comes up with a login role that can read its two account-scoped
-- tables and cannot write a single row — which fails at the first upload rather
-- than at boot, because `Open` pings successfully and the first INSERT is where it
-- stops.
--
-- DML AND NOT OWNERSHIP, AND NOT `all`. `all` would include TRUNCATE, REFERENCES
-- and TRIGGER on tables the tenant boundary has no opinion about. The rule is the
-- one `10-cluster.sh` states for the whole cluster: the login role is granted "DML
-- per account-scoped table … and nothing else: no ownership, no DDL, no ability to
-- alter a policy." `pgtest`'s `TestTheLoginRoleCannotAlterItsOwnPolicies` asserts
-- the negative of that on the protected tables, and TRUNCATE on `assets` is the
-- case that proves the grant list is narrow rather than merely unfavourable.
--
-- NO `select` GRANT IS REDUNDANT HERE but not in general: these two tables hold
-- EVERY tenant's rows and are read by the publisher rather than by a request, so
-- the SELECT grant is what lets the publisher drain the queue.
grant select, insert, update, delete on outbox_events     to darkroom_app;
grant select, insert, update, delete on idempotency_keys  to darkroom_app;

-- WHY THERE IS NO SEQUENCE GRANT. darkroom mints every id in Go — `assets.id`,
-- `outbox_events.id` and the variant ids are UUIDs the service generates, and
-- `assets.storage_key` is written by `storagekey.GenerateOriginal` /
-- `storagekey.GenerateVariant`, the only two functions in that package that mint
-- one (`FromStored` rehydrates a key out of a row). There is no `serial`, no
-- `identity` and therefore no sequence for the login role to need USAGE or
-- UPDATE on. `migrations/0001_assets.sql` is the place that would change and a
-- packet that adds a serial column has to add the grant here.

-- WHY THE DOWN BELOW IS COMMENTED OUT RATHER THAN EXECUTABLE.
--
-- darkroom's three applied migrations are PLAIN SQL with no up/down markers:
-- `migrations/README.md` applies them with `psql -f`, and the runner under this
-- tree (`migrations.Apply`) has no notion of a down half to look for. There
-- is therefore no marker here either, and that has one consequence which is worth
-- stating because it was measured rather than reasoned: appending an EXECUTABLE
-- down to this file meant `psql -f migrations/0004_account_isolation.sql` applied
-- the up half and then immediately undid it, leaving `relrowsecurity = false` on
-- both tables while `relforcerowsecurity = true` survived -- the one catalog
-- column that says the policies are supposed to bite, set on a table with no
-- policies at all. That is a database which reads as protected in `pg_class` and
-- reads as nothing in `pg_policies`, and it is precisely the state
-- `templates/database/tenancy/substrate.sql` says no lint in this fleet checks
-- for.
--
-- So the down is here, complete and reviewable, as SQL inside a comment. It is
-- not executed by applying this file, and the way to run it is to uncomment it.
-- The alternative -- inventing `-- +migrate` markers for this one file while the
-- three before it have none, and which nothing in this tree would read --
-- would leave the directory with two conventions and a migration runner that has
-- to know which is which.

-- ----------------------------------------------------------------------------
-- Down
--
-- REVERSIBLE, and it has to be written here because the substrate is a template
-- with no `down`: kit keeps one copy for the fleet and an adopter's down has to
-- name the adopter's tables. Reverse order, so the policies go before the table
-- they protect is handed back.
--
-- Each DROP POLICY names its policy rather than sweeping the table, and each is
-- `if exists`, because `protect_table` is documented as dropping and recreating
-- its own policies on a re-run: a down that assumed a fixed set of names would
-- fail the second time through.
--
-- The grants go too. Leaving `darkroom_app` holding DML on a table whose RLS is
-- off is the exact state this migration exists to prevent, and a down that unwound
-- the policies but not the grants would leave it behind.
-- ----------------------------------------------------------------------------

-- drop policy if exists assets_app_update          on assets;
-- drop policy if exists assets_app_delete          on assets;
-- drop policy if exists assets_app_insert          on assets;
-- drop policy if exists assets_app_select          on assets;
-- drop policy if exists asset_variants_app_update on asset_variants;
-- drop policy if exists asset_variants_app_delete on asset_variants;
-- drop policy if exists asset_variants_app_insert on asset_variants;
-- drop policy if exists asset_variants_app_select on asset_variants;

-- alter table assets         disable row level security;
-- alter table asset_variants disable row level security;

-- revoke all on table assets         from darkroom_app;
-- revoke all on table asset_variants from darkroom_app;
--
-- The two unprotected tables' grants go too, and for the same reason: leaving
-- darkroom_app holding DML on a table this migration no longer has an opinion
-- about is not a neutral state.
-- revoke all on table outbox_events    from darkroom_app;
-- revoke all on table idempotency_keys from darkroom_app;

-- The index `protect_table` created on each `account_id` goes with the policies.
-- Named here rather than dropped by a `where indexdef like ...` sweep, because a
-- sweep matches on a string this file does not own.
-- drop index if exists assets_account_id_rls_idx;
-- drop index if exists asset_variants_account_id_rls_idx;

-- The substrate's own objects last, so nothing above depends on them while it
-- runs. `cascade` is on `drop schema` and NOT on the functions, deliberately: the
-- functions live IN that schema, so `drop schema cascade` removes them, and naming
-- them separately first would be a second list to keep in step with kit's.
-- drop schema if exists cafaye cascade;
