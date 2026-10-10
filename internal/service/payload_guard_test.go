package service

// What the three payloads carry, held in place.
//
// The bus is a broadcast surface: every consumer on the platform receives every
// darkroom event. So an event carries the FACT — this asset is ready, this one is
// gone — and never the identifier of whose it was, and above all never a
// capability to read the object.
//
// `internal/outbox` refuses to publish an envelope carrying `account_id`, a
// storage key or a capability URL at all, which is the proof that a leak cannot
// reach the wire. This is the other half: a proof about the payloads THIS service
// builds, which needs no database and therefore runs on a bare machine where the
// harness-driven tests SKIP.
//
// It reads `service.go` as text, in the shape `internal/variants/variants_test.go`
// uses for its blank imports and `tests/tenant_scoping.rs` used for the Rust
// store's predicates, because the property being held is about three specific
// literals in one file.

import (
	"os"
	"regexp"
	"strings"
	"testing"
)

// payloadKey is one key inside a `map[string]any{…}` literal in the source.
var payloadKey = regexp.MustCompile(`"([a-z0-9_]+)"\s*:`)

// bannedPayloadKeys are the names no payload literal may carry. Subset of
// `internal/outbox`'s forbiddenDataKeys, narrowed to what these three payloads
// could plausibly have grown on their own.
var bannedPayloadKeys = map[string]bool{
	"account_id":    true, // the tenant id: the bus is a broadcast surface
	"tenant_id":     true,
	"tenant":        true,
	"account":       true,
	"storage_key":   true,
	"bucket":        true,
	"bucket_key":    true,
	"object_key":    true,
	"key":           true,
	"presigned":     true,
	"presigned_url": true,
	"url":           true,
	"uri":           true,
	"signature":     true,
	"secret":        true,
	"token":         true,
}

// readEnqueuePayloads pulls the map literal handed to each `enqueue` call out of
// `service.go`.
//
// It walks the balanced braces after `NewEvent(` rather than taking the next N
// lines, so a payload that gained a field does not silently fall outside the
// window this guard reads.
func readEnqueuePayloads(t *testing.T) map[string][]string {
	t.Helper()

	source, err := os.ReadFile("service.go")
	if err != nil {
		t.Fatalf("reading service.go: %v — this guard checks the SOURCE, so it cannot run from elsewhere", err)
	}
	text := string(source)

	out := map[string][]string{}
	for _, marker := range []string{"NewEvent("} {
		rest := text
		offset := 0
		for {
			at := strings.Index(rest, marker)
			if at < 0 {
				break
			}
			abs := offset + at
			// The event type is the first argument, read off the line the call is on.
			line := text[strings.LastIndexByte(text[:abs], '\n')+1:]
			line = line[strings.Index(line, "NewEvent("):]
			typeArg := strings.TrimSpace(strings.SplitN(strings.SplitN(line, "NewEvent(", 2)[1], ",", 2)[0])

			open := strings.Index(text[abs:], "{")
			if open < 0 {
				break
			}
			open += abs
			depth, end := 0, -1
			for i := open; i < len(text); i++ {
				switch text[i] {
				case '{':
					depth++
				case '}':
					depth--
					if depth == 0 {
						end = i
					}
				}
				if end > 0 {
					break
				}
			}
			if end < 0 {
				break
			}

			var keys []string
			for _, m := range payloadKey.FindAllStringSubmatch(text[open:end], -1) {
				keys = append(keys, m[1])
			}
			if len(keys) > 0 {
				out[typeArg] = keys
			}
			rest = text[end:]
			offset = end
		}
	}
	return out
}

// The whole guard: three payloads, none of them carrying a tenant id, a storage
// key or a capability — and the set of payloads is THREE, because a fourth event
// type is a `cafaye.yml` change and a core catalog row, not a code edit.
func TestNoPayloadCarriesATenantIDABucketKeyOrACapability(t *testing.T) {
	payloads := readEnqueuePayloads(t)
	if len(payloads) == 0 {
		t.Fatal("no payload literals were found in service.go: a guard that read none proves nothing")
	}
	if len(payloads) != len(eventTypesForTest()) {
		t.Errorf("found %d payload literals for %d event types: every event this service emits should have one",
			len(payloads), len(eventTypesForTest()))
	}

	for eventType, keys := range payloads {
		for _, key := range keys {
			if bannedPayloadKeys[key] {
				t.Errorf("%s's payload carries %q: the bus is a broadcast surface, so an event describes a fact and never carries the identifier of whose it was",
					eventType, key)
			}
		}
	}
}

// And the payloads carry what a consumer actually needs, so the guard above is
// not satisfied by an empty literal. A test that only checked for banned keys
// would pass on a payload that said nothing.
func TestThePayloadsCarryTheFactTheEventIsAbout(t *testing.T) {
	payloads := readEnqueuePayloads(t)

	mustHave := map[string]map[string]bool{
		// Keyed by the Go identifier the call site spells, which is what
		// readEnqueuePayloads reads off the line. The constant's VALUE is what a
		// consumer sees and is guarded by
		// TestEveryEventTypeHasThreeSegmentsAndThePublisherPrefix.
		"EventAssetReady": {
			"asset_id":     true,
			"kind":         true,
			"content_type": true,
			"byte_size":    true,
			"checksum":     true,
		},
		"EventAssetDeleted": {
			"asset_id": true,
		},
		"EventVariantCreated": {
			"variant_id":   true,
			"asset_id":     true,
			"kind":         true,
			"content_type": true,
			"width":        true,
			"height":       true,
		},
	}

	for eventType, want := range mustHave {
		got := payloads[eventType]
		if len(got) == 0 {
			t.Errorf("%s has no payload literal in service.go", eventType)
			continue
		}
		have := map[string]bool{}
		for _, key := range got {
			have[key] = true
		}
		for key := range want {
			if !have[key] {
				t.Errorf("%s's payload is missing %q: a consumer building on this event has nothing to act on", eventType, key)
			}
		}
	}
}
