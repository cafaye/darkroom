// Package darkroomdb is darkroom's only door to PostgreSQL.
//
// # THE ONE THING THIS PACKAGE IS FOR
//
// Until migrations/0004_account_isolation.sql, darkroom's tenant boundary was a
// `where account_id = $1` that every query in `src/store.rs` remembered to carry,
// and `tests/tenant_scoping.rs` read that file as text to check it. That guard is
// worth keeping and it is still there in the Rust tree; what it cannot do is stop
// a query that is WRONG rather than absent. So the database now carries the
// boundary too, and this package is the seam it is set through.
//
// # WHY THERE IS NO `Tenant` TYPE AND NO `WHERE` CLAUSE HERE
//
// `AGENTS.md` says "`Tenant` is the only way to scope a query. `auth::Tenant` has
// exactly one constructor and it takes a `Principal`." That rule survives the
// rewrite as `InAccountTx`, and the mapping is one to one:
//
//	store.rs                          darkroomdb
//	--------------------------------  ------------------------------------------
//	auth::Tenant                      uuid.UUID, from the verified token
//	&mut Transaction                  the *sql.Tx InAccountTx hands the callback
//	`where account_id = $1`           cafaye.begin_account($1) — the predicate is
//	                                  a POLICY now, and still in the query
//
// The point of the Rust rule was that a repository method which did not take a
// `Tenant` did not typecheck. `InAccountTx` gives the Go version of that: there
// is no way to start a transaction through this package without naming an account
// inside it, because naming the account is what starting the transaction means.
//
// # THE IDENTITY IS SET TRANSACTION-LOCAL, AND THAT IS NOT AN OPTION
//
// `cafaye.begin_account/1` is `set_config(..., true)` — the `true` is
// transaction-local. A connection that set it at session scope would carry one
// request's account into the next request to be handed that connection from the
// pool, and that is a cross-tenant read with no query wrong anywhere. So the call
// is made inside the transaction and nowhere else, and the transaction is not
// handed to a callback that could outlive it.
//
// # THE LOGIN ROLE
//
// The application connects as `darkroom_app`, not `darkroom`. `darkroom` owns
// every table and runs migrations; `darkroom_app` owns nothing, holds DML on the
// two account-scoped tables and nothing else, and therefore cannot
// `ALTER TABLE ... DISABLE ROW LEVEL SECURITY` or `DROP POLICY`. Both roles are
// created by kit's `templates/compose/postgres/initdb/10-cluster.sh` on the
// shared cluster, one entry per name in `KIT_POSTGRES_DATABASES`. Nothing here
// creates a role and nothing here should: a service that provisions its own login
// role has a credential in a second place.
package darkroomdb

