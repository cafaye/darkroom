package service

// The ORDER POLICY, asserted rather than trusted: which values exist, which are
// refused, and what the zero value of a query means.

import (
	"strings"
	"testing"

	"github.com/cafaye/darkroom/internal/problem"
)

// TestTheDocumentedOrdersAreTheOnlyTwo is the document's `enum: [asc, desc]`,
// read out of the parser rather than believed of it.
func TestTheDocumentedOrdersAreTheOnlyTwo(t *testing.T) {
	for raw, want := range map[string]SortOrder{"asc": OrderAsc, "desc": OrderDesc} {
		got, pErr := ParseSortOrder(raw)
		if pErr != nil {
			t.Errorf("ParseSortOrder(%q) was refused: %v", raw, pErr)
			continue
		}
		if got != want {
			t.Errorf("ParseSortOrder(%q) = %v, want %v", raw, got, want)
		}
	}
}

// TestAnOrderTheDocumentDoesNotEnumerateIs422NamingTheField is the difference from
// `clampPageLimit`, stated as a table: `limit` is a promise about work and is
// clamped, `order` is a value the document enumerates and can only be got wrong.
func TestAnOrderTheDocumentDoesNotEnumerateIs422NamingTheField(t *testing.T) {
	for _, raw := range []string{
		"",                        // `?order=` is a client with no value, not a request for the default
		"ASC",                     // the document enumerates lowercase words
		"Desc",                    //
		"asc ",                    // a trailing space is a different string, not a tolerant read
		"ascending",               // the friendly spelling is not in the enum
		"oldest",                  // what a human means by asc, and what nothing here guesses
		"newest",                  //
		"d e s c",                 //
		"desc; drop table assets", // and the injection attempt, refused like any other
	} {
		_, pErr := ParseSortOrder(raw)
		if pErr == nil {
			t.Errorf("ParseSortOrder(%q) was accepted", raw)
			continue
		}
		if pErr.Status() != 422 || pErr.Code() != problem.CodeValidationFailed {
			t.Errorf("ParseSortOrder(%q) is %d/%s, want 422/validation_failed", raw, pErr.Status(), pErr.Code())
		}
		found := false
		for _, f := range pErr.Fields() {
			if f.Field == "order" {
				found = true
			}
		}
		if !found {
			t.Errorf("ParseSortOrder(%q) does not name the order field: %v", raw, pErr.Fields())
		}
	}
}

// TestAQueryThatNamesNoOrderIsNewestFirst is done-means 1 at the type level: the
// zero value has to BE the document's `default: desc`, because the handler builds
// `service.ListQuery{}` for a request with no `order` and every caller that predates
// this field expects newest-first.
func TestAQueryThatNamesNoOrderIsNewestFirst(t *testing.T) {
	var zero SortOrder
	if zero != OrderDesc {
		t.Errorf("the zero SortOrder is %v, want %v (the document's default)", zero, OrderDesc)
	}
	if (ListQuery{}).Order != OrderDesc {
		t.Error("ListQuery{} is not newest-first")
	}
	if !OrderDesc.Valid() || !OrderAsc.Valid() {
		t.Error("the two documented orders are not both valid")
	}
}

// TestTheTwoDirectionsAreOppositeAndNeverAnUnvettedString is the SQL half with no
// database: the comparison operator and the sort direction come from ONE value, they
// move together, and neither copies anything out of a request.
//
// A FORGED SortOrder is in the table on purpose. Nothing on the wire can produce one —
// [ParseSortOrder] admits two words — and a value that cannot be produced must not be
// able to put arbitrary text into a query either. It walks descending, which is the
// behavior before this packet existed.
func TestTheTwoDirectionsAreOppositeAndNeverAnUnvettedString(t *testing.T) {
	cases := []struct {
		order     SortOrder
		compare   string
		direction string
	}{
		{OrderDesc, "<", "desc"},
		{OrderAsc, ">", "asc"},
		{SortOrder(7), "<", "desc"},
		{SortOrder(-1), "<", "desc"},
	}
	for _, c := range cases {
		if got := c.order.comparison(); got != c.compare {
			t.Errorf("%v.comparison() = %q, want %q", c.order, got, c.compare)
		}
		if got := c.order.direction(); got != c.direction {
			t.Errorf("%v.direction() = %q, want %q", c.order, got, c.direction)
		}
		// Whatever it is, it is one of the two pairs above and nothing else — the
		// property that says a query parameter cannot reach the query string.
		if c.order.comparison() != "<" && c.order.comparison() != ">" {
			t.Errorf("%v.comparison() = %q, which is not an operator", c.order, c.order.comparison())
		}
	}
	if OrderAsc.comparison() == OrderDesc.comparison() {
		t.Error("both directions compare the same way, so one of the walks skips rows")
	}
}

// TestACursorRefusesTheDirectionItWasNotMintedFor is done-means 5 without a database:
// the cursor carries the direction and the pair that cannot be walked is refused.
func TestACursorRefusesTheDirectionItWasNotMintedFor(t *testing.T) {
	var absent *Cursor
	if pErr := absent.AgreesWith(OrderAsc); pErr != nil {
		t.Errorf("a request with no cursor was refused: %v", pErr)
	}

	for _, c := range []struct {
		minted, asked SortOrder
		wantRefusal   bool
	}{
		{OrderDesc, OrderDesc, false},
		{OrderAsc, OrderAsc, false},
		{OrderDesc, OrderAsc, true},
		{OrderAsc, OrderDesc, true},
	} {
		cursor := &Cursor{CreatedAt: now(), ID: NewUUID(), Order: c.minted}
		pErr := cursor.AgreesWith(c.asked)
		if c.wantRefusal {
			if pErr == nil {
				t.Errorf("a cursor minted for %s was accepted for a %s walk", c.minted, c.asked)
				continue
			}
			if pErr.Status() != 422 || pErr.Code() != problem.CodeValidationFailed {
				t.Errorf("%s into %s is %d/%s, want 422/validation_failed", c.minted, c.asked, pErr.Status(), pErr.Code())
			}
			named := false
			for _, f := range pErr.Fields() {
				if f.Field == "cursor" {
					named = true
				}
			}
			if !named {
				t.Errorf("%s into %s does not name the cursor field: %v", c.minted, c.asked, pErr.Fields())
			}
			// AND IT NAMES BOTH DIRECTIONS, because a client holding a cursor does not
			// know which way its own walk was going and "cursor mismatch" alone would
			// send it looking for a bug in its token encoder.
			detail := pErr.Detail()
			if !strings.Contains(detail, c.minted.String()) || !strings.Contains(detail, c.asked.String()) {
				t.Errorf("the refusal %q does not name both directions", detail)
			}
			continue
		}
		if pErr != nil {
			t.Errorf("a cursor minted for %s was refused for a %s walk: %v", c.minted, c.asked, pErr)
		}
	}
}
