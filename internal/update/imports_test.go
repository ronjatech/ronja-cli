package update

import (
	"os/exec"
	"strings"
	"testing"
)

// internal/api attaches the Ronja bearer token to every request, so any path
// from this package to it puts a credential one refactor from github.com.
// Checked over the transitive closure, since a shared helper in between is how
// it would be lost.
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
