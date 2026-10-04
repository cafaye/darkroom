// Command stripbaseurl removes the `options.BaseURL+` token from the route
// registrations oapi-codegen emits, and is the reason `go generate` has two
// lines in generate.go rather than one.
//
// # Why the token is removed at all
//
// `oapi-codegen` emits every route as
//
//	r.Get(options.BaseURL+"/v1/me", wrapper.GetCurrentPrincipal)
//
// and core's contract harness cannot read that line. `RECOGNISERS`' chi entry
// requires a `/`-prefixed string literal as the FIRST term after `(`, and
// concatenation is resolved only in the other direction — `"/v1/x/"+prefix+"/y"`
// — because identity's own router writes paths that way. So every route in a
// generated file is INVISIBLE to `account-scope-check`.
//
// The consequence is not a missed route. It is a number: `route_defs` is 0 over
// a surface it saw none of, and 0 over nothing prints exactly like 0 over
// everything. `surface.minimum` is the one field in `account_scope.yml` that
// notices a route appearing where the recognisers cannot reach, so with the
// token left in place darkroom's only decrease-detector cannot be set above
// zero — which is where pantry's sits, with a DECISION NEEDED attached.
//
// `REPORT-pantry-account-scope-01.md` §3 records the other half of this: on
// pantry, deleting the token from four lines took the run from 1 failure and 5
// warnings to 0 and 1, because `route_defs` went from 0 to 6 and every
// declaration in that file became matchable. This command does it for the whole
// generated tree instead of by hand, because `bin/prime-go` regenerates and
// diffs: a hand edit would be a permanently red gate, and the honest way to
// carry a mechanical post-processing step is to make it part of generation.
//
// # What it changes, and what it does not
//
// Only the token. `options.BaseURL` defaults to `/` when a caller leaves it
// empty, so `options.BaseURL+"/v1/me"` and `"/v1/me"` are the same route — the
// rewrite changes no behaviour, and darkroom serves at `/` and nowhere else
// (`internal/httpapi/routes_test.go` holds the router to the document's paths).
//
// # It refuses rather than reporting success it did not achieve
//
// A generator that stops emitting the token — or emits it spelled differently —
// leaves this command with nothing to rewrite. Exiting 0 there would be a green
// that says "the registrations are readable" while the file still says
// `options.BaseURL+`, which is the exact failure this exists to remove. So zero
// rewrites is an error naming the file, and so is a token that is still present
// after the rewrite.
package main

import (
	"fmt"
	"os"
	"strings"
)

const (
	// The file `go generate` is running over, relative to its working directory
	// (`internal/api/`).
	generated = "api.gen.go"

	// The token, and what it is rewritten to.
	token = "options.BaseURL+"
	drop  = ""

	// The header comment prepended to the file, so a reader who opens a file
	// that says DO NOT EDIT and finds a line that is not the generator's has an
	// answer on the first screen rather than in this comment.
	note = `// POST-PROCESSED BY internal/api/stripbaseurl, run by the second //go:generate
// line above. The route registrations below read ` + "`" + `r.Get("/healthz", …)` + "`" + ` rather
// than the generator's ` + "`" + `r.Get(options.BaseURL+"/healthz", …)` + "`" + ` so that
// core's account-scope checker can read them; see that command's own header. The
// token and ` + "`" + `options.BaseURL` + "`" + ` default to ` + "`" + `/` + "`" + ` when a caller leaves them
// empty, so the routes are the same routes. Regeneration rewrites the change, and
// this step puts it back.
`
)

func main() {
	source, err := os.ReadFile(generated)
	if err != nil {
		fmt.Fprintf(os.Stderr, "stripbaseurl: cannot read %s: %v\n", generated, err)
		os.Exit(1)
	}

	rewritten := strings.ReplaceAll(string(source), token, drop)
	count := strings.Count(string(source), token)
	if count == 0 {
		fmt.Fprintf(os.Stderr,
			"stripbaseurl: %s contains no %q, so this command achieved nothing and the\n"+
				"  routes in it are still invisible to core's account-scope checker. Either the\n"+
				"  generator changed how it spells the base URL, or this command is no longer\n"+
				"  the right place to remove it. Do not delete this step to make the gate green:\n"+
				"  find out which of the two it is first.\n", generated, token)
		os.Exit(1)
	}

	if strings.Contains(rewritten, token) {
		fmt.Fprintf(os.Stderr, "stripbaseurl: %s still contains %q after rewriting it.\n", generated, token)
		os.Exit(1)
	}

	// The note goes ABOVE the generator's own header, which says `DO NOT EDIT` and
	// names a version. Putting this under it reads as a second, competing claim on
	// the file; putting it above means the first line a reader sees is the one
	// that explains why the file is not byte-identical to the generator's output.
	if !strings.HasPrefix(rewritten, "// POST-PROCESSED BY") {
		rewritten = note + rewritten
	}

	if err := os.WriteFile(generated, []byte(rewritten), 0o644); err != nil {
		fmt.Fprintf(os.Stderr, "stripbaseurl: cannot write %s: %v\n", generated, err)
		os.Exit(1)
	}

	fmt.Printf("stripbaseurl: rewrote %d route registration(s) in %s\n", count, generated)
}
