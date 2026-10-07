package service

// The three-way denial shape, one test per account-scoped statement in store.go.
//
// `core/docs/tenancy.md` §"The three-way denial shape" is the contract: a boundary
// has to answer three questions, not one. "Does an unauthenticated request fail" is
// satisfied by a table with no policy at all, by a table with no predicate at all, and
// by a service whose database is switched off — all three are the BUG. So each entry
// point below is proved three ways:
//
//	no-identity    a transaction with NO identity set   -> zero rows
//	other-account  a VALID identity for another account  -> zero rows, indistinguishable
//	               from nonexistence
//	own-account    this account's own identity           -> ITS OWN ROWS, with a column
//	               read off the row (checksum, status, storage key)
//
// THE THIRD ARM IS THE CONTROL, and it is the one with the information in it. A suite
// that returns nothing to everybody passes the first two arms for free; only the third
// separates an isolated service from a broken one.
//
// # WHY THESE RUN AT THE STORE LEVEL AND NOT THE SERVICE LEVEL
//
// The entry points are the store methods, and the boundary is drawn by the transaction
// they run in: every one takes a `pgx.Tx` that `darkroomdb.InAccountTx` opened with an
// identity set, and the policies in `migrations/0004_account_isolation.sql` do the
// filtering. So the three actors are three transactions:
//
//	no-identity    `InTx` — a transaction that never called `cafaye.begin_account`.
//	               `current_account_id()` reads NULL, which matches no row under
//	               FORCE RLS. This is the state a deploy step, or a code path that
//	               reached for `InTx` instead of `InAccountTx`, would produce.
//	other-account  `InAccountTx(bobAccount, …)` — a real identity, the wrong one.
//	own-account    `InAccountTx(aliceAccount, …)` — a real identity, the right one.
//
// The service-level no-identity case (an empty `Caller`) is refused earlier and more
// loudly — `InAccountTx` raises on an empty account, because a NULL identity reads
// zero rows rather than failing. That is a stronger guarantee and it is asserted in
// `TestAnAccountIdentityThatDoesNotMatchTheRowCannotBeWritten`; the arm below is the
// one the three-way shape asks for, which is the DATABASE's answer to a request with
// no identity at all.
//
// # WHAT EACH ARM'S TOKEN IS
//
// The declaration points one line per arm, and the token is what that line carries.
// The two denial arms assert absence (zero rows, or — for a write — the paired read
// that tells "matched zero rows" from "found nothing to do"); the third arm asserts a
// column read off the row that came back. A write's denial arm is never an absent
// spelling: an empty result is a lie about a row that exists, so a denied write is
// paired with a read of the row it was aimed at and THAT read is what is declared.
//
// These run in `newHarness`'s private schema and skip with a named reason when there
// is no database — see `service_test.go`'s header for why that is honest.

import (
	"context"
	"errors"
	"testing"

	"github.com/jackc/pgx/v5"

	"github.com/cafaye/darkroom/internal/checksum"
	"github.com/cafaye/darkroom/internal/domain"
	"github.com/cafaye/darkroom/internal/variants"
)

// withAccount runs fn inside a transaction scoped to the given account — the
// other-account and own-account arms' actor.
func (h *harness) withAccount(account string, fn func(tx pgx.Tx) error) error {
	h.t.Helper()
	return h.pg.Store().InAccountTx(context.Background(), account, fn)
}

// withNoIdentity runs fn inside a transaction with NO identity set — the no-identity
// arm's actor. `InTx` is the transaction that deliberately does not call
// `cafaye.begin_account`, so `current_account_id()` reads NULL and every policy
// filters every row out.
func (h *harness) withNoIdentity(fn func(tx pgx.Tx) error) error {
	h.t.Helper()
	return h.pg.Store().InTx(context.Background(), fn)
}

// pendingAsset creates a `pending` asset without completing it: the create and the
// client PUT, no complete. The markReady and markFailed entry points only act on a
// `pending` row, so a completed asset would make their denial arms meaningless.
func (h *harness) pendingAsset(caller Caller, filename, contentType string, body []byte) *CreatedUpload {
	h.t.Helper()
	ctx := context.Background()
	sum := checksum.SHA256Hex(body)
	created, pErr := h.svc.CreateUpload(ctx, caller, CreateUpload{
		Filename:    filename,
		ContentType: contentType,
		ByteSize:    int64(len(body)),
		Checksum:    sum,
	})
	if pErr != nil {
		h.t.Fatalf("CreateUpload: %v", pErr)
	}
	if err := h.objects.ApplyPresignedPut(ctx, created.Presigned.URL, body, contentType); err != nil {
		h.t.Fatalf("the client PUT: %v", err)
	}
	return created
}

