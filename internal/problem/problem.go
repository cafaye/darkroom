// Package problem is darkroom's error envelope: RFC 9457 `application/problem+json`
// plus cafaye's `code` and `trace_id` extensions.
//
// Copied from `moon/cafaye/core/docs/openapi-conventions.md#error-envelope`, which
// is normative: "no service invents its own error body". The Rust tree carries the
// same shape in `src/error.rs` and the two are the same contract, not two designs.
//
// # WHY THIS IS A SEPARATE PACKAGE AND NOT PART OF THE SERVICE
//
// The service DECIDES the status — a checksum mismatch is 422 because the service
// said so, not because a handler thought so — and the HTTP layer TRANSPORTS it.
// Packet 05 owns the router, so it needs the status, the code and the body without
// importing the state machine to get them. That is the whole reason this is its
// own package: `Problem` is the seam between a decision and a response, and a
// decision that cannot be rendered is a decision nobody can finish.
//
// # TWO RULES FROM core THAT DRIVE THE DESIGN
//
//   - "Never 404 for authorization failures on a resource the caller cannot see —
//     404 is correct there, 403 is not allowed to leak existence." Tenant isolation
//     therefore produces [NotFound], never [Forbidden]. `AGENTS.md` says it from
//     the other side: "Cross-tenant is 404, not 403. And the body must be
//     byte-identical to the one for an id that never existed."
//   - "`trace_id` is always present and always matches the `X-Trace-Id` response
//     header." The header and the body field come from the SAME value at the one
//     place that writes both, so they cannot drift.
//
// # WHY `code` IS THE CONTRACT AND NOT `status`
//
// A client branches on `code`, and core's set is a reserved vocabulary rather than
// one error per endpoint: `conflict` and `idempotency_key_reused` are BOTH 409 and
// a client can act on exactly one of them, because one means "the world changed" and
// the other means "pick a new key". Collapsing them into a status would throw away
// the distinction the document exists to preserve, which is why the table below is
// asserted by a test rather than left to a reader.
package problem

import (
	"context"
	"errors"
	"fmt"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"

	"github.com/cafaye/darkroom/internal/objectstore"
)

// ErrorBase is the base URI for every `type`. core owns this list; the codes below
// are all in it, and each is the last segment of its own `type`.
const ErrorBase = "https://docs.cafaye.com/errors"

// Code is the machine-readable slug a client switches on. core: "`code` is the same
// slug as the last segment of `type`."
type Code string

// The reserved codes. `AGENTS.md`: "The code slug set is the contract."
const (
	CodeUnauthorized         Code = "unauthorized"
	CodeForbidden            Code = "forbidden"
	CodeNotFound             Code = "not_found"
	CodeConflict             Code = "conflict"
	CodeValidationFailed     Code = "validation_failed"
	CodeIdempotencyKeyReused Code = "idempotency_key_reused"
	CodeInternal             Code = "internal"
	CodeUnavailable          Code = "unavailable"
)

// statusOf, titleOf and requiredOf are the three facts per code. They live in one
// table rather than in three functions so that a code cannot be added with a status
// that disagrees with its title — which is a breaking change for every client that
// switches on it, and is exactly the kind of drift a table makes impossible.
var registry = map[Code]struct {
	status   int
	title    string
	required bool
}{
	CodeUnauthorized:         {401, "Unauthorized", false},
	CodeForbidden:            {403, "Forbidden", false},
	CodeNotFound:             {404, "Not found", false},
	CodeConflict:             {409, "Conflict", false},
	CodeValidationFailed:     {422, "Validation failed", true},
	CodeIdempotencyKeyReused: {409, "Idempotency key reused", false},
	CodeInternal:             {500, "Internal server error", false},
	CodeUnavailable:          {503, "Service unavailable", false},
}

// FieldError is one entry in `errors[]`. core: "`errors[]` appears only for 422 and
// lists per-field failures."
type FieldError struct {
	Field string `json:"field"`
	Code  string `json:"code"`
}

// NewField builds one entry. Exists so a field name and a code are never spelled
// twice in two different files and drift.
func NewField(field, code string) FieldError { return FieldError{Field: field, Code: code} }

// Error is every failure this service can return, carrying its own status.
//
// ONE TYPE rather than one Go type per failure, because the status is the thing
// that must not be assembled by hand at a call site: `AGENTS.md` says "Errors map
// to HTTP status through one place", and the constructors below are that place. A
// service that grows a second error type grows a second place.
type Error struct {
	code   Code
	detail string
	fields []FieldError
	// cause is for the log and never for a response body. `AGENTS.md`: "Probe
	// failures are logged, never returned in detail" and core: `detail` "never
	// contains an internal error, a host, a role or a query fragment".
	cause error
}

