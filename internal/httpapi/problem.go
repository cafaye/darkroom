// The problem writer, and why it renders JSON by hand.
//
// `../cafaye/gateway/internal/httpapi/problem.go` and this file are the same
// decision reached twice, and the second time it is worth saying what the decision
// WAS rather than re-deriving it:
//
// `internal/problem` owns `Error`, which carries the status, the code, the title and
// the field errors. This package owns the BYTES. And `problem.Problem` — the wire
// struct — is written in the document's field order, so `encoding/json` renders it
// correctly and this file does not need the hand-rolled writer the gateway needed.
//
// That is a real difference and not luck: `internal/problem` is THIS repository's
// hand-written type, and Go marshals a struct in field-declaration order. The
// gateway's was `api.Problem`, a GENERATED struct, and oapi-codegen writes fields
// alphabetically — so `json.Marshal` produced
// `{"code":…,"detail":…,"instance":…,"status":…,"title":…,"trace_id":…,"type":…}`
// where the document declares `type, title, status, detail, instance, code,
// trace_id`.
//
// EVERY client parses both spellings identically, which is exactly why it would
// have shipped, and `internal/equivalence` found it on the first run of a single 503
// case. If this file ever starts marshalling a GENERATED struct instead, that trap
// returns; `TestTheProblemBodyIsInTheDocumentOrder` is what stops it.
//
// # THE TRACE ID COMES FROM ONE PLACE
//
// core: "`trace_id` is always present and always matches the `X-Trace-Id` response
// header." [traceContext] sets the header from the id it put in the context, and
// [writeProblemError] reads the SAME value for the body. Two places writing an id
// would be two ids, and the header would stop being evidence.
package httpapi

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"

	"github.com/cafaye/darkroom/internal/api"
	"github.com/cafaye/darkroom/internal/auth"
	"github.com/cafaye/darkroom/internal/problem"
)

// traceContextKey is a package-private type, so nothing outside this package can
// read or forge the id. A string key would be forgeable by anything importing
// `context`, and a forged trace id is a forged log correlation.
type traceContextKey struct{}

func withTraceID(ctx context.Context, id string) context.Context {
	return context.WithValue(ctx, traceContextKey{}, id)
}

// traceIDOf is the id for this request, minting one if the middleware was not
// mounted. It cannot be empty in a body, because the one caller that has to have
// something — [writeProblemError] — is reachable from a path where the middleware
// did run, and a direct handler test gets an id rather than a `""`.
func traceIDOf(r *http.Request) string {
	if id, ok := r.Context().Value(traceContextKey{}).(string); ok && id != "" {
		return id
	}
	return newTraceID()
}

// refuse is the [auth.Refuser] this package supplies.
//
// The dependency runs one way on purpose: `internal/auth` does not import the
// package that renders the envelope, which is what keeps the verifier testable
// without an HTTP surface.
func refuse(w http.ResponseWriter, r *http.Request, refusal *auth.Refusal) {
	p := problem.Invalid(refusal.Detail)
	switch refusal.Status {
	case http.StatusUnauthorized:
		p = problem.Unauthorized(refusal.Detail)
	case http.StatusForbidden:
		p = problem.Forbidden(refusal.Detail)
	case http.StatusServiceUnavailable:
		p = problem.Unavailable(refusal.Detail)
	default:
		// A status this package has no code for. A refusal that reached here is a
		// defect in `internal/auth`, and the honest answer for a defect is a document
		// the contract describes rather than a code nobody wrote a title for:
		// `internal/problem`'s registry is total by construction, so the rewrite
		// lands on a code that exists. Logged, because it should never happen.
		slog.Error("auth refusal with an unmapped status", "status", refusal.Status, "code", refusal.Code)
		p = problem.Unavailable(refusal.Detail)
	}
	writeProblemError(w, r, p)
}

// problemPayloadTooLarge is the 413.
//
// A 422 code with a 422 body would be a lie, so this is the one refusal here that
// does not come out of `internal/problem`'s registry: `payload_too_large` is NOT on
// core's reserved list, and the registry's statuses are total by construction — which
// is exactly what stops a handler writing a status by hand. So this file writes the
// status and the document together, once, and
// `TestTheBodyLimitIsTheDocumented413` holds both.
//
// The gateway reuses the same slug for the same failure (identity answers with it),
// and one vocabulary across the platform beats a service-private synonym for the
// same refusal.
func problemPayloadTooLarge(limit int64) *problem.Error {
	return problem.InvalidFields(
		fmt.Sprintf("the request body exceeded %d bytes", limit),
		problem.NewField("body", "too_large"),
	)
}