// ---------------------------------------------------------------------------
// findAssetByChecksum — the duplicate-upload pre-check, a select on assets
// ---------------------------------------------------------------------------

func TestThreeWayDenialFindAssetByChecksum(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()
	h.upload(h.alice, "photo.png", "image/png", pngBytes)
	sum := checksum.SHA256Hex(pngBytes)

	// ARM 1 — no identity: zero rows.
	var noIdentity *domain.Asset
	if err := h.withNoIdentity(func(tx pgx.Tx) error {
		found, err := findAssetByChecksum(ctx, tx, sum)
		noIdentity = found
		return err
	}); err != nil {
		t.Fatalf("noIdentity: %v", err)
	}
	if noIdentity != nil {
		t.Error("noIdentity: a transaction with no identity read an asset by checksum")
	}

	// ARM 2 — another account's valid identity: zero rows.
	var otherAccount *domain.Asset
	if err := h.withAccount(bobAccount, func(tx pgx.Tx) error {
		found, err := findAssetByChecksum(ctx, tx, sum)
		otherAccount = found
		return err
	}); err != nil {
		t.Fatalf("otherAccount: %v", err)
	}
	if otherAccount != nil {
		t.Error("otherAccount: bob read alice's asset by checksum")
	}

	// ARM 3 — its own account: its own row, with the checksum read off it.
	var ownAccount *domain.Asset
	if err := h.withAccount(aliceAccount, func(tx pgx.Tx) error {
		found, err := findAssetByChecksum(ctx, tx, sum)
		ownAccount = found
		return err
	}); err != nil {
		t.Fatalf("ownAccount: %v", err)
	}
	if ownAccount.Checksum != sum {
		t.Errorf("ownAccount: the row carries %q, want %q", ownAccount.Checksum, sum)
	}
}

// ---------------------------------------------------------------------------
// findAsset — the fetch-by-id every id-scoped path goes through
// ---------------------------------------------------------------------------

func TestThreeWayDenialFindAsset(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()
	_, ready, _ := h.upload(h.alice, "photo.png", "image/png", pngBytes)
	sum := checksum.SHA256Hex(pngBytes)

	// ARM 1 — no identity: zero rows.
	var noIdentity *domain.Asset
	err := h.withNoIdentity(func(tx pgx.Tx) error {
		found, err := findAsset(ctx, tx, ready.ID)
		noIdentity = found
		return err
	})
	if !errors.Is(err, pgx.ErrNoRows) {
		t.Fatalf("noIdentity: want ErrNoRows, got %v", err)
	}
	if noIdentity != nil {
		t.Error("noIdentity: a transaction with no identity read an asset")
	}

	// ARM 2 — another account's valid identity: zero rows.
	var otherAccount *domain.Asset
	err = h.withAccount(bobAccount, func(tx pgx.Tx) error {
		found, err := findAsset(ctx, tx, ready.ID)
		otherAccount = found
		return err
	})
	if !errors.Is(err, pgx.ErrNoRows) {
		t.Fatalf("otherAccount: want ErrNoRows, got %v", err)
	}
	if otherAccount != nil {
		t.Error("otherAccount: bob read alice's asset")
	}

	// ARM 3 — its own account: its own row, with the checksum read off it.
	var ownAccount *domain.Asset
	err = h.withAccount(aliceAccount, func(tx pgx.Tx) error {
		found, err := findAsset(ctx, tx, ready.ID)
		ownAccount = found
		return err
	})
	if err != nil {
		t.Fatalf("ownAccount: %v", err)
	}
	if ownAccount.Checksum != sum {
		t.Errorf("ownAccount: the row carries %q, want %q", ownAccount.Checksum, sum)
	}
}

// ---------------------------------------------------------------------------
// markReady — the compare-and-set that completes an upload, an update on assets
// ---------------------------------------------------------------------------

