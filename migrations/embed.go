// Package migrations holds darkroom's SQL migrations and applies them.
//
// WHY THIS IS A PACKAGE RATHER THAN A DIRECTORY THE RUNTIME READS.
//
// `migrations/README.md` says the migrations "are applied in filename order by
// `sqlx::migrate!`, which embeds them in the binary — there is no `migrations/`
// directory to lose in a distroless image." That sentence is about Rust and its
// reason survives the rewrite intact: a migration set that is read from the
// filesystem at boot is a migration set that a distroless image cannot have, so
// the set is compiled in and a deploy step applies it.
//
// THE ALTERNATIVE REJECTED, so it is not proposed again by the next reader. `go:embed`
// refuses a `..` in its pattern, so a package under `internal/` cannot embed the
// repository's own top-level `migrations/`. The two ways round that are to copy
// the SQL into `internal/`, which gives the fleet two files claiming to be one
// schema and guarantees they diverge, or to put a `package` clause in the
// directory that already holds the SQL — which is what this is. The embed sits
// where the files sit, and there is one copy.
//
// # WHAT APPLIES THEM IS NOT HERE
//
// `Apply` takes a `*sql.DB`-shaped interface rather than reaching for a pool of
// its own, so the caller owns the connection and the search_path. That matters
// for the test harness in `internal/darkroomdb`, which applies the whole set
// into a throwaway schema per test and needs the `search_path` to be the test's
// rather than this package's idea of one. A migration runner that opened its own
// connection would apply the schema into whatever `search_path` the role
// happened to have, which on the shared cluster is `"$user", public`.
package migrations

import (
	"context"
	"embed"
	"fmt"
	"io/fs"
	"sort"
	"strings"

	"github.com/jackc/pgx/v5/pgconn"
)

//go:embed *.sql
var files embed.FS

// FS is the compiled-in migration set. It is exported so a deploy step can apply
// the same bytes the binary carries, with psql, if that is what the operator
// would rather do — `migrations/README.md` documents both routes.
var FS fs.FS = files

// File is one migration, in the order it must be applied.
//
// There is no version, no checksum and no `down` half. That is not a shortcut:
// darkroom's three applied migrations are plain SQL with no up/down markers, and
// a runner that understood versions but not the marker convention would be a
// second convention. Filename order is the whole of it, and
// `migrations/README.md` "Rules" says the same thing.
type File struct {
	// Name is the filename, and it is the version. `0004_...` sorts before
	// `00010_...` because these are four digits wide and zero-padded; the
	// filenames are the sort.
	Name string
	SQL  string
}

// Names returns every migration filename in application order.
//
// Sorted, and the sort is the contract rather than a convenience: `os.ReadDir`
// already returns sorted names, but this function is also given `embed.FS`, whose
// `ReadDir` promise is weaker, and an ordering that holds for one and not the
// other is an ordering that holds until it does not.
func Names() ([]string, error) {
	entries, err := fs.ReadDir(files, ".")
	if err != nil {
		return nil, fmt.Errorf("reading the compiled-in migrations: %w", err)
	}
	names := make([]string, 0, len(entries))
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".sql") {
			continue
		}
		names = append(names, e.Name())
	}
	sort.Strings(names)
	return names, nil
}

// All returns every migration, in application order.
func All() ([]File, error) {
	names, err := Names()
	if err != nil {
		return nil, err
	}
	out := make([]File, 0, len(names))
	for _, n := range names {
		b, err := files.ReadFile(n)
		if err != nil {
			return nil, fmt.Errorf("reading %s: %w", n, err)
		}
		out = append(out, File{Name: n, SQL: string(b)})
	}
	return out, nil
}

// Execer is the one method `Apply` needs from a connection.
//
// Narrow on purpose, and not for testability theater: it is what lets the harness
// hand `Apply` a connection whose `search_path` is a per-test schema, and it is
// what stops this package from acquiring a pool of its own and deciding the
// search_path itself. `*pgx.Conn` and `pgx.Tx` both satisfy it, which is what lets
// a caller apply the set inside a transaction — see `Apply`.
type Execer interface {
	Exec(ctx context.Context, sql string, arguments ...any) (pgconn.CommandTag, error)
}

// Apply runs every migration against `db`, in order, on the connection it was
// given.
//
// THE ERROR NAMES THE FILE AND NOT THE STATEMENT INDEX, because the file is what a
// deploy step can act on and the statement index is what a reader has to count
// lines to use. It also names the file rather than the LINE, because a migration
// may legitimately contain several statements and `0004_account_isolation.sql`
// contains nine hundred lines of them.
//
// IT DOES NOT OPEN A TRANSACTION, and that is worth naming because it looks like an
// omission. A transaction here would be wrong in one direction and insufficient in
// the other: Postgres refuses `CREATE INDEX CONCURRENTLY` and `VACUUM` inside a
// transaction block, so a future migration that needs one could not be applied by
// this function at all; and a rollback that undid half the set would leave
// `cafaye.protect_table` defined with two of darkroom's tables protected, which is
// a state no `psql -f` of the same files can produce and therefore a state
// nothing knows how to recover from.
//
// The caller that wants one wraps this: the harness in `internal/darkroomdb/pgtest`
// applies the set inside a transaction, because a test's schema is thrown away
// either way. A DEPLOY step does not, because it should not be able to half-apply.
func Apply(ctx context.Context, db Execer) error {
	all, err := All()
	if err != nil {
		return err
	}
	for _, f := range all {
		if _, err := db.Exec(ctx, f.SQL); err != nil {
			return fmt.Errorf("applying %s: %w", f.Name, err)
		}
	}
	return nil
}
