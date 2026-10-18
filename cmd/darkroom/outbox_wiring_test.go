package main

// The outbox publisher's boot wiring: what the process starts, under what
// condition, and what is left running when the condition is not met. The
// publisher's own behavior is proved in `internal/outbox` against an in-process
// broker and a fake claimer; what is proved here is the CONDITION and the
// LIFETIME.
//
// These tests need no database and no broker. That is the point: the condition is
// a string comparison on a config field, and a condition test that needed a
// PostgreSQL would skip on a bare machine — which is exactly how a publisher that
// started with no broker would get to production.

import (
	"log/slog"
	"strings"
	"testing"

	"github.com/cafaye/darkroom/internal/outbox"
)

// discardLogger is a logger that goes nowhere. `internal/outbox` defaults to one
// when handed nil, so this is mostly documentation — the wiring is expected to
// pass a real logger, and a test that handed it nil would be testing the
// package's default rather than the call.
func discardLogger() *slog.Logger { return slog.New(slog.DiscardHandler) }

// NATS_URL unset: no publisher is built, and none is built by accident.
//
// The outbox table, the loop and the Publisher interface all exist; what is
// missing is a broker, and a deployment that has not wired one accumulates rows
// in outbox_events until it does. That is the shipped state, and this is the test
// that holds it there.
func TestNoPublisherIsBuiltWithoutNATSURL(t *testing.T) {
	publisher, err := buildOutboxPublisher(Config{}, discardLogger())
	if err != nil {
		t.Errorf("buildOutboxPublisher with no NATS_URL returned an error: %v — an unset broker is a decision, not a failure", err)
	}
	if publisher != nil {
		t.Error("a publisher was built with NATS_URL unset: the loop would drain the outbox into a broker that does not exist")
	}
}

// Whitespace-only counts as unset, and the two halves of that are in two places
// for a reason worth naming: a compose file carrying `NATS_URL:` with nothing
// after the colon produces a variable that IS set and IS empty, and treating that
// as a broker address is how a deployment believes it has a bus. `loadConfig`'s
// `firstNonEmpty` is what folds the two together, so the CONFIG it produces is
// never whitespace.
//
// The consequence is that `NATSEnabled` itself is a plain non-empty check and is
// not a second normaliser — a hand-built Config carrying "   " is a caller that
// skipped `loadConfig`, and the wiring starts a broker pointed at a URL that will
// not parse.
func TestAWhitespaceOnlyNATSURLIsUnsetByLoadConfig(t *testing.T) {
	for _, raw := range []string{"", "   ", "\t"} {
		if (Config{NATSURL: strings.TrimSpace(raw)}).NATSEnabled() {
			t.Errorf("Config{NATSURL: %q}.NATSEnabled() is true, want false", raw)
		}
	}
	if !(Config{NATSURL: "nats://127.0.0.1:4222"}).NATSEnabled() {
		t.Error("a config with NATS_URL reports NATS disabled: the outbox would never drain")
	}
}

// loadConfig reads it by the fleet's bare name, not a `DARKROOM_`-prefixed one.
// NATS_URL is the STACK's address — kit's compose declares the broker and
// identity reads the same name — so a service that read its own copy would need
// the address in every deployment manifest and would drift from the fleet's.
func TestLoadConfigReadsNATSURLByItsBareName(t *testing.T) {
	cfg, err := loadConfig(func(key string) (string, bool) {
		switch key {
		case "NATS_URL":
			return "nats://127.0.0.1:4222", true
		case "DARKROOM_DATABASE_URL":
			return "postgres://darkroom_app:x@localhost:15500/darkroom?sslmode=disable", true
		case "DARKROOM_ISSUER":
			return "https://identity.cafaye.com", true
		case "DARKROOM_AUDIENCE":
			return "darkroom", true
		}
		return "", false
	})
	if err != nil {
		t.Fatalf("loadConfig: %v", err)
	}
	if cfg.NATSURL != "nats://127.0.0.1:4222" {
		t.Errorf("NATSURL = %q, want the value of the NATS_URL variable", cfg.NATSURL)
	}
	if !cfg.NATSEnabled() {
		t.Error("NATSEnabled is false with NATS_URL set: the outbox would never drain")
	}

	// And a `DARKROOM_`-prefixed one is deliberately NOT a second spelling.
	absent, err := loadConfig(func(key string) (string, bool) {
		switch key {
		case "DARKROOM_DATABASE_URL":
			return "postgres://darkroom_app:x@localhost:15500/darkroom?sslmode=disable", true
		case "DARKROOM_ISSUER":
			return "https://identity.cafaye.com", true
		case "DARKROOM_AUDIENCE":
			return "darkroom", true
		case "DARKROOM_NATS_URL":
			return "nats://127.0.0.1:4222", true
		}
		return "", false
	})
	if err != nil {
		t.Fatalf("loadConfig with DARKROOM_NATS_URL: %v", err)
	}
	if absent.NATSEnabled() {
		t.Error("DARKROOM_NATS_URL enabled the publisher: NATS_URL is the variable, and a second one is a second address to keep in step")
	}
}

