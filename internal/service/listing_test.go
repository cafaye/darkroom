package service

// The ORDER, against a real PostgreSQL, in the direction the document enumerates.
//
// These run in `newHarness`'s private schema and skip with a named reason when
// there is no database — see `service_test.go`'s header for why that is honest.
//
// # WHY THE EXPECTED ORDER IS BUILT FROM THE SEED AND NOT FROM A RESPONSE
//
// Every assertion here compares against the order the assets were CREATED in, which
// the test knows and the database is asked about later. "The ascending page is the
// reverse of the descending page" would pass against an implementation that answered
// both requests with the same wrongly-ordered page and reversed one of them in Go,
// which is the class of thing this packet exists to fix.

import (
	"context"
	"go/ast"
	"go/parser"
	"go/token"
	"regexp"
	"strconv"
	"testing"

	"github.com/cafaye/darkroom/internal/checksum"
)

// seedAssets creates n distinct assets for one caller and returns their ids in
// CREATION order, which is oldest-first.
//
// Distinct bytes per asset, so `UNIQUE (account_id, checksum)` does not collapse
// them into duplicates — a page test over duplicates measures the wrong thing. The
// create/complete pair is run directly rather than through `harness.upload` because
// these tests do not care about the state machine and a failed upload would leave a
// `pending` row in the middle of an ordering assertion.
func seedAssets(t *testing.T, h *harness, caller Caller, n int) []string {
	t.Helper()
	ctx := context.Background()
	ids := make([]string, 0, n)
	for i := range n {
		body := append(pngBytes, byte('a'+i))
		sum := checksum.SHA256Hex(body)
		created, pErr := h.svc.CreateUpload(ctx, caller, CreateUpload{
			Filename: "photo.png", ContentType: "image/png",
			ByteSize: int64(len(body)), Checksum: sum,
		})
		if pErr != nil {
			t.Fatalf("creating asset %d: %v", i, pErr)
		}
		if err := h.objects.Put(ctx, created.Asset.StorageKey(), body, "image/png"); err != nil {
			t.Fatal(err)
		}
		if _, pErr := h.svc.CompleteUpload(ctx, caller, created.Asset.ID, sum); pErr != nil {
			t.Fatalf("completing asset %d: %v", i, pErr)
		}
		ids = append(ids, created.Asset.ID)
	}
	return ids
}

// pageIDs is the order a page arrived in, which is the only thing an ordering
// assertion is about.
func pageIDs(page *AssetPage) []string {
	ids := make([]string, 0, len(page.Assets))
	for _, a := range page.Assets {
		ids = append(ids, a.ID)
	}
	return ids
}

func reversed(ids []string) []string {
	out := make([]string, len(ids))
	for i, id := range ids {
		out[len(ids)-1-i] = id
	}
	return out
}

func assertOrder(t *testing.T, got, want []string, what string) {
	t.Helper()
	if len(got) != len(want) {
		t.Fatalf("%s holds %d assets, want %d", what, len(got), len(want))
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("%s: position %d is %s, want %s\n got %v\nwant %v", what, i, got[i], want[i], got, want)
		}
	}
}

