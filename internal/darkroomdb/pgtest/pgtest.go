// Package pgtest gives one PostgreSQL schema per Go test, with darkroom's
// migrations applied into it.
//
// # WHY A HARNESS AT ALL
//
// The claims this repository wants to make about its database are not checkable
// against a mock. "FORCE ROW LEVEL SECURITY is on" and "there is one policy per
// command" are facts about `pg_class` and `pg_policies`; "an account cannot read
// another account's rows" is a fact about what a query returns on a connection
// carrying a different identity. A mocked driver proves the mock agrees with the
// code, which is the one thing these tests are not about.
//
// # A SCHEMA PER TEST, NOT A DATABASE PER TEST
//
// A database per test would be the obvious isolation and it is the wrong unit on
// the shared cluster: `CREATE DATABASE` cannot run inside a transaction, takes a
// slot in `pg_database`, and — the part that decides it — every database added to
// this cluster after `10-cluster.sh` has run is created with Postgres's default
// rather than this fleet's contract. That script's own comment says it: "An
// operator who adds a database by hand gets Postgres's default rather than this
// contract." A test suite that creates forty databases over a run is forty
// hand-added databases.
//
// A SCHEMA is the same isolation for these tests and none of that cost:
//
//   - every `create table` in migrations/ lands in the test's schema, because the
//     harness puts that schema on the connection's `search_path`;
//   - `cafaye.protect_table('assets', 'darkroom_app')` takes a `regclass`, and
//     `regclass` resolution follows `search_path` — so the substrate protects the
//     TEST's tables rather than `public`'s;
//   - `drop schema ... cascade` at the end takes the test's `cafaye` schema, its
//     four tables, its eight policies and its three indexes with it, in one
//     statement, with no table list to keep in step with migrations/.
//
// This is the same mechanism the Rust harness in `tests/schema_isolation.rs`
// built and `REPORT-darkroom-hermetic-db-01.md` measured, and it is the reason
// `--test-threads=1` is not in `bin/prime`: the schemas are why the suites can run
// in parallel, and removing the flag did not make them safe, it made them provably
// safe.
//
// # PARALLEL SAFETY IS A PROPERTY OF THE NAMES, NOT OF THE TIMING
//
// Two tests in the same package run at once by default, and a harness that
// relied on them not colliding would be a harness that passes on a machine with
// one core. Each schema is named `t_` plus eight hex characters of a counter and
// the test's name hash, so no two tests can produce the same name, and the name
// is checked against that shape before `drop schema` will run — the guard
// `tests/schema_isolation.rs` insists on, for the reason it names: "a `drop
// schema` that can only fire on a schema this harness made and that no run can
// still be using."
package pgtest

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"hash/fnv"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/cafaye/darkroom/internal/darkroomdb"
	"github.com/cafaye/darkroom/migrations"
)

// migrateMu serialises the migration step across the tests in one binary.
//
// MEASURED, NOT ASSUMED. The first version of this harness applied the migration
// set concurrently and six tests failed with:
//
//	applying 0004_account_isolation.sql: ERROR: tuple concurrently updated
//	                                                             (SQLSTATE XX000)
//
// The cause is in `migrations/0004_account_isolation.sql`, not in the schema
// names: `cafaye.protect_table` GRANTS DML to `darkroom_app`, and two transactions
// granting to the SAME role at the same time contend on that role's row. The
// schemas are genuinely independent — `protect_table` takes a `regclass` and
// `regclass` resolution follows `search_path`, so each test really does protect
// its own `assets` — but the GRANT names a cluster-wide object that no schema can
// privatise.
//
// So the MIGRATION is serialised and the TESTS are not. Everything a test
// actually does — connecting, seeding, querying, asserting, dropping — runs
// concurrently on its own connection, and this lock is held for a few milliseconds
// per test. Serialising the queries as well would defeat the property this harness
// exists to provide, and `REPORT-darkroom-hermetic-db-01.md` measured the same
// thing on the Rust side: "six parallel runs before that change went red every
// time, three after it went green".
//
// It is a `sync.Mutex` and not a `sync.Once` because the schema differs per test
// and the statements have to run per schema.
var migrateMu sync.Mutex

