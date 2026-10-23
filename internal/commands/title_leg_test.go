package commands

import (
	"strings"
	"testing"

	"github.com/ronjatech/ronja-cli/internal/api"
	"github.com/ronjatech/ronja-cli/internal/wfdir"
)

// The FOLDER kinds' title leg beyond the data app: the workflow's title was
// drift-checked against the LOCAL baseline only, so a fresh checkout — CI —
// patched it blind and reverted a rename made in the web app.

func cloneStackWorkflow(t *testing.T, f *fakeInstance) string {
	t.Helper()
	f.AddWorkflow(&api.Workflow{ID: "wf-1", Lifecycle: api.LifecycleLive, FeatureScope: "private", Title: "Monthly report"},
		api.WorkflowFile{Path: "main.py", Content: "live\n"})
	f.versions["wf-1"] = []api.Workflow{{ID: "wfv-1"}}
	signIn(t, f)
	out, err := runCLI(t, t.TempDir(), "wf", "clone", "wf-1", "--stack", "dev", "--json")
	if err != nil {
		t.Fatalf("clone: %v", err)
	}
	return decodeJSON(t, out)["root"].(string)
}

// A FRESH CHECKOUT WITH A RECORDED TITLE refuses a web rename rather than
// reverting it.
func TestWorkflowFreshCheckoutRefusesATitleRenamedElsewhere(t *testing.T) {
	f := newFakeInstance(t)
	root := cloneStackWorkflow(t, f)
	lock, _ := wfdir.LoadLock(root)
	lock.SetAppliedTitle("dev", "Monthly report")
	if err := wfdir.SaveLock(root, lock); err != nil {
		t.Fatal(err)
	}
	f.workflows["wf-1"].Title = "Renamed in the web app"
	writeLocal(t, root, "main.py", "mine\n")
	freshCheckout(t, root)

	_, err := runCLI(t, root, "wf", "push", "--json")
	if err == nil || !strings.Contains(err.Error(), "Renamed in the web app") {
		t.Fatalf("a fresh checkout over a web rename was not refused: %v", err)
	}
	if len(f.titlePatches) != 0 {
		t.Errorf("the title was patched anyway: %+v", f.titlePatches)
	}
}

// With NOTHING recorded, a fresh checkout cannot tell whose rename it is, and a
// workflow's head anchor cannot tell it either — a live workflow in a private
// feature is retitled in place, which files no version. So the push is REFUSED,
// on every push, not recorded and reverted one push later; --force renames.
func TestWorkflowFreshCheckoutWithNoRecordedTitleRefuses(t *testing.T) {
	f := newFakeInstance(t)
	root := cloneStackWorkflow(t, f)
	// Renamed in place, as the server does for a private workflow: no version.
	f.workflows["wf-1"].Title = "Renamed in the web app"
	freshCheckout(t, root)
	// The second push may be refused by the files guard instead (the first one
	// left the draft it checked out open); what matters is that it is refused
	// and writes nothing.
	for push := 1; push <= 2; push++ {
		_, err := runCLI(t, root, "wf", "push", "--json")
		if err == nil || (push == 1 && !strings.Contains(err.Error(), "Renamed in the web app")) {
			t.Fatalf("push %d over an unrecorded web rename was not refused: %v", push, err)
		}
		if len(f.titlePatches) != 0 {
			t.Fatalf("push %d patched the title: %+v", push, f.titlePatches)
		}
	}
	if lock, _ := wfdir.LoadLock(root); lock.AppliedTitle("dev") != "" {
		t.Errorf("a refused push recorded the web rename as this folder's own: %q", lock.AppliedTitle("dev"))
	}
	if _, err := runCLI(t, root, "wf", "push", "--force", "--json"); err != nil {
		t.Fatalf("push --force: %v", err)
	}
	if len(f.titlePatches) != 1 {
		t.Errorf("--force patched %+v, want the declared title once", f.titlePatches)
	}
}

// A WORKFLOW's head never vouches for its title, so even this folder's own
// intended rename in a fresh checkout with nothing recorded is refused — and
// --force is the way through.
func TestWorkflowFreshCheckoutRenameWithNoRecordedTitleNeedsForce(t *testing.T) {
	f := newFakeInstance(t)
	root := cloneStackWorkflow(t, f)
	retitle(t, root, "Quarterly report")
	freshCheckout(t, root)
	_, err := runCLI(t, root, "wf", "push", "--json")
	if err == nil {
		t.Fatal("an unvouched rename was written")
	}
	// The likeliest history on a fresh checkout is a build server that renamed
	// it and never committed the lock, so the refusal names that way out.
	for _, want := range []string{"ronja wf push --stack dev --force", wfdir.LockName} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("the refusal does not name the remedy (%q): %v", want, err)
		}
	}
	if len(f.titlePatches) != 0 {
		t.Fatalf("patched %+v", f.titlePatches)
	}
	if _, err := runCLI(t, root, "wf", "push", "--force", "--json"); err != nil {
		t.Fatalf("push --force: %v", err)
	}
	if len(f.titlePatches) != 1 {
		t.Errorf("--force patched %+v", f.titlePatches)
	}
}