// TestBothDirectionsReturnTheSamePageInOppositeOrder is done-means 3, and
// done-means 1 and 2 beside it: the request that names no order and the one that
// names `desc` are the same page, and `asc` is that page walked the other way.
func TestBothDirectionsReturnTheSamePageInOppositeOrder(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()
	const total = 7
	created := seedAssets(t, h, h.alice, total) // oldest-first

	newestFirst := reversed(created)
	pageOf := func(q ListQuery) *AssetPage {
		t.Helper()
		q.Limit = 3
		page, pErr := h.svc.ListAssets(ctx, h.alice, q)
		if pErr != nil {
			t.Fatalf("ListAssets(%+v): %v", q, pErr)
		}
		return page
	}

	// OMITTING `order` — `ListQuery{}`, exactly what the handler builds for a request
	// with no `order` — and naming `desc` are the same answer, which is done-means 1
	// and 2 and is the "behaves exactly as today" half of this packet.
	omitted := pageOf(ListQuery{})
	explicit := pageOf(ListQuery{Order: OrderDesc})
	assertOrder(t, pageIDs(omitted), newestFirst[:3], "the default page")
	assertOrder(t, pageIDs(explicit), newestFirst[:3], "order=desc")
	assertOrder(t, pageIDs(omitted), pageIDs(explicit), "the default page against order=desc")

	// AND `asc` IS THE SAME PAGE IN THE OPPOSITE ORDER — the same three assets, which
	// is what makes this a direction and not a different filter.
	ascending := pageOf(ListQuery{Order: OrderAsc})
	assertOrder(t, pageIDs(ascending), created[:3], "order=asc")
	if pageIDs(ascending)[0] != created[0] {
		t.Errorf("order=asc started at %s, want the oldest asset %s",
			pageIDs(ascending)[0], created[0])
	}
}

// TestPaginationWalksEveryRowExactlyOnceInEitherDirection is done-means 4, and the
// property a naive implementation gets wrong: seven assets over a page of three is
// three pages, and every one of them is visited ONCE — none skipped, none repeated —
// in both directions.
//
// THE PLANTED DIVERGENCE THIS TEST CATCHES: an implementation that sorts ascending
// and keeps the descending predicate (`(created_at, id) < ($1, $2)`). Page one is
// right, page two starts from the wrong end of the ordering and repeats what page
// one already returned, and the walk either loops or ends with rows missing. It was
// planted here to confirm the test goes red, and the cursor carrying its direction is
// what stops a client from reaching that state.
func TestPaginationWalksEveryRowExactlyOnceInEitherDirection(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()
	const total = 7
	const limit = 3
	created := seedAssets(t, h, h.alice, total)

	for _, c := range []struct {
		order SortOrder
		want  []string
	}{
		{OrderDesc, reversed(created)},
		{OrderAsc, created},
	} {
		seen := map[string]int{}
		walked := []string{}
		pages := 0
		var cursor *Cursor
		for {
			page, pErr := h.svc.ListAssets(ctx, h.alice, ListQuery{
				Limit: limit, Cursor: cursor, Order: c.order,
			})
			if pErr != nil {
				t.Fatalf("order=%s page %d: %v", c.order, pages, pErr)
			}
			pages++
			if len(page.Assets) > limit {
				t.Fatalf("order=%s page %d returned %d rows for a limit of %d",
					c.order, pages, len(page.Assets), limit)
			}
			for _, id := range pageIDs(page) {
				seen[id]++
				walked = append(walked, id)
			}
			if !page.HasMore {
				break
			}
			if page.Next == nil || page.NextToken == "" {
				t.Fatalf("order=%s page %d claims more and carries no cursor", c.order, pages)
			}
			// THE NEXT CURSOR CARRIES THE DIRECTION THIS PAGE WAS WALKED IN, and the
			// token the service hands the client decodes back to it. A cursor that lost
			// it here would be refused on the next call, which is the other half of
			// done-means 4: the walk works because the token says which way it goes.
			if page.Next.Order != c.order {
				t.Fatalf("order=%s page %d minted a %s cursor", c.order, pages, page.Next.Order)
			}
			decoded, pErr := DecodeCursor(page.NextToken)
			if pErr != nil {
				t.Fatalf("order=%s: the cursor does not decode: %v", c.order, pErr)
			}
			if decoded.Order != c.order {
				t.Fatalf("order=%s: the token decodes to order=%s", c.order, decoded.Order)
			}
			cursor = decoded
			if pages > total {
				t.Fatalf("order=%s: pagination did not terminate", c.order)
			}
		}

		if pages < 2 {
			t.Errorf("order=%s walked %d of %d assets in %d page(s); the set is larger than one page", c.order, len(walked), total, pages)
		}
		if len(seen) != total {
			t.Errorf("order=%s visited %d distinct assets, want %d", c.order, len(seen), total)
		}
		for id, times := range seen {
			if times != 1 {
				t.Errorf("order=%s visited %s %d times, want once", c.order, id, times)
			}
		}
		// AND THE WALK IS IN THE REQUESTED DIRECTION, not merely complete: an
		// implementation that ignored `order` entirely would satisfy every line above.
		assertOrder(t, walked, c.want, "the order="+c.order.String()+" walk")
	}
}

