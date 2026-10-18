// The HTTP layer: the router, the middleware order, the trace context, the body
// limit, and the transport of every decision `internal/service` makes.
//
// `AGENTS.md`, from the Rust side this replaces: "Everything that is a *decision*
// lives here rather than in a handler…" — and this file is the half that
// translates. `service` decides the status; `problem` owns the code and the title;
// this package renders the envelope, extracts the request, and calls the rule. A
// handler below that decides anything is a bug with a status code.
//
// # THE ROUTER IS BUILT FROM THE GENERATED FILE, NOT FROM A TABLE
//
// This is the packet's central point, and it is worth being precise about what
// replaced what. `src/http.rs` hand-maintained `http::OPERATIONS` "because axum
// cannot enumerate its own routes", and the Rust README admits the cost: a planted
// `.route()` on a new path left all nine drift tests green. A hand-maintained
// inventory is a second copy that goes stale silently.
//
// Here the routes come out of `internal/api/api.gen.go`, which `oapi-codegen`
// produced from `openapi/v1.yaml`. [New] walks THAT router with `chi.Walk` and
// re-registers every route it finds, wrapped in the scope its method requires. So:
//
//   - there is no path list in this repository to forget,
//   - a new `operationId` in the document is a new method on the generated
//     `ServerInterface`, and not implementing it does not compile,
//   - and a route that is not in the document cannot be reached, because the only
//     thing that registers routes is code derived from the document.
//
// [routes_test.go] then holds all three facts together, in both directions.
//
// # THE PROBES ARE EXEMPT BY AN EXPLICIT SET, NOT BY ORDER
//
// `src/http.rs` mounts auth with a `Router::layer`, which in axum applies to every
// route the router holds — so "register the probe before the layer" does NOT exempt
// it, and there is a test in the Rust tree saying so. chi's `Use` has the opposite
// property: it applies to routes registered on that router AFTER the call, and to no
// others. So darkroom builds the router in three `chi.Router`s and the exemption is
// structural — the probe router has no auth middleware to remove.
package httpapi

import (
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"log/slog"
	"net/http"
	"strings"

	"github.com/go-chi/chi/v5"

	"github.com/cafaye/darkroom/internal/api"
	"github.com/cafaye/darkroom/internal/auth"
	"github.com/cafaye/darkroom/internal/darkroomdb"
	"github.com/cafaye/darkroom/internal/service"
)

// MaxBodyBytes is the request body ceiling, and it is the document's number.
//
// `openapi/v1.yaml`, `POST /v1/uploads`'s `413`: "The request body exceeded 4096
// bytes." Every request body in this service is a small JSON object naming a file
// the client is about to PUT directly to storage — the bytes never come through
// here — so 4 KiB is generous by an order of magnitude and a bigger limit would
// only move the ceiling for a client sending something malformed.
const MaxBodyBytes = 4096

// Options is what a handler needs to serve a request.
//
// The pair of dependencies is the service's, unchanged: "Dependencies point
// downward only." This package knows `service`, `darkroomdb`, `problem` and
// `auth`; nothing below it knows this package exists.
type Options struct {
	// The state machine. Required.
	Service *service.Service

	// The verifier. Required — there is no unauthenticated mode, because a service
	// whose auth can be switched off is one whose 401 has never been exercised.
	Verifier *auth.Verifier

	// The database, for `GET /readyz`'s `select 1` and nothing else. See
	// [darkroomdb.Ping] for why the probe asks about the connection and not about a
	// policy.
	DB *darkroomdb.Store

	// Where probe failures go. Nil means the default logger. A refusal never goes
	// here — `AGENTS.md`: "Probe failures are logged, never returned in detail."
	Log *slog.Logger
}

// Server is the handler set. It implements [api.ServerInterface], which is the
// compile-time half of the drift check: all nine operations, no more and no fewer,
// because a method that is missing fails the build and a method that is extra is a
// method the document does not describe.
type Server struct {
	service  *service.Service
	verifier *auth.Verifier
	db       *darkroomdb.Store
	log      *slog.Logger
}

