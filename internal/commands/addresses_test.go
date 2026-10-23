package commands

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/ronjatech/ronja-cli/internal/api"
	"github.com/ronjatech/ronja-cli/internal/wfdir"
)

// Declared moves and removals (`moved` / `removed` in ronja.json), and rename
// detection.
//
// The fakes are servers: a table bound to an address is a row the fake holds,
// and a "create" is a POST the fake records. So "nothing was created" is read
// off what the server received, never off what the CLI meant to do.

// stackFixture is one stack of a pipeline folder: the fake instance it points
// at, and the files it has bound there (path → table id).
type stackFixture struct {
	name  string
	fake  *fakePipelineInstance
	bound map[string]string
}

// movedPipelineTree lays out a tree holding one pipeline folder, "sales", that
// names every given stack, binds each stack's tables in the LOCK (the shape a
// CI checkout has: no .ronja/ at all), and declares the given moves/removals.
func movedPipelineTree(t *testing.T, stacks []stackFixture, files map[string]string,
	moved []wfdir.MovedEntry, removed []string) (tree, root string) {
	t.Helper()
	tree = t.TempDir()
	root = filepath.Join(tree, "sales")
	if err := os.MkdirAll(root, 0o755); err != nil {
		t.Fatal(err)
	}
	manifest := &wfdir.Manifest{
		Kind:    wfdir.KindPipeline,
		Title:   "Sales pipeline",
		Stacks:  map[string]wfdir.Stack{},
		Moved:   moved,
		Removed: removed,
	}
	lock := &wfdir.Lock{}
	for _, s := range stacks {
		manifest.Stacks[s.name] = wfdir.Stack{URL: s.fake.URL(), TenantID: testTenantID, FeatureID: "collection-1"}
		if len(s.bound) > 0 {
			lock.SetBinding(s.name, wfdir.Binding{Tables: s.bound})
			for path, id := range s.bound {
				if code := s.fake.CodeOf(id); code != "" {
					lock.SetTableLive(s.name, path, id, wfdir.HashString(code))
				}
			}
		}
	}
	if err := wfdir.SaveFolder(root, manifest, lock); err != nil {
		t.Fatalf("write folder: %v", err)
	}
	for path, content := range files {
		if err := wfdir.WriteFile(root, path, content); err != nil {
			t.Fatalf("write %s: %v", path, err)
		}
	}
	return tree, root
}

// ordersInstance is a fake instance holding one feature and one live table,
// "orders", under the given id.
func ordersInstance(t *testing.T, tableID string) *fakePipelineInstance {
	t.Helper()
	f := newFakePipelineInstance(t)
	f.AddFeature("collection-1", "Sales pipeline", "private")
	f.AddTable(&api.Table{ID: tableID, Name: "orders", FeatureID: "collection-1", Code: "SELECT * FROM raw"})
	return f
}

func lockOf(t *testing.T, root string) *wfdir.Lock {
	t.Helper()
	lock, err := wfdir.LoadLock(root)
	if err != nil {
		t.Fatalf("load lock: %v", err)
	}
	return lock
}

func readBytes(t *testing.T, path string) []byte {
	t.Helper()
	body, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	return body
}

var ordersMove = []wfdir.MovedEntry{{From: "orders.sql", To: "sales_orders.sql"}}

// A CHAIN resolves in declared order: [{A,B},{B,C}] against a stack at A lands
// at C — and against a stack that already applied the first entry (at B), the
// first entry is a no-op and the second still lands it at C.
func TestPipelinePushMovedChainResolvesInDeclaredOrder(t *testing.T) {
	chain := []wfdir.MovedEntry{{From: "a.sql", To: "b.sql"}, {From: "b.sql", To: "c.sql"}}
	for _, at := range []string{"a.sql", "b.sql"} {
		t.Run("stack at "+at, func(t *testing.T) {
			prod := ordersInstance(t, "table-orders")
			signInPipeline(t, prod)
			_, root := movedPipelineTree(t,
				[]stackFixture{{name: "prod", fake: prod, bound: map[string]string{at: "table-orders"}}},
				map[string]string{"c.sql": "SELECT * FROM raw"}, chain, nil)

			out, err := runCLI(t, root, "pipeline", "push", "--stack", "prod", "--json")
			if err != nil {
				t.Fatalf("push: %v\n%s", err, out)
			}
			if len(prod.created) != 0 {
				t.Fatalf("a chained rename created %+v", prod.created)
			}
			if got := lockOf(t, root).Binding("prod").Tables; len(got) != 1 || got["c.sql"] != "table-orders" {
				t.Fatalf("binding = %v, want c.sql → table-orders only", got)
			}
			result := &pipelinePushResult{}
			decodeJSONInto(t, out, result)
			if len(result.Moved) != 1 || result.Moved[0].From != at || result.Moved[0].To != "c.sql" {
				t.Errorf("moved = %+v, want one move %s → c.sql", result.Moved, at)
			}
		})
	}
}

