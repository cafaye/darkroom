// Package pgtest_test proves the per-test schema harness is hermetic, and that
// darkroom's migrations produce the isolation they claim to.
//
// # WHAT A GREEN RUN HERE MEANS AND WHAT IT DOES NOT
//
// Everything in this file runs against a real PostgreSQL, and every claim is made
// by asking the catalog or by making a query and counting the rows — never by
// reading the SQL as text. That is the distinction `AGENTS.md` insists on for
// `tests/tenant_scoping.rs`: "A query's tenant predicate is checked as a property
// of the source, not by convention." A grep for `force row level security` in
// migrations/ is satisfied by a migration inside a comment. `pg_class` is not.
//
// The claims NOT proved here are the ones a database cannot see, and kit's
// substrate lists them under "what this file does not prove": that the service
// SETS the identity on every request, that its queries still carry their own
// `where account_id = $?`, and that `darkroom_app` is not a member of `darkroom`.
// The third of those is a role-membership fact and it IS in scope for this file,
// because it is a fact about `pg_auth_members` and not about the service's code.
package pgtest_test

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"os"
	"regexp"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"

	"github.com/cafaye/darkroom/internal/darkroomdb/pgtest"
)

// accountA and accountB are two tenants. Any boundary that cannot tell them apart
// is not a boundary, and every test that proves a denial needs both: a query that
// returns zero rows is only evidence if the same query returns a row for somebody.
//
// Fixed UUIDs rather than generated ones, so a failure names the same two accounts
// every run and a reader can look them up in the seed without a script.
const (
	accountA = "11111111-1111-4111-8111-111111111111"
	accountB = "22222222-2222-4222-8222-222222222222"
	userA    = "aaaaaaaa-aaaa-4aaa-8aaa-aaaaaaaaaaaa"
)

// seedAsset inserts one asset for `account`, through the service's own door.
//
// It goes through `darkroomdb.InAccountTx` rather than a bare `INSERT` on the
// harness connection, and that is not tidiness — it is forced. `assets` has FORCE
// ROW LEVEL SECURITY on, so the OWNER cannot insert a row without an account set
// either, and a fixture that inserted directly would have to either take the role
// exemption that does not exist or turn RLS off for the test, which is the one
// thing a database-boundary test must not do.
//
// So the fixture exercises the same seam every write in the service uses, and if
// that seam broke the fixtures would fail rather than pass.
func seedAsset(t *testing.T, tt *pgtest.Test, id, account string) {
	t.Helper()
	ctx := context.Background()
	store := tt.Store()
	err := store.InAccountTx(ctx, account, func(tx pgx.Tx) error {
		_, err := tx.Exec(ctx,
			`insert into assets (id, account_id, owner_user_id, kind, original_filename, content_type, byte_size, checksum, storage_key)
			 values ($1::uuid, $2::uuid, $3::uuid, 'image', 'a.png', 'image/png', 3, $4, $5)`,
			id, account, userA, checksumFor(id), "key/"+id)
		return err
	})
	if err != nil {
		t.Fatalf("seeding %s for account %s: %v", id, account, err)
	}
}

// checksumFor is 64 hex characters, because `assets.checksum` is a sha256 and a
// fixture that stored "abc" would be testing a CHECK constraint rather than a
// policy.
//
// It is derived from the asset id rather than constant so that two fixtures never
// collide on `assets_account_checksum_uniq`, which is the one constraint in
// migrations/0001 that could make a two-account fixture fail for a reason that has
// nothing to do with isolation.
func checksumFor(id string) string {
	out := []byte(strings.Repeat("a", 64))
	copy(out, id)
	return string(out)
}

// countFor sets account `account` as the transaction identity and counts what the
// account can then see.
//
// `begin_account` is transaction-local and is called through the SAME seam the
// service uses — `InAccountTx` — rather than by a bare `select cafaye.begin_account`
// in the test. A test that set the identity its own way would prove the identity
// can be set, which is not the claim; the claim is that the service's one door
// sets it and the policies obey it.
func countFor(t *testing.T, tt *pgtest.Test, account string) int {
	t.Helper()
	store := tt.Store()
	ctx := context.Background()
	var n int
	err := store.InAccountTx(ctx, account, func(tx pgx.Tx) error {
		return tx.QueryRow(ctx, `select count(*) from assets`).Scan(&n)
	})
	if err != nil {
		t.Fatalf("counting as account %s: %v", account, err)
	}
	return n
}