// TestACursorFromTheOtherDirectionIsRefused is done-means 5 against the database: the
// refusal is a 422 naming `cursor`, in both directions, and it returns no rows at
// all rather than a plausible page.
//
// The second half matters more than the status code. A refusal that still rendered
// a page would let a client that ignores the error carry on with a walk that skips
// and repeats rows, which is the defect the token's third segment exists to close.
func TestACursorFromTheOtherDirectionIsRefused(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()
	const total = 5
	seedAssets(t, h, h.alice, total)

	first, pErr := h.svc.ListAssets(ctx, h.alice, ListQuery{Limit: 2, Order: OrderDesc})
	if pErr != nil {
		t.Fatalf("the descending first page: %v", pErr)
	}
	if first.Next == nil {
		t.Fatal("a page holding two of five assets carries no cursor")
	}

	for _, c := range []struct {
		minted, asked SortOrder
	}{
		{OrderDesc, OrderAsc},
		{OrderAsc, OrderDesc},
	} {
		token := EncodeCursor(Cursor{
			CreatedAt: first.Next.CreatedAt, ID: first.Next.ID, Order: c.minted,
		})
		cursor, pErr := DecodeCursor(token)
		if pErr != nil {
			t.Fatalf("the cursor just minted does not decode: %v", pErr)
		}
		page, pErr := h.svc.ListAssets(ctx, h.alice, ListQuery{
			Limit: 2, Cursor: cursor, Order: c.asked,
		})
		h.assert(pErr).status(422, "a "+c.minted.String()+" cursor presented as "+c.asked.String()).
			code("validation_failed", "the refusal's code").
			field("cursor", "the refusal names the cursor")
		if page != nil {
			t.Errorf("a refused request returned %d assets; a refusal renders nothing", len(page.Assets))
		}

		// AND THE SAME CURSOR IN ITS OWN DIRECTION STILL WORKS, so the refusal is the
		// pairing and not a cursor that stopped being readable.
		if _, pErr := h.svc.ListAssets(ctx, h.alice, ListQuery{
			Limit: 2, Cursor: cursor, Order: c.minted,
		}); pErr != nil {
			t.Errorf("a %s cursor used for a %s walk was refused: %v", c.minted, c.minted, pErr)
		}
	}
}

// tenantPredicate is the shape of a query that names the tenant itself: the column,
// a comparison, and a BOUND PARAMETER.
//
// A placeholder on the right-hand side is what makes this narrow enough to be
// trustworthy. `insertAsset`'s SQL contains `account_id` too — it writes
// `(select cafaye.current_account_id())`, reading the identity off the transaction —
// and a guard that flagged every `account_id` would be red on the one insert that
// deliberately does not take one as a parameter.
var tenantPredicate = regexp.MustCompile(`account_id\s*(?:=|<>|!=|like|in)\s*\(?\s*\$\d`)

// isATenantPredicate reports whether a string literal is a query naming the tenant.
func isATenantPredicate(literal string) bool { return tenantPredicate.MatchString(literal) }