// BOTH ENDS BOUND is refused before a single request: one address, two rows.
func TestPipelinePushRefusesAMoveWhoseBothEndsAreBound(t *testing.T) {
	prod := ordersInstance(t, "table-orders")
	prod.AddTable(&api.Table{ID: "table-sales", Name: "sales_orders", FeatureID: "collection-1", Code: "SELECT 2"})
	signInPipeline(t, prod)
	_, root := movedPipelineTree(t,
		[]stackFixture{{name: "prod", fake: prod, bound: map[string]string{
			"orders.sql": "table-orders", "sales_orders.sql": "table-sales"}}},
		map[string]string{"sales_orders.sql": "SELECT * FROM raw"}, ordersMove, nil)
	before := readBytes(t, wfdir.LockPath(root))

	_, stderr, err := runPipelineCLI(t, root, "pipeline", "push", "--stack", "prod")
	if err == nil || !strings.Contains(err.Error(), "BOTH are bound") {
		t.Fatalf("err = %v, want the both-bound refusal\n%s", err, stderr)
	}
	assertNoTableWrites(t, prod)
	if after := readBytes(t, wfdir.LockPath(root)); !bytes.Equal(before, after) {
		t.Error("a refused move rewrote the lock")
	}
}

// THE OLD FILE STILL ON DISK is ambiguous and refused: the declaration says it
// is gone, the folder says it is not.
func TestPipelinePushRefusesAMoveWhoseOldFileIsStillThere(t *testing.T) {
	prod := ordersInstance(t, "table-orders")
	signInPipeline(t, prod)
	_, root := movedPipelineTree(t,
		[]stackFixture{{name: "prod", fake: prod, bound: map[string]string{"orders.sql": "table-orders"}}},
		map[string]string{"orders.sql": "SELECT * FROM raw", "sales_orders.sql": "SELECT * FROM raw"}, ordersMove, nil)

	_, _, err := runPipelineCLI(t, root, "pipeline", "push", "--stack", "prod")
	if err == nil || !strings.Contains(err.Error(), "still in this folder") {
		t.Fatalf("err = %v, want the still-on-disk refusal", err)
	}
	// The way to REUSE the path is named too: the entry goes only once no stack
	// still binds the old row there.
	if want := "if orders.sql is a NEW file reusing the path, delete the entry once no stack in ronja.lock.json still binds orders.sql"; !strings.Contains(err.Error(), want) {
		t.Errorf("err = %v, want it to say %q", err, want)
	}
	assertNoTableWrites(t, prod)
	// sync apply refuses the same folder, by its own reason.
	tree := filepath.Dir(root)
	out, _ := runCLI(t, tree, "sync", "apply", "--stack", "prod", "--json")
	if line := applyFolderLine(t, applyReportOf(t, out), "sales"); line.Reason != syncReasonAddressConflict {
		t.Errorf("apply = %+v, want %s", line, syncReasonAddressConflict)
	}
}

// THE NAME TRAVELS WITH THE ROW. A table whose name was never recorded has the
// file STEM as its name baseline, so without the carry the new stem would make
// the live name ("orders") read as somebody else's rename — and a `-- @name` in
// the very commit that moved the file would be refused as drift instead of
// renaming the same row in place.
func TestPipelinePushMovedCarriesTheNameBaseline(t *testing.T) {
	prod := ordersInstance(t, "table-orders")
	signInPipeline(t, prod)
	_, root := movedPipelineTree(t,
		[]stackFixture{{name: "prod", fake: prod, bound: map[string]string{"orders.sql": "table-orders"}}},
		map[string]string{"sales_orders.sql": "-- @table One row per order.\n-- @name Sales orders\nSELECT * FROM raw"},
		ordersMove, nil)
	if got := lockOf(t, root).TableName("prod", "orders.sql"); got != "" {
		t.Fatalf("fixture records a name (%q); the case under test is a folder that has none", got)
	}

	out, stderr, err := runPipelineCLI(t, root, "pipeline", "push", "--stack", "prod", "--json")
	if err != nil {
		t.Fatalf("push: %v\n%s\n%s", err, out, stderr)
	}
	if len(prod.created) != 0 {
		t.Fatalf("created %+v", prod.created)
	}
	var renamed bool
	for _, u := range prod.updates {
		if u.Name != nil && *u.Name == "Sales orders" && (u.ID == "table-orders" || prod.tables[u.ID] != nil && prod.tables[u.ID].ParentModelID == "table-orders") {
			renamed = true
		}
	}
	if !renamed {
		t.Fatalf("the same row was not renamed in place; updates = %+v\n%s", prod.updates, stderr)
	}
}