// Compile-time assertion, and it is the assertion rather than the comment above it:
// a handler set that stopped satisfying the generated interface would fail here
// with a message naming the method, instead of at the registration site with a
// message about a type.
var _ api.ServerInterface = (*Server)(nil)

// New builds the router.
//
// It returns an `http.Handler` rather than a `*chi.Mux` on purpose: callers get a
// handler and cannot register a route on it, so the only way to change the surface
// is to change the document. `routes_test.go` type-asserts back to `chi.Routes` to
// walk it, and that assertion failing is a finding — see that file.
func New(options Options) (http.Handler, error) {
	if options.Service == nil {
		return nil, fmt.Errorf("httpapi: %w: Service is required", errNotConfigured)
	}
	if options.Verifier == nil {
		// NOT `if options.Verifier == nil { serve everything }`. A service that can
		// be started without a verifier is a service that will be, in some
		// environment, and the first request it serves is the one nobody tested.
		return nil, fmt.Errorf("httpapi: %w: Verifier is required; there is no unauthenticated mode", errNotConfigured)
	}
	log := options.Log
	if log == nil {
		log = slog.Default()
	}

	server := &Server{
		service:  options.Service,
		verifier: options.Verifier,
		db:       options.DB,
		log:      log,
	}

	// The generated router. This is the document, one translation removed, and it is
	// the ONLY place routes are registered — including in [New] below.
	generated := api.HandlerWithOptions(server, api.ChiServerOptions{BaseURL: "/", ErrorHandlerFunc: bindingErrorHandler})
	// The generated handler IS a `*chi.Mux` — `HandlerWithOptions` builds one and
	// returns it as an `http.Handler` — and this assertion is what lets the router be
	// built from the DOCUMENT rather than from a table. If a future generator version
	// returned something else, refusing here is right: a walk that found nothing
	// would leave a router with no routes and every drift test green.
	generatedRoutes, ok := generated.(chi.Routes)
	if !ok {
		return nil, fmt.Errorf("httpapi: %w: the generated router is a %T, which cannot be walked, so the surface could not be built from the document", errNotConfigured, generated)
	}

	root := chi.NewRouter()
	// Outermost: a trace id for EVERY request, including one refused by the body
	// limit below. A refusal with no `X-Trace-Id` is a refusal nobody can quote in a
	// bug report, which is the one job the header has.
	root.Use(traceContext)
	// Then the limit, so an oversized body is refused before a handler reads it and
	// before the verifier reads the header. `src/http.rs` mounts it in the same
	// relative order and says why.
	root.Use(bodyLimit(MaxBodyBytes))
	// Then chi's own 404 and 405, which are not in the document and so are not in
	// the generated router. They get the same envelope as everything else — an
	// unrouted path answered as `text/plain` would be the one response in this
	// service that is not `application/problem+json`.
	root.NotFound(problemNotFound())
	root.MethodNotAllowed(problemMethodNotAllowed())

	// The two groups, and the split between them is the ONLY exemption in this
	// router.
	//
	// The generated router holds all nine operations, so it cannot simply be mounted
	// twice: mounting it whole under each group would SERVE the /v1 routes at both
	// `/v1/uploads` and `/uploads`, and `chi.Walk` reports both spellings — which is
	// how the first run of this file found the bug. So the routes are SPLIT, once,
	// by [APIV1Prefix], and each half is registered on the group that owns it.
	//
	// `routes_test.go` holds that split against the document's `security: []`, so
	// moving a route across the boundary means editing the contract.
	probes, v1 := chi.NewRouter(), chi.NewRouter()
	splitRoutes(generatedRoutes, probes, func(method, pattern string, h http.Handler) {
		// INSIDE the credential gate, and the nesting is the whole point: the layer
		// needs a verified principal to name `principal_key`, and it must not be
		// reachable by an anonymous caller who would otherwise get to burn a key.
		// `guarded(method, pattern, inner)` puts the gate on the OUTSIDE of `inner`,
		// so `inner` has to be this layer — wrapping `guarded` instead would run the
		// ledger first, and `auth.TenantOf` would panic on every anonymous request.
		//
		// And only for the routes the document declares idempotent, which is
		// [idempotentRoutes]' job and whose agreement with `openapi/v1.yaml` is
		// asserted in both directions in `routes_test.go`.
		//
		// The route is named by its DOCUMENT path — `/v1/uploads`, with the prefix —
		// and NOT by `pattern`, which `splitRoutes` has already stripped. Two
		// spellings of the same route in one package is how a list stops matching the
		// code that uses it, so the list, the ledger's `endpoint` column and the
		// migration's own example are all the document's spelling. The endpoint
		// string is derived from the same value, which is what keeps a replay's scope
		// and this list from ever disagreeing.
		endpoint := method + " " + APIV1Prefix + pattern

		inner := h
		if declaresIdempotencyKey(endpoint) {
			inner = server.idempotency(endpoint, h)
		}
		v1.Method(method, pattern, server.guarded(method, pattern, inner))
	})

	// The probe group has NO auth middleware to remove, which is the structural half
	// of the exemption. `src/http.rs` could not do this — `Router::layer` in axum
	// applies to every route the router holds, so "register the probe before the
	// layer" does not exempt it, and the Rust tree has a test saying so.
	root.Mount(APIV1Prefix, v1)
	root.Mount(APIProbesPrefix, probes)

	return root, nil
}

