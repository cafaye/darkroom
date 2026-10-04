package service

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/cafaye/darkroom/internal/checksum"
	"github.com/cafaye/darkroom/internal/darkroomdb"
	"github.com/cafaye/darkroom/internal/darkroomdb/pgtest"
	"github.com/cafaye/darkroom/internal/domain"
	"github.com/cafaye/darkroom/internal/objectstore"
	"github.com/cafaye/darkroom/internal/problem"
	"github.com/cafaye/darkroom/internal/variants"
)

// The tests below run against a real PostgreSQL, one private schema per test.
//
// THEY SKIP RATHER THAN FAIL when there is no database, and `pgtest` names the
// reason. A suite that cannot run on a machine with no Postgres is a suite nobody
// runs, and `AGENTS.md` is explicit that a skip is honest: "A skip is honest; a test
// that silently passes without proving anything is not." The isolation is per test
// rather than per run — see `pgtest`'s header for the argument that decided it — so
// these run in parallel and cannot see each other's rows.

const (
	aliceAccount = "11111111-1111-4111-8111-111111111111"
	aliceUser    = "aaaaaaaa-aaaa-4aaa-8aaa-aaaaaaaaaaaa"
	bobAccount   = "22222222-2222-4222-8222-222222222222"
	bobUser      = "bbbbbbbb-bbbb-4bbb-8bbb-bbbbbbbbbbbb"
)

// harness is a service over a fresh schema, an in-memory object store, and two
// accounts that have never heard of each other.
type harness struct {
	t       *testing.T
	pg      *pgtest.Test
	svc     *Service
	objects *objectstore.Memory
	alice   Caller
	bob     Caller
}

// assert wraps a problem error so a test can chain what it expects onto the call
// that produced it, without repeating the `t` it would otherwise have to carry into
// every helper.
func (h *harness) assert(err *problem.Error) *problemAssert { return &problemAssert{t: h.t, err: err} }

func newHarness(t *testing.T) *harness {
	t.Helper()
	pg := pgtest.New(t)
	db := pg.Store()
	objects := objectstore.NewMemory()
	return &harness{
		t:       t,
		pg:      pg,
		svc:     New(db, objects),
		objects: objects,
		alice:   Caller{AccountID: aliceAccount, UserID: aliceUser},
		bob:     Caller{AccountID: bobAccount, UserID: bobUser},
	}
}

// pngBytes is a real 8x8 PNG, because a variant test that decoded a made-up file
// would be testing the decoder's error path.
var pngBytes = func() []byte {
	return []byte{
		0x89, 0x50, 0x4e, 0x47, 0x0d, 0x0a, 0x1a, 0x0a,
		0x00, 0x00, 0x00, 0x0d, 0x49, 0x48, 0x44, 0x52,
		0x00, 0x00, 0x00, 0x01, 0x00, 0x00, 0x00, 0x01,
		0x08, 0x06, 0x00, 0x00, 0x00, 0x1f, 0x15, 0xc4,
		0x89, 0x00, 0x00, 0x00, 0x0a, 0x49, 0x44, 0x41,
		0x54, 0x78, 0x9c, 0x63, 0x00, 0x01, 0x00, 0x00,
		0x05, 0x00, 0x01, 0x0d, 0x0a, 0x2d, 0xb4, 0x00,
		0x00, 0x00, 0x00, 0x49, 0x45, 0x4e, 0x44, 0xae,
		0x42, 0x60, 0x82,
	}
}()

// upload runs create, PUTs the bytes the way a client does, and completes. The PUT is
// through the presigned URL's key on the object store directly, because that is the
// path a client takes: darkroom never sees the bytes.
func (h *harness) upload(caller Caller, filename, contentType string, body []byte) (*CreatedUpload, *domain.Asset, *problemAssert) {
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
	// THROUGH THE PRESIGNED URL, which is the only path a client has and the only
	// path that is not `Store.Put`. Going through `Put` would make the "the service
	// never writes the client's bytes" assertion count the test's own write.
	if err := h.objects.ApplyPresignedPut(ctx, created.Presigned.URL, body, contentType); err != nil {
		h.t.Fatalf("the client PUT: %v", err)
	}
	ready, pErr := h.svc.CompleteUpload(ctx, caller, created.Asset.ID, sum)
	return created, ready, &problemAssert{t: h.t, err: pErr}
}

// problemAssert keeps the assertion next to the call, because a test that separates
// "what happened" from "what should have happened" by a screenful is a test that
// reports the wrong thing.
//
// THE FIELD IS `*problem.Error` AND NOT `error`, which is the typed-nil trap this
// file's first draft walked into: the service returns a concrete pointer, a nil
// pointer stored in an `error` field is a NON-NIL interface, and `if err != nil` on it
// reports "expected success, got <nil>" — the most confusing possible failure for a
// test whose assertion is "it worked". The concrete type is what makes nil mean nil.
type problemAssert struct {
	t   *testing.T
	err *problem.Error
}

func (p *problemAssert) status(want int, msg string) *problemAssert {
	p.t.Helper()
	if p.err == nil {
		p.t.Fatalf("%s: expected %d, got success", msg, want)
	}
	if p.err.Status() != want {
		p.t.Fatalf("%s: got %d (%v), want %d", msg, p.err.Status(), p.err, want)
	}
	return p
}

func (p *problemAssert) code(want, msg string) *problemAssert {
	p.t.Helper()
	if p.err == nil {
		p.t.Fatalf("%s: expected code %q, got success", want, msg)
	}
	if string(p.err.Code()) != want {
		p.t.Fatalf("%s: got code %q (%v), want %q", msg, p.err.Code(), p.err, want)
	}
	return p
}

