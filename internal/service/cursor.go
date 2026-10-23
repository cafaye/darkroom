// Cursor pagination: an opaque token over `(created_at, id)` and the direction it
// was minted under.
//
// core: "`limit` defaults to 25 and is capped at 100. […] clients must not depend on
// the internal structure of the cursor." Both halves are decisions, and the second is
// why this is base64 rather than two query parameters: a client that can read its own
// cursor learns the sort key and the id space, and will start building a cursor by
// hand.
//
// # WHY BOTH COLUMNS
//
// The page is ordered by `(created_at, id)` in one direction or the other, and a
// cursor is a row-value comparison on that same pair. Ordering by `created_at` alone
// would drop or repeat every row that shares a timestamp — and `created_at` defaults
// to `now()`, so two rows inserted in one transaction routinely share it. `id` is the
// tiebreak, so the comparison is lexicographic on the pair and no row can fall between
// two pages.
//
// # WHY THE DIRECTION IS IN THE TOKEN TOO
//
// Because the comparison is only correct for the direction it was minted under: a
// descending token into an ascending scan skips rows below it and repeats rows above
// it, with a 200 and a plausible-looking page. See [Cursor.AgreesWith].
//
// The token's SHAPE therefore carries three segments where it used to carry two. That
// is a client-visible change in the strict sense and a compatible one in practice: the
// document says clients must not parse the cursor, nothing has ever been served from
// this implementation, and a shim that accepted the two-segment form would accept a
// token that cannot say which way the page it points at was being walked — which is
// the one thing the third segment is for.
//
// # WHY base64url AND NOT base64
//
// `+` and `/` in a query string are a percent-encoding bug in every client that has
// not heard of them. The URL-safe, unpadded alphabet removes the problem instead of
// documenting it, and it is what core specifies.
package service

import (
	"encoding/base64"
	"strconv"
	"strings"
	"time"

	"github.com/anywaye/darkroom/internal/problem"
)

// The page-size rules from core/docs/openapi-conventions.md.
//
// `DefaultPageLimit` and `MaxPageLimit` are separate names for one policy rather
// than one constant used twice, because "25" and "100" appearing as literals in the
// same file is how a cap and a default drift into being the same number.
const (
	DefaultPageLimit = 25
	MaxPageLimit     = 100
)

// clampPageLimit is the whole of the limit policy.
//
// CLAMPED, NOT REJECTED. "A larger request is clamped, not rejected — the cap is a
// promise to the server about how much work it will do, not a validation rule for the
// client to satisfy." A `limit=1000` is a client asking for more than the server
// offers; answering 422 would make every well-meaning client handle an error to get
// the smaller page it would have got anyway.
func clampPageLimit(limit int) int {
	if limit <= 0 {
		return DefaultPageLimit
	}
	return min(limit, MaxPageLimit)
}

// Cursor is one position in the `(created_at, id)` ordering, in one direction.
//
// A struct and not two arguments, because the pair is meaningless apart: passing a
// time without the id it broke ties with is a bug the type system can prevent. `Order`
// is in the struct for the same reason — the position is only meaningful relative to
// the direction it was found in, so a cursor that did not carry one would be a
// half-position.
type Cursor struct {
	CreatedAt time.Time
	ID        string
	// Order is the direction the page this position came from was walked in. The
	// zero value is [OrderDesc], so a Cursor built by a caller that predates this
	// field continues to mean newest-first.
	Order SortOrder
}

// AgreesWith reports whether this cursor may be used for a scan in direction o, and
// refuses the pair that would silently corrupt the walk.
//
// THIS IS THE RULE, and it is a refusal rather than an adjustment because there is no
// honest adjustment: a descending position has no meaning in an ascending scan, and
// answering with *some* page — the first page, the page from the epoch, a page
// assembled by inverting the predicate — is a different answer each time and none of
// them is the one that was asked for. A client that switches `order` mid-walk, which
// is exactly what a UI does when the user clicks the sort column, gets a 422 naming
// `cursor` and restarts the walk instead of a corrupted one.
//
// 422 and not 400, and the same 422 [DecodeCursor] answers, because both are "this
// token is not a cursor for this request": syntactically fine, semantically wrong.
// And the message names BOTH directions, because a client holding a cursor does not
// know which way its own walk was going and "cursor mismatch" alone would send it
// looking for a bug in its token encoder.
func (c *Cursor) AgreesWith(o SortOrder) *problem.Error {
	if c == nil {
		return nil
	}
	if c.Order == o {
		return nil
	}
	return problem.InvalidFields(
		"cursor was minted for order="+c.Order.String()+" and cannot be used with order="+o.String(),
		problem.NewField("cursor", "invalid_value"),
	)
}

// cursorSeparator is between the segments of the encoded token. `|` cannot appear in
// a base64url ALPHABET, which is why the encoding is safe to split on it and why it
// is not merely a comma.
const cursorSeparator = "|"

