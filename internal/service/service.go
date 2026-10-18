// Package service is darkroom's business rules: the signed-upload state machine,
// the checksum verification, the duplicate-upload resolution, and the tenant-scoped
// queries — with no HTTP in it.
//
// `AGENTS.md`, from the Rust side this replaces: "Everything that is a *decision*
// lives here rather than in a handler, so it can be tested without an HTTP round
// trip and so a handler stays a translation between the wire and a call." Packet 05
// owns the router; this package is the half that decides, and the half every
// assertion about correctness belongs to.
//
// # THE UPLOAD STATE MACHINE
//
//	POST /v1/uploads
//	     │
//	     ├── duplicate (same account, same checksum) ──▶ the existing asset, 201
//	     ▼
//	  pending ──complete: object present, measured size matches, checksum verified──▶ ready  (+ darkroom.asset.ready)
//	          │
//	          ├──complete: object absent────────────────▶ failed (409)
//	          ├──complete: measured size ≠ declared─────▶ failed (422)
//	          └──complete: checksum mismatch────────────▶ failed (422)
//
// `ready` and `failed` are TERMINAL and [domain.AssetStatus.Terminal] is the one
// place that says so. A `failed` asset is never retried into `ready`; the client
// creates a new upload. That is what keeps "this upload failed" a fact, and it keeps
// `UNIQUE (account_id, checksum)` meaningful — a checksum that failed once cannot be
// silently attempted again under the same key.
//
// # THE THREE CALLS, AND WHY THE BYTES NEVER PASS THROUGH HERE
//
//	create → the client PUTs straight to object storage → complete
//
// `objectstore.Store.Put` is used by VARIANT GENERATION and by nothing else. A 1 GiB
// upload is one request to the bucket, not a stream through an API process, and the
// in-memory store's `Stats.Puts` exists so a test can assert that: "A signed-upload
// design that quietly proxied every upload would still pass every functional test and
// fail this one."
//
// # TENANT SCOPE IS SET ON THE TRANSACTION, NOT IN THE QUERY
//
// Every method below opens its transaction with `darkroomdb.InAccountTx`, which sets
// `cafaye.begin_account` INSIDE it, and the policies in
// `migrations/0004_account_isolation.sql` do the filtering. There is no
// `and account_id = $N` anywhere in this package, and that is the point of the Go
// rewrite rather than an omission: the Rust tree needed 1,609 lines of
// source-scanning tests across two files to hold its hand-written predicates in
// place, because making the `Tenant` ARGUMENT mandatory does not make its USE
// mandatory. Here the predicate is a policy and a query cannot forget it.
//
// See `store.go` for what that costs and what the alternative was.
package service

import (
	"context"
	"crypto/rand"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"time"
	"unicode"

	"github.com/cafaye/darkroom/internal/checksum"
	"github.com/cafaye/darkroom/internal/darkroomdb"
	"github.com/cafaye/darkroom/internal/domain"
	"github.com/cafaye/darkroom/internal/objectstore"
	"github.com/cafaye/darkroom/internal/problem"
	"github.com/cafaye/darkroom/internal/storagekey"
	"github.com/cafaye/darkroom/internal/variants"
)

// PendingSweepAfter is how long a `pending` asset may live before a sweeper would
// fail it. TWICE the presign TTL, so an upload whose URL is still valid is never
// swept out from under a client that is mid-PUT. The sweeper itself is not in this
// packet — README "Not done" says so — but the number is here because it is a
// property of the state machine and a second copy of it elsewhere would drift.
var PendingSweepAfter = 2 * objectstore.PresignTTL

// Service is what a handler needs to serve a request.
//
// TWO FIELDS AND NO MORE, and the pair is fixed by the dependency rule: "Dependencies
// point downward only." This package knows `darkroomdb` (persistence) and
// `objectstore` (bytes); it knows nothing about axum, a router, a config file or a
// clock. The one thing it does reach for outside itself is `time.Now`, through
// [now], and that is a function rather than a field so a test cannot accidentally
// construct a Service whose clock is a field it forgets to pass.
type Service struct {
	db      *darkroomdb.Store
	objects objectstore.Store
}

// New builds the service over a store and an object store.
func New(db *darkroomdb.Store, objects objectstore.Store) *Service {
	return &Service{db: db, objects: objects}
}

// Caller is the verified identity behind a request: the account that OWNS the rows
// and the user who asked.
//
// A struct rather than two string parameters because the two are not
// interchangeable and a swapped argument would still compile. `AccountID` is what
// scopes every transaction; `UserID` is recorded as the asset's `owner_user_id` and
// never used to scope anything.
//
// It is NOT a `Tenant` type with one constructor and no other way in. That rule
// survives the rewrite as the transaction: there is no way to reach this package's
// database without naming an account inside `InAccountTx`, which is why a method
// whose signature forgot the caller would not compile either.
type Caller struct {
	AccountID string
	UserID    string
}

