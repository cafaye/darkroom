// Command darkroom's configuration: the environment in, a validated Config out.
//
// This is the Go spelling of `src/config.rs`, and it is deliberately as thin as
// that file is only where the two differ for a reason this file names. Everything
// that validates is still validated — a variable that is present but unparseable
// fails startup rather than silently defaulting a typo, which is how a service
// ends up listening on the wrong port in production.
//
// # FOUR RULES, and each one is a decision rather than a default
//
//   - `main` is the only place that reads the environment. The [Lookup] type is
//     the seam a test uses, and no package below `cmd` reads an environment
//     variable: `AGENTS.md`'s "`Tenant` is the only way to scope a query" is the
//     same discipline one level down, and a `os.Getenv` inside `internal/service`
//     would make its tests order-dependent.
//   - `DARKROOM_DATABASE_URL` is the only database URL this binary reads, and
//     `DATABASE_URL` is deliberately NOT a fallback. `AGENTS.md`: "`DATABASE_URL`
//     is `darkroom`, the OWNER. `DARKROOM_DATABASE_URL` is `darkroom_app`, the
//     LOGIN the application uses." Accepting the owner as a fallback would mean a
//     deployment that forgot the login variable runs the application with the
//     credential that can `ALTER TABLE ... DISABLE ROW LEVEL SECURITY`, and it
//     would do so silently. Missing is a startup failure with that sentence in
//     it.
//   - The R2 region rules are NOT repeated here. `objectstore.S3Config.normalise`
//     owns them, it is tested, and a second copy in configuration is a second set
//     of rules that can disagree with the first. This file passes bucket, region,
//     endpoint and path style through and lets the store refuse.
//   - `DARKROOM_DB_MAX_CONNECTIONS` and `DARKROOM_ENV` are NOT read, and that is a
//     finding rather than an omission. `darkroomdb.Open` owns the pool size and
//     sets `MaxConns` from the shared cluster's per-role cap, so a variable that
//     looked like it configured it would be a lie in a deployment manifest. The
//     Go tree has no HMAC verifier, so there is no dev-auth environment to gate:
//     `auth.New` is the only verifier and it is always on. Both are annotated in
//     `.env.example` rather than silently left to be read and believed.
package main

import (
	"fmt"
	"net"
	"os"
	"strconv"
	"strings"
)

// Lookup is the environment, injected.
//
// An interface rather than direct `os.Getenv` calls so a test can supply a map
// and never mutate process state, which would make tests order-dependent. This is
// `src/config.rs`'s `Lookup` trait, for the reason its own header gives.
type Lookup func(key string) (string, bool)

// OSLookup reads the real process environment, and it is the ONLY caller of
// `os.LookupEnv` in this repository's service code.
func OSLookup(key string) (string, bool) { return os.LookupEnv(key) }

// Defaults, and each is the value a service with no configuration at all would
// have to pick anyway. 8080 is every other anywaye service, because a service that
// listens somewhere else is one more thing to configure.
const (
	defaultBindAddr  = "0.0.0.0"
	defaultPort      = 8080
	defaultLogLevel  = "info"
	defaultLogFormat = "json"
)

// objectStoreKind is which `objectstore.Store` the process mounts.
type objectStoreKind string

const (
	// storeMemory is the development store. Bytes are lost on restart, so the
	// startup log says so: `src/main.rs` warns, and an operator who discovers it
	// by uploading into the void has learned it the expensive way.
	storeMemory objectStoreKind = "memory"
	storeS3     objectStoreKind = "s3"

	// natsURLVar is the broker address, and it is NOT prefixed `DARKROOM_`. It is
	// the one variable this binary reads by its bare fleet-wide name, because
	// NATS_URL is the stack's and not the service's: kit's compose file declares
	// the broker, identity reads the same name, and a service that read
	// `DARKROOM_NATS_URL` instead would need its own copy of the address in every
	// deployment manifest. Two services on one bus is one variable.
	natsURLVar = "NATS_URL"
)