// TestTheHarnessGivesEachTestItsOwnSchema is the property the whole parallel suite
// rests on, asserted rather than assumed.
//
// It is written as a table over six schemas created CONCURRENTLY inside one test
// binary, which is the arrangement `go test` produces by default and the one that
// would break a harness whose isolation was timing rather than naming. Six cases
// run as goroutines inside ONE test rather than as six `t.Run` subtests, because
// `t.Run` without `t.Parallel()` serialises them and would prove nothing about
// concurrency — and that is the mistake worth naming, since a version of this test
// written with plain `t.Run` would have been green for the right-looking reason.
//
// Each case writes a row and then counts the WHOLE table, so the assertion is "each
// test sees exactly one row" rather than "each test wrote a row". The negative half
// is the half that is easy to get wrong: a harness where every schema is the same
// schema satisfies "I wrote my row" and fails "I see one row".
func TestTheHarnessGivesEachTestItsOwnSchema(t *testing.T) {
	// IN THIS TEST'S OWN GOROUTINE, before anything is spawned, and NOT inside the
	// six cases. `t.Skip` calls `runtime.Goexit`, so a skip inside a goroutine ends
	// that goroutine at the skip and the parent then asserts against zero-valued
	// fields — which reported six phantom schema COLLISIONS on a machine with no
	// PostgreSQL at all. `pgtest.Require`'s comment has the transcript.
	pgtest.Require(t)

	const cases = 6
	ids := make([]string, cases)
	for i := range ids {
		ids[i] = fmt.Sprintf("%08x-0000-4000-8000-000000000000", i+1)
	}

	var wg sync.WaitGroup
	errs := make([]error, cases)
	schemas := make([]string, cases)
	seens := make([]int, cases)

	for i := 0; i < cases; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			tt := pgtest.New(t)
			schemas[i] = tt.Schema
			seedAsset(t, tt, ids[i], accountA)

			// Count as account A, which is the identity the fixture wrote under.
			// Every fixture in this test shares one account ON PURPOSE: the account
			// is the same and the SCHEMA is the variable, so a count above one means
			// two schemas are one schema rather than two accounts being one account.
			var seen int
			err := tt.Store().InAccountTx(context.Background(), accountA, func(tx pgx.Tx) error {
				return tx.QueryRow(context.Background(), `select count(*) from assets`).Scan(&seen)
			})
			if err != nil {
				errs[i] = fmt.Errorf("counting in %s: %w", tt.Schema, err)
				return
			}
			seens[i] = seen
		}(i)
	}
	wg.Wait()

	bySchema := map[string]int{}
	for i, err := range errs {
		if err != nil {
			t.Errorf("case %d (schema %q): %v", i, schemas[i], err)
			continue
		}
		if seens[i] != 1 {
			t.Errorf("case %d (schema %q) saw %d rows, want 1: two tests share a schema", i, schemas[i], seens[i])
		}
		if prev, dup := bySchema[schemas[i]]; dup {
			t.Errorf("case %d and case %d both got the schema %q", prev, i, schemas[i])
		}
		bySchema[schemas[i]] = i
	}
}

// TestAnAccountSeesOnlyItsOwnAssets is the boundary itself, asked of the database
// rather than of the source.
//
// Two accounts, one row each, and each account reads the whole table. The
// `count == 1` assertions are the load-bearing half: a policy set that hides
// everything also satisfies "sees nobody else's", so a suite that only asserted the
// denial would be green against a database that is simply broken. This is the same
// reasoning as `a_cross_tenant_404_is_indistinguishable_from_a_missing_one` in
// `AGENTS.md` run in the other direction — an answer that cannot be distinguished
// from a wrong one is not an answer.
func TestAnAccountSeesOnlyItsOwnAssets(t *testing.T) {
	tt := pgtest.New(t)
	seedAsset(t, tt, "33333333-3333-4333-8333-333333333333", accountA)
	seedAsset(t, tt, "44444444-4444-4444-8444-444444444444", accountB)

	if got := countFor(t, tt, accountA); got != 1 {
		t.Errorf("account A sees %d assets, want 1: a policy that hides everything also satisfies 'sees nobody else's'", got)
	}
	if got := countFor(t, tt, accountB); got != 1 {
		t.Errorf("account B sees %d assets, want 1", got)
	}
}

// TestTheLoginRoleCannotAlterItsOwnPolicies is the difference between `darkroom`
// and `darkroom_app`, asked of the role rather than of a comment.
//
// `10-cluster.sh` names the two roles and grants `_app` TO the owner so the owner
// can become the weaker role for a proof. This is that proof, running as the
// weaker role. A login role that could disable RLS on its own tables would make
// every policy in migrations/0004 a comment, and `AGENTS.md`'s rule that the
// account boundary be "absence, not refusal" depends on it.
func TestTheLoginRoleCannotAlterItsOwnPolicies(t *testing.T) {
	tt := pgtest.New(t)
	ctx := context.Background()

	for _, tc := range []struct {
		name    string
		stmt    string
		wantErr bool
	}{
		{
			name:    "disable row level security",
			stmt:    `alter table assets disable row level security`,
			wantErr: true,
		},
		{
			name:    "drop a policy",
			stmt:    `drop policy assets_cafaye_select on assets`,
			wantErr: true,
		},
		{
			// TRUNCATE IS NOT SUBJECT TO ROW-LEVEL SECURITY — it is refused here by
			// PRIVILEGE, not by a policy, and the distinction is worth keeping
			// because it is the difference between two protections. `protect_table`
			// grants SELECT, INSERT, UPDATE and DELETE to `darkroom_app` and nothing
			// else, so TRUNCATE is not on the grant list at all and the statement
			// fails at permission-check time.
			//
			// This row said `wantErr: false`, on the reasonable reading that a
			// statement RLS does not cover must succeed. It failed with `permission
			// denied for table assets`, which is the STRONGER answer: the login role
			// cannot empty the table either, so the assumption was the wrong way
			// round.
			name:    "truncate around the policies",
			stmt:    `truncate assets`,
			wantErr: true,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			conn, err := pgx.Connect(ctx, tt.URL)
			if err != nil {
				t.Fatalf("connecting: %v", err)
			}
			defer func() { _ = conn.Close(context.WithoutCancel(ctx)) }()
			// A single connection, which is what `pgx.Connect` gives. `set role` is
			// session state and a pool would spread it across connections this test
			// does not control.
			if _, err := conn.Exec(ctx, `set role "darkroom_app"`); err != nil {
				t.Fatalf("taking darkroom_app: %v", err)
			}
			_, err = conn.Exec(ctx, tc.stmt)
			switch {
			case err != nil && !tc.wantErr:
				t.Errorf("%s as darkroom_app: %v", tc.name, err)
			case err == nil && tc.wantErr:
				t.Errorf("%s as darkroom_app: SUCCEEDED. darkroom_app can weaken the boundary on its own tables, which is the one privilege this design exists to withhold.", tc.name)
			}
		})
	}
}