// CreateUpload is `POST /v1/uploads`.
//
// `Checksum` is a CLAIM, validated for shape here and verified against the stored
// bytes at complete time: "`AGENTS.md`: The checksum is computed, never trusted."
type CreateUpload struct {
	Filename    string
	ContentType string
	ByteSize    int64
	Checksum    string
}

// CreatedUpload is the result of a create. `Duplicate` is true when the existing
// asset was returned instead of a second one, so the handler can label the response
// without re-deriving a fact the service already knows.
type CreatedUpload struct {
	Asset     *domain.Asset
	Presigned objectstore.PresignedPut
	Duplicate bool
}

// CreateUpload creates the `pending` row and issues a presigned PUT for it.
//
// # THE DUPLICATE DECISION, AND WHY IT IS 201 AND NOT 409
//
// A duplicate returns the EXISTING asset with 201 and `Duplicate: true`. The
// reasoning, in the order that decided it:
//
//   - A 409 makes the client solve a problem it did not create. Two uploads of the
//     same screenshot are unrelated from the client's side, so a 409 means every such
//     client has to implement "on conflict, fetch the existing asset and carry on",
//     and every client that has not yet will look like it is failing.
//   - The bytes are already there and already VERIFIED. The existing asset reached
//     `ready`, which means its checksum was checked against what storage holds.
//     Handing it back gives a usable asset with no second upload of a possibly huge
//     file.
//   - Re-presigning against the EXISTING key makes the response usable anyway: the
//     caller gets a working URL, so a client that just wants to PUT and complete
//     makes the same three calls either way.
//   - It is still ONE asset, which is the point of the constraint: no second row, no
//     second object, no second bill.
//
// What it is NOT is silent: `Duplicate` becomes a response header, so a client that
// wants to tell the user "already uploaded" can.
//
// THE ONE CASE IT DOES NOT COVER: an existing asset in `failed`. A failed asset's
// checksum was never verified against real bytes, so returning it would hand back
// something unusable — and the constraint still holds the row, so this is a 409
// saying the bytes previously failed. The honest answer, because the constraint says
// they already exist and this service does not get to decide otherwise.
//
// # NO EVENT IS EMITTED HERE
//
// "No event for a `pending` asset. Nothing is in storage, so every consumer would
// fail. `darkroom.asset.ready` is at complete, and that is the moment the bytes
// exist." A row created in one transaction and a URL minted outside it is the whole
// create path.
func (s *Service) CreateUpload(ctx context.Context, caller Caller, req CreateUpload) (*CreatedUpload, *problem.Error) {
	// --- validate before anything is written or signed --------------------
	filename, pErr := validateFilename(req.Filename)
	if pErr != nil {
		return nil, pErr
	}
	kind, err := domain.ParseAssetKind(req.ContentType)
	if err != nil {
		return nil, problem.InvalidFields(
			"content_type must be a supported image, audio, video or document type",
			problem.NewField("content_type", "unsupported_media_type"),
		).Wrap(err)
	}
	contentType := domain.BaseContentType(req.ContentType)

	switch {
	case req.ByteSize < 0:
		return nil, problem.InvalidFields(
			"byte_size must not be negative",
			problem.NewField("byte_size", "out_of_range"),
		)
	case req.ByteSize > objectstore.MaxUploadBytes:
		// A 422 BEFORE a URL is issued, so a client that cannot possibly upload
		// never holds a credential.
		return nil, problem.InvalidFields(
			fmt.Sprintf("byte_size must not exceed %d", objectstore.MaxUploadBytes),
			problem.NewField("byte_size", "too_large"),
		)
	}
	if !checksum.IsValidFormat(req.Checksum) {
		// Checked HERE rather than at complete: a client that sends
		// `"checksum": "abc"` gets a 422 naming the field now, rather than a 422
		// twenty minutes later after it has uploaded a gigabyte.
		return nil, problem.InvalidFields(
			"checksum must be 64 hex characters (sha256)",
			problem.NewField("checksum", "invalid_format"),
		)
	}
	claimed := strings.ToLower(req.Checksum)

	// --- the duplicate path, checked before the write ---------------------
	// A SELECT first rather than catching the constraint violation, so the common
	// duplicate is a cheap indexed read instead of a failed transaction. The insert
	// is still the authority: two concurrent duplicates both miss here and one of
	// them gets the constraint violation and falls through to the same resolution.
	var existing *domain.Asset
	if err := s.db.InAccountTx(ctx, caller.AccountID, func(tx pgxTx) error {
		found, err := findAssetByChecksum(ctx, tx, claimed)
		if err != nil {
			return err
		}
		existing = found
		return nil
	}); err != nil {
		return nil, problem.FromDB(err)
	}
	if existing != nil {
		return s.resolveDuplicate(ctx, existing)
	}

	// --- create ----------------------------------------------------------
	now := now()
	key := storagekey.GenerateOriginal(caller.AccountID)
	asset := domain.Asset{
		ID:               NewUUID(),
		AccountID:        caller.AccountID,
		OwnerUserID:      caller.UserID,
		Kind:             kind,
		OriginalFilename: filename,
		ContentType:      contentType,
		ByteSize:         req.ByteSize,
		// The CLAIM, lowercased. It is what the unique constraint is keyed on and
		// it is what complete compares against — after computing the real value.
		Checksum:  claimed,
		Status:    domain.StatusPending,
		Metadata:  map[string]any{},
		CreatedAt: now,
		UpdatedAt: now,
	}
	asset = asset.WithStorageKey(key.String())

	var created *domain.Asset
	if err := s.db.InAccountTx(ctx, caller.AccountID, func(tx pgxTx) error {
		row, err := insertAsset(ctx, tx, asset)
		if err != nil {
			return err
		}
		created = row
		return nil
	}); err != nil {
		if problem.IsUniqueViolation(err) {
			// Lost the race. THE WINNER'S ROW IS THE ANSWER, so re-read it and take
			// the same path the pre-check would have. A conflict with no row behind
			// it is a winner that rolled back between our conflict and this read;
			// that is 409, not an invented asset.
			var winner *domain.Asset
			if err := s.db.InAccountTx(ctx, caller.AccountID, func(tx pgxTx) error {
				found, err := findAssetByChecksum(ctx, tx, claimed)
				if err != nil {
					return err
				}
				winner = found
				return nil
			}); err != nil {
				return nil, problem.FromDB(err)
			}
			if winner == nil {
				slog.Warn("checksum conflict but the row is gone", "account_id", caller.AccountID)
				return nil, problem.Conflict("a concurrent upload of the same bytes is in progress")
			}
			return s.resolveDuplicate(ctx, winner)
		}
		return nil, problem.FromDB(err)
	}

	presigned, err := s.objects.PresignPut(ctx, created.StorageKey(), contentType, req.ByteSize, objectstore.PresignTTL)
	if err != nil {
		return nil, problem.FromObjectStore(err)
	}
	return &CreatedUpload{Asset: created, Presigned: presigned}, nil
}

