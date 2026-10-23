package commands

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/ronjatech/ronja-cli/internal/api"
	"github.com/ronjatech/ronja-cli/internal/wfdir"
)

// `moved` and `removed` in an AUTOMATION folder. The pipeline fixtures and the
// rest of the address tests are in addresses_test.go.

func movedAutomationFolder(t *testing.T, f *fakeAutomationInstance, files map[string]string,
	bound map[string]string, moved []wfdir.MovedEntry, removed []string) (tree, root string) {
	t.Helper()
	tree = t.TempDir()
	root = filepath.Join(tree, "orders")
	if err := os.MkdirAll(root, 0o755); err != nil {
		t.Fatal(err)
	}
	seen := map[string]string{}
	for path := range bound {
		seen[path] = "2026-08-10T10:00:00Z"
	}
	manifest, lock := automationStack(f, "collection-1", bound, seen)
	manifest.Moved, manifest.Removed = moved, removed
	writeAutomationFolder(t, root, manifest, lock, files)
	return tree, root
}

// An automation file renamed with a `moved` entry keeps its automation: no
// create, no orphan refusal, the anchor travels, and the NAME is left alone.
func TestAutomationPushMovedKeepsTheAutomation(t *testing.T) {
	f := newFakeAutomationInstance(t)
	signInAutomation(t, f)
	f.AddAutomation(&api.Automation{ID: "job-1", Name: "nightly", CronExpr: "0 2 * * *", Enabled: true})
	tree, root := movedAutomationFolder(t, f, map[string]string{"nightly_refresh.json": nightlyBody},
		map[string]string{"nightly.json": "job-1"},
		[]wfdir.MovedEntry{{From: "nightly.json", To: "nightly_refresh.json"}}, nil)

	// The dry-run first, which lists the move and writes nothing.
	out, err := runCLI(t, tree, "sync", "apply", "--stack", "prod", "--dry-run", "--json")
	if err != nil {
		t.Fatalf("dry-run: %v\n%s", err, out)
	}
	if actions := unitActions(applyFolderLine(t, applyReportOf(t, out), "orders")); actions["nightly.json"] != string(applyActionMove) {
		t.Fatalf("units = %v", actions)
	}

	out, stderr, err := runAutomationCLI(t, root, "automation", "push", "--stack", "prod", "--json")
	if err != nil {
		t.Fatalf("push: %v\n%s\n%s", err, out, stderr)
	}
	if len(f.created) != 0 || len(f.deleted) != 0 {
		t.Fatalf("created=%v deleted=%v", f.created, f.deleted)
	}
	for _, u := range f.updates {
		if _, renamed := u.Body["name"]; renamed {
			t.Errorf("a move renamed the automation: %+v", u.Body)
		}
	}
	lock := lockOf(t, root)
	if id, seen := lock.AutomationSeen("prod", "nightly_refresh.json"); id != "job-1" || seen == "" {
		t.Errorf("new address = (%q, %q), want job-1 with its anchor", id, seen)
	}
	if lock.AutomationStem("prod", "nightly_refresh.json") != "nightly" {
		t.Errorf("the name baseline did not travel: %q", lock.AutomationStem("prod", "nightly_refresh.json"))
	}
}