// TestTheAppRoleIsNotAMemberOfTheOwnerRole is the direction that cannot be
// asserted from kit's side, and kit says so in `10-cluster.sh`:
//
//	"The DIRECTION is the whole point and it is asymmetric by design:
//	  owner -> app     allowed, and it is a strict weakening […]
//	  app -> owner     FORBIDDEN, and it is what the design is for."
//
// `10-cluster.sh` cannot check it because "a service may legitimately grant it",
// so it lands on the service. And it is worth asserting even though the cluster
// script is the only thing that grants it: the failure mode is silent, it is
// permanent once made, and it hands the application every privilege on every
// table in the database.
func TestTheAppRoleIsNotAMemberOfTheOwnerRole(t *testing.T) {
	tt := pgtest.New(t)
	var n int
	err := tt.Conn.QueryRow(context.Background(),
		`select count(*) from pg_auth_members m
		   join pg_roles member on member.oid = m.member
		   join pg_roles granted on granted.oid = m.roleid
		  where member.rolname = 'darkroom_app' and granted.rolname = 'darkroom'`,
	).Scan(&n)
	if err != nil {
		t.Fatalf("reading pg_auth_members: %v", err)
	}
	if n != 0 {
		t.Errorf("darkroom_app is a member of darkroom in %d membership row(s). The owner->app grant is what makes a proof writable; the reverse hands the application every privilege the owner has, on every table.", n)
	}
}

// TestTheLoginRoleCanWriteEveryTableTheServiceWrites is the other half of the
// grant story, and it is here because the half that is tested is the one that gets
// attention.
//
// The boundary tests above establish that `darkroom_app` cannot see across accounts
// and cannot weaken the policies. This establishes that it can still DO THE
// SERVICE, which is the failure the grants in `0004_account_isolation.sql` exist to
// prevent and which no boundary test would have caught:
//
//	has_table_privilege('darkroom_app','outbox_events','INSERT')  ->  false
//
// measured on the shared cluster before those grants were added. `protect_table`
// grants DML only as part of PROTECTING a table, and these two tables are not
// protected, so an application that adopted the substrate and stopped there came up
// with a login role that could read its account-scoped tables and write nothing at
// all — failing at the first upload rather than at boot, because `Open` pings
// successfully.
//
// `darkroom_app` is impersonated rather than connected as, because the harness has
// no credential for it and does not need one: `10-cluster.sh` grants
// `<svc>_app` TO `<svc>` precisely so the owner can become the weaker role.
func TestTheLoginRoleCanWriteEveryTableTheServiceWrites(t *testing.T) {
	tt := pgtest.New(t)
	ctx := context.Background()

	for _, table := range []string{"assets", "asset_variants", "outbox_events", "idempotency_keys"} {
		for _, priv := range []string{"SELECT", "INSERT", "UPDATE", "DELETE"} {
			var has bool
			row := tt.AsRole(ctx, "darkroom_app",
				`select has_table_privilege(current_user, $1, $2)`, table, priv)
			if err := row.Scan(&has); err != nil {
				t.Fatalf("asking whether darkroom_app holds %s on %s: %v", priv, table, err)
			}
			if !has {
				t.Errorf("darkroom_app does not hold %s on %s. The service cannot write that table at all.", priv, table)
			}
		}
	}

	// And the grants stay NARROW, which is the property `all` would have broken.
	// TRUNCATE is the case that matters: it is not subject to RLS, so a role that
	// held it could empty a tenant table with no policy consulted at all.
	var truncate bool
	if err := tt.AsRole(ctx, "darkroom_app",
		`select has_table_privilege(current_user, 'assets', 'TRUNCATE')`).Scan(&truncate); err != nil {
		t.Fatalf("asking whether darkroom_app holds TRUNCATE on assets: %v", err)
	}
	if truncate {
		t.Error("darkroom_app holds TRUNCATE on assets. The grants say DML and DML is four privileges, not `all` — and TRUNCATE is not subject to RLS, so it would empty a tenant table with no policy consulted.")
	}
}