func TestThreeWayDenialMarkReady(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()
	created := h.pendingAsset(h.alice, "photo.png", "image/png", pngBytes)
	sum := checksum.SHA256Hex(pngBytes)

	// ARM 1 — no identity: the update matches nothing.
	var noIdentity *domain.Asset
	if err := h.withNoIdentity(func(tx pgx.Tx) error {
		row, err := markReady(ctx, tx, created.Asset.ID, sum)
		noIdentity = row
		return err
	}); err != nil {
		t.Fatalf("noIdentity: %v", err)
	}
	if noIdentity != nil {
		t.Error("noIdentity: the update matched a row")
	}
	// The paired read: alice's asset is unchanged. A row count of zero is also what a
	// write which found nothing to do returns, so only this read tells a refusal from
	// a no-op.
	if got := h.asset(t, h.alice, created.Asset.ID); got.Status != domain.StatusPending {
		t.Errorf("assertUnchanged: alice's asset is %q after a no-identity update", got.Status)
	}

	// ARM 2 — another account's valid identity: the update matches nothing.
	var otherAccount *domain.Asset
	if err := h.withAccount(bobAccount, func(tx pgx.Tx) error {
		row, err := markReady(ctx, tx, created.Asset.ID, sum)
		otherAccount = row
		return err
	}); err != nil {
		t.Fatalf("otherAccount: %v", err)
	}
	if otherAccount != nil {
		t.Error("otherAccount: the update matched a row")
	}
	if got := h.asset(t, h.alice, created.Asset.ID); got.Status != domain.StatusPending {
		t.Errorf("assertUnchanged: alice's asset is %q after bob's update", got.Status)
	}

	// ARM 3 — its own account: the row comes back `ready`.
	var ownAccount *domain.Asset
	if err := h.withAccount(aliceAccount, func(tx pgx.Tx) error {
		row, err := markReady(ctx, tx, created.Asset.ID, sum)
		ownAccount = row
		return err
	}); err != nil {
		t.Fatalf("ownAccount: %v", err)
	}
	if ownAccount.Status != domain.StatusReady {
		t.Errorf("ownAccount: the row is %q, want ready", ownAccount.Status)
	}
}

// ---------------------------------------------------------------------------
// markFailed — the failure path's compare-and-set, an update on assets
// ---------------------------------------------------------------------------

func TestThreeWayDenialMarkFailed(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()
	created := h.pendingAsset(h.alice, "photo.png", "image/png", pngBytes)

	// ARM 1 — no identity: the update matches nothing.
	var noIdentity *domain.Asset
	if err := h.withNoIdentity(func(tx pgx.Tx) error {
		row, err := markFailed(ctx, tx, created.Asset.ID, "checksum_mismatch")
		noIdentity = row
		return err
	}); err != nil {
		t.Fatalf("noIdentity: %v", err)
	}
	if noIdentity != nil {
		t.Error("noIdentity: the update matched a row")
	}
	if got := h.asset(t, h.alice, created.Asset.ID); got.Status != domain.StatusPending {
		t.Errorf("assertUnchanged: alice's asset is %q after a no-identity update", got.Status)
	}

	// ARM 2 — another account's valid identity: the update matches nothing.
	var otherAccount *domain.Asset
	if err := h.withAccount(bobAccount, func(tx pgx.Tx) error {
		row, err := markFailed(ctx, tx, created.Asset.ID, "checksum_mismatch")
		otherAccount = row
		return err
	}); err != nil {
		t.Fatalf("otherAccount: %v", err)
	}
	if otherAccount != nil {
		t.Error("otherAccount: the update matched a row")
	}
	if got := h.asset(t, h.alice, created.Asset.ID); got.Status != domain.StatusPending {
		t.Errorf("assertUnchanged: alice's asset is %q after bob's update", got.Status)
	}

	// ARM 3 — its own account: the row comes back `failed`.
	var ownAccount *domain.Asset
	if err := h.withAccount(aliceAccount, func(tx pgx.Tx) error {
		row, err := markFailed(ctx, tx, created.Asset.ID, "checksum_mismatch")
		ownAccount = row
		return err
	}); err != nil {
		t.Fatalf("ownAccount: %v", err)
	}
	if ownAccount.Status != domain.StatusFailed {
		t.Errorf("ownAccount: the row is %q, want failed", ownAccount.Status)
	}
}

