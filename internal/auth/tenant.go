package auth

import (
	"fmt"
	"net/http"

	"github.com/cafaye/darkroom/internal/service"
)

// Tenant is the account a request acts on. The ONLY way to scope a query.
//
// # WHAT GO CAN EXPRESS, AND WHAT IT CANNOT
//
// The Rust tree's guarantee was the type system. `src/auth.rs` has a `Tenant` with
// exactly one constructor, that constructor takes a verified `Principal`, and a
// repository method that does not take a `Tenant` DOES NOT COMPILE. `AGENTS.md`
// states the consequence directly: "A repository method that does not take a
// `Tenant` will not typecheck, which is the point."
//
// Go has no dependent types, so that exact guarantee is not expressible and it would
// be dishonest to write a comment claiming otherwise. What IS expressible, and is
// here, is the half that matters at the trust boundary:
//
//   - The fields are UNEXPORTED, and there is one constructor, and it takes a
//     `*Principal`. So `Tenant{AccountID: idFromARequestBody}` does not compile
//     anywhere outside this file, and neither does a cast. `openapi/v1.yaml` says
//     "`account_id` is the tenancy boundary and is taken from this token and from
//     nowhere else", and this is the Go shape of that sentence.
//
// The half Go cannot express is the MANDATORY part — that every handler obtains
// one. That is [TenantOf]'s job, and it is the design decision this packet asked
// to make explicitly rather than leave to a reader.
//
// # HOW THE FORGET-THE-TENANT CASE BEHAVES: A PANIC, AND IT IS TESTED
//
// `TenantOf` PANICS when there is no verified principal. Not a nil `*Tenant`, not a
// zero `Tenant` with an empty account, and not a 500 written on the spot.
//
// The three alternatives and why each is worse:
//
//   - **A nil pointer returned by the accessor.** This is what
//     `../cafaye/gateway` does and its own comment is honest about the cost: "a
//     handler which forgets to read the principal gets a nil pointer", which is why
//     `gateway/internal/httpapi/principal.go` hand-writes a nil check for a state
//     "that can't happen". A nil that a handler forgets to check IS the nil-deref
//     deep in a query the packet rules out. The check it needs exists in the
//     handler, which is the one place that will not be written.
//   - **A zero `Tenant` with an empty account.** Strictly worse than nil: nil
//     crashes, and a zero account is a VALID account to `InAccountTx`. It would
//     scope a query to `account_id = ”`, and with FORCE ROW LEVEL SECURITY
//     `current_account_id()` returning `”` rather than NULL matches no rows rather
//     than all of them — so every read would be empty and every write would report
//     success having changed nothing. That is the failure `AGENTS.md` calls "a
//     cleanup or a query that reports success and did nothing". A silent
//     cross-tenant-shaped hole, from a handler that forgot one line.
//   - **A 500 written by the accessor.** This one is tempting and it is the worst of
//     the three, because it CONVERGES. The mistake becomes a documented behaviour
//     of the accessor, a test asserts the 500, and the gate that would have caught
//     a missing tenant has been replaced by a handler that returns a tidy error for
//     a wiring defect. The process keeps serving traffic built on an unscoped read.
//
// A panic is the only one of the four that cannot be mistaken for an answer:
//
//   - In a TEST it fails the test, at the line of the missing call, with a message
//     naming the route. A nil does not, and neither does a 500.
//   - In PRODUCTION `net/http`'s per-connection recover logs the stack and drops
//     THAT connection. A drop is visible; a 500 with a request id is not.
//
// Two things keep the panic honest, and without both it is just a crash:
//
//  1. It is not reachable through the router, because every route a client can
//     reach is behind [Verifier.Require]. `TestEveryRouteIsBehindAGate` holds that,
//     and `TestAMissingTenantPanicsInsteadOfScopingToNothing` shows the panic
//     happening when the gate IS bypassed.
//  2. The panic message names the route and says what to do, so the log line is
//     the fix rather than a mystery. `TestThePanicMessageNamesTheRoute` asserts
//     that, because a panic with a useless message is a panic people route around
//     by adding a nil check.
//
// # AND THE THIRD THING, WHICH IS THE REAL ANSWER
//
// A panic catches the mistake when it happens. The STRUCTURAL guard is that a
// handler is not handed `*http.Request` and left to find its own scope: every
// `internal/httpapi` handler calls [TenantOf] as its first statement, and the
// generated `ServerInterface` gives every operation a signature. The half that
// cannot fail is `InAccountTx`: a service method that forgot its caller would not
// compile either, because there is no other way to reach the database.
type Tenant struct {
	accountID string
	userID    string
}

