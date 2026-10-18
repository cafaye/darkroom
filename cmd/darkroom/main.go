// Command darkroom serves the media service.
//
// This is the Go rewrite's `cmd/`, and it is a PORT of `src/main.rs` rather than a
// new design: same process lifetime, same order, same refusals. Everything it does
// is a call into a library package, because a signal path is not worth testing
// through a mock and none of the decisions belong here.
//
// # TWO RULES THAT CAME FROM `identity`'s AGENTS.md AND APPLY HERE
//
//   - **Liveness never touches a dependency.** `/healthz` is unconditional, and
//     `internal/httpapi.Healthz` holds that half.
//   - **Migrations are a deploy step, not a boot step.** Nothing here applies a
//     migration. A service that migrates itself on start is a service where two
//     replicas racing to deploy can deadlock on `CREATE TABLE`.
//
// # THE DATABASE IS OPENED BEFORE THE LISTENER, AND THE DIFFERENCE FROM RUST IS
// # DELIBERATE
//
// `src/main.rs` built the pool before the listener but noted that "the service
// still does NOT ping here: a database that is down at boot shows up as a failing
// readiness probe, which is a deployable state, rather than a crash loop."
//
// `darkroomdb.Open` DOES ping — `pool.Ping` before it returns — so this process
// refuses to start against an unreachable database, which is the behavior change
// and it is upstream's, made in packet 01 and tested there. `ping` in a comment
// here would be a second opinion; the call is one line and it is that call.
//
// The reason it is right for this service rather than merely defensible: a
// readiness probe that answers 503 because the pool cannot be built is a probe
// that says nothing a 503 from `/readyz` already says, while a process that never
// opens a listener is a deployment that fails visibly at rollout rather than
// silently absorbing traffic it cannot serve.
//
// # THE FOUR DEPENDENCIES, AND WHY THE BINARY REFUSES WITHOUT THEM
//
// service, object store, verifier, logger. `internal/httpapi.Options` already
// refuses a nil Service or a nil Verifier, and this function does not paper over
// that: a nil store and a nil database reach `New` and are refused there rather
// than producing a listener that answers 503 to everything. "There is no
// unauthenticated mode" is in `httpapi.Options` for the same reason it is here —
// a service whose auth can be switched off is one whose 401 has never been
// exercised.
//
// # NO DEV AUTH VERIFIER, AND THAT IS A FINDING RATHER THAN A GAP
//
// `src/main.rs` could return an HMAC verifier when `DARKROOM_ENV=development`.
// `internal/auth` has no such constructor, and adding one would be a security
// surface this packet did not ask for. `auth.New` is the only verifier, the dev
// path is GONE rather than dormant, and `DARKROOM_ENV` is read only to be logged.
// A development deployment therefore needs a real JWKS endpoint, which is a
// behavior change from the Rust binary and is recorded in the commit that lands
// it.
package main

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/cafaye/darkroom/internal/auth"
	"github.com/cafaye/darkroom/internal/darkroomdb"
	"github.com/cafaye/darkroom/internal/httpapi"
	"github.com/cafaye/darkroom/internal/objectstore"
	"github.com/cafaye/darkroom/internal/outbox"
	"github.com/cafaye/darkroom/internal/service"
	"github.com/cafaye/darkroom/migrations"
)

// The server's own timeouts, and why there is no write timeout.
//
// The read-header bound is the one that matters: a client that opens a connection
// and sends nothing would otherwise hold a goroutine forever. Read and idle bound
// a slow client. There is NO WriteTimeout because a write timeout fires on a slow
// CLIENT and turns a slow reader into a truncated response, which is worse than a
// slow reader — `pantry/cmd/pantry/main.go` argues it at the same values, and the
// house shape is one shape.
const (
	readHeaderTimeout = 5 * time.Second
	readTimeout       = 15 * time.Second
	idleTimeout       = 60 * time.Second

	// shutdownGrace bounds the drain. In-flight uploads are given it to finish; a
	// request cut off mid-transaction is safe — the transaction rolls back and the
	// outbox row never existed — but a client that got no response has to retry, so
	// the drain is worth the wait.
	shutdownGrace = 30 * time.Second

	// connectTimeout bounds the startup database ping, so a black-holed address
	// fails the rollout in thirty seconds rather than hanging it.
	connectTimeout = 10 * time.Second
)

// version is stamped rather than read from a build flag, because the only thing
// it is for is a startup log line and a `-version` that lied would be worse than
// one that is coarse.
const version = "0.1.0-go06"

func main() {
	if len(os.Args) > 1 && os.Args[1] == "migrate" {
		if err := runMigrate(); err != nil {
			slog.Error("darkroom: migrate failed", "error", err)
			os.Exit(1)
		}
		return
	}
	if err := run(); err != nil {
		// slog's default handler is installed before config is read, so even a
		// failure to read the config goes through the same structured pipeline. A
		// service that cannot log why it will not start is a service somebody
		// debugs with `print`.
		slog.Error("darkroom: exiting", "error", err)
		os.Exit(1)
	}
}

