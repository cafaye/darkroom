package outbox

import (
	"context"
	"errors"
	"fmt"
	"sync"

	"github.com/jackc/pgx/v5"

	"github.com/cafaye/darkroom/internal/darkroomdb"
)

// ErrNotInBatch means a caller tried to mark an event the batch it holds does
// not contain. It is a bug in the loop, not a runtime condition, and silently
// accepting it would let the loop believe it had published something it never
// touched — and, worse, let one replica's batch mark a row another replica had
// already claimed.
var ErrNotInBatch = errors.New("event is not in this batch")

// claimQuery is the whole of the publisher's read path.
//
// Four properties, every one of which `internal/service/outbox.go` named in
// advance and none of which is obvious:
//
//   - `published_at is null` is the ONLY definition of unpublished. Not
//     `attempts = 0`, and not anything an in-memory queue says.
//   - `order by created_at`, and the reason is per-subject ordering: `time` is
//     the moment of the STATE CHANGE, so two events for one asset can sit in
//     `created_at` order differently from the order the state changed in. A
//     consumer that builds a thumbnail from `darkroom.asset.ready` and one that
//     deletes from `darkroom.asset.deleted` would see them reordered.
//   - `for update skip locked`, which is the multi-replica problem. A plain
//     `select … limit n` from N replicas hands the same n rows to every one of
//     them, and every one of them publishes. `skip locked` makes the second
//     replica walk past rows the first has locked. A bare `for update` is worse
//     than no lock here: the replicas SERIALIZE on the head-of-queue rows
//     instead of skipping them, so correctness improves and throughput
//     collapses.
//   - It names ONE table. See [Store.Claim] for why that is the tenant axis and
//     not just tidiness.
const claimQuery = `
	SELECT id, event_type, source, subject, time, data
	FROM outbox_events
	WHERE published_at IS NULL
	ORDER BY created_at, id
	LIMIT $1
	FOR UPDATE SKIP LOCKED`

// Store is the outbox_events table, read the one way the publisher reads it.
type Store struct {
	db *darkroomdb.Store
}

// NewStore returns a Store backed by db.
//
// db is the same *darkroomdb.Store the request paths use, which is a pool
// capped at 4 by `darkroomdb.Open` from the shared cluster's per-role limit. The
// publisher spends ONE of those four on the claim transaction, which is the
// whole of its budget and is worth stating: it is not a pool of its own and it
// cannot outgrow the role's connection limit.
func NewStore(db *darkroomdb.Store) *Store { return &Store{db: db} }

// Claim selects up to limit unpublished events and holds them for the duration
// of a transaction.
//
// # THE TRANSACTION SETS NO ACCOUNT, AND THAT IS THE TENANT AXIS
//
// `AGENTS.md` says "`auth.Tenant` is the only way to scope a query, and
// `InAccountTx` is the only way to start a transaction" — and this is neither,
// on purpose, and the reason is the tenant axis rather than a shortcut.
//
// `outbox_events` carries NO `account_id` column and NO policy. It is the one
// table `migrations/0004_account_isolation.sql` grants DML on outside
// `cafaye.protect_table`, and `tenancy.yml` records it alongside
// `idempotency_keys` as deliberately unscoped: "outbox_events is drained by
// darkroom's own code, never by a query that reaches a customer's rows."
//
// So there is no tenant to scope the claim BY, and the risk runs the other way:
// wrapping this in `InAccountTx` would put an identity on a transaction that
// reads a table no policy guards, which is a claim that the rows are this
// account's when nothing in the database agrees. The claim is opened through
// `darkroomdb.BeginTx`, which is the identity-free transaction `darkroomdb.InTx`
// already documents `outbox_events` as being for.
//
// What holds the axis is that [claimQuery] names ONE table and it is that one.
// A claim that reached into `assets` — to carry a checksum or a storage key
// onto the bus, say — would need an identity and a policy to be safe, and this
// transaction has neither. `store_test.go` reads [claimQuery] as text and holds
// that, because a join is exactly the kind of edit that reads as an improvement.
//
// # WHY THE TRANSACTION STAYS OPEN
//
// The claim and the `published_at` write belong in ONE transaction, or a
// publisher that crashes between them republishes forever. Holding it means a
// crashed publisher's rows are released by Postgres rather than stranded, and it
// means a published event is marked in the same transaction that claimed it. The
// trade is a row lock held for the length of one batch's publishing, which is
// what `skip locked` and `DefaultBatchSize` bound.
func (s *Store) Claim(ctx context.Context, limit int) (Batch, error) {
	if limit <= 0 {
		return nil, fmt.Errorf("outbox: claim limit must be positive, got %d", limit)
	}

	tx, err := s.db.BeginTx(ctx)
	if err != nil {
		return nil, fmt.Errorf("outbox: beginning the claim transaction: %w", err)
	}

	rows, err := tx.Query(ctx, claimQuery, limit)
	if err != nil {
		_ = tx.Rollback(ctx)
		return nil, fmt.Errorf("outbox: claiming a batch: %w", err)
	}
	defer rows.Close()

	events, err := scanEvents(rows)
	if err != nil {
		_ = tx.Rollback(context.WithoutCancel(ctx))
		return nil, err
	}

	return &pgBatch{tx: tx, events: events}, nil
}

