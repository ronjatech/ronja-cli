package commands

import (
	"context"
	"strings"
	"testing"

	"github.com/ronjatech/ronja-cli/internal/wfdir"
)

// `sync apply` and METRIC files: every metric a push would create is a counted,
// gated creation unit, exactly as a .sql file with no table behind it is.
//
// The hole these pin: the units used to come from the .sql files alone, the
// metric pass ran later in the same push, and nothing on that path asked the
// gate — so an apply WITHOUT --allow-create created metrics, never listed them
// in the dry-run and never counted them.

// aovMetricPath is the metric file these tests lay down. Its recipe reads the
// folder's own orders.sql by stem, which is the main case.
const aovMetricPath = "metrics/aov.json"

// metricApplyTree is applyPipelineTree with one bound .sql file and one metric
// file — bound to metricID when it is not empty, new otherwise.
func metricApplyTree(t *testing.T, f *fakePipelineInstance, metricID string) (tree, root string) {
	t.Helper()
	tree, root = applyPipelineTree(t, f,
		map[string]string{
			"orders.sql":  "SELECT * FROM raw",
			aovMetricPath: metricFileBody(aovRecipe),
		},
		map[string]string{"orders.sql": "table-orders"},
		nil)
	if metricID != "" {
		lock, err := wfdir.LoadLock(root)
		if err != nil {
			t.Fatal(err)
		}
		lock.SetMetricSeen("prod", aovMetricPath, metricID, "", "")
		if err := wfdir.SaveLock(root, lock); err != nil {
			t.Fatal(err)
		}
	}
	return tree, root
}

func unitActions(line syncApplyFolderReport) map[string]string {
	out := map[string]string{}
	for _, unit := range line.Units {
		out[unit.Path] = unit.Action
	}
	return out
}

// THE PLAN GATE. A metric file with no metric behind it refuses the folder as a
// create BEFORE the push runs — so not even the folder's SQL is published, which
// is what every other create unit earns.
//
// The reason is asserted, not just the absence of a create: with the plan gate
// gone, the push-side gate below still stops the metric, but the folder then
// reports `not_deployed` AFTER publishing its table — a different and later
// refusal than the one apply promises.
func TestSyncApplyRefusesAMetricCreateWithoutAllowCreate(t *testing.T) {
	f := newFakePipelineInstance(t)
	signInPipeline(t, f)
	seedFeature(f, "private")
	tree, _ := metricApplyTree(t, f, "")

	out, err := runCLI(t, tree, "sync", "apply", "--stack", "prod", "--json")
	if len(f.metricsCreated) != 0 {
		t.Fatalf("apply created %d metric(s) without --allow-create: %+v\n%s", len(f.metricsCreated), f.metricsCreated, out)
	}
	if code := exitCodeOf(err); code != syncExitDrifted {
		t.Fatalf("exit = %d (%v), want %d\n%s", code, err, syncExitDrifted, out)
	}
	line := applyFolderLine(t, applyReportOf(t, out), "sales")
	if line.Verdict != verdictRefused || line.Reason != syncReasonWouldCreate {
		t.Fatalf("folder = %+v, want %s/%s", line, verdictRefused, syncReasonWouldCreate)
	}
	if !strings.Contains(line.Detail, aovMetricPath) {
		t.Errorf("the refusal does not name the metric file: %q", line.Detail)
	}
	if got := unitActions(line); got[aovMetricPath] != string(applyActionCreate) || got["orders.sql"] != string(applyActionUpdate) {
		t.Errorf("units = %+v, want the metric as a create beside the bound table", line.Units)
	}
	if len(f.committed) != 0 || len(f.checkouts) != 0 {
		t.Errorf("a refused folder still wrote: checkouts=%v committed=%v", f.checkouts, f.committed)
	}
}