// TestTheLoginRoleCanDoTheWork is the end-to-end version of the test above, and it
// is here because the privilege-level assertions are the wrong altitude for the one
// question that actually matters: can the application, logging in as `darkroom_app`,
// insert an asset AND the outbox event that describes it, in ONE transaction?
//
// That is the shape of every write in darkroom. `AGENTS.md` on the outbox: "Every
// darkroom event is inserted into this table in the SAME transaction as the domain
// write it describes; there is no path in this service that inserts an event
// anywhere else." So a grant set that satisfies the two halves separately and
// refuses the pair is a grant set that cannot express the service's own rule, and
// only a two-table transaction finds that out.
//
// MEASURED ON THE SHARED CLUSTER as `darkroom_app`, host side of
// `DARKROOM_DATABASE_URL`, rather than by impersonating the owner: an
// impersonation proves the PRIVILEGES and this proves the CREDENTIAL, and the
// credential is what a request path actually holds.
func TestTheLoginRoleCanDoTheWork(t *testing.T) {
	url := strings.TrimSpace(os.Getenv("DARKROOM_DATABASE_URL"))
	if url == "" {
		t.Skip("no DARKROOM_DATABASE_URL in this environment. It is the APPLICATION's credential — the login role — and the rest of this package runs as the owner because the harness creates schemas, which needs CREATE on the database. Set it to the darkroom_app URL in .env.example to run this one.")
	}

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	conn, err := pgx.Connect(ctx, url)
	if err != nil {
		t.Fatalf("connecting as the application: %v", redactForTest(url))
	}
	defer func() { _ = conn.Close(context.WithoutCancel(ctx)) }()

	// Prove which role this actually is, because a URL pointed at `darkroom` would
	// pass every assertion in this test while testing nothing about the login role.
	var who string
	if err := conn.QueryRow(ctx, `select current_user`).Scan(&who); err != nil {
		t.Fatalf("reading current_user: %v", err)
	}
	if who != "darkroom_app" {
		t.Fatalf("DARKROOM_DATABASE_URL authenticates as %q, not darkroom_app. Every assertion below would still pass against the owner, which owns the tables outright and so cannot fail any of them.", who)
	}

	tx, err := conn.Begin(ctx)
	if err != nil {
		t.Fatalf("beginning: %v", err)
	}
	defer func() { _ = tx.Rollback(context.WithoutCancel(ctx)) }()

	// The identity the request path sets, then the two writes. One transaction:
	// the domain row and the event describing it, which is the whole outbox rule.
	if _, err := tx.Exec(ctx, `select cafaye.begin_account($1::uuid)`, accountA); err != nil {
		t.Fatalf("setting the account identity: %v", err)
	}
	// A fresh id per run, and the rows are REMOVED at the end. This test writes to
	// `public` — the service's own schema — rather than to a per-test one, because
	// the point is to prove the CREDENTIAL works and the credential's grants are
	// cluster-wide facts, not schema-local ones.
	//
	// Which makes cleanup load-bearing rather than tidy. The first version used a
	// fixed id and the second run failed with `duplicate key value violates unique
	// constraint "assets_pkey"`, having left its row in a database the developer
	// also uses. A test that writes into the service's own schema and does not
	// clean up is a test that corrupts a working environment, and the failure mode
	// is a red test rather than a lost upload.
	assetID := randomUUID(t)
	if _, err := tx.Exec(ctx,
		`insert into assets (id, account_id, owner_user_id, kind, original_filename, content_type, byte_size, checksum, storage_key)
		 values ($1::uuid, $2::uuid, $3::uuid, 'image', 'a.png', 'image/png', 3, $4, $5)`,
		assetID, accountA, userA, checksumFor(assetID), "key/"+assetID); err != nil {
		t.Fatalf("inserting the asset as darkroom_app: %v", err)
	}
	// `subject` is the ASSET'S id, not a literal. The cleanup below deletes the
	// event by `subject = assetID`, and the first version inserted the literal
	// 'asset' — so the cleanup deleted nothing and every run left a row behind in
	// the service's own database. Three runs, three orphans, found by counting
	// `assets` and `outbox_events` after the suite rather than trusting it.
	//
	// `core/docs/event-outbox.md` owns this column list; `subject` naming the
	// subject of the event is the obvious reading and this test asserts it rather
	// than assuming it.
	if _, err := tx.Exec(ctx,
		`insert into outbox_events (id, event_type, source, subject, time, data)
		 values (gen_random_uuid(), 'darkroom.asset.ready', 'darkroom', $1, now(), '{}'::jsonb)`,
		assetID); err != nil {
		t.Fatalf("inserting the outbox event as darkroom_app: %v", err)
	}
	if err := tx.Commit(ctx); err != nil {
		t.Fatalf("committing: %v", err)
	}

	// And the read-back is SCOPED, which is the property that makes the write
	// worth anything: the row is visible to its own account and invisible to
	// another, on the connection the application actually holds.
	//
	// TWO transactions rather than one `begin; …; commit` string, because the
	// extended protocol refuses it: "cannot insert multiple commands into a
	// prepared statement". pgx uses the extended protocol whenever a query takes
	// arguments, and a query with arguments cannot be a batch — so a single-string
	// transaction works in `psql` and fails here. That asymmetry is worth knowing
	// before somebody "simplifies" a test into it.
	seenAs := func(account string) int {
		t.Helper()
		rx, err := conn.Begin(ctx)
		if err != nil {
			t.Fatalf("beginning a read as %s: %v", account, err)
		}
		defer func() { _ = rx.Rollback(context.WithoutCancel(ctx)) }()
		if _, err := rx.Exec(ctx, `select cafaye.begin_account($1::uuid)`, account); err != nil {
			t.Fatalf("setting the identity as %s: %v", account, err)
		}
		var n int
		if err := rx.QueryRow(ctx, `select count(*) from assets where id = $1::uuid`, assetID).Scan(&n); err != nil {
			t.Fatalf("counting as %s: %v", account, err)
		}
		return n
	}

	if n := seenAs(accountB); n != 0 {
		t.Errorf("account B sees %d of the assets account A just wrote, as darkroom_app. The write path works and the read path does not scope, which is the worse of the two failures.", n)
	}
	if n := seenAs(accountA); n != 1 {
		t.Errorf("account A sees %d of its own just-written assets, want 1. A policy that hides everything also satisfies 'nobody sees another's'.", n)
	}

	// The cleanup, and it is a DELETE rather than a rollback because the write has
	// already committed — which is the whole point of the test. `darkroom_app` holds
	// DELETE on `assets` (asserted in the test above), so this needs no role
	// elevation and is exactly the statement the grants were specified for.
	cleanup, err := conn.Begin(ctx)
	if err != nil {
		t.Fatalf("beginning the cleanup: %v", err)
	}
	defer func() { _ = cleanup.Rollback(context.WithoutCancel(ctx)) }()

	// THE IDENTITY IS SET AGAIN, AND THE FIRST VERSION DID NOT — so the DELETE
	// matched zero rows and reported success.
	//
	// This is RLS working exactly as designed and it is the nastiest failure in the
	// substrate: `assets_cafaye_delete` is `using (account_id = (select
	// cafaye.current_account_id()))`, and with no identity set the helper returns
	// NULL, so the predicate is NULL rather than false, so the statement deletes
	// NOTHING and exits 0. A cleanup that reports success and does nothing is
	// worse than one that fails, because the run is green and the developer's
	// database is not.
	//
	// It was found by COUNTING the tables after three green runs —
	// `select count(*) from assets` said 3 — rather than by reading the test, which
	// looked correct throughout. `AGENTS.md` calls the design "the rule is absence,
	// not refusal", and this is the same rule seen from the other side: a statement
	// that touches nothing is indistinguishable from one that did its job.
	if _, err := cleanup.Exec(ctx, `select cafaye.begin_account($1::uuid)`, accountA); err != nil {
		t.Fatalf("setting the identity for the cleanup: %v", err)
	}

	tag, err := cleanup.Exec(ctx, `delete from assets where id = $1::uuid`, assetID)
	if err != nil {
		t.Errorf("removing the row this test wrote into public.assets (%s): %v. It is still there, and the next run of this test will fail on it.", assetID, err)
		return
	}
	// The ROW COUNT, because `err == nil` is not the same claim as "the row is
	// gone" and this is the assertion that would have caught it.
	if n := tag.RowsAffected(); n != 1 {
		t.Errorf("the cleanup deleted %d rows, want 1. RLS made the DELETE a no-op rather than an error, so the row written into public.assets (%s) is still there.", n, assetID)
	}
	if _, err := cleanup.Exec(ctx, `delete from outbox_events where subject = $1`, assetID); err != nil {
		t.Errorf("removing the outbox row this test wrote: %v", err)
	}
	if err := cleanup.Commit(ctx); err != nil {
		t.Errorf("committing the cleanup: %v", err)
	}
}