// APIProbesPrefix and APIV1Prefix are the two mount points, and they are the ONLY
// place the exemption boundary is written down.
//
// `openapi/v1.yaml` puts the probes in their own tag with `security: []`, and
// `TestEveryUnauthenticatedRouteIsOneTheDocumentExempts` asserts that the set this
// file exempts is exactly the set the document exempts — so adding an unauthenticated
// route means editing the document, and that test is what notices if someone edits
// this file instead.
//
// The probes are mounted at `/` because they are at the top of the document's own
// paths (`/healthz`, `/readyz`), and mounting them at `/` LAST is what keeps chi's
// tree unambiguous: a `Mount("/")` registered first would swallow everything after
// it.
const (
	APIProbesPrefix = "/"
	APIV1Prefix     = "/v1"
)

// splitRoutes walks the GENERATED router once and sends each route to one of two
// places: `probe` gets everything outside [APIV1Prefix], `guarded` everything inside.
//
// The walk is what makes the surface the document's rather than a table's — see
// [New]. And the split is on the PATH rather than on a list of names, because a list
// of names is a second inventory: a tenth operation added to the document would be
// gated or not gated by which of two lists mentioned it, and both lists would be
// silent about it.
func splitRoutes(generated chi.Routes, probe *chi.Mux, guarded func(method, pattern string, h http.Handler)) {
	walked := 0
	err := chi.Walk(generated, func(method, pattern string, handler http.Handler, _ ...func(http.Handler) http.Handler) error {
		if method == http.MethodConnect {
			// chi's own convention: a route with no pattern, served on the raw
			// connection. Not in this document and not servable through `http.Handler`.
			return nil
		}
		if strings.HasPrefix(pattern, APIV1Prefix) {
			// The PREFIX IS STRIPPED here and added back by the `Mount` above, because
			// chi's `Mount` joins the mount point with the subrouter's own patterns.
			// Registering `/v1/uploads` on a router mounted at `/v1` would serve it at
			// `/v1/v1/uploads` — which is what the first run of `TestNoCredentialIs401`
			// reported, as fifteen 404s instead of fifteen 401s.
			guarded(method, strings.TrimPrefix(pattern, APIV1Prefix), handler)
		} else {
			probe.Method(method, pattern, handler)
		}
		walked++
		return nil
	})
	if err != nil {
		panic(fmt.Sprintf("httpapi: walking the generated router: %v", err))
	}
	if walked == 0 {
		// A router with no routes would make every comparison in routes_test.go pass
		// vacuously. The guard is that the walk must have FOUND something.
		panic("httpapi: the generated router registered no routes, so nothing is served and nothing is gated")
	}
}