// ---------------------------------------------------------------------------
// listAssets — the account's own page, a select on assets
// ---------------------------------------------------------------------------

func TestThreeWayDenialListAssets(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()
	h.upload(h.alice, "photo.png", "image/png", pngBytes)
	sum := checksum.SHA256Hex(pngBytes)
	// Bob uploads DIFFERENT bytes: the same bytes would resolve to alice's asset if
	// he could see it, which would hide the leak behind a duplicate.
	h.upload(h.bob, "bob.png", "image/png", []byte("bob's distinct bytes"))

	// ARM 1 — no identity: zero rows.
	var noIdentity []*domain.Asset
	if err := h.withNoIdentity(func(tx pgx.Tx) error {
		rows, err := listAssets(ctx, tx, OrderDesc, 100, nil)
		noIdentity = rows
		return err
	}); err != nil {
		t.Fatalf("noIdentity: %v", err)
	}
	if !empty(noIdentity) {
		t.Errorf("noIdentity: a transaction with no identity listed %d assets", len(noIdentity))
	}

	// ARM 2 — another account's valid identity: his own row, and none of alice's.
	var otherAccount []*domain.Asset
	if err := h.withAccount(bobAccount, func(tx pgx.Tx) error {
		rows, err := listAssets(ctx, tx, OrderDesc, 100, nil)
		otherAccount = rows
		return err
	}); err != nil {
		t.Fatalf("otherAccount: %v", err)
	}
	if len(otherAccount) != 1 || otherAccount[0].AccountID != bobAccount {
		t.Errorf("otherAccount: bob's page is %v, want his own 1 asset and none of alice's", otherAccount)
	}

	// ARM 3 — its own account: her own row, with the checksum read off it.
	var ownAccount []*domain.Asset
	if err := h.withAccount(aliceAccount, func(tx pgx.Tx) error {
		rows, err := listAssets(ctx, tx, OrderDesc, 100, nil)
		ownAccount = rows
		return err
	}); err != nil {
		t.Fatalf("ownAccount: %v", err)
	}
	if len(ownAccount) != 1 || ownAccount[0].Checksum != sum {
		t.Errorf("ownAccount: alice's page is %v, want her own 1 asset", ownAccount)
	}
}

// ---------------------------------------------------------------------------
// listStorageKeys — the delete path's key read, a select on assets + asset_variants
// ---------------------------------------------------------------------------

func TestThreeWayDenialListStorageKeys(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()
	_, ready, _ := h.upload(h.alice, "photo.png", "image/png", pngBytes)

	// ARM 1 — no identity: zero rows.
	var noIdentity []string
	if err := h.withNoIdentity(func(tx pgx.Tx) error {
		keys, err := listStorageKeys(ctx, tx, ready.ID)
		noIdentity = keys
		return err
	}); err != nil {
		t.Fatalf("noIdentity: %v", err)
	}
	if !empty(noIdentity) {
		t.Errorf("noIdentity: a transaction with no identity read %d storage keys", len(noIdentity))
	}

	// ARM 2 — another account's valid identity: zero rows.
	var otherAccount []string
	if err := h.withAccount(bobAccount, func(tx pgx.Tx) error {
		keys, err := listStorageKeys(ctx, tx, ready.ID)
		otherAccount = keys
		return err
	}); err != nil {
		t.Fatalf("otherAccount: %v", err)
	}
	if !empty(otherAccount) {
		t.Errorf("otherAccount: bob read %d of alice's storage keys", len(otherAccount))
	}

	// ARM 3 — its own account: its own keys, with the storage key read off the row.
	var ownAccount []string
	if err := h.withAccount(aliceAccount, func(tx pgx.Tx) error {
		keys, err := listStorageKeys(ctx, tx, ready.ID)
		ownAccount = keys
		return err
	}); err != nil {
		t.Fatalf("ownAccount: %v", err)
	}
	if len(ownAccount) != 1 || ownAccount[0] != ready.StorageKey() {
		t.Errorf("ownAccount: alice's keys are %v, want [%s]", ownAccount, ready.StorageKey())
	}
}

// ---------------------------------------------------------------------------
// deleteAsset — the delete, which is the one that would end a customer's data
// ---------------------------------------------------------------------------

