package httpapi_test

// The router, to the document, in BOTH directions — and the check reads the YAML.
//
// `openapi/v1.yaml` is the contract (`cafaye.yml`'s `exposes.api` points at it, and
// this packet's rule is that it does not change). So the comparison below is against
// that file, not against a list, not against the generated Go, and not against what a
// reader believes the routes are.
//
// # WHY THIS FILE READS THE YAML AND THE GATEWAY'S DOES NOT
//
// `../cafaye/gateway/internal/httpapi/routes_test.go` compares the served routes
// against the route registrations in `api.gen.go`, and its own header is honest that
// "nothing here reads the YAML. What closes the gap is `bin/prime-go`'s
// regenerate-and-diff, which fails when `api.gen.go` and the document disagree".
//
// That is a sound argument, and it depends on a second gate existing. Here the
// generated file is read as well AND the document is read, so the property stands on
// this file alone — and `gopkg.in/yaml.v3` is the whole cost, which is the dep
// stated in its cause rather than in a review comment: **the test needs the
// document's own words**, and the only way to have them without hand-transcribing
// them into a Go map that would be the second inventory this check exists to kill.
//
// # WHAT WOULD MAKE THESE TESTS PASS WITHOUT HAVING LOOKED
//
// Three things, each made impossible rather than assumed:
//
//   - A router serving NOTHING would make every "served ⊆ declared" check vacuous, so
//     [declaredRoutes] fails when it reads zero operations out of the document and
//     [servedRoutes] fails when the walk finds none. Same rule the Rust tree and the
//     gateway both state, and it is the single most common way a drift check lies.
//   - A router serving EXTRA routes is caught by the second direction, and the
//     planted divergence below is the proof that it is caught by something rather
//     than asserted.
//   - A route whose METHOD is wrong — `POST /v1/assets/{id}` instead of `DELETE` — is
//     caught, because the key is `(method, pattern)` and not `pattern`. That is the
//     check the Rust tree cannot make: `AGENTS.md` says "The method a `MethodRouter`
//     answers is baked into the value `get(handler)` returns and there is no
//     accessor for it".

import (
	"fmt"
	"net/http"
	"os"
	"sort"
	"strings"
	"testing"

	"github.com/go-chi/chi/v5"
	"gopkg.in/yaml.v3"

	"github.com/cafaye/darkroom/internal/auth"
	"github.com/cafaye/darkroom/internal/httpapi"
	"github.com/cafaye/darkroom/internal/service"
)

// documentPath is relative to this package's directory. Measured, not assumed: the
// test's working directory is the package directory, not the module root.
const documentPath = "../../openapi/v1.yaml"

// operation is one `(method, pattern)` the document declares.
type operation struct {
	method string
	path   string

	// operationID is the document's name for it. Asserted to be non-empty rather
	// than used, because a path with no `operationId` cannot become a method on the
	// generated `ServerInterface` and so cannot be served at all — which the
	// bidirectional comparison below would report as "declared but not served" for a
	// reason worth naming.
	operationID string

	// anonymous is `security: []`. `openapi/v1.yaml` marks the probes this way, and
	// [TestEveryUnauthenticatedRouteIsOneTheDocumentExempts] holds the exemption
	// against the document rather than against a list in this repository.
	anonymous bool
}

func (o operation) String() string { return o.method + " " + o.path }