// THE FOLDER'S OWN RENAME, pushed twice before it is published: the second push
// must not read its own staged rename as a rename made elsewhere — on a stack
// folder, and on a legacy one with no lock to remember anything in.
func TestTitleRenamePushedTwiceBeforePublishIsNotDrift(t *testing.T) {
	t.Run("workflow on a stack", func(t *testing.T) {
		f := newFakeInstance(t)
		root := cloneStackWorkflow(t, f)
		retitle(t, root, "Quarterly report")
		writeLocal(t, root, "main.py", "one\n")
		if _, err := runCLI(t, root, "wf", "push", "--json"); err != nil {
			t.Fatalf("first push: %v", err)
		}
		writeLocal(t, root, "main.py", "two\n")
		if _, err := runCLI(t, root, "wf", "push", "--json"); err != nil {
			t.Fatalf("the second push refused the folder's own pending rename: %v", err)
		}
	})
	t.Run("data app in a legacy folder", func(t *testing.T) {
		f := newFakeAppInstance(t)
		f.AddApp(&api.DataApp{ID: "app-1", Lifecycle: api.LifecycleLive},
			api.DataAppFile{Path: "App.tsx", Content: "live\n"})
		signInApp(t, f)
		dir := t.TempDir() + "/app"
		if _, err := runCLI(t, t.TempDir(), "app", "clone", "app-1", dir); err != nil {
			t.Fatalf("clone: %v", err)
		}
		setAppTitle(t, dir, "Revenue cockpit")
		writeLocal(t, dir, "App.tsx", "one\n")
		if _, err := runCLI(t, dir, "app", "push", "--no-validate"); err != nil {
			t.Fatalf("first push: %v", err)
		}
		writeLocal(t, dir, "App.tsx", "two\n")
		if _, err := runCLI(t, dir, "app", "push", "--no-validate"); err != nil {
			t.Fatalf("the second push refused the folder's own pending rename: %v", err)
		}
	})
}

// A WEB RENAME OF THE LIVE WORKFLOW WHILE THE FOLDER'S OWN RENAME IS STAGED: a
// live workflow is retitled in place, which files no version, so no commit
// precondition would catch it — publishing the draft would silently replace the
// colleague's title with the folder's. The push itself has to see it.
func TestWorkflowPushRefusesALiveRetitleWhileItsOwnRenameIsStaged(t *testing.T) {
	f := newFakeInstance(t)
	root := cloneStackWorkflow(t, f)
	retitle(t, root, "Quarterly report")
	writeLocal(t, root, "main.py", "one\n")
	if _, err := runCLI(t, root, "wf", "push", "--json"); err != nil {
		t.Fatalf("first push: %v", err)
	}
	// Retitled in place, as the server does for a live private workflow.
	f.workflows["wf-1"].Title = "Renamed in the web app"
	writeLocal(t, root, "main.py", "two\n")
	_, err := runCLI(t, root, "wf", "push", "--json")
	if err == nil || !strings.Contains(err.Error(), "Renamed in the web app") {
		t.Fatalf("a live retitle made while this folder's rename was staged was not refused: %v", err)
	}
	if lock, _ := wfdir.LoadLock(root); lock.AppliedTitle("dev") == "Renamed in the web app" {
		t.Error("the refused push recorded the web rename as this folder's own")
	}
}

// A MODULE's title on a fresh checkout: a colleague renamed the live module,
// and a push with no local baseline used to patch the title straight back —
// the module's title leg checked only the local baseline. The lock's applied
// title (recorded at create) now refuses it.
func TestModuleFreshCheckoutRefusesATitleRenamedElsewhere(t *testing.T) {
	f := newFakeInstance(t)
	signInAsModuleAdmin(t, f)
	root := t.TempDir()
	if _, err := runCLI(t, root, "module", "init", "emab_tracker",
		"--feature", "collection-1", "--title", "EMAB tracker", "--stack", "dev"); err != nil {
		t.Fatalf("module init: %v", err)
	}
	if err := wfdir.WriteFile(root, "tracker.py", "x = 1\n"); err != nil {
		t.Fatal(err)
	}
	if _, err := runCLI(t, root, "module", "push", "--json"); err != nil {
		t.Fatalf("first push: %v", err)
	}
	if _, err := runCLI(t, root, "module", "publish", "--json"); err != nil {
		t.Fatalf("publish: %v", err)
	}
	live := f.mod.rows["module-new-1"]
	live.Title = "Renamed by a colleague"
	freshCheckout(t, root)
	patches := len(f.mod.patches)

	_, err := runCLI(t, root, "module", "push", "--json")
	if err == nil || !strings.Contains(err.Error(), "Renamed by a colleague") {
		t.Fatalf("a fresh checkout over a renamed module was not refused: %v", err)
	}
	if len(f.mod.patches) != patches || live.Title != "Renamed by a colleague" {
		t.Errorf("the title was reverted: %q (patches %+v)", live.Title, f.mod.patches)
	}
}