func (p *problemAssert) field(want, msg string) *problemAssert {
	p.t.Helper()
	if p.err == nil {
		p.t.Fatalf("%s: expected field %q, got success", want, msg)
	}
	for _, f := range p.err.Fields() {
		if f.Field == want {
			return p
		}
	}
	p.t.Fatalf("%s: no field %q in %v", msg, want, p.err.Fields())
	return p
}

func (p *problemAssert) ok() {
	p.t.Helper()
	if p.err != nil {
		p.t.Fatalf("expected success, got %v", p.err)
	}
}

// ----------------------------------------------------------------------------
// the three-call flow
// ----------------------------------------------------------------------------

func TestTheThreeCallsMakeAnAssetReadyAndEmitExactlyOneEvent(t *testing.T) {
	h := newHarness(t)
	_, ready, res := h.upload(h.alice, "photo.png", "image/png", pngBytes)
	res.ok()

	if ready.Status != domain.StatusReady {
		t.Fatalf("status = %q, want ready", ready.Status)
	}
	if ready.Checksum != checksum.SHA256Hex(pngBytes) {
		t.Errorf("the row carries %q, want the COMPUTED checksum", ready.Checksum)
	}
	if ready.AccountID != aliceAccount || ready.OwnerUserID != aliceUser {
		t.Errorf("the row carries account %q owner %q", ready.AccountID, ready.OwnerUserID)
	}
	// `kind` is DERIVED, never supplied: a client sends `content_type` and the
	// server decides which decoders run.
	if ready.Kind != domain.KindImage {
		t.Errorf("kind = %q, want image", ready.Kind)
	}

	// Exactly one event, and it is the READY one — not a create event, because
	// "No event for a `pending` asset. Nothing is in storage, so every consumer
	// would fail."
	events := h.outboxEvents(t)
	if len(events) != 1 {
		t.Fatalf("outbox has %d events, want 1: %v", len(events), events)
	}
	if events[0]["event_type"] != "darkroom.asset.ready" {
		t.Errorf("event_type = %v, want darkroom.asset.ready", events[0]["event_type"])
	}
	if events[0]["source"] != Source {
		t.Errorf("source = %v, want %q", events[0]["source"], Source)
	}
	if events[0]["subject"] != ready.ID {
		t.Errorf("subject = %v, want the asset id %q", events[0]["subject"], ready.ID)
	}
	data, _ := events[0]["data"].(map[string]any)
	if data == nil || data["checksum"] != checksum.SHA256Hex(pngBytes) {
		t.Errorf("the event data does not carry the computed checksum: %v", data)
	}
}

func TestTheClientBytesNeverPassThroughTheService(t *testing.T) {
	h := newHarness(t)
	h.upload(h.alice, "photo.png", "image/png", pngBytes)

	// ONE assertion, and it is the one that distinguishes a signed-upload design
	// from a proxy: the service must never have written to the object store on the
	// client's behalf. "A signed-upload design that quietly proxied every upload
	// would still pass every functional test and fail this one."
	stats := h.objects.Stats()
	if stats.Puts != 0 {
		t.Errorf("the service wrote %d objects; the client PUTs straight to storage", stats.Puts)
	}
	if stats.Gets != 1 {
		t.Errorf("the service read the object %d times at complete; exactly one read is the stated trade", stats.Gets)
	}
	if stats.Heads != 1 {
		t.Errorf("the service probed %d times; head-then-get-then-hash is one of each", stats.Heads)
	}
}

func TestTheComputedChecksumIsWrittenNotTheClaim(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()
	sum := checksum.SHA256Hex(pngBytes)

	created, pErr := h.svc.CreateUpload(ctx, h.alice, CreateUpload{
		Filename: "photo.png", ContentType: "image/png",
		ByteSize: int64(len(pngBytes)), Checksum: sum,
	})
	if pErr != nil {
		t.Fatalf("CreateUpload: %v", pErr)
	}
	// Before complete, the row carries the client's CLAIM — that is what the unique
	// constraint is keyed on.
	if created.Asset.Checksum != sum {
		t.Errorf("a pending asset carries %q, want the claim %q", created.Asset.Checksum, sum)
	}
	if created.Asset.Status != domain.StatusPending {
		t.Errorf("a new asset is %q, want pending", created.Asset.Status)
	}
	if err := h.put(ctx, created, pngBytes, "image/png"); err != nil {
		t.Fatal(err)
	}
	ready, pErr := h.svc.CompleteUpload(ctx, h.alice, created.Asset.ID, sum)
	if pErr != nil {
		t.Fatalf("CompleteUpload: %v", pErr)
	}
	if ready.Checksum != sum {
		t.Errorf("the ready asset carries %q, want the computed %q", ready.Checksum, sum)
	}
	if got := h.assetsRow(t, h.alice, ready.ID)["checksum"]; got != sum {
		t.Errorf("the COLUMN carries %v, want the computed value: the claimed one is never stored", got)
	}
}

// ----------------------------------------------------------------------------
// the three failure paths, each of which must mark the asset `failed`
// ----------------------------------------------------------------------------