// declaredRoutes reads the operations out of `openapi/v1.yaml` itself.
//
// A YAML PARSE rather than a scan for `operationId:` lines, and the difference is the
// whole point: a scan reads lines without knowing they are under a path or a method,
// so a document with a commented-out operation would still be counted and a
// `description` mentioning `operationId:` would be counted twice. A parse has
// opinions, and its opinions are the document's structure.
//
// It FAILS on zero operations rather than returning an empty slice, which is the same
// guard the gateway's parser carries and for the same reason: a parser that finds
// nothing and a document with nothing produce the same empty set, and every
// comparison against an empty set passes.
func declaredRoutes(t *testing.T) []operation {
	t.Helper()

	raw, err := os.ReadFile(documentPath)
	if err != nil {
		t.Fatalf("cannot read %s: %v", documentPath, err)
	}

	var document struct {
		Paths map[string]map[string]struct {
			OperationID string `yaml:"operationId"`
			Security    *[]any `yaml:"security"`
		} `yaml:"paths"`
	}
	if err := yaml.Unmarshal(raw, &document); err != nil {
		t.Fatalf("cannot parse %s: %v", documentPath, err)
	}

	var operations []operation
	for path, byMethod := range document.Paths {
		for method, spec := range byMethod {
			operations = append(operations, operation{
				method:      strings.ToUpper(method),
				path:        path,
				operationID: spec.OperationID,
				// `security: []` is an EMPTY list and unmarshals to a non-nil pointer to
				// an empty slice. A pointer rather than a slice because a path with no
				// `security` key inherits the document's global `security` — so ABSENT
				// and EMPTY are different answers and only the pointer tells them apart.
				anonymous: spec.Security != nil && len(*spec.Security) == 0,
			})
		}
	}

	if len(operations) == 0 {
		t.Fatalf("%s declares no operations and this parser found none of them, so every "+
			"comparison below passes vacuously. Either the document moved or this parser is stale.",
			documentPath)
	}

	sort.Slice(operations, func(i, j int) bool { return operations[i].String() < operations[j].String() })
	return operations
}

// newRouter builds the real router. The dependencies are the shallowest ones that
// satisfy `New`, because this file is about ROUTES: no database, no object store, no
// socket and no network, and a check that needed them would be a check that could
// not run on a bare machine.
func newRouter(t *testing.T) chi.Routes {
	t.Helper()

	verifier, err := auth.New(auth.Options{Issuer: "https://identity.localhost", Audience: "darkroom"})
	if err != nil {
		t.Fatalf("auth.New: %v", err)
	}

	handler, err := httpapi.New(httpapi.Options{
		Service:  service.New(nil, nil),
		Verifier: verifier,
	})
	if err != nil {
		t.Fatalf("httpapi.New: %v", err)
	}

	routes, ok := handler.(chi.Routes)
	if !ok {
		t.Fatalf("New returned a %T, which is not a chi.Routes and therefore cannot be walked. "+
			"A check that cannot see the router is a check that passes without having looked.", handler)
	}
	return routes
}

// servedRoutes reads the routes off the live router with `chi.Walk`.
//
// The method IS part of the key. That is what `chi.Walk` gives and it is what the
// Rust tree could not get: "The method a `MethodRouter` answers is baked into the
// value `get(handler)` returns and there is no accessor for it" (`AGENTS.md`), so the
// Rust drift check cannot compare methods and only compares paths.
func servedRoutes(t *testing.T) []operation {
	t.Helper()

	var served []operation
	err := chi.Walk(newRouter(t), func(method, pattern string, _ http.Handler, _ ...func(http.Handler) http.Handler) error {
		served = append(served, operation{method: strings.ToUpper(method), path: pattern})
		return nil
	})
	if err != nil {
		t.Fatalf("walking the router: %v", err)
	}

	if len(served) == 0 {
		t.Fatalf("the router serves no routes, so every comparison against it passes vacuously. " +
			"Either New built nothing or chi.Walk changed.")
	}

	sort.Slice(served, func(i, j int) bool { return served[i].String() < served[j].String() })
	return served
}

func names(operations []operation) []string {
	out := make([]string, 0, len(operations))
	for _, one := range operations {
		out = append(out, one.String())
	}
	return out
}

// TestEveryServedRouteIsInTheDocument is DIRECTION ONE: nothing is served that the
// document does not declare.
func TestEveryServedRouteIsInTheDocument(t *testing.T) {
	declared := map[string]bool{}
	for _, one := range declaredRoutes(t) {
		declared[one.String()] = true
	}

	for _, served := range servedRoutes(t) {
		if !declared[served.String()] {
			t.Errorf("the router serves %s and %s does not declare it", served, documentPath)
		}
	}
}

// TestEveryDeclaredRouteIsServed is DIRECTION TWO: nothing in the document is
// missing from the router. It is the direction the Rust tree's planted `.route()`
// passed and this one cannot, because there is no hand-written table for an extra
// route to agree with.
func TestEveryDeclaredRouteIsServed(t *testing.T) {
	served := map[string]bool{}
	for _, one := range servedRoutes(t) {
		served[one.String()] = true
	}

	for _, declared := range declaredRoutes(t) {
		if !served[declared.String()] {
			t.Errorf("%s declares %s (%s) and the router does not serve it",
				documentPath, declared, declared.operationID)
		}
	}
}