// THE DRY-RUN LISTS IT. The preview reads the same decisions the apply acts on,
// so a metric that would be created is a `create` unit there too, counted in the
// summary — not invisible until the run that makes it.
func TestSyncApplyDryRunListsAMetricCreate(t *testing.T) {
	f := newFakePipelineInstance(t)
	signInPipeline(t, f)
	seedFeature(f, "private")
	tree, _ := metricApplyTree(t, f, "")

	out, err := runCLI(t, tree, "sync", "apply", "--stack", "prod", "--dry-run", "--json")
	if code := exitCodeOf(err); code != syncExitDrifted {
		t.Fatalf("exit = %d (%v), want %d\n%s", code, err, syncExitDrifted, out)
	}
	if len(f.metricsCreated) != 0 || len(f.checkouts) != 0 {
		t.Fatalf("--dry-run wrote: metrics=%v checkouts=%v", f.metricsCreated, f.checkouts)
	}
	report := applyReportOf(t, out)
	if got := unitActions(applyFolderLine(t, report, "sales")); got[aovMetricPath] != string(applyActionCreate) {
		t.Fatalf("the dry-run does not list the metric create: %+v", applyFolderLine(t, report, "sales").Units)
	}
	if report.Summary.Creates != 1 {
		t.Errorf("summary = %+v, want the one metric counted as a creation unit", report.Summary)
	}
}

// --allow-create LETS IT THROUGH AND COUNTS IT. The number named before the
// first write includes the metric, and summary.created reports the row that
// landed — a run that made a metric and said "0 rows created" would be the
// report contradicting the organization.
func TestSyncApplyAllowCreateCountsAMetricCreate(t *testing.T) {
	f := newFakePipelineInstance(t)
	signInPipeline(t, f)
	seedFeature(f, "private")
	tree, _ := metricApplyTree(t, f, "")

	var out string
	var err error
	stderr := captureStderr(t, func() {
		out, err = runCLI(t, tree, "sync", "apply", "--stack", "prod", "--allow-create", "--json")
	})
	if code := exitCodeOf(err); code != syncExitClean {
		t.Fatalf("exit = %d (%v), want %d\n%s\n%s", code, err, syncExitClean, out, stderr)
	}
	if len(f.metricsCreated) != 1 {
		t.Fatalf("metricsCreated = %+v, want the one new metric", f.metricsCreated)
	}
	if !strings.Contains(stderr, "will create 1 row") {
		t.Errorf("the metric was not in the count named before the writes:\n%s", stderr)
	}
	summary := applyReportOf(t, out).Summary
	if summary.Creates != 1 || summary.Created != 1 {
		t.Errorf("summary = %+v, want one creation unit and one row created", summary)
	}
}

// A BOUND METRIC IS AN UPDATE, and needs no flag: the unit grain has to tell a
// metric the lock already records from one it does not, or every folder that
// defines a metric would be refused for ever.
func TestSyncApplyTreatsABoundMetricAsAnUpdate(t *testing.T) {
	f := newFakePipelineInstance(t)
	signInPipeline(t, f)
	seedFeature(f, "private")
	f.AddMetric("table-aov", "aov", `{"source":"table-orders","value":"revenue / orders","version":1}`)
	tree, _ := metricApplyTree(t, f, "table-aov")

	out, err := runCLI(t, tree, "sync", "apply", "--stack", "prod", "--dry-run", "--json")
	if code := exitCodeOf(err); code != syncExitClean {
		t.Fatalf("exit = %d (%v), want %d\n%s", code, err, syncExitClean, out)
	}
	line := applyFolderLine(t, applyReportOf(t, out), "sales")
	if got := unitActions(line); got[aovMetricPath] != string(applyActionUpdate) {
		t.Fatalf("units = %+v, want the bound metric as an update", line.Units)
	}
}

