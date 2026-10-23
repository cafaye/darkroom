package outbox

// The loop's proof, without a database.
//
// This package's tests do not use `pgtest.Require`, and that is not a shortcut:
// the database suites in this repository SKIP with a named reason when no
// PostgreSQL answers (`mise.toml`, `bin/prime`), and a loop whose correctness
// depended on them would be unproved on every bare machine in CI. The CLAIM is
// `Claimer`, an interface, precisely so the drain can be driven by a fake — and
// the fake is where the interesting behavior lives, because the interesting
// behavior is what the loop does with what it was handed: mark what the
// publisher accepted, leave what it refused, stop when its context dies.
//
// The transport is proved against a real in-process broker in nats_test.go.

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"
)

// ----------------------------------------------------------------------------
// fakes
// ----------------------------------------------------------------------------

// fakeBatch is a claimed set of events with no transaction behind it. It records
// every mark, which is what the loop's behavior is observable through: a row
// marked published that the publisher never accepted is the bug, and a row left
// unmarked after a successful publish is the other one.
type fakeBatch struct {
	events []Event

	mu        sync.Mutex
	published []string
	failed    []string
	released  bool
	committed bool
}

func (b *fakeBatch) Events() []Event { return b.events }

func (b *fakeBatch) MarkPublished(_ context.Context, eventID string) error {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.published = append(b.published, eventID)
	return nil
}

func (b *fakeBatch) MarkFailed(_ context.Context, eventID string) error {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.failed = append(b.failed, eventID)
	return nil
}

func (b *fakeBatch) Commit(context.Context) error {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.committed = true
	return nil
}

func (b *fakeBatch) Release(context.Context) error {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.released = true
	return nil
}

func (b *fakeBatch) counts() (published, failed int, released, committed bool) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return len(b.published), len(b.failed), b.released, b.committed
}

// fakeClaimer hands out one batch and then goes dry. batches counts the claim
// calls, which is what the loop's poll behavior is measured through.
type fakeClaimer struct {
	batches []*fakeBatch
	err     error

	mu       sync.Mutex
	claims   int
	lastSize int
}

func (c *fakeClaimer) Claim(_ context.Context, limit int) (Batch, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.claims++
	c.lastSize = limit
	if c.err != nil {
		return nil, c.err
	}
	if len(c.batches) == 0 {
		return &fakeBatch{}, nil
	}
	b := c.batches[0]
	c.batches = c.batches[1:]
	return b, nil
}

func (c *fakeClaimer) claimCount() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.claims
}

// capturingPublisher records every envelope it was handed, and refuses the ones
// its refuse func names. It is the transport double, and the refusal is what
// makes the loop's failure path observable.
//
// `received` is every ATTEMPT and `accepted` is every DELIVERY. The two differ
// exactly when the transport refused, which is the distinction the loop's failure
// path is proved through: an envelope the transport refused was still handed to
// the transport, and that is what proves the queue was not stalled.
type capturingPublisher struct {
	refuse func(Envelope) error

	mu       sync.Mutex
	received []Envelope
	accepted []Envelope
}