// resolveDuplicate turns an existing row into a usable create response, or refuses
// one that was never verified.
//
// Shared by the pre-check and the constraint-violation fallback, so the two cannot
// disagree about what a duplicate is.
func (s *Service) resolveDuplicate(ctx context.Context, existing *domain.Asset) (*CreatedUpload, *problem.Error) {
	if existing.Status == domain.StatusFailed {
		return nil, problem.Conflict(
			"these bytes were uploaded before and that upload failed; start a new upload with different content",
		)
	}
	// Re-presign against the EXISTING key, scoped to exactly that key by the same
	// rules as a fresh upload.
	presigned, err := s.objects.PresignPut(ctx, existing.StorageKey(), existing.ContentType, existing.ByteSize, objectstore.PresignTTL)
	if err != nil {
		return nil, problem.FromObjectStore(err)
	}
	slog.Info("duplicate upload resolved to the existing asset", "asset_id", existing.ID, "account_id", existing.AccountID)
	return &CreatedUpload{Asset: existing, Presigned: presigned, Duplicate: true}, nil
}

// CompleteUpload verifies the object and marks the asset `ready`.
//
// THE VERIFICATION IS THE WHOLE POINT, and it has one input: the bytes.
//
//  1. `head` the key. Absent → the upload never landed → 409, and the asset is
//     `failed`. The `head` is metadata only and is the cheap half: it answers "did
//     it land, and how big" without downloading a gigabyte to discover the gigabyte
//     is not there.
//  2. The MEASURED size against the declared one. A client that uploaded more than
//     it said is a 422 — the object is there, it is just the wrong one.
//  3. READ THE OBJECT BACK AND HASH IT. Not "ask the backend what checksum it
//     recorded, and read only if it has none" — that ordering is the defect the Rust
//     tree was changed for, and `objectstore.ObjectMeta` has no checksum field
//     because of it: on AWS S3 the recorded `FULL_OBJECT` sha256 is real and
//     comparing against it is cheaper; on Cloudflare R2 there is no `FULL_OBJECT`
//     SHA-256 at all (its compatibility table offers `COMPOSITE` for SHA-256 and
//     `FULL_OBJECT` for CRC-64/NVME only), so the same code asks a question whose
//     answer depends on the backend, and the honest answer — nothing, or a value that
//     is not the object's sha256 — turns a check into a coin flip. "A correctness
//     property may not depend on which backend answered it."
//  4. `ready` with the checksum COMPUTED FROM THE STORED BYTES, and
//     `darkroom.asset.ready` enqueued in the SAME transaction. If that commit fails,
//     the asset stays `pending` and no event exists — which is the outbox rule.
func (s *Service) CompleteUpload(ctx context.Context, caller Caller, assetID, claimedChecksum string) (*domain.Asset, *problem.Error) {
	if !checksum.IsValidFormat(claimedChecksum) {
		return nil, problem.InvalidFields(
			"checksum must be 64 hex characters (sha256)",
			problem.NewField("checksum", "invalid_format"),
		)
	}

	// --- read the asset, scoped by the transaction's identity -------------
	// Absence covers "no such asset" and "not yours" identically, and the caller
	// turns both into the same 404. Reading it before touching storage also stops a
	// caller using this endpoint to probe whether an object key exists in a bucket
	// they have no claim to.
	asset, pErr := s.find(ctx, caller.AccountID, assetID)
	if pErr != nil {
		return nil, pErr
	}

	switch asset.Status {
	case domain.StatusReady:
		// Terminal, and answering with the asset rather than a 409: reaching here
		// with `ready` means the key was not used (the idempotency layer did its
		// job), and a repeated complete that returns the asset is a success.
		return asset, nil
	case domain.StatusFailed:
		return nil, problem.Conflict("this upload has already failed and cannot be completed")
	}

	// --- 1. does the object exist? ---------------------------------------
	meta, err := s.objects.Head(ctx, asset.StorageKey())
	if err != nil {
		if isNotFound(err) {
			// The URL was issued and never used, or the bytes were removed. Either
			// way the upload did not happen, and the row becomes a permanent record
			// of that.
			s.fail(ctx, caller.AccountID, assetID, "object_absent_at_complete")
			return nil, problem.Conflict(
				"no object was found in storage for this upload; it may have expired before the bytes were sent",
			)
		}
		return nil, problem.FromObjectStore(err)
	}

	// --- 2. the measured size against the declared size -------------------
	if meta.ByteSize != asset.ByteSize {
		s.fail(ctx, caller.AccountID, assetID, "size_mismatch")
		return nil, problem.InvalidFields(
			fmt.Sprintf("the object in storage is %d bytes but %d were declared", meta.ByteSize, asset.ByteSize),
			problem.NewField("byte_size", "mismatch"),
		)
	}

	// --- 3. the checksum, computed from what storage actually holds -------
	// ONE read, every backend, no exceptions and no fallback. The client's claim is
	// never what is compared against — that is the security property of the
	// signed-upload pattern — and neither is anything the backend says about its own
	// bytes.
	bytes, err := s.objects.Get(ctx, asset.StorageKey())
	if err != nil {
		return nil, problem.FromObjectStore(err)
	}
	actual := checksum.SHA256Hex(bytes)
	// The buffer is dead the moment it is hashed. A 1 GiB upload would otherwise
	// sit in this process's heap until the response is written.
	bytes = nil

	if !checksum.Match(claimedChecksum, actual) {
		// The bytes in storage are not the bytes the client said it would upload:
		// a corrupted upload, a wrong client, or a client registering a checksum
		// for content it never sent. The last one is why this is a hard failure.
		s.fail(ctx, caller.AccountID, assetID, "checksum_mismatch")
		return nil, problem.InvalidFields(
			"the checksum does not match the bytes in storage",
			problem.NewField("checksum", "mismatch"),
		)
	}

	// --- 4. mark ready and emit, in the same transaction -----------------
	var ready *domain.Asset
	commitErr := s.db.InAccountTx(ctx, caller.AccountID, func(tx pgxTx) error {
		row, err := markReady(ctx, tx, assetID, actual)
		if err != nil {
			return err
		}
		if row == nil {
			// Another complete won the compare-and-set. NOT an error: the state
			// this request was trying to produce exists. The caller is re-read
			// after this transaction, outside it.
			return nil
		}
		ready = row
		return enqueue(ctx, tx, NewEvent(EventAssetReady, row.ID, now(), map[string]any{
			"asset_id":      row.ID,
			"owner_user_id": row.OwnerUserID,
			"kind":          row.Kind.String(),
			"content_type":  row.ContentType,
			"byte_size":     row.ByteSize,
			"checksum":      row.Checksum,
		}))
	})
	if commitErr != nil {
		return nil, problem.FromDB(commitErr)
	}
	if ready == nil {
		// The lost race: report whatever the winner produced, which is the same
		// terminal state.
		return s.find(ctx, caller.AccountID, assetID)
	}
	slog.Info("asset ready", "asset_id", ready.ID, "account_id", ready.AccountID)
	return ready, nil
}