// writeStatus writes `p`'s envelope at a status the registry does not carry.
//
// # THE ONE PLACE A STATUS IS NOT THE CODE'S
//
// `internal/problem` states "A handler never writes a status code by hand, and the
// cross-tenant 404 is NotFound rather than Forbidden at every call site that reaches
// it", and its `registry` is total by construction so that "a code cannot be added
// with a status that disagrees with its title". That is the right rule and it is why
// 413 is interesting: `payload_too_large` is NOT on core's reserved list, so there is
// no registry entry, and `problem.InvalidFields` renders a 422 for it.
//
// A 422 whose detail says "the body exceeded 4096 bytes" is a lie about what
// happened — the document declares a `413` on `POST /v1/uploads` for exactly this,
// and a client that switches on `status` to decide whether to shrink its retry would
// be told the request was semantically wrong.
//
// So this function renders the SAME envelope with the document's status, in one
// place, and `TestTheBodyLimitIsTheDocumented413` asserts both the 413 and the
// `application/problem+json` content type. Everything else in this package goes
// through `writeProblemError`, which reads the status from the registry.
func writeStatus(w http.ResponseWriter, r *http.Request, status int, p *problem.Error) {
	if p == nil {
		p = problem.Internal("the request could not be completed")
	}

	traceID := traceIDOf(r)
	body := p.Problem(instanceOf(r), traceID)
	body.Status = status

	encoded, err := json.Marshal(body)
	if err != nil {
		slog.Error("a problem document did not encode", "path", r.URL.Path, "error", err)
		encoded = []byte(`{"type":"https://docs.cafaye.com/errors/internal","title":"Internal server error",` +
			`"status":500,"detail":"the request could not be completed","instance":"/",` +
			`"code":"internal","trace_id":""}`)
	}

	w.Header().Set("Content-Type", "application/problem+json")
	w.Header().Set("X-Trace-Id", traceID)
	w.WriteHeader(status)
	if _, err := w.Write(encoded); err != nil {
		slog.Error("a problem document did not write", "path", r.URL.Path, "error", err)
	}
}

// problemNotFound answers a path chi did not route.
//
// The SAME envelope as every other refusal, and that is the whole point: a service
// whose 404 for an unrouted path is `text/plain` has one response outside the
// contract, and a client switching on `application/problem+json` has to special-case
// it. `not_found` rather than a code about routing, because from the caller's side
// the question is the same one `GET /v1/assets/{missing}` asks.
func problemNotFound() http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		writeProblemError(w, r, problem.NotFound("the requested item does not exist"))
	}
}

// problemMethodNotAllowed answers a method chi routed to nothing for this path.
//
// `not_found` and NOT a 405: chi's own 405 would be a status the document does not
// declare on any path, and `openapi/v1.yaml` is the contract. Refusing with the
// document's absence is also the non-leaking answer — "this path exists but not for
// this method" is one more fact about the surface than a client needs, and
// `AGENTS.md`'s rule is that absence and refusal must be indistinguishable.
func problemMethodNotAllowed() http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		writeProblemError(w, r, problem.NotFound("the requested item does not exist"))
	}
}

// writeProblemError renders `p` as the documented envelope, with the trace id from
// the context and the same value in the header.
func writeProblemError(w http.ResponseWriter, r *http.Request, p *problem.Error) {
	if p == nil {
		// A nil error reaching the writer is a defect in the caller, and answering
		// 500 is better than panicking on a response path: the rule is that a
		// decision which cannot be rendered still gets a document.
		slog.Error("a nil problem reached the writer", "path", r.URL.Path)
		p = problem.Internal("the request could not be completed")
	}

	traceID := traceIDOf(r)
	body := p.Problem(instanceOf(r), traceID)

	// `Marshal` first, `WriteHeader` second. A marshalling failure then means nothing
	// has been written yet, so there is no half-sent document for a client to parse.
	// `json.Marshal` rather than `json.NewEncoder(w).Encode`, which appends a
	// newline to every body.
	encoded, err := json.Marshal(body)
	if err != nil {
		slog.Error("a problem document did not encode", "path", r.URL.Path, "error", err)
		encoded = []byte(`{"type":"https://docs.cafaye.com/errors/internal","title":"Internal server error",` +
			`"status":500,"detail":"the request could not be completed","instance":"/",` +
			`"code":"internal","trace_id":""}`)
	}

	w.Header().Set("Content-Type", "application/problem+json")
	w.Header().Set("X-Trace-Id", traceID)
	w.WriteHeader(body.Status)
	if _, err := w.Write(encoded); err != nil {
		slog.Error("a problem document did not write", "path", r.URL.Path, "error", err)
	}
}

// instanceOf is the path a problem document names.
//
// The path, never the query: a caller who put a credential in one should not find it
// echoed back in an error body. `URL.EscapedPath` rather than `URL.Path` because the
// former keeps the caller's own spelling — `/v1/assets/caf%C3%A9` stays escaped,
// which is what lets a caller correlate a refusal with their own request log. The
// gateway went further and normalised dot segments; darkroom does not, because chi
// has already resolved them by the time a handler runs and normalising twice is two
// spellings of "what path is this".
func instanceOf(r *http.Request) string {
	return r.URL.EscapedPath()
}

// bindingErrorHandler is the `ErrorHandlerFunc` for the generated router.
//
// The two refusals the generated binder produces for a malformed
// `Idempotency-Key` header (or any other binding failure) bypass this
// service's envelope unless this function is set: the runtime's default writes
// `text/plain` with a bare sentence. Every other refusal here is
// `application/problem+json` with the documented code, so setting this keeps
// the surface uniform. README "Not done" names this as the fix.
func bindingErrorHandler(w http.ResponseWriter, r *http.Request, err error) {
	field := ""
	code := "invalid"
	switch e := err.(type) {
	case *api.RequiredParamError:
		field = e.ParamName
		code = "required"
	case *api.RequiredHeaderError:
		field = e.ParamName
		code = "required"
	case *api.InvalidParamFormatError:
		field = e.ParamName
		code = "invalid_format"
	case *api.TooManyValuesForParamError:
		field = e.ParamName
		code = "too_many_values"
	case *api.UnmarshalingParamError:
		field = e.ParamName
		code = "invalid"
	default:
		// Anything else is not a binding failure this function knows how to
		// name; render it as a generic validation failure rather than letting
		// the runtime's text/plain default escape.
	}
	p := problem.Invalid("the request could not be bound")
	if field != "" {
		p = problem.InvalidFields("the request could not be bound",
			problem.NewField(field, code))
	}
	writeProblemError(w, r, p)
}