func TestAChecksumMismatchFailsTheAsset(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()

	created, pErr := h.svc.CreateUpload(ctx, h.alice, CreateUpload{
		Filename: "photo.png", ContentType: "image/png",
		ByteSize: int64(len(pngBytes)) + 1, Checksum: checksum.SHA256Hex(pngBytes),
	})
	if pErr != nil {
		t.Fatalf("CreateUpload: %v", pErr)
	}
	// The client uploads DIFFERENT bytes than it claimed: the last of the three
	// reasons the Rust header names, and the one that is an attack rather than a bug.
	// The DECLARED SIZE is the uploaded size, so the size check passes and the
	// checksum is the only thing that disagrees — otherwise this test would prove the
	// size check twice.
	other := append(append([]byte{}, pngBytes...), 'x')
	if err := h.objects.Put(ctx, created.Asset.StorageKey(), other, "image/png"); err != nil {
		t.Fatal(err)
	}

	_, pErr = h.svc.CompleteUpload(ctx, h.alice, created.Asset.ID, checksum.SHA256Hex(pngBytes))
	h.assert(pErr).status(422, "a checksum that does not match the stored bytes").code("validation_failed", "").field("checksum", "")

	asset := h.asset(t, h.alice, created.Asset.ID)
	if asset.Status != domain.StatusFailed {
		t.Fatalf("status = %q, want failed", asset.Status)
	}
	if asset.Metadata["failure_reason"] != "checksum_mismatch" {
		t.Errorf("the row does not say why: %v", asset.Metadata)
	}
	// NO EVENT FOR A FAILURE.
	if events := h.outboxEvents(t); len(events) != 0 {
		t.Errorf("a failed upload emitted %d events; a consumer cannot act on it", len(events))
	}
}

func TestAnAbsentObjectFailsTheAsset(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()
	body := []byte("never uploaded")

	created, pErr := h.svc.CreateUpload(ctx, h.alice, CreateUpload{
		Filename: "note.txt", ContentType: "application/json",
		ByteSize: int64(len(body)), Checksum: checksum.SHA256Hex(body),
	})
	if pErr != nil {
		t.Fatalf("CreateUpload: %v", pErr)
	}
	// The presigned URL was issued and never used.
	_, pErr = h.svc.CompleteUpload(ctx, h.alice, created.Asset.ID, checksum.SHA256Hex(body))
	h.assert(pErr).status(409, "an upload that never landed").code("conflict", "")

	asset := h.asset(t, h.alice, created.Asset.ID)
	if asset.Status != domain.StatusFailed {
		t.Fatalf("status = %q, want failed", asset.Status)
	}
	if asset.Metadata["failure_reason"] != "object_absent_at_complete" {
		t.Errorf("the row does not say why: %v", asset.Metadata)
	}
}

func TestAMeasuredSizeThatDisagreesWithTheDeclaredFailsTheAsset(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()

	// Declare 10 bytes, upload 20. The object is THERE and it is the wrong one, so
	// this is a 422 rather than a 409 — the Rust tree draws exactly that line.
	declared := checksum.SHA256Hex([]byte("0123456789"))
	created, pErr := h.svc.CreateUpload(ctx, h.alice, CreateUpload{
		Filename: "note.json", ContentType: "application/json",
		ByteSize: 10, Checksum: declared,
	})
	if pErr != nil {
		t.Fatalf("CreateUpload: %v", pErr)
	}
	if err := h.objects.Put(ctx, created.Asset.StorageKey(), []byte("twenty bytes long!!"), "application/json"); err != nil {
		t.Fatal(err)
	}

	_, pErr = h.svc.CompleteUpload(ctx, h.alice, created.Asset.ID, declared)
	h.assert(pErr).status(422, "a measured size that is not the declared one").field("byte_size", "")

	asset := h.asset(t, h.alice, created.Asset.ID)
	if asset.Status != domain.StatusFailed {
		t.Fatalf("status = %q, want failed", asset.Status)
	}
	if asset.Metadata["failure_reason"] != "size_mismatch" {
		t.Errorf("the row does not say why: %v", asset.Metadata)
	}
}

func TestAMalformedChecksumIsRejectedAtCreateBeforeAnythingIsWritten(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()
	for _, bad := range []string{"", "abc", strings.Repeat("z", 64), strings.Repeat("a", 63), strings.Repeat("a", 65)} {
		_, pErr := h.svc.CreateUpload(ctx, h.alice, CreateUpload{
			Filename: "photo.png", ContentType: "image/png",
			ByteSize: int64(len(pngBytes)), Checksum: bad,
		})
		h.assert(pErr).status(422, "checksum "+bad).field("checksum", "")
	}
	if rows := h.countAssets(t, h.alice); rows != 0 {
		t.Errorf("%d assets were written; a malformed checksum is refused before the write", rows)
	}
	if h.objects.Stats().Presigns != 0 {
		t.Error("a URL was signed for a request that was refused")
	}
	// And the same shape is refused at complete, where a client that computed the
	// digest wrong would otherwise upload first and find out afterwards.
	created, pErr := h.svc.CreateUpload(ctx, h.alice, CreateUpload{
		Filename: "photo.png", ContentType: "image/png",
		ByteSize: int64(len(pngBytes)), Checksum: checksum.SHA256Hex(pngBytes),
	})
	if pErr != nil {
		t.Fatal(pErr)
	}
	_, pErr = h.svc.CompleteUpload(ctx, h.alice, created.Asset.ID, "not-a-digest")
	h.assert(pErr).status(422, "a malformed checksum at complete").field("checksum", "")
}

func TestByteSizeIsBoundedAtCreateAndNotAfterwards(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()
	sum := checksum.SHA256Hex(pngBytes)

	_, pErr := h.svc.CreateUpload(ctx, h.alice, CreateUpload{
		Filename: "huge.png", ContentType: "image/png",
		ByteSize: -1, Checksum: sum,
	})
	h.assert(pErr).status(422, "a negative size").field("byte_size", "")

	_, pErr = h.svc.CreateUpload(ctx, h.alice, CreateUpload{
		Filename: "huge.png", ContentType: "image/png",
		ByteSize: objectstore.MaxUploadBytes + 1, Checksum: sum,
	})
	// A 422 BEFORE a URL is issued, so a client that cannot possibly upload never
	// holds a credential.
	h.assert(pErr).status(422, "a size above the cap").field("byte_size", "")
	if h.objects.Stats().Presigns != 0 {
		t.Error("an oversized request was signed for")
	}
}