func (p *capturingPublisher) Publish(_ context.Context, e Envelope) error {
	// Recorded BEFORE the refusal, because an attempt that was refused is an
	// attempt: the loop must have handed this envelope to the transport for the
	// queue to be proved unstalled.
	p.mu.Lock()
	p.received = append(p.received, e)
	refuse := p.refuse
	p.mu.Unlock()

	if refuse != nil {
		if err := refuse(e); err != nil {
			return err
		}
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	p.accepted = append(p.accepted, e)
	return nil
}

func (p *capturingPublisher) envelopes() []Envelope {
	p.mu.Lock()
	defer p.mu.Unlock()
	out := make([]Envelope, len(p.received))
	copy(out, p.received)
	return out
}

func (p *capturingPublisher) delivered() []Envelope {
	p.mu.Lock()
	defer p.mu.Unlock()
	out := make([]Envelope, len(p.accepted))
	copy(out, p.accepted)
	return out
}

func readyEvent(id string) Event {
	return Event{
		ID:      id,
		Type:    "darkroom.asset.ready",
		Source:  "darkroom",
		Subject: "018f6f3a-0000-7000-8000-000000000001",
		Time:    time.Date(2026, 10, 10, 12, 0, 0, 0, time.UTC),
		Data:    []byte(`{"asset_id":"018f6f3a-0000-7000-8000-000000000001"}`),
	}
}

// ----------------------------------------------------------------------------
// the batch size and the poll interval are the two constants identity documented
// ----------------------------------------------------------------------------

// The batch is 100 and the poll is one second, and the shape of the argument is
// worth keeping beside the values: a bigger transaction holds row locks longer,
// and a backoff that grows while idle leaves an uploaded asset's event sitting
// unpublished for minutes.
func TestTheBatchAndThePollAreTheDocumentedConstants(t *testing.T) {
	if DefaultBatchSize != 100 {
		t.Errorf("DefaultBatchSize = %d, want 100: a larger batch holds its row locks longer and a smaller one costs more round trips", DefaultBatchSize)
	}
	if DefaultPollInterval != time.Second {
		t.Errorf("DefaultPollInterval = %v, want 1s: an exponential backoff would leave a newly completed upload's event unpublished for minutes", DefaultPollInterval)
	}

	// And a service built with no opinion gets the default rather than zero. A
	// batch size of zero is a claim that claims nothing, which is a loop that
	// never publishes and never says so.
	s := NewService(&fakeClaimer{}, NoopPublisher{}, nil, 0)
	if s.BatchSize() != DefaultBatchSize {
		t.Errorf("NewService(…, 0).BatchSize() = %d, want %d", s.BatchSize(), DefaultBatchSize)
	}
}

// ----------------------------------------------------------------------------
// what the loop does with a batch
// ----------------------------------------------------------------------------

// The whole shape in one test: claim, publish every envelope, mark each
// published, commit the batch. What is asserted is the COUNT on each side —
// three events claimed are three envelopes handed to the publisher and three
// marks written — because a loop that published two and marked three would pass
// a test that only looked at the last line.
func TestRunOncePublishesEveryEventItClaimsAndMarksThem(t *testing.T) {
	ids := []string{
		"018f6f3a-0000-7000-8000-00000000000a",
		"018f6f3a-0000-7000-8000-00000000000b",
		"018f6f3a-0000-7000-8000-00000000000c",
	}
	events := make([]Event, len(ids))
	for i, id := range ids {
		events[i] = readyEvent(id)
	}

	batch := &fakeBatch{events: events}
	claimer := &fakeClaimer{batches: []*fakeBatch{batch}}
	publisher := &capturingPublisher{}

	s := NewService(claimer, publisher, nil, 0)
	n, err := s.RunOnce(context.Background())
	if err != nil {
		t.Fatalf("RunOnce: %v", err)
	}
	if n != len(ids) {
		t.Errorf("RunOnce published %d events, want %d", n, len(ids))
	}
	if got := publisher.envelopes(); len(got) != len(ids) {
		t.Errorf("the publisher received %d envelopes, want %d", len(got), len(ids))
	}

	published, failed, released, committed := batch.counts()
	if published != len(ids) {
		t.Errorf("%d events were marked published, want %d", published, len(ids))
	}
	if failed != 0 {
		t.Errorf("%d events were marked failed, want 0 — nothing failed", failed)
	}
	if !committed {
		t.Error("the batch was never committed: the published_at writes are not durable")
	}
	if released {
		t.Error("the batch was released as well as committed: a pass that succeeded should not roll back")
	}
	if claimer.claimCount() != 1 {
		t.Errorf("the claimer was called %d times, want 1", claimer.claimCount())
	}

	// The claim asked for the configured batch, not for everything in the table.
	if claimer.lastSize != DefaultBatchSize {
		t.Errorf("the claim limit was %d, want %d", claimer.lastSize, DefaultBatchSize)
	}
}

// A publish that fails does not fail the pass. The event is left claimable — not
// marked published — and the loop moves on to the next one, because one
// permanently unserialisable event must not stop every event behind it.
func TestAFailedPublishLeavesTheEventClaimableAndDoesNotBlockTheBatch(t *testing.T) {
	good := readyEvent("018f6f3a-0000-7000-8000-00000000000a")
	bad := readyEvent("018f6f3a-0000-7000-8000-00000000000b")
	last := readyEvent("018f6f3a-0000-7000-8000-00000000000c")

	batch := &fakeBatch{events: []Event{good, bad, last}}
	publisher := &capturingPublisher{refuse: func(e Envelope) error {
		if e.ID == bad.ID {
			return errors.New("the broker refused this subject")
		}
		return nil
	}}

	s := NewService(&fakeClaimer{batches: []*fakeBatch{batch}}, publisher, nil, 0)
	n, err := s.RunOnce(context.Background())
	if err != nil {
		t.Fatalf("RunOnce: %v — a single event's publish failure is not a pass failure", err)
	}
	if n != 2 {
		t.Errorf("RunOnce published %d events, want 2 — the refused one does not count", n)
	}

	published, failed, _, committed := batch.counts()
	if published != 2 {
		t.Errorf("%d events were marked published, want 2", published)
	}
	if failed != 1 {
		t.Errorf("%d events were marked failed, want 1 — the refused event must stay claimable", failed)
	}
	if !committed {
		t.Error("the batch was never committed: the two successful events' marks would be lost")
	}

	// THE POINT OF THE WHOLE TEST, and it is the one a test that only counted
	// marks would miss: the event after the refused one was still published.
	got := publisher.envelopes()
	if len(got) != 3 {
		t.Fatalf("the publisher received %d envelopes, want 3 — the refused one must be attempted too", len(got))
	}
	if got[2].ID != last.ID {
		t.Errorf("the third envelope is %s, want %s — a refused event stalled the queue", got[2].ID, last.ID)
	}
}

// A claim failure is the loop's to log and retry, not the caller's to handle:
// RunOnce reports it, and Run carries on rather than returning, because a
// database blip is not a reason to stop publishing for the life of the process.
func TestAClaimFailureIsReportedAndTheLoopCarriesOn(t *testing.T) {
	claimer := &fakeClaimer{err: errors.New("connection refused")}
	s := NewService(claimer, NoopPublisher{}, nil, 0)

	_, err := s.RunOnce(context.Background())
	if err == nil {
		t.Fatal("RunOnce returned nil for a failed claim: the caller would believe it published something")
	}
	if !errors.Is(err, claimer.err) {
		t.Errorf("RunOnce's error does not wrap the claim failure: %v", err)
	}
}

// ----------------------------------------------------------------------------
// the envelope that reaches the publisher
// ----------------------------------------------------------------------------

// The envelope a publisher receives is built from the row and carries core's
// seven attributes and no others. core sets `additionalProperties: false`, so an
// eighth attribute is a payload every consumer on the platform rejects.
func TestTheCarriedEnvelopeIsCoreShapeAndNothingElse(t *testing.T) {
	batch := &fakeBatch{events: []Event{readyEvent("018f6f3a-0000-7000-8000-00000000000a")}}
	publisher := &capturingPublisher{}

	s := NewService(&fakeClaimer{batches: []*fakeBatch{batch}}, publisher, nil, 0)
	if _, err := s.RunOnce(context.Background()); err != nil {
		t.Fatalf("RunOnce: %v", err)
	}

	got := publisher.envelopes()
	if len(got) != 1 {
		t.Fatalf("the publisher received %d envelopes, want 1", len(got))
	}
	e := got[0]

	// Marshaled rather than reflected, because the MARSHALED form is what a
	// consumer receives: a struct tag that dropped an attribute would pass a
	// reflection walk and fail here.
	raw, err := json.Marshal(e)
	if err != nil {
		t.Fatalf("marshaling the envelope: %v", err)
	}
	var onTheWire map[string]json.RawMessage
	if err := json.Unmarshal(raw, &onTheWire); err != nil {
		t.Fatalf("the envelope is not a JSON object: %v", err)
	}

	want := map[string]bool{
		"specversion": true, "id": true, "type": true, "source": true,
		"subject": true, "time": true, "data": true,
	}
	for key := range onTheWire {
		if !want[key] {
			t.Errorf("the envelope carries %q, which core does not declare", key)
		}
	}
	for key := range want {
		if _, present := onTheWire[key]; !present {
			t.Errorf("the envelope is missing %q", key)
		}
	}

	if e.SpecVersion != "1.0" {
		t.Errorf("specversion = %q, want 1.0", e.SpecVersion)
	}
	if e.Source != Source {
		t.Errorf("source = %q, want the package's own %q", e.Source, Source)
	}
	if e.Source != "darkroom" {
		t.Errorf("source = %q, want darkroom — it must equal `name` in anywaye.yml, which core's event-naming.md requires", e.Source)
	}
	if e.Subject == "" {
		t.Error("subject is empty: core REQUIRES it, and it is the entity the event is about")
	}
	// `time` is the moment of the state change read off the row, not the moment of
	// the publish, and it is RFC3339 UTC.
	if !strings.HasSuffix(e.Time, "Z") {
		t.Errorf("time = %q, want RFC3339 in UTC", e.Time)
	}
	if e.Time != "2026-10-10T12:00:00Z" {
		t.Errorf("time = %q, want 2026-10-10T12:00:00Z — the row's value, not the clock's", e.Time)
	}
	if err := e.Validate(); err != nil {
		t.Errorf("the carried envelope does not validate: %v", err)
	}
}

// ----------------------------------------------------------------------------
// the capability guard
// ----------------------------------------------------------------------------

// An event describes a fact and never carries the capability to act on it.
//
// This is the requirement that catches a real leak rather than a hypothetical
// one: darkroom spends a whole packet refusing to hand a presigned URL to a
// caller without a verified principal, and a second one putting FORCE RLS between
// an account and another account's rows. An event that carried either onto the
// bus would hand every consumer on the platform the ability to read an object
// the HTTP surface spent those packets protecting — once, to everyone, with no
// retraction.
//
// Asserted at the PUBLISHER, which is the last darkroom-owned gate before the
// wire, so an event type added later by someone who did not read the payload
// tests is stopped here rather than there.
func TestNoPublishedEventCarriesAPresignedURLABucketKeyOrATenantID(t *testing.T) {
	cases := []struct {
		name  string
		data  string
		field string
	}{
		{"a presigned upload URL", `{"presigned":"https://bucket.s3.amazonaws.com/k?X-Amz-Signature=deadbeef"}`, "presigned"},
		{"a presigned URL under an innocent key", `{"location":"https://bucket.s3.amazonaws.com/k?X-Amz-Signature=deadbeef"}`, "location"},
		{"a bare bucket address", `{"endpoint":"https://bucket.s3.amazonaws.com"}`, "endpoint"},
		{"a storage key", `{"storage_key":"t/aaaaaaaa-…/original/abc"}`, "storage_key"},
		{"a bucket name", `{"bucket":"darkroom-media"}`, "bucket"},
		{"a tenant id", `{"account_id":"018f6f3a-0000-7000-8000-000000000001"}`, "account_id"},
		{"a tenant id under its other spelling", `{"tenant":"018f6f3a-0000-7000-8000-000000000001"}`, "tenant"},
		{"an object key", `{"object_key":"t/aaaaaaaa-…/variant/webp/abc"}`, "object_key"},
		{"a credential", `{"secret":"AKIA…"}`, "secret"},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			e := readyEvent("018f6f3a-0000-7000-8000-00000000000a")
			e.Data = []byte(tc.data)

			err := e.Envelope().RejectCapabilities()
			if err == nil {
				t.Fatalf("an envelope carrying %s was accepted for the bus: %s", tc.field, tc.data)
			}
			if !strings.Contains(err.Error(), tc.field) && !strings.Contains(err.Error(), "URL") {
				t.Errorf("the refusal does not name what it refused: %v", err)
			}

			// AND IT IS REFUSED BY THE PUBLISHER, not only by the helper. The
			// loop's path to the wire goes through Publish, so that is where the
			// refusal has to be observable.
			publisher := &capturingPublisher{refuse: Envelope.RejectCapabilities}
			batch := &fakeBatch{events: []Event{e}}
			s := NewService(&fakeClaimer{batches: []*fakeBatch{batch}}, publisher, nil, 0)

			n, runErr := s.RunOnce(context.Background())
			if runErr != nil {
				t.Fatalf("RunOnce: %v — a refused publish is a pass failure, not a crash", runErr)
			}
			if n != 0 {
				t.Errorf("%d events were published, want 0", n)
			}
			if got := publisher.delivered(); len(got) != 0 {
				t.Errorf("the publisher delivered %d envelopes, want 0 — the refusal must come from the transport", len(got))
			}
			if got := publisher.envelopes(); len(got) != 1 {
				t.Errorf("the publisher was handed %d envelopes, want 1 — the loop must have attempted it for the failure to be recorded", len(got))
			}
			_, failed, _, committed := batch.counts()
			if failed != 1 {
				t.Errorf("%d events were marked failed, want 1 — a refused event stays claimable", failed)
			}
			if !committed {
				t.Error("the batch was never committed: the failed mark is not durable")
			}
		})
	}
}

