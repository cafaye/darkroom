package outbox

// The transport, against a real broker.
//
// The loop is proved without a database in publisher_test.go, using a fake
// claimer and a capturing publisher. That proves the loop hands the transport an
// envelope and marks what the transport accepted — it cannot prove the
// transport puts anything ONTO A BUS, because a capturing publisher IS the bus
// from the loop's point of view and a fake that never refused would pass a
// transport that never connected.
//
// So this file stands up an in-process NATS server with JetStream enabled and
// publishes through it for real. That is the one place this repository opens a
// socket to something that is not the database named by the environment, and it
// is not an exception to "no network in tests": the server is the test's own
// process, on 127.0.0.1, on a random free port, and nothing here reaches
// anywhere.

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/nats-io/nats-server/v2/server"
	"github.com/nats-io/nats.go"
)

// startNATSTestServer starts an in-process JetStream broker and returns its
// client URL.
//
// JetStream is enabled, which is the whole of the broker's configuration and the
// reason the transport is built the way it is: a JetStream publish is a request
// that blocks until the broker ACKS, so a nil return from [NATSPublisher.Publish]
// is the ack the loop marks a row from. A core-NATS server would answer
// immediately and the test would prove nothing about the mark.
func startNATSTestServer(t *testing.T) string {
	t.Helper()

	s, err := server.NewServer(&server.Options{
		Host:      "127.0.0.1",
		Port:      -1, // a random free port: the tests run in parallel and share nothing
		JetStream: true,
		StoreDir:  t.TempDir(),
	})
	if err != nil {
		t.Fatalf("starting the in-process NATS server: %v", err)
	}
	go s.Start()
	if !s.ReadyForConnections(10 * time.Second) {
		s.Shutdown()
		t.Fatal("the in-process NATS server did not become ready")
	}
	t.Cleanup(s.Shutdown)

	return s.ClientURL()
}

// addTestStream creates a JetStream stream covering subjects, the way the fleet's
// stack must. A publish to a subject no stream matches is REFUSED by the broker,
// so without this the transport works and nothing is ever stored — which is the
// refusal the companion test proves and not a delivery.
func addTestStream(t *testing.T, url string, subjects ...string) {
	t.Helper()

	nc, err := nats.Connect(url)
	if err != nil {
		t.Fatalf("connecting to the in-process NATS server: %v", err)
	}
	t.Cleanup(nc.Close)

	js, err := nc.JetStream()
	if err != nil {
		t.Fatalf("opening the JetStream context: %v", err)
	}
	if _, err := js.AddStream(&nats.StreamConfig{Name: "TEST", Subjects: subjects}); err != nil {
		t.Fatalf("adding the test stream: %v", err)
	}
}

// subscribeTestStream reads everything the stream is holding, with a deadline
// long enough that the broker's own acknowledgement has certainly landed.
func subscribeTestStream(t *testing.T, url string) []*nats.Msg {
	t.Helper()

	nc, err := nats.Connect(url)
	if err != nil {
		t.Fatalf("connecting to read the stream back: %v", err)
	}
	t.Cleanup(nc.Close)

	js, err := nc.JetStream()
	if err != nil {
		t.Fatalf("opening the JetStream context to read: %v", err)
	}

	// A durable pull consumer rather than a subscription: JetStream retention is
	// what makes the message still there to be read after the publisher has
	// disconnected, which is the property under test.
	sub, err := js.SubscribeSync("darkroom.>")
	if err != nil {
		t.Fatalf("subscribing to the test stream: %v", err)
	}
	defer sub.Unsubscribe()

	var out []*nats.Msg
	for {
		msg, err := sub.NextMsg(500 * time.Millisecond)
		if err != nil {
			break // the stream drained: NextMsg times out once there is nothing left
		}
		out = append(out, msg)
	}
	return out
}

func darkroomEvent(id string) Event {
	return Event{
		ID:      id,
		Type:    "darkroom.asset.ready",
		Source:  "darkroom",
		Subject: "018f6f3a-0000-7000-8000-000000000001",
		Time:    time.Date(2026, 10, 10, 12, 0, 0, 0, time.UTC),
		Data:    []byte(`{"asset_id":"018f6f3a-0000-7000-8000-000000000001"}`),
	}
}

