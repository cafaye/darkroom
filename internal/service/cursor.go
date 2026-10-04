// Cursor pagination: an opaque token over `(created_at, id)`.
//
// core: "`limit` defaults to 25 and is capped at 100. […] clients must not depend on
// the internal structure of the cursor." Both halves are decisions, and the second is
// why this is base64 rather than two query parameters: a client that can read its own
// cursor learns the sort key and the id space, and will start building a cursor by
// hand.
//
// # WHY BOTH COLUMNS
//
// The page is ordered by `(created_at desc, id desc)` and a cursor is a row-value
// comparison on that same pair. Ordering by `created_at` alone would drop or repeat
// every row that shares a timestamp — and `created_at` defaults to `now()`, so two
// rows inserted in one transaction routinely share it. `id` is the tiebreak, so the
// comparison is lexicographic on the pair and no row can fall between two pages.
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

	"github.com/cafaye/darkroom/internal/problem"
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

// Cursor is one position in the `(created_at, id)` ordering.
//
// A struct and not two arguments, because the pair is meaningless apart: passing a
// time without the id it broke ties with is a bug the type system can prevent.
type Cursor struct {
	CreatedAt time.Time
	ID        string
}

// cursorSeparator is between the two halves of the encoded token. `|` cannot appear in
// a base64url ALPHABET, which is why the encoding is safe to split on it and why it
// is not merely a comma.
const cursorSeparator = "|"

// EncodeCursor renders a position as the token a client passes back.
func EncodeCursor(at time.Time, id string) string {
	raw := strconv.FormatInt(at.UnixNano(), 10) + cursorSeparator + id
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
	text := string(decoded)
	rawNanos, id, ok := strings.Cut(text, cursorSeparator)
	if !ok {
		return badCursor()
	}
	nanos, ok := parseInt64(rawNanos)
	if !ok {
		return badCursor()
	}
	// THE ID MUST BE A UUID, and this check is not decoration. The cursor's second
	// half is bound as `$2::uuid`, so an unchecked string reaches Postgres as an
	// invalid input syntax for type uuid — a 500 for a malformed request, and the one
	// way a client could turn a 422 into a server error. It was found by a test that
	// passed `MTIzfGV4dA` (which is base64 for "123|ext"): the separator was there and
	// both halves were non-empty, so the first draft of this accepted it.
	if !isUUID(id) {
		return badCursor()
	}
	return &Cursor{CreatedAt: time.Unix(0, nanos).UTC(), ID: id}, nil
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