// fail moves an asset to `failed` and emits NOTHING.
//
// "No event for a failure. A consumer cannot act on 'this upload did not complete'
// in a way that is better served by the client receiving its 4xx." Its own
// transaction, because the caller is about to return an error and an error that left
// the transaction open would mean the failure is recorded only if the caller commits.
//
// The failure to RECORD the failure is logged and swallowed: the client is getting a
// 4xx either way, and replacing it with a 500 because a bookkeeping write failed
// would tell the caller less about the thing they did wrong.
// FailStalePending marks pending assets older than cutoff as failed for one account.
// A cross-account sweep calls this per account (see darkroomdb.FailStalePending).
func (s *Service) FailStalePending(ctx context.Context, account string, cutoff time.Time) (int64, error) {
	return s.db.FailStalePending(ctx, account, cutoff)
}

// DeleteExpiredIdempotencyKeys removes ledger rows older than cutoff.
func (s *Service) DeleteExpiredIdempotencyKeys(ctx context.Context, cutoff time.Time) (int64, error) {
	return s.db.DeleteExpiredIdempotencyKeys(ctx, cutoff)
}

func (s *Service) fail(ctx context.Context, accountID, assetID, reason string) {
	if err := s.db.InAccountTx(ctx, accountID, func(tx pgxTx) error {
		_, err := markFailed(ctx, tx, assetID, reason)
		return err
	}); err != nil {
		slog.Error("could not record the upload failure", "asset_id", assetID, "reason", reason, "error", err)
	}
}

