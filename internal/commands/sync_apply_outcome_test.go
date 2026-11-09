package commands

import (
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/ronjatech/ronja-cli/internal/api"
	"github.com/ronjatech/ronja-cli/internal/wfdir"
)

func unitByPath(t *testing.T, line syncApplyFolderReport, path string) syncApplyUnitReport {
	t.Helper()
	for _, u := range line.Units {
		if u.Path == path {
			return u
		}
	}
	t.Fatalf("no unit %q in %+v", path, line.Units)
	return syncApplyUnitReport{}
}

// A folder whose automation loop created one file, was refused another and
// created a third reports each unit's own outcome under its refused verdict.
func TestSyncApplyReportsAPartlyAppliedAutomationFolder(t *testing.T) {
	f := newFakeAutomationInstance(t)
	signInAutomation(t, f)
	f.failCreateNamed = map[string]int{"b_nightly": http.StatusBadRequest}
	tree := t.TempDir()
	root := filepath.Join(tree, "orders")
	if err := os.MkdirAll(root, 0o755); err != nil {
		t.Fatal(err)
	}
	manifest, lock := automationStack(f, "collection-1", nil, nil)
	writeAutomationFolder(t, root, manifest, lock, map[string]string{
		"a_hook.json":    `{"triggerKind": "webhook", "prompt": "Handle the delivery."}`,
		"b_nightly.json": `{"cronExpr": "0 2 * * *", "prompt": "Run the nightly."}`,
		"c_daily.json":   `{"cronExpr": "0 6 * * *", "prompt": "Run the daily."}`,
	})

	out, err := runCLI(t, tree, "sync", "apply", "--stack", "prod", "--allow-create", "--json")
	if code := exitCodeOf(err); code != syncExitDrifted {
		t.Fatalf("exit = %d (%v), want %d\n%s", code, err, syncExitDrifted, out)
	}
	line := applyFolderLine(t, applyReportOf(t, out), "orders")
	if line.Verdict != verdictRefused {
		t.Fatalf("verdict = %s, want refused\n%s", line.Verdict, out)
	}
	for path, want := range map[string]string{
		"a_hook.json": unitOutcomeApplied, "b_nightly.json": unitOutcomeRefused, "c_daily.json": unitOutcomeApplied,
	} {
		if got := unitByPath(t, line, path); got.Outcome != want {
			t.Errorf("%s: outcome %q (%s), want %q", path, got.Outcome, got.OutcomeDetail, want)
		}
	}
	if got := unitByPath(t, line, "b_nightly.json").OutcomeDetail; got == "" {
		t.Error("the refused unit says nothing about why")
	}
}

// checks_not_applied refuses the folder with its tables live: the SQL units
// say applied.
func TestSyncApplyChecksNotAppliedKeepsTheTablesApplied(t *testing.T) {
	f := newFakePipelineInstance(t)
	signInPipeline(t, f)
	seedFeature(f, "private")
	f.AddTable(&api.Table{ID: "table-gl", Name: "gl_lines", FeatureID: "collection-1", Code: glSQL})
	gammal := f.AddCheck("table-gl", api.TableCheck{Name: "Gammal", Expression: "true", Enabled: true})
	tree, _ := checksApplyTree(t, f, "SELECT * FROM raw WHERE shipped", glSQL, oneCheck, gammal)

	out, _ := runCLI(t, tree, "sync", "apply", "--stack", "prod", "--json")
	line := applyFolderLine(t, applyReportOf(t, out), "sales")
	if line.Reason != syncReasonChecksNotApplied {
		t.Fatalf("line = %+v", line)
	}
	for _, u := range line.Units {
		if strings.HasSuffix(u.Path, ".sql") && u.Outcome != unitOutcomeApplied {
			t.Errorf("%s: outcome %q, want applied: its SQL was published", u.Path, u.Outcome)
		}
	}
}

// A dry run reports no outcome.
func TestSyncApplyDryRunHasNoUnitOutcome(t *testing.T) {
	f := newFakeAutomationInstance(t)
	signInAutomation(t, f)
	tree := t.TempDir()
	root := filepath.Join(tree, "orders")
	if err := os.MkdirAll(root, 0o755); err != nil {
		t.Fatal(err)
	}
	manifest, lock := automationStack(f, "collection-1", nil, nil)
	writeAutomationFolder(t, root, manifest, lock, map[string]string{"daily.json": `{"cronExpr": "0 6 * * *", "prompt": "Run."}`})
	out, _ := runCLI(t, tree, "sync", "apply", "--stack", "prod", "--allow-create", "--dry-run", "--json")
	for _, u := range applyFolderLine(t, applyReportOf(t, out), "orders").Units {
		if u.Outcome != "" {
			t.Fatalf("a dry run reported outcome %q for %s", u.Outcome, u.Path)
		}
	}
}