// ----------------------------------------------------------------------------
// the state machine's terminal states
// ----------------------------------------------------------------------------

func TestReadyAndFailedAreTerminalAndAreNotRetried(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()
	body := []byte("terminal")

	// --- ready ----------------------------------------------------------
	readyCreated, pErr := h.svc.CreateUpload(ctx, h.alice, CreateUpload{
		Filename: "note.json", ContentType: "application/json",
		ByteSize: int64(len(body)), Checksum: checksum.SHA256Hex(body),
	})
	if pErr != nil {
		t.Fatal(pErr)
	}
	if err := h.objects.Put(ctx, readyCreated.Asset.StorageKey(), body, "application/json"); err != nil {
		t.Fatal(err)
	}
	first, pErr := h.svc.CompleteUpload(ctx, h.alice, readyCreated.Asset.ID, checksum.SHA256Hex(body))
	if pErr != nil {
		t.Fatal(pErr)
	}
	// Completing a READY asset again returns the asset rather than refusing: reaching
	// here with `ready` means the idempotency key was not used, and a repeated
	// complete that returns the asset is a success.
	second, pErr := h.svc.CompleteUpload(ctx, h.alice, readyCreated.Asset.ID, checksum.SHA256Hex(body))
	if pErr != nil {
		t.Fatalf("completing a ready asset again: %v", pErr)
	}
	if second.ID != first.ID || second.Status != domain.StatusReady {
		t.Errorf("the second complete returned %v", second)
	}
	// AND NO SECOND EVENT: the compare-and-set matched nothing the second time.
	if events := h.outboxEvents(t); len(events) != 1 {
		t.Errorf("outbox has %d events after two completes, want 1: %v", len(events), events)
	}

	// --- failed ---------------------------------------------------------
	failedCreated, pErr := h.svc.CreateUpload(ctx, h.alice, CreateUpload{
		Filename: "other.json", ContentType: "application/json",
		ByteSize: int64(len(body)), Checksum: checksum.SHA256Hex([]byte("other")),
	})
	if pErr != nil {
		t.Fatal(pErr)
	}
	if _, pErr := h.svc.CompleteUpload(ctx, h.alice, failedCreated.Asset.ID, checksum.SHA256Hex(body)); pErr == nil {
		t.Fatal("an absent object must fail")
	}
	// The bytes arrive afterwards — a late PUT, a retried upload — and the asset
	// STAYS failed. "A failed upload is never retried into `ready`; the client
	// creates a new one."
	if err := h.objects.Put(ctx, failedCreated.Asset.StorageKey(), body, "application/json"); err != nil {
		t.Fatal(err)
	}
	_, pErr = h.svc.CompleteUpload(ctx, h.alice, failedCreated.Asset.ID, checksum.SHA256Hex(body))
	h.assert(pErr).status(409, "completing an asset that already failed").code("conflict", "")
	if asset := h.asset(t, h.alice, failedCreated.Asset.ID); asset.Status != domain.StatusFailed {
		t.Fatalf("a late PUT moved the asset to %q; `failed` is terminal", asset.Status)
	}
}

func TestADuplicateReturnsTheExistingAssetAndNeverCreatesASecond(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()
	_, first, _ := h.upload(h.alice, "photo.png", "image/png", pngBytes)

	again, pErr := h.svc.CreateUpload(ctx, h.alice, CreateUpload{
		Filename: "a-different-name.png", ContentType: "image/png",
		ByteSize: int64(len(pngBytes)), Checksum: checksum.SHA256Hex(pngBytes),
	})
	if pErr != nil {
		t.Fatalf("a duplicate must be 201 with the existing asset, not a 409: %v", pErr)
	}
	if !again.Duplicate {
		t.Error("Duplicate is false, so the handler cannot label the response")
	}
	if again.Asset.ID != first.ID {
		t.Errorf("the duplicate created asset %q, want the existing %q", again.Asset.ID, first.ID)
	}
	// Re-presigned against the EXISTING key, so a client that just wants to PUT and
	// complete makes the same three calls either way.
	if again.Presigned.Key != first.StorageKey() {
		t.Errorf("the re-issued URL is for %q, want the existing %q", again.Presigned.Key, first.StorageKey())
	}
	if rows := h.countAssets(t, h.alice); rows != 1 {
		t.Errorf("%d assets exist; the same bytes for one account are ONE asset", rows)
	}

	// A failed asset's checksum was never verified, so returning it would hand back
	// something unusable — and the constraint still holds the row, so it is a 409.
	failedBody := []byte("doomed")
	failedCreated, pErr := h.svc.CreateUpload(ctx, h.alice, CreateUpload{
		Filename: "doomed.json", ContentType: "application/json",
		ByteSize: int64(len(failedBody)), Checksum: checksum.SHA256Hex(failedBody),
	})
	if pErr != nil {
		t.Fatal(pErr)
	}
	if _, pErr := h.svc.CompleteUpload(ctx, h.alice, failedCreated.Asset.ID, checksum.SHA256Hex(failedBody)); pErr == nil {
		t.Fatal("the doomed upload should have failed")
	}
	_, pErr = h.svc.CreateUpload(ctx, h.alice, CreateUpload{
		Filename: "doomed.json", ContentType: "application/json",
		ByteSize: int64(len(failedBody)), Checksum: checksum.SHA256Hex(failedBody),
	})
	h.assert(pErr).status(409, "a duplicate of a FAILED asset").code("conflict", "")
}