// ListQuery is one page request.
//
// `Limit` is clamped by [clampPageLimit] rather than rejected, so a caller may pass
// 0 for "the default" and 1000 for "as much as you will give me" and get a sensible
// page either way. `Order` is the direction, and it is a [SortOrder] rather than a
// string because this package refuses every value the document does not enumerate
// (see [ParseSortOrder]) — a caller of the service and a caller of the wire cannot
// disagree about what `order` means, which is the whole reason the decision lives
// here.
type ListQuery struct {
	Limit  int
	Cursor *Cursor
	// Order is the direction to walk. The zero value is [OrderDesc], which is what
	// omitting `order` on the wire means, so `ListQuery{}` is newest-first.
	Order SortOrder
}

// AssetPage is one page plus the cursor for the next one.
//
// `Next` is nil when this is the last page, which is what `limit + 1` bought: one
// extra row read and no second `count(*)`.
type AssetPage struct {
	Assets    []*domain.Asset
	Next      *Cursor
	HasMore   bool
	NextToken string
}

// ListAssets returns one page in the requested direction, scoped to the caller's
// account.
//
// # THE FIRST CHECK IS THE DIRECTION THE CURSOR WAS MINTED UNDER
//
// Before the query, and it is a refusal. The cursor's row-value comparison is only
// correct for the direction it was minted in — see [Cursor.AgreesWith] for the walk
// that goes wrong otherwise — so a request that names one direction and carries a
// cursor from the other has no page this function could honestly return.
//
// It is here as well as in the handler because this is the method that would perform
// the walk: a caller that reaches the rules directly, with no HTTP layer above it,
// gets the same refusal the wire does. That is the weaker of the two guarantees this
// repository has and it is a weaker one on purpose — `AGENTS.md` records the same
// trade for tenant scoping, where the Go rule is "a single entry point" rather than a
// property of every signature.
func (s *Service) ListAssets(ctx context.Context, caller Caller, q ListQuery) (*AssetPage, *problem.Error) {
	if pErr := q.Cursor.AgreesWith(q.Order); pErr != nil {
		return nil, pErr
	}
	limit := clampPageLimit(q.Limit)

	// ONE EXTRA ROW to find out whether there is a next page, without a second
	// count query.
	rows, err := s.readAll(ctx, caller.AccountID, func(tx pgxTx) ([]*domain.Asset, error) {
		return listAssets(ctx, tx, q.Order, limit+1, q.Cursor)
	})
	if err != nil {
		return nil, problem.FromDB(err)
	}

	page := &AssetPage{HasMore: len(rows) > limit}
	if page.HasMore {
		rows = rows[:limit]
		// The next cursor is the LAST row of THIS page, which is where the next
		// query's comparison starts — not the last row read, which is one further on
		// and would skip an asset. It carries `q.Order`, because that is the direction
		// this page was walked in and the only one its position means anything in.
		last := rows[len(rows)-1]
		page.Next = &Cursor{CreatedAt: last.CreatedAt, ID: last.ID, Order: q.Order}
		page.NextToken = EncodeCursor(*page.Next)
	}
	page.Assets = rows
	return page, nil
}

