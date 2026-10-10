package outbox

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"time"
)

// DefaultBatchSize is how many events one pass claims.
//
// Big enough that a burst of uploads completing does not fall behind, small
// enough that a single pass holds its row locks briefly. The trade is real in
// both directions and identity measured it first: a larger batch means a longer
// transaction and a slower commit under load, a smaller one means more round
// trips per event. 100 events is well inside what Postgres handles comfortably
// in one transaction.
const DefaultBatchSize = 100

// DefaultPollInterval is how long Run waits after a pass that found nothing.
//
// There is no NOTIFY in this design — `migrations/0002_outbox_events.sql` states
// the publisher's only query is `where published_at is null order by created_at`
// — so an idle outbox is discovered by polling.
//
// A fixed one-second poll rather than an exponential backoff, and deliberately:
// the common case is an empty table, and a backoff that grew while idle would
// leave a freshly completed upload's `darkroom.asset.ready` sitting unpublished
// for minutes while every thumbnail builder waited on it. The cost of the simple
// version is one cheap indexed query per second, which is nothing against the
// write traffic the service is already taking.
const DefaultPollInterval = time.Second

// Claimer hands out batches of unpublished events. *Store implements it; the
// interface exists so the loop can be tested without a database.
type Claimer interface {
	Claim(ctx context.Context, limit int) (Batch, error)
}

// Publisher puts one envelope on the bus.
//
// The interface is one method wide because that is the whole contract with a
// transport. Everything the transport owns — connection handling, subject
// naming, headers, retries, the ack — belongs behind it, which is why the NATS
// implementation is one file and nothing else: a second bus, or a change to this
// one, touches the implementation and not the loop.
type Publisher interface {
	Publish(ctx context.Context, e Envelope) error
}

// NoopPublisher accepts events and reports success.
//
// It is the loop's test double and nothing else, and it is NEVER WIRED. The
// running service gets NATSPublisher when NATS_URL is set and no loop at all
// when it is not — see cmd/darkroom for that condition. What it is NOT is the
// shipped publisher: wiring it would mark every event published and drain the
// outbox into nowhere, which is worse than the honest accumulation an
// unconfigured broker produces. The rows are the promise, and a promise this
// process keeps by discarding is not a promise.
//
// It stays because the loop's tests need a Publisher that never touches a
// network, and because deleting a tested implementation to leave the interface
// abstract would make the loop's own tests lie about what they are testing.
type NoopPublisher struct{}

// Publish discards the event. It never fails, so it is a valid target for tests
// and for a dry run — and for nothing else.
func (NoopPublisher) Publish(context.Context, Envelope) error { return nil }

// Service is the loop that drains the outbox onto the bus.
type Service struct {
	claimer   Claimer
	publisher Publisher
	logger    *slog.Logger
	batchSize int
}

// NewService returns a Service. A non-positive batchSize means DefaultBatchSize.
func NewService(claimer Claimer, publisher Publisher, logger *slog.Logger, batchSize int) *Service {
	if logger == nil {
		logger = slog.New(slog.DiscardHandler)
	}
	if batchSize <= 0 {
		batchSize = DefaultBatchSize
	}
	return &Service{
		claimer:   claimer,
		publisher: publisher,
		logger:    logger,
		batchSize: batchSize,
	}
}

// BatchSize is how many events one pass claims.
func (s *Service) BatchSize() int { return s.batchSize }

// RunOnce claims one batch and publishes it, returning how many events reached
// the bus.
//
// A publish that fails does not fail the pass. The event is marked failed —
// which leaves it claimable, with attempts incremented — and the loop moves to
// the next one. core's event-naming.md is explicit that a message which cannot
// be processed must not block the queue, and the alternative here would be
// worse than a wedge: one permanently unserializable event would stop every
// event behind it from ever being published.
//
// The batch is committed either way, because rolling back would re-claim the
// successful events and publish them again on the next pass. Delivery is
// at-least-once by design, so a duplicate is survivable and a stall is not. That
// is also why `outbox_events.id` is minted before the insert: a retry publishes
// the same id and the consumer's dedupe key actually dedupes.
func (s *Service) RunOnce(ctx context.Context) (int, error) {
	batch, err := s.claimer.Claim(ctx, s.batchSize)
	if err != nil {
		return 0, fmt.Errorf("outbox: claiming a batch: %w", err)
	}

	published, err := s.publishAll(ctx, batch)
	if err != nil {
		// The context is gone or the transaction is broken; there is nothing
		// useful to mark. Release so the rows are not held.
		if releaseErr := batch.Release(ctx); releaseErr != nil {
			s.logger.Warn("releasing the outbox batch failed", "error", releaseErr)
		}
		return published, err
	}

	if err := batch.Commit(ctx); err != nil {
		return published, fmt.Errorf("outbox: committing the batch: %w", err)
	}
	return published, nil
}

// publishAll walks the batch, marking each event published or failed. It returns
// an error only for a failure that affects the whole batch — a dead context or a
// broken transaction — never for a single event's publish failing.
func (s *Service) publishAll(ctx context.Context, batch Batch) (int, error) {
	published := 0

	for _, e := range batch.Events() {
		if err := ctx.Err(); err != nil {
			return published, err
		}

		if err := s.publisher.Publish(ctx, e.Envelope()); err != nil {
			s.logger.Warn("publishing an event failed; it stays queued for retry",
				"type", e.Type, "id", e.ID, "error", err)

			if markErr := batch.MarkFailed(ctx, e.ID); markErr != nil {
				// The mark failed, so the transaction is unusable. Report it: the
				// batch will be released and re-claimed, and a silent swallow here
				// would leave a loop that believes it published something it did not.
				return published, fmt.Errorf("outbox: recording the failure of %s: %w", e.ID, markErr)
			}
			continue
		}

		if err := batch.MarkPublished(ctx, e.ID); err != nil {
			return published, fmt.Errorf("outbox: recording %s as published: %w", e.ID, err)
		}
		published++
	}
	return published, nil
}

// Run drains the outbox until ctx is done.
//
// It returns nil on a clean shutdown. A claim failure is logged and retried
// rather than returned: the caller is a goroutine started at boot, and returning
// would stop publishing for the rest of the process's life because a database
// blipped once.
func (s *Service) Run(ctx context.Context) error {
	ticker := time.NewTicker(DefaultPollInterval)
	defer ticker.Stop()

	for {
		n, err := s.RunOnce(ctx)
		switch {
		case err == nil:
			if n > 0 {
				s.logger.Debug("published outbox events", "count", n)
				continue // a full batch may mean there is more waiting
			}
		case errors.Is(err, context.Canceled), errors.Is(err, context.DeadlineExceeded):
			s.logger.Info("outbox publisher stopping")
			return nil
		default:
			s.logger.Error("outbox publisher pass failed; retrying", "error", err)
		}

		select {
		case <-ctx.Done():
			s.logger.Info("outbox publisher stopping")
			return nil
		case <-ticker.C:
		}
	}
}
