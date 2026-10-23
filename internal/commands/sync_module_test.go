package commands

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/ronjatech/ronja-cli/internal/api"
	"github.com/ronjatech/ronja-cli/internal/wfdir"
)

// Module folders in `ronja sync`: every verb has a module arm of its own, so a
// tree holding modules can be green, and `sync apply` deploys them. The
// two-run idempotence, the draft-kind apply rules and the server-state legs run
// the module fixture beside the other kinds (moduleIdempotenceFixture); what is
// here is what only a module has, and each verb's arm pinned directly.

// syncModuleStatusLine runs `sync status --stack prod` over a tree and answers
// the exit code and the one folder's line.
func syncModuleStatusLine(t *testing.T, tree string, args ...string) (int, map[string]any, string) {
	t.Helper()
	out, err := runCLI(t, tree, append([]string{"sync", "status", "--json"}, args...)...)
	report := decodeJSON(t, out)
	folders := report["folders"].([]any)
	if len(folders) != 1 {
		t.Fatalf("folders = %v, want one\n%s", folders, out)
	}
	return exitCodeOf(err), folders[0].(map[string]any), out
}

// THE STATUS ARM. A module folder is reported on from `module status`'s own
// report: clean when the organization holds it, drifted for an edit here, an
// edit there or a package name the row does not carry, and unknown on a fresh
// clone, which has no baseline to compare the draft-kind way against.
func TestSyncStatusModuleFolder(t *testing.T) {
	cases := []struct {
		name     string
		change   func(t *testing.T, fx idempotenceFixture)
		wantExit int
		want     string
		detail   string
	}{
		{"clean", func(*testing.T, idempotenceFixture) {}, syncExitClean, verdictClean, ""},
		{"edited here", func(t *testing.T, fx idempotenceFixture) {
			writeLocal(t, fx.root, "tracker.py", "edited here\n")
		}, syncExitDrifted, verdictDrifted, "tracker.py"},
		{"edited on the server", func(t *testing.T, fx idempotenceFixture) {
			fx.draftKind.webPublish("edited there\n")
		}, syncExitDrifted, verdictDrifted, "changed on the server"},
		{"package name", func(t *testing.T, fx idempotenceFixture) {
			m, err := wfdir.LoadManifest(fx.root, wfdir.ModuleKind)
			if err != nil {
				t.Fatal(err)
			}
			m.Name = "emab_tracker_v2"
			if err := wfdir.SaveManifest(fx.root, m); err != nil {
				t.Fatal(err)
			}
		}, syncExitDrifted, verdictDrifted, "package name"},
		{"fresh clone", func(t *testing.T, fx idempotenceFixture) {
			if err := os.RemoveAll(filepath.Join(fx.root, wfdir.StateDirName)); err != nil {
				t.Fatal(err)
			}
		}, syncExitUnknown, verdictUnknown, "no local baseline"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			fx := moduleIdempotenceFixture(t)
			tc.change(t, fx)
			fx.resetRequests()
			code, line, out := syncModuleStatusLine(t, fx.tree, "--stack", "prod")
			if code != tc.wantExit || line["verdict"] != tc.want || line["kind"] != wfdir.KindModule {
				t.Fatalf("exit = %d, line = %v, want %d / %s\n%s", code, line, tc.wantExit, tc.want, out)
			}
			if detail, _ := line["detail"].(string); !strings.Contains(detail, tc.detail) {
				t.Errorf("detail = %q, want it to mention %q", detail, tc.detail)
			}
			// Read off the MODULE routes, never the workflow ones a `default:`
			// arm would have sent a module's id to.
			for _, r := range fx.requests() {
				if strings.Contains(r, "/api/v2/workflow") {
					t.Fatalf("a module folder was read as a workflow: %s", r)
				}
			}
		})
	}
}

// A module folder bound here with nothing deployed is drifted, not clean — and
// `module init --stack` produces exactly that folder, seeding __init__.py.
func TestSyncStatusNeverDeployedModuleFolder(t *testing.T) {
	f := newFakeInstance(t)
	signIn(t, f)
	tree := t.TempDir()
	root := filepath.Join(tree, "tracker")
	if err := os.MkdirAll(root, 0o755); err != nil {
		t.Fatal(err)
	}
	if out, err := runCLI(t, root, "module", "init", "emab_tracker", "--feature", "collection-1", "--stack", "prod"); err != nil {
		t.Fatalf("module init: %v\n%s", err, out)
	}
	code, line, out := syncModuleStatusLine(t, tree, "--stack", "prod")
	if code != syncExitDrifted || line["verdict"] != verdictDrifted {
		t.Fatalf("exit = %d, line = %v\n%s", code, line, out)
	}
	if detail, _ := line["detail"].(string); !strings.Contains(detail, "never been deployed") || !strings.Contains(detail, "the module") {
		t.Errorf("detail = %q", detail)
	}
}