// REMOVED: the lock entry goes, and the row is not touched — no read, no
// write, no delete, nothing that names it.
func TestPipelinePushRemovedDropsTheEntryAndLeavesTheRowAlone(t *testing.T) {
	prod := ordersInstance(t, "table-orders")
	prod.AddTable(&api.Table{ID: "table-legacy", Name: "legacy_rollup", FeatureID: "collection-1", Code: "SELECT 9"})
	signInPipeline(t, prod)
	_, root := movedPipelineTree(t,
		[]stackFixture{{name: "prod", fake: prod, bound: map[string]string{
			"orders.sql": "table-orders", "legacy_rollup.sql": "table-legacy"}}},
		map[string]string{"orders.sql": "SELECT * FROM raw"}, nil, []string{"legacy_rollup.sql"})
	lock := lockOf(t, root)
	lock.SetChecksSeen("prod", wfdir.ChecksPath("legacy_rollup"), "table-legacy", map[string]wfdir.LockCheckEntry{"Fresh": {CheckID: "check-1"}})
	if err := wfdir.SaveLock(root, lock); err != nil {
		t.Fatal(err)
	}

	out, stderr, err := runPipelineCLI(t, root, "pipeline", "push", "--stack", "prod", "--json")
	if err != nil {
		t.Fatalf("push: %v\n%s\n%s", err, out, stderr)
	}
	lock = lockOf(t, root)
	if got := lock.Binding("prod").Tables; got["legacy_rollup.sql"] != "" || got["orders.sql"] != "table-orders" {
		t.Fatalf("binding = %v, want legacy_rollup.sql dropped and orders.sql kept", got)
	}
	if id, _ := lock.ChecksSeen("prod", wfdir.ChecksPath("legacy_rollup")); id != "" {
		t.Error("the removed table's checks are still managed — a later --prune would silence them")
	}
	for _, r := range prod.requestOrder() {
		if strings.Contains(r, "table-legacy") {
			t.Errorf("a request touched the removed row: %s", r)
		}
	}
	if prod.tables["table-legacy"] == nil || prod.CodeOf("table-legacy") != "SELECT 9" {
		t.Error("the removed row was changed")
	}
	if strings.Contains(stderr, "bound but gone") {
		t.Errorf("a declared removal was reported as an undeclared one:\n%s", stderr)
	}
	result := &pipelinePushResult{}
	decodeJSONInto(t, out, result)
	if len(result.Removed) != 1 || result.Removed[0].Path != "legacy_rollup.sql" || result.Removed[0].ID != "table-legacy" {
		t.Errorf("removed = %+v", result.Removed)
	}
}

// A REMOVED FILE STILL ON DISK is refused: dropping its entry would make the
// very next push create a second row for it.
func TestPipelinePushRefusesARemovedFileThatIsStillThere(t *testing.T) {
	prod := ordersInstance(t, "table-orders")
	signInPipeline(t, prod)
	_, root := movedPipelineTree(t,
		[]stackFixture{{name: "prod", fake: prod, bound: map[string]string{"orders.sql": "table-orders"}}},
		map[string]string{"orders.sql": "SELECT * FROM raw"}, nil, []string{"orders.sql"})

	_, _, err := runPipelineCLI(t, root, "pipeline", "push", "--stack", "prod")
	if err == nil || !strings.Contains(err.Error(), "still in this folder") {
		t.Fatalf("err = %v", err)
	}
	if want := `if orders.sql is a NEW file reusing the path, delete it from "removed" once no stack in ronja.lock.json still binds orders.sql`; !strings.Contains(err.Error(), want) {
		t.Errorf("err = %v, want it to say %q", err, want)
	}
	if got := lockOf(t, root).Binding("prod").Tables; got["orders.sql"] != "table-orders" {
		t.Errorf("a refused removal dropped the entry: %v", got)
	}
}

// RENAME DETECTION in a classic single-folder push is a NOTE, and the push goes
// on exactly as it did before rename detection existed: the new file is
// created, the old row is left alone.
func TestPipelinePushNotesAnUndeclaredRename(t *testing.T) {
	prod := ordersInstance(t, "table-orders")
	signInPipeline(t, prod)
	_, root := movedPipelineTree(t,
		[]stackFixture{{name: "prod", fake: prod, bound: map[string]string{"orders.sql": "table-orders"}}},
		map[string]string{"sales_orders.sql": "SELECT * FROM raw"}, nil, nil)

	_, stderr, err := runPipelineCLI(t, root, "pipeline", "push", "--stack", "prod")
	if err != nil {
		t.Fatalf("push: %v\n%s", err, stderr)
	}
	want := `orders.sql is bound but gone, and sales_orders.sql is new — add a "moved" entry to ronja.json if this is a rename, or a "removed" entry if orders.sql was dropped on purpose.`
	if !strings.Contains(stderr, want) {
		t.Errorf("no rename note:\n%s", stderr)
	}
	if len(prod.created) != 1 {
		t.Errorf("the classic push changed behaviour: created = %+v", prod.created)
	}
	if prod.tables["table-orders"] == nil {
		t.Error("the old row was touched")
	}
}