func TestTheSameBytesInAnotherAccountAreNotADuplicate(t *testing.T) {
	h := newHarness(t)
	_, _, _ = h.upload(h.alice, "photo.png", "image/png", pngBytes)
	_, other, _ := h.upload(h.bob, "photo.png", "image/png", pngBytes)

	// `UNIQUE (account_id, checksum)` has the account in its scope, and RLS hides
	// alice's row from bob entirely — so neither the constraint nor the pre-check
	// can see across the boundary.
	if other.AccountID != bobAccount {
		t.Errorf("bob's asset carries account %q", other.AccountID)
	}
	if alice, bob := h.countAssets(t, h.alice), h.countAssets(t, h.bob); alice != 1 || bob != 1 {
		t.Errorf("alice sees %d assets and bob sees %d, want 1 each: the constraint is (account_id, checksum) and the policy hides the other tenant entirely", alice, bob)
	}
}

// ----------------------------------------------------------------------------
// tenant isolation
// ----------------------------------------------------------------------------

func TestACrossTenantReadIsTheSame404AsAMissingOne(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()
	_, ready, _ := h.upload(h.alice, "photo.png", "image/png", pngBytes)

	// THE REGRESSION GUARD `AGENTS.md` names, on this service's read paths:
	// "a_cross_tenant_404_is_indistinguishable_from_a_missing_one […] covers **all
	// five** id-scoped routes, not just `GET /v1/assets/{id}`".
	missing := "99999999-9999-4999-8999-999999999999"

	for name, call := range map[string]func(id string) *problemAssert{
		"get asset": func(id string) *problemAssert {
			_, err := h.svc.GetAsset(ctx, h.bob, id)
			return h.assert(err)
		},
		"complete": func(id string) *problemAssert {
			_, err := h.svc.CompleteUpload(ctx, h.bob, id, checksum.SHA256Hex(pngBytes))
			return h.assert(err)
		},
		"delete": func(id string) *problemAssert {
			return h.assert(h.svc.DeleteAsset(ctx, h.bob, id))
		},
		"list variants": func(id string) *problemAssert {
			_, err := h.svc.ListVariants(ctx, h.bob, id)
			return h.assert(err)
		},
		"create variant": func(id string) *problemAssert {
			_, err := h.svc.CreateVariant(ctx, h.bob, id, variants.KindThumbnail)
			return h.assert(err)
		},
	} {
		foreign := call(ready.ID).status(404, "bob reading alice's asset via "+name)
		absent := call(missing).status(404, "bob reading an id that never existed via "+name)

		// BYTE-IDENTICAL BODIES, not merely equal statuses: "a different body is
		// just as much of an oracle as a different status."
		if foreignBody(t, foreign) != foreignBody(t, absent) {
			t.Errorf("%s: the cross-tenant 404 and the missing 404 differ:\n  %s\n  %s",
				name, foreignBody(t, foreign), foreignBody(t, absent))
		}
	}

	// And the asset survived every one of those attempts.
	if got := h.asset(t, h.alice, ready.ID); got.Status != domain.StatusReady {
		t.Fatalf("alice's asset is %q after bob tried to delete it", got.Status)
	}
	if events := h.outboxEvents(t); len(events) != 1 {
		t.Errorf("outbox has %d events, want 1: a refused delete emits nothing", len(events))
	}
}

func TestListAssetsSeesOnlyTheCallersAccount(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()
	// DISTINCT bytes per upload: the same bytes for one account are ONE asset by
	// `UNIQUE (account_id, checksum)`, and a listing test that uploaded the same file
	// three times would measure the duplicate rule instead of the listing.
	for i := range 3 {
		body := append(append([]byte{}, pngBytes...), byte('a'+i))
		_, _, res := h.upload(h.alice, "a.png", "image/png", body)
		res.ok()
	}
	// Bob uploads DIFFERENT bytes: the same bytes would resolve to alice's asset if
	// he could see it, which would hide the leak behind a duplicate.
	bobBody := append(pngBytes, byte('b'))
	h.upload(h.bob, "b.png", "image/png", bobBody)

	page, pErr := h.svc.ListAssets(ctx, h.alice, ListQuery{Limit: 100})
	if pErr != nil {
		t.Fatalf("ListAssets: %v", pErr)
	}
	if len(page.Assets) != 3 {
		t.Errorf("alice sees %d assets, want her own 3", len(page.Assets))
	}
	for _, a := range page.Assets {
		if a.AccountID != aliceAccount {
			t.Errorf("alice's page contains an asset owned by %q", a.AccountID)
		}
	}
}

func TestAnAccountIdentityThatDoesNotMatchTheRowCannotBeWritten(t *testing.T) {
	// The insert takes its `account_id` from the TRANSACTION'S IDENTITY rather than
	// from an argument, so there is no value for a caller to get wrong. This walks
	// the property: the row alice inserts carries alice, whatever she asked for.
	h := newHarness(t)
	ctx := context.Background()
	_, ready, _ := h.upload(h.alice, "photo.png", "image/png", pngBytes)
	if ready.AccountID != aliceAccount {
		t.Errorf("the row carries %q, want the transaction's identity %q", ready.AccountID, aliceAccount)
	}
	// An empty account is refused at the seam rather than reading nothing:
	// `InAccountTx` raises, because a NULL identity matches nothing and a statement
	// that deletes nothing "exits 0".
	if _, pErr := h.svc.GetAsset(ctx, Caller{AccountID: "", UserID: aliceUser}, ready.ID); pErr == nil {
		t.Error("an empty account must not read anything")
	}
}

// ----------------------------------------------------------------------------
// pagination
// ----------------------------------------------------------------------------