// An UNDECLARED automation rename keeps today's refusal (push without --prune,
// apply as orphaned_file), now naming the declared ways through.
func TestAutomationUndeclaredRenameNamesMovedAndRemoved(t *testing.T) {
	f := newFakeAutomationInstance(t)
	signInAutomation(t, f)
	f.AddAutomation(&api.Automation{ID: "job-1", Name: "nightly", CronExpr: "0 2 * * *", Enabled: true})
	tree, root := movedAutomationFolder(t, f, map[string]string{"nightly_refresh.json": nightlyBody},
		map[string]string{"nightly.json": "job-1"}, nil, nil)

	_, _, err := runAutomationCLI(t, root, "automation", "push", "--stack", "prod")
	if err == nil {
		t.Fatal("an undeclared rename pushed")
	}
	for _, want := range []string{"nightly.json is bound but gone, and nightly_refresh.json is new", `"moved" entry`, `"removed" entry`, "--prune"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("push refusal does not say %q:\n%v", want, err)
		}
	}
	out, _ := runCLI(t, tree, "sync", "apply", "--stack", "prod", "--json")
	if line := applyFolderLine(t, applyReportOf(t, out), "orders"); line.Reason != syncReasonOrphanedFile || !strings.Contains(line.Detail, `"moved" entry`) {
		t.Errorf("apply = %+v", line)
	}
	if len(f.created) != 0 || len(f.updates) != 0 || len(f.deleted) != 0 {
		t.Errorf("a refused folder wrote: created=%v updates=%v deleted=%v", f.created, f.updates, f.deleted)
	}
}

// `removed` on an automation stops managing it and deletes NOTHING — where
// --prune would have soft-deleted and paused it.
func TestAutomationPushRemovedLeavesTheAutomationRunning(t *testing.T) {
	f := newFakeAutomationInstance(t)
	signInAutomation(t, f)
	f.AddAutomation(&api.Automation{ID: "job-1", Name: "nightly", CronExpr: "0 2 * * *", Enabled: true})
	f.AddAutomation(&api.Automation{ID: "job-2", Name: "weekly", CronExpr: "0 4 * * 1", Enabled: true})
	_, root := movedAutomationFolder(t, f, map[string]string{"weekly.json": `{"enabled": true, "cronExpr": "0 4 * * 1"}`},
		map[string]string{"nightly.json": "job-1", "weekly.json": "job-2"}, nil, []string{"nightly.json"})

	_, stderr, err := runAutomationCLI(t, root, "automation", "push", "--stack", "prod", "--prune")
	if err != nil {
		t.Fatalf("push: %v\n%s", err, stderr)
	}
	if len(f.deleted) != 0 {
		t.Fatalf("a removed automation was deleted (even under --prune): %v", f.deleted)
	}
	if row := f.RowOf("job-1"); row == nil || !row.Enabled {
		t.Error("the removed automation was paused or deleted")
	}
	for _, r := range f.requestOrder() {
		if strings.Contains(r, "job-1") {
			t.Errorf("a request touched the removed automation: %s", r)
		}
	}
	if id, _ := lockOf(t, root).AutomationSeen("prod", "nightly.json"); id != "" {
		t.Error("the removed automation is still in the lock")
	}
}

// A MOVE UNDER --prune deletes nothing: the declaration re-keys the automation
// before the orphan pass looks, so the renamed file's automation is never the
// orphan --prune would soft-delete and pause.
func TestAutomationPushMovedWithPruneDeletesNothing(t *testing.T) {
	f := newFakeAutomationInstance(t)
	signInAutomation(t, f)
	f.AddAutomation(&api.Automation{ID: "job-1", Name: "nightly", CronExpr: "0 2 * * *", Enabled: true})
	_, root := movedAutomationFolder(t, f, map[string]string{"nightly_refresh.json": nightlyBody},
		map[string]string{"nightly.json": "job-1"},
		[]wfdir.MovedEntry{{From: "nightly.json", To: "nightly_refresh.json"}}, nil)

	out, stderr, err := runAutomationCLI(t, root, "automation", "push", "--stack", "prod", "--prune", "--json")
	if err != nil {
		t.Fatalf("push --prune: %v\n%s\n%s", err, out, stderr)
	}
	if len(f.deleted) != 0 || len(f.created) != 0 {
		t.Fatalf("a moved automation was pruned or re-created: deleted=%v created=%v", f.deleted, f.created)
	}
	if row := f.RowOf("job-1"); row == nil || !row.Enabled {
		t.Error("the moved automation was paused or deleted")
	}
	if id, _ := lockOf(t, root).AutomationSeen("prod", "nightly_refresh.json"); id != "job-1" {
		t.Errorf("the lock does not bind the new file: %q", id)
	}
}