// THE CHECK ARM. A module makes no references, so its folder is clean with no
// edges and costs no request — signed in or not. A marker in a module file is a
// finding (the server refuses it at save), and a `dependencies` block a warning:
// reported as edges it would send the reader to `ronja bind`, which refuses a
// module folder.
func TestSyncCheckModuleFolder(t *testing.T) {
	t.Run("clean", func(t *testing.T) {
		fx := moduleIdempotenceFixture(t)
		fx.resetRequests()
		out, err := runCLI(t, fx.tree, "sync", "check", "--stack", "prod", "--json")
		if code := exitCodeOf(err); code != syncExitClean {
			t.Fatalf("exit = %d (%v)\n%s", code, err, out)
		}
		folder := checkFolders(t, out)[0]
		if folder["verdict"] != verdictClean || folder["kind"] != wfdir.KindModule || len(edgesOf(t, folder)) != 0 {
			t.Fatalf("folder = %v", folder)
		}
		for _, r := range fx.requests() {
			if strings.Contains(r, "/workflow") || strings.Contains(r, "/feature/model") {
				t.Fatalf("a module folder's check asked the instance about references it cannot make: %s", r)
			}
		}
	})
	t.Run("a marker is a finding", func(t *testing.T) {
		fx := moduleIdempotenceFixture(t)
		writeLocal(t, fx.root, "tracker.py", "TABLE = \"{{ ref('table-orders') }}\"\n")
		out, err := runCLI(t, fx.tree, "sync", "check", "--stack", "prod", "--json")
		if code := exitCodeOf(err); code != syncExitDrifted {
			t.Fatalf("exit = %d (%v)\n%s", code, err, out)
		}
		folder := checkFolders(t, out)[0]
		findings, _ := folder["findings"].([]any)
		if folder["verdict"] != verdictBroken || len(findings) != 1 || !strings.Contains(findings[0].(string), "marker-free") {
			t.Fatalf("folder = %v", folder)
		}
	})
	t.Run("a dependencies block is a warning", func(t *testing.T) {
		fx := moduleIdempotenceFixture(t)
		m, err := wfdir.LoadManifest(fx.root, wfdir.ModuleKind)
		if err != nil {
			t.Fatal(err)
		}
		m.Dependencies = map[string]wfdir.Dependency{"orders": {Kind: "table"}}
		if err := wfdir.SaveManifest(fx.root, m); err != nil {
			t.Fatal(err)
		}
		out, err := runCLI(t, fx.tree, "sync", "check", "--stack", "prod", "--json")
		if code := exitCodeOf(err); code != syncExitClean {
			t.Fatalf("exit = %d (%v)\n%s", code, err, out)
		}
		folder := checkFolders(t, out)[0]
		warnings, _ := folder["warnings"].([]any)
		if len(edgesOf(t, folder)) != 0 || len(warnings) != 1 || !strings.Contains(warnings[0].(string), "orders") {
			t.Fatalf("folder = %v", folder)
		}
	})
}