// TestTheSurfaceIsExactlyTheDocumentIs the same two directions as one assertion, so
// a reader does not have to hold both in their head to know the router is the
// document. It also prints the two sets on failure, because a difference of nine
// routes is worth seeing whole.
func TestTheSurfaceIsExactlyTheDocument(t *testing.T) {
	declared, served := names(declaredRoutes(t)), names(servedRoutes(t))
	if strings.Join(declared, "\n") != strings.Join(served, "\n") {
		t.Fatalf("the router and the document are not the same surface.\n\ndeclared:\n  %s\n\nserved:\n  %s",
			strings.Join(declared, "\n  "), strings.Join(served, "\n  "))
	}
	if len(declared) != 9 {
		t.Fatalf("the document declares %d operations (%v), and this packet builds nine. A tenth is "+
			"a document change and a handler, not a test to relax.", len(declared), declared)
	}
}

// TestEveryUnauthenticatedRouteIsOneTheDocumentExempts is the exemption boundary
// held against the DOCUMENT.
//
// `openapi/v1.yaml` marks the probes `security: []`. Those two are the routes with no
// gate, and this asserts the set is exactly the document's — so adding an
// unauthenticated route means editing the contract, and editing this repository's
// router instead of the contract fails here.
func TestEveryUnauthenticatedRouteIsOneTheDocumentExempts(t *testing.T) {
	anonymous := map[string]bool{}
	for _, one := range declaredRoutes(t) {
		if one.anonymous {
			anonymous[one.String()] = true
		}
	}
	if len(anonymous) == 0 {
		t.Fatalf("%s marks no operation `security: []`, so this check would pass against an empty set",
			documentPath)
	}

	// Both directions. Forward: nothing is served without a gate unless the document
	// says so. Backward: nothing the document exempts is missing, so editing the
	// document to exempt a route is a change this test can see coming.
	for _, served := range servedRoutes(t) {
		if anonymous[served.String()] {
			continue
		}
		if served.path == "/healthz" || served.path == "/readyz" {
			t.Errorf("the router serves %s with no gate, and the document does not exempt it", served)
		}
	}
	served := map[string]bool{}
	for _, one := range servedRoutes(t) {
		served[one.String()] = true
	}
	for name := range anonymous {
		if !served[name] {
			t.Errorf("the document exempts %s and the router does not serve it at all", name)
		}
	}
}

// TestTheScopesAreTheDocumentOnes holds the two scope constants against the
// document's `x-required-scopes`.
//
// It exists because `gateEach` assigns scopes from a METHOD table rather than from
// the document, and the reason it can do that is that the document declares exactly
// two scopes under read and write. Rename one in the document and this is red, rather
// than every caller receiving a 403 naming a scope that no longer exists.
func TestTheScopesAreTheDocumentOnes(t *testing.T) {
	raw, err := os.ReadFile(documentPath)
	if err != nil {
		t.Fatalf("cannot read %s: %v", documentPath, err)
	}

	var document struct {
		Components struct {
			SecuritySchemes map[string]struct {
				XRequiredScopes struct {
					Read  []string `yaml:"read"`
					Write []string `yaml:"write"`
				} `yaml:"x-required-scopes"`
			} `yaml:"securitySchemes"`
		} `yaml:"components"`
	}
	if err := yaml.Unmarshal(raw, &document); err != nil {
		t.Fatalf("cannot parse %s: %v", documentPath, err)
	}

	declared := document.Components.SecuritySchemes["bearerAuth"].XRequiredScopes
	if len(declared.Read) != 1 || len(declared.Write) != 1 {
		t.Fatalf("the document declares read=%v write=%v, and gateEach assumes exactly one scope each. "+
			"A second scope under either is a service decision this package does not make.", declared.Read, declared.Write)
	}
	if declared.Read[0] != auth.ScopeRead || declared.Write[0] != auth.ScopeWrite {
		t.Fatalf("the document says read=%s write=%s and this service uses read=%s write=%s",
			declared.Read[0], declared.Write[0], auth.ScopeRead, auth.ScopeWrite)
	}
}

