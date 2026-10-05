package httpapi_test

// `GET /v1/assets`'s two REFUSALS, at the wire: an `order` the document does not
// enumerate, and a cursor minted for the other direction.
//
// # WHY BOTH ANSWER WITHOUT A DATABASE, AND WHY THAT IS THE POINT
//
// `httpapi_test.go`'s header explains why this package's service is `service.New(nil,
// nil)`: past the auth gate everything that needs a database is the `--db` tier's
// job. Both refusals here are decided BEFORE the service is called — the order is
// parsed and the cursor is checked against it in the handler, so a request this
// server can answer about itself never opens a transaction. That is also what makes
// these assertions meaningful: if either refusal ever moved after the service call,
// the handler would call `ListAssets` on a service with no store and the test would
// PANIC on the nil pointer rather than quietly pass.
//
// The successful ordering — `order=asc` answering oldest-first, and a walk following
// the direction — is `internal/service`'s to prove and is proven there against real
// Postgres (`listing_test.go`). A 200 shape is not asserted here for the same reason
// this file asserts no successful body: doing it honestly needs a seeded database, and
// a body decoded from a `nil` store is not an answer.

import (
	"encoding/json"
	"net/http"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/cafaye/darkroom/internal/service"
)

// validationProblem is [problem] plus the field list the 422s carry. It is decoded
// with a struct so a test that forgets to assert a field cannot pass on its absence.
//
// The wire spells it `errors`, which is the document's key (`problem.Problem.Errors`
// renders it) and not `fields` — which is the name `*problem.Error` calls it in Go.
// A test that decoded the wrong key would report "the refusal names no field" for
// every 422 in the service, which is exactly the assertion that has to be right.
type validationProblem struct {
	problem
	Errors []struct {
		Field string `json:"field"`
		Code  string `json:"code"`
	} `json:"errors"`
}

func decodeValidationProblem(t *testing.T, r *recorder) validationProblem {
	t.Helper()
	var p validationProblem
	if err := json.Unmarshal(r.body, &p); err != nil {
		t.Fatalf("the body is not a problem document: %v\n%s", err, r.body)
	}
	return p
}

func (p validationProblem) names(field string) bool {
	for _, f := range p.Errors {
		if f.Field == field {
			return true
		}
	}
	return false
}

// TestListAssetsRefusesAnOrderTheDocumentDoesNotEnumerate is done-means 6 at the wire,
// and it is also the VERIFICATION the packet asks for: the generated binder does not
// enforce the document's `enum: [asc, desc]`.
//
// `api.ListAssetsParamsOrder` has a `Valid()` method and `api.gen.go` never calls it
// — the generated binding is
// `runtime.BindQueryParameterWithOptions(…, Type: "string")`, which takes whatever
// the query string holds. So this is asserted rather than assumed: if the router ever
// started enforcing the enum, these would come back as the generated
// `InvalidParamFormatError` — a 400 with no `fields` — and the assertions on 422,
// `validation_failed` and the `order` field would fail.
//
// It is a REFUSAL and not the default that omitting `order` gets, because `order` is
// a value the document enumerates: there is no nearest answer to `oldest`, and serving
// newest-first to a client that asked for something else is the silent wrong answer
// this parameter's absence used to be.
func TestListAssetsRefusesAnOrderTheDocumentDoesNotEnumerate(t *testing.T) {
	for _, raw := range []string{
		"anything-else", // the packet's own example
		"oldest",        // what a human means by asc
		"ASC",           // the document enumerates lowercase words
		"desc ",         // a trailing space is a different string
		"",              // `?order=` is a client with no value, not a request for the default
	} {
		id := newIdentity(t)
		rec := newRecorder()
		path := "/v1/assets?order=" + url.QueryEscape(raw)
		handlerFor(t, id).ServeHTTP(rec, asCaller(t, id, request(http.MethodGet, path, nil)))
		rec.read()

		if rec.status != http.StatusUnprocessableEntity {
			t.Errorf("order=%q answered %d, want 422: %s", raw, rec.status, rec.body)
			continue
		}
		p := decodeValidationProblem(t, rec)
		if p.Code != "validation_failed" {
			t.Errorf("order=%q gave code %q, want validation_failed", raw, p.Code)
		}
		if !p.names("order") {
			t.Errorf("order=%q is a 422 that does not name the order field: %v", raw, p.Errors)
		}
		if rec.Header().Get("Content-Type") != "application/problem+json" {
			t.Errorf("order=%q is not application/problem+json", raw)
		}
	}
}

// TestListAssetsRefusesACursorFromTheOtherDirection is done-means 5 at the wire.
//
// A cursor records the direction its page was walked in, because the row-value
// comparison it stands for is only correct for that direction: a descending token in
// an ascending scan skips every row below the position and repeats the ones above it,
// with a 200 and a plausible-looking page. A UI that re-sorts mid-walk — which is what
// clicking the sort column does — has to be told, and told in a way it can act on.
func TestListAssetsRefusesACursorFromTheOtherDirection(t *testing.T) {
	for _, c := range []struct{ minted, asked service.SortOrder }{
		{service.OrderDesc, service.OrderAsc},
		{service.OrderAsc, service.OrderDesc},
	} {
		id := newIdentity(t)
		token := service.EncodeCursor(service.Cursor{
			CreatedAt: time.Now(), ID: service.NewUUID(), Order: c.minted,
		})
		path := "/v1/assets?order=" + c.asked.String() + "&cursor=" + url.QueryEscape(token)

		rec := newRecorder()
		handlerFor(t, id).ServeHTTP(rec, asCaller(t, id, request(http.MethodGet, path, nil)))
		rec.read()

		if rec.status != http.StatusUnprocessableEntity {
			t.Fatalf("a %s cursor presented as %s answered %d, want 422: %s",
				c.minted, c.asked, rec.status, rec.body)
		}
		p := decodeValidationProblem(t, rec)
		if p.Code != "validation_failed" {
			t.Errorf("got code %q, want validation_failed", p.Code)
		}
		if !p.names("cursor") {
			t.Errorf("the refusal does not name the cursor field: %v", p.Errors)
		}
		// AND IT NAMES BOTH DIRECTIONS, because a client holding a cursor does not know
		// which way its own walk was going: "cursor mismatch" alone would send it
		// looking for a bug in its token encoder.
		if !strings.Contains(p.Detail, c.minted.String()) || !strings.Contains(p.Detail, c.asked.String()) {
			t.Errorf("the detail %q does not name both directions", p.Detail)
		}
	}
}