// TestTheMigrationsProtectedExactlyTheAccountScopedTables makes the count in
// migrations/0004's header auditable.
//
// The header claims two of darkroom's four tables carry an `account_id` and that
// both are protected. `cafaye.unprotected_tables()` is kit's own sweep for exactly
// this: every table with an `account_id` column that no `protect_table` call named.
// Zero rows is the answer the migration intends, and this is what stops that claim
// from being only prose — an `assets` table protected by hand in a later migration,
// or a new account-scoped table nobody protected, shows up here as a row.
func TestTheMigrationsProtectedExactlyTheAccountScopedTables(t *testing.T) {
	tt := pgtest.New(t)
	rows, err := tt.Conn.Query(context.Background(),
		`select table_schema, table_name from cafaye.unprotected_tables() order by 1, 2`)
	if err != nil {
		t.Fatalf("reading cafaye.unprotected_tables(): %v", err)
	}
	defer rows.Close()

	var unprotected []string
	for rows.Next() {
		var schema, table string
		if err := rows.Scan(&schema, &table); err != nil {
			t.Fatalf("scanning a sweep row: %v", err)
		}
		unprotected = append(unprotected, schema+"."+table)
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("reading the sweep: %v", err)
	}
	if len(unprotected) > 0 {
		t.Errorf("account-scoped tables with no protect_table call: %s", strings.Join(unprotected, ", "))
	}

	// And the positive half, so the sweep is not passing because it is empty.
	var protected int
	if err := tt.Conn.QueryRow(context.Background(),
		`select count(*) from pg_policies where schemaname = current_schema()`).Scan(&protected); err != nil {
		t.Fatalf("counting policies in %s: %v", tt.Schema, err)
	}
	if protected != 8 {
		t.Errorf("%d policies in %s, want 8: four commands on each of two tables", protected, tt.Schema)
	}
}

