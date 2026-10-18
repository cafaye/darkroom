// The direction a listing walks.
//
// # WHY A TYPE RATHER THAN A STRING, AND WHY THE WIRE REFUSES
//
// `openapi/v1.yaml` declares `order` with `enum: [asc, desc]` and `default: desc`,
// and `internal/api`'s generated binder asks for exactly that and nothing more:
// `runtime.BindQueryParameterWithOptions(…, Type: "string")`. It binds whatever
// the query string holds and NEVER checks the enum — `api.ListAssetsParamsOrder`
// has a `Valid()` method that nothing calls. So an unknown `order` reaches this
// package as a plain string and something here has to refuse it, which is why the
// first version of this endpoint ignored the parameter entirely rather than
// half-reading it.
//
// REJECTED, NOT CLAMPED, and the difference from `clampPageLimit` is the whole of
// the policy. A `limit` is "a promise to the server about how much work it will
// do", so a request over the cap is a smaller page and nothing else. `order` is a
// VALUE THE DOCUMENT ENUMERATES: there is no third direction to fall back to, no
// "nearest" answer, and a client that sends `order=oldest` has made a mistake
// rather than an oversized request. Answering newest-first to a client that asked
// for something else is the silent-wrong-answer defect this packet exists to
// remove, so `order=oldest` is a 422 naming `order`.
//
// # WHY `OrderDesc` IS THE ZERO VALUE
//
// `ListQuery{}` must mean the document's default, so `desc` — the default the
// document declares and the behavior every existing caller gets — is `iota`'s
// zero. A `ListQuery` built by a caller who never heard of this field is
// therefore newest-first, which is what omitting `order` does.
package service

import (
	"github.com/cafaye/darkroom/internal/problem"
)

// SortOrder is the direction a page is walked in.
//
// An `int` and not a string, so the value that reaches SQL is one this package
// chose. [SortOrder.comparison] and [SortOrder.direction] are the only two ways a
// SortOrder becomes text, and neither copies anything out of a request — which is
// what makes "a query parameter cannot reach the query string" structural rather
// than a review promise.
type SortOrder int

const (
	// OrderDesc is newest first, and the zero value: `openapi/v1.yaml` says
	// `default: desc` and every caller that predates this packet expects it.
	OrderDesc SortOrder = iota
	// OrderAsc is oldest first.
	OrderAsc
)

// String is the document's spelling of the direction.
//
// The zero value's name, `desc`, which is also what a value this package did not
// mint is treated as everywhere below. A forged `SortOrder(7)` is not a request:
// the only constructor for a wire value is [ParseSortOrder], which admits two.
func (o SortOrder) String() string {
	if o == OrderAsc {
		return "asc"
	}
	return "desc"
}

// Valid reports whether o is one of the two the document enumerates.
//
// [SortOrder.comparison] and [SortOrder.direction] do NOT consult it, and that is
// deliberate: a predicate a caller can forget is not a guarantee, so the two
// fragments answer descending for anything that is not ascending. Valid is for the
// one place that must be able to say "no" — [DecodeCursor], which refuses a token
// whose direction segment is neither word.
func (o SortOrder) Valid() bool { return o == OrderAsc || o == OrderDesc }

// ParseSortOrder turns the document's enum into a [SortOrder], or refuses it.
//
// 422 naming `order`, for the reason above: the parameter is present and its value
// is not one of the two the document enumerates. The empty string is refused too,
// and that is the one case worth arguing about: `?order=` is not a client asking
// for the default, it is a client that built a query string from a variable it did
// not have. Reading it as `desc` is the silent-wrong-answer defect again, and a
// caller that omits the parameter entirely gets `desc` anyway.
func ParseSortOrder(raw string) (SortOrder, *problem.Error) {
	switch raw {
	case "asc":
		return OrderAsc, nil
	case "desc":
		return OrderDesc, nil
	}
	return OrderDesc, problem.InvalidFields(
		"order must be asc or desc",
		problem.NewField("order", "invalid_value"),
	)
}

// comparison is the row-value operator a cursor's position is tested with.
//
// `<` FOR A DESCENDING SCAN AND `>` FOR AN ASCENDING ONE, and the two must move
// together or the walk is quietly wrong: a descending cursor fed into an ascending
// scan skips every row below the cursor's position and repeats the ones above it,
// with a 200 and a plausible-looking page. [Cursor.AgreesWith] is what stops that
// from being reachable, and this is the operator it would be protecting.
//
// NOT INTERPOLATED FROM ANYTHING. A SortOrder is two values, this returns one of
// two strings, and there is no path from a query parameter to this text.
func (o SortOrder) comparison() string {
	if o == OrderAsc {
		return ">"
	}
	return "<"
}

// direction is the `order by` suffix, and it comes from the SAME value as
// [SortOrder.comparison] so the predicate and the sort cannot disagree.
//
// `created_at asc, id asc` over `assets_account_created_idx` — which is
// `(account_id, created_at desc, id desc)` — is a BACKWARD scan of that index. It
// is the same range scan the descending direction gets and no sort, because a
// btree index is walked in either direction at the same cost. No second index, and
// no migration.
func (o SortOrder) direction() string {
	if o == OrderAsc {
		return "asc"
	}
	return "desc"
}