func TestPaginationClampsTheLimitAndWalksEveryRowExactlyOnce(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()
	// Distinct bytes per asset, so `UNIQUE (account_id, checksum)` does not collapse
	// them into duplicates — a page test over duplicates measures the wrong thing.
	const total = 7
	for i := range total {
		body := append(pngBytes, byte('a'+i))
		created, pErr := h.svc.CreateUpload(ctx, h.alice, CreateUpload{
			Filename: "photo.png", ContentType: "image/png",
			ByteSize: int64(len(body)), Checksum: checksum.SHA256Hex(body),
		})
		if pErr != nil {
			t.Fatalf("creating asset %d: %v", i, pErr)
		}
		if err := h.objects.Put(ctx, created.Asset.StorageKey(), body, "image/png"); err != nil {
			t.Fatal(err)
		}
		if _, pErr := h.svc.CompleteUpload(ctx, h.alice, created.Asset.ID, checksum.SHA256Hex(body)); pErr != nil {
			t.Fatalf("completing asset %d: %v", i, pErr)
		}
	}

	// A request for MORE than the cap is CLAMPED, not rejected: "the cap is a promise
	// to the server about how much work it will do, not a validation rule for the
	// client to satisfy."
	clamped, pErr := h.svc.ListAssets(ctx, h.alice, ListQuery{Limit: 1000})
	if pErr != nil {
		t.Fatalf("a limit above the cap: %v", pErr)
	}
	if len(clamped.Assets) != total {
		t.Errorf("a clamped page has %d rows, want %d", len(clamped.Assets), total)
	}
	if clamped.HasMore {
		t.Error("a page holding every row reports more")
	}

	// And the DEFAULT applies to a request that named no limit at all.
	deflt, pErr := h.svc.ListAssets(ctx, h.alice, ListQuery{})
	if pErr != nil {
		t.Fatal(pErr)
	}
	if len(deflt.Assets) != min(total, DefaultPageLimit) {
		t.Errorf("the default page has %d rows, want %d", len(deflt.Assets), min(total, DefaultPageLimit))
	}

	// WALK IT: every id exactly once, newest first, and no page that claims more and
	// then stops.
	seen := map[string]bool{}
	order := []string{}
	var cursor *Cursor
	pages := 0
	for {
		page, pErr := h.svc.ListAssets(ctx, h.alice, ListQuery{Limit: 3, Cursor: cursor})
		if pErr != nil {
			t.Fatalf("page %d: %v", pages, pErr)
		}
		pages++
		for _, a := range page.Assets {
			if seen[a.ID] {
				t.Fatalf("asset %s appeared on two pages", a.ID)
			}
			seen[a.ID] = true
			order = append(order, a.ID)
		}
		if !page.HasMore {
			break
		}
		if page.Next == nil || page.NextToken == "" {
			t.Fatal("a page with more rows carries no cursor")
		}
		decoded, pErr2 := DecodeCursor(page.NextToken)
		if pErr2 != nil {
			t.Fatalf("the cursor does not decode: %v", pErr2)
		}
		cursor = decoded
		if pages > total {
			t.Fatal("pagination did not terminate")
		}
	}
	if len(seen) != total {
		t.Errorf("the walk saw %d of %d assets", len(seen), total)
	}
	for i := 1; i < len(order); i++ {
		if order[i] == order[i-1] {
			t.Errorf("the walk repeated %s", order[i])
		}
	}
}

// ----------------------------------------------------------------------------
// variants and delete
// ----------------------------------------------------------------------------

func TestCreateAndListVariants(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()
	_, ready, _ := h.upload(h.alice, "photo.png", "image/png", pngBytes)

	for _, kind := range []variants.VariantKind{variants.KindThumbnail, variants.KindPreview} {
		v, pErr := h.svc.CreateVariant(ctx, h.alice, ready.ID, kind)
		if pErr != nil {
			t.Fatalf("CreateVariant(%s): %v", kind, pErr)
		}
		if v.Kind != kind || v.ContentType != kind.ContentType() {
			t.Errorf("variant is %q/%q", v.Kind, v.ContentType)
		}
		if v.Width == nil || v.Height == nil || *v.Width <= 0 {
			t.Errorf("variant has no dimensions: %v", v)
		}
		// The bytes really are in storage, under a DIFFERENT key namespace from the
		// original, so a delete of one can never address the other.
		if v.StorageKey() == ready.StorageKey() {
			t.Error("the variant shares the original's key")
		}
		if _, err := h.objects.Head(ctx, v.StorageKey()); err != nil {
			t.Errorf("the variant object is not in storage: %v", err)
		}
	}

	// Re-requesting a kind REPLACES rather than accumulating.
	if _, pErr := h.svc.CreateVariant(ctx, h.alice, ready.ID, variants.KindThumbnail); pErr != nil {
		t.Fatalf("re-requesting a thumbnail: %v", pErr)
	}
	listed, pErr := h.svc.ListVariants(ctx, h.alice, ready.ID)
	if pErr != nil {
		t.Fatal(pErr)
	}
	if len(listed) != 2 {
		t.Errorf("listing %d variants, want 2 after a repeat request", len(listed))
	}

	events := h.outboxEvents(t)
	if len(events) != 4 {
		t.Errorf("outbox has %d events, want 1 ready + 3 variant-created: %v", len(events), events)
	}

	// Only a READY IMAGE can be derived from, and the refusal names WHICH.
	pending, pErr := h.svc.CreateUpload(ctx, h.alice, CreateUpload{
		Filename: "pending.png", ContentType: "image/png",
		ByteSize: int64(len(pngBytes)), Checksum: checksum.SHA256Hex([]byte("pending")),
	})
	if pErr != nil {
		t.Fatal(pErr)
	}
	_, pErr = h.svc.CreateVariant(ctx, h.alice, pending.Asset.ID, variants.KindThumbnail)
	h.assert(pErr).status(422, "a pending asset").field("status", "")

	videoBody := []byte("not an image, but a declared mp4")
	video, pErr := h.svc.CreateUpload(ctx, h.alice, CreateUpload{
		Filename: "clip.mp4", ContentType: "video/mp4",
		ByteSize: int64(len(videoBody)), Checksum: checksum.SHA256Hex(videoBody),
	})
	if pErr != nil {
		t.Fatal(pErr)
	}
	if err := h.objects.Put(ctx, video.Asset.StorageKey(), videoBody, "video/mp4"); err != nil {
		t.Fatal(err)
	}
	if _, pErr := h.svc.CompleteUpload(ctx, h.alice, video.Asset.ID, checksum.SHA256Hex(videoBody)); pErr != nil {
		t.Fatal(pErr)
	}
	_, pErr = h.svc.CreateVariant(ctx, h.alice, video.Asset.ID, variants.KindThumbnail)
	h.assert(pErr).status(422, "a ready VIDEO").field("kind", "")
}

