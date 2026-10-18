package outbox

import (
	"context"
	"encoding/json"
	"fmt"
	"net/url"
	"time"

	"github.com/nats-io/nats.go"
)

// DefaultNATSConnectTimeout bounds the initial connection to the broker.
//
// It is a connect timeout only. A broker that is down at boot is not a boot
// failure — see [NewNATSPublisher] for why the outbox table, not the process's
// liveness, is what couples darkroom to the bus — so this bounds the wait for a
// broker that is slow to answer rather than one that is absent.
const DefaultNATSConnectTimeout = 5 * time.Second

// DefaultPublishTimeout bounds one publish's wait for the broker's ack.
//
// JetStream's PublishMsg is a request that blocks until the broker acks or the
// context dies, and the loop's context — the process's — has no deadline, so
// without a budget a single unanswered publish hangs the drain loop and holds
// its transaction open for the life of the process. The budget is applied inside
// Publish, so every caller is protected and not just the loop.
//
// Ten seconds is long enough that a slow-but-healthy broker never trips it (a
// healthy one acks in milliseconds) and short enough that a silent one costs the
// loop one bounded wait per event rather than the process's remaining lifetime.
// It is a package constant so a deployment can reason about the worst case: a
// full batch against a fully silent broker drains in at most
// DefaultBatchSize * DefaultPublishTimeout, loudly, with every row left
// claimable.
const DefaultPublishTimeout = 10 * time.Second

// NATSPublisher publishes envelopes to a NATS JetStream subject.
//
// # WHY JETSTREAM AND NOT CORE NATS
//
// The outbox contract sets `published_at` from the broker's ack: a row marked
// published that the broker never saw is an event announced and lost, which is
// the failure the table exists to prevent. Core NATS' Publish returns when the
// message is on the wire, not when the broker has stored it, so marking on its
// return would mark rows published that were never delivered. JetStream's
// PublishMsg is a request that blocks until the broker acks, so a nil return
// from Publish IS the ack the loop marks the row from — "ack, then mark" is the
// call, not a convention layered around it.
//
// # THE PUBLISH IS BOUNDED BY A BUDGET, AND AN UNANSWERED PUBLISH IS A FAILURE
//
// "Until the broker acks" is not the same as "forever". PublishMsg is a select
// on the reply inbox and the context's Done channel, so a broker that neither
// acks nor errors — a half-configured stream, a network black hole, a JetStream
// layer that is up but has no matching stream and no core subscriber — blocks
// the call for as long as the context allows. The loop runs on the process's
// context, which has no deadline, so an unbounded publish is a publish that can
// hang the drain loop (and hold its transaction open) for the life of the
// process. Publish therefore applies DefaultPublishTimeout to every call: a
// publish that is not acknowledged within the budget returns an error, which is
// what the loop records and what leaves the row claimable for the next pass.
// The budget is the difference between a slow broker and a wedged one.
//
// # THE SUBJECT IS THE EVENT TYPE
//
// core's event-outbox.md: "No mapping table, no prefix rewriting." A row with
// type `darkroom.asset.ready` is published on the subject `darkroom.asset.ready`
// and nothing else. The subject is read off the envelope, so a new event type
// needs no change in this file — and the stream filter the stack must declare is
// therefore `darkroom.>`.
//
// # WHAT THE SUBJECT DOES NOT CARRY
//
// Not an asset id, not an account, not a tenant, and above all not a capability.
// The subject is the TYPE, which is a routing key, so subscribing to
// `darkroom.>` is subscribing to the whole of this service's facts and to no
// service's secrets. The entity the event is about is `envelope.subject` — a
// bare opaque UUID, per core — and `envelope.data` carries the identifiers a
// consumer needs and nothing that reads an object. See
// TestNoPublishedEventCarriesAPresignedURLABucketKeyOrATenantID, which is the
// proof, and `internal/service` for the payloads it guards.
//
// # THE STREAM IS THE STACK'S, NOT THIS PACKAGE'S
//
// A JetStream publish to a subject no stream matches fails, and that failure is
// correct: it leaves the row claimable with attempts incremented, and the event
// leaves the table only once a stream exists that covers its subject. Creating
// one here would mean this service choosing a stream name, a retention policy
// and a replication factor for the fleet's bus — deployment decisions that
// belong to the NATS stack. kit's compose runs the broker with `-js` and defines
// no streams, so the fleet-level fact this packet imposes is: **the stack must
// define a stream whose subject filter covers what darkroom publishes
// (`darkroom.>`), or every publish fails and no event ever leaves the table.**
// The failure is loud — attempts climbs — which is the direction a
// misconfiguration should fail in.
type NATSPublisher struct {
	nc *nats.Conn
	js nats.JetStreamContext
}

