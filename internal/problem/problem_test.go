package problem

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"

	"github.com/cafaye/darkroom/internal/objectstore"
)

func TestCodeIsTheLastSegmentOfType(t *testing.T) {
	// core: "`code` is the same slug as the last segment of `type`."
	for _, err := range allCodes() {
		p := err.Problem("/v1/assets", "trace-1")
		last := p.Type[len(p.Type)-len(string(p.Code)):]
		if last != string(p.Code) {
			t.Errorf("type %q does not end in code %q", p.Type, p.Code)
		}
		if want := ErrorBase + "/" + string(p.Code); p.Type != want {
			t.Errorf("type = %q, want %q", p.Type, want)
		}
	}
}

func TestReservedCodesMapToTheirReservedStatus(t *testing.T) {
	// The mapping in core/docs/openapi-conventions.md, asserted rather than
	// assumed: a code moved to the wrong status is a breaking change for every
	// client that switches on it. Note that TWO codes are 409 and that they are
	// distinguishable — collapsing them would throw away the distinction core draws,
	// which is why this asserts the pair and not just the number.
	cases := []struct {
		err    *Error
		status int
		code   Code
	}{
		{Unauthorized("x"), 401, CodeUnauthorized},
		{Forbidden("x"), 403, CodeForbidden},
		{NotFound("x"), 404, CodeNotFound},
		{Conflict("x"), 409, CodeConflict},
		{Invalid("x"), 422, CodeValidationFailed},
		{IdempotencyKeyReused("x"), 409, CodeIdempotencyKeyReused},
		{Internal("x"), 500, CodeInternal},
		{Unavailable("x"), 503, CodeUnavailable},
	}
	for _, c := range cases {
		if c.err.Status() != c.status {
			t.Errorf("%s is %d, want %d", c.code, c.err.Status(), c.status)
		}
		if c.err.Code() != c.code {
			t.Errorf("code = %s, want %s", c.err.Code(), c.code)
		}
		if p := c.err.Problem("/x", "t"); p.Status != c.status {
			t.Errorf("problem status for %s is %d, want %d", c.code, p.Status, c.status)
		}
	}
	if Conflict("a").Status() != IdempotencyKeyReused("a").Status() ||
		Conflict("a").Code() == IdempotencyKeyReused("a").Code() {
		t.Error("conflict and idempotency_key_reused must be distinguishable at the same status")
	}
}

func TestErrorsArrayAppearsOnlyOn422(t *testing.T) {
	// A 422 with no per-field errors OMITS the array rather than sending `[]`:
	// "Emitting an empty array elsewhere is the kind of thing a client starts
	// branching on" is the reason, and `omitempty` is the mechanism.
	if p := Invalid("no field detail").Problem("/v1/x", "t"); p.Errors != nil {
		t.Errorf("a 422 with no fields sent %v", p.Errors)
	}
	fields := InvalidFields("checksum does not match", NewField("checksum", "mismatch"))
	p := fields.Problem("/v1/x", "t")
	if len(p.Errors) != 1 || p.Errors[0].Field != "checksum" || p.Errors[0].Code != "mismatch" {
		t.Errorf("errors[] = %v", p.Errors)
	}

	// Everything else must not serialise the key AT ALL. The check is on the
	// decoded object's keys rather than on the raw string: `type` is a
	// `https://errors.cafaye.com/...` URI, so a substring test would match every
	// response and prove nothing.
	for _, err := range []*Error{NotFound("x"), Conflict("x"), Internal("x"), Unauthorized("x"), Unavailable("x")} {
		raw, err2 := json.Marshal(err.Problem("/v1/x", "trace"))
		if err2 != nil {
			t.Fatalf("marshalling: %v", err2)
		}
		var decoded map[string]any
		if err := json.Unmarshal(raw, &decoded); err != nil {
			t.Fatalf("decoding %s: %v", raw, err)
		}
		if _, present := decoded["errors"]; present {
			t.Errorf("%s leaked an errors key: %s", err.Code(), raw)
		}
		for _, required := range []string{"type", "title", "status", "detail", "instance", "code", "trace_id"} {
			if _, present := decoded[required]; !present {
				t.Errorf("%s is missing %q: %s", err.Code(), required, raw)
			}
		}
		if decoded["trace_id"] != "trace" {
			t.Errorf("trace_id = %v, want the value passed in", decoded["trace_id"])
		}
	}
}

func TestDetailNeverCarriesACauseOrAHost(t *testing.T) {
	// core: `detail` "never contains an internal error, a host, a role or a query
	// fragment: those go to the log under the same trace_id." The cause is attached
	// for the log and must not reach the body — asserted on the RENDERED body,
	// because the detail string and the cause are separate fields and only the
	// rendering is the contract.
	cause := fmt.Errorf("dial tcp 10.0.0.5:5432: connection refused")
	for name, err := range map[string]*Error{
		"db":       FromDB(cause),
		"objects":  FromObjectStore(cause),
		"wrapped":  Internal("the request could not be completed").Wrap(cause),
		"unavail":  FromDB(pgErr("53300", "too many connections")),
		"notfound": FromDB(pgx.ErrNoRows),
	} {
		body, err2 := json.Marshal(err.Problem("/v1/assets/ast_1", "trace-abc"))
		if err2 != nil {
			t.Fatalf("%s: marshalling: %v", name, err2)
		}
		for _, leak := range []string{"10.0.0.5", "postgres://", "connection refused", "too many connections"} {
			if strings.Contains(strings.ToLower(string(body)), strings.ToLower(leak)) {
				t.Errorf("%s leaked %q into the body: %s", name, leak, body)
			}
		}
		// …and the cause is still reachable for the log, in every case. The body is
		// caller-safe AND the log line is not empty: a 500 that swallowed its cause
		// would leave the operator with nothing but a trace id.
		if err.Unwrap() == nil {
			t.Errorf("%s has no cause for the log", name)
		}
	}
}