func runMigrate() error {
	url := os.Getenv("DARKROOM_OWNER_DATABASE_URL")
	if url == "" {
		url = os.Getenv("DARKROOM_DATABASE_URL")
	}
	if url == "" {
		return fmt.Errorf("DARKROOM_OWNER_DATABASE_URL (or DARKROOM_DATABASE_URL) is required for migrate")
	}
	ctx := context.Background()
	pool, err := darkroomdb.Open(ctx, url)
	if err != nil {
		return fmt.Errorf("open db: %w", err)
	}
	defer pool.Close()
	tx, err := pool.BeginTx(ctx)
	if err != nil {
		return fmt.Errorf("begin tx: %w", err)
	}
	if err := migrations.Apply(ctx, tx); err != nil {
		_ = tx.Rollback(ctx)
		return err
	}
	if err := tx.Commit(ctx); err != nil {
		return fmt.Errorf("commit: %w", err)
	}
	slog.Info("darkroom: migrations applied")
	return nil
}

func run() error {
	config, err := loadConfig(OSLookup)
	if err != nil {
		return fmt.Errorf("darkroom: configuration: %w", err)
	}

	logger := newLogger(config)
	slog.SetDefault(logger)

	logger.Info("darkroom: starting",
		"version", version,
		"env", config.Environment,
		"object_store", config.ObjectStore)

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	// The database first, and the order is load-bearing: a listener that opens
	// without a pool behind it is a process that answers 503 to everything.
	connectCtx, cancelConnect := context.WithTimeout(ctx, connectTimeout)
	db, err := darkroomdb.Open(connectCtx, config.DatabaseURL)
	cancelConnect()
	if err != nil {
		return fmt.Errorf("darkroom: the database could not be opened: %w", err)
	}
	defer db.Close()

	objects, err := buildObjectStore(ctx, config, logger)
	if err != nil {
		return fmt.Errorf("darkroom: the object store could not be opened: %w", err)
	}

	verifier, err := auth.New(auth.Options{
		Issuer:   config.Issuer,
		Audience: config.Audience,
		JWKSURL:  config.JWKSURL,
		Log:      logger,
	})
	if err != nil {
		return fmt.Errorf("darkroom: the token verifier could not be built: %w", err)
	}

	handler, err := httpapi.New(httpapi.Options{
		Service:  service.New(db, objects),
		Verifier: verifier,
		DB:       db,
		Log:      logger,
	})
	if err != nil {
		return fmt.Errorf("darkroom: the router could not be built: %w", err)
	}

	// THE OUTBOX PUBLISHER, AND THE CONDITION IT STARTS UNDER.
	//
	// `NATS_URL` set and nothing else. With it unset there is no loop, no
	// publisher and no connection — rows accumulate in `outbox_events` and drain
	// the moment a broker is configured, which is the whole of the argument for
	// publishing from a table rather than from the request path. That is the
	// shipped state and it is the honest one: a `NoopPublisher` wired here would
	// mark every event published and drain the outbox into nowhere, which is
	// worse than accumulation because the rows are the promise and this process
	// would be keeping it by discarding.
	//
	// It is built BEFORE the listener rather than after it, and the order is
	// load-bearing: a loop started against a publisher that failed to build is a
	// goroutine that panics on its first claim.
	publisher, err := buildOutboxPublisher(config, logger)
	if err != nil {
		return fmt.Errorf("darkroom: %w", err)
	}

	// The loop runs on `ctx` — the same context the HTTP server drains on — so a
	// SIGTERM stops both. There is no second shutdown path to forget.
	if publisher != nil {
		loop := outbox.NewService(outbox.NewStore(db), publisher, logger, 0)
		go func() {
			// Run returns nil on a clean shutdown and logs its own retries, so the
			// only thing to report is an unexpected exit — which for this loop
			// means the process is shutting down anyway.
			if err := loop.Run(ctx); err != nil {
				logger.Error("darkroom: the outbox publisher loop exited", "error", err)
			}
		}()
		logger.Info("darkroom: the outbox publisher is running", "broker", config.NATSURL)
	}

	srv := &http.Server{
		Addr:              config.addr(),
		Handler:           handler,
		ReadHeaderTimeout: readHeaderTimeout,
		ReadTimeout:       readTimeout,
		IdleTimeout:       idleTimeout,
	}

	errs := make(chan error, 1)
	go func() {
		logger.Info("darkroom: listening", "addr", config.addr())
		errs <- srv.ListenAndServe()
	}()

	// THE OUTBOX PUBLISHER, AND THE CONDITION IT STARTS UNDER.
	//
	// `NATS_URL` set and nothing else. With it unset there is no loop, no
	// publisher, and no connection — rows accumulate in `outbox_events` and drain
	// the moment a broker is configured, which is the whole of the argument for
	// publishing from a table rather than from the request path. That is the
	// shipped state and it is the honest one: a `NoopPublisher` wired here would
	// mark every event published and drain the outbox into nowhere, which is
	// worse than accumulation because the rows are the promise and this process
	// would be keeping it by discarding.
	//
	// The loop runs on `ctx` — the same context the HTTP server drains on — so a
	// SIGTERM stops both. There is no second shutdown path to forget.
	//
	// A broker that is DOWN when this runs is not a boot failure: the publisher
	// connects with RetryOnFailedConnect, so the loop starts, every publish
	// fails, the rows stay claimable, and they drain when the broker returns. What
	// fails the boot is a NATS_URL that does not parse — a typo belongs at boot,
	// where an operator reads it, not at the first event, where nobody does.
	// (Built above, before the listener — see there.)

	select {
	case err := <-errs:
		if errors.Is(err, http.ErrServerClosed) {
			return nil
		}
		return fmt.Errorf("darkroom: serving: %w", err)
	case <-ctx.Done():
		// A second signal now kills the process rather than being swallowed, which
		// is what makes a wedged drain recoverable from the console.
		stop()
		logger.Info("darkroom: shutting down", "grace", shutdownGrace.String())
		shutdownCtx, cancel := context.WithTimeout(context.Background(), shutdownGrace)
		defer cancel()
		if err := srv.Shutdown(shutdownCtx); err != nil {
			// The grace is spent, so in-flight requests are dropped. Said at error
			// level: a silent truncation here is how a deploy eats a request without
			// anybody finding out.
			logger.Error("darkroom: shutdown did not finish inside the grace", "error", err)
			return fmt.Errorf("darkroom: shutdown: %w", err)
		}
		logger.Info("darkroom: stopped")
		return nil
	}
}