func TestDeleteRemovesTheRowEveryObjectAndEmitsOneEvent(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()
	_, ready, _ := h.upload(h.alice, "photo.png", "image/png", pngBytes)
	variant, pErr := h.svc.CreateVariant(ctx, h.alice, ready.ID, variants.KindThumbnail)
	if pErr != nil {
		t.Fatal(pErr)
	}
	if h.objects.Len() != 2 {
		t.Fatalf("storage holds %d objects, want 2 before the delete", h.objects.Len())
	}

	if pErr := h.svc.DeleteAsset(ctx, h.alice, ready.ID); pErr != nil {
		t.Fatalf("DeleteAsset: %v", pErr)
	}
	if _, pErr := h.svc.GetAsset(ctx, h.alice, ready.ID); pErr == nil {
		t.Error("the asset survived its own delete")
	}
	// EVERY variant's object goes too: "deleting an asset whose thumbnail remains is
	// not a delete."
	if !h.objects.IsEmpty() {
		t.Errorf("storage still holds %d objects after the delete", h.objects.Len())
	}
	if h.objects.Stats().Deletes != 2 {
		t.Errorf("the service issued %d deletes, want 2", h.objects.Stats().Deletes)
	}
	events := h.outboxEvents(t)
	if len(events) != 3 || events[2]["event_type"] != "darkroom.asset.deleted" {
		t.Errorf("outbox after a delete: %v", events)
	}
	// The variant row is gone through the migration's `on delete cascade`, and
	// deleting it is NOT left for a sweep.
	if rows := h.countVariants(t, h.alice); rows != 0 {
		t.Errorf("%d variant rows survived", rows)
	}
	_ = variant

	// A second delete is a 404, not a success and not a 500: the caller asked for it
	// not to exist and it does not.
	if pErr := h.svc.DeleteAsset(ctx, h.alice, ready.ID); pErr == nil {
		t.Error("deleting an absent asset must be a 404")
	} else {
		h.assert(pErr).status(404, "a repeated delete")
	}
}

// ----------------------------------------------------------------------------
// the outbox transaction, asserted from the negative
// ----------------------------------------------------------------------------

func TestARolledBackTransactionLeavesZeroOutboxRows(t *testing.T) {
	// THE HALF OF THE OUTBOX RULE THAT CANNOT BE PROVED BY WATCHING A SUCCESS.
	// "A transaction that rolls back must leave zero rows in `outbox_events`", and a
	// green complete-upload test proves nothing about it.
	pg := pgtest.New(t)
	db := pg.Store()
	objects := objectstore.NewMemory()
	svc := New(db, objects)

	ctx := context.Background()
	sum := checksum.SHA256Hex(pngBytes)
	created, pErr := svc.CreateUpload(ctx, Caller{AccountID: aliceAccount, UserID: aliceUser}, CreateUpload{
		Filename: "photo.png", ContentType: "image/png",
		ByteSize: int64(len(pngBytes)), Checksum: sum,
	})
	if pErr != nil {
		t.Fatal(pErr)
	}
	createdID := created.Asset.ID
	key := created.Asset.StorageKey()

	// Roll back a transaction that DID enqueue, exactly the way a failed commit
	// after the domain write would.
	errRollback := db.InAccountTx(ctx, aliceAccount, func(tx pgxTx) error {
		if _, err := markReady(ctx, tx, createdID, sum); err != nil {
			return err
		}
		if err := enqueue(ctx, tx, NewEvent(EventAssetReady, createdID, time.Now(), map[string]any{
			"asset_id": createdID,
		})); err != nil {
			return err
		}
		if n, err := countOutbox(ctx, tx); err != nil {
			return err
		} else if n != 1 {
			return errAlreadyGone // the row IS visible inside the transaction
		}
		return errRollbackSentinel
	})
	if errRollback == nil {
		t.Fatal("the transaction was supposed to roll back")
	}

	// Outside it: zero rows, and the asset is still `pending`. Both halves, because a
	// rollback that undid the event but kept the `ready` row would be a different
	// bug and an easier one to miss.
	if n := countOutboxOutside(t, db); n != 0 {
		t.Errorf("outbox has %d rows after a rollback, want 0", n)
	}
	asset, pErr := svc.GetAsset(ctx, Caller{AccountID: aliceAccount, UserID: aliceUser}, createdID)
	if pErr != nil {
		t.Fatal(pErr)
	}
	if asset.Status != domain.StatusPending {
		t.Errorf("the asset is %q after a rollback, want pending: the transaction undid both writes", asset.Status)
	}

	// And the happy path afterwards still works, so the rollback did not leave the
	// connection or the row wedged.
	if err := objects.Put(ctx, key, pngBytes, "image/png"); err != nil {
		t.Fatal(err)
	}
	ready, pErr := svc.CompleteUpload(ctx, Caller{AccountID: aliceAccount, UserID: aliceUser}, createdID, sum)
	if pErr != nil {
		t.Fatalf("completing after a rollback: %v", pErr)
	}
	if ready.Status != domain.StatusReady || ready.ID != createdID {
		t.Errorf("after the rollback the complete produced %v", ready)
	}
	if n := countOutboxOutside(t, db); n != 1 {
		t.Errorf("outbox has %d rows after a successful complete, want 1", n)
	}
}