// A DELETED METRIC FILE is no longer orphaned in silence: the lock's metric
// entry is visited even though no file names it.
func TestADeletedMetricFileIsNoLongerSilent(t *testing.T) {
	prod := ordersInstance(t, "table-orders")
	prod.AddMetric("metric-aov", "aov", aovRecipe)
	signInPipeline(t, prod)
	tree, root := movedPipelineTree(t,
		[]stackFixture{{name: "prod", fake: prod, bound: map[string]string{"orders.sql": "table-orders"}}},
		map[string]string{"orders.sql": "SELECT * FROM raw"}, nil, nil)
	lock := lockOf(t, root)
	lock.SetMetricSeen("prod", aovMetricPath, "metric-aov", "", "")
	if err := wfdir.SaveLock(root, lock); err != nil {
		t.Fatal(err)
	}

	_, stderr, err := runPipelineCLI(t, root, "pipeline", "push", "--stack", "prod")
	if err != nil {
		t.Fatalf("push: %v\n%s", err, stderr)
	}
	if !strings.Contains(stderr, aovMetricPath+" is bound but gone") {
		t.Errorf("the gone metric was not reported:\n%s", stderr)
	}
	out, _ := runCLI(t, tree, "sync", "apply", "--stack", "prod", "--json")
	line := applyFolderLine(t, applyReportOf(t, out), "sales")
	if line.Reason != syncReasonOrphanedFile || !strings.Contains(line.Detail, aovMetricPath) {
		t.Errorf("apply = %+v", line)
	}
	if prod.tables["metric-aov"] == nil {
		t.Error("the metric was touched")
	}
}

// A METRIC MOVES the way a table does, its recorded id and name with it.
func TestPipelinePushMovesAMetric(t *testing.T) {
	prod := ordersInstance(t, "table-orders")
	prod.AddMetric("metric-aov", "aov", aovRecipe)
	signInPipeline(t, prod)
	_, root := movedPipelineTree(t,
		[]stackFixture{{name: "prod", fake: prod, bound: map[string]string{"orders.sql": "table-orders"}}},
		map[string]string{"orders.sql": "SELECT * FROM raw", "metrics/average_order.json": metricFileBody(aovRecipe)},
		[]wfdir.MovedEntry{{From: aovMetricPath, To: "metrics/average_order.json"}}, nil)
	lock := lockOf(t, root)
	lock.SetMetricSeen("prod", aovMetricPath, "metric-aov", "", "")
	if err := wfdir.SaveLock(root, lock); err != nil {
		t.Fatal(err)
	}

	_, stderr, err := runPipelineCLI(t, root, "pipeline", "push", "--stack", "prod")
	if err != nil {
		t.Fatalf("push: %v\n%s", err, stderr)
	}
	if len(prod.metricsCreated) != 0 {
		t.Fatalf("a moved metric was created again: %+v", prod.metricsCreated)
	}
	lock = lockOf(t, root)
	if id, stem := lock.MetricStem("prod", "metrics/average_order.json"); id != "metric-aov" || stem != "aov" {
		t.Errorf("metric at the new address = (%q, stem %q), want (metric-aov, aov)", id, stem)
	}
	if id, _, _ := lock.MetricSeen("prod", aovMetricPath); id != "" {
		t.Error("the old metric address is still recorded")
	}
}

// `moved` IN A FILE THAT DOES NOT SAY IT NEEDS FORMAT 5 is refused, naming the
// line to add: an older CLI ignores the key and creates a second row.
func TestPipelinePushRefusesMovedBelowItsFormatVersion(t *testing.T) {
	prod := ordersInstance(t, "table-orders")
	signInPipeline(t, prod)
	_, root := movedPipelineTree(t,
		[]stackFixture{{name: "prod", fake: prod, bound: map[string]string{"orders.sql": "table-orders"}}},
		map[string]string{"sales_orders.sql": "SELECT * FROM raw"}, ordersMove, nil)
	// Rewrite the manifest by hand without the stamp, as a person adding the key
	// would.
	raw := readBytes(t, wfdir.ManifestPath(root))
	raw = bytes.Replace(raw, []byte(`"formatVersion": 5,`), []byte(`"formatVersion": 2,`), 1)
	if err := os.WriteFile(wfdir.ManifestPath(root), raw, 0o644); err != nil {
		t.Fatal(err)
	}

	_, _, err := runPipelineCLI(t, root, "pipeline", "push", "--stack", "prod")
	if err == nil || !strings.Contains(err.Error(), `Add "formatVersion": 5`) {
		t.Fatalf("err = %v, want the format refusal", err)
	}
	assertNoTableWrites(t, prod)
}

