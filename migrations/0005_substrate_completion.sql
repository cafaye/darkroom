-- 0005_substrate_completion.sql — the four substrate functions 0004 did not carry.
--
-- WHY THIS FILE EXISTS. `0004_account_isolation.sql` is kit's
-- `templates/database/tenancy/substrate.sql` copied verbatim, and it was copied
-- while the template held EIGHT functions. kit added four more the next day —
-- `anywaye.current_user_id`, `anywaye.begin_user`, `anywaye.protect_membership_table`
-- and `anywaye.membership_tables` (kit commit 537441e, 2026-10-05 13:04, "a CALLER
-- scope, so 'which accounts am I in' is expressible") — and darkroom's copy was
-- never compared against the template again. It stayed at eight, and nothing in
-- this repository said so. kit's `tests/substrate_copy_check.py` is what says so,
-- in four lines:
--
--   FAIL  MISSING function: anywaye.begin_user
--   FAIL  MISSING function: anywaye.current_user_id
--   FAIL  MISSING function: anywaye.membership_tables
--   FAIL  MISSING function: anywaye.protect_membership_table
--
-- WHAT IT CHANGES, MEASURED: nothing that runs. darkroom has no membership table
-- (the string `account_users` occurs in this tree once, inside a comment in 0004),
-- no Go code calls `anywaye.begin_user/1`, and no Go code calls
-- `anywaye.current_user_id/0`. The four functions are installed as definitions and
-- nothing is wired to them. That is the repair's whole value, and it is worth
-- being plain about its size: it makes the copy equal kit's template so that the
-- NEXT drift is a red checker rather than silence, and it means a packet that adds
-- a membership table — the thing the caller scope was built for — starts from a
-- complete substrate instead of discovering halfway through that its copy carries
-- eight of the fleet's twelve functions.
--
-- WHY A NEW MIGRATION AND NOT AN EDIT TO 0004. `migrations/README.md`: "Never edit
-- an applied migration." 0004 has run — on this cluster's darkroom database, in
-- every schema `pgtest` has ever made, and on every CI run — and editing a file
-- the database already has means the next deploy silently does nothing while the
-- repository and the database disagree about the schema. All four functions here
-- are `create or replace`, so this file applied to an 8-function database
-- converges to 12, and applied twice converges to the same 12.
--
-- WHERE THE BYTES CAME FROM, BECAUSE `kit.ref` CANNOT ANSWER THAT QUESTION.
-- darkroom's `kit.ref` pins d3d56f7 (2026-10-02 13:23), and
-- `templates/database/tenancy/substrate.sql` first appears in kit at e8605c9
-- (2026-10-02 19:23) — six hours AFTER the pin. The pinned ref therefore names no
-- version of this file at all, let alone one carrying the four functions; it names
-- the compose stack, which is what `docker-compose.yml` fetches it for. The blocks
-- below are kit's template bytes as the checker reads it — kit's working tree at
-- origin/master 46d08f2, template sha256
-- eaab2893825647495ee0db770841ac60fe855791b148423430ca44db1f8334c2, unchanged
-- since 537441e — copied with their commentary and not re-derived.
--
-- ORDER, AND THE ONE DEPENDENCY. The four blocks are in kit's order and no reorder
-- was needed: `protect_membership_table` calls `anywaye.protect_table`, which 0004
-- defines, and 0005 runs after 0004 because these filenames are the apply order.
-- Nothing else in the four references a function that is not a builtin or part of
-- 0004.
--
-- NO CALLS AT THE BOTTOM, AND WHY. 0004 ends with darkroom's two
-- `anywaye.protect_table(...)` calls; this file ends with definitions only.
-- `protect_membership_table` is called by the migration of a service that HAS a
-- membership table, and darkroom does not: there is no `account_users` table here,
-- and a call naming a table that does not exist would abort the migration. The
-- substrate is complete without a call; the call is a decision about darkroom's
-- schema that no table has asked for yet.

-- THE CALLER, AS A TRANSACTION-LOCAL IDENTITY. The third scope, and the reason
-- this file grew one.
--
-- WHAT WAS MISSING, MEASURED. An account boundary answers "which account is this
-- request standing in". It does not answer "which accounts is this PERSON a
-- member of" — and that is not a rare question, it is the first question a
-- product asks. `identity` answered it with `GET /v1/accounts`, and the query was
-- a two-table join on the membership table keyed by the caller's user id. The
-- policy on that table filters on `account_id = current_account_id()`, the caller
-- is not standing in an account yet — that is what makes them a caller rather
-- than a tenant — so the predicate is NULL, and the query that exists to
-- DISCOVER the caller's accounts returned none of them. The row was in the
-- database the whole time; `identity` answered `[]` and its product rendered
-- "you are not a member of any account" to a user who owned one.
--
-- WHY A THIRD SCOPE AND NOT A LOOSER POLICY. Every alternative was a way of
-- saying something false:
--
--   * drop the FORCE, or exempt the login role — that is not an account
--     boundary any more, it is a convention.
--   * let a caller read their own memberships without an account identity —
--     the policy still cannot tell WHICH caller is asking, so this reads every
--     membership in the fleet.
--   * a SECURITY DEFINER function that bypasses RLS — the mechanism kit's
--     MD24 already rejected for credentials, for the same reason.
--
-- So the substrate needed to be able to NAME THE CALLER, and naming a caller is
-- exactly what a GUC is for. This is `begin_account`'s twin: same
-- transaction-local lifetime, same NULL-clears, same "the service passes the
-- value it already computed" property.
--
-- WHAT IT IS NOT. It is not a session, not an authorisation and not an
-- identity claim: it says who the database is currently answering, and nothing
-- else. A service that set it from a request body would be handing the
-- database a caller's identity chosen by the caller — which is why the value is
-- the same `users.id` the service resolved the session to, and why this file's
-- twin for accounts warns at the same length.
create or replace function anywaye.current_user_id()
returns uuid
language plpgsql
stable
as $caf$
declare
  raw text;
begin
  -- `missing_ok = true` and the empty check are the two cases `begin_account`'s
  -- reader handles, copied rather than re-derived: an unset dotted GUC reads as
  -- set-to-empty, and a caller with no identity is a legitimate state.
  raw := current_setting('anywaye.user_id', true);
  if raw is null or raw = '' then
    return null;
  end if;
  return raw::uuid;
exception
  when invalid_text_representation then
    raise exception 'anywaye.user_id is set to % which is not a uuid', raw
      using errcode = '22023',
            hint = 'anywaye.begin_user/1 takes a uuid. A request that reaches the '
                   'database with a malformed user has a bug in whatever '
                   'authenticated it, and this is the cheapest place to find it.';
end
$caf$;

-- OPEN THE CALLER SCOPE, ONCE, TRANSACTION-LOCAL. Every word of
-- `begin_credential`'s contract applies here and is not restated: `is_local`
-- because every driver holds a pool, NULL clears rather than fails, and outside
-- an explicit transaction the setting expires at the end of the statement that
-- set it — so a caller that forgets the transaction reads zero rows rather than
-- somebody else's.
create or replace function anywaye.begin_user(p_user uuid)
returns void
language plpgsql
as $caf$
begin
  if p_user is null then
    perform set_config('anywaye.user_id', '', true);
    return;
  end if;
  perform set_config('anywaye.user_id', p_user::text, true);
end
$caf$;

-- PROTECT ONE MEMBERSHIP TABLE. The third member of this family, and it is the
-- first one whose job is to let a read THROUGH the account boundary rather than
-- to resolve something inside it.
--
-- A membership table is an ordinary account-scoped table with one extra fact to
-- carry: it is also the map FROM a person TO accounts. Those are different reads
-- and the ordinary policy can only do one of them. `protect_table`'s predicate
-- answers "which rows of this table belong to the account I am standing in" —
-- true, useless, and exactly wrong for the query that asks a signed-in person
-- which accounts they are in, because that query happens BEFORE any account is
-- being stood in. Without this function that query returns an empty list and
-- nothing anywhere says the rows are being withheld: the policy is present, the
-- columns are right, the join is correct, and the answer is `[]`.
--
-- So this function calls `protect_table` first — the write side, the `for select`
-- account read, ENABLE, FORCE, the `account_id` index and the role check all
-- come from there, not from a copy — and then adds ONE policy:
--
--   select anywaye.protect_membership_table('account_users', 'user_id');
--
-- `for select` only, for the same reason the credential policy is: a caller with
-- no account identity still meets the ordinary account policies on insert,
-- update and delete, so this grants DISCOVERY and not AUTHORSHIP. Being able to
-- see that you are a member of an account is not the ability to change it,
-- invite to it, or read a row in it.
--
-- The predicate is `<user_column> = (select anywaye.current_user_id())`, wrapped
-- in a scalar subquery for the InitPlan reason every other policy in this file
-- is, and it is NULL rather than true when the caller has no user identity — so
-- a worker that forgot `begin_user` reads zero memberships instead of all of
-- them. That is the whole safety property, and it is the same property the
-- account boundary has: absent an identity, nothing.
create or replace function anywaye.protect_membership_table(
  p_table regclass,
  p_user_column text,
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
  -- 0. THE COLUMN NAME IS A BARE IDENTIFIER, for the reason
  --    `protect_credential_table`'s check 0 gives: `%I` makes this not about
  --    injection, it is about a typo becoming a policy that quietly admits
  --    nothing instead of a migration that stops.
  if p_user_column is null
     or p_user_column !~ '^[A-Za-z_][A-Za-z0-9_$]*$' then
    raise exception 'anywaye.protect_membership_table(%, %): the user column must be one bare column name, and % is not one.', p_table, coalesce(p_user_column, '<null>'), coalesce(p_user_column, 'null')
      using errcode = 'undefined_column',
            hint = 'The second argument is the column on this table holding the member''s `users.id` — `user_id` in every service kit has shipped. It is a column of this table and nothing else.';
  end if;

  -- 1. AND IT EXISTS, before `protect_table` runs, so a misspelt column cannot
  --    half-install a table. The message quotes the COLUMN rather than the
  --    table, which `protect_credential_table`'s check 1 gets right for a
  --    credential and would get wrong here: `protect_credential_table(%, %)`
  --    interpolates the login role into the second `%` and the table into the
  --    third, which reads as though the role were the missing column. That is a
  --    pre-existing bug in the credential function's message, not something this
  --    one copies.
  if not exists (
    select 1 from pg_attribute
    where attrelid = p_table and attname = p_user_column and not attisdropped
  ) then
    raise exception 'anywaye.protect_membership_table(%, %): the table has no % column.', p_table, p_user_column, p_user_column
      using errcode = 'undefined_column',
            hint = 'A membership table is read by the column that names the member. Name the one this table has; if it has none, this table is not a membership table and does not want this mechanism.';
  end if;

  -- 2. THE SAME PROTECTION AS EVERY OTHER ACCOUNT-SCOPED TABLE, from the same
  --    function, so there is no second copy of ENABLE/FORCE to drift. This call
  --    also raises if the table has no `account_id`, which a membership table
  --    must have: it is the edge of the account graph, not a user-scoped
  --    resource that happens to mention accounts.
  perform anywaye.protect_table(p_table, p_login_role);

  base := (select c.relname from pg_class c where c.oid = p_table::regclass::oid);

  -- 3. THE INDEX ON THE USER COLUMN, for the reason `protect_table` indexes
  --    `account_id`: the policy is evaluated against every candidate row, and
  --    the query this exists for is a scan of the whole table looking for this
  --    caller's memberships. Without the index that scan is sequential however
  --    few rows it returns.
  execute format('create index if not exists %I on %s (%I)',
                 base || '_anywaye_' || p_user_column || '_idx', p_table, p_user_column);

  -- 4. ONE POLICY, FOR ONE COMMAND, FOR THE TWO NAMED ROLES, PREDICATED ON THE
  --    CALLER rather than on an account.
  qual := format('%I = (select anywaye.current_user_id())', p_user_column);

  --    DROP before CREATE, so a re-run converges and a corrected qualifier cannot
  --    survive one.
  execute format('drop policy if exists %I on %s', base || '_anywaye_members', p_table);
  execute format('create policy %I on %s for select to %I, %I using (%s)',
                 base || '_anywaye_members', p_table, current_user, login_role, qual);

  -- 5. USAGE on `anywaye`, for the reason the other two functions grant it:
  --    without it the login role cannot call `anywaye.begin_user/1` and every
  --    membership read fails with `permission denied for schema anywaye` — which
  --    reads as a permissions bug rather than as the missing GRANT that it is.
  if to_regnamespace('anywaye') is not null then
    execute format('grant usage on schema anywaye to %I', login_role);
  end if;
end
$caf$;

-- WHICH TABLES CAN BE READ WITHOUT AN ACCOUNT BECAUSE OF WHO IS ASKING. The
-- second kind of exemption, and the audit for it is here for the reason the first
-- one is: an exemption nobody can list is an exemption nobody notices has been
-- widened.
--
-- `credential_tables` exists because "no table here can be resolved without an
-- account" is a sentence an operator has to be able to check. Before
-- `protect_membership_table` there was exactly one way for a policy to stop
-- requiring an account, so one query covered the question. Now there are two, and
-- the honest form of that sentence is two queries — or one sweep that reports a
-- table readable under either rule, which is what an operator actually wants to
-- read on a fleet they did not write.
--
-- IT IS NOT `credential_tables` RENAMED. The two are different claims and
-- conflating them would be the same error this file keeps refusing: a membership
-- table resolved by a caller is not a table that can be resolved by anybody who
-- guesses a digest, and a credential table is not a table whose rows a signed-in
-- person may enumerate. So this reads the `_anywaye_members` policy and calls
-- `current_user_id`, and reports the USER column, which is the column whose value
-- the caller must already hold.
--
-- Everything else is `credential_tables`' query with two names changed, and that
-- is deliberate rather than lazy: the `pg_depend` reading is the part that took
-- the work. Deparse-and-regexp would have been five lines, and `credential_tables`
-- carries 40 lines of measured explanation of why it does not work — an audit
-- that spells a function name returns NOTHING as the cluster's own admin role and
-- the whole answer in every other session, so the same query and the same
-- substring rules are what make the two functions agree with each other.
create or replace function anywaye.membership_tables()
returns table (table_schema text, table_name text, user_column text)
language sql
stable
as $caf$
  -- (1) The caller function, by OID, with `pronargs = 0` as part of its identity.
  with user_fn as (
    select fn.oid
      from pg_proc fn
      join pg_namespace fns on fns.oid = fn.pronamespace
     where fns.nspname = 'anywaye'
       and fn.proname = 'current_user_id'
       and fn.pronargs = 0
  ),

  -- (2) The policies in the substrate's own naming that call it, with the columns
  --     of THEIR OWN table the call sits beside. Same two dependency joins, same
  --     `deptype`, same `refobjsubid > 0` narrowing of the column match.
  discover as (
    select pol.oid as policy_oid,
           pol.polrelid,
           att.attname as column_name
      from pg_policy pol
      join pg_depend fd
        on fd.classid = 'pg_policy'::regclass
       and fd.objid = pol.oid
       and fd.refclassid = 'pg_proc'::regclass
       and fd.deptype = 'n'
      join user_fn uf on uf.oid = fd.refobjid
      left join pg_depend ud
        on ud.classid = 'pg_policy'::regclass
       and ud.objid = pol.oid
       and ud.refclassid = 'pg_class'::regclass
       and ud.refobjid = pol.polrelid
       and ud.refobjsubid > 0
      left join pg_attribute att
        on att.attrelid = pol.polrelid
       and att.attnum = ud.refobjsubid
       and not att.attisdropped
     where pol.polname like '%!_anywaye!_members' escape '!'
       and pol.polcmd = 'r'
  )

  -- (3) One row per policy, with `'(unresolved)'` when the qualifier names more
  --     than one of the table's own columns — reported, not dropped, for the
  --     reason cost 4 of `credential_tables` above gives.
  select n.nspname::text,
         c.relname::text,
         case when g.columns_named = 1 then g.column_name
              else '(unresolved)' end
    from (select policy_oid,
                 polrelid,
                 count(distinct column_name) as columns_named,
                 min(column_name)            as column_name
            from discover
           group by policy_oid, polrelid) g
    join pg_class c on c.oid = g.polrelid
    join pg_namespace n on n.oid = c.relnamespace
   order by n.nspname, c.relname
$caf$;

-- WHY THE DOWN BELOW IS COMMENTED OUT RATHER THAN EXECUTABLE. `migrations/README.md`
-- and 0004 both carry the reason, and 0004 carries the measurement: these files run
-- under `psql -f`, and an executable down is applied by that same command right
-- after the up half, so an operator re-applying this file would install the four
-- functions and then immediately drop them — `psql` exits 0 and the database never
-- moved. There are no up/down markers in this directory and this file adds none.
--
-- The down is REVERSIBLE, and its limit is stated so nobody has to find it: no
-- table in darkroom carries a `_anywaye_members` policy — there is no membership
-- table at all — so dropping these four functions cannot strand a policy. A
-- database that HAD called `anywaye.protect_membership_table` would have to drop
-- each `_anywaye_members` policy before the function, and this comment is where that
-- reader should learn it. The order below is reverse-of-definition and is a reading
-- aid only: the cross-references among the four live inside string bodies
-- (`$caf$ ... $caf$`), which Postgres does not record as dependencies, so any drop
-- order succeeds.

-- ----------------------------------------------------------------------------
-- Down
-- ----------------------------------------------------------------------------

-- drop function if exists anywaye.membership_tables();
-- drop function if exists anywaye.protect_membership_table(regclass, text, text);
-- drop function if exists anywaye.begin_user(uuid);
-- drop function if exists anywaye.current_user_id();