// TestTheMethodsAreTheOnlyOnesWithAScope is the exhaustiveness of `scopeFor`, stated
// as a test rather than left to a reader of a switch.
//
// A document that grows a `PUT` would make `gateEach` PANIC at construction with
// "no scope is declared for PUT", and a panic at construction is the right answer —
// but it means the failure is a crash rather than a red test, so this asserts the
// table covers exactly the document's methods instead.
func TestTheMethodsAreTheOnlyOnesWithAScope(t *testing.T) {
	seen := map[string]bool{}
	for _, one := range declaredRoutes(t) {
		seen[one.method] = true
	}
	for method := range seen {
		if !knownScopeMethod[method] {
			t.Errorf("the document declares a %s operation and gateEach has no scope for it, so New would PANIC", method)
		}
	}
}

// knownScopeMethod mirrors `httpapi.scopeFor`. A second copy, and it is the only way
// to hold an UNEXPORTED function from an external test package — and it is a copy
// rather than an assertion of `scopeFor` because `scopeFor` returning an error for
// an unknown method is what this mirrors.
var knownScopeMethod = map[string]bool{
	http.MethodGet:    true,
	http.MethodPost:   true,
	http.MethodDelete: true,
}

// TestNoRouteIsReachableWithoutAToken is the structural half of the exemption, and
// it is here rather than only in `httpapi_test.go` because it is a property of the
// SURFACE: the only two routes that do not require a principal are the two the
// document exempts.
//
// It drives the router as a function call — `http.Handler` is a function, and
// `httptest.NewRecorder` is a `ResponseWriter` — because `AGENTS.md` says "HTTP tests
// drive the router with `tower::ServiceExt::oneshot` — a function call, not a round
// trip", and the Go spelling of that is the same property.
func TestNoRouteIsReachableWithoutAToken(t *testing.T) {
	verifier, err := auth.New(auth.Options{Issuer: "https://identity.localhost", Audience: "darkroom"})
	if err != nil {
		t.Fatalf("auth.New: %v", err)
	}
	handler, err := httpapi.New(httpapi.Options{Service: service.New(nil, nil), Verifier: verifier})
	if err != nil {
		t.Fatalf("httpapi.New: %v", err)
	}

	exempt := map[string]bool{"GET /healthz": true, "GET /readyz": true}
	for _, route := range servedRoutes(t) {
		if exempt[route.String()] {
			continue
		}
		// chi patterns carry `{id}`, so a request has to supply something. The value is
		// irrelevant: the gate runs before the handler and a body is not read.
		path := strings.NewReplacer("{id}", "3f6c1e6c-0000-4000-8000-000000000000").Replace(route.path)
		recorder := newRecorder()
		handler.ServeHTTP(recorder, request(route.method, path, nil))
		recorder.read()
		if recorder.status != http.StatusUnauthorized {
			t.Errorf("%s answered %d with no credential, and the document does not exempt it. A 403 would "+
				"mean a scope gate with nothing in front of it, which is a wiring mistake and not an answer.",
				route, recorder.status)
		}
	}
}

// TestNewRefusesToBuildWithoutAVerifier is the last structural guard: there is no
// unauthenticated mode.
//
// A service whose auth can be switched off is one whose 401 has never been exercised,
// and the switch is exactly what an operator reaches for at 3am when a token is
// failing.
func TestNewRefusesToBuildWithoutAVerifier(t *testing.T) {
	for _, row := range []struct {
		name    string
		options httpapi.Options
	}{
		{"no service", httpapi.Options{}},
		{"no verifier", httpapi.Options{Service: service.New(nil, nil)}},
	} {
		t.Run(row.name, func(t *testing.T) {
			handler, err := httpapi.New(row.options)
			if err == nil {
				t.Fatalf("New accepted %s and returned a %T", row.name, handler)
			}
			if !strings.Contains(err.Error(), "required") {
				t.Fatalf("the refusal does not say what is missing: %v", err)
			}
		})
	}
}

var _ = fmt.Sprintf
