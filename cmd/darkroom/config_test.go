// The configuration table, and it is a table rather than a handful of `if`s
// because every row is a deployment that went wrong in a specific way.
//
// The rules under test are in `config.go`'s header. What is asserted here is that
// each is still true, which is the only reason the file can be read as a list of
// decisions rather than as code.
//
// No database, no socket, no sleep: `Lookup` is a map, so this suite is a function
// call — `AGENTS.md`'s "no network in tests" holds by construction.
package main

import (
	"strings"
	"testing"

	"github.com/cafaye/darkroom/internal/objectstore"
)

// env builds a Lookup from a map, which is the whole of the seam.
func env(pairs map[string]string) Lookup {
	return func(key string) (string, bool) {
		v, ok := pairs[key]
		return v, ok
	}
}

// valid is the smallest environment that starts. Every refusal row adds one
// variable's worth of breakage to this, so a row that asserts "missing X fails"
// is testing the absence of exactly one thing.
func valid() map[string]string {
	return map[string]string{
		"DARKROOM_DATABASE_URL": "postgres://darkroom_app:cafaye@localhost:15500/darkroom?sslmode=disable",
		"DARKROOM_ISSUER":       "https://identity.cafaye.com",
		"DARKROOM_AUDIENCE":     "darkroom",
	}
}

// with returns a copy of valid with one key set, so each row is readable on its
// own line instead of as a diff against a shared literal.
func with(key, value string) Lookup { return env(merge(valid(), map[string]string{key: value})) }

// merge copies, because a test that mutated the shared map would make the next
// row depend on the order this one ran in.
func merge(base, extra map[string]string) map[string]string {
	out := make(map[string]string, len(base)+len(extra))
	for k, v := range base {
		out[k] = v
	}
	for k, v := range extra {
		out[k] = v
	}
	return out
}

func TestTheDefaultsAreTheDocumentedOnes(t *testing.T) {
	cfg, err := loadConfig(env(valid()))
	if err != nil {
		t.Fatalf("a minimal valid environment was refused: %v", err)
	}
	if cfg.Port != defaultPort {
		t.Errorf("Port = %d, want %d", cfg.Port, defaultPort)
	}
	if cfg.addr() != "0.0.0.0:8080" {
		t.Errorf("addr() = %q, want 0.0.0.0:8080", cfg.addr())
	}
	if cfg.BindAddr != defaultBindAddr {
		t.Errorf("BindAddr = %q, want %q", cfg.BindAddr, defaultBindAddr)
	}
	if cfg.ObjectStore != storeMemory {
		t.Errorf("ObjectStore = %q, want memory", cfg.ObjectStore)
	}
	if cfg.LogLevel != defaultLogLevel || cfg.LogFormat != defaultLogFormat {
		t.Errorf("log = %q/%q, want %q/%q", cfg.LogLevel, cfg.LogFormat, defaultLogLevel, defaultLogFormat)
	}
	// The dev verifier is GONE in this tree, so this is recorded and nothing more.
	// `main.go`'s header says so; asserting it here is what stops the variable
	// quietly being trusted by a reader who skimmed the compose file.
	if cfg.Environment != "production" {
		t.Errorf("Environment = %q, want production", cfg.Environment)
	}
	// `auth.New` derives the JWKS path from the issuer, so an absent URL is a
	// supported state rather than a required field the Rust tree demanded.
	if cfg.JWKSURL != "" {
		t.Errorf("JWKSURL = %q, want empty so auth.New derives it from the issuer", cfg.JWKSURL)
	}
}

// TestDATABASEURLIsNotAFallbackForTheLoginRole is the one row that is a security
// property rather than a convenience.
//
// `AGENTS.md`: "`DATABASE_URL` is `darkroom`, the OWNER. `DARKROOM_DATABASE_URL`
// is `darkroom_app`, the LOGIN the application uses." Accepting the owner as a
// fallback would run the application with the one credential that can
// `ALTER TABLE ... DISABLE ROW LEVEL SECURITY`, silently, in exactly the
// deployments that forgot a variable.
func TestDATABASEURLIsNotAFallbackForTheLoginRole(t *testing.T) {
	// The OWNER url INSTEAD OF the login url, not alongside it — the deployment
	// this row is about is one where somebody set the variable this repository
	// does not read and never set the one it does.
	only := merge(valid(), map[string]string{
		"DATABASE_URL": "postgres://darkroom:cafaye@localhost:15500/darkroom?sslmode=disable",
	})
	delete(only, "DARKROOM_DATABASE_URL")

	cfg, err := loadConfig(env(only))
	if err == nil {
		t.Fatalf("a deployment carrying only the OWNER url started, with DatabaseURL = %q", cfg.DatabaseURL)
	}
	if !strings.Contains(err.Error(), "DARKROOM_DATABASE_URL") {
		t.Errorf("the refusal does not name the variable to set: %v", err)
	}
}