// errRollbackSentinel is how the rollback test says "return an error on purpose".
var errRollbackSentinel = errors.New("rolling back on purpose")

// ----------------------------------------------------------------------------
// fixtures
// ----------------------------------------------------------------------------

func (h *harness) asset(t *testing.T, caller Caller, id string) *domain.Asset {
	t.Helper()
	asset, pErr := h.svc.GetAsset(context.Background(), caller, id)
	if pErr != nil {
		t.Fatalf("reading back the asset: %v", pErr)
	}
	return asset
}

// assetsRow reads one row as the DATABASE holds it, bypassing the service.
//
// THROUGH AN ACCOUNT TRANSACTION, and that is the finding rather than the plumbing:
// a query on a connection with no identity set reads ZERO ROWS of a FORCE-RLS table
// and reports no error. `AGENTS.md` names it — "a `DELETE` with no `begin_account`
// **deletes nothing and exits 0**" — and this helper is the first thing in this
// repository that trips over it in practice, because the first draft of it read on
// `pgtest.Test.Conn` and reported "the row is not there" for an asset that had just
// been created.
func (h *harness) assetsRow(t *testing.T, caller Caller, id string) map[string]any {
	t.Helper()
	var idText, accountText, sum, status string
	err := h.pg.Store().InAccountTx(context.Background(), caller.AccountID, func(tx pgxTx) error {
		return tx.QueryRow(context.Background(),
			`select id::text, account_id::text, checksum, status from assets where id = $1`, id).
			Scan(&idText, &accountText, &sum, &status)
	})
	if err != nil {
		t.Fatalf("reading the row directly: %v", err)
	}
	return map[string]any{
		"id": idText, "account_id": accountText, "checksum": sum, "status": status,
	}
}

// countAssets counts the rows ONE ACCOUNT can see.
//
// PER ACCOUNT and not globally, and the reason is the same FORCE-RLS property: there
// is no session that can count every tenant's rows without disabling the policies,
// which is precisely what `darkroom_app` cannot do. A test that wanted a global count
// would have to turn RLS off to get it, and that is the capability this database does
// not hand out on purpose.
func (h *harness) countAssets(t *testing.T, caller Caller) int {
	t.Helper()
	return h.scalar(t, caller, `select count(*) from assets`)
}

func (h *harness) countVariants(t *testing.T, caller Caller) int {
	t.Helper()
	return h.scalar(t, caller, `select count(*) from asset_variants`)
}

func (h *harness) scalar(t *testing.T, caller Caller, query string) int {
	t.Helper()
	var n int
	if err := h.pg.Store().InAccountTx(context.Background(), caller.AccountID, func(tx pgxTx) error {
		return tx.QueryRow(context.Background(), query).Scan(&n)
	}); err != nil {
		t.Fatalf("%s: %v", query, err)
	}
	return n
}

// put writes bytes the way a client does, through the presigned URL.
func (h *harness) put(ctx context.Context, created *CreatedUpload, body []byte, contentType string) error {
	return h.objects.ApplyPresignedPut(ctx, created.Presigned.URL, body, contentType)
}

func (h *harness) outboxEvents(t *testing.T) []map[string]any {
	t.Helper()
	rows, err := h.pg.Conn.Query(context.Background(),
		`select event_type, source, subject, data from outbox_events order by created_at, id`)
	if err != nil {
		t.Fatalf("reading the outbox: %v", err)
	}
	defer rows.Close()
	out := []map[string]any{}
	for rows.Next() {
		var eventType, source, subject string
		var data []byte
		if err := rows.Scan(&eventType, &source, &subject, &data); err != nil {
			t.Fatalf("scanning an event: %v", err)
		}
		var decoded map[string]any
		if err := json.Unmarshal(data, &decoded); err != nil {
			t.Fatalf("event data is not JSON: %v", err)
		}
		out = append(out, map[string]any{"event_type": eventType, "source": source, "subject": subject, "data": decoded})
	}
	return out
}

// countOutboxOutside reads the queue on a connection that is not inside the
// transaction being asserted about — which is the whole point: a row visible inside a
// transaction is not a committed row.
func countOutboxOutside(t *testing.T, db *darkroomdb.Store) int {
	t.Helper()
	var n int
	if err := db.InTx(context.Background(), func(tx pgxTx) error {
		return tx.QueryRow(context.Background(), `select count(*) from outbox_events`).Scan(&n)
	}); err != nil {
		t.Fatalf("counting outbox rows: %v", err)
	}
	return n
}

func scalarCount(t *testing.T, conn *pgx.Conn, query string) int {
	t.Helper()
	var n int
	if err := conn.QueryRow(context.Background(), query).Scan(&n); err != nil {
		t.Fatalf("%s: %v", query, err)
	}
	return n
}

// foreignBody renders the problem a cross-tenant 404 WOULD carry, so the comparison
// is on the bytes a client would receive rather than on two error values.
func foreignBody(t *testing.T, p *problemAssert) string {
	t.Helper()
	if p.err == nil {
		t.Fatal("expected a 404 to render, got success")
	}
	// ONE instance and ONE trace id for both, so the comparison isolates the parts
	// that could leak and nothing else. `problem.FromDB` builds a NotFound with the
	// text "the requested item does not exist", so this is a real cross-check of the
	// mapper rather than a tautology over one constant.
	raw, marshalErr := json.Marshal(p.err.Problem("/v1/assets/"+missingID, "trace-1"))
	if marshalErr != nil {
		t.Fatal(marshalErr)
	}
	return string(raw)
}

const missingID = "99999999-9999-4999-8999-999999999999"