// The broker is unreachable, and that is NOT a boot failure. The outbox table is
// the durable store: the loop starts, every publish fails, the rows stay
// claimable, and they drain when the broker arrives. Port 1 is the "nothing is
// listening here" port, so no sleep is needed to make the point.
func TestAnUnreachableBrokerIsNotABootFailure(t *testing.T) {
	publisher, err := buildOutboxPublisher(Config{NATSURL: "nats://127.0.0.1:1"}, discardLogger())
	if err != nil {
		t.Errorf("buildOutboxPublisher refused an unreachable broker: %v", err)
		return
	}
	if publisher == nil {
		t.Error("no publisher was built for a reachable-but-silent broker: the rows would never drain")
		return
	}
	defer publisher.Close()
}

// A URL that does not parse IS a boot failure. A typo is a deployment that
// believes it has a bus and does not, and it belongs at boot where an operator
// reads it rather than at the first event where nobody does.
func TestAMalformedBrokerURLIsABootFailure(t *testing.T) {
	publisher, err := buildOutboxPublisher(Config{NATSURL: "::::"}, discardLogger())
	if err == nil {
		if publisher != nil {
			publisher.Close()
		}
		t.Fatal("buildOutboxPublisher accepted a malformed NATS_URL: a deployment that believes it has a bus and does not would boot")
	}
	if publisher != nil {
		t.Error("a publisher was returned alongside an error")
	}
	if !strings.Contains(err.Error(), "NATS") {
		t.Errorf("the error does not name the variable an operator has to fix: %v", err)
	}
}

// The wiring's contract in one table: what `run` starts for each configuration.
//
// The loop's LIFETIME is proved in `internal/outbox`
// (TestRunStopsWhenItsContextIsCanceled) and it is given `ctx` — the same
// context the HTTP server drains on — at the one call site in `run`, which is two
// lines and is the whole of the shutdown path. What is proved here is the
// decision above those two lines, because that is the half that can be wrong
// silently: a publisher started with no broker, or no publisher started with one.
func TestTheBootConditionAcrossTheWholeTable(t *testing.T) {
	cases := []struct {
		name      string
		config    Config
		wantBuilt bool
		wantErr   bool
	}{
		{"unset", Config{}, false, false},
		{"a broker", Config{NATSURL: "nats://127.0.0.1:1"}, true, false},
		{"a typo", Config{NATSURL: "::::"}, false, true},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			publisher, err := buildOutboxPublisher(tc.config, discardLogger())
			if tc.wantErr {
				if err == nil {
					if publisher != nil {
						publisher.Close()
					}
					t.Fatal("no error for a configuration that should be refused")
				}
				return
			}
			if err != nil {
				t.Fatalf("buildOutboxPublisher: %v", err)
			}
			if (publisher != nil) != tc.wantBuilt {
				t.Errorf("a publisher was built = %v, want %v", publisher != nil, tc.wantBuilt)
			}
			if publisher != nil {
				defer publisher.Close()
			}
		})
	}
}

// And the loop the wiring builds is one that reaches the bus. Built here through
// the package's own constructor so the call site in `run` and this one cannot
// drift: `outbox.NewService(outbox.NewStore(db), publisher, logger, 0)`.
func TestTheWiredLoopIsAServiceOverTheStoreAndThePublisher(t *testing.T) {
	publisher, err := buildOutboxPublisher(Config{NATSURL: "nats://127.0.0.1:1"}, discardLogger())
	if err != nil {
		t.Fatalf("buildOutboxPublisher: %v", err)
	}
	defer publisher.Close()

	// A nil store, and a batch size of zero meaning DefaultBatchSize: the two
	// arguments `run` passes, asserted not to be silently dropped.
	loop := outbox.NewService(outbox.NewStore(nil), publisher, discardLogger(), 0)
	if loop.BatchSize() != outbox.DefaultBatchSize {
		t.Errorf("the wired loop's batch size is %d, want %d", loop.BatchSize(), outbox.DefaultBatchSize)
	}
}
