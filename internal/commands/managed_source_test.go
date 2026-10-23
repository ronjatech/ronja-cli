package commands

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/ronjatech/ronja-cli/internal/api"
	"github.com/ronjatech/ronja-cli/internal/wfdir"
)

// A checkout with no git remote recorded an EMPTY source label, so every note
// about a row it manages named the repository by its folder id alone. It now
// records the repository directory's name and the folder's path, marked as
// local — and that label names no repository: it is never foreign, and never
// replaces a label a checkout with a remote recorded.
func TestALocalCheckoutRecordsAReadableSourceLabel(t *testing.T) {
	git, err := exec.LookPath("git")
	if err != nil {
		t.Skip("no git binary")
	}
	repo := filepath.Join(t.TempDir(), "roastery")
	folder := filepath.Join(repo, "apps", "roastery-dashboard")
	if err := os.MkdirAll(folder, 0o755); err != nil {
		t.Fatal(err)
	}
	if out, err := exec.Command(git, "-C", repo, "init", "-q").CombinedOutput(); err != nil {
		t.Fatalf("git init: %v\n%s", err, out)
	}
	label := gitSourceLabel(folder)
	if want := "roastery/apps/roastery-dashboard (local, no git remote)"; label != want {
		t.Fatalf("label = %q, want %q", label, want)
	}
	if outside := gitSourceLabel(t.TempDir()); !strings.HasSuffix(outside, localSourceSuffix) {
		t.Errorf("a folder in no repository is labelled %q", outside)
	}

	const remote = "github.com/acme/roastery//apps/roastery-dashboard"
	switch {
	case foreignClaim(remote, label) || foreignClaim(label, remote):
		t.Error("a local label reads as another repository")
	case relabelTo(remote, label):
		t.Error("a deploy from a local checkout would replace the repository's label")
	case !relabelTo("", label):
		t.Error("an empty label is not given the local one")
	case !relabelTo(label, remote):
		t.Error("a checkout with a remote does not replace a local label")
	}

	mb := &api.ManagedBy{FolderID: "1693a650-0000-0000-0000-000000000000", SourceLabel: label, StackLabel: "main"}
	line := describeManagedBy(wfdir.DataAppKind, mb, noticeForChange)
	if !strings.Contains(line, `(roastery/apps/roastery-dashboard (local, no git remote), stack "main")`) || strings.Contains(line, "folder id") {
		t.Errorf("notice = %q", line)
	}
}
