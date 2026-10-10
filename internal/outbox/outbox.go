// Package outbox drains darkroom's transactional outbox onto the bus.
//
// # WHY THE CLAIM AND THE PUBLISH ARE HERE, AND THE INSERT IS NOT
//
// `internal/service/outbox.go` owns the WRITE half and says so at length: the
// insert takes a `pgx.Tx`, there is no second entry point, and a rolled-back
// transaction leaves zero rows. That half carries the correctness guarantee and
// it is already proved.
//
// This package is the READ half, and until this packet it did not exist. The
// same file said "a publisher that cannot publish is worse than an absent one,
// so there is none, and that is a decision rather than an unfinished-by-oversight"
// — which was true when there was no broker. kit's BRK-1 installed one and
// pointed this service at it, so the reason the file gave for the absence has
// gone and the publisher with it.
//
// The Rust tree DID have the read half and the Go rewrite did not port it;
// `internal/service/outbox.go` records that loss rather than quietly leaving a
// gap. This is where it comes back.
//
// # THE LOOP IS TIED TO THE PROCESS, AND IT IS THE ONLY THING THAT MOVES A ROW
//
// Nothing in a request path reads the queue. `Service.Run` is started once at
// boot on the process's shutdown context — the same one the HTTP server drains
// on — so a SIGTERM stops both. A request that had to publish would couple an
// upload's latency to a broker's, which is the entire failure mode the table
// exists to remove.
//
// # WHAT THE ROW COLUMNS ARE
//
// `migrations/0002_outbox_events.sql` stores `event_type, source, subject,
// time, data` — which is the envelope minus `specversion` and `id`, and `id` is
// the row's own primary key. So the envelope a consumer receives is RECONSTRUCTED
// from the row rather than stored beside it, and the reconstruction cannot
// disagree with the row because it IS the row. `service.Event.Envelope()` builds
// the same seven attributes on the write side; the two are held apart by
// TestTheEnvelopeCarriesExactlyTheRequiredAttributes on each side rather than by
// a shared function, because the shared function would have to live in one
// package and the row is the other's.
package outbox

import (
	"encoding/json"
	"time"
)

// Source is darkroom, which is the envelope `source` and the first segment of
// every type. It is `service.Source` read off a row, and a row that carried
// another publisher's name here would be a row this service did not write.
const Source = "darkroom"

// SpecVersion pins the envelope dialect. core's schema makes it a const "1.0" and
// says it increments only for a breaking envelope change.
const SpecVersion = "1.0"

// Event is one claimed outbox row.
//
// It is deliberately NOT the write side's `service.Event`: that one carries a
// `map[string]any` built by the handler and a `NewUUID()` this package would then
// have to trust. This one is read out of the table, so its `Data` is exactly the
// bytes that were inserted, and its `ID` is the primary key the consumers
// deduplicate on.
type Event struct {
	ID      string
	Type    string
	Source  string
	Subject string
	Time    time.Time
	Data    json.RawMessage
}

// Envelope is the cafaye event envelope, reproducing
// cafaye/core/schemas/event-envelope.schema.json.
//
// The JSON tags are the wire contract. core sets `additionalProperties: false`,
// so these seven attributes and nothing else are ever emitted: an undeclared one
// is rejected by every consumer on the platform.
//
// `subject` has no `omitempty` because core REQUIRES it — a required field with a
// reserved value, not an absent one. An event with no subject is refused by
// [Envelope.Validate] rather than emitted with an empty string that core's
// `minLength: 1` would then reject at every consumer.
type Envelope struct {
	SpecVersion string          `json:"specversion"`
	ID          string          `json:"id"`
	Type        string          `json:"type"`
	Source      string          `json:"source"`
	Subject     string          `json:"subject"`
	Time        string          `json:"time"`
	Data        json.RawMessage `json:"data"`
}

// Envelope builds the wire form of a claimed row.
//
// `time` is the moment of the STATE CHANGE, read off the row, and not the moment
// of the publish: "a row that sat unpublished for an hour still reports when the
// change happened." It is re-formatted here rather than passed through, because
// the column is `timestamptz` and the contract is RFC3339 UTC.
func (e Event) Envelope() Envelope {
	return Envelope{
		SpecVersion: SpecVersion,
		ID:          e.ID,
		Type:        e.Type,
		Source:      e.Source,
		Subject:     e.Subject,
		Time:        e.Time.UTC().Format(time.RFC3339Nano),
		Data:        e.Data,
	}
}