// GetAsset returns one asset.
//
// THE SINGLE 404 PATH. A cross-tenant read and a genuinely missing asset are
// byte-identical responses, so existence never leaks: "The rule is absence, not
// refusal" (`AGENTS.md`).
func (s *Service) GetAsset(ctx context.Context, caller Caller, assetID string) (*domain.Asset, *problem.Error) {
	return s.find(ctx, caller.AccountID, assetID)
}

// DeleteAsset removes the row, the storage objects, and emits
// `darkroom.asset.deleted`.
//
// ORDER MATTERS, and the reason is which failure is recoverable: the storage objects
// are deleted BEFORE the row is committed, because a row that says "gone" while a
// gigabyte of objects survives is a leak nobody will ever find, whereas objects
// removed just before a rolled-back row are a missing object for an asset that still
// exists — recoverable, and far cheaper. The window is one transaction, not a retry
// loop.
//
// Every VARIANT's object goes too: deleting an asset whose thumbnail remains is not a
// delete. A storage failure is logged and the row delete still proceeds, because "a
// storage outage must not make an asset undeletable" and the operator's sweeper for
// orphaned objects is the safety net.
func (s *Service) DeleteAsset(ctx context.Context, caller Caller, assetID string) *problem.Error {
	keys, err := s.readKeys(ctx, caller.AccountID, assetID)
	if err != nil {
		return err
	}
	if len(keys) == 0 {
		// Nothing to delete and no asset: the same 404, from the same fact. A key
		// list that is empty is either a missing asset or another account's, and
		// there is no reason here to tell them apart.
		return problem.NotFound("asset not found")
	}

	for _, key := range keys {
		if err := s.objects.Delete(ctx, key); err != nil {
			slog.Error("could not delete a storage object", "key", key, "asset_id", assetID, "error", err)
		}
	}

	commitErr := s.db.InAccountTx(ctx, caller.AccountID, func(tx pgxTx) error {
		deleted, err := deleteAsset(ctx, tx, assetID)
		if err != nil {
			return err
		}
		if !deleted {
			// Gone between the key read and here. The caller asked for it not to
			// exist and it does not, so this is a 404 rather than a second delete.
			return errAlreadyGone
		}
		return enqueue(ctx, tx, NewEvent(EventAssetDeleted, assetID, now(), map[string]any{
			// NO ACCOUNT ID, and that is the packet BRK-5's rule rather than a
			// tidying. The bus is a broadcast surface: every consumer on the platform
			// receives every darkroom event, so an event carries the FACT — this asset
			// is gone — and never the identifier of whose it was. A consumer that
			// needs the account asks darkroom over a request carrying a principal,
			// where a policy scopes the answer. `internal/outbox` refuses to publish
			// an envelope carrying `account_id` at all, so this is the only spelling
			// this packet can have.
			"asset_id": assetID,
		}))
	})
	if commitErr != nil {
		if isAlreadyGone(commitErr) {
			return problem.NotFound("asset not found")
		}
		return problem.FromDB(commitErr)
	}
	slog.Info("asset deleted", "asset_id", assetID, "account_id", caller.AccountID)
	return nil
}

