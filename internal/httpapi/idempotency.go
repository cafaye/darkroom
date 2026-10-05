// The `Idempotency-Key` layer: the HTTP half of a ledger whose other half is
// `internal/service/idempotency.go`.
//
// # WHAT LIVES WHERE, AND WHY IT IS SPLIT THERE
//
// That file says this half is "packet 05's, because it is a router concern and this
// package holds no HTTP", and the split is the dependency rule rather than a
// convenience: `service` holds the reserve/record/release decisions and this file
// holds the header, the response capture and the composition order. Nothing here
// reaches into `service`'s transactions and nothing there knows what a header is.
//
// # THE FIVE STEPS, IN THE ORDER THE SERVICE NAMES THEM
//
// `service`'s own header is the specification and it is followed literally:
//
//  1. RESERVE. `ReserveIdempotent`, before the handler runs.
//  2. BODY CHECK. A hit with a different hash is `409 idempotency_key_reused` —
//     inside that call, because the hash comparison is a ledger fact.
//  3. REPLAY. A hit with `status_code != 0` is the recorded response: written here,
//     with `Idempotency-Replayed: true`, and the handler is NOT called.
//  4. RUN. This layer holds the reservation, so the handler does the work.
//  5. RECORD OR RELEASE. A 2xx is stored; anything else releases the reservation.
//
// # WHERE IT SITS IN THE MIDDLEWARE ORDER, AND WHY IT IS INSIDE THE GATE
//
// `root.Use(traceContext)` then `root.Use(bodyLimit)` are the two outer layers and
// are untouched. This one is registered per-ROUTE rather than with `Use`, for the
// same reason `guarded` is: chi's `Use` applies to every route registered after it,
// and an idempotency ledger mounted globally would try to hash the body of a
// `DELETE /v1/assets/{id}` that has none.
//
// It is INSIDE the credential gate — inside `guarded` — because the ledger's scope
// is `(key, endpoint, principal_key)` and there is no principal before the gate. An
// anonymous request must not be able to reserve a key, and the layer needs a
// verified principal to name one. `TestAnUnauthenticatedIdempotencyRequestReservesNothing`
// is that ordering, asserted.
//
// # AND WHY THE ROUTES ARE CHOSEN FROM THE GENERATED PARAMS
//
// Only `createUpload` and `completeUpload` declare `IdempotencyKey` in
// `openapi/v1.yaml`, so only their generated `Params` structs carry the field. The
// list of idempotent routes is read out of the generated types by
// [declaresIdempotencyKey] rather than written here as operation names: a
// hand-written list of the two route patterns would be a SECOND inventory of the
// document's surface, which is the failure this repository has already paid for once
// — `src/http.rs`'s `http::OPERATIONS` is the Rust version of it and its own README
// admits that a route added without a row there left nine drift tests green.
//
// So if a third operation declares the parameter, this layer picks it up because the
// generated struct grew a field, and the only way it does NOT is a document edit
// nobody made.
//
// # WHY IT IS A MIDDLEWARE AND NOT A HANDLER CONCERN
//
// Capturing the response is the whole reason. Steps 3 and 5 both need the status and
// the exact bytes the handler produced, and a handler cannot see its own output —
// `writeJSON` marshals and writes, and the status is chosen in the same statement.
// A wrapper around the handler is the only place both are visible, which is why
// `service.CreateUpload` was never the right home for it. `handlers.go`'s comment
// says as much and this file is the answer to it.
//
// # AND WHY THE RESPONSE CAPTURE IS NOT A SHARED `ResponseWriter` ADAPTER
//
// It has to record the bytes AND pass them through in the same write, because the
// client's response is the artifact and a replay is a copy of it. [capturingWriter]
// buffers rather than tees, and this is the one consequence worth naming: a buffered
// response is not streamed. This service answers 201 and 200 with small JSON
// documents and 204 with nothing, so there is nothing to stream — but a future
// operation that streamed would need this layer to stop buffering, and the buffer is
// where that would show up.
package httpapi

import (
	"bytes"
	"io"
	"log/slog"
	"net/http"

	"github.com/cafaye/darkroom/internal/auth"
	"github.com/cafaye/darkroom/internal/service"
)