// A SIBLING STILL READING THE OLD STEM is refused before anything is sent: the
// move changes the file's name, and `{{ ref('orders') }}` names a file that is
// no longer there.
func TestPipelinePushRefusesASiblingRefToAMovedStem(t *testing.T) {
	prod := ordersInstance(t, "table-orders")
	prod.AddTable(&api.Table{ID: "table-summary", Name: "summary", FeatureID: "collection-1",
		Code: "SELECT * FROM {{ ref('table-orders') }}", InputModels: []string{"table-orders"}})
	signInPipeline(t, prod)
	_, root := movedPipelineTree(t,
		[]stackFixture{{name: "prod", fake: prod, bound: map[string]string{
			"orders.sql": "table-orders", "summary.sql": "table-summary"}}},
		map[string]string{
			"sales_orders.sql": "SELECT * FROM raw",
			"summary.sql":      "SELECT count(*) FROM {{ ref('orders') }}",
		}, ordersMove, nil)

	_, stderr, err := runPipelineCLI(t, root, "pipeline", "push", "--stack", "prod")
	if err == nil {
		t.Fatalf("a ref to the old stem was pushed\n%s", stderr)
	}
	// The EXISTING local alias check is what refuses it — no machinery of the
	// move's own.
	if !strings.Contains(err.Error(), `"orders" is neither a table id nor a name this folder resolves`) {
		t.Errorf("the refusal is not the alias check's: %v", err)
	}
	assertNoTableWrites(t, prod)
}

// A FOLDER WITH NO STACK keeps its ids in ronja.json's "instances", so that is
// what a move re-keys there — alongside the local baseline.
func TestPipelinePushMovesOnALegacyFolder(t *testing.T) {
	prod := ordersInstance(t, "table-orders")
	signInPipeline(t, prod)
	root := t.TempDir()
	manifest := &wfdir.Manifest{Kind: wfdir.KindPipeline, Title: "Sales pipeline", Moved: ordersMove}
	manifest.SetBinding(prod.Key(), wfdir.Binding{FeatureID: "collection-1", Tables: map[string]string{"orders.sql": "table-orders"}})
	writePipelineFolder(t, root, manifest, map[string]string{"sales_orders.sql": "SELECT * FROM raw"})
	writePipelineBaseline(t, root, prod.Key(), "collection-1",
		map[string]string{"orders.sql": "SELECT * FROM raw"},
		map[string]wfdir.TableState{"orders.sql": {TableID: "table-orders"}})

	_, stderr, err := runPipelineCLI(t, root, "pipeline", "push")
	if err != nil {
		t.Fatalf("push: %v\n%s", err, stderr)
	}
	if len(prod.created) != 0 {
		t.Fatalf("created %+v", prod.created)
	}
	loaded, err := wfdir.LoadManifest(root, wfdir.PipelineKind)
	if err != nil {
		t.Fatal(err)
	}
	b, _, _ := loaded.Binding(prod.Key())
	if b.Tables["sales_orders.sql"] != "table-orders" || b.Tables["orders.sql"] != "" {
		t.Errorf("legacy binding = %v", b.Tables)
	}
	inst := pipelineStateOf(t, root, prod.Key())
	if inst.Tables["sales_orders.sql"].TableID != "table-orders" || inst.Tables["sales_orders.sql"].Stem != "orders" {
		t.Errorf("legacy baseline = %+v", inst.Tables)
	}
}

// assertNoTableWrites fails on any request a refused push should never have
// sent: a create, a checkout, a code write, a build or a commit.
func assertNoTableWrites(t *testing.T, f *fakePipelineInstance) {
	t.Helper()
	if len(f.created) != 0 || len(f.updates) != 0 || len(f.checkouts) != 0 || len(f.syncs) != 0 || len(f.committed) != 0 {
		t.Errorf("a refused push wrote: created=%v updates=%v checkouts=%v syncs=%v committed=%v",
			f.created, f.updates, f.checkouts, f.syncs, f.committed)
	}
}

// --- automations -------------------------------------------------------------

const nightlyBody = `{"enabled": true, "cronExpr": "0 2 * * *"}`