// CreateVariant derives an image from a `ready` asset and stores it.
//
// Rejects anything that is not a `ready` image with a 422 that says WHICH of the two
// it is. A 404 here would mean the asset is not the caller's; a 422 means it is
// theirs and cannot be derived from — and the distinction matters because the 422
// names the field the caller has to fix.
//
// Re-requesting a kind REPLACES the variant (`upsertVariant`), so it is idempotent
// rather than accumulating rows nobody will ever read.
func (s *Service) CreateVariant(ctx context.Context, caller Caller, assetID string, kind variants.VariantKind) (*domain.AssetVariant, *problem.Error) {
	if !kind.Valid() {
		return nil, problem.InvalidFields(
			fmt.Sprintf("unknown variant kind: %s", kind),
			problem.NewField("kind", "unknown_value"),
		)
	}
	asset, pErr := s.find(ctx, caller.AccountID, assetID)
	if pErr != nil {
		return nil, pErr
	}
	switch {
	case asset.Status != domain.StatusReady:
		return nil, problem.InvalidFields(
			"only a ready asset can have a variant generated from it",
			problem.NewField("status", "not_ready"),
		)
	case asset.Kind != domain.KindImage:
		return nil, problem.InvalidFields(
			fmt.Sprintf("variants are only supported for images, and this asset is a %s", asset.Kind),
			problem.NewField("kind", "unsupported_kind"),
		)
	}

	// Read the original. The original row is NEVER mutated by variant generation.
	original, err := s.objects.Get(ctx, asset.StorageKey())
	if err != nil {
		return nil, problem.FromObjectStore(err)
	}
	encoded, err := variants.Encode(original, kind, asset.ContentType)
	if err != nil {
		return nil, fromVariants(err)
	}

	// A DISTINCT key namespace from the original, so a delete of one can never
	// address the other.
	key := storagekey.GenerateVariant(caller.AccountID, kind.String())
	if err := s.objects.Put(ctx, key.String(), encoded.Bytes, kind.ContentType()); err != nil {
		return nil, problem.FromObjectStore(err)
	}

	at := now()
	width, height := int32(encoded.Width), int32(encoded.Height)
	sourceWidth, sourceHeight := int32(encoded.SourceWidth), int32(encoded.SourceHeight)
	variant := domain.AssetVariant{
		ID:          NewUUID(),
		AssetID:     asset.ID,
		AccountID:   caller.AccountID,
		Kind:        kind,
		ContentType: kind.ContentType(),
		ByteSize:    int64(len(encoded.Bytes)),
		Width:       &width,
		Height:      &height,
		Metadata: map[string]any{
			"source_asset_id": asset.ID,
			"source_checksum": asset.Checksum,
			// Recorded so a consumer can tell "the thumbnail is 256 wide" from "the
			// source was 100 wide and we did not upscale".
			"source_width":  sourceWidth,
			"source_height": sourceHeight,
		},
		CreatedAt: at,
		UpdatedAt: at,
	}
	variant = variant.WithStorageKey(key.String())

	var stored *domain.AssetVariant
	commitErr := s.db.InAccountTx(ctx, caller.AccountID, func(tx pgxTx) error {
		row, err := upsertVariant(ctx, tx, variant)
		if err != nil {
			return err
		}
		stored = row
		return enqueue(ctx, tx, NewEvent(EventVariantCreated, row.ID, at, map[string]any{
			"variant_id":   row.ID,
			"asset_id":     asset.ID,
			"kind":         row.Kind.String(),
			"content_type": row.ContentType,
			"byte_size":    row.ByteSize,
			"width":        row.Width,
			"height":       row.Height,
		}))
	})
	if commitErr != nil {
		return nil, problem.FromDB(commitErr)
	}
	return stored, nil
}

// ListVariants returns one asset's variants, tenant-scoped.
//
// THE PARENT IS CHECKED FIRST, and its absence is the same 404 as a missing asset on
// `GetAsset`. Without that check an empty list would answer "no" — and a caller
// cannot tell that from "this asset has no variants yet", which is an existence
// oracle with one bit. "A rule is absence, not refusal" cuts both ways: it also means
// the parent must be refused rather than reported as an empty set.
func (s *Service) ListVariants(ctx context.Context, caller Caller, assetID string) ([]*domain.AssetVariant, *problem.Error) {
	if _, pErr := s.find(ctx, caller.AccountID, assetID); pErr != nil {
		return nil, pErr
	}
	var out []*domain.AssetVariant
	err := s.db.InAccountTx(ctx, caller.AccountID, func(tx pgxTx) error {
		found, err := listVariants(ctx, tx, assetID)
		out = found
		return err
	})
	if err != nil {
		return nil, problem.FromDB(err)
	}
	return out, nil
}

// ----------------------------------------------------------------------------
// the two reads every id-scoped path goes through
// ----------------------------------------------------------------------------

// find reads one asset and maps absence onto THE 404.
//
// Every id-scoped method routes through this, so "one place that turns absence into a
// 404" is structural rather than a convention — `AGENTS.md` records that the rule
// "covers **all five** id-scoped routes, not just `GET /v1/assets/{id}` — a rule
// checked on the oldest route is a rule checked on one route."
func (s *Service) find(ctx context.Context, accountID, assetID string) (*domain.Asset, *problem.Error) {
	var asset *domain.Asset
	err := s.db.InAccountTx(ctx, accountID, func(tx pgxTx) error {
		row, err := findAsset(ctx, tx, assetID)
		if err != nil {
			return err
		}
		asset = row
		return nil
	})
	if err != nil {
		if isNoRows(err) {
			return nil, problem.NotFound("asset not found")
		}
		return nil, problem.FromDB(err)
	}
	return asset, nil
}