// buildOutboxPublisher builds the outbox publisher for a configured broker.
//
// It returns nil when no broker is configured, and an error when one is
// configured and cannot be built. The split is the point: an unset `NATS_URL` is
// a decision with a log line, and a malformed one is a MISCONFIGURATION that
// belongs at boot where an operator reads it rather than at the first event
// where nobody does.
//
// A broker that is not LISTENING is neither. `RetryOnFailedConnect` is inside
// `NewNATSPublisher`, so the connection is established lazily, the loop starts,
// every publish fails, the rows stay claimable, and they drain when the broker
// returns. Refusing to boot without a bus would couple the media surface to the
// bus's availability, turning an event delay into an upload outage.
func buildOutboxPublisher(config Config, logger *slog.Logger) (*outbox.NATSPublisher, error) {
	if !config.NATSEnabled() {
		logger.Info("darkroom: NATS_URL is not set; the outbox publisher is not " +
			"started, and events will accumulate in outbox_events until a broker is configured")
		return nil, nil
	}

	publisher, err := outbox.NewNATSPublisher(config.NATSURL)
	if err != nil {
		return nil, fmt.Errorf("building the NATS publisher for %s: %w", config.NATSURL, err)
	}
	return publisher, nil
}

// buildObjectStore mounts the configured store.
//
// The in-memory branch warns LOUDLY and names itself in the startup log, which is
// `src/main.rs`'s behavior and its reason: an operator who discovers the void by
// uploading into it has learned something the log already said.
func buildObjectStore(ctx context.Context, config Config, logger *slog.Logger) (objectstore.Store, error) {
	switch config.ObjectStore {
	case storeMemory:
		logger.Warn("darkroom: the object store is IN-MEMORY: uploads are lost on restart",
			"variable", "DARKROOM_OBJECT_STORE")
		return objectstore.NewMemory(), nil
	case storeS3:
		// No credential here and none possible: `objectstore.New` refuses a static
		// one and takes the AWS SDK's own chain. `AGENTS.md`: "No secrets in the
		// repo. Object-storage credentials are the AWS SDK's own chain."
		return objectstore.New(ctx, objectstore.S3Config{
			Bucket:    config.S3Bucket,
			Region:    config.S3Region,
			Endpoint:  config.S3Endpoint,
			PathStyle: config.S3PathStyle,
		})
	default:
		// Unreachable: `loadConfig` refuses every other value with an error naming
		// the variable. A third store is a new branch here AND in config.go.
		return nil, fmt.Errorf("darkroom: no object store named %q", config.ObjectStore)
	}
}

// newLogger builds slog from the two log variables.
//
// `human` exists because a developer's terminal is not a log aggregator and a
// stack of JSON is unread there; `json` is the default because everything that is
// machine-read gets it. `DARKROOM_LOG_LEVEL` is parsed by slog itself and an
// unparseable value is a startup refusal in `loadConfig`, so there is no silent
// fallback to `info` here.
func newLogger(config Config) *slog.Logger {
	var level slog.Level
	if err := level.UnmarshalText([]byte(config.LogLevel)); err != nil {
		// Unreachable: `loadConfig` checked the same list. A panic rather than a
		// default, because a logger running at a level nobody asked for is a log
		// somebody trusts and should not.
		panic("darkroom: DARKROOM_LOG_LEVEL passed validation and then failed to parse: " + err.Error())
	}
	opts := &slog.HandlerOptions{Level: level}
	if config.LogFormat == "human" {
		return slog.New(slog.NewTextHandler(os.Stdout, opts))
	}
	return slog.New(slog.NewJSONHandler(os.Stdout, opts))
}