// THE REQUIREMENT: an unpublished darkroom row reaches the stream under a
// `darkroom.` subject, and the proof is a message READ BACK OUT OF THE BROKER.
//
// A test that counted the publisher's return value would prove the transport
// believed it delivered. This one reads the stream, so the message being there
// is the broker's statement and not this package's.
func TestAnUnpublishedEventReachesTheStreamUnderADarkroomSubject(t *testing.T) {
	url := startNATSTestServer(t)
	addTestStream(t, url, "darkroom.>")

	publisher, err := NewNATSPublisher(url)
	if err != nil {
		t.Fatalf("NewNATSPublisher: %v", err)
	}
	defer publisher.Close()

	e := darkroomEvent("018f6f3a-0000-7000-8000-000000000001")
	envelope := e.Envelope()

	if err := publisher.Publish(context.Background(), envelope); err != nil {
		t.Fatalf("Publish: %v", err)
	}

	stored := subscribeTestStream(t, url)
	if len(stored) != 1 {
		t.Fatalf("the stream holds %d messages, want 1 — the event did not reach the bus", len(stored))
	}

	// THE SUBJECT IS THE EVENT TYPE. core's event-outbox.md: "No mapping table, no
	// prefix rewriting." So the subject on the wire is the same three-segment
	// type the row carried, which is what makes `darkroom.>` the filter the stack
	// must declare.
	if got := stored[0].Subject; got != string(e.Type) {
		t.Errorf("the message is on subject %q, want %q — the subject is the event type", got, e.Type)
	}
	if stored[0].Subject != "darkroom.asset.ready" {
		t.Errorf("the message is on subject %q, want darkroom.asset.ready", stored[0].Subject)
	}

	// And the payload is the envelope, unmodified, with the row's own identity
	// inside it: the id the consumers deduplicate on is the row's primary key.
	var onTheWire map[string]json.RawMessage
	if err := json.Unmarshal(stored[0].Data, &onTheWire); err != nil {
		t.Fatalf("the message is not a JSON envelope: %v", err)
	}
	if !hasKey(onTheWire, "specversion") || !hasKey(onTheWire, "id") || !hasKey(onTheWire, "data") {
		t.Errorf("the envelope on the bus is missing a required attribute: %v", onTheWire)
	}
	var id string
	if err := json.Unmarshal(onTheWire["id"], &id); err != nil {
		t.Fatalf("the envelope's id is not a string: %v", err)
	}
	if id != e.ID {
		t.Errorf("the envelope's id is %q, want %q — a retry must publish the SAME id for the consumer's dedupe key to dedupe", id, e.ID)
	}
}

// The refusal that makes the positive test meaningful: a subject no stream
// matches is REFUSED by the broker, Publish returns an error, and the message is
// not there. That is the direction a misconfigured stack must fail in — the row
// stays claimable with attempts climbing, so an operator watching the table sees
// it, rather than the loop marking events published that never left.
func TestAPublishToASubjectNoStreamMatchesIsRefused(t *testing.T) {
	url := startNATSTestServer(t) // deliberately no addTestStream: no stream exists

	publisher, err := NewNATSPublisher(url)
	if err != nil {
		t.Fatalf("NewNATSPublisher: %v", err)
	}
	defer publisher.Close()

	err = publisher.Publish(context.Background(), darkroomEvent("018f6f3a-0000-7000-8000-000000000001").Envelope())
	if err == nil {
		t.Fatal("Publish returned nil for a subject no stream matches: the loop would mark the row published and the event would be lost")
	}

	// The refusal names the subject, because "no stream matches" and "the broker
	// is down" are two different operator actions and an error that hid which
	// one it was would send somebody to the wrong one.
	if !strings.Contains(err.Error(), "darkroom.asset.ready") {
		t.Errorf("the refusal does not name the subject: %v", err)
	}
}

// The refusal is bounded. A publish is a REQUEST, so a broker that never answers
// is a publish that waits for the context — and the loop's context is the
// process's, which has no deadline. The budget is what makes the difference
// between a slow broker and a wedged one.
func TestAnUnansweredPublishIsBoundedByThePublishTimeout(t *testing.T) {
	if DefaultPublishTimeout <= 0 {
		t.Fatal("DefaultPublishTimeout is not positive: an unbounded publish can hang the loop for the life of the process")
	}
	if worst := time.Duration(DefaultBatchSize) * DefaultPublishTimeout; worst > time.Hour {
		t.Errorf("a full batch against a silent broker takes %v, which is not a bound anybody can reason about", worst)
	}
}