// idempotentRoutes are the routes the ledger wraps, as chi patterns.
//
// # WHY THIS IS A LIST AND NOT A WALK OF THE DOCUMENT, STATED PLAINLY
//
// The document is the inventory, and this list is a second copy of one fact about
// it — so `TestTheIdempotentRoutesAreExactlyTheDocumentOnes` in `routes_test.go`
// holds the two against each other in BOTH directions, and that test is what makes
// this acceptable rather than a drift waiting to happen. A route added to
// `openapi/v1.yaml` with `Idempotency-Key` and not to this list fails the suite; a
// route added here that the document does not declare idempotent fails it too.
//
// # WHAT WAS TRIED AND WHY IT IS WORSE
//
// The obvious move is to ask the GENERATED types — reflect over
// `api.ServerInterfaceWrapper`'s methods and look for a `Params` struct with an
// `IdempotencyKey` field, which really would make the generated file the inventory.
// It does not work, and the reason is worth recording: `chi.Walk` hands this package
// a route's `http.Handler`, and by then the generated binder has collapsed
// `CreateUpload`'s method into an anonymous `http.HandlerFunc` closure. There is no
// longer a method value to inspect and no `Params` type to reach from a
// `http.Handler`, so the answer would have to come from the handler's runtime type —
// which is `http.HandlerFunc` for all nine operations, identically.
//
// Getting the same answer from the generated SOURCE by reading `api.gen.go` as text
// is what `AGENTS.md` warns against: the Rust tree held an `account_id` predicate
// in place with `include_str!`, and a text guard pins a SPELLING rather than a
// behavior, so it breaks on a harmless reformat and passes on a real divergence
// spelled differently.
//
// So the list is here, it is tiny, and a test in `routes_test.go` — which already
// reads `openapi/v1.yaml` and compares it to this router — is what keeps it honest.
var idempotentRoutes = map[string]bool{
	http.MethodPost + " /v1/uploads":               true,
	http.MethodPost + " /v1/uploads/{id}/complete": true,
}

// declaresIdempotencyKey reports whether the ledger wraps this operation.
//
// `operation` is the document's `METHOD /v1/path`, which is what
// [idempotentRoutes] is keyed by and what the ledger's `endpoint` column stores.
func declaresIdempotencyKey(operation string) bool {
	return idempotentRoutes[operation]
}

// DeclaresIdempotencyKey is [declaresIdempotencyKey] exported, for the one caller
// outside this package: the drift test that holds this list against
// `openapi/v1.yaml`.
//
// Exported rather than tested through the router because the layer is a middleware
// and a middleware is invisible from outside — so the only way to ask "is this route
// wrapped" is to ask the code that wraps it. That is a weaker check than the
// behavior in `idempotency_test.go`, and both exist: the behavior proves the two
// declared routes work, and this proves there are exactly two.
func DeclaresIdempotencyKey(operation string) bool {
	return declaresIdempotencyKey(operation)
}

// errNotConfigured is a configuration this package cannot serve. A sentinel, so a
// caller can tell its own mistake from a defect here.
var errNotConfigured = fmt.Errorf("httpapi: invalid configuration")

