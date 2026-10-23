package commands

import (
	"encoding/json"
	"path/filepath"
	"strings"
	"testing"

	"github.com/ronjatech/ronja-cli/internal/api"
	"github.com/ronjatech/ronja-cli/internal/wfdir"
)

// A CLONE RECORDS THE LIVE NAME. `pipeline clone` writes a table called "Sales
// orders" to Sales-orders.sql — the stem is a slug of the name, not the name —
// so the clone records the exact live name as the folder's baseline, and the
// first `-- @name` after it is a rename rather than drift. These are the shapes
// where the stem and the name part company: a name with spaces, a stem that
// took a collision suffix, and a metric.

// clonePipeline clones collection-1 into <dir>/out, on a named stack when
// stack is non-empty and as a legacy folder otherwise, and returns the root.
func clonePipeline(t *testing.T, stack string) string {
	t.Helper()
	dir := t.TempDir()
	args := []string{"pipeline", "clone", "collection-1", "out", "--json"}
	if stack != "" {
		args = append(args, "--stack", stack)
	}
	if _, stderr, err := runPipelineCLI(t, dir, args...); err != nil {
		t.Fatalf("clone: %v\n%s", err, stderr)
	}
	return filepath.Join(dir, "out")
}

func pushAndPublish(t *testing.T, root string, stack ...string) {
	t.Helper()
	for _, verb := range []string{"push", "publish"} {
		args := append([]string{"pipeline", verb, "--json"}, stack...)
		if out, stderr, err := runPipelineCLI(t, root, args...); err != nil {
			t.Fatalf("%s: %v\n%s\n%s", verb, err, out, stderr)
		}
	}
}

func TestPipelineCloneThenNameRenamesATableWithSpaces(t *testing.T) {
	for _, stack := range []string{"", "dev"} {
		t.Run("stack="+stack, func(t *testing.T) {
			f := newFakePipelineInstance(t)
			signInPipeline(t, f)
			f.AddFeature("collection-1", "Sales pipeline", "private")
			f.AddTable(&api.Table{ID: "table-orders", Name: salesOrders, FeatureID: "collection-1", Code: "SELECT * FROM raw"})
			root := clonePipeline(t, stack)

			body := readFile(t, root, "Sales-orders.sql")
			if err := wfdir.WriteFile(root, "Sales-orders.sql", namedOrders("Orders, all lines", body)); err != nil {
				t.Fatal(err)
			}
			var flags []string
			if stack != "" {
				flags = []string{"--stack", stack}
			}
			pushAndPublish(t, root, flags...)
			if got := f.tables["table-orders"].Name; got != "Orders, all lines" {
				t.Errorf("table-orders is called %q after the rename", got)
			}
			if len(f.created) != 0 {
				t.Errorf("a rename created a table: %+v", f.created)
			}
		})
	}
}

// The second of two tables whose names slug alike lands on Sales-orders-2.sql,
// whose stem is neither table's name: the recorded name is what makes the first
// `-- @name` on it a rename.
func TestPipelineCloneThenNameRenamesACollisionSuffixedTable(t *testing.T) {
	f := newFakePipelineInstance(t)
	signInPipeline(t, f)
	f.AddFeature("collection-1", "Sales pipeline", "private")
	f.AddTable(&api.Table{ID: "table-a", Name: salesOrders, FeatureID: "collection-1", Code: "SELECT 1 AS a"})
	f.AddTable(&api.Table{ID: "table-b", Name: "Sales: orders", FeatureID: "collection-1", Code: "SELECT 2 AS b"})
	root := clonePipeline(t, "")

	body := readFile(t, root, "Sales-orders-2.sql")
	id, was := "table-a", salesOrders
	if strings.Contains(body, "AS b") {
		id, was = "table-b", "Sales: orders"
	}
	if err := wfdir.WriteFile(root, "Sales-orders-2.sql", namedOrders("Orders, second", body)); err != nil {
		t.Fatal(err)
	}
	pushAndPublish(t, root)
	if got := f.tables[id].Name; got != "Orders, second" {
		t.Errorf("%s (was %q) is called %q after the rename", id, was, got)
	}
}

func TestPipelineCloneThenNameRenamesAMetric(t *testing.T) {
	f := newFakePipelineInstance(t)
	signInPipeline(t, f)
	f.AddFeature("collection-1", "Sales pipeline", "private")
	f.AddTable(&api.Table{ID: "table-orders", Name: "orders", FeatureID: "collection-1", Code: "SELECT * FROM raw"})
	f.AddMetric("table-aov", aovName, `{"value":"revenue / orders","source":"table-orders","version":1}`)
	root := clonePipeline(t, "")

	const path = "metrics/Average-order-value.json"
	var file map[string]any
	if err := json.Unmarshal([]byte(readFile(t, root, path)), &file); err != nil {
		t.Fatal(err)
	}
	file["name"] = "AOV, all orders"
	b, err := json.Marshal(file)
	if err != nil {
		t.Fatal(err)
	}
	if err := wfdir.WriteFile(root, path, string(b)); err != nil {
		t.Fatal(err)
	}
	pushAndPublish(t, root)
	if got := f.tables["table-aov"].Name; got != "AOV, all orders" {
		t.Errorf("table-aov is called %q after the rename", got)
	}
	if len(f.metricsCreated) != 0 {
		t.Errorf("a rename created a metric: %+v", f.metricsCreated)
	}
}
