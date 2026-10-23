// The transactional outbox: the event envelope and the only way to write one.
//
// # THE RULE THIS FILE MAKES HARD TO VIOLATE
//
// `core/docs/event-outbox.md`: "A service never publishes an event outside a
// transaction that also wrote the domain state it describes." `AGENTS.md` says the
// same from the other side: "One way to emit an event. `Outbox::enqueue` takes a
// `&mut Transaction`, and that is the entire design. […] If you find yourself wanting
// an `enqueue_after_commit`, you have described the bug `core/docs/event-outbox.md`
// is about."
//
// So [enqueue] takes a `pgx.Tx` and there is no second entry point, no variant that
// takes a pool, and no queue in memory. A function without a transaction has NO WAY
// to publish, which is the property: the guarantee is structural rather than a rule
// somebody has to remember. `TestARolledBackTransactionLeavesZeroOutboxRows` asserts
// the negative, and `bin/prime` runs it.
//
// # WHY THERE IS NO PUBLISHER LOOP, AND NO READ PATH
//
// A publisher that cannot publish is worse than an absent one, so there is none,
// and that is a decision rather than an unfinished-by-oversight. The Rust tree
// reached the same decision for the same reason — its `src/outbox.rs` said "The
// publisher loop that moves rows to NATS is **not** in this packet […] a publisher
// loop that cannot publish is better absent than silently no-op" — and
// `README.md`'s "Not done" carries the bullet forward, so the absence is stated
// where a reader will look for it. What is here is the table
// (`migrations/0002_outbox_events.sql`) and the insert path, which is the half
// that carries the correctness guarantee.
//
// # AND THERE IS NO READ PATH IN THIS FILE
//
// Not one, and that is the whole of the honest statement. An earlier version of
// this comment said "`claimUnpublished` exists for the publisher that a later
// packet adds, and it is the only read path in this file", and `grep -rn
// claimUnpublished .` returned that sentence and nothing else. It was wrong twice:
// there is no such function, and the only read path in this file was — and still
// is — [countOutbox], a test-only `select count(*)` used by
// `TestARolledBackTransactionLeavesZeroOutboxRows` and by nobody else. Nothing in
// a request path reads the queue.
//
// The Rust tree DID have it, and losing it is worth recording rather than
// quietly leaving as a gap: `src/outbox.rs` carried a real `claim_unpublished`
// with `OutboxRow`, `where published_at is null order by created_at limit $1 for
// update skip locked`, and the Go rewrite did not port it. It was not written
// again here on purpose, and the reason is specific rather than general: every
// function in this tree is reached by something — a handler, another function,
// or a test — and a read path with no caller would be the first one whose
// coverage depended on a reader of this comment rather than on a call site. It
// would also be in the one place where being wrong is a double-publish: an
// unexercised `for update skip locked` that turned out to be a plain `for
// update`, or to none, is a bug no compiler catches and no test in the tree
// would.
//
// # WHAT A PUBLISHER'S READ PATH HAS TO GET RIGHT
//
// Four things, none of which are this file's business until there is a publisher
// and none of which are obvious:
//
//   - `published_at is null` is the ONLY definition of unpublished.
//     `migrations/0002_outbox_events.sql` adds `published_at timestamptz null`
//     with no default and a partial index `outbox_events_unpublished_idx on
//     outbox_events (created_at) where published_at is null`, and that index's
//     comment states the intent: the publisher's only query is `where
//     published_at is null order by created_at`. So a row is published when
//     `published_at` is set, and unpublished until then — NOT when `attempts` is
//     zero, and not when some in-memory queue says so. `attempts` exists as a
//     column and nothing in this tree writes it.
//
//   - `order by created_at`, and the reason is per-subject ordering. `time` is the
//     moment of the STATE CHANGE, not the moment of the insert
//     ([NewEvent]'s argument), so two events for one asset can sit in
//     `created_at` order differently from the order the state changed in. A
//     consumer that builds a thumbnail from `darkroom.asset.ready` and one that
//     deletes from `darkroom.asset.deleted` will see them reordered if the claim
//     sorts by anything else.
//
//   - `for update skip locked`, or an equivalent, and this is the multi-replica
//     problem. A plain `select … limit n` from N replicas hands the same n rows
//     to every one of them, and every one of them publishes. `skip locked` makes
//     the second replica walk past rows the first has locked, which is what lets
//     replicas run without contending. A bare `for update` is worse than no lock
//     here: the replicas serialize on the same head-of-queue rows instead of
//     skipping them, so correctness improves and throughput collapses.
//
//   - The claim and the `published_at` write belong in ONE transaction, or a
//     publisher that crashes between them republishes forever. That is also why
//     [Event.ID] is minted before the insert: "a retry publishes the same `id`
//     and the consumer's dedupe key actually dedupes" ([Event.Envelope]). A
//     duplicate delivery is survivable; an unbounded one is not.
//
// # NO EVENT FOR A `pending` ASSET, AND NO EVENT FOR A FAILURE
//
// Both are stated as rules in `AGENTS.md` and both are load-bearing:
//
//   - "No event for a `pending` asset. Nothing is in storage, so every consumer
//     would fail. `darkroom.asset.ready` is at complete, and that is the moment the
//     bytes exist."
//   - "No event for a failure. A consumer cannot act on 'this upload did not
//     complete' in a way that is better served by the client receiving its 4xx."
package service

