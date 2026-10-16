// Package cibackend pins the CI backend switch to a backend kit ships.
//
// `ci/backend` is the one-line switch kit's `backends/README.md` defines:
// `instabox` runs the pipeline in a sandbox, `github-actions` keeps
// `.github/workflows/ci.yml` as the runner. A value kit does not ship is a
// pipeline no backend will ever pick up — the tree would report green on a
// runner that never ran it. The test below reads that file and asserts the
// value is one of the two backends kit's table names, so the failure mode is
// a red test naming the unknown backend rather than a silent no-run.
//
// It lives in its own package because no existing one owns the repository's
// shape: `variants` owns image variants, `httpapi` owns the document-to-router
// drift, and a backend assertion in either would be a claim filed under the
// wrong name. It needs no database, so it runs in the default tier; it is
// mapped in `bin/prime`'s `-list` step like every other suite.
package cibackend