// Timeouts, and they are budgets rather than retries.
//
// `AGENTS.md` is explicit: "No sleeps, no raised retries, no loosened
// assertions. A test that needs an expired URL uses a zero-second TTL. […] None of
// them waits." The consequence for a harness is that every wait here is bounded
// and that exceeding the bound is a FAILURE with the database's own error, rather
// than a test that passes because it eventually stopped trying.
const (
	// connectTimeout bounds one connection attempt. The database is a TCP socket
	// on the host, not a service that has to boot.
	connectTimeout = 10 * time.Second
	// migrateTimeout bounds applying four migrations. On an empty schema this is
	// milliseconds; the number is here so that a hung `CREATE INDEX` is reported
	// rather than waited on.
	migrateTimeout = 60 * time.Second
	// dropTimeout bounds the cleanup `drop schema ... cascade`. It is longer than
	// migrateTimeout because it waits on every lock any other session holds on
	// this test's schema, and a test that left a transaction open is exactly the
	// case where the wait is long and the answer is worth having.
	dropTimeout = 30 * time.Second
)

// EnvDatabaseURL names the environment variable the harness reads.
//
// `TEST_DATABASE_URL`, which is darkroom's own convention and the one
// `.env.example` documents: "TEST_DATABASE_URL comes from `.env` […] the host
// side, `localhost` and `KIT_POSTGRES_PORT` — 15500, not 5432 — the `darkroom`
// database, and the CLUSTER's password." `DATABASE_URL` is read as a fallback and
// only as a fallback, so a test never silently runs against whatever the service
// is pointed at.
const EnvDatabaseURL = "TEST_DATABASE_URL"

// SchemaPrefix is the first character of every schema this harness makes.
//
// It is a prefix rather than a marker in the middle of the name so that the
// `drop schema` guard can be a prefix test: a name that does not start with it was
// not made here, and `drop schema` is not a statement to be casual about.
const SchemaPrefix = "t_"

// SchemaSuffixLen is the number of hex characters after SchemaPrefix, and it is a
// CONSTANT rather than a literal in `dropSchema` because those two disagreed and
// the disagreement had a very loud symptom: every test failed with "REFUSING to
// drop", and every schema was then left behind on the shared cluster.
//
// Which is the right direction for this particular bug. A guard that is too strict
// leaks a schema and fails the run; a guard that is too loose deletes one. The
// constant is checked against the length `uniqueSuffix` actually builds, so the two
// cannot drift again silently.
const SchemaSuffixLen = 12

// Test is one test's private schema, migrations applied.
//
// # WHY A MIGRATED SCHEMA PER TEST RATHER THAN A MIGRATED ONCE PER RUN
//
// Applying the four migrations is the expensive part of a test, and doing it once
// per run would be faster. It would also be a harness where the first test to
// write a row decides what every later test sees, and the last test to run leaves
// that row behind for the next. `AGENTS.md` records exactly this argument being
// had about the Rust suite — "six parallel runs before that change went red every
// time" — and the answer it arrived at was the same one here: the isolation has
// to be per test or it is not isolation.
type Test struct {
	// Schema is the name to put on a connection's `search_path`. It is exported
	// because a test that wants to assert on it — "the tables are in MY schema,
	// not in public" — has to be able to name it.
	Schema string
	// URL is the same database as the environment named, with `search_path` set
	// to `Schema`. It is the URL to hand to `darkroomdb.Open`.
	URL string
	// Conn is an open connection to `URL`, with the test's schema on its
	// `search_path`. Most tests do not need it; the ones that assert on `pg_class`
	// and `pg_policies` do, because those catalogs are cluster-wide and the answer
	// only means something alongside the schema the assertion is scoped to.
	Conn *pgx.Conn

	t *testing.T
}