// THE APPLY ARMS, on a module folder that has never been pushed: ONE creation
// unit, refused without --allow-create (nothing is created), listed by the
// dry-run, and with the flag created, pushed AND published — after which a
// second apply sends nothing.
func TestSyncApplyModuleCreate(t *testing.T) {
	f := newFakeInstance(t)
	signIn(t, f)
	tree := t.TempDir()
	root := filepath.Join(tree, "tracker")
	if err := os.MkdirAll(root, 0o755); err != nil {
		t.Fatal(err)
	}
	if out, err := runCLI(t, root, "module", "init", "emab_tracker", "--feature", "collection-1"); err != nil {
		t.Fatalf("module init: %v\n%s", err, out)
	}
	writeLocal(t, root, "tracker.py", "x = 1\n")

	out, err := runCLI(t, tree, "sync", "apply", "--json")
	line := applyFolderLine(t, applyReportOf(t, out), "tracker")
	if exitCodeOf(err) != syncExitDrifted || line.Reason != syncReasonWouldCreate || len(f.mod.created) != 0 {
		t.Fatalf("without --allow-create: exit %d, %+v, created %+v\n%s", exitCodeOf(err), line, f.mod.created, out)
	}
	if len(line.Units) != 1 || line.Units[0].Action != string(applyActionCreate) || line.Units[0].Path != "" {
		t.Fatalf("units = %+v, want the folder as one create", line.Units)
	}

	f.mu.Lock()
	f.Requests = nil
	f.mu.Unlock()
	out, err = runCLI(t, tree, "sync", "apply", "--dry-run", "--allow-create", "--json")
	report := applyReportOf(t, out)
	line = applyFolderLine(t, report, "tracker")
	if err != nil || line.Verdict != verdictApplied || report.Summary.Created != 1 ||
		len(line.Units) != 1 || line.Units[0].Action != string(applyActionCreate) {
		t.Fatalf("dry-run: %v, %+v, summary %+v\n%s", err, line, report.Summary, out)
	}
	if writes := writesIn(f.Requests); len(writes) != 0 || len(f.mod.created) != 0 {
		t.Fatalf("a dry-run sent %v", writes)
	}

	out, err = runCLI(t, tree, "sync", "apply", "--allow-create", "--json")
	report = applyReportOf(t, out)
	line = applyFolderLine(t, report, "tracker")
	if err != nil || line.Verdict != verdictApplied || report.Summary.Created != 1 {
		t.Fatalf("--allow-create: %v, %+v\n%s", err, line, out)
	}
	if len(f.mod.created) != 1 || len(f.mod.committed) != 1 || len(f.mod.draftOf) != 0 {
		t.Fatalf("created %d, committed %d, drafts %v — want one module, published, nothing left open",
			len(f.mod.created), len(f.mod.committed), f.mod.draftOf)
	}
	if got := f.ModuleFileContents("module-new-1")["tracker.py"]; got != "x = 1\n" {
		t.Fatalf("the live module holds tracker.py = %q", got)
	}

	f.mu.Lock()
	f.Requests = nil
	f.mu.Unlock()
	out, err = runCLI(t, tree, "sync", "apply", "--json")
	if err != nil {
		t.Fatalf("second apply: %v\n%s", err, out)
	}
	if writes := writesIn(f.Requests); len(writes) != 0 {
		t.Fatalf("the apply after the create sent %v\n%s", writes, out)
	}
	if line := applyFolderLine(t, applyReportOf(t, out), "tracker"); len(line.Units) != 1 || line.Units[0].Action != string(applyActionUpdate) {
		t.Fatalf("units = %+v, want the bound module as one update", line.Units)
	}
}

// A MODULE DRAFT AN EARLIER RUN LEFT STAGED IS PUBLISHED BY THE NEXT. Run one
// pushes an edit into a draft and its commit is refused; run two has nothing to
// push and must still publish — the publish gate is "did the draft pre-exist",
// never "did the push write".
func TestSyncApplyPublishesAModuleDraftAnEarlierRunLeftBehind(t *testing.T) {
	fx := moduleIdempotenceFixture(t)
	writeLocal(t, fx.root, "tracker.py", "NEW\n")
	fx.draftKind.failCommit(403)
	if code, line, _, out := applyOnce(t, fx); code != syncExitDrifted {
		t.Fatalf("run one: exit %d %+v\n%s", code, line, out)
	}
	fx.draftKind.failCommit(0)
	code, line, _, out := applyOnce(t, fx)
	if code != syncExitClean || line.Verdict != verdictApplied || fx.draftKind.commits() != 1 {
		t.Fatalf("run two: exit %d %+v, commits %d\n%s", code, line, fx.draftKind.commits(), out)
	}
	if got := fx.draftKind.liveMain(); got != "NEW\n" {
		t.Fatalf("live tracker.py = %q — reported deployed while the edit sat in a draft\n%s", got, out)
	}
}