// NewTenant is the only constructor, and it takes a verified principal.
//
// Exported because `internal/httpapi` needs it — for a handler that already holds
// the `*Principal` the gate put on the request — and unexported-FIELDS because
// there is no second way in. Every caller of this function has a token it verified;
// a caller that has only a string has an `id` from a body and must not be able to
// arrive here.
func NewTenant(principal *Principal) Tenant {
	if principal == nil {
		// The constructor that cannot be trusted to be given a principal refuses
		// rather than minting an empty account. Same reason as [TenantOf]: an empty
		// account is a real value to `InAccountTx` and an empty one matches nothing.
		panic("auth: NewTenant was given no principal; a Tenant cannot be built from a nil Principal")
	}
	return Tenant{accountID: principal.AccountID, userID: principal.Sub}
}

// AccountID is the tenancy boundary, and the only value any query may be scoped by.
func (t Tenant) AccountID() string { return t.accountID }

// UserID is `sub`, recorded as `owner_user_id` and never used to scope anything.
func (t Tenant) UserID() string { return t.userID }

// Caller is the service's view of a tenant: the account that OWNS rows and the user
// who asked.
//
// A method rather than a function because it is the same fact the tenant carries
// and a second translation between the two is a second place they could differ.
func (t Tenant) Caller() service.Caller {
	return service.Caller{AccountID: t.accountID, UserID: t.userID}
}

// TenantOf is the tenant for this request, and it PANICS if there is none.
//
// # WHY A PANIC, STATED ONCE AND IN FULL
//
// The packet asks for one decision here and this is it. See the type comment above
// for all three rejected alternatives with the failure each one produces; the short
// version is that **an empty account is not a safe default**, because
// `migrations/0004_account_isolation.sql` gives FORCE ROW LEVEL SECURITY and
// `InAccountTx` with an empty account scopes a statement to no rows while every
// write still exits 0. "The rule is absence, not refusal" cuts the other way here:
// the rule is that an unscoped statement asserts rows exist rather than that it saw
// them, and `AGENTS.md` says to check the identity before anything else.
//
// So this function has two states and two outcomes, and the failing one is loud on
// purpose:
//
//	TenantOf(r) → verified tenant
//	TenantOf(r) → panic naming the route
//
// The panic is a programming error, not an input, which is why it is not a problem
// document: RFC 9457 has no code for "this deployment is wired wrong", and
// inventing one would give the defect a documented shape.
func TenantOf(r *http.Request) Tenant {
	principal := PrincipalOf(r)
	if principal == nil {
		panic(fmt.Sprintf(
			"auth: TenantOf: no verified principal on this request, so there is no tenant to "+
				"scope it to. %s %s is reachable without this package's Require gate: either the "+
				"route is mounted outside it, or it was mounted after it in the middleware order. "+
				"A tenant is never inferred, defaulted or read from the request — a handler that "+
				"needs an account calls TenantOf and that panicking is the bug.",
			r.Method, r.URL.Path))
	}

	tenant := NewTenant(principal)
	if tenant.accountID == "" {
		// Unreachable through the gate: `checkClaims` refuses a token with no
		// `account_id`, and `Authenticate` is the only writer of a principal. It is
		// here because this is the one function whose failure mode is a SILENT
		// cross-tenant read, and "unreachable" is a claim about code in another
		// file.
		panic(fmt.Sprintf(
			"auth: TenantOf: the verified principal carries no account_id, so %s %s cannot be "+
				"scoped. Do not default this to the empty string: InAccountTx with an empty account "+
				"matches no rows and every write still exits 0.",
			r.Method, r.URL.Path))
	}
	return tenant
}