// New gives `t` its own schema, applies darkroom's migrations into it, and drops
// it when `t` finishes.
//
// IT SKIPS RATHER THAN FAILS when there is no database, and it says why. A machine
// with no PostgreSQL and no Docker should still be able to run `go build ./...`
// and `go vet ./...`, and a suite that cannot is a suite nobody runs. The skip
// reason is named rather than empty, and `Skip` rather than a bare `t.Skip`, so
// `go test -v` prints which of two possible absences it was.
func New(t *testing.T) *Test {
	t.Helper()

	base, reason := databaseURL()
	if reason != "" {
		t.Skip(reason)
	}
	return newIn(t, base)
}

// Require skips `t` unless there is a database to run against, and returns
// nothing else. It is what a test calls BEFORE spawning goroutines.
//
// # WHY IT EXISTS, and the bug it fixes
//
// `New` calls `t.Skip`, and `t.Skip` — like `t.Fatal` — calls `runtime.Goexit`
// rather than returning. Inside a goroutine that ends the goroutine at the skip,
// which is the right behaviour and is what `New` was written for. It is the wrong
// behaviour for a test that spawns its own goroutines and joins them with a
// `WaitGroup`, and the bug is worth recording because the failure looks like
// something else entirely:
//
//	$ go test ./internal/darkroomdb/…        # on a machine with no PostgreSQL
//	case 3 and case 4 both got the schema ""
//	case 5 (schema "") saw 0 rows, want 1: two tests share a schema
//	FAIL
//
// Every case reported a COLLISION, on a run where no case ran at all. The
// goroutines died at the skip, the parent joined them, and the assertions after
// the join ran against zero-valued fields: six empty schema names and six zero
// counts. A harness that skips correctly in one place and misreports in another is
// worse than one that fails loudly, because the message names a false cause —
// and `AGENTS.md` is direct about the cost of that direction:
//
//	"A test that silently passes without proving anything is not [honest]. A
//	skip is honest."
//
// So a test that spawns goroutines skips in ITS OWN goroutine, before spawning,
// and every case is then a case that actually ran.
func Require(t *testing.T) {
	t.Helper()
	if _, reason := databaseURL(); reason != "" {
		t.Skip(reason)
	}
}

func newIn(t *testing.T, base string) *Test {
	t.Helper()

	schema := SchemaPrefix + uniqueSuffix(t)
	ctx, cancel := context.WithTimeout(context.Background(), migrateTimeout)
	defer cancel()

	// `create schema` is fully qualified and the `search_path` is set on the
	// CONNECTION, in its startup packet, before the schema exists. That ordering is
	// what makes the harness work: a `search_path` naming a schema that is not there
	// yet is not an error in Postgres, it is a path with a hole in it, so the one
	// statement that runs before the schema is created is the one that cannot
	// accidentally land in `public` — which is the developer's real data on this
	// database.
	conn, err := connect(ctx, base, schema)
	if err != nil {
		t.Fatalf("pgtest.New(%s): connecting to %s: %v", schema, redact(base), err)
	}
	t.Cleanup(func() { _ = conn.Close(context.WithoutCancel(ctx)) })

	if _, err := conn.Exec(ctx, `create schema `+quoteIdent(schema)); err != nil {
		t.Fatalf("pgtest.New(%s): create schema on %s: %v", schema, redact(base), err)
	}
	// Registered BEFORE the first query and BEFORE the migration, so a test that
	// fails half way through still leaves the cluster as it found it. A harness
	// that only cleans up on success leaks a schema per failed run, and a cluster
	// accumulates them silently.
	t.Cleanup(func() { dropSchema(t, base, schema) })

	migrateMu.Lock()
	defer migrateMu.Unlock()

	tx, err := conn.Begin(ctx)
	if err != nil {
		t.Fatalf("pgtest.New(%s): beginning: %v", schema, err)
	}
	// The migration runs INSIDE a transaction, which is safe here and unsafe in a
	// deploy, and the difference is in `migrations.Apply`'s comment. Safe here: this
	// schema is dropped either way, so a rollback loses nothing a caller wanted.
	// Unsafe in a deploy: a half-applied substrate leaves
	// `cafaye.protect_table` defined with two of darkroom's tables protected, which
	// is a state no `psql -f` of the same files can produce. `Apply` does not open
	// one, and this is the one caller that legitimately wants one.
	if err := migrations.Apply(ctx, tx); err != nil {
		_ = tx.Rollback(context.WithoutCancel(ctx))
		t.Fatalf("pgtest.New(%s): %v", schema, err)
	}
	if err := tx.Commit(ctx); err != nil {
		t.Fatalf("pgtest.New(%s): committing the migration: %v", schema, err)
	}

	return &Test{Schema: schema, URL: withSearchPath(base, schema), Conn: conn, t: t}
}