import (
	"context"
	"encoding/json"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
)

// Source is the service name, which is also the envelope `source` and the first
// segment of every event type. `anywaye.yml` `exposes.events` names it; a test asserts
// the two agree rather than trusting this constant.
const Source = "darkroom"

// EventType is a published event type. Three segments, past tense, from core's action
// vocabulary.
type EventType string

// The three events this service emits, and the three it must not.
const (
	// EventAssetReady is the upload completing and the bytes being in storage. NOT
	// the moment the presigned URL was issued: "A consumer that builds a thumbnail
	// on `ready` and a consumer that notifies a user both need the bytes to exist;
	// neither can work at `created`."
	EventAssetReady EventType = "darkroom.asset.ready"
	// EventAssetDeleted is the asset and its storage object being gone.
	EventAssetDeleted EventType = "darkroom.asset.deleted"
	// EventVariantCreated is a derived image existing and being servable.
	EventVariantCreated EventType = "darkroom.variant.created"
)

// eventTypes is every type this package can emit. A guard walks it, because an event
// type is a routing contract and a typo in one surfaces only when a consumer tries to
// subscribe — which is a day, not a build.
var eventTypes = []EventType{EventAssetReady, EventAssetDeleted, EventVariantCreated}

// Event is an event about to be written into `outbox_events`.
//
// The `id` is minted HERE, before the insert, and is the same value on every retry:
// core says "The row and the message carry one identity, so a retry publishes the
// same `id` and the consumer's dedupe key actually dedupes."
type Event struct {
	ID        string
	Type      EventType
	Subject   string
	EventTime time.Time
	Data      map[string]any
}

// NewEvent builds an event about `subject`.
//
// `at` is the moment of the STATE CHANGE and not the moment of the insert: "a row
// that sat unpublished for an hour still reports when the change happened." Every
// caller in this package passes the same instant it put on the row.
func NewEvent(eventType EventType, subject string, at time.Time, data map[string]any) *Event {
	return &Event{
		ID:        NewUUID(),
		Type:      eventType,
		Subject:   subject,
		EventTime: at,
		Data:      orEmpty(data),
	}
}

// Envelope is the CloudEvents shape a publisher would put on the bus. It is
// constructed here rather than in a publisher so the row and the message cannot
// describe different things.
func (e *Event) Envelope() map[string]any {
	return map[string]any{
		"specversion": "1.0",
		"id":          e.ID,
		"type":        string(e.Type),
		"source":      Source,
		"subject":     e.Subject,
		"time":        e.EventTime.UTC().Format(time.RFC3339Nano),
		"data":        e.Data,
	}
}

// enqueue inserts the event into `outbox_events`, INSIDE `tx`.
//
// THE PARAMETER IS THE DESIGN. There is no version taking a pool or a connection,
// so there is no code path that can enqueue outside a transaction: if the caller
// rolls back, this row goes with it, and a commit that fails to publish leaves a
// `ready` asset and no event rather than an event about an asset that does not
// exist.
func enqueue(ctx context.Context, tx pgx.Tx, event *Event) error {
	data, err := json.Marshal(event.Data)
	if err != nil {
		return fmt.Errorf("enqueue(%s): marshaling data: %w", event.Type, err)
	}
	if _, err := tx.Exec(ctx, `insert into outbox_events (id, event_type, source, subject, time, data)
		values ($1::uuid, $2, $3, $4, $5, $6::jsonb)`,
		event.ID, string(event.Type), Source, event.Subject, event.EventTime, data); err != nil {
		return fmt.Errorf("enqueue(%s): %w", event.Type, err)
	}
	return nil
}

// countOutbox is how the tests read the queue back. It is a test-only helper rather
// than part of the service's API, because nothing in a request path counts events
// and a function that exists only for a test should say so in its name.
func countOutbox(ctx context.Context, tx pgx.Tx) (int, error) {
	var n int
	if err := tx.QueryRow(ctx, `select count(*) from outbox_events`).Scan(&n); err != nil {
		return 0, fmt.Errorf("countOutbox: %w", err)
	}
	return n, nil
}

// eventTypesForTest exposes the type list to the guard in the tests without
// exporting it: an exported slice is a mutable package-level variable that any
// importer could empty, and a guard that walks an empty slice passes having checked
// nothing.
func eventTypesForTest() []EventType {
	out := make([]EventType, len(eventTypes))
	copy(out, eventTypes)
	return out
}
