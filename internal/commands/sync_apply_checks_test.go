package commands

import (
	"os"
	"strings"
	"sync"
	"testing"

	"github.com/ronjatech/ronja-cli/internal/api"
	"github.com/ronjatech/ronja-cli/internal/wfdir"
)

// `ronja sync apply` and a pipeline folder's HEALTH CHECKS: the checks leg is
// ungoverned and advisory, so it never holds the folder's SQL publish back —
// and it still fails the run, as checks_not_applied.

// checksApplyTree is a stack-shaped tree whose folder builds orders.sql (live
// "SELECT * FROM raw") and gl_lines.sql (live glSQL) and keeps
// checks/gl_lines.json. The lock records that the folder owns `owned` on
// table-gl, so a check the file no longer declares is an orphan.
func checksApplyTree(t *testing.T, f *fakePipelineInstance, ordersSQL, glFile, checks string, owned ...*api.TableCheck) (tree, root string) {
	t.Helper()
	seedFeature(f, "private")
	f.AddTable(&api.Table{ID: "table-gl", Name: "gl_lines", FeatureID: "collection-1", Code: glSQL})
	tree, root = applyPipelineTree(t, f,
		map[string]string{"orders.sql": ordersSQL, "gl_lines.sql": glFile, "checks/gl_lines.json": checks},
		map[string]string{"orders.sql": "table-orders", "gl_lines.sql": "table-gl"},
		nil)
	// A baseline at the live SQL, so a push visits only what really changed —
	// without one every file is pushed and every table's checks wait on its
	// publish, and the push leg is never exercised.
	writePipelineBaseline(t, root, f.Key(), "collection-1",
		map[string]string{"orders.sql": "SELECT * FROM raw", "gl_lines.sql": glSQL},
		map[string]wfdir.TableState{"orders.sql": {TableID: "table-orders"}, "gl_lines.sql": {TableID: "table-gl"}})
	if len(owned) > 0 {
		lock, err := wfdir.LoadLock(root)
		if err != nil {
			t.Fatal(err)
		}
		managed := map[string]wfdir.LockCheckEntry{}
		for _, c := range owned {
			managed[strings.ToLower(c.Name)] = wfdir.LockCheckEntry{CheckID: c.ID}
		}
		lock.SetChecksSeen("prod", "checks/gl_lines.json", "table-gl", managed)
		if err := wfdir.SaveLock(root, lock); err != nil {
			t.Fatal(err)
		}
	}
	return tree, root
}

const oneCheck = `{"checks":[{"name":"Unik rad per id","expression":"count(*) = count(distinct id)"}]}`

// THE PUBLISH LEG. An orphan on a table whose SQL is staged: the push holds the
// checks (pending publish), the publish commits the SQL and then refuses the
// checks. The commit stands, and the verdict says so — checks_not_applied, exit 1.
func TestSyncApplyPublishesTheSQLPastAChecksOrphanAfterTheCommit(t *testing.T) {
	f := newFakePipelineInstance(t)
	signInPipeline(t, f)
	seedFeature(f, "private")
	f.AddTable(&api.Table{ID: "table-gl", Name: "gl_lines", FeatureID: "collection-1", Code: glSQL})
	gammal := f.AddCheck("table-gl", api.TableCheck{Name: "Gammal", Expression: "true", Enabled: true})
	tree, _ := applyPipelineTree(t, f,
		map[string]string{"gl_lines.sql": "SELECT 2 AS id\n", "checks/gl_lines.json": oneCheck},
		map[string]string{"gl_lines.sql": "table-gl"}, nil)
	recordOwned(t, tree, gammal)

	out, err := runCLI(t, tree, "sync", "apply", "--stack", "prod", "--json")
	if code := exitCodeOf(err); code != syncExitDrifted {
		t.Fatalf("exit = %d (%v), want %d\n%s", code, err, syncExitDrifted, out)
	}
	if f.CodeOf("table-gl") != "SELECT 2 AS id\n" {
		t.Fatalf("the SQL was not published: %q", f.CodeOf("table-gl"))
	}
	line := applyFolderLine(t, applyReportOf(t, out), "sales")
	if line.Verdict != verdictRefused || line.Reason != syncReasonChecksNotApplied ||
		!strings.Contains(line.Detail, "gl_lines.sql") || !strings.Contains(line.Detail, "Gammal") {
		t.Fatalf("line = %+v", line)
	}
	if !f.checkNamed("table-gl", "Gammal").Enabled {
		t.Error("apply has no --prune and must not silence the orphan")
	}
	// The tree's summary must not say the folder was not deployed: its SQL is live.
	if strings.Contains(err.Error(), "was not deployed") || !strings.Contains(err.Error(), "not fully deployed") {
		t.Errorf("summary = %v", err)
	}
}