// THE STATUS COMMANDS evaluate the declarations in memory: a pending move is
// listed (and is not drift — it rewrites the lock, never a row), and an
// undeclared rename is what `sync status` reports apply would refuse.
func TestStatusReportsPendingMovesAndUndeclaredRenames(t *testing.T) {
	prod := ordersInstance(t, "table-orders")
	signInPipeline(t, prod)
	_, root := movedPipelineTree(t,
		[]stackFixture{{name: "prod", fake: prod, bound: map[string]string{"orders.sql": "table-orders"}}},
		map[string]string{"sales_orders.sql": "SELECT * FROM raw"}, ordersMove, nil)
	before := readBytes(t, wfdir.LockPath(root))

	out, _ := runCLI(t, root, "pipeline", "status", "--stack", "prod", "--json")
	report := &pipelineStatusReport{}
	decodeJSONInto(t, out, report)
	if len(report.Moved) != 1 || report.Moved[0].To != "sales_orders.sql" || len(report.Gone) != 0 || len(report.WillCreate) != 0 {
		t.Errorf("status = moved %+v gone %v willCreate %v", report.Moved, report.Gone, report.WillCreate)
	}
	if !bytes.Equal(before, readBytes(t, wfdir.LockPath(root))) {
		t.Fatal("pipeline status rewrote the lock")
	}

	// The same folder with the declaration taken out: an undeclared rename.
	manifest, err := wfdir.LoadManifest(root, wfdir.PipelineKind)
	if err != nil {
		t.Fatal(err)
	}
	manifest.Moved = nil
	if err := wfdir.SaveManifest(root, manifest); err != nil {
		t.Fatal(err)
	}
	out, _ = runCLI(t, root, "pipeline", "status", "--stack", "prod", "--json")
	report = &pipelineStatusReport{}
	decodeJSONInto(t, out, report)
	if len(report.Gone) != 1 || !strings.Contains(report.Gone[0], "orders.sql is bound but gone, and sales_orders.sql is new") {
		t.Errorf("gone = %v", report.Gone)
	}
	verdict := verdictOfPipelineStatus(report, 1, nil)
	if verdict.Verdict != verdictDrifted || !strings.Contains(verdict.Detail, `gone with no "moved" or "removed" entry`) {
		t.Errorf("verdict = %+v", verdict)
	}
}

// --- checks travel with their table ------------------------------------------

// A SAVE THAT FAILS HALF WAY HEALS ON THE NEXT RUN, whichever half failed. The
// local baseline is written before the binding: with the binding written first
// and the baseline failing, the next run would see the move as applied and
// leave the baseline's drift anchor and name at the old path for good.
func TestPipelinePushMoveHealsAFailedSave(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("file permissions do not stop root")
	}
	for _, tc := range []struct {
		name string
		// readOnly is the directory made unwritable for the first push.
		readOnly func(root string) string
	}{
		{"baseline write fails", func(root string) string { return filepath.Join(root, wfdir.StateDirName) }},
		{"binding write fails", func(root string) string { return root }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			prod := ordersInstance(t, "table-orders")
			signInPipeline(t, prod)
			root := t.TempDir()
			manifest := &wfdir.Manifest{Kind: wfdir.KindPipeline, Title: "Sales pipeline", Moved: ordersMove}
			manifest.SetBinding(prod.Key(), wfdir.Binding{FeatureID: "collection-1", Tables: map[string]string{"orders.sql": "table-orders"}})
			writePipelineFolder(t, root, manifest, map[string]string{"sales_orders.sql": "SELECT * FROM raw"})
			writePipelineBaseline(t, root, prod.Key(), "collection-1",
				map[string]string{"orders.sql": "SELECT * FROM raw"},
				map[string]wfdir.TableState{"orders.sql": {TableID: "table-orders", Name: "Orders"}})

			dir := tc.readOnly(root)
			if err := os.Chmod(dir, 0o555); err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { _ = os.Chmod(dir, 0o755) })
			if _, _, err := runPipelineCLI(t, root, "pipeline", "push"); err == nil {
				t.Fatal("a push that could not save its move succeeded")
			}
			if err := os.Chmod(dir, 0o755); err != nil {
				t.Fatal(err)
			}

			if _, stderr, err := runPipelineCLI(t, root, "pipeline", "push"); err != nil {
				t.Fatalf("the push after: %v\n%s", err, stderr)
			}
			if len(prod.created) != 0 {
				t.Fatalf("created %+v", prod.created)
			}
			if b := pipelineBindingOf(t, root, prod.Key()); b.Tables["sales_orders.sql"] != "table-orders" || b.Tables["orders.sql"] != "" {
				t.Errorf("binding = %v", b.Tables)
			}
			inst := pipelineStateOf(t, root, prod.Key())
			got := inst.Tables["sales_orders.sql"]
			if got.TableID != "table-orders" || got.LiveSHA256 != wfdir.HashString("SELECT * FROM raw") || got.Name != "Orders" {
				t.Errorf("baseline at the new path = %+v, want the id, drift anchor and name carried", got)
			}
			if _, stranded := inst.Tables["orders.sql"]; stranded {
				t.Errorf("the baseline is still at the old path: %+v", inst.Tables)
			}
		})
	}
}