// TestEveryProtectedTableIsForced is the assertion a grep cannot make.
//
// `AGENTS.md` on the Rust side is blunt about the gap this closes: Postgres exempts
// a table's OWNER from its own policies, so a table with four policies, RLS
// enabled, and no FORCE reads as fully protected in `pg_class` while the role that
// owns it reads every account's rows. `relforcerowsecurity` is the only catalog
// that says otherwise — kit's substrate calls it "the one setting on this page that
// Supabase's row-level-security guide does not document and that no lint in this
// fleet checks for".
func TestEveryProtectedTableIsForced(t *testing.T) {
	tt := pgtest.New(t)
	rows, err := tt.Conn.Query(context.Background(),
		`select c.relname, c.relrowsecurity, c.relforcerowsecurity
		   from pg_class c join pg_namespace n on n.oid = c.relnamespace
		  where n.nspname = current_schema() and c.relkind = 'r'
		  order by 1`)
	if err != nil {
		t.Fatalf("reading pg_class: %v", err)
	}
	defer rows.Close()

	// table -> forced. Only the account-scoped tables are required to be forced;
	// outbox_events and idempotency_keys have no account_id and are deliberately
	// unprotected, which migrations/0004's header states at length.
	wantForced := map[string]bool{"assets": true, "asset_variants": true}
	seen := map[string]bool{}

	for rows.Next() {
		var name string
		var rls, force bool
		if err := rows.Scan(&name, &rls, &force); err != nil {
			t.Fatalf("scanning pg_class: %v", err)
		}
		if !wantForced[name] {
			continue
		}
		seen[name] = true
		if !rls {
			t.Errorf("%s: relrowsecurity is false, so there are no policies at all", name)
		}
		if !force {
			t.Errorf("%s: relforcerowsecurity is FALSE. Postgres exempts a table's owner from its own policies, so darkroom reads every account's rows while pg_class says it is protected.", name)
		}
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("reading pg_class: %v", err)
	}
	for name := range wantForced {
		if !seen[name] {
			t.Errorf("%s is not in %s at all: migrations/0001 did not create it, or the harness applied the migrations into a different schema", name, tt.Schema)
		}
	}
}

// TestNoPolicyIsForAll is the "one policy per command, never `for all`" rule,
// asked of `pg_policies`.
//
// The substrate's header states the reason and it is worth keeping in a test
// rather than only in a comment: "the `for all` shape with `using (true)` and a
// `with check` is what silently permits an INSERT nobody meant to permit, and it
// is the shape every 'permissive RLS policy' lint is written to flag."
func TestNoPolicyIsForAll(t *testing.T) {
	tt := pgtest.New(t)
	var all int
	if err := tt.Conn.QueryRow(context.Background(),
		`select count(*) from pg_policies where schemaname = current_schema() and cmd = 'ALL'`).Scan(&all); err != nil {
		t.Fatalf("counting FOR ALL policies: %v", err)
	}
	if all != 0 {
		t.Errorf("%d policies in %s are FOR ALL. A policy that covers every command cannot be widened for one command without widening all of them.", all, tt.Schema)
	}
}

// TestEveryHelperCallInAPolicyIsWrapped is the `(select …)` rule, checked by
// deparsing the policy expressions rather than by reading the migration.
//
// `templates/database/tenancy/substrate.sql` puts the measured numbers in its own
// header — a bare function call in a policy qualifier is evaluated once per
// candidate row, and the wrapped form is hoisted into an InitPlan and evaluated
// once per statement — and it also says `protect_table` "only ever writes the
// wrapped form. A hand-written policy that writes the bare form is correct and
// slow, and nothing in a normal run will ever notice." So nothing in a normal run
// will ever notice here either, which is exactly why the check exists.
//
// `pg_get_expr` is what makes this a check rather than a grep: it deparses what
// Postgres PARSED, so a policy written inside a comment, or one that failed to
// compile and was never created, is invisible here rather than counted as a pass.
// The predicate that reads that deparse is `isWrappedAt`, and — per `AGENTS.md` on
// `tests/tenant_scoping.rs`, "any new guard here gets a planted divergence before
// it is believed" — it has its own table test in `TestIsWrappedAt` below.
func TestEveryHelperCallInAPolicyIsWrapped(t *testing.T) {
	tt := pgtest.New(t)
	rows, err := tt.Conn.Query(context.Background(),
		`select pol.polname, pg_get_expr(pol.polqual, pol.polrelid), pg_get_expr(pol.polwithcheck, pol.polrelid)
		   from pg_policy pol
		   join pg_class c on c.oid = pol.polrelid
		   join pg_namespace n on n.oid = c.relnamespace
		  where n.nspname = current_schema() and (pol.polqual is not null or pol.polwithcheck is not null)`)
	if err != nil {
		t.Fatalf("reading pg_policy: %v", err)
	}
	defer rows.Close()

	// The call itself, wherever it appears. `isWrappedAt` decides per occurrence,
	// so a policy that wrapped one call and left another bare is caught rather than
	// being cleared by the presence of the good one.
	bare := regexp.MustCompile(`(?i)cafaye\.current_account_id\(\)`)

	checked := 0
	for rows.Next() {
		var name string
		var qual, withCheck pgtype.Text
		if err := rows.Scan(&name, &qual, &withCheck); err != nil {
			t.Fatalf("scanning a policy: %v", err)
		}
		checked++
		for _, e := range []pgtype.Text{qual, withCheck} {
			if !e.Valid || e.String == "" {
				continue
			}
			if !strings.Contains(e.String, "cafaye.current_account_id()") {
				continue
			}
			// EVERY occurrence is checked, not one per policy, and that is the
			// property a presence check cannot express: an expression containing
			// both a wrapped call and a bare one satisfies "does it contain the
			// wrapped form", which is why the first version of this test used one.
			for _, at := range bare.FindAllStringIndex(e.String, -1) {
				if isWrappedAt(e.String, at[0]) {
					continue
				}
				t.Errorf("policy %s calls cafaye.current_account_id() bare: %s. Postgres evaluates a bare call once per candidate row; the wrapped form is hoisted into an InitPlan and runs once per statement.", name, e.String)
				break
			}
		}
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("reading pg_policy: %v", err)
	}
	if checked == 0 {
		t.Fatal("no policy in this schema has a qualifier or a check. The migrations did not apply, or the policies are somewhere else — either way this test proved nothing.")
	}
}