// Config is the validated configuration. Every field is either required or has a
// documented default; nothing here is "best effort".
type Config struct {
	BindAddr string
	Port     int

	// DatabaseURL is the LOGIN url, never the owner's. See the package header.
	DatabaseURL string

	// ObjectStore is which store, plus the S3 knobs. The R2 rules are applied by
	// `objectstore`, not here — see the package header.
	ObjectStore objectStoreKind
	S3Bucket    string
	S3Region    string
	S3Endpoint  string
	S3PathStyle bool

	Issuer   string
	Audience string
	JWKSURL  string

	// NATSURL is the broker the outbox publisher drains `outbox_events` onto, and
	// it is the one OPTIONAL address in this table. Empty means no publisher and
	// no loop: rows accumulate in the table and drain when a broker appears,
	// which is the whole point of publishing from a table rather than from the
	// request path. See [Config.NATSEnabled].
	NATSURL string

	LogLevel  string
	LogFormat string

	// Environment is recorded and logged, and nothing else. It gates no verifier,
	// because the Go tree has exactly one.
	Environment string
}

// addr is the listen address, one string so the log line and the listener cannot
// disagree about it.
//
// `DARKROOM_BIND_ADDR` MAY CARRY ITS OWN PORT, and this is not a convenience —
// the alternative is an address that cannot be expressed. Binding a service to
// one interface on one port while `PORT` serves another is not a configuration
// anybody asks for, and with the bare-host-only reading the only way to get it
// is a variable whose name promises an address and silently delivers half of one.
// So a value that already contains a port wins outright, and a bare host still
// takes `PORT` exactly as before.
//
// The failure this replaces was silent and load-bearing: `127.0.0.1:8081`
// became `127.0.0.1:8081:8080`, which the listener refused with "too many
// colons in address" — a clear error, but one that arrives as a crash on
// startup for a setting that reads as correct.
func (c Config) addr() string {
	if _, _, err := net.SplitHostPort(c.BindAddr); err == nil {
		return c.BindAddr
	}
	return fmt.Sprintf("%s:%d", c.BindAddr, c.Port)
}

// loadConfig reads and validates, failing on the FIRST problem with a specific
// message — an operator fixing a deployment sees the actual cause rather than a
// list of four things that might be wrong.
func loadConfig(lookup Lookup) (Config, error) {
	port := defaultPort
	if raw, ok := firstNonEmpty(lookup, "PORT"); ok {
		parsed, err := strconv.Atoi(strings.TrimSpace(raw))
		if err != nil || parsed < 1 || parsed > 65535 {
			return Config{}, fmt.Errorf("PORT must be a number between 1 and 65535, got %q", raw)
		}
		port = parsed
	}

	// Required, and required BY THIS NAME. The error says what is missing and
	// what the right variable is, because "connection refused" at startup with no
	// mention of a variable is how a half-finished deployment costs an hour.
	databaseURL, ok := firstNonEmpty(lookup, "DARKROOM_DATABASE_URL")
	if !ok {
		return Config{}, fmt.Errorf("DARKROOM_DATABASE_URL is required: darkroom authenticates as darkroom_app, the LOGIN role that owns no table and cannot drop a policy. DATABASE_URL is the OWNER credential and is deliberately not read here")
	}

	store, bucket, region, endpoint, pathStyle, err := loadObjectStore(lookup)
	if err != nil {
		return Config{}, err
	}

	issuer, ok := firstNonEmpty(lookup, "DARKROOM_ISSUER")
	if !ok {
		return Config{}, fmt.Errorf("DARKROOM_ISSUER is required")
	}
	audience, ok := firstNonEmpty(lookup, "DARKROOM_AUDIENCE")
	if !ok {
		return Config{}, fmt.Errorf("DARKROOM_AUDIENCE is required")
	}
	// Optional, because `auth.New` defaults it to `{Issuer}` + its own well-known
	// path. Rust required it; here the derivation is the only spelling there is,
	// and an operator who publishes keys elsewhere says so.
	jwksURL, _ := firstNonEmpty(lookup, "DARKROOM_JWKS_URL")

	// Optional, and the ONLY address in this file that is. Its absence is a
	// decision rather than an omission — see [Config.NATSEnabled].
	natsURL, _ := firstNonEmpty(lookup, natsURLVar)

	logLevel, ok := firstNonEmpty(lookup, "DARKROOM_LOG_LEVEL")
	if !ok {
		logLevel = defaultLogLevel
	}
	if !isLogLevel(logLevel) {
		return Config{}, fmt.Errorf("DARKROOM_LOG_LEVEL is invalid: %q is not a level", logLevel)
	}

	logFormat, ok := firstNonEmpty(lookup, "DARKROOM_LOG_FORMAT")
	if !ok {
		logFormat = defaultLogFormat
	}
	if logFormat != "json" && logFormat != "human" {
		return Config{}, fmt.Errorf("DARKROOM_LOG_FORMAT is invalid: %q is not json or human", logFormat)
	}

	bindAddr, ok := firstNonEmpty(lookup, "DARKROOM_BIND_ADDR")
	if !ok {
		bindAddr = defaultBindAddr
	}
	environment, ok := firstNonEmpty(lookup, "DARKROOM_ENV")
	if !ok {
		environment = "production"
	}

	return Config{
		BindAddr:    bindAddr,
		Port:        port,
		DatabaseURL: databaseURL,
		ObjectStore: store,
		S3Bucket:    bucket,
		S3Region:    region,
		S3Endpoint:  endpoint,
		S3PathStyle: pathStyle,
		Issuer:      issuer,
		Audience:    audience,
		JWKSURL:     jwksURL,
		NATSURL:     natsURL,
		LogLevel:    logLevel,
		LogFormat:   logFormat,
		Environment: environment,
	}, nil
}

