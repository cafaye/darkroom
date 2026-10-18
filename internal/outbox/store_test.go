package outbox

// The claim query, held in place.
//
// [claimQuery] is the one statement in this repository that reads a table no
// policy guards, so it is the one statement where a JOIN is exactly the kind of
// edit that reads as an improvement and is not. The property it would break is
// the tenant axis: `outbox_events` has no `account_id` and no policy, so a claim
// that reached into `assets` would read another account's rows under a
// transaction carrying no identity at all — the "reports success and did nothing"
// failure `AGENTS.md` names, in its most expensive form.
//
// Read as text, in the shape `internal/variants/variants_test.go` already uses
// for its blank imports and `tests/tenant_scoping.rs` used for the Rust store's
// predicates. The property being held is about the FILE.

import (
	"os"
	"strings"
	"testing"
)

// The claim names ONE table and it is the unscoped one. Not `assets`, not
// `asset_variants`, not a join carrying a checksum onto the bus.
func TestTheClaimQueryNamesOneTableAndItIsTheUnscopedOne(t *testing.T) {
	if !strings.Contains(claimQuery, "outbox_events") {
		t.Error("the claim query does not name outbox_events")
	}

	// Every table name the account-scoped migrations create, and none of them may
	// appear in a claim that runs with no identity set.
	forbidden := []string{"assets", "asset_variants", "idempotency_keys", "api_keys"}
	for _, table := range forbidden {
		if strings.Contains(claimQuery, table) {
			t.Errorf("the claim query names %s: the claim transaction sets NO account identity, "+
				"so reaching a scoped table from it would read another tenant's rows and report success", table)
		}
	}
}

// The four properties `internal/service/outbox.go` named in advance, asserted on
// the text rather than on a database run, because a `for update` that quietly
// became a plain `for update` is a bug no test without two replicas sees.
func TestTheClaimQueryCarriesAllFourDocumentedProperties(t *testing.T) {
	query := strings.ToLower(claimQuery)

	cases := []struct {
		property string
		want     string
		why      string
	}{
		// The ONLY definition of unpublished. Not `attempts = 0`, and not anything
		// an in-memory queue says.
		{"the definition of unpublished", "published_at is null", "a row is published when published_at is set, and not when attempts is zero"},
		// Per-subject ordering: `time` is the moment of the STATE CHANGE, so two
		// events for one asset can sit in created_at order differently from the
		// order the state changed in.
		{"the ordering", "order by created_at", "a consumer building a thumbnail and one deleting the asset must not see them reordered"},
		// What lets N replicas run at once. A bare `for update` SERIALIZES them on
		// the head of the table, which is worse than no lock.
		{"the multi-replica lock", "for update skip locked", "a plain select hands the same rows to every replica and every one of them publishes"},
		// The bound, or a claim would take the whole table and hold it.
		{"the bound", "limit $1", "the batch size is what bounds how long the row locks are held"},
	}
	for _, tc := range cases {
		if !strings.Contains(query, tc.want) {
			t.Errorf("the claim query does not have %s (%q): %s", tc.property, tc.want, tc.why)
		}
	}
}

// The claim transaction is the identity-free one, and this is where that is
// proved rather than asserted in a comment.
//
// `darkroomdb.BeginTx` is the only transaction starter in this repository that
// does not call `cafaye.begin_account`, and it is the one [Store.Claim] uses. If
// the claim ever moved to `InAccountTx` it would put an identity on a
// transaction that reads a table no policy guards — a claim that the rows belong
// to an account, which nothing in the database agrees with, and which would make
// the publisher read one tenant's events if the table ever grew a policy.
func TestTheClaimUsesTheIdentityFreeTransaction(t *testing.T) {
	source, err := os.ReadFile("store.go")
	if err != nil {
		t.Fatalf("reading store.go: %v — this guard checks the SOURCE, so it cannot run from elsewhere", err)
	}
	text := string(source)

	// Read the body of Claim rather than the whole file: the count below is the
	// number of transaction starters INSIDE Claim, and an occurrence elsewhere in
	// the file would be a different function's.
	start := strings.Index(text, "func (s *Store) Claim(")
	if start < 0 {
		t.Fatal("store.go has no Claim: the publisher has no read path")
	}
	body := text[start:]
	if end := strings.Index(body, "\nfunc "); end > 0 {
		body = body[:end]
	}

	if !strings.Contains(body, "s.db.BeginTx(") {
		t.Error("Claim does not use darkroomdb.BeginTx: the claim transaction must be the identity-free one")
	}
	for _, forbidden := range []string{"InAccountTx", "begin_account", "current_account_id"} {
		if strings.Contains(body, forbidden) {
			t.Errorf("Claim calls %s: an identity on a transaction reading a table no policy guards is a claim no column in the database agrees with", forbidden)
		}
	}
}
