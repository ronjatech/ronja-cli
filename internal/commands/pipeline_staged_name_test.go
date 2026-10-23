package commands

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/ronjatech/ronja-cli/internal/api"
)

// A name this folder STAGED on its own draft is the folder's own. Changing the
// declared name again before the publish — or after a publish refused the
// staged one as taken, whose advice is exactly "change the name and push
// again" — must rename the draft once more, never read the folder's own staged
// rename as somebody else's.

func writeOrders(t *testing.T, root, content string) {
	t.Helper()
	if err := os.WriteFile(filepath.Join(root, "orders.sql"), []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}

// RENAME, RENAME AGAIN, PUBLISH — for a table.
func TestPipelinePushRenamesAStagedTableNameAgainBeforePublish(t *testing.T) {
	f := newFakePipelineInstance(t)
	signInPipeline(t, f)
	_, root := namedTableTree(t, f, "orders", namedOrders("Sales", "SELECT * FROM raw"))
	if out, err := runCLI(t, root, "pipeline", "push", "--stack", "prod", "--json"); err != nil {
		t.Fatalf("first push: %v\n%s", err, out)
	}
	writeOrders(t, root, namedOrders(salesOrders, "SELECT * FROM raw"))
	if out, err := runCLI(t, root, "pipeline", "push", "--stack", "prod", "--json"); err != nil {
		t.Fatalf("a second rename of the folder's own staged name was refused: %v\n%s", err, out)
	}
	if out, err := runCLI(t, root, "pipeline", "publish", "--stack", "prod", "--json"); err != nil {
		t.Fatalf("publish: %v\n%s", err, out)
	}
	if got := f.tables["table-orders"].Name; got != salesOrders {
		t.Fatalf("live table is called %q, want %q", got, salesOrders)
	}
	if got := lockedTableName(t, root); got != salesOrders {
		t.Errorf("lock records %q, want %q", got, salesOrders)
	}
}

// PUBLISH REFUSED AS TAKEN → EDIT THE HEADER → PUSH → PUBLISH — for a table.
func TestPipelinePushAfterATakenTableNameFollowsTheAdvice(t *testing.T) {
	f := newFakePipelineInstance(t)
	signInPipeline(t, f)
	_, root := namedTableTree(t, f, "orders", namedOrders(salesOrders, "SELECT * FROM raw"))
	f.AddTable(&api.Table{ID: "table-other", Name: salesOrders, FeatureID: "collection-1", Code: "SELECT 1"})
	if out, err := runCLI(t, root, "pipeline", "push", "--stack", "prod", "--json"); err != nil {
		t.Fatalf("push: %v\n%s", err, out)
	}
	if out, err := runCLI(t, root, "pipeline", "publish", "--stack", "prod", "--json"); err == nil {
		t.Fatalf("publish landed a taken name:\n%s", out)
	}
	writeOrders(t, root, namedOrders("Orders, all lines", "SELECT * FROM raw"))
	if out, err := runCLI(t, root, "pipeline", "push", "--stack", "prod", "--json"); err != nil {
		t.Fatalf("the push the refusal advised was refused: %v\n%s", err, out)
	}
	if out, err := runCLI(t, root, "pipeline", "publish", "--stack", "prod", "--json"); err != nil {
		t.Fatalf("publish: %v\n%s", err, out)
	}
	if got := f.tables["table-orders"].Name; got != "Orders, all lines" {
		t.Errorf("live table is called %q", got)
	}
}

// A STAGED NAME IS ONLY EVER COMPARED AGAINST THE DRAFT IT WAS STAGED ON: a
// different draft carrying that same name was not put there by this checkout.
func TestPipelinePushStagedNameDoesNotVouchForAnotherDraft(t *testing.T) {
	f := newFakePipelineInstance(t)
	signInPipeline(t, f)
	_, root := namedTableTree(t, f, "orders", namedOrders("Sales", "SELECT * FROM raw"))
	if out, err := runCLI(t, root, "pipeline", "push", "--stack", "prod", "--json"); err != nil {
		t.Fatalf("first push: %v\n%s", err, out)
	}
	// The staged draft is discarded in the web app, and somebody else's draft
	// is opened carrying the same name.
	for id, row := range f.tables {
		if row.ParentModelID == "table-orders" {
			f.dropDraft(id)
		}
	}
	other := f.AddDraft("table-orders", "table-draft-other", "SELECT * FROM raw")
	other.Name = "Sales"
	writeOrders(t, root, namedOrders(salesOrders, "SELECT * FROM raw"))
	if out, err := runCLI(t, root, "pipeline", "push", "--stack", "prod", "--json"); err == nil {
		t.Fatalf("a name staged on a different draft vouched for this one:\n%s", out)
	}
}

// RENAME, RENAME AGAIN, PUBLISH — for a metric.
func TestPipelinePushRenamesAStagedMetricNameAgainBeforePublish(t *testing.T) {
	f := newFakePipelineInstance(t)
	signInPipeline(t, f)
	_, root := boundMetricTree(t, f, "aov")
	writeMetricFile(t, root, namedMetricBody("AOV"))
	if out, err := runCLI(t, root, "pipeline", "push", "--stack", "prod", "--json"); err != nil {
		t.Fatalf("first push: %v\n%s", err, out)
	}
	writeMetricFile(t, root, namedMetricBody(aovName))
	if out, err := runCLI(t, root, "pipeline", "push", "--stack", "prod", "--json"); err != nil {
		t.Fatalf("a second rename of the folder's own staged name was refused: %v\n%s", err, out)
	}
	if out, err := runCLI(t, root, "pipeline", "publish", "--stack", "prod", "--json"); err != nil {
		t.Fatalf("publish: %v\n%s", err, out)
	}
	if got := f.tables["table-aov"].Name; got != aovName {
		t.Fatalf("live metric is called %q, want %q", got, aovName)
	}
	if got := lockedMetricName(t, root); got != aovName {
		t.Errorf("lock records %q, want %q", got, aovName)
	}
}

// PUBLISH REFUSED AS TAKEN → EDIT "name" → PUSH → PUBLISH — for a metric. A
// metric and a table share one namespace, and the feature already holds a
// table called "Revenue".
func TestPipelinePushAfterATakenMetricNameFollowsTheAdvice(t *testing.T) {
	f := newFakePipelineInstance(t)
	signInPipeline(t, f)
	_, root := boundMetricTree(t, f, "aov")
	writeMetricFile(t, root, namedMetricBody("Revenue"))
	if out, err := runCLI(t, root, "pipeline", "push", "--stack", "prod", "--json"); err != nil {
		t.Fatalf("push: %v\n%s", err, out)
	}
	if out, err := runCLI(t, root, "pipeline", "publish", "--stack", "prod", "--json"); err == nil {
		t.Fatalf("publish landed a taken name:\n%s", out)
	}
	writeMetricFile(t, root, namedMetricBody(aovName))
	if out, err := runCLI(t, root, "pipeline", "push", "--stack", "prod", "--json"); err != nil {
		t.Fatalf("the push the refusal advised was refused: %v\n%s", err, out)
	}
	if out, err := runCLI(t, root, "pipeline", "publish", "--stack", "prod", "--json"); err != nil {
		t.Fatalf("publish: %v\n%s", err, out)
	}
	if got := f.tables["table-aov"].Name; got != aovName {
		t.Errorf("live metric is called %q", got)
	}
}