// TestATenantPredicateIsRecognised is the guard's own table, because a guard that has
// never been shown red is a guard nobody can rely on (`AGENTS.md`).
//
// The first draft of the check compared each literal to ONE planted string, so it was
// green on every predicate except that exact sentence — which is a guard that proves
// nothing at all. This one recognises the SHAPE, and the row that matters is the
// planted one: `where account_id = $1 and id = $2`, added to `listAssets` and caught
// by [TestTheListQueryNeverNamesTheTenantItself].
func TestATenantPredicateIsRecognised(t *testing.T) {
	for _, c := range []struct {
		literal string
		want    bool
	}{
		{`where account_id = $1 and id = $2`, true}, // PLANTED: the defect, verbatim
		{`where account_id=$1`, true},
		{`where account_id  =  $2 and status = 'pending'`, true},
		{`where account_id <> $1`, true},
		{`where account_id in ($1, $2)`, true},
		{`where account_id like $1`, true},
		// `account_id` used the two ways this package legitimately does, which a
		// guard written as "any literal mentioning account_id" would have flagged.
		{`insert into assets (id, account_id, …) values ($1, (select cafaye.current_account_id()), …)`, false},
		{`select ` + assetColumns + ` from assets`, false},
		{`where (created_at, id) > ($1, $2::uuid)`, false},
		{`where id = $1`, false},
		// And the COMMENT that explains the rule, which is what the first draft matched:
		// `AGENTS.md` writes the placeholder as `$N`, and the digits are what keeps a
		// sentence about the rule from looking like the defect.
		{`account_id = $N`, false},
	} {
		if got := isATenantPredicate(c.literal); got != c.want {
			t.Errorf("isATenantPredicate(%q) = %v, want %v", c.literal, got, c.want)
		}
	}
}

// TestTheListQueryNeverNamesTheTenantItself is done-means 7, structurally: this
// package's queries are scoped by the RLS policies `darkroomdb.InAccountTx` puts in
// place, and a hand-written `account_id = $N` would be a second source of truth for
// the tenant — the exact thing `AGENTS.md` calls "being WRONG in a way RLS is not".
//
// # WHY IT PARSES THE SOURCE INSTEAD OF MATCHING TEXT
//
// The first draft counted occurrences of `account_id = $` in the file, and it was red
// on a correct tree: both `store.go` and `service.go` carry the phrase inside the
// comments that EXPLAIN the rule — "WHY THERE IS NO `and account_id = $N` IN ANY OF
// IT". A guard that fires on the documentation of the rule cannot go green without
// deleting the explanation, which is the AGENTS.md failure exactly: "a set
// comparison that asserted an accident of file layout".
//
// So it reads STRING LITERALS through `go/parser`, which is the only place a
// predicate can be: a value that reaches Postgres came from a literal here, so a
// literal that names the tenant is the thing to look for, and a comment is not one.
// Both planted divergences were run against it — a `where account_id = $1` appended
// to `listAssets`'s SQL, and an ascending sort keeping the descending predicate — and
// the first caught this one, the second caught
// [TestPaginationWalksEveryRowExactlyOnceInEitherDirection].
//
// It is a second copy of the rule and not a replacement for the behavioural half:
// `pgtest`'s policy guards read the live policies' `pg_get_expr`, and
// `TestListAssetsSeesOnlyTheCallersAccount` holds the behaviour against real
// Postgres. This one only says the spelling has not crept back in.
func TestTheListQueryNeverNamesTheTenantItself(t *testing.T) {
	fset := token.NewFileSet()
	for _, name := range []string{"store.go", "service.go", "order.go"} {
		file, err := parser.ParseFile(fset, name, nil, 0)
		if err != nil {
			t.Fatalf("parsing %s: %v", name, err)
		}
		ast.Inspect(file, func(n ast.Node) bool {
			lit, ok := n.(*ast.BasicLit)
			if !ok || lit.Kind != token.STRING {
				return true
			}
			text, err := strconv.Unquote(lit.Value)
			if err != nil {
				t.Errorf("%s: unquoting %s: %v", name, lit.Value, err)
				return true
			}
			if isATenantPredicate(text) {
				t.Errorf("%s:%d names the tenant in a query: %s\n  the tenant is the POLICY's job; a query cannot forget what a policy already does",
					name, fset.Position(lit.Pos()).Line, text)
			}
			return true
		})
	}
}