// NewNATSPublisher connects to the broker at url and returns a publisher over
// its JetStream context.
//
// RetryOnFailedConnect is the whole of the boot-time resilience, and it is a
// deliberate choice rather than an omission: a broker that is down when the
// process starts is NOT a startup failure. The outbox table is the durable
// store — rows accumulate while the broker is away and drain when it returns,
// which is the entire point of publishing from a table rather than from the
// request path. The connection reconnects on its own; publishes made while it is
// down fail, the loop marks those rows failed, and the next pass retries them.
// Refusing to boot without a bus would couple the media surface to the bus's
// availability, turning an event delay into an upload outage.
//
// What this DOES refuse is a url that does not parse: a typo is a deployment
// that believes it has a bus and does not, and that belongs at boot where an
// operator reads it, not at the first event where nobody does.
func NewNATSPublisher(rawURL string) (*NATSPublisher, error) {
	// Refuse a url that does not parse BEFORE handing it to the client. The
	// client's own parser is lenient enough that some malformed inputs were
	// accepted (and then retried forever under RetryOnFailedConnect), which
	// made this check environment-dependent. Parsing here is deterministic and
	// does not depend on DNS or on the client's retry behavior.
	parsed, err := url.Parse(rawURL)
	if err != nil || parsed == nil || parsed.Scheme == "" || parsed.Host == "" {
		return nil, fmt.Errorf("outbox: %q is not a broker URL: %w", rawURL, err)
	}
	url := rawURL
	nc, err := nats.Connect(url,
		nats.RetryOnFailedConnect(true),
		nats.Timeout(DefaultNATSConnectTimeout),
	)
	if err != nil {
		return nil, fmt.Errorf("outbox: connecting to the NATS broker: %w", err)
	}

	js, err := nc.JetStream()
	if err != nil {
		nc.Close()
		return nil, fmt.Errorf("outbox: opening the JetStream context: %w", err)
	}

	return &NATSPublisher{nc: nc, js: js}, nil
}

// Publish puts one envelope on the subject named by its type.
//
// It returns nil only after the broker has acked, and an error otherwise. The
// wait is bounded by DefaultPublishTimeout: a publish that is not acknowledged
// within the budget is a failure, not a wait — the error is what the loop
// records and what leaves the row claimable for the next pass, and the row is
// never marked published on this path. A canceled context (a shutdown) aborts
// an in-flight publish the same way, because the budget is derived from the
// caller's context.
//
// THE PAYLOAD IS VALIDATED BEFORE IT IS MARSHALED, and the check is not
// ceremony. This is the last point at which anything darkroom owns sees the
// bytes: after this call the envelope is on a bus every service on the platform
// subscribes to, and a presigned URL, a bucket key or a tenant id that reached
// the wire here would be a capability handed to every consumer at once. See
// [forbiddenDataKeys] for what is refused and why.
func (p *NATSPublisher) Publish(ctx context.Context, e Envelope) error {
	if err := e.Validate(); err != nil {
		return err
	}

	payload, err := json.Marshal(e)
	if err != nil {
		return fmt.Errorf("outbox: marshaling the envelope: %w", err)
	}

	// The budget is applied here, inside Publish, so every caller is protected
	// and not just the loop: the process's context has no deadline, and an
	// unbounded publish is a publish that can hang forever. Deriving from the
	// caller's context keeps a shutdown able to abort the publish immediately.
	publishCtx, cancel := context.WithTimeout(ctx, DefaultPublishTimeout)
	defer cancel()

	if _, err := p.js.PublishMsg(&nats.Msg{Subject: e.Type, Data: payload}, nats.Context(publishCtx)); err != nil {
		return fmt.Errorf("outbox: publishing %s: %w", e.Type, err)
	}
	return nil
}

// Close closes the broker connection.
//
// It is safe to call after the loop has stopped, and safe to call concurrently
// with a publish in flight — the publish fails, which is the outcome a shutdown
// wants. It is not an error to close twice.
func (p *NATSPublisher) Close() {
	p.nc.Close()
}
