package commands

import (
	"strings"
	"testing"

	"github.com/ronjatech/ronja-cli/internal/wfdir"
)

// A metric's NAME as a declared, converging attribute: the optional `"name"` in
// metrics/<stem>.json renames the SAME metric in place, on the table's three
// arms, and a file without one never sends a name.

const aovName = "Average order value"

func namedMetricBody(name string) string {
	return `{"recipe":` + aovRecipe + `,"name":"` + name + `"}`
}

func lockedMetricName(t *testing.T, root string) string {
	t.Helper()
	lock, err := wfdir.LoadLock(root)
	if err != nil {
		t.Fatal(err)
	}
	_, name := lock.MetricName("prod", aovMetricPath)
	return name
}

func writeMetricFile(t *testing.T, root, body string) {
	t.Helper()
	if err := wfdir.WriteFile(root, aovMetricPath, body); err != nil {
		t.Fatal(err)
	}
}

// boundMetricTree is metricApplyTree with a live metric called `liveName` bound
// to the file.
func boundMetricTree(t *testing.T, f *fakePipelineInstance, liveName string) (tree, root string) {
	t.Helper()
	seedFeature(f, "private")
	f.AddMetric("table-aov", liveName, `{"source":"table-orders","value":"revenue / orders","version":1}`)
	return metricApplyTree(t, f, "table-aov")
}

// THE RENAME, through apply: same row, renamed at publish, the name recorded,
// and a second apply sends nothing.
func TestSyncApplyRenamesAMetricInPlaceFromItsFile(t *testing.T) {
	f := newFakePipelineInstance(t)
	signInPipeline(t, f)
	tree, root := boundMetricTree(t, f, "aov")
	writeMetricFile(t, root, namedMetricBody(aovName))

	if out, err := runCLI(t, tree, "sync", "apply", "--stack", "prod", "--json"); err != nil {
		t.Fatalf("apply: %v\n%s", err, out)
	}
	if len(f.metricsCreated) != 0 {
		t.Fatalf("a rename created a metric: %+v", f.metricsCreated)
	}
	if got := f.tables["table-aov"].Name; got != aovName {
		t.Fatalf("live metric is called %q, want %q", got, aovName)
	}
	if got := lockedMetricName(t, root); got != aovName {
		t.Errorf("lock records %q, want %q", got, aovName)
	}
	f.updates = nil
	if out, err := runCLI(t, tree, "sync", "apply", "--stack", "prod", "--json"); err != nil {
		t.Fatalf("second apply: %v\n%s", err, out)
	}
	if sent := renamesSent(f); len(sent) != 0 {
		t.Errorf("the second apply sent %v", sent)
	}
}

// A NAME ADDED TO A FILE WHOSE DEFINITION IS UP TO DATE is still a rename to
// stage: the recipe fingerprint deliberately ignores the name, so the up-to-date
// shortcut must ask the name separately or the rename never happens.
func TestPipelinePushStagesARenameOfAnUpToDateMetric(t *testing.T) {
	f := newFakePipelineInstance(t)
	signInPipeline(t, f)
	tree, root := boundMetricTree(t, f, "aov")
	if out, err := runCLI(t, tree, "sync", "apply", "--stack", "prod", "--json"); err != nil {
		t.Fatalf("first apply: %v\n%s", err, out)
	}
	writeMetricFile(t, root, namedMetricBody(aovName))
	if out, err := runCLI(t, tree, "sync", "apply", "--stack", "prod", "--json"); err != nil {
		t.Fatalf("second apply: %v\n%s", err, out)
	}
	if got := f.tables["table-aov"].Name; got != aovName {
		t.Errorf("live metric is called %q — the rename of an up-to-date definition never ran", got)
	}
}