// A `dependencies` block in a module folder is inert, and apply says so as a
// note rather than refusing it as an unbound alias: `module push` never reads
// the block, and `ronja bind` — the remedy that refusal would name — refuses a
// module folder.
func TestSyncApplyNotesAModulesInertDependencies(t *testing.T) {
	fx := moduleIdempotenceFixture(t)
	m, err := wfdir.LoadManifest(fx.root, wfdir.ModuleKind)
	if err != nil {
		t.Fatal(err)
	}
	m.Dependencies = map[string]wfdir.Dependency{"orders": {Kind: "table"}}
	if err := wfdir.SaveManifest(fx.root, m); err != nil {
		t.Fatal(err)
	}
	code, line, writes, out := applyOnce(t, fx)
	if code != syncExitClean || line.Verdict != verdictApplied || len(writes) != 0 {
		t.Fatalf("exit %d %+v, writes %v\n%s", code, line, writes, out)
	}
	if len(line.Notes) != 1 || !strings.Contains(line.Notes[0], "orders") {
		t.Fatalf("notes = %v, want the inert-dependencies note", line.Notes)
	}
}

// A shared-feature module is created as a PROPOSAL, and `module publish`
// reports that with a nil error. Apply must not read it as deployed.
func TestSyncApplyDoesNotCallAProposedModuleDeployed(t *testing.T) {
	f := newFakeInstance(t)
	signInAsModuleAdmin(t, f)
	f.mod.sharedFeature = true
	tree := t.TempDir()
	root := filepath.Join(tree, "tracker")
	if err := os.MkdirAll(root, 0o755); err != nil {
		t.Fatal(err)
	}
	if out, err := runCLI(t, root, "module", "init", "emab_tracker", "--feature", "collection-1"); err != nil {
		t.Fatalf("module init: %v\n%s", err, out)
	}
	out, err := runCLI(t, tree, "sync", "apply", "--allow-create", "--json")
	line := applyFolderLine(t, applyReportOf(t, out), "tracker")
	if exitCodeOf(err) != syncExitDrifted || line.Reason != syncReasonSubmittedForReview {
		t.Fatalf("exit %d, %+v\n%s", exitCodeOf(err), line, out)
	}
}

// THE PREFLIGHT ARM. A module push refuses a folder without its __init__.py and
// one carrying a marker before its first byte; the dry-run says so too, under
// the same reason — and never the workflow arm's missing `main.py`.
func TestSyncApplyDryRunAgreesOnTheModuleRefusals(t *testing.T) {
	t.Run("no __init__.py", func(t *testing.T) {
		fx := moduleIdempotenceFixture(t)
		if err := os.Remove(filepath.Join(fx.root, moduleInitFile)); err != nil {
			t.Fatal(err)
		}
		assertApplyAgrees(t, fx.tree, filepath.Base(fx.root), syncReasonMissingEntrypoint, "--stack", "prod")
		if line := applyFolderLine(t, applyReportOf(t, mustApplyDry(t, fx.tree)), filepath.Base(fx.root)); !strings.Contains(line.Detail, moduleInitFile) {
			t.Errorf("detail = %q, want the module's own refusal", line.Detail)
		}
	})
	t.Run("a marker", func(t *testing.T) {
		fx := moduleIdempotenceFixture(t)
		writeLocal(t, fx.root, "tracker.py", "TABLE = \"{{ ref('table-orders') }}\"\n")
		assertApplyAgrees(t, fx.tree, filepath.Base(fx.root), syncReasonModuleMarker, "--stack", "prod")
	})
	t.Run("a create with no package name", func(t *testing.T) {
		f := newFakeInstance(t)
		signIn(t, f)
		tree := t.TempDir()
		seedManifest(t, filepath.Join(tree, "tracker"), wfdir.ModuleKind, &wfdir.Manifest{Title: "Tracker",
			Stacks: map[string]wfdir.Stack{"prod": {URL: f.URL(), TenantID: testTenantID, FeatureID: "collection-1"}}})
		writeLocal(t, filepath.Join(tree, "tracker"), moduleInitFile, "\n")
		assertApplyAgrees(t, tree, "tracker", syncReasonNoModuleName, "--stack", "prod", "--allow-create")
		if len(f.mod.created) != 0 {
			t.Fatalf("created %+v", f.mod.created)
		}
	})
}

func mustApplyDry(t *testing.T, tree string) string {
	t.Helper()
	out, _ := runCLI(t, tree, "sync", "apply", "--dry-run", "--stack", "prod", "--json")
	return out
}