func recordOwned(t *testing.T, tree string, owned ...*api.TableCheck) {
	t.Helper()
	root := tree + "/sales"
	lock, err := wfdir.LoadLock(root)
	if err != nil {
		t.Fatal(err)
	}
	managed := map[string]wfdir.LockCheckEntry{}
	for _, c := range owned {
		managed[strings.ToLower(c.Name)] = wfdir.LockCheckEntry{CheckID: c.ID}
	}
	lock.SetChecksSeen("prod", "checks/gl_lines.json", "table-gl", managed)
	if err := wfdir.SaveLock(root, lock); err != nil {
		t.Fatal(err)
	}
}

// THE PUSH LEG. The orphan is on a table whose SQL did not move, so the push
// itself fails on it — and ONLY on it — while another table's SQL is staged.
// The staged table still publishes.
func TestSyncApplyPublishesTheSQLPastAChecksOrphanInThePush(t *testing.T) {
	f := newFakePipelineInstance(t)
	signInPipeline(t, f)
	seedFeature(f, "private")
	f.AddTable(&api.Table{ID: "table-gl", Name: "gl_lines", FeatureID: "collection-1", Code: glSQL})
	gammal := f.AddCheck("table-gl", api.TableCheck{Name: "Gammal", Expression: "true", Enabled: true})
	tree, _ := checksApplyTree(t, f, "SELECT * FROM raw WHERE shipped", glSQL, oneCheck, gammal)

	out, err := runCLI(t, tree, "sync", "apply", "--stack", "prod", "--json")
	if code := exitCodeOf(err); code != syncExitDrifted {
		t.Fatalf("exit = %d (%v), want %d\n%s", code, err, syncExitDrifted, out)
	}
	if f.CodeOf("table-orders") != "SELECT * FROM raw WHERE shipped" {
		t.Fatalf("a checks-only push problem held the SQL publish back: %q", f.CodeOf("table-orders"))
	}
	line := applyFolderLine(t, applyReportOf(t, out), "sales")
	if line.Verdict != verdictRefused || line.Reason != syncReasonChecksNotApplied || !strings.Contains(line.Detail, "orders.sql") {
		t.Fatalf("line = %+v", line)
	}
}

// NEVER ON A CANCELLED CONTEXT. A Ctrl-C during the checks pass leaves the
// unreached checks files refused — a checks-only tally — and apply must not
// read that as "only the checks" and go on to commit.
func TestSyncApplyDoesNotPublishAfterAnInterruptedChecksPass(t *testing.T) {
	f := newFakePipelineInstance(t)
	signInPipeline(t, f)
	tree, _ := checksApplyTree(t, f, "SELECT * FROM raw WHERE shipped", glSQL, oneCheck)
	var once sync.Once
	f.checkState.onList = func() {
		once.Do(func() {
			self, err := os.FindProcess(os.Getpid())
			if err == nil {
				_ = self.Signal(os.Interrupt)
			}
		})
	}

	out, err := runCLI(t, tree, "sync", "apply", "--stack", "prod", "--json")
	if code := exitCodeOf(err); code != syncExitUnknown {
		t.Fatalf("exit = %d (%v), want %d\n%s", code, err, syncExitUnknown, out)
	}
	if len(f.committed) != 0 {
		t.Fatalf("an interrupted run published %v", f.committed)
	}
	if line := applyFolderLine(t, applyReportOf(t, out), "sales"); line.Reason != syncReasonInterrupted {
		t.Fatalf("line = %+v", line)
	}
}

// A CHECKS LEG THAT GOT NO ANSWER is unknown, not refused: the read timed out.
func TestSyncApplyChecksLegTimeoutIsUnknown(t *testing.T) {
	f := newFakePipelineInstance(t)
	signInPipeline(t, f)
	tree, _ := checksApplyTree(t, f, "SELECT * FROM raw", glSQL, oneCheck)
	failOnceWithATimeout(t, "GET", "/api/v2/feature/model/table-gl/checks")

	out, err := runCLI(t, tree, "sync", "apply", "--stack", "prod", "--json")
	if code := exitCodeOf(err); code != syncExitUnknown {
		t.Fatalf("exit = %d (%v), want %d\n%s", code, err, syncExitUnknown, out)
	}
	if line := applyFolderLine(t, applyReportOf(t, out), "sales"); line.Verdict != verdictUnknown || line.Reason != syncReasonChecksNotApplied {
		t.Fatalf("line = %+v", line)
	}
}

// A CHECKS-ONLY CHANGE deploys: nothing to publish, the checks land, applied.
func TestSyncApplyDeploysAChecksOnlyChange(t *testing.T) {
	f := newFakePipelineInstance(t)
	signInPipeline(t, f)
	tree, _ := checksApplyTree(t, f, "SELECT * FROM raw", glSQL, oneCheck)

	out, err := runCLI(t, tree, "sync", "apply", "--stack", "prod", "--json")
	if code := exitCodeOf(err); code != syncExitClean {
		t.Fatalf("exit = %d (%v), want %d\n%s", code, err, syncExitClean, out)
	}
	if f.checkNamed("table-gl", "Unik rad per id") == nil {
		t.Fatal("the check was not created")
	}
	if len(f.committed) != 0 {
		t.Errorf("a checks-only change committed %v", f.committed)
	}
}