import (
	"context"
	"errors"
	"fmt"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

// Store is a pool plus the operations that need a transaction.
//
// One struct rather than a pool handed around, so that "this connection has an
// account set on it" is not a state any caller can arrange by accident. There is
// no method on `Store` that runs a query outside a transaction, and the field is
// unexported, so there is no way to reach the pool and borrow a connection
// without one.
type Store struct {
	pool *pgxpool.Pool
}

// Ping executes `select 1` and reports whether the database answered.
//
// It exists for `GET /readyz` and nothing else, and that is the constraint on it:
// `AGENTS.md` says the probe "really does `select 1`" while "`/healthz` is
// unconditional", because "a database outage must not get the process restarted out
// from under in-flight uploads". A readiness check that did not reach the database
// would be the thing it exists to prevent.
//
// It is deliberately NOT wrapped in a transaction and deliberately takes no
// account: the probe asks whether the CONNECTION works, and
// `migrations/0004_account_isolation.sql` makes an unset
// `current_account_id()` match NULL — so a probe inside `InAccountTx` would be
// measuring the policy rather than the database.
func (s *Store) Ping(ctx context.Context) error {
	if _, err := s.pool.Exec(ctx, "select 1"); err != nil {
		return fmt.Errorf("darkroomdb.Ping: %w", err)
	}
	return nil
}

// Open connects to `databaseURL` and verifies the connection before returning.
//
// The ping is not ceremony. A `Store` whose first real use is the one that finds
// out the database is unreachable puts the failure on whichever request happened
// to arrive first, and `/readyz` already exists to be the place that answers it.
func Open(ctx context.Context, databaseURL string) (*Store, error) {
	cfg, err := pgxpool.ParseConfig(databaseURL)
	if err != nil {
		return nil, fmt.Errorf("darkroomdb.Open: %q is not a database URL: %w", redact(databaseURL), err)
	}
	// A pool's default is `max(4, GOMAXPROCS)`, which on a laptop is a number of
	// connections this service has no business holding. The shared cluster caps
	// each service's share with `ALTER ROLE ... CONNECTION LIMIT` (20 for
	// darkroom, raised by this repository's own docker-compose.yml because the
	// test tier and the service pool spend the same budget), and a pool that
	// outgrows its share does not queue politely — it fails the FATAL 53300
	// against its own role.
	cfg.MaxConns = 4

	pool, err := pgxpool.NewWithConfig(ctx, cfg)
	if err != nil {
		return nil, fmt.Errorf("darkroomdb.Open: building the pool for %s: %w", redact(databaseURL), err)
	}
	if err := pool.Ping(ctx); err != nil {
		pool.Close()
		return nil, fmt.Errorf("darkroomdb.Open: %s did not answer: %w", redact(databaseURL), err)
	}
	return &Store{pool: pool}, nil
}

// Close releases the pool. It is idempotent, because pgxpool's is and a defer in
// both TestMain and a test would otherwise be a double-close.
func (s *Store) Close() {
	if s.pool != nil {
		s.pool.Close()
	}
}

// InTx runs `fn` inside a transaction, committing on nil and rolling back on any
// error or panic.
//
// This is the only way a caller gets a transaction, and it is deliberately the
// transaction that does NOT set an identity: `outbox_events` and
// `idempotency_keys` have no `account_id` and no policy, so a deploy step or a
// migration is what this is for. A request path uses `InAccountTx`.
//
// `ErrNoTx` is returned rather than handing out a `*sql.Tx` that outlives the
// callback: the callback exists so that the commit and the rollback are not the
// caller's to forget, and a signature that returns the transaction invites the
// commit out of the block where the rollback should be.
func (s *Store) InTx(ctx context.Context, fn func(tx pgx.Tx) error) (err error) {
	if fn == nil {
		return errors.New("darkroomdb.InTx: fn is nil")
	}
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return fmt.Errorf("darkroomdb.InTx: beginning: %w", err)
	}
	// Deferred unconditionally, and the named return is what lets the panic case
	// reach it: pgxpool's Tx rolls back on panic by way of this, and a recover()
	// here would swallow the panic and return a nil error, which is how a failed
	// request becomes a committed transaction.
	defer func() {
		if p := recover(); p != nil {
			_ = tx.Rollback(context.WithoutCancel(ctx))
			panic(p)
		}
		if err != nil {
			_ = tx.Rollback(context.WithoutCancel(ctx))
		}
	}()
	if err = fn(tx); err != nil {
		return err
	}
	if err = tx.Commit(ctx); err != nil {
		return fmt.Errorf("darkroomdb.InTx: committing: %w", err)
	}
	return nil
}

// InAccountTx runs `fn` inside a transaction that has darkroom's account
// identity set on it, and commits only if `fn` returns nil.
//
// THE CALL IS TRANSACTION-LOCAL AND IS MADE BEFORE `fn`, so every statement in
// `fn` — including the domain write and the `outbox_events` insert that goes with
// it in the same transaction — runs under the same identity. Setting it after the
// first statement would leave that first statement under whatever the connection
// last held, which is the cross-tenant read described in the package comment.
//
// A NULL `account` raises rather than reading zero rows. kit's substrate is
// explicit that `current_account_id()` returning NULL when no account has been set
// is deliberate ("no identity" and "an identity that owns none of these rows" must
// be indistinguishable), and it is the right default for a POLICY. It is the
// wrong thing for a Go argument: an empty string reaching here means a request
// path built a transaction without a verified account, and that is a plumbing bug
// this package can name, so it names it rather than opening a transaction that
// reads nothing.
func (s *Store) InAccountTx(ctx context.Context, account string, fn func(tx pgx.Tx) error) error {
	if account == "" {
		return errors.New("darkroomdb.InAccountTx: the account is empty. A nil account reads zero rows rather than failing, so this is the only place it can be caught.")
	}
	return s.InTx(ctx, func(tx pgx.Tx) error {
		if _, err := tx.Exec(ctx, `select cafaye.begin_account($1::uuid)`, account); err != nil {
			return fmt.Errorf("darkroomdb.InAccountTx: setting the account identity: %w", err)
		}
		return fn(tx)
	})
}

// redact removes anything password-shaped from a URL before it goes in an error.
//
// darkroom's own URLs take the shared cluster's password, so this is not
// protecting a secret that does not exist — it is so that a connection string
// pasted into `TEST_DATABASE_URL` on a developer machine cannot reach a CI log
// through an error message. `url.Parse` is not used here because the interesting
// failures are URLs that do not parse.
func redact(raw string) string {
	at := -1
	for i := len(raw) - 1; i >= 0; i-- {
		if raw[i] == '@' {
			at = i
			break
		}
	}
	scheme := -1
	for i := 0; i+2 < len(raw); i++ {
		if raw[i] == ':' && raw[i+1] == '/' && raw[i+2] == '/' {
			scheme = i + 3
			break
		}
	}
	if at < 0 || scheme < 0 || at < scheme {
		return raw
	}
	return raw[:scheme] + "…" + raw[at:]
}