// THE MANAGED LEGS, as they read for a module: adopt claims the module row at
// the folder's one address ("", resourceKind "module") with its anchor, and
// unmanage releases it and leaves the module recorded in the lock.
func TestSyncAdoptAndUnmanageModuleFolder(t *testing.T) {
	fx := moduleIdempotenceFixture(t)
	m := fx.mod.managed
	adoptFixture(t, fx)
	manifest := loadAnyManifest(t, fx.root)
	if !manifest.Adopted() || manifest.FormatVersion != 6 {
		t.Fatalf("adopt minted no id at format version 6: %+v", manifest)
	}
	c := m.ClaimAt("collection-1", manifest.ID, "")
	if c == nil || c.Kind != wfdir.ManagedModule || c.ResourceID != "module-1" || c.Applied["headVersionID"] != "modv-1" {
		t.Fatalf("claim = %+v", c)
	}

	out, err := runCLI(t, fx.tree, "sync", "unmanage", "--stack", "prod", "--json")
	if err != nil {
		t.Fatalf("unmanage: %v\n%s", err, out)
	}
	if got := m.Claims("collection-1"); len(got) != 0 {
		t.Fatalf("claims left: %+v", got)
	}
	if loadAnyManifest(t, fx.root).Adopted() {
		t.Fatal("the folder id was not removed")
	}
	if id := mustLoadLock(t, fx.root).Binding("prod").ModuleID; id != "module-1" {
		t.Fatalf("the lock records module %q after unmanage, want module-1", id)
	}
	if row := fx.mod.mod.rows["module-1"]; row == nil || row.Lifecycle != api.LifecycleLive {
		t.Fatalf("unmanage touched the module row: %+v", row)
	}
}

// THE SERVER WINS for a module lock too: a lock naming another module is
// rewritten from the claim by an acting run, and the deploy proceeds on the
// claimed row with the claim's anchor.
func TestManagedServerWinsOverADisagreeingModuleLock(t *testing.T) {
	fx := moduleIdempotenceFixture(t)
	adoptFixture(t, fx)
	editLock(t, fx.root, func(l *wfdir.Lock) {
		if err := l.SetManaged("prod", wfdir.ModuleKind, "", "module-stale", map[string]any{}); err != nil {
			t.Fatal(err)
		}
	})
	code, line, writes, out := applyOnce(t, fx)
	if code != syncExitClean || line.Verdict != verdictApplied {
		t.Fatalf("apply: exit %d %+v\n%s", code, line, out)
	}
	for _, w := range writes {
		if !strings.Contains(w, "/api/v2/managed") {
			t.Fatalf("an unchanged module sent %s", w)
		}
	}
	lock := mustLoadLock(t, fx.root)
	if id := lock.Binding("prod").ModuleID; id != "module-1" {
		t.Fatalf("lock still records %q, want the server's module-1", id)
	}
	if lock.HeadVersion("prod") == "" {
		t.Fatal("the rewritten lock has no anchor: the drift guard is disarmed")
	}
}

// `module init --stack` mints a folder id like every other kind's init, and
// the first push's create claims the folder's address.
func TestModuleInitOnAStackMintsAFolderID(t *testing.T) {
	f := newFakeInstance(t)
	signIn(t, f)
	root := t.TempDir()
	if out, err := runCLI(t, root, "module", "init", "emab_tracker", "--feature", "collection-1", "--stack", "dev"); err != nil {
		t.Fatalf("init: %v\n%s", err, out)
	}
	id := folderIDOf(t, root)
	if v := loadAnyManifest(t, root).FormatVersion; v != 6 {
		t.Fatalf("init --stack wrote an id at format version %d, want 6", v)
	}
	if out, err := runCLI(t, root, "module", "push", "--json"); err != nil {
		t.Fatalf("push: %v\n%s", err, out)
	}
	if len(f.mod.created) != 1 || f.mod.created[0].Managed == nil || f.mod.created[0].Managed.FolderID != id {
		t.Fatalf("the create carried no claim for the minted id: %+v", f.mod.created)
	}
	if c := f.managed.ClaimAt("collection-1", id, ""); c == nil || c.Kind != wfdir.ManagedModule {
		t.Fatalf("claim = %+v", c)
	}

	plain := t.TempDir()
	if _, err := runCLI(t, plain, "module", "init", "emab_tracker", "--feature", "collection-1"); err != nil {
		t.Fatalf("init without a stack: %v", err)
	}
	if strings.Contains(readFile(t, wfdir.ManifestPath(plain)), `"id"`) {
		t.Fatalf("init without --stack minted a folder id:\n%s", readFile(t, wfdir.ManifestPath(plain)))
	}
}
