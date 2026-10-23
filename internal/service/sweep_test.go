package service

import (
	"context"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/anywaye/darkroom/internal/darkroomdb"
	"github.com/anywaye/darkroom/internal/darkroomdb/pgtest"
)

const sweepAlice = "11111111-1111-4111-8111-111111111111"

func TestFailStalePendingMarksOldPendingFailed(t *testing.T) {
	pg := pgtest.New(t)
	store := pg.Store()
	ctx := context.Background()
	old := time.Now().Add(-10 * time.Minute)
	fresh := time.Now()

	for i, ca := range []time.Time{old, fresh} {
		cs := "000000000000000000000000000000000000000000000000000000000000000" + string(rune('1'+i))
		err := store.InAccountTx(ctx, sweepAlice, func(tx pgx.Tx) error {
			_, err := tx.Exec(ctx, `insert into assets (id, account_id, owner_user_id, kind, original_filename, content_type, byte_size, checksum, storage_key, status, created_at, updated_at)
				values ($1, $2::uuid, $3::uuid, 'image', 'f.png', 'image/png', 1, $6, $4, 'pending', $5, $5)`,
				uuid.New(), sweepAlice, "aaaaaaaa-aaaa-4aaa-8aaa-aaaaaaaaaaaa", "a/"+sweepAlice+"/"+uuid.New().String()+"/original", ca, cs)
			return err
		})
		if err != nil {
			t.Fatalf("insert: %v", err)
		}
	}

	n, err := store.FailStalePending(ctx, sweepAlice, time.Now().Add(-5*time.Minute))
	if err != nil {
		t.Fatalf("sweep: %v", err)
	}
	if n != 1 {
		t.Fatalf("swept %d, want 1", n)
	}
}

func TestFailStalePendingLeavesReadyAlone(t *testing.T) {
	pg := pgtest.New(t)
	store := pg.Store()
	ctx := context.Background()
	old := time.Now().Add(-10 * time.Minute)
	err := store.InAccountTx(ctx, sweepAlice, func(tx pgx.Tx) error {
		_, err := tx.Exec(ctx, `insert into assets (id, account_id, owner_user_id, kind, original_filename, content_type, byte_size, checksum, storage_key, status, created_at, updated_at)
			values ($1, $2::uuid, $3::uuid, 'image', 'f.png', 'image/png', 1, '1111111111111111111111111111111111111111111111111111111111111111', $4, 'ready', $5, $5)`,
			uuid.New(), sweepAlice, "aaaaaaaa-aaaa-4aaa-8aaa-aaaaaaaaaaaa", "a/"+sweepAlice+"/y/original", old)
		return err
	})
	if err != nil {
		t.Fatalf("insert: %v", err)
	}
	n, err := store.FailStalePending(ctx, sweepAlice, time.Now().Add(-5*time.Minute))
	if err != nil {
		t.Fatalf("sweep: %v", err)
	}
	if n != 0 {
		t.Fatalf("swept %d, want 0", n)
	}
}

func TestDeleteExpiredIdempotencyKeysRemovesOldRows(t *testing.T) {
	pg := pgtest.New(t)
	store := pg.Store()
	ctx := context.Background()
	old := time.Now().Add(-25 * time.Hour)
	fresh := time.Now()
	for _, ca := range []time.Time{old, fresh} {
		err := store.InTx(ctx, func(tx pgx.Tx) error {
			_, err := tx.Exec(ctx, `insert into idempotency_keys (key, endpoint, principal_key, request_hash, status_code, created_at, updated_at)
				values ($1, 'POST /v1/uploads', 'u:a', 'h', 0, $2, $2)`,
				"key-"+ca.Format(time.RFC3339Nano), ca)
			return err
		})
		if err != nil {
			t.Fatalf("insert: %v", err)
		}
	}
	n, err := store.DeleteExpiredIdempotencyKeys(ctx, time.Now().Add(-24*time.Hour))
	if err != nil {
		t.Fatalf("sweep: %v", err)
	}
	if n != 1 {
		t.Fatalf("deleted %d, want 1", n)
	}
}

var _ = darkroomdb.Store{}