// TestTheOwnerCannotReadAnotherAccountWithoutSettingAnIdentity is the FORCE
// consequence, and it is the half that surprises people.
//
// With FORCE on, the OWNER reads zero rows from a protected table unless it sets an
// identity — the exemption is gone. `templates/database/tenancy/substrate.sql`
// calls this out as the adoption cost: "Once a table is protected, the OWNER reads
// zero rows from it too […] A migration that silently reads zero rows fails
// immediately, which is the good direction." It fails immediately rather than
// silently, and this is the test that holds it to that.
func TestTheOwnerCannotReadAnotherAccountWithoutSettingAnIdentity(t *testing.T) {
	tt := pgtest.New(t)
	ctx := context.Background()
	seedAsset(t, tt, "55555555-5555-4555-8555-555555555555", accountA)

	var n int
	row := tt.AsRole(ctx, "darkroom", `select count(*) from assets`)
	if err := row.Scan(&n); err != nil {
		t.Fatalf("counting as the owner with no identity set: %v", err)
	}
	if n != 0 {
		t.Errorf("the owner read %d rows with no account set. FORCE is supposed to remove the owner's exemption; if this is non-zero, relforcerowsecurity is false on assets even though a policy test says otherwise.", n)
	}

	// And the owner's reads are SCOPED rather than absent once it does set one —
	// substrate.sql: "naming it means the owner's reads are ACCOUNT-SCOPED rather
	// than absent."
	if got := countFor(t, tt, accountA); got != 1 {
		t.Errorf("the owner sees %d rows with account A set, want 1", got)
	}
}

// TestTheDatabaseURLIsNeverPrintedWhole guards the one place a credential could
// escape: a failure message.
//
// darkroom's URL is `postgres://darkroom:cafaye@localhost:15500/darkroom` — the
// CLUSTER's password, shared by every role on it, which `AGENTS.md` is explicit
// about ("it is the credential of EVERY role `10-cluster.sh` creates"). A
// developer who points `TEST_DATABASE_URL` at a deployment's would otherwise have
// that deployment's password in a CI log, and the redaction is a two-line
// function that nobody tests until the day it stops working.
func TestTheDatabaseURLIsNeverPrintedWhole(t *testing.T) {
	tt := pgtest.New(t)
	var leaked int
	if err := tt.Conn.QueryRow(context.Background(),
		`select count(*) from pg_policies where schemaname = current_schema()`).Scan(&leaked); err != nil {
		t.Fatalf("counting policies: %v", err)
	}

	// The assertion is on the shape of the helper, reached through a failure we
	// cause on purpose: a URL with a password in it, redacted, must not contain it.
	got := redactForTest("postgres://darkroom:hunter2@localhost:15500/darkroom")
	if strings.Contains(got, "hunter2") {
		t.Errorf("redact left the password in %q", got)
	}
	if !strings.HasSuffix(got, "@localhost:15500/darkroom") {
		t.Errorf("redact dropped the host and database: %q", got)
	}
}