func (e *Error) Error() string {
	if e.cause != nil {
		return fmt.Sprintf("%s: %s: %v", e.code, e.detail, e.cause)
	}
	return fmt.Sprintf("%s: %s", e.code, e.detail)
}

// Unwrap exposes the cause to `errors.Is`/`errors.As` for a log handler, and to
// nothing else: it is not reachable from the rendered body.
func (e *Error) Unwrap() error { return e.cause }

// Code is the slug. Read by packet 05's router and asserted against the last
// segment of `type` by a test.
func (e *Error) Code() Code { return e.code }

// Status is the HTTP status. Every code has exactly one.
func (e *Error) Status() int { return registry[e.code].status }

// Detail is the caller-safe sentence. It never carries a cause: [FromDB] and
// [FromObjectStore] put the cause in the log and a fixed sentence here.
func (e *Error) Detail() string { return e.detail }

// Fields is the `errors[]` array, non-empty only for 422.
func (e *Error) Fields() []FieldError { return e.fields }

// Constructors. A handler never writes a status code by hand, and the cross-tenant
// 404 is [NotFound] rather than [Forbidden] at every call site that reaches it.

// Unauthorized is 401. Never says which part of the credential failed.
func Unauthorized(detail string) *Error {
	return &Error{code: CodeUnauthorized, detail: detail}
}

// Forbidden is 403: the token verified and lacks a SCOPE. A capability failure, and
// only ever that one. `AGENTS.md`: "Cross-tenant is 404, not 403. […] A 403 leaks
// existence; a different body is just as much of an oracle as a different status."
func Forbidden(detail string) *Error {
	return &Error{code: CodeForbidden, detail: detail}
}

// NotFound is 404, and it is ALSO the answer to "this asset belongs to another
// account". "The rule is absence, not refusal" (`AGENTS.md`) — a refusal
// distinguishes the two cases, so a caller enumerating ids would get a directory of
// every asset on the platform without reading a single row.
func NotFound(detail string) *Error {
	return &Error{code: CodeNotFound, detail: detail}
}

// Conflict is 409: a state conflict. Completing an upload whose object never arrived,
// or a duplicate that is not allowed to resolve to the existing asset.
func Conflict(detail string) *Error {
	return &Error{code: CodeConflict, detail: detail}
}

// Unavailable is 503: a dependency this request needs is not answering. Not 500,
// because 503 is what tells an orchestrator the instance is alive and should not be
// restarted — and `AGENTS.md` makes that exact distinction for the probes: "A
// database outage must not get the process restarted out from under in-flight
// uploads; that is `/readyz`'s job."
func Unavailable(detail string) *Error {
	return &Error{code: CodeUnavailable, detail: detail}
}

// Internal is 500 with a caller-safe message. The cause goes to the log.
func Internal(detail string) *Error {
	return &Error{code: CodeInternal, detail: detail}
}

// Invalid is 422 for a whole-request failure with no single offending field.
func Invalid(detail string) *Error {
	return &Error{code: CodeValidationFailed, detail: detail}
}

// InvalidFields is 422 with per-field detail, which is the shape core documents.
func InvalidFields(detail string, fields ...FieldError) *Error {
	return &Error{code: CodeValidationFailed, detail: detail, fields: fields}
}

// IdempotencyKeyReused is 409 for a replay of one `Idempotency-Key` with a different
// body. Distinct from [Conflict] because a client CAN act on it: it means "pick a
// new key", not "the world changed".
func IdempotencyKeyReused(detail string) *Error {
	return &Error{code: CodeIdempotencyKeyReused, detail: detail}
}

// Wrap attaches a cause for the log without changing what the caller sees. Used by
// [FromDB] and [FromObjectStore], and by nothing that puts the cause in `detail`.
func (e *Error) Wrap(cause error) *Error {
	e.cause = cause
	return e
}

// Problem is the wire body. Field names and requiredness match identity's
// `components.schemas.Problem` exactly — a client generated from either document
// sees the same object.
type Problem struct {
	Type     string `json:"type"`
	Title    string `json:"title"`
	Status   int    `json:"status"`
	Detail   string `json:"detail"`
	Instance string `json:"instance"`
	Code     Code   `json:"code"`
	TraceID  string `json:"trace_id"`
	// Errors appears only on 422. Emitting an empty array elsewhere is the kind
	// of thing a client starts branching on.
	Errors []FieldError `json:"errors,omitempty"`
}