// THE CARRIED BASELINE IS A STEM, NOT A RECORDED NAME. A table moved with no
// name recorded keeps its old stem as the baseline — the rule is unchanged — and
// a drift refusal still says it compared against a STEM, with the way out that
// goes with one (declare the current name first), rather than claiming the
// folder once agreed the row was called that.
func TestPipelinePushMovedNameDriftSaysItComparedAgainstAStem(t *testing.T) {
	prod := ordersInstance(t, "table-orders")
	prod.tables["table-orders"].Name = "Orders (renamed in the app)"
	signInPipeline(t, prod)
	_, root := movedPipelineTree(t,
		[]stackFixture{{name: "prod", fake: prod, bound: map[string]string{"orders.sql": "table-orders"}}},
		map[string]string{"sales_orders.sql": "-- @table One row per order.\n-- @name Sales orders\nSELECT * FROM raw"},
		ordersMove, nil)

	out, stderr, err := runPipelineCLI(t, root, "pipeline", "push", "--stack", "prod")
	if err == nil {
		t.Fatalf("a name renamed elsewhere was overwritten\n%s", stderr)
	}
	all := err.Error() + "\n" + out + "\n" + stderr
	want := `"orders", the stem of the file it had before a "moved" entry renamed it (this folder has no name recorded for it; if the current name is the folder's own, declare that name and push once to adopt it)`
	if !strings.Contains(all, want) {
		t.Errorf("the drift refusal does not say it compared against the carried stem:\n%s", all)
	}
	if strings.Contains(all, "last agreed it had") {
		t.Errorf("the refusal claims a name the folder never saw:\n%s", all)
	}
	for _, u := range prod.updates {
		if u.Name != nil {
			t.Errorf("a refused rename was sent: %+v", u)
		}
	}
}

// A FOLDER THAT CANNOT BE READ is `unreadable`, not `address_conflict`: the
// declarations may be perfectly fine, and the reason a tree command gives is
// what tells the reader where to look.
//
// Asked of the evaluation directly, because through `sync apply` the walk and
// the folder read both fail on the same directory first; what is left for the
// evaluation is a folder that became unreadable in between.
func TestAddressEvaluationTellsAnUnreadableFolderFromAConflict(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("file permissions do not stop root")
	}
	root := t.TempDir()
	for path, body := range map[string]string{"orders.sql": "SELECT 1", "metrics/aov.json": metricFileBody(aovRecipe)} {
		if err := wfdir.WriteFile(root, path, body); err != nil {
			t.Fatal(err)
		}
	}
	evaluate := func(removed string) error {
		f := &folder{Root: root, Kind: wfdir.PipelineKind,
			Manifest: &wfdir.Manifest{Kind: wfdir.KindPipeline, Removed: []string{removed}}}
		_, err := f.evaluateAddresses()
		return err
	}

	// A removed file still on disk is a declaration to fix.
	if err := evaluate("orders.sql"); err == nil || addressFailureReason(err) != syncReasonAddressConflict {
		t.Errorf("still on disk: err = %v, reason = %q", err, addressFailureReason(err))
	}

	metrics := filepath.Join(root, wfdir.MetricsDirName)
	if err := os.Chmod(metrics, 0o311); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(metrics, 0o755) })
	err := evaluate("legacy.sql")
	if err == nil {
		t.Fatal("an unreadable folder evaluated")
	}
	if got := addressFailureReason(err); got != syncReasonUnreadable {
		t.Errorf("unreadable folder: reason = %q (%v), want %s", got, err, syncReasonUnreadable)
	}
}