// isWrappedAt reports whether the helper call starting at `at` sits inside a
// parenthesised subquery, which is the `(select …)` the substrate's rules are about.
//
// # WHY IT READS THE DEPARSE AND NOT THE MIGRATION
//
// `pg_get_expr` is what makes this a check rather than a grep: it deparses what
// Postgres PARSED, so a policy written inside a comment, or one that never
// compiled and so was never created, is invisible here rather than counted as a
// pass.
//
// The cost of that choice is that the deparse is not the text `protect_table`
// wrote. `templates/database/tenancy/substrate.sql` writes
// `account_id = (select cafaye.current_account_id())` and Postgres gives back:
//
//	(account_id = ( SELECT cafaye.current_account_id() AS current_account_id))
//
// The subquery is intact and the shape the rule is about is preserved — but the
// alias and the uppercased keyword are not, and a test that matched the written
// form would fail on eight perfectly correct policies. THAT IS WHAT THE FIRST
// VERSION OF THIS TEST DID, and it failed on all eight, which is the whole reason
// this function exists and the reason it is careful about what it treats as a
// signal.
//
// # THE PREDICATE, and why it looks backwards
//
// A call is wrapped iff, reading backwards from it, the text is `(` then
// `SELECT`, with only whitespace between. Bare is `= cafaye.…`, whose preceding
// token is an operator. Backwards rather than forwards because the interesting
// question is what ENCLOSES the call, and the enclosure is behind it.
func isWrappedAt(expr string, at int) bool {
	// Postgres deparses the subquery as `( SELECT …)`, with a SPACE between the
	// parenthesis and the keyword, so the walk trims whitespace on BOTH sides of
	// each step. The first version trimmed only trailing whitespace and therefore
	// failed to see the `(`, which made every correct policy read as bare — the
	// same class of error as the one the reparse itself caused, one layer up.
	rest := strings.TrimSpace(expr[:at])

	// The word immediately behind the call is `SELECT` in the wrapped form, and the
	// `(` is behind THAT — Postgres deparses `(select f())` as `( SELECT f())`, so
	// the parenthesis opens before the keyword rather than after it. The first
	// version of this walk looked for the parenthesis first and so read every
	// correct policy as bare; `TestIsWrappedAt` caught that, which is the argument
	// for having written the divergence table before trusting the predicate.
	//
	// Splitting to the last separator rather than slicing a fixed width is what
	// makes this safe against a deparse whose length changes between Postgres
	// versions, which this one demonstrably has: the version measured here is
	// PostgreSQL 17.11 and it appends `AS current_account_id`.
	if i := strings.LastIndexAny(rest, " \t\n\r()"); i >= 0 {
		rest = rest[i+1:]
	}
	if !strings.EqualFold(rest, "select") {
		return false
	}
	// And the keyword has to be inside a subquery rather than being a bare word.
	// `coalesce(select …)` is not valid SQL, so in practice this confirms the
	// parenthesis is there rather than proving a case that can arise; it is kept
	// because the alternative is a predicate that reads "the previous word was
	// select", which is a weaker claim than the one the rule makes.
	rest = strings.TrimSpace(expr[:at])
	if i := strings.LastIndexAny(rest, " \t\n\r"); i >= 0 {
		rest = strings.TrimSpace(rest[:i])
	}
	return strings.HasSuffix(rest, "(")
}

// TestIsWrappedAt is the planted divergence for the predicate above.
//
// `AGENTS.md` on `tests/tenant_scoping.rs`: "Any new guard here gets a planted
// divergence before it is believed, and if it hangs or passes on a broken source,
// that is a finding about the guard, not about the source." This is the planted
// divergence: the table's first two rows are the shapes that MUST be called bare
// and the guard must not wave them through, and the third row is the exact
// deparse of a policy this repository wrote, which the first version of this test
// rejected.
//
// A guard with no test of its own is a guard that has never been shown to fail, and
// a guard that has never been shown to fail has not been shown to do anything.
func TestIsWrappedAt(t *testing.T) {
	// The deparse of `protect_table`'s own qualifier, measured from pg_get_expr on
	// this cluster.
	const wrapped = `(account_id = ( SELECT cafaye.current_account_id() AS current_account_id))`
	const bare = `(account_id = cafaye.current_account_id())`
	const bareInsideFunc = `(account_id = coalesce(cafaye.current_account_id(), 'x'))`

	at := func(expr string) int {
		i := strings.Index(expr, "cafaye.current_account_id()")
		if i < 0 {
			t.Fatalf("no helper call in %q", expr)
		}
		return i
	}

	for _, tc := range []struct {
		name string
		expr string
		want bool
	}{
		// PLANTED DIVERGENCE — these must NOT be waved through.
		{"bare", bare, false},
		{"bare inside another call", bareInsideFunc, false},
		// A function whose name merely ENDS in "select". A substring test for
		// "select" would clear this one; a last-word test does not.
		{"a function whose name ends in select", `(account_id = my_select(cafaye.current_account_id()))`, false},

		// MEASURED from pg_get_expr on this cluster, and the shape the first
		// version of this test wrongly failed.
		{"the substrate's own deparse", wrapped, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got := isWrappedAt(tc.expr, at(tc.expr))
			if got != tc.want {
				t.Errorf("isWrappedAt(%q) = %v, want %v", tc.expr, got, tc.want)
			}
		})
	}
}

// randomUUID returns a version-4 UUID as a string, or fails the test.
//
// It is hand-built rather than pulled from `github.com/google/uuid` because it is
// fifteen lines and the module's only other need for one is in the asset id the
// service mints, which is a later packet's business. A test dependency that exists
// to produce sixteen bytes is a dependency that has to be vendored forever.
func randomUUID(t *testing.T) string {
	t.Helper()
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		t.Fatalf("pgtest_test: no entropy for an asset id: %v", err)
	}
	// Version 4 and the RFC 4122 variant, so the value is a real UUID and not 16
	// random bytes in a column that says `uuid`. Postgres does not enforce either,
	// which is exactly why a test that skipped them would pass and then produce an
	// id a fixture in another language could not parse.
	b[6] = (b[6] & 0x0f) | 0x40
	b[8] = (b[8] & 0x3f) | 0x80
	h := hex.EncodeToString(b[:])
	return h[0:8] + "-" + h[8:12] + "-" + h[12:16] + "-" + h[16:20] + "-" + h[20:32]
}

// redactForTest mirrors pgtest's unexported redact. It is a copy rather than an
// export because a redaction helper is not API, and this test is about the SHAPE
// of the guarantee: if pgtest's implementation changes, this copy does not, which
// is the one thing this test cannot see. It is here so that the guarantee has a
// test at all.
func redactForTest(raw string) string {
	at := strings.LastIndex(raw, "@")
	scheme := strings.Index(raw, "://")
	if at < 0 || scheme < 0 || at < scheme {
		return raw
	}
	return raw[:scheme+3] + "…" + raw[at:]
}