// NATSEnabled reports whether the outbox publisher loop should run.
//
// It is the WHOLE of what the boot wiring consults, and it is a method rather
// than `cfg.NATSURL != ""` at the call site so the condition has one spelling:
// a service that starts a drain loop with no broker is a service whose events go
// into a publisher that cannot publish, which is the failure mode
// `internal/service/outbox.go` recorded the absence of this packet to avoid.
//
// Whitespace-only counts as absent, because a compose file carrying
// `NATS_URL:` with nothing after the colon produces a variable that IS set and IS
// empty — `firstNonEmpty` is what makes the two the same thing here.
func (c Config) NATSEnabled() bool { return c.NATSURL != "" }

// loadObjectStore picks the store and carries the S3 knobs through unexamined.
//
// EVERY VALIDATION IS DELIBERATELY ABSENT and that is the point: the R2 rules —
// `auto` is legal only with an R2 endpoint, a real region with an R2 endpoint is
// refused, path style is forced for R2 — are in `objectstore.S3Config.normalise`,
// are tested in `internal/objectstore`, and are the same rules the Rust
// `resolve_s3_region` implemented. Restating them here would be a second copy
// that could disagree with the first, and the disagreement would be an
// authentication failure against a healthy bucket.
//
// `DARKROOM_S3_PATH_STYLE=true` is passed through rather than defaulted: R2 is
// forced path-style by the store, and MinIO needs the flag, so the store is the
// only place that can tell those two apart.
func loadObjectStore(lookup Lookup) (kind objectStoreKind, bucket, region, endpoint string, pathStyle bool, err error) {
	raw, ok := firstNonEmpty(lookup, "DARKROOM_OBJECT_STORE")
	if !ok {
		return storeMemory, "", "", "", false, nil
	}
	switch raw {
	case string(storeMemory):
		return storeMemory, "", "", "", false, nil
	case string(storeS3):
		bucket, ok = firstNonEmpty(lookup, "DARKROOM_S3_BUCKET")
		if !ok {
			return "", "", "", "", false, fmt.Errorf("DARKROOM_S3_BUCKET is required when DARKROOM_OBJECT_STORE=s3")
		}
		region, _ = firstNonEmpty(lookup, "DARKROOM_S3_REGION")
		endpoint, _ = firstNonEmpty(lookup, "DARKROOM_S3_ENDPOINT")
		style, _ := firstNonEmpty(lookup, "DARKROOM_S3_PATH_STYLE")
		return storeS3, bucket, region, endpoint, style == "true", nil
	default:
		return "", "", "", "", false, fmt.Errorf("DARKROOM_OBJECT_STORE is invalid: %q is not memory or s3", raw)
	}
}

// firstNonEmpty is a lookup that treats a whitespace-only value as absent.
//
// Because a compose file carrying `DARKROOM_S3_ENDPOINT:` with nothing after the
// colon produces a variable that IS set and IS empty, and "present but empty"
// falling through to a default is the difference between an S3 client pointed at
// AWS's real endpoint and one pointed at the local MinIO.
func firstNonEmpty(lookup Lookup, key string) (string, bool) {
	raw, ok := lookup(key)
	if !ok {
		return "", false
	}
	trimmed := strings.TrimSpace(raw)
	if trimmed == "" {
		return "", false
	}
	return trimmed, true
}

// isLogLevel is `error warn info debug trace off`, and the list is this
// function's whole body rather than a map lookup in the caller, so the answer to
// "is this a level" lives in one place.
func isLogLevel(raw string) bool {
	switch strings.ToLower(raw) {
	case "error", "warn", "info", "debug", "trace", "off":
		return true
	default:
		return false
	}
}