// readAll runs a read inside an account transaction. The two helpers below exist
// because a Go closure cannot both return a value and be the transaction body, and
// `InAccountTx` takes one function — so each shape gets its own one-liner instead of
// an out-parameter that a reader has to follow.
func (s *Service) readAll(ctx context.Context, accountID string, fn func(pgxTx) ([]*domain.Asset, error)) ([]*domain.Asset, error) {
	var out []*domain.Asset
	err := s.db.InAccountTx(ctx, accountID, func(tx pgxTx) error {
		rows, err := fn(tx)
		out = rows
		return err
	})
	return out, err
}

func (s *Service) readKeys(ctx context.Context, accountID, assetID string) ([]string, *problem.Error) {
	var keys []string
	err := s.db.InAccountTx(ctx, accountID, func(tx pgxTx) error {
		rows, err := listStorageKeys(ctx, tx, assetID)
		keys = rows
		return err
	})
	if err != nil {
		return nil, problem.FromDB(err)
	}
	return keys, nil
}

// ----------------------------------------------------------------------------
// validation and small helpers
// ----------------------------------------------------------------------------

// validateFilename checks what the name will be used FOR.
//
// The name is stored, returned to clients, and ends up in a `Content-Disposition` on
// the download path eventually, so the rules are about not carrying control
// characters and not being absurdly long. A path is not a filename: STRIPPING would
// change the name and ACCEPTING would put a separator into a header value.
func validateFilename(filename string) (string, *problem.Error) {
	trimmed := strings.TrimSpace(filename)
	switch {
	case trimmed == "":
		// A stray space from a copy-paste is not a security event, so a name that is
		// only whitespace is empty rather than "invalid".
		return "", problem.InvalidFields("filename must not be empty", problem.NewField("filename", "required"))
	case len([]rune(trimmed)) > maxFilenameRunes:
		return "", problem.InvalidFields(
			fmt.Sprintf("filename must be at most %d characters", maxFilenameRunes),
			problem.NewField("filename", "too_long"),
		)
	case strings.ContainsAny(trimmed, "/\\"):
		return "", problem.InvalidFields(
			"filename must not contain a path separator",
			problem.NewField("filename", "invalid_format"),
		)
	}
	for _, r := range trimmed {
		if unicode.IsControl(r) {
			return "", problem.InvalidFields(
				"filename must not contain control characters",
				problem.NewField("filename", "invalid_format"),
			)
		}
	}
	return trimmed, nil
}

// maxFilenameRunes is counted in RUNES and not bytes, because the limit is about what
// a human sees in a list and a 512-byte cap rejects 128 four-byte filenames for a
// reason that has nothing to do with headers or filesystems.
const maxFilenameRunes = 512

// fromVariants maps the variant pipeline's deliberate failures onto the envelope.
//
// `variants.Error` is NOT the platform's error type and does not claim to be — packet
// 02 says so in its own header — so this is the one place the two meet. Its `Status`
// is carried across rather than re-decided, because packet 02 already drew the line
// between "the caller sent something undecodable" and "this repository is broken".
func fromVariants(err error) *problem.Error {
	var ve *variants.Error
	if errors.As(err, &ve) {
		fields := make([]problem.FieldError, 0, len(ve.Fields))
		for _, f := range ve.Fields {
			fields = append(fields, problem.NewField(f.Field, f.Code))
		}
		switch ve.Status {
		case variants.StatusUnprocessable:
			return problem.InvalidFields(ve.Detail, fields...)
		default:
			return problem.Internal(ve.Detail).Wrap(err)
		}
	}
	return problem.Internal("the request could not be completed").Wrap(err)
}

// now is the clock, in one place.
//
// A function rather than a field on [Service] so that no caller can inject a time and
// no test has to: the fields are deliberately exactly the store and the object store,
// and adding a third would make this a container rather than a set of rules.
func now() time.Time { return time.Now().UTC() }

// NewUUID mints a version 4 UUID.
//
// `AGENTS.md`, from `migrations/0004_account_isolation.sql`: "darkroom mints every id
// in Go — `assets.id`, `outbox_events.id` and the variant ids are UUIDs the service
// generates […] There is no `serial`, no `identity` and therefore no sequence for the
// login role to need USAGE or UPDATE on." So this is not a convenience: it is why
// that migration needs no sequence grant, and it is why a packet that adds a serial
// column has to add the grant.
//
// crypto/rand, and it panics rather than falling back: an id that repeats is a row
// that overwrites a customer's asset, and there is no weaker source to reach for.
func NewUUID() string {
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		panic("service: no entropy from crypto/rand: " + err.Error())
	}
	b[6] = (b[6] & 0x0f) | 0x40 // version 4
	b[8] = (b[8] & 0x3f) | 0x80 // RFC 4122 variant
	return fmt.Sprintf("%x-%x-%x-%x-%x", b[0:4], b[4:6], b[6:8], b[8:10], b[10:16])
}