// The three payloads darkroom actually emits, asserted to pass the same guard.
//
// The guard is proved above by feeding it poison; this is the half that feeds it
// the real thing, because a guard that only ever saw a synthetic payload is a
// guard that has not been shown to admit the service's own events.
func TestDarkroomsOwnPayloadsPassTheCapabilityGuard(t *testing.T) {
	cases := []struct {
		name string
		data string
	}{
		{"asset.ready", `{"asset_id":"018f6f3a-0000-7000-8000-000000000001","owner_user_id":"018f6f3a-0000-7000-8000-000000000002","kind":"original","content_type":"image/png","byte_size":1234,"checksum":"9f86d081884c7d659a2feaa0c55ad015a3bf4f1b2b0b822cd15d6c15b0f00a08"}`},
		{"asset.deleted", `{"asset_id":"018f6f3a-0000-7000-8000-000000000001"}`},
		{"variant.created", `{"variant_id":"018f6f3a-0000-7000-8000-000000000003","asset_id":"018f6f3a-0000-7000-8000-000000000001","kind":"thumbnail","content_type":"image/webp","byte_size":99,"width":64,"height":64}`},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			e := readyEvent("018f6f3a-0000-7000-8000-000000000001")
			e.Data = []byte(tc.data)

			if err := e.Envelope().RejectCapabilities(); err != nil {
				t.Errorf("darkroom's own %s payload was refused: %v", tc.name, err)
			}
			if err := e.Envelope().Validate(); err != nil {
				t.Errorf("darkroom's own %s payload does not validate: %v", tc.name, err)
			}
		})
	}
}