// THE PUSH-SIDE GATE, on the path that actually creates. Apply's plan refuses a
// create unit before the push runs; this is the same refusal inside the push, so
// the property never rests on the plan and the push agreeing about which files
// are units. A metric WAS created here once, by an apply that promised none.
func TestPipelinePushNoCreateRefusesEveryCreate(t *testing.T) {
	f := newFakePipelineInstance(t)
	signInPipeline(t, f)
	seedFeature(f, "private")
	_, root := applyPipelineTree(t, f,
		map[string]string{
			"orders.sql":  "SELECT * FROM raw WHERE shipped",
			"refunds.sql": "SELECT * FROM raw WHERE refunded",
			aovMetricPath: metricFileBody(aovRecipe),
		},
		map[string]string{"orders.sql": "table-orders"},
		nil)
	t.Chdir(root)
	stack := flagStack
	flagStack = "prod"
	t.Cleanup(func() { flagStack = stack })
	resolved, err := resolveInstance()
	if err != nil {
		t.Fatal(err)
	}
	folder, err := openFolder(context.Background(), resolved, wfdir.PipelineKind)
	if err != nil {
		t.Fatal(err)
	}

	result, _ := runPipelinePush(context.Background(), folder, nil, pipelinePushOptions{NoCreate: true})
	if len(f.metricsCreated) != 0 || len(f.created) != 0 {
		t.Fatalf("a NoCreate push created: tables=%+v metrics=%+v", f.created, f.metricsCreated)
	}
	if result == nil {
		t.Fatal("no result")
	}
	outcomes := map[string]string{}
	for _, file := range result.Files {
		outcomes[file.Path] = file.Outcome
	}
	for _, metric := range result.Metrics {
		outcomes[metric.Path] = metric.Outcome
	}
	if outcomes["refunds.sql"] != pushOutcomeRefused || outcomes[aovMetricPath] != metricOutcomeRefused {
		t.Errorf("outcomes = %+v, want both creates refused", outcomes)
	}
	// The bound file is an update, and NoCreate leaves updates alone.
	if outcomes["orders.sql"] != pushOutcomePushed {
		t.Errorf("orders.sql = %q, want pushed — NoCreate must not refuse an update", outcomes["orders.sql"])
	}
	// And through applyFolder, the way apply reaches the push, with
	// --allow-create absent — so the WIRING is under test as well as the gate:
	// a plan and a push that disagreed would otherwise be held only by the plan.
	allow := flagSyncAllowCreate
	flagSyncAllowCreate = false
	t.Cleanup(func() { flagSyncAllowCreate = allow })
	if _, created, _, _, _ := applyFolder(context.Background(), folder, nil); created != 0 {
		t.Fatalf("applyFolder reported %d row(s) created", created)
	}
	if len(f.metricsCreated) != 0 || len(f.created) != 0 {
		t.Fatalf("applyFolder without --allow-create created: tables=%+v metrics=%+v", f.created, f.metricsCreated)
	}
}

// AN ADOPTED METRIC IS NOT A ROW THIS RUN CREATED. The create was refused
// name_taken and the metric holding the name — made by an earlier push that
// lost its recording, so holding exactly this file's definition — was adopted.
// The summary must not report it as created, nor as a row left uncreated: its
// unit is the update of a row that was there all along.
func TestSyncApplyDoesNotCountAnAdoptedMetricAsCreated(t *testing.T) {
	f := newFakePipelineInstance(t)
	signInPipeline(t, f)
	seedFeature(f, "private")
	tree, _ := metricApplyTree(t, f, "")
	lost := f.AddMetric("table-aov", "aov", aovResolved)
	lost.FeatureID = "collection-1"
	f.nameTakenOnMetricCreate = `a table or metric called "aov" already exists in this feature`

	out, err := runCLI(t, tree, "sync", "apply", "--stack", "prod", "--allow-create", "--json")
	if code := exitCodeOf(err); code != syncExitClean {
		t.Fatalf("exit = %d (%v), want %d\n%s", code, err, syncExitClean, out)
	}
	report := applyReportOf(t, out)
	if report.Summary.Created != 0 {
		t.Errorf("summary.created = %d, want 0 — the adopted metric was made by an earlier push", report.Summary.Created)
	}
	if report.Summary.Creates != 0 {
		t.Errorf("summary.creates = %d, want 0 — the adopted metric is not a row left uncreated", report.Summary.Creates)
	}
	line := applyFolderLine(t, report, "sales")
	if got := unitActions(line); got[aovMetricPath] != string(applyActionUpdate) {
		t.Errorf("units = %+v, want the adopted metric reported as an update", line.Units)
	}
}

// THE ADMIN IS ASKED TO CREATE THE NAME THE PUSH WOULD HAVE CREATED: the
// declared "name" when the file has one, not the file's stem.
func TestPipelineMetricCreateRefusalNamesTheDeclaredName(t *testing.T) {
	f := newFakePipelineInstance(t)
	signInPipeline(t, f)
	seedFeature(f, "private")
	_, root := metricApplyTree(t, f, "")
	writeMetricFile(t, root, namedMetricBody(aovName))
	f.failMetricCreate = 403

	out, err := runCLI(t, root, "pipeline", "push", "--stack", "prod", "--json")
	if err == nil {
		t.Fatalf("a refused create went through:\n%s", out)
	}
	if !strings.Contains(out, `create \"`+aovName+`\"`) && !strings.Contains(out, `create "`+aovName+`"`) {
		t.Errorf("the refusal does not ask for the declared name %q:\n%s", aovName, out)
	}
}