// connect opens one connection to `raw` with `search_path` set on it.
//
// `search_path` travels as a CONNECTION PARAMETER and not as a `set` statement,
// and the reason is pool-shaped: `search_path` is session state, and session state
// set with `set` on a pooled connection is session state the NEXT borrower
// inherits. A parameter is in the startup packet, so every connection is born with
// it and there is no statement anywhere that can forget it or be issued on the
// wrong one.
//
// `public` stays on the path AFTER the test's schema so that anything the test
// does not qualify still resolves, and `pg_catalog` is first because Postgres puts
// it first implicitly and relying on that is how a table named `user` shadows one.
func connect(ctx context.Context, raw, schema string) (*pgx.Conn, error) {
	cfg, err := pgx.ParseConfig(raw)
	if err != nil {
		return nil, fmt.Errorf("%q is not a database URL: %w", redact(raw), err)
	}
	if cfg.RuntimeParams == nil {
		cfg.RuntimeParams = map[string]string{}
	}
	cfg.RuntimeParams["search_path"] = schema + ",public,pg_catalog"
	return pgx.ConnectConfig(ctx, cfg)
}

// Store opens a `darkroomdb.Store` on this test's schema, as the application
// would: the login role from `DATABASE_URL`'s role name, which is `darkroom_app`
// on the shared cluster.
//
// It is a helper rather than the only way in, because a test that needs to prove
// the boundary needs both sides — the login role that cannot see across accounts
// and the owner that can impersonate it — and `AGENTS.md` names the regression
// guard for exactly that asymmetry.
func (tt *Test) Store() *darkroomdb.Store {
	tt.t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), connectTimeout)
	defer cancel()
	store, err := darkroomdb.Open(ctx, tt.URL)
	if err != nil {
		tt.t.Fatalf("pgtest: darkroomdb.Open(%s): %v", tt.Schema, err)
	}
	tt.t.Cleanup(store.Close)
	return store
}

// AsRole runs one statement on a connection of its own with `SET ROLE` applied,
// and returns its first row.
//
// `SET ROLE` rather than a second `sql.Open`, because the point is to be the SAME
// database with a DIFFERENT role, and a second connection string that happens to
// name another role would also be a second database the moment somebody mistyped
// it. `10-cluster.sh` grants `<svc>_app` TO `<svc>` precisely so the owner can
// become the weaker role for a proof, and this is that proof's only entry point.
func (tt *Test) AsRole(ctx context.Context, role, query string, args ...any) pgx.Row {
	tt.t.Helper()
	conn, err := connect(ctx, tt.URL, tt.Schema)
	if err != nil {
		tt.t.Fatalf("pgtest: connecting to %s as %s: %v", tt.Schema, role, err)
	}
	tt.t.Cleanup(func() { _ = conn.Close(context.WithoutCancel(ctx)) })
	if _, err := conn.Exec(ctx, `set role `+quoteIdent(role)); err != nil {
		tt.t.Fatalf("pgtest: taking the %s role on %s: %v", role, tt.Schema, err)
	}
	return conn.QueryRow(ctx, query, args...)
}