// guarded is one /v1 route's two gates, in the only order that can be right: the
// CREDENTIAL first, then the CAPABILITY.
//
// The order is the contract. `RequireScope` answers 401 for a missing principal
// precisely so that a scope gate with nothing in front of it is a wiring mistake
// rather than a decision — and it can only answer 401 because the credential gate is
// mounted first. Swapping them turns every unauthenticated request on a write route
// into a 403, which tells an anonymous caller that the route exists and what it
// needs.
//
// The scope comes from the METHOD. `openapi/v1.yaml` declares two scopes under
// `x-required-scopes` — read and write — and does NOT say per operation which one an
// operation needs; the service's answer is that a read is a read and a write is a
// write, which is what `src/http.rs` decided by hand at each of its nine
// `require_scope(&principal, write)` calls. It lives here rather than in nine
// arguments so there is one place to look, and `TestTheScopesAreTheDocumentOnes`
// holds the two constants against the document so a rename there is red here rather
// than every caller receiving a 403 naming a scope that no longer exists.
func (s *Server) guarded(method, pattern string, h http.Handler) http.Handler {
	scope, err := scopeFor(method)
	if err != nil {
		// Unreachable while the document carries nine operations, and a PANIC rather
		// than a silent skip because a skipped route is a route that stopped existing.
		// Failing at CONSTRUCTION means it is a startup failure, not a 404 in
		// production that nobody notices until a client reports it.
		panic(fmt.Sprintf("httpapi: %v for the document's %s %s", err, method, pattern))
	}

	scopeGate, err := s.verifier.RequireScope(refuse, scope)
	if err != nil {
		// The scope constant is empty, which is a defect in this file and not a
		// decision. Same reasoning as the panic above.
		panic(fmt.Sprintf("httpapi: RequireScope for %s %s: %v", method, pattern, err))
	}

	return s.verifier.Require(refuse)(scopeGate(h))
}

// scopeFor is the method-to-scope table, and the whole of it.
//
// It is a `switch` and not a `map` so that an unhandled method is a COMPILE
// exhaustiveness question a reader can answer in one screen rather than a silent
// zero value from a map that did not have the key.
func scopeFor(method string) (string, error) {
	switch method {
	case http.MethodGet:
		return auth.ScopeRead, nil
	case http.MethodPost, http.MethodDelete:
		return auth.ScopeWrite, nil
	default:
		return "", fmt.Errorf("no scope is declared for %s", method)
	}
}

// traceContext is the request-id layer.
//
// Two jobs, and the second is the one that is easy to get wrong:
//
//  1. Echo `X-Request-Id` when the caller sent one, so a client that already has a
//     correlation id keeps it.
//  2. Mint one otherwise, ALWAYS. There is no request this service answers without
//     an id, because core's conventions say `trace_id` "is always present and always
//     matches the `X-Trace-Id` response header", and a problem body rendered by a
//     handler that never called this would have nothing to put in it.
//
// It is `Middleware` from the conventions and it is the OUTERMOST layer, so the id
// exists before anything can refuse — including the body limit.
func traceContext(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		id := strings.TrimSpace(r.Header.Get("X-Request-Id"))
		if id == "" || len(id) > 128 {
			// Over-long is replaced rather than truncated: a caller-chosen id is echoed
			// verbatim or not at all, and a truncated id is a DIFFERENT id that still
			// looks like the caller's. The bound is 128 because an id is a log key and
			// an unbounded one is a log-injection vector.
			id = newTraceID()
		}

		w.Header().Set("X-Trace-Id", id)
		next.ServeHTTP(w, r.WithContext(withTraceID(r.Context(), id)))
	})
}

// bodyLimit is the 413 ceiling.
//
// `http.MaxBytesReader` rather than chi's `middleware.MaxBody`, because the ANSWER
// matters more than the refusal: MaxBytesReader makes the reader fail, and what
// turns that failure into a document is a handler. Here the layer answers 413
// itself, so a body one byte over the limit is a problem document from the same
// writer as every other refusal, and a handler never sees a truncated body it might
// try to parse.
func bodyLimit(limit int64) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if r.ContentLength > limit {
				writeStatus(w, r, http.StatusRequestEntityTooLarge, problemPayloadTooLarge(limit))
				return
			}
			r.Body = http.MaxBytesReader(w, r.Body, limit)
			next.ServeHTTP(w, r)
		})
	}
}

// newTraceID is 32 hex characters, the shape core's conventions show. `panic` on a
// `crypto/rand` failure because an id that is not unique costs support its search
// and this is the only place the uniqueness comes from.
func newTraceID() string {
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		panic("httpapi: crypto/rand is unavailable: " + err.Error())
	}
	return hex.EncodeToString(b[:])
}
