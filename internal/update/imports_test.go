package update

import (
	"os/exec"
	"strings"
	"testing"
)

// The invariant: internal/api attaches `Authorization: Bearer <token>` to every
// request it sends, so any path from this package to that one would put a live
// Ronja credential one refactor away from being sent to github.com.
//
// Asserted over the TRANSITIVE dependency set rather than the direct imports,
// because the way this would actually be lost is a helper added to some shared
// package in between. `go list` is the only thing that knows the real closure;
// a test that could not run it would be a test of nothing, so it skips rather
// than passes silently.
func TestDoesNotImportTheAPIClient(t *testing.T) {
	const forbidden = "github.com/ronjatech/ronja-cli/internal/api"

	if _, err := exec.LookPath("go"); err != nil {
		t.Skip("no go toolchain on PATH; cannot resolve the dependency closure")
	}
	out, err := exec.Command("go", "list", "-deps", "github.com/ronjatech/ronja-cli/internal/update").CombinedOutput()
	if err != nil {
		t.Fatalf("go list -deps: %v\n%s", err, out)
	}
	for _, dep := range strings.Split(strings.TrimSpace(string(out)), "\n") {
		if strings.TrimSpace(dep) == forbidden {
			t.Fatalf("internal/update depends on %s — the release fetch must never be able to reach the credential-attaching client", forbidden)
		}
	}
}