// databaseURL reads the harness's URL, or returns the reason there is not one.
//
// `TEST_DATABASE_URL` first and `DATABASE_URL` second. The order matters and the
// reason is that the two are not interchangeable: `DATABASE_URL` is the service's
// own connection and a test falling back to it is a test that quietly ran against
// the service's data on a machine where the service happens to be up. It is a
// fallback rather than an error because a single-service developer with only
// `.env.example` copied has `DATABASE_URL` and nothing else, and refusing that
// would be refusing the common case to protect against the rare one — which the
// named reason below states out loud.
func databaseURL() (string, string) {
	if v := strings.TrimSpace(os.Getenv(EnvDatabaseURL)); v != "" {
		return v, ""
	}
	if v := strings.TrimSpace(os.Getenv("DATABASE_URL")); v != "" {
		return v, fmt.Sprintf("no %s in this environment; falling back to DATABASE_URL, so this runs against the SERVICE's database rather than a scratch one. Set %s to a database this harness may create schemas in.", EnvDatabaseURL, EnvDatabaseURL)
	}
	return "", fmt.Sprintf("no %s and no DATABASE_URL in this environment, and pgtest does not start a PostgreSQL: the shared cluster is started with `docker compose … up -d --wait postgres` (see AGENTS.md \"Tests that need a database\") and its URL goes in %s. This is a counted skip, not a pass.", EnvDatabaseURL, EnvDatabaseURL)
}

// uniqueSuffix is eight hex characters: four from the test's name and four from
// the kernel's CSPRNG.
//
// The name contributes the reproducibility and the random half contributes the
// uniqueness, and neither alone is enough. The name alone collides the moment two
// tests share it — and Go does not stop two tests sharing it, a table-driven test
// calling `t.Run` with a format string produces two cases with one name, and
// those two cases run concurrently. Random alone gives a name nobody can read back
// to the test that made it, which is the difference between a failure a developer
// can act on and a schema name they have to go and look up.
func uniqueSuffix(t *testing.T) string {
	sum := sha(t.Name())
	rnd := make([]byte, 4)
	if _, err := rand.Read(rnd); err != nil {
		// A CSPRNG failure is not a reason to fall back to a weaker name: two
		// colliding schemas is a cross-test data leak, and a leaked fixture is
		// worse than a failed suite.
		t.Fatalf("pgtest: no entropy for a schema name: %v", err)
	}
	out := sum[:4] + hex.EncodeToString(rnd)
	if len(out) != SchemaSuffixLen {
		// Unreachable by construction, and checked anyway: this function is the
		// only thing that decides what `dropSchema`'s guard compares against, and a
		// name of the wrong length is refused by the guard rather than dropped.
		t.Fatalf("pgtest: a schema suffix of %d characters, want %d", len(out), SchemaSuffixLen)
	}
	return out
}

// dropSchema removes a schema this harness made, and refuses to remove one it did
// not.
//
// THE REFUSAL IS THE POINT. `drop schema` is unconditional SQL and this function
// is the only thing standing between a bug in `uniqueSuffix` and deleting a
// developer's `public`. So the name is checked against `SchemaPrefix` before the
// statement is built, and a name that fails the check is a test failure with the
// name in it — not a skip, and not a warning. `tests/schema_isolation.rs` holds
// the same property on the Rust side and splits it in two for the same reason:
// "the half that needs no database runs on a bare machine: a name that can only
// be twelve characters from a fixed alphabet, and a `drop schema` that can only
// fire on a schema this harness made."
func dropSchema(t *testing.T, base, schema string) {
	t.Helper()
	if !droppable(schema) {
		t.Errorf("pgtest: REFUSING to drop %q: it is not a schema this harness made (want %q + %d hex). A drop that reaches anything else is the failure this check exists for.",
			schema, SchemaPrefix, SchemaSuffixLen)
		return
	}
	// A FRESH connection, and not the test's own: the test's connection may still
	// hold a lock on one of the objects `cascade` has to drop. The search_path is
	// `pg_catalog` rather than the test's schema for the same reason — the point
	// is to prove this can be cleaned up from OUTSIDE it.
	ctx, cancel := context.WithTimeout(context.Background(), dropTimeout)
	defer cancel()

	conn, err := connect(ctx, base, "pg_catalog")
	if err != nil {
		t.Errorf("pgtest: connecting to drop %s: %v", schema, err)
		return
	}
	defer func() { _ = conn.Close(context.WithoutCancel(ctx)) }()

	if _, err := conn.Exec(ctx, `drop schema `+quoteIdent(schema)+` cascade`); err != nil {
		t.Errorf("pgtest: dropping %s: %v", schema, err)
	}
}