// RENAMED ELSEWHERE IS DRIFT: refused, nothing sent; --force renames it back.
func TestPipelinePushRefusesAMetricNameRenamedElsewhere(t *testing.T) {
	f := newFakePipelineInstance(t)
	signInPipeline(t, f)
	tree, root := boundMetricTree(t, f, "aov")
	writeMetricFile(t, root, namedMetricBody(aovName))
	if out, err := runCLI(t, tree, "sync", "apply", "--stack", "prod", "--json"); err != nil {
		t.Fatalf("apply: %v\n%s", err, out)
	}
	f.tables["table-aov"].Name = "Web name"
	// A change to the file so the push has something to do besides the name.
	writeMetricFile(t, root, `{"recipe":{"value":"revenue","source":"orders"},"name":"`+aovName+`"}`)
	f.updates = nil

	out, err := runCLI(t, root, "pipeline", "push", "--stack", "prod", "--json")
	if err == nil {
		t.Fatalf("push went ahead over a renamed metric:\n%s", out)
	}
	if sent := renamesSent(f); len(sent) != 0 {
		t.Fatalf("renames sent = %v", sent)
	}
	if !strings.Contains(out, "Web name") {
		t.Errorf("the refusal does not name the live name:\n%s", out)
	}
	if _, err := runCLI(t, root, "pipeline", "push", "--stack", "prod", "--force", "--json"); err != nil {
		t.Fatalf("push --force: %v", err)
	}
	if sent := renamesSent(f); len(sent) != 1 {
		t.Errorf("renames sent = %v, want the one --force allowed", sent)
	}
}

// LIVE EQUALS DESIRED IS NEVER DRIFT, for a metric's name too: a stale anchor
// beside a live metric that already holds the declared name is re-anchored.
func TestPipelinePushReanchorsAMetricNameItAlreadyHolds(t *testing.T) {
	f := newFakePipelineInstance(t)
	signInPipeline(t, f)
	_, root := boundMetricTree(t, f, aovName)
	writeMetricFile(t, root, namedMetricBody(aovName))
	lock, _ := wfdir.LoadLock(root)
	lock.SetMetricName("prod", aovMetricPath, "table-aov", "A stale anchor")
	if err := wfdir.SaveLock(root, lock); err != nil {
		t.Fatal(err)
	}
	if out, err := runCLI(t, root, "pipeline", "push", "--stack", "prod", "--json"); err != nil {
		t.Fatalf("push refused the metric's own name: %v\n%s", err, out)
	}
	if sent := renamesSent(f); len(sent) != 0 {
		t.Errorf("renames sent = %v", sent)
	}
	if got := lockedMetricName(t, root); got != aovName {
		t.Errorf("lock records %q, want %q", got, aovName)
	}
}

// AN ABSENT NAME IS NEVER SENT, whatever the live metric is called.
func TestPipelinePushNeverSendsAnUndeclaredMetricName(t *testing.T) {
	f := newFakePipelineInstance(t)
	signInPipeline(t, f)
	_, root := boundMetricTree(t, f, "Somebody's name")
	if out, err := runCLI(t, root, "pipeline", "push", "--stack", "prod", "--json"); err != nil {
		t.Fatalf("push: %v\n%s", err, out)
	}
	if sent := renamesSent(f); len(sent) != 0 {
		t.Errorf("renames sent = %v", sent)
	}
}

// A NEW METRIC WITH A DECLARED NAME is created under it, and the name recorded.
func TestPipelinePushCreatesAMetricUnderItsDeclaredName(t *testing.T) {
	f := newFakePipelineInstance(t)
	signInPipeline(t, f)
	seedFeature(f, "private")
	_, root := metricApplyTree(t, f, "")
	writeMetricFile(t, root, namedMetricBody(aovName))
	if out, err := runCLI(t, root, "pipeline", "push", "--stack", "prod", "--json"); err != nil {
		t.Fatalf("push: %v\n%s", err, out)
	}
	if len(f.metricsCreated) != 1 || f.metricsCreated[0].Name != aovName {
		t.Fatalf("metricsCreated = %+v", f.metricsCreated)
	}
	if got := lockedMetricName(t, root); got != aovName {
		t.Errorf("lock records %q", got)
	}
}