func TestFromDBMapsEachFailureToItsOneStatus(t *testing.T) {
	cases := []struct {
		name   string
		err    error
		status int
		code   Code
	}{
		{"no rows is a 404", pgx.ErrNoRows, 404, CodeNotFound},
		{"a unique violation is a 409", pgErr("23505", "duplicate key"), 409, CodeConflict},
		{"too many connections is a 503", pgErr("53300", "sorry"), 503, CodeUnavailable},
		{"a deadlock is a 503", pgErr("40P01", "deadlock"), 503, CodeUnavailable},
		{"a serialization failure is a 503", pgErr("40001", "retry"), 503, CodeUnavailable},
		{"a check violation is a 500", pgErr("23514", "check"), 500, CodeInternal},
		{"anything else is a 500", pgErr("42601", "syntax"), 500, CodeInternal},
		{"a bare driver error is a 500", errors.New("boom"), 500, CodeInternal},
		{"a deadline is a 503", context.DeadlineExceeded, 503, CodeUnavailable},
	}
	for _, c := range cases {
		got := FromDB(c.err)
		if got.Status() != c.status || got.Code() != c.code {
			t.Errorf("%s: got %d/%s, want %d/%s", c.name, got.Status(), got.Code(), c.status, c.code)
		}
	}
	if FromDB(nil) != nil {
		t.Error("FromDB(nil) must be nil, so a caller can pass a result through")
	}
	// The two the service branches on by hand, because it RESOLVES them on purpose.
	if !IsUniqueViolation(pgErr("23505", "x")) || IsUniqueViolation(pgErr("23503", "x")) {
		t.Error("IsUniqueViolation must be exactly 23505")
	}
	if !IsCheckViolation(pgErr("23514", "x")) || IsCheckViolation(pgErr("23505", "x")) {
		t.Error("IsCheckViolation must be exactly 23514")
	}
	// And a WRAPPED pgconn error is still recognisable, which is what
	// `errors.As` buys and what a bare type assertion would not.
	wrapped := fmt.Errorf("inserting the asset: %w", pgErr("23505", "x"))
	if !IsUniqueViolation(wrapped) {
		t.Error("a wrapped unique violation must still be recognised: the service's duplicate branch depends on it")
	}
}

func TestFromObjectStoreMapsEachSentinelToItsOneStatus(t *testing.T) {
	cases := []struct {
		sentinel error
		status   int
		code     Code
	}{
		// Absent is a client-visible state, which is how objectstore documents it:
		// "A client-visible state at complete time (409)". Not 404 — the OBJECT is
		// missing, the ASSET is not.
		{objectstore.ErrNotFound, 409, CodeConflict},
		{objectstore.ErrTooLarge, 422, CodeValidationFailed},
		{objectstore.ErrUnavailable, 503, CodeUnavailable},
	}
	for _, c := range cases {
		// Wrapped, because that is how a real store returns it: `*objectstore.Error`
		// wraps the sentinel and adds the operation name.
		err := &objectstore.Error{Op: "head", Err: c.sentinel}
		got := FromObjectStore(err)
		if got.Status() != c.status || got.Code() != c.code {
			t.Errorf("%v: got %d/%s, want %d/%s", c.sentinel, got.Status(), got.Code(), c.status, c.code)
		}
	}
	if got := FromObjectStore(&objectstore.Error{Op: "get", Err: errors.New("bucket darkroom-prod-eu s3")}); got.Status() != 500 {
		t.Errorf("an unmapped store error is %d, want 500", got.Status())
	} else if got.Detail() != "the request could not be completed" {
		t.Errorf("an unmapped store error said %q", got.Detail())
	}
	if FromObjectStore(nil) != nil {
		t.Error("FromObjectStore(nil) must be nil")
	}
}

func TestTitlesAreFixedPerCode(t *testing.T) {
	// core: "`title` is a fixed, human-readable summary for that code; it may be
	// reworded without a version bump." Fixed means fixed: two calls with different
	// details produce the same title, so a client that switches on the title keeps
	// working when the sentence is reworded.
	for _, err := range allCodes() {
		other := *err
		first := err.Problem("/x", "t")
		other.detail = "a completely different sentence"
		second := other.Problem("/y", "u")
		if first.Title != second.Title {
			t.Errorf("%s title changed with the detail: %q vs %q", err.Code(), first.Title, second.Title)
		}
		if first.Title == "" {
			t.Errorf("%s has an empty title", err.Code())
		}
	}
	if len(registry) != 8 {
		t.Errorf("the registry holds %d codes; every one of them needs a status and a title", len(registry))
	}
}

// allCodes is one constructed error per code, so a new code cannot be added to the
// registry without a construction a test walks.
func allCodes() []*Error {
	return []*Error{
		Unauthorized("x"), Forbidden("x"), NotFound("x"), Conflict("x"),
		Invalid("x"), IdempotencyKeyReused("x"), Internal("x"), Unavailable("x"),
	}
}

func pgErr(code, message string) error {
	return &pgconn.PgError{Code: code, Message: message}
}