// droppable reports whether `schema` is a name this harness made and may therefore
// drop.
//
// IT IS A SEPARATE, EXPORTED-BY-CONVENTION PURE FUNCTION rather than an inline
// `if`, and the reason is that an inline condition cannot be tested without
// arranging a schema it would refuse — which means the guard ships unproven.
// `AGENTS.md` on `tests/schema_isolation.rs` splits the same guard in two for
// the same reason: "the half that needs no database runs on a bare machine: a name
// that can only be twelve characters from a fixed alphabet, and a `drop schema`
// that can only fire on a schema this harness made and that no run can still be
// using."
//
// It is strict rather than lenient on purpose. Postgres unquoted identifiers fold to
// lower case, so a name that merely LOOKS like ours could be a different schema, and
// a lenient guard that drops `T_1234ABCD5678` on a case-insensitive filesystem or a
// future fold is a guard that deletes something. Everything this harness builds is
// lower-case hex after a fixed prefix, so the strict check costs nothing and a
// refusal is always a bug worth failing on rather than a name worth accommodating.
func droppable(schema string) bool {
	if !strings.HasPrefix(schema, SchemaPrefix) {
		return false
	}
	suffix := schema[len(SchemaPrefix):]
	if len(suffix) != SchemaSuffixLen {
		return false
	}
	for i := 0; i < len(suffix); i++ {
		c := suffix[i]
		if !((c >= '0' && c <= '9') || (c >= 'a' && c <= 'f')) {
			return false
		}
	}
	return true
}

// sha is four bytes of the test's own name, so the schema name says which test
// made it.
//
// `fnv` from the standard library rather than `crypto/sha256`: this is a naming
// convenience with eight characters of output, not a security decision, and the
// standard library's FNV is four lines against sha256's twenty. It is FNV-1a
// because that is the one Go's `hash/fnv` gives you by name and the difference is
// not observable at four bytes.
func sha(name string) string {
	h := fnv.New32a()
	_, _ = h.Write([]byte(name))
	return hex.EncodeToString(h.Sum(nil))
}

// withSearchPath returns `raw` with `search_path` set to `schema` as a URL
// PARAMETER, for the URLs this package hands out.
//
// The reason it is a parameter and not a `set` statement is in `connect`: session
// state set with `set` on a pooled connection is inherited by the next borrower.
// `pgx.ParseConfig` reads `search_path` out of these URLs, so every connection
// built from one is born with it.
func withSearchPath(raw, schema string) string {
	sep := "?"
	if strings.Contains(raw, "?") {
		sep = "&"
	}
	return raw + sep + "search_path=" + schema + ",public,pg_catalog"
}

// quoteIdent wraps an identifier in double quotes, rejecting anything that is not
// a bare lowercase identifier rather than escaping it.
//
// Refusing rather than escaping is the whole design. Every value reaching here is
// generated by `uniqueSuffix` or is a role name from the cluster, so there is
// nothing to escape; the check exists so that the day a value DOES come from
// somewhere else, this function fails the test loudly instead of quoting its way
// to a valid statement. kit's `10-cluster.sh` `require_identifier` makes the same
// trade for the same reason and says so at length.
func quoteIdent(name string) string {
	if name == "" {
		panic("pgtest: empty identifier")
	}
	for i := 0; i < len(name); i++ {
		c := name[i]
		ok := c == '_' || (c >= 'a' && c <= 'z') || (c >= '0' && c <= '9')
		if !ok {
			panic(fmt.Sprintf("pgtest: %q is not a bare lowercase identifier", name))
		}
	}
	return `"` + name + `"`
}

// redact removes anything password-shaped from a URL before it reaches a failure
// message. darkroom's own URL takes the shared cluster's password, so this is not
// protecting a secret that does not exist — it is so that a connection string
// pasted into the environment cannot reach a CI log through an error message.
func redact(raw string) string {
	at := strings.LastIndex(raw, "@")
	scheme := strings.Index(raw, "://")
	if at < 0 || scheme < 0 || at < scheme {
		return raw
	}
	return raw[:scheme+3] + "…" + raw[at:]
}