// A folder interrupted mid-write is unknown: its units get no outcome, since
// the write may have landed.
func TestSettleUnitOutcomesLeavesAnInterruptedWriteOpen(t *testing.T) {
	line := syncApplyFolderReport{Verdict: verdictUnknown, Reason: syncReasonInterrupted,
		Units: []syncApplyUnitReport{{Path: "main.py"}}}
	settleUnitOutcomes(&line, wfdir.WorkflowKind, nil, nil)
	if got := line.Units[0].Outcome; got != "" {
		t.Fatalf("outcome %q, want none: the write may have landed", got)
	}
}

// A pipeline folder that published part of itself, as the change-list job
// deploys one: a table and a metric published, a table live already held, and
// a table whose commit was refused. Each unit reports its own outcome under the
// folder's refused verdict.
func TestSyncApplyReportsAPartlyPublishedPipelineFolder(t *testing.T) {
	f := newFakePipelineInstance(t)
	signInPipeline(t, f)
	f.AddFeature("collection-1", "Sales pipeline", "private")
	f.AddTable(&api.Table{ID: "table-orders", Name: "orders", FeatureID: "collection-1", Code: "SELECT * FROM raw"})
	f.AddTable(&api.Table{ID: "table-refunds", Name: "refunds", FeatureID: "collection-1", Code: "SELECT * FROM raw WHERE refunded"})
	f.AddTable(&api.Table{ID: "table-gl", Name: "gl", FeatureID: "collection-1", Code: "SELECT * FROM raw WHERE booked"})
	f.AddMetric("metric-aov", "aov", strings.Replace(aovRecipe, `"source":"orders"`, `"source":"table-orders"`, 1)).
		ReportingTimezone = "Europe/Stockholm"
	dir := t.TempDir()
	if _, stderr, err := runPipelineCLI(t, dir, "pipeline", "clone", "collection-1", "out", "--stack", "prod", "--json"); err != nil {
		t.Fatalf("clone: %v\n%s", err, stderr)
	}
	root := filepath.Join(dir, "out")
	// As the deploy job's copy: no local baseline, so every file is asked
	// whether live already holds it.
	if err := os.RemoveAll(filepath.Join(root, ".ronja")); err != nil {
		t.Fatal(err)
	}
	writeLocal(t, root, "orders.sql", "SELECT * FROM raw WHERE shipped")
	writeLocal(t, root, "gl.sql", "SELECT * FROM raw WHERE booked AND posted")
	writeLocal(t, root, "metrics/aov.json", metricFileWithZone(
		`{"value":"revenue / orders","source":"table-orders","base_measures":[{"name":"revenue","agg":"sum","column":"net_amount"}]}`,
		"Europe/Stockholm"))
	// Someone writes gl's draft between the check and its commit, so the
	// server refuses that one commit.
	f.beforeServe = func(r *http.Request) {
		id, ok := strings.CutSuffix(strings.TrimPrefix(r.URL.Path, "/api/v2/feature/model/"), "/commit")
		if !ok || r.Method != http.MethodPost {
			return
		}
		f.mu.Lock()
		defer f.mu.Unlock()
		if row := f.tables[id]; row != nil && row.ParentModelID == "table-gl" {
			row.UpdatedAt = row.UpdatedAt.Add(time.Minute)
		}
	}

	out, _ := runCLI(t, dir, "sync", "apply", "--stack", "prod", "--json")
	line := applyFolderLine(t, applyReportOf(t, out), "out")
	if line.Verdict != verdictRefused {
		t.Fatalf("verdict %s (%s), want refused\n%s", line.Verdict, line.Detail, out)
	}
	if f.CodeOf("table-orders") != "SELECT * FROM raw WHERE shipped" || !strings.Contains(f.RecipeOf("metric-aov"), "net_amount") {
		t.Fatalf("orders or aov did not go live\n%s", out)
	}
	for path, want := range map[string]string{
		"orders.sql":       unitOutcomeApplied,
		"refunds.sql":      unitOutcomeApplied,
		"metrics/aov.json": unitOutcomeApplied,
		"gl.sql":           unitOutcomeRefused,
	} {
		if got := unitByPath(t, line, path); got.Outcome != want {
			t.Errorf("%s: outcome %q (%s), want %q", path, got.Outcome, got.OutcomeDetail, want)
		}
	}
}