// Problem renders the envelope for `instance` and `traceID`.
//
// The two arguments come from the request context, and packet 05 writes the
// `X-Trace-Id` header from the SAME traceID it passes here — that is what makes
// "`trace_id` is always present and always matches the `X-Trace-Id` response
// header" structural rather than a convention two places have to keep.
func (e *Error) Problem(instance, traceID string) Problem {
	return Problem{
		Type:     ErrorBase + "/" + string(e.code),
		Title:    registry[e.code].title,
		Status:   e.Status(),
		Detail:   e.detail,
		Instance: instance,
		Code:     e.code,
		TraceID:  traceID,
		Errors:   e.fields,
	}
}

// FromDB maps a database failure onto the envelope.
//
// THREE CASES, and the third is the one that is easy to get wrong:
//
//   - `pgx.ErrNoRows` is [NotFound]. It is what an id-scoped read returns for
//     another account's row, so this arm is the reason a cross-tenant get and a
//     missing id produce the same body.
//   - A unique violation is a [Error] with no status of its own: it is the
//     DUPLICATE-UPLOAD signal the service resolves on purpose, and a generic
//     mapping would turn it into a 500 at the one place that is written to handle
//     it. [IsUniqueViolation] is how the service recognises it.
//   - Everything else is 500 with a FIXED detail, or 503 for the codes that mean
//     "this connection cannot do anything right now". The cause goes to the log
//     under the trace id and nowhere else.
func FromDB(err error) *Error {
	if err == nil {
		return nil
	}
	if errors.Is(err, pgx.ErrNoRows) {
		return NotFound("the requested item does not exist").Wrap(err)
	}
	if IsUniqueViolation(err) {
		return Conflict("this item already exists").Wrap(err)
	}
	if isRetryable(err) {
		return Unavailable("database is unavailable").Wrap(err)
	}
	return Internal("the request could not be completed").Wrap(err)
}

// FromObjectStore maps an object-store failure onto the envelope.
//
// `ErrNotFound` is 409 and not 404: the OBJECT is missing, the ASSET is not, and
// the client's request was well-formed. It is a client-visible state, which is
// exactly how `objectstore` documents that sentinel — "A client-visible state at
// complete time (409)". `ErrTooLarge` is 422 because the object contradicts the
// scope the client declared, which is the same class of fault as a byte_size that
// does not match at complete time.
//
// Everything else is 500 with a fixed detail. The SDK's error string carries a
// bucket name and a region, and `AGENTS.md` says so in as many words for the
// probes: "An unauthenticated caller must not learn that a database host is
// `10.0.0.5`." The same rule holds here.
func FromObjectStore(err error) *Error {
	if err == nil {
		return nil
	}
	switch {
	case errors.Is(err, objectstore.ErrNotFound):
		return Conflict("the object was not found in storage").Wrap(err)
	case errors.Is(err, objectstore.ErrTooLarge):
		return InvalidFields(
			"the object is larger than the declared size",
			NewField("byte_size", "too_large"),
		).Wrap(err)
	case errors.Is(err, objectstore.ErrUnavailable):
		return Unavailable("object storage is unavailable").Wrap(err)
	default:
		return Internal("the request could not be completed").Wrap(err)
	}
}

// IsUniqueViolation reports whether err is Postgres 23505, on `(account_id,
// checksum)` or on `storage_key` or on `(asset_id, kind)`.
//
// Named rather than left inline because the create path resolves ONE of these on
// purpose — the duplicate-upload rule in the service's header — and a reader who
// cannot see which constraint this means cannot tell an intentional branch from an
// accident.
func IsUniqueViolation(err error) bool {
	var pgErr *pgconn.PgError
	return errors.As(err, &pgErr) && pgErr.Code == "23505"
}

// IsCheckViolation reports whether err is Postgres 23514, a `check` constraint from
// `migrations/0001_assets.sql`.
//
// Only reachable with a value the service derived itself, so it is 500 and never
// 422: a `check` failing on an enum this package parsed means the code and the
// schema disagree, and blaming the client for a row this service wrote is how a
// deploy bug turns into a support ticket.
func IsCheckViolation(err error) bool {
	var pgErr *pgconn.PgError
	return errors.As(err, &pgErr) && pgErr.Code == "23514"
}

// isRetryable is the "the database cannot answer right now" set: the two
// transaction-abort codes and pgx's own pool timeout.
//
// 53300 (`too_many_connections`) is in the list because `AGENTS.md` records the
// measured version of it: the shared cluster caps each role's connections and "a
// pool that outgrows its share does not queue politely — it fails the FATAL 53300
// against its own role". That is a 503, not a 500.
func isRetryable(err error) bool {
	var pgErr *pgconn.PgError
	if errors.As(err, &pgErr) {
		switch pgErr.Code {
		case "40001", "40P01", "53300", "57P01":
			return true
		}
	}
	return errors.Is(err, pgx.ErrTxClosed) || errors.Is(err, context.DeadlineExceeded)
}
