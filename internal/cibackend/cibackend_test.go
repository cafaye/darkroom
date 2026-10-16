package cibackend

import (
	"os"
	"strings"
	"testing"
)

// backendPath is relative to this package's directory, the same spelling
// `internal/httpapi` uses for `openapi/v1.yaml`. A test that locates the file
// any other way is a test about the locator rather than about the switch.
const backendPath = "../../ci/backend"

// shippedBackends are the backends kit ships, from the table in kit's
// `backends/README.md`. A third backend is a line in that table first, and a
// line here second — a backend invented in this file alone is a switch
// position nothing runs.
var shippedBackends = []string{"github-actions", "instabox"}

// TestBackendNamesAShippedBackend fails when `ci/backend` names a backend kit
// does not ship. The read is `os.ReadFile`, not a helper that could default:
// a missing file must be an error naming the path, never an empty backend
// that some later comparison quietly accepts.
func TestBackendNamesAShippedBackend(t *testing.T) {
	raw, err := os.ReadFile(backendPath)
	if err != nil {
		t.Fatalf("reading %s: %v — the backend switch is missing, so no backend runs this tree", backendPath, err)
	}
	backend := strings.TrimSpace(string(raw))
	for _, shipped := range shippedBackends {
		if backend == shipped {
			return
		}
	}
	t.Fatalf("ci/backend names %q, which is not one of the backends kit ships (%s) — "+
		"a backend nothing runs is a pipeline that never executes",
		backend, strings.Join(shippedBackends, ", "))
}