func TestARefusalNamesTheVariableItIsAbout(t *testing.T) {
	for _, tc := range []struct {
		name  string
		env   map[string]string
		wants string
	}{
		{
			name:  "no issuer",
			env:   map[string]string{"DARKROOM_ISSUER": ""},
			wants: "DARKROOM_ISSUER",
		},
		{
			name:  "no audience",
			env:   map[string]string{"DARKROOM_AUDIENCE": ""},
			wants: "DARKROOM_AUDIENCE",
		},
		{
			name:  "s3 with no bucket",
			env:   map[string]string{"DARKROOM_OBJECT_STORE": "s3"},
			wants: "DARKROOM_S3_BUCKET",
		},
		{
			name:  "a port that is not a port",
			env:   map[string]string{"PORT": "eighty-eighty"},
			wants: "PORT",
		},
		{
			name:  "a port out of range",
			env:   map[string]string{"PORT": "70000"},
			wants: "PORT",
		},
		{
			name:  "a log level that is not a level",
			env:   map[string]string{"DARKROOM_LOG_LEVEL": "chatty"},
			wants: "DARKROOM_LOG_LEVEL",
		},
		{
			name:  "a log format that is not a format",
			env:   map[string]string{"DARKROOM_LOG_FORMAT": "yaml"},
			wants: "DARKROOM_LOG_FORMAT",
		},
		{
			name:  "an object store that is neither",
			env:   map[string]string{"DARKROOM_OBJECT_STORE": "gcs"},
			wants: "DARKROOM_OBJECT_STORE",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, err := loadConfig(env(merge(valid(), tc.env)))
			if err == nil {
				t.Fatalf("%v started", tc.env)
			}
			if !strings.Contains(err.Error(), tc.wants) {
				t.Errorf("the refusal names %q, want it to name %s: %v", tc.wants, tc.wants, err)
			}
		})
	}
}

// TestAnS3ConfigurationIsCarriedThroughUnexamined is the row that holds the
// delegation to `objectstore.S3Config.normalise`.
//
// The R2 rules are NOT restated in `config.go` — that is the design — so this
// test asserts the boundary rather than the rules: whatever the operator wrote
// reaches the store verbatim, and the store is what refuses. A second copy of the
// rules in configuration would pass this test, which is why the comment below the
// assertion names what the rules actually are.
func TestAnS3ConfigurationIsCarriedThroughUnexamined(t *testing.T) {
	for _, tc := range []struct {
		name      string
		region    string
		endpoint  string
		pathStyle string
	}{
		{name: "r2 with auto", region: "auto", endpoint: "https://acct.r2.cloudflarestorage.com"},
		{name: "r2 with no region at all", endpoint: "https://acct.r2.cloudflarestorage.com"},
		{name: "r2 with the alias", region: "us-east-1", endpoint: "https://acct.r2.cloudflarestorage.com"},
		// The two refusals below are `objectstore`'s to make. Configuration carries
		// both of them without an opinion, and `TestTheR2RulesLiveInTheStore` is
		// where the refusal itself is shown.
		{name: "r2 with a real region", region: "eu-west-1", endpoint: "https://acct.r2.cloudflarestorage.com"},
		{name: "auto with no endpoint", region: "auto"},
		{name: "aws", region: "us-east-1"},
		{name: "minio", region: "us-east-1", endpoint: "http://localhost:9000", pathStyle: "true"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			cfg, err := loadConfig(env(merge(valid(), map[string]string{
				"DARKROOM_OBJECT_STORE":  "s3",
				"DARKROOM_S3_BUCKET":     "a-bucket",
				"DARKROOM_S3_REGION":     tc.region,
				"DARKROOM_S3_ENDPOINT":   tc.endpoint,
				"DARKROOM_S3_PATH_STYLE": tc.pathStyle,
			})))
			if err != nil {
				t.Fatalf("configuration refused %s, but the R2 rules are the store's: %v", tc.name, err)
			}
			if cfg.S3Bucket != "a-bucket" || cfg.S3Region != tc.region || cfg.S3Endpoint != tc.endpoint {
				t.Errorf("carried %q/%q/%q, want %q/%q/%q verbatim",
					cfg.S3Bucket, cfg.S3Region, cfg.S3Endpoint, "a-bucket", tc.region, tc.endpoint)
			}
			if cfg.S3PathStyle != (tc.pathStyle == "true") {
				t.Errorf("PathStyle = %v for %q", cfg.S3PathStyle, tc.pathStyle)
			}
			if cfg.ObjectStore != storeS3 {
				t.Errorf("ObjectStore = %q, want s3", cfg.ObjectStore)
			}
		})
	}
}

