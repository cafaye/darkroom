package api

// The generation wiring, and the two questions a reader has about a generated
// tree answered here rather than in a document nobody is required to read.
//
// # The pin
//
// oapi-codegen v2.8.0, pinned to an exact version on the line below rather than
// in oapi-codegen.yaml. `go run <module>@<version>` resolves in module-aware
// mode and ignores this module's go.mod, so the generator is a build tool
// invoked on purpose and cannot be moved by `go get`.
//
// # The document is this repository's own, and it does not change in packet 05
//
// `openapi/v1.yaml` is the contract `anywaye.yml`'s `exposes.api` points at, and
// the packet's rule is that it is the contract, not an artifact of this packet.
// There is no vendored copy and no provenance file: the generated code and the
// document sit in the same commit, so the only way they can disagree is a
// regeneration that was not committed.
//
// The consequence is stated rather than hidden: **regeneration is only correct
// immediately after the document changes, in the same commit.** Adding an
// operation is a document edit and a `go generate` in one commit, and
// `internal/httpapi`'s drift test is what notices if the second half is missing.
//
// # WHAT IS EDITED IN THE GENERATED FILE, AND WHY IT IS NOT A DRIFT
//
// The second `//go:generate` line below runs `internal/api/stripbaseurl`, which
// removes the `options.BaseURL+` token from the route registrations. darkroom
// serves at `/` and nowhere else — the document declares no `servers:` block —
// so `options.BaseURL` is `/` and the rewrite changes no behavior. It is here
// because core's `account-scope-check` requires a `/`-prefixed string literal as
// the first term after `(` and concatenation is invisible to it: without this
// step `route_defs` is 0 over a surface it saw none of, and 0 over nothing
// prints exactly like 0 over everything. Read that command's own header before
// touching `api.gen.go`.
var _ = struct{}{}

//go:generate go run github.com/oapi-codegen/oapi-codegen/v2/cmd/oapi-codegen@v2.8.0 --config oapi-codegen.yaml -generate models,chi-server ../../openapi/v1.yaml
//go:generate go run ./stripbaseurl