// scanEvents reads the claimed rows.
//
// `data` is stored as `jsonb`, so what comes back is the exact bytes that were
// inserted. It is passed through rather than re-encoded: an encode-decode cycle
// would reorder object keys and emit a payload that differs byte-for-byte from
// the row, and the row is the thing consumers' contract tests were written
// against.
func scanEvents(rows pgx.Rows) ([]Event, error) {
	events := make([]Event, 0, 16)

	for rows.Next() {
		var e Event
		if err := rows.Scan(&e.ID, &e.Type, &e.Source, &e.Subject, &e.Time, &e.Data); err != nil {
			return nil, fmt.Errorf("outbox: scanning a claimed event: %w", err)
		}
		events = append(events, e)
	}

	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("outbox: reading the claimed batch: %w", err)
	}

	return events, nil
}

// Batch is a claimed set of events held under a row lock until it is committed
// or released.
type Batch interface {
	// Events returns the claimed envelopes.
	Events() []Event
	// MarkPublished records that an event reached the bus.
	MarkPublished(ctx context.Context, eventID string) error
	// MarkFailed records a failed attempt, leaving the event claimable.
	MarkFailed(ctx context.Context, eventID string) error
	// Commit releases the row locks and makes the marks durable.
	Commit(ctx context.Context) error
	// Release rolls back, leaving every claimed event claimable again. It is safe
	// to call after Commit and safe to call twice.
	Release(ctx context.Context) error
}

// pgBatch is a Batch backed by an open pgx transaction.
type pgBatch struct {
	tx      pgx.Tx
	events  []Event
	claimed map[string]struct{}

	mu      sync.Mutex
	settled bool
}

func (b *pgBatch) Events() []Event { return b.events }

func (b *pgBatch) MarkPublished(ctx context.Context, eventID string) error {
	return b.mark(ctx, eventID, `
		UPDATE outbox_events
		SET published_at = now(), attempts = attempts + 1
		WHERE id = $1::uuid`)
}

// MarkFailed records the attempt and leaves the row claimable.
//
// There is NO `last_error` column in this table — `migrations/0002_outbox_events.sql`
// never added one and identity's did — so the reason lives in the log rather than
// in the row. What the row carries is `attempts`, which is the operator's signal
// that the loop is trying: a row whose attempts climb while `published_at` stays
// NULL is a broker that is refusing, and that is visible from a
// `select count(*) from outbox_events where published_at is null`.
func (b *pgBatch) MarkFailed(ctx context.Context, eventID string) error {
	return b.mark(ctx, eventID, `
		UPDATE outbox_events
		SET attempts = attempts + 1
		WHERE id = $1::uuid`)
}

func (b *pgBatch) mark(ctx context.Context, eventID string, query string) error {
	if !b.holds(eventID) {
		return fmt.Errorf("outbox: %w: %s", ErrNotInBatch, eventID)
	}

	tag, err := b.tx.Exec(ctx, query, eventID)
	if err != nil {
		return fmt.Errorf("outbox: marking %s: %w", eventID, err)
	}
	if tag.RowsAffected() == 0 {
		return fmt.Errorf("outbox: %w: %s (no row was updated)", ErrNotInBatch, eventID)
	}
	return nil
}

// holds reports whether the batch claimed eventID. A caller that marks something
// it never claimed is usually a loop that invented an event, and the UPDATE would
// otherwise hit a row belonging to another instance's batch.
func (b *pgBatch) holds(eventID string) bool {
	if b.claimed == nil {
		b.claimed = make(map[string]struct{}, len(b.events))
		for _, e := range b.events {
			b.claimed[e.ID] = struct{}{}
		}
	}
	_, ok := b.claimed[eventID]
	return ok
}

func (b *pgBatch) Commit(ctx context.Context) error {
	b.mu.Lock()
	defer b.mu.Unlock()

	if b.settled {
		return nil
	}
	if err := b.tx.Commit(ctx); err != nil {
		return fmt.Errorf("outbox: committing the batch: %w", err)
	}
	b.settled = true
	return nil
}

// Release rolls the claim back so the events are available again.
//
// It runs against a fresh context when the caller's is already canceled — which
// is exactly when a publisher is shutting down — because a rollback issued on a
// dead context would fail and leave the connection until the pool reaped it. If
// even that fails the transaction is discarded, which is still the outcome
// Release is asking for.
func (b *pgBatch) Release(ctx context.Context) error {
	b.mu.Lock()
	defer b.mu.Unlock()

	if b.settled {
		return nil
	}
	b.settled = true

	if err := b.tx.Rollback(ctx); err == nil || errors.Is(err, pgx.ErrTxClosed) {
		return nil
	}

	return b.tx.Rollback(context.WithoutCancel(ctx))
}