// PUBLISH WITHOUT A PUSH SINCE THE RENAME reads the declarations too. A draft
// staged under orders.sql, then the file renamed with a `moved` entry, then
// `publish`: the draft is published as the renamed file's, and the publish —
// which writes the lock anyway — records the move with it.
func TestPipelinePublishAppliesAMoveDeclaredSinceThePush(t *testing.T) {
	prod := ordersInstance(t, "table-orders")
	signInPipeline(t, prod)
	_, root := movedPipelineTree(t,
		[]stackFixture{{name: "prod", fake: prod, bound: map[string]string{"orders.sql": "table-orders"}}},
		map[string]string{"orders.sql": "SELECT * FROM raw WHERE ok"}, nil, nil)
	if _, stderr, err := runPipelineCLI(t, root, "pipeline", "push", "--stack", "prod"); err != nil {
		t.Fatalf("push: %v\n%s", err, stderr)
	}
	if draft := pipelineStateOf(t, root, prod.Key()).Tables["orders.sql"].DraftID; draft == "" {
		t.Fatal("the push staged no draft; the case under test is a publish of one")
	}
	if err := os.Rename(filepath.Join(root, "orders.sql"), filepath.Join(root, "sales_orders.sql")); err != nil {
		t.Fatal(err)
	}
	manifest := pipelineManifestOf(t, root)
	manifest.Moved = ordersMove
	if err := wfdir.SaveManifest(root, manifest); err != nil {
		t.Fatal(err)
	}

	out, stderr, err := runPipelineCLI(t, root, "pipeline", "publish", "--stack", "prod", "--json")
	if err != nil {
		t.Fatalf("publish: %v\n%s\n%s", err, out, stderr)
	}
	result := &pipelinePublishResult{}
	decodeJSONInto(t, out, result)
	if len(result.Files) != 1 || result.Files[0].Path != "sales_orders.sql" || result.Files[0].Outcome != outcomePublished {
		t.Fatalf("published = %+v\n%s", result.Files, stderr)
	}
	if len(prod.committed) != 1 {
		t.Errorf("committed = %v", prod.committed)
	}
	if got := lockOf(t, root).Binding("prod").Tables; len(got) != 1 || got["sales_orders.sql"] != "table-orders" {
		t.Errorf("binding after publish = %v, want sales_orders.sql → table-orders only", got)
	}
	if len(prod.created) != 0 {
		t.Errorf("created %+v", prod.created)
	}
}

// A PURE MOVE THAT ALSO PRUNES THE BASELINE SAVES THE BASELINE FIRST. A move
// has to reach the local baseline before the lock, or a failure between the two
// leaves the lock saying the move is applied while the baseline's fingerprints
// stay at the old path for good (saveAddresses). Here the baseline cannot be
// written at all: the lock must still bind the OLD path afterwards, so the next
// run applies the move again — and it does.
func TestPipelinePushMoveThatCannotSaveTheBaselineLeavesTheLockUnmoved(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("a read-only directory does not stop root writing to it")
	}
	prod := ordersInstance(t, "table-orders")
	signInPipeline(t, prod)
	_, root := movedPipelineTree(t,
		[]stackFixture{{name: "prod", fake: prod, bound: map[string]string{"orders.sql": "table-orders"}}},
		map[string]string{"orders.sql": "SELECT * FROM raw"}, nil, nil)
	if out, stderr, err := runPipelineCLI(t, root, "pipeline", "push", "--stack", "prod", "--json"); err != nil {
		t.Fatalf("first push: %v\n%s\n%s", err, out, stderr)
	}
	// The rename, declared, and a phantom in the baseline for the prune.
	if err := os.Rename(filepath.Join(root, "orders.sql"), filepath.Join(root, "sales_orders.sql")); err != nil {
		t.Fatal(err)
	}
	manifest, err := wfdir.LoadManifest(root, wfdir.PipelineKind)
	if err != nil {
		t.Fatal(err)
	}
	manifest.Moved = ordersMove
	if err := wfdir.SaveManifest(root, manifest); err != nil {
		t.Fatal(err)
	}
	state, err := wfdir.LoadState(root)
	if err != nil {
		t.Fatal(err)
	}
	for _, inst := range state.Instances {
		inst.State.Files["ghost.sql"] = wfdir.FileState{SHA256: wfdir.HashString("SELECT 1")}
	}
	if err := wfdir.SaveState(root, state); err != nil {
		t.Fatal(err)
	}
	stateDir := filepath.Join(root, ".ronja")
	if err := os.Chmod(stateDir, 0o555); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(stateDir, 0o755) })

	_, stderr, err := runPipelineCLI(t, root, "pipeline", "push", "--stack", "prod", "--json")
	if err == nil {
		t.Fatalf("a push that could not save its baseline succeeded\n%s", stderr)
	}
	if lock := lockOf(t, root); lock.Binding("prod").Tables["orders.sql"] != "table-orders" {
		t.Fatalf("the lock records the move although the baseline could not: %+v\n%s", lock.Binding("prod").Tables, stderr)
	}

	if err := os.Chmod(stateDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if out, stderr, err := runPipelineCLI(t, root, "pipeline", "push", "--stack", "prod", "--json"); err != nil {
		t.Fatalf("the retry: %v\n%s\n%s", err, out, stderr)
	}
	if got := lockOf(t, root).Binding("prod").Tables["sales_orders.sql"]; got != "table-orders" {
		t.Errorf("after the retry sales_orders.sql is bound to %q, want table-orders", got)
	}
	if len(prod.created) != 0 {
		t.Errorf("created %+v", prod.created)
	}
}