// ----------------------------------------------------------------------------
// the loop's lifetime
// ----------------------------------------------------------------------------

// Run returns nil when its context is canceled, and it returns promptly. The
// loop is tied to the process's context — the same one the HTTP server drains
// on — so a shutdown that had to wait a poll interval for it would be a shutdown
// that outlives its grace.
func TestRunStopsWhenItsContextIsCanceled(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	s := NewService(&fakeClaimer{}, NoopPublisher{}, nil, 0)

	done := make(chan error, 1)
	go func() { done <- s.Run(ctx) }()

	cancel()
	select {
	case err := <-done:
		if err != nil {
			t.Errorf("Run returned %v on a canceled context, want nil", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("Run did not return within 5s of its context being canceled")
	}
}

// The loop drains and then stops on its own once the queue is dry, and the
// poll interval is what bounds the wait. Asserted through the claim count rather
// than through a sleep: three claims at a one-second interval is three seconds,
// which is the honest cost of proving the loop keeps going.
func TestRunKeepsPollingWhileTheQueueIsDry(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	claimer := &fakeClaimer{} // never hands out a batch: the queue stays dry
	s := NewService(claimer, NoopPublisher{}, nil, 0)

	done := make(chan error, 1)
	go func() { done <- s.Run(ctx) }()

	// Three poll intervals, then canceled. The point is that the loop made MORE
	// THAN ONE claim: a loop that polled once and stopped would leave an event
	// written in the gap unpublished for the life of the process.
	deadline := time.Now().Add(4 * time.Second)
	for claimer.claimCount() < 3 {
		if time.Now().After(deadline) {
			cancel()
			t.Fatalf("the loop made %d claims in 4s, want at least 3 — an idle outbox is discovered by polling", claimer.claimCount())
		}
		time.Sleep(20 * time.Millisecond)
	}
	cancel()

	select {
	case err := <-done:
		if err != nil {
			t.Errorf("Run returned %v, want nil", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("Run did not return after cancellation")
	}
}