// HeaderIdempotencyKey is the request header the document declares.
//
// `openapi/v1.yaml`'s `parameters.IdempotencyKey` is `in: header` with
// `name: Idempotency-Key`, and the generated code reads it through
// `http.CanonicalHeaderKey`. The constant is here so this layer and any future
// caller agree on the spelling, and `TestTheIdempotencyHeaderIsTheDocumentedOne`
// holds it against the document.
const HeaderIdempotencyKey = "Idempotency-Key"

// ReplayedHeader is the document's header for a replayed response.
//
// `openapi/v1.yaml` types it as an `enum: ['true']`, so there is exactly one value
// this service ever writes and the constant is the whole of it. Absent means "this
// response is not a replay", which is why every non-replay path below leaves it
// unset rather than writing `false`: the document has no `false` member, and a
// client testing for header presence is testing for a replay.
const ReplayedHeader = "Idempotency-Replayed"

// replayedTrue is [ReplayedHeader]'s only permitted value.
const replayedTrue = "true"

// idempotency wraps one route so a keyed request reserves, runs, and records.
//
// `endpoint` is the ROUTE — `POST /v1/uploads`, the document's spelling — and never
// `r.URL.Path`. The distinction is load-bearing: `idempotency_keys`'s primary key is
// `(key, endpoint, principal_key)` and `endpoint` is "the route, e.g.
// `POST /v1/uploads`" per the migration. Using the request's own path would make
// `POST /v1/uploads/{id}/complete` a different endpoint per asset id, so a client
// reusing one key across two completions of the same asset would be told
// `idempotency_key_reused` for a request the contract considers identical. The route
// is what makes the scope "the operation" as the document means it.
//
// The caller derives it from the same value [idempotentRoutes] is keyed by, so this
// layer's scope and that list cannot drift apart.
func (s *Server) idempotency(endpoint string, next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		raw := r.Header.Values(HeaderIdempotencyKey)
		if len(raw) == 0 {
			// NO KEY AT ALL. "Requests without the key are processed normally", and
			// this is the whole of done-means 4: not a reservation, not a header, not
			// a read of the body. The handler cannot tell this branch from any
			// request, which is the strongest form of "behaves exactly as it did".
			next.ServeHTTP(w, r)
			return
		}
		if len(raw) > 1 {
			// The document types the key as ONE string, so a repeated header is not a
			// key this service is willing to interpret: which of the two would be the
			// scope? Refused as a broken key rather than silently taking the first,
			// because two spellings of one header is the client disagreeing with
			// itself and a silent choice would make a replay's scope depend on
			// header order.
			writeProblemError(w, r, service.ValidateIdempotencyKey(""))
			return
		}

		key := raw[0]
		if pErr := service.ValidateIdempotencyKey(key); pErr != nil {
			// THE EMPTY KEY IS HERE AND NOT DOWNSTREAM. A `""` that reached
			// `ReserveIdempotent` would be refused by the same call — the service
			// validates — but by then nothing would be wrong, because the handler has
			// already run and `CompleteIdempotent` would have stored a response for a
			// reservation that never existed. Refusing first is what makes "present
			// but empty" different from "absent" on the wire, which is the whole of
			// done-means 5.
			writeProblemError(w, r, pErr)
			return
		}

		tenant := auth.TenantOf(r)
		principal := service.PrincipalScope(tenant.UserID(), tenant.AccountID())

		// The body has to be read here and put back, because the hash is computed
		// over BYTES — `service.RequestHash` says so, and says why two spellings of
		// the same JSON are different requests — and because the handler still has to
		// parse it. `bodyLimit` has already wrapped `r.Body` in a
		// `http.MaxBytesReader`, so this read is bounded by the document's 4096
		// even though nothing here names a limit.
		body, err := io.ReadAll(r.Body)
		if err != nil {
			// An UNREADABLE body, not an oversized one: `bodyLimit` answers the known
			// length itself, and the chunked case fails inside `decodeJSON`. Neither
			// has reached the handler yet, and a reservation made on a hash of half a
			// body is a reservation nobody can ever satisfy. The handler runs and
			// answers with its own refusal — 400 for a truncated read — so the client
			// gets one document rather than this layer's opinion of it.
			next.ServeHTTP(w, r)
			return
		}
		r.Body = io.NopCloser(bytes.NewReader(body))

		// STEP 1-3: reserve, and either get the work or get the recorded response.
		replay, pErr := s.service.ReserveIdempotent(r.Context(), key, endpoint, principal, service.RequestHash(body))
		switch {
		case pErr != nil:
			// Covers the 409 for a different body AND the 409 for a request still in
			// flight. Both are `problem.Error`s the service already chose, so nothing
			// here re-decides which is which — and neither writes
			// `Idempotency-Replayed`, because neither replayed anything.
			writeProblemError(w, r, pErr)
			return
		case replay != nil:
			// STEP 3: THE ORIGINAL RESPONSE, verbatim.
			w.Header().Set(ReplayedHeader, replayedTrue)
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(replay.StatusCode)
			if _, err := w.Write(replay.Body); err != nil {
				slog.Error("a replayed response did not write", "endpoint", endpoint, "error", err)
			}
			return
		}

		// STEP 4: THE WORK.
		captured := &capturingWriter{ResponseWriter: w}
		next.ServeHTTP(captured, r)

		// STEP 5: RECORD OR RELEASE.
		//
		// A 2xx is recorded and everything else releases. The status is the only
		// signal, deliberately: this layer does not parse the body to decide whether
		// an outcome was good, because the service's own errors are the client's to
		// fix and a retry with the same key must be able to try again. "A 500 that
		// poisoned the key would make every subsequent retry fail forever."
		status, recorded := captured.recorded()
		if status >= 200 && status < 300 {
			s.service.CompleteIdempotent(r.Context(), key, endpoint, principal, status, recorded)
		} else {
			s.service.ReleaseIdempotent(r.Context(), key, endpoint, principal)
		}
	})
}