// A cancelled context aborts the publish, because the budget is DERIVED from the
// caller's. A shutdown must not wait out a publish to a broker that is gone.
func TestACancelledContextAbortsThePublish(t *testing.T) {
	url := startNATSTestServer(t)
	addTestStream(t, url, "darkroom.>")

	publisher, err := NewNATSPublisher(url)
	if err != nil {
		t.Fatalf("NewNATSPublisher: %v", err)
	}
	defer publisher.Close()

	ctx, cancel := context.WithCancel(context.Background())
	cancel() // cancelled before the call: shutdown racing the drain

	if err := publisher.Publish(ctx, darkroomEvent("018f6f3a-0000-7000-8000-000000000001").Envelope()); err == nil {
		t.Error("Publish returned nil on a cancelled context: a shutdown would wait out the publish")
	}
}

// A transport that would put a capability on the bus refuses it BEFORE it
// marshals. This is the proof that the guard is in the transport and not only in
// a helper the loop never calls.
func TestTheTransportRefusesAnEnvelopeCarryingACapability(t *testing.T) {
	url := startNATSTestServer(t)
	addTestStream(t, url, "darkroom.>")

	publisher, err := NewNATSPublisher(url)
	if err != nil {
		t.Fatalf("NewNATSPublisher: %v", err)
	}
	defer publisher.Close()

	e := darkroomEvent("018f6f3a-0000-7000-8000-000000000001")
	e.Data = []byte(`{"asset_id":"018f6f3a-0000-7000-8000-000000000001","presigned":"https://bucket.s3.amazonaws.com/k?X-Amz-Signature=deadbeef"}`)

	if err := publisher.Publish(context.Background(), e.Envelope()); err == nil {
		t.Fatal("the transport accepted an envelope carrying a presigned URL: the capability is on the bus")
	}
	if stored := subscribeTestStream(t, url); len(stored) != 0 {
		t.Errorf("the stream holds %d messages, want 0 — a refused envelope was delivered", len(stored))
	}
}

// A URL that does not PARSE is a boot failure, not a first-event failure.
//
// A broker that is not LISTENING is deliberately NOT, and this is the half that
// matters: `RetryOnFailedConnect` is set, so an unreachable broker leaves
// [NewNATSPublisher] holding a connection in the reconnecting state and returns
// no error at all. That is the design — the outbox table is the durable store,
// rows accumulate while the broker is away and drain when it returns, and
// refusing to boot without a bus would couple the media surface to the bus's
// availability.
//
// What is refused is a typo: `not a url` or `::::` is a deployment that believes
// it has a bus and does not, and that belongs at boot where an operator reads it
// rather than at the first event where nobody does.
func TestAMalformedBrokerURLIsRefusedAtConstruction(t *testing.T) {
	for _, url := range []string{"not a url", "::::", "nats://[::1"} {
		if _, err := NewNATSPublisher(url); err == nil {
			t.Errorf("NewNATSPublisher(%q) returned nil: a deployment with a mistyped NATS_URL would believe it has a bus", url)
		}
	}
}

// AND AN UNREACHABLE ONE IS NOT, which is the same fact from the other side and
// the reason an operator who points NATS_URL at a broker that has not started
// yet sees a running process rather than a crash loop.
func TestAnUnreachableBrokerIsNotAConstructionFailure(t *testing.T) {
	// Port 1 is closed on every machine (it is the "nothing is listening here"
	// port — see AGENTS.md's no-sleeps rule) and it is bound to loopback, so no
	// sleep is needed to make the point: the call returns immediately with a
	// connection in the reconnecting state.
	publisher, err := NewNATSPublisher("nats://127.0.0.1:1")
	if err != nil {
		t.Fatalf("NewNATSPublisher refused an unreachable broker: %v — the outbox table is the durable store, and rows drain when the broker arrives", err)
	}
	defer publisher.Close()
}

func hasKey(m map[string]json.RawMessage, key string) bool {
	_, ok := m[key]
	return ok
}

func contains(s, sub string) bool {
	return len(s) >= len(sub) && (s == sub || indexOf(s, sub) >= 0)
}

func indexOf(s, sub string) int {
	for i := 0; i+len(sub) <= len(s); i++ {
		if s[i:i+len(sub)] == sub {
			return i
		}
	}
	return -1
}