func TestThreeWayDenialDeleteAsset(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()
	_, ready, _ := h.upload(h.alice, "photo.png", "image/png", pngBytes)

	// ARM 1 — no identity: the delete matches nothing.
	var noIdentity bool
	if err := h.withNoIdentity(func(tx pgx.Tx) error {
		deleted, err := deleteAsset(ctx, tx, ready.ID)
		noIdentity = deleted
		return err
	}); err != nil {
		t.Fatalf("noIdentity: %v", err)
	}
	if noIdentity {
		t.Error("noIdentity: the delete matched a row")
	}
	// The paired read: alice's asset survived.
	if got := h.asset(t, h.alice, ready.ID); got.Status != domain.StatusReady {
		t.Errorf("assertUnchanged: alice's asset is %q after a no-identity delete", got.Status)
	}

	// ARM 2 — another account's valid identity: the delete matches nothing.
	var otherAccount bool
	if err := h.withAccount(bobAccount, func(tx pgx.Tx) error {
		deleted, err := deleteAsset(ctx, tx, ready.ID)
		otherAccount = deleted
		return err
	}); err != nil {
		t.Fatalf("otherAccount: %v", err)
	}
	if otherAccount {
		t.Error("otherAccount: the delete matched a row")
	}
	if got := h.asset(t, h.alice, ready.ID); got.Status != domain.StatusReady {
		t.Errorf("assertUnchanged: alice's asset is %q after bob's delete", got.Status)
	}

	// ARM 3 — its own account: the delete succeeds.
	var ownAccount bool
	if err := h.withAccount(aliceAccount, func(tx pgx.Tx) error {
		deleted, err := deleteAsset(ctx, tx, ready.ID)
		ownAccount = deleted
		return err
	}); err != nil {
		t.Fatalf("ownAccount: %v", err)
	}
	if !ownAccount {
		t.Error("ownAccount: alice's delete reported false")
	}
}

// ---------------------------------------------------------------------------
// listVariants — one asset's variants, a select on asset_variants
// ---------------------------------------------------------------------------

func TestThreeWayDenialListVariants(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()
	_, ready, _ := h.upload(h.alice, "photo.png", "image/png", pngBytes)
	if _, pErr := h.svc.CreateVariant(ctx, h.alice, ready.ID, variants.KindThumbnail); pErr != nil {
		t.Fatalf("CreateVariant: %v", pErr)
	}

	// ARM 1 — no identity: zero rows.
	var noIdentity []*domain.AssetVariant
	if err := h.withNoIdentity(func(tx pgx.Tx) error {
		rows, err := listVariants(ctx, tx, ready.ID)
		noIdentity = rows
		return err
	}); err != nil {
		t.Fatalf("noIdentity: %v", err)
	}
	if !empty(noIdentity) {
		t.Errorf("noIdentity: a transaction with no identity listed %d variants", len(noIdentity))
	}

	// ARM 2 — another account's valid identity: zero rows.
	var otherAccount []*domain.AssetVariant
	if err := h.withAccount(bobAccount, func(tx pgx.Tx) error {
		rows, err := listVariants(ctx, tx, ready.ID)
		otherAccount = rows
		return err
	}); err != nil {
		t.Fatalf("otherAccount: %v", err)
	}
	if !empty(otherAccount) {
		t.Errorf("otherAccount: bob listed %d of alice's variants", len(otherAccount))
	}

	// ARM 3 — its own account: its own variants, with the kind read off the row.
	var ownAccount []*domain.AssetVariant
	if err := h.withAccount(aliceAccount, func(tx pgx.Tx) error {
		rows, err := listVariants(ctx, tx, ready.ID)
		ownAccount = rows
		return err
	}); err != nil {
		t.Fatalf("ownAccount: %v", err)
	}
	if len(ownAccount) != 1 || ownAccount[0].Kind != variants.KindThumbnail {
		t.Errorf("ownAccount: alice's variants are %v, want one thumbnail", ownAccount)
	}
}

// empty is the denial arms' assertion for a list, named so that tenancy.yml can
// point at a line and name a token: Go spells "this list is empty" as
// `len(x) == 0`, and a builtin call is not a thing a reader can go and look at.
// identity's declaration does the same with its `countAuditRows` helper — the
// token names the assertion rather than the language's spelling of nothing.
func empty[T any](items []T) bool { return len(items) == 0 }