// cursorSegments is how many halves a token has, and the reason [DecodeCursor] can
// tell a three-segment token from the two-segment one it used to write.
//
// A CONSTANT rather than three `strings.Cut` calls, so adding a field to the token is
// one edit here and the count of edits cannot drift from the count of segments.
const cursorSegments = 3

// EncodeCursor renders a position as the token a client passes back.
//
// It takes the [Cursor] and not the three fields, because a token that could be
// encoded from a position whose direction the caller had to remember separately is a
// token that can be minted without a direction — the bug [Cursor.AgreesWith] then has
// to catch, made unrepresentable here instead.
func EncodeCursor(c Cursor) string {
	raw := strconv.FormatInt(c.CreatedAt.UnixNano(), 10) + cursorSeparator +
		c.ID + cursorSeparator + c.Order.String()
	return base64.RawURLEncoding.EncodeToString([]byte(raw))
}

// DecodeCursor parses a token, and refuses everything it cannot parse as a 422
// naming `cursor`.
//
// 422 and not 400 because that is what the Rust tree answers and a change would be a
// client-visible one: a cursor is part of the request, and this service's contract for
// "well-formed but wrong" is 422. Every failure is the SAME error, so a client cannot
// tell a truncated token from a forged one — an oracle is not built from parse errors.
func DecodeCursor(raw string) (*Cursor, *problem.Error) {
	badCursor := func() (*Cursor, *problem.Error) {
		return nil, problem.InvalidFields(
			"cursor is not a valid pagination cursor",
			problem.NewField("cursor", "invalid_format"),
		)
	}

	decoded, err := base64.RawURLEncoding.DecodeString(raw)
	if err != nil {
		return badCursor()
	}
	parts := strings.Split(string(decoded), cursorSeparator)
	// EXACTLY THREE SEGMENTS, and the two-segment form this function used to accept
	// is now one of the refusals. A two-segment token cannot say which way its page
	// was walked, and accepting it would be the compatibility shim the packet is
	// explicit is not wanted: there is no deployed instance to be compatible with.
	if len(parts) != cursorSegments {
		return badCursor()
	}
	nanos, ok := parseInt64(parts[0])
	if !ok {
		return badCursor()
	}
	id := parts[1]
	// THE ID MUST BE A UUID, and this check is not decoration. The cursor's second
	// half is bound as `$2::uuid`, so an unchecked string reaches Postgres as an
	// invalid input syntax for type uuid — a 500 for a malformed request, and the one
	// way a client could turn a 422 into a server error. It was found by a test that
	// passed `MTIzfGV4dA` (which is base64 for "123|ext"): the separator was there and
	// both halves were non-empty, so the first draft of this accepted it.
	if !isUUID(id) {
		return badCursor()
	}
	// AND THE DIRECTION MUST BE ONE OF THE TWO WORDS, decoded through the one parser
	// [ParseSortOrder] is. An unknown word is a 422 naming `cursor` rather than a
	// silent "desc": a token whose third segment says `oldest` is not a cursor this
	// service minted, and guessing which scan it was for is the failure
	// [Cursor.AgreesWith] exists to prevent.
	order, pErr := ParseSortOrder(parts[2])
	if pErr != nil {
		return badCursor()
	}
	return &Cursor{CreatedAt: time.Unix(0, nanos).UTC(), ID: id, Order: order}, nil
}

// isUUID reports whether s is a canonical 8-4-4-4-12 hex UUID.
//
// The SHAPE and not a round trip through a uuid library: the cursor is opaque to the
// client and this is the only place that ever parses one, so the check is for "would
// `$::uuid` accept this" — which is exactly the 8-4-4-4-12 hex form, case-insensitive
// and with an optional version nibble, because Postgres accepts all of those.
func isUUID(s string) bool {
	if len(s) != 36 {
		return false
	}
	for i := 0; i < 36; i++ {
		if i == 8 || i == 13 || i == 18 || i == 23 {
			if s[i] != '-' {
				return false
			}
			continue
		}
		c := s[i]
		switch {
		case c >= '0' && c <= '9', c >= 'a' && c <= 'f', c >= 'A' && c <= 'F':
		default:
			return false
		}
	}
	return true
}

// parseInt64 parses a base-10 int64, reporting whether it was one at all.
//
// A (value, ok) pair rather than a sentinel: "0" is a legal timestamp and a table
// whose `created_at` defaults to `now()` has no row at the epoch, so a
// failed-parse-as-zero would turn into a cursor that silently starts a page from the
// beginning of time. Refusing is the honest answer.
func parseInt64(s string) (int64, bool) {
	n, err := strconv.ParseInt(s, 10, 64)
	if err != nil {
		return 0, false
	}
	return n, true
}