// TestTheR2RulesLiveInTheStore is the other half of the pair above, and it is
// here rather than in `internal/objectstore` because the THING BEING ASSERTED is
// the delegation: the two configurations configuration declined to judge are the
// two the store refuses. If a future copy of these rules moved into
// `config.go` and started accepting them, this row goes red.
//
// It costs one call into `objectstore` and it is what makes the previous test
// more than a claim in a comment.
func TestTheR2RulesLiveInTheStore(t *testing.T) {
	for _, tc := range []struct {
		name   string
		region string
		endpt  string
		refuse bool
	}{
		{name: "r2 with auto", region: "auto", endpt: "https://acct.r2.cloudflarestorage.com"},
		{name: "r2 with no region", endpt: "https://acct.r2.cloudflarestorage.com"},
		{name: "aws", region: "us-east-1"},
		{name: "r2 with a real region", region: "eu-west-1", endpt: "https://acct.r2.cloudflarestorage.com", refuse: true},
		{name: "auto with no endpoint", region: "auto", refuse: true},
		{name: "aws with no region", refuse: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			// The store refuses at NORMALISE, before any client is built, so this
			// opens no socket and reads no credential: `New` validates first and
			// only then reaches the SDK's chain.
			_, err := objectstore.New(t.Context(), objectstore.S3Config{
				Bucket:   "a-bucket",
				Region:   tc.region,
				Endpoint: tc.endpt,
			})
			if tc.refuse && err == nil {
				t.Errorf("objectstore accepted region=%q endpoint=%q; the rules are meant to live there, not in configuration", tc.region, tc.endpt)
			}
			if !tc.refuse && err != nil {
				t.Errorf("objectstore refused region=%q endpoint=%q: %v", tc.region, tc.endpt, err)
			}
		})
	}
}

// TestAWhitespaceOnlyValueIsAbsent is the compose-file row.
//
// `DARKROOM_S3_ENDPOINT:` with nothing after the colon produces a variable that
// IS set and IS empty. Treating it as a value would point an S3 client at AWS's
// real endpoint instead of the local one, with no error anywhere.
func TestAWhitespaceOnlyValueIsAbsent(t *testing.T) {
	cfg, err := loadConfig(env(merge(valid(), map[string]string{
		"DARKROOM_S3_ENDPOINT": "   ",
		"DARKROOM_BIND_ADDR":   "",
	})))
	if err != nil {
		t.Fatalf("a whitespace-only value was treated as a refusal: %v", err)
	}
	if cfg.S3Endpoint != "" {
		t.Errorf("S3Endpoint = %q, want empty", cfg.S3Endpoint)
	}
	if cfg.BindAddr != defaultBindAddr {
		t.Errorf("BindAddr = %q, want the default rather than an empty listen address", cfg.BindAddr)
	}
}

func TestTheEnvironmentIsReadAndNothingElseIsReadFromIt(t *testing.T) {
	cfg, err := loadConfig(with("DARKROOM_ENV", "development"))
	if err != nil {
		t.Fatalf("a development environment was refused: %v", err)
	}
	if cfg.Environment != "development" {
		t.Errorf("Environment = %q, want development", cfg.Environment)
	}
	// `DARKROOM_DB_MAX_CONNECTIONS` is named in `.env.example` and is NOT read:
	// `darkroomdb.Open` owns the pool size from the shared cluster's per-role cap.
	// A variable that looked like it configured it would be a lie in a deployment
	// manifest, so a test asserts it is inert rather than trusting the comment.
	cfg, err = loadConfig(with("DARKROOM_DB_MAX_CONNECTIONS", "10"))
	if err != nil {
		t.Fatalf("an unrecognised variable was treated as a refusal: %v", err)
	}
	if cfg == (Config{}) {
		t.Error("loadConfig returned nothing at all")
	}
}

// A BIND ADDRESS THAT ALREADY CARRIES A PORT IS NOT A HOST AND A COLON.
//
// This is the case the reading rejected, and it was found by running the binary
// rather than by reading it: `DARKROOM_BIND_ADDR=127.0.0.1:8081` reached
// `addr()` as a bare host, was joined with `PORT`, and produced
// `127.0.0.1:8081:8080`. The listener refused it with "too many colons in
// address", so the crash was loud — but it was a crash on startup for a value
// that reads as a complete and correct listen address, and nothing in the suite
// covered the join because every test set a bare host.
func TestABindAddrThatCarriesItsOwnPortIsUsedWhole(t *testing.T) {
	cfg, err := loadConfig(with("DARKROOM_BIND_ADDR", "127.0.0.1:8081"))
	if err != nil {
		t.Fatalf("a bind address with a port was refused: %v", err)
	}
	if got := cfg.addr(); got != "127.0.0.1:8081" {
		t.Errorf("addr() = %q, want 127.0.0.1:8081 — PORT must not be appended to an address that already has one", got)
	}
}

// And the bare-host reading is NOT lost while fixing that, because it is what
// `PORT` is for and what the Dockerfile's `DARKROOM_BIND_ADDR=0.0.0.0` means.
func TestABareBindHostStillTakesPort(t *testing.T) {
	cfg, err := loadConfig(env(merge(valid(), map[string]string{
		"DARKROOM_BIND_ADDR": "127.0.0.1",
		"PORT":               "8081",
	})))
	if err != nil {
		t.Fatalf("a bare bind host with PORT was refused: %v", err)
	}
	if got := cfg.addr(); got != "127.0.0.1:8081" {
		t.Errorf("addr() = %q, want 127.0.0.1:8081", got)
	}
}