// capturingWriter is a `ResponseWriter` that records the status and the bytes while
// passing both through to the real one.
//
// It writes through rather than buffering-and-replaying, so the client's response
// timing is unchanged and this layer is not a place a slow client can be made to
// wait on: the buffer is a COPY for the ledger, never a detour.
//
// The first status wins, matching `net/http` and [httptest.ResponseRecorder]. A
// handler that calls `WriteHeader` twice has a bug, and what the client actually
// received is the first one.
//
// [http.Flusher] is implemented and deliberately does nothing but pass through,
// because an `http.ResponseWriter` that does not implement it makes a handler that
// flushes fail a type assertion, and one that claims it and ignores it would make
// that handler wait forever for a flush that never happens. This service has no
// streaming operation, so the pass-through is the whole behaviour.
//
// `http.Hijacker`, `io.ReaderFrom` and `http.Pusher` are deliberately NOT
// implemented: nothing in this service needs them, and each would be a capability
// this adapter cannot honestly offer while keeping a second copy of the bytes.
type capturingWriter struct {
	http.ResponseWriter
	status int
	body   bytes.Buffer
}

// WriteHeader records the status and forwards it.
func (c *capturingWriter) WriteHeader(status int) {
	if c.status != 0 {
		return
	}
	c.status = status
	c.ResponseWriter.WriteHeader(status)
}

// Write records the body and forwards it.
//
// A `Write` with no preceding `WriteHeader` is a 200 in `net/http`, and the
// recording says 200 too rather than 0 — so the ledger stores the response the
// client actually received.
func (c *capturingWriter) Write(b []byte) (int, error) {
	if c.status == 0 {
		c.status = http.StatusOK
	}
	if _, err := c.body.Write(b); err != nil {
		return 0, err
	}
	return c.ResponseWriter.Write(b)
}

// Flush passes through. See the type's comment.
func (c *capturingWriter) Flush() {
	if f, ok := c.ResponseWriter.(http.Flusher); ok {
		f.Flush()
	}
}

// recorded is the response the client received, or 0 for a handler that wrote
// nothing at all.
//
// The 0 case is not a 200 and not a 204, and [Server.idempotency] treats it as a
// failure and releases the reservation. `net/http` would answer such a handler with
// 200 and an empty body, and recording THAT as this key's response would hand the
// client's next retry a success it never earned.
func (c *capturingWriter) recorded() (int, []byte) {
	return c.status, c.body.Bytes()
}
