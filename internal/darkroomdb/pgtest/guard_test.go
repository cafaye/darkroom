// This file is in package `pgtest` and not `pgtest_test`, and that is the whole
// reason it exists.
//
// `droppable` and `uniqueSuffix` are deliberately unexported: `droppable` is the
// last thing between a bug in the name generator and a `drop schema public`, and
// exporting it would put it in the package's API for a caller to misuse.
// `pgtest_test.go` is `package pgtest_test` and so cannot reach either.
//
// So the guard's own tests live here, in the package, with the same access the
// guard has. Pantry's harness is `package pantrydb_test` throughout and never faced
// this, because it had no unexported predicate to test: its isolation came from
// `tests/rls.sh` starting a scratch cluster rather than from a name a Go function
// had to police. The split here is the cost of putting the isolation in Go where
// the fleet's parallel suites can use it, and it is paid in one file rather than by
// exporting the guard.
package pgtest

import "testing"

// TestDroppableIsThePlantedDivergenceForTheDropGuard, and the guard exists because
// `drop schema` is unconditional SQL.
//
// `AGENTS.md` on `tests/tenant_scoping.rs`: "Any new guard here gets a planted
// divergence before it is believed." Every row below marked PLANTED is a name that
// must be REFUSED, and the reason each one matters is that a harness which dropped
// it would delete a schema it did not create — `public` being the one that matters
// and the one whose loss a developer notices immediately.
//
// The two rows marked REAL are the only two accepted, and they are accepted for
// different reasons: one has the right shape and the other is the shape
// `uniqueSuffix` actually produces. Both are checked because a guard that rejects
// everything is as broken as one that accepts everything, and the only way to tell
// them apart is to test both directions.
func TestDroppableIsThePlantedDivergenceForTheDropGuard(t *testing.T) {
	for _, tc := range []struct {
		name   string
		schema string
		want   bool
	}{
		{"PLANTED: the developer's own schema", "public", false},
		{"PLANTED: empty", "", false},
		{"PLANTED: the prefix alone", "t_", false},
		{"PLANTED: too short", "t_abc", false},
		// One character over SchemaSuffixLen. The first draft of this row was
		// `t_0123456789ab` labeled "too long", which is byte-identical to the REAL
		// row two lines below it — so the row asserted `false` for a name the
		// harness builds and the table failed on its own correct implementation.
		// A planted divergence has to be a real divergence, not a plausible-looking
		// string, and nothing checks that except running it.
		{"PLANTED: one character too long", "t_0123456789abc", false},
		{"PLANTED: uppercase, which Postgres folds and which this does not accept", "t_0123456789AB", false},
		{"PLANTED: a schema-qualified name", "public.t_0123456789ab", false},
		{"PLANTED: a table name that starts the same way", "t_0123456789ab_suffix", false},
		{"PLANTED: not hex at all", "t_zzzzzzzzzzzz", false},

		{"REAL: the shape uniqueSuffix builds", "t_0123456789ab", true},
		{"REAL: another of the same shape", "t_ffffffffffff", true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := droppable(tc.schema); got != tc.want {
				t.Errorf("droppable(%q) = %v, want %v", tc.schema, got, tc.want)
			}
		})
	}
}

// TestDroppableAgreesWithUniqueSuffix is the other direction of the same guard, and
// it is the one that would have caught the bug this harness actually shipped.
//
// `SchemaSuffixLen` and `uniqueSuffix` disagreed — the guard demanded 8 hex
// characters and the generator built 12 — and every test failed with "REFUSING to
// drop" while leaving its schema behind on the cluster. A test that only plants
// divergences finds that; this one prevents it, by asserting that a name the
// generator builds is a name the guard accepts.
//
// It is a property over the GENERATOR rather than a string comparison, because the
// generator is what varies and the constant is what is pinned.
func TestDroppableAgreesWithUniqueSuffix(t *testing.T) {
	// Twenty names, because twenty collisions of the test-name half are not the
	// failure mode and a single sample would not show the random half doing
	// anything. The point is that the SHAPE is stable across many draws.
	for i := 0; i < 20; i++ {
		name := uniqueSuffix(t)
		if !droppable(SchemaPrefix + name) {
			t.Fatalf("uniqueSuffix produced %q, which droppable refuses. The generator and the drop guard disagree, and every test using it will fail to clean up.", name)
		}
		if len(name) != SchemaSuffixLen {
			t.Fatalf("uniqueSuffix produced %d characters, want SchemaSuffixLen (%d)", len(name), SchemaSuffixLen)
		}
	}
}
