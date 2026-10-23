package commands

import (
	"bytes"
	"os"
	"strings"
	"testing"

	"github.com/ronjatech/ronja-cli/internal/api"
	"github.com/ronjatech/ronja-cli/internal/wfdir"
)

// `moved` and `removed` across a tree: `sync apply` (each stack applies a
// declaration when it is next deployed), its dry run, and `sync check`. The
// fixtures are addresses_test.go's.

// ADAM'S RENAME, end to end. A file renamed orders.sql → sales_orders.sql with
// a `moved` entry, applied to two stacks one after the other: each stack keeps
// ITS OWN table, nothing is created anywhere, and the other stack's entry is
// untouched until that stack is applied.
func TestSyncApplyMovedKeepsEachStacksTableBound(t *testing.T) {
	prod := ordersInstance(t, "table-orders-prod")
	dev := ordersInstance(t, "table-orders-dev")
	tree, root := movedPipelineTree(t,
		[]stackFixture{
			{name: "prod", fake: prod, bound: map[string]string{"orders.sql": "table-orders-prod"}},
			{name: "dev", fake: dev, bound: map[string]string{"orders.sql": "table-orders-dev"}},
		},
		map[string]string{"sales_orders.sql": "SELECT * FROM raw"}, ordersMove, nil)

	signInPipeline(t, prod)
	out, err := runCLI(t, tree, "sync", "apply", "--stack", "prod", "--json")
	if err != nil {
		t.Fatalf("apply prod: %v\n%s", err, out)
	}
	if len(prod.created) != 0 {
		t.Fatalf("prod: a rename CREATED %+v", prod.created)
	}
	lock := lockOf(t, root)
	if got := lock.Binding("prod").Tables; got["sales_orders.sql"] != "table-orders-prod" || got["orders.sql"] != "" {
		t.Fatalf("prod binding after apply = %v, want sales_orders.sql → table-orders-prod only", got)
	}
	// The drift anchor travelled with the id: without it the next push would
	// have nothing to compare the live SQL against.
	if lock.TableLive("prod", "sales_orders.sql") == "" {
		t.Error("prod's live fingerprint did not travel with the move")
	}
	// The stack not yet applied is untouched — a declaration is evaluated by each
	// stack's own apply, whenever that runs.
	if got := lock.Binding("dev").Tables; got["orders.sql"] != "table-orders-dev" || got["sales_orders.sql"] != "" {
		t.Fatalf("dev binding after the PROD apply = %v, want it untouched", got)
	}
	report := applyReportOf(t, out)
	line := applyFolderLine(t, report, "sales")
	if line.Verdict != verdictApplied {
		t.Errorf("prod folder = %+v", line)
	}
	if !strings.Contains(strings.Join(line.Notes, "\n"), "orders.sql → sales_orders.sql") {
		t.Errorf("the apply did not say it re-keyed the lock: %+v", line.Notes)
	}
	if prod.CodeOf("table-orders-prod") != "SELECT * FROM raw" {
		t.Errorf("the prod table's SQL moved: %q", prod.CodeOf("table-orders-prod"))
	}

	signInPipeline(t, dev)
	out, err = runCLI(t, tree, "sync", "apply", "--stack", "dev", "--json")
	if err != nil {
		t.Fatalf("apply dev: %v\n%s", err, out)
	}
	if len(dev.created) != 0 {
		t.Fatalf("dev: a rename CREATED %+v", dev.created)
	}
	lock = lockOf(t, root)
	if got := lock.Binding("dev").Tables; got["sales_orders.sql"] != "table-orders-dev" || got["orders.sql"] != "" {
		t.Fatalf("dev binding after apply = %v", got)
	}
	if got := lock.Binding("prod").Tables; got["sales_orders.sql"] != "table-orders-prod" {
		t.Fatalf("the dev apply disturbed prod: %v", got)
	}
	// Applying again is a no-op for the declaration: from is no longer bound.
	out, err = runCLI(t, tree, "sync", "apply", "--stack", "dev", "--dry-run", "--json")
	if err != nil {
		t.Fatalf("re-apply dry-run: %v\n%s", err, out)
	}
	if actions := unitActions(applyFolderLine(t, applyReportOf(t, out), "sales")); actions["orders.sql"] != "" {
		t.Errorf("an applied move was reported again: %v", actions)
	}
}

// A DRY-RUN LISTS THE MOVE AND WRITES NOTHING: not the lock a real apply
// re-keys, and not a local baseline either.
func TestSyncApplyDryRunListsAMoveAndNeverWritesTheLock(t *testing.T) {
	prod := ordersInstance(t, "table-orders")
	signInPipeline(t, prod)
	tree, root := movedPipelineTree(t,
		[]stackFixture{{name: "prod", fake: prod, bound: map[string]string{"orders.sql": "table-orders"}}},
		map[string]string{"sales_orders.sql": "SELECT * FROM raw"}, ordersMove, nil)
	before := readBytes(t, wfdir.LockPath(root))

	for _, args := range [][]string{
		{"sync", "apply", "--stack", "prod", "--dry-run", "--json"},
		{"sync", "status", "--stack", "prod", "--json"},
		{"sync", "check", "--stack", "prod", "--json"},
	} {
		out, _ := runCLI(t, tree, args...)
		if after := readBytes(t, wfdir.LockPath(root)); !bytes.Equal(before, after) {
			t.Fatalf("`%s` rewrote %s:\n%s\n---\n%s", strings.Join(args, " "), wfdir.LockName, before, after)
		}
		if _, err := os.Stat(wfdir.StatePath(root)); err == nil {
			t.Fatalf("`%s` wrote a local baseline", strings.Join(args, " "))
		}
		if args[1] == "apply" {
			line := applyFolderLine(t, applyReportOf(t, out), "sales")
			var move *syncApplyUnitReport
			for i := range line.Units {
				if line.Units[i].Action == string(applyActionMove) {
					move = &line.Units[i]
				}
			}
			if move == nil || move.Path != "orders.sql" || move.To != "sales_orders.sql" || move.ID != "table-orders" {
				t.Fatalf("the dry-run did not list the move: %+v", line.Units)
			}
			// The same row, not a create — and one that already holds the
			// moved file's SQL, which the dry run compares.
			if actions := unitActions(line); actions["sales_orders.sql"] != string(applyActionUnchanged) {
				t.Errorf("the moved file is not its row, unchanged: %v", actions)
			}
		}
	}
	if len(prod.created) != 0 || len(prod.updates) != 0 {
		t.Errorf("a read-only command wrote: created=%v updates=%v", prod.created, prod.updates)
	}
}

// NEITHER END BOUND is a no-op: a stack that never had the row (or already
// re-keyed it and then lost the lock) has nothing to move, and the new file is
// what it always would have been — a create.
func TestSyncApplyMoveWithNeitherEndBoundIsANoOp(t *testing.T) {
	prod := ordersInstance(t, "table-orders")
	signInPipeline(t, prod)
	tree, root := movedPipelineTree(t,
		[]stackFixture{{name: "prod", fake: prod, bound: map[string]string{"other.sql": "table-orders"}}},
		map[string]string{"other.sql": "SELECT * FROM raw", "sales_orders.sql": "SELECT 1"}, ordersMove, nil)
	before := readBytes(t, wfdir.LockPath(root))

	out, _ := runCLI(t, tree, "sync", "apply", "--stack", "prod", "--dry-run", "--json")
	line := applyFolderLine(t, applyReportOf(t, out), "sales")
	actions := unitActions(line)
	if actions["orders.sql"] != "" || actions["sales_orders.sql"] != string(applyActionCreate) {
		t.Errorf("units = %v, want no move and a create for the unbound file", actions)
	}
	if after := readBytes(t, wfdir.LockPath(root)); !bytes.Equal(before, after) {
		t.Error("a no-op move rewrote the lock")
	}
}

// RENAME DETECTION in `sync apply`: an undeclared rename is REFUSED — no create
// for the new file, nothing published — and the refusal names both halves.
func TestSyncApplyRefusesAnUndeclaredRename(t *testing.T) {
	prod := ordersInstance(t, "table-orders")
	signInPipeline(t, prod)
	tree, _ := movedPipelineTree(t,
		[]stackFixture{{name: "prod", fake: prod, bound: map[string]string{"orders.sql": "table-orders"}}},
		map[string]string{"sales_orders.sql": "SELECT * FROM raw"}, nil, nil)

	for _, extra := range [][]string{nil, {"--allow-create"}} {
		args := append([]string{"sync", "apply", "--stack", "prod", "--json"}, extra...)
		out, err := runCLI(t, tree, args...)
		if code := exitCodeOf(err); code != syncExitDrifted {
			t.Fatalf("%v: exit = %d (%v)\n%s", extra, code, err, out)
		}
		line := applyFolderLine(t, applyReportOf(t, out), "sales")
		if line.Reason != syncReasonOrphanedFile {
			t.Fatalf("%v: folder = %+v, want %s", extra, line, syncReasonOrphanedFile)
		}
		want := `orders.sql is bound but gone, and sales_orders.sql is new — add a "moved" entry`
		if !strings.Contains(line.Detail, want) || !strings.Contains(line.Detail, `"removed" entry if orders.sql was dropped on purpose`) {
			t.Errorf("%v: detail = %q", extra, line.Detail)
		}
	}
	if len(prod.created) != 0 || len(prod.committed) != 0 {
		t.Errorf("a refused rename wrote: created=%v committed=%v", prod.created, prod.committed)
	}
}

// An UNDECLARED DELETE with no new file beside it is refused the same way, and
// the sentence names no new file.
func TestSyncApplyRefusesAnUndeclaredDeleteWithoutNamingANewFile(t *testing.T) {
	prod := ordersInstance(t, "table-orders")
	prod.AddTable(&api.Table{ID: "table-refunds", Name: "refunds", FeatureID: "collection-1", Code: "SELECT 3"})
	signInPipeline(t, prod)
	tree, _ := movedPipelineTree(t,
		[]stackFixture{{name: "prod", fake: prod, bound: map[string]string{
			"orders.sql": "table-orders", "refunds.sql": "table-refunds"}}},
		map[string]string{"orders.sql": "SELECT * FROM raw"}, nil, nil)

	out, _ := runCLI(t, tree, "sync", "apply", "--stack", "prod", "--json")
	line := applyFolderLine(t, applyReportOf(t, out), "sales")
	if line.Reason != syncReasonOrphanedFile || !strings.Contains(line.Detail, "refunds.sql is bound but gone — add") {
		t.Fatalf("folder = %+v", line)
	}
	if strings.Contains(line.Detail, "is new") {
		t.Errorf("named a new file where there is none: %q", line.Detail)
	}
}

// RENAME, THEN RETIRE: orders.sql was moved to sales.sql, and later sales.sql
// is dropped with a `removed` entry while the `moved` entry stays. A stack that
// already applied the move has its entry dropped; a stack still lagging at
// orders.sql is moved and then dropped in the same evaluation — no rename
// detection, and no request that touches either row.
func TestSyncApplyRemovesAMovesDestinationOnEveryStack(t *testing.T) {
	fixture := func(id string) *fakePipelineInstance {
		f := ordersInstance(t, "table-orders-"+id)
		f.AddTable(&api.Table{ID: "table-keep-" + id, Name: "keep", FeatureID: "collection-1", Code: "SELECT 1"})
		return f
	}
	prod, dev := fixture("prod"), fixture("dev")
	tree, root := movedPipelineTree(t,
		[]stackFixture{
			{name: "prod", fake: prod, bound: map[string]string{"sales.sql": "table-orders-prod", "keep.sql": "table-keep-prod"}},
			{name: "dev", fake: dev, bound: map[string]string{"orders.sql": "table-orders-dev", "keep.sql": "table-keep-dev"}},
		},
		map[string]string{"keep.sql": "SELECT 1"},
		[]wfdir.MovedEntry{{From: "orders.sql", To: "sales.sql"}}, []string{"sales.sql"})

	for _, s := range []struct {
		name string
		fake *fakePipelineInstance
	}{{"prod", prod}, {"dev", dev}} {
		signInPipeline(t, s.fake)
		out, err := runCLI(t, tree, "sync", "apply", "--stack", s.name, "--json")
		if err != nil {
			t.Fatalf("apply %s: %v\n%s", s.name, err, out)
		}
		line := applyFolderLine(t, applyReportOf(t, out), "sales")
		if line.Verdict != verdictApplied || line.Reason != "" {
			t.Fatalf("%s folder = %+v", s.name, line)
		}
		if got := lockOf(t, root).Binding(s.name).Tables; len(got) != 1 || got["keep.sql"] != "table-keep-"+s.name {
			t.Fatalf("%s binding = %v, want keep.sql only", s.name, got)
		}
		if len(s.fake.created) != 0 {
			t.Errorf("%s: created %+v", s.name, s.fake.created)
		}
		for _, r := range s.fake.requestOrder() {
			if strings.Contains(r, "table-orders-") {
				t.Errorf("%s: a request touched the retired row: %s", s.name, r)
			}
		}
	}
}

// `sync check` REPORTS `moved` BELOW FORMAT 4. Every acting command refuses the
// folder, and a CLI older than the format ignores the key and creates a second
// row — so a tree gate that called the folder clean would be the one place the
// problem went unsaid.
func TestSyncCheckReportsMovedBelowItsFormatVersion(t *testing.T) {
	prod := ordersInstance(t, "table-orders")
	signInPipeline(t, prod)
	tree, root := movedPipelineTree(t,
		[]stackFixture{{name: "prod", fake: prod, bound: map[string]string{"orders.sql": "table-orders"}}},
		map[string]string{"sales_orders.sql": "SELECT * FROM raw"}, ordersMove, nil)
	raw := readBytes(t, wfdir.ManifestPath(root))
	raw = bytes.Replace(raw, []byte(`"formatVersion": 5,`), []byte(`"formatVersion": 2,`), 1)
	if err := os.WriteFile(wfdir.ManifestPath(root), raw, 0o644); err != nil {
		t.Fatal(err)
	}

	out, err := runCLI(t, tree, "sync", "check", "--stack", "prod", "--json")
	if exitCodeOf(err) == 0 {
		t.Fatalf("sync check passed a folder every push refuses\n%s", out)
	}
	folders := checkFolders(t, out)
	if len(folders) != 1 {
		t.Fatalf("folders = %v", folders)
	}
	findings, _ := folders[0]["findings"].([]any)
	var said bool
	for _, finding := range findings {
		if s, _ := finding.(string); strings.Contains(s, `Add "formatVersion": 5`) {
			said = true
		}
	}
	if !said || folders[0]["verdict"] != verdictBroken {
		t.Errorf("folder = %v, want a broken verdict naming the format", folders[0])
	}
}

// `sync apply` pushes and then publishes the same folder, and both say what the
// evaluation re-keyed: the note is said once.
func TestSyncApplySaysEachMoveOnce(t *testing.T) {
	prod := ordersInstance(t, "table-orders")
	tree, _ := movedPipelineTree(t,
		[]stackFixture{{name: "prod", fake: prod, bound: map[string]string{"orders.sql": "table-orders"}}},
		map[string]string{"sales_orders.sql": "SELECT * FROM raw WHERE shipped"}, ordersMove, nil)
	signInPipeline(t, prod)
	var out string
	var err error
	stderr := captureStderr(t, func() {
		out, err = runCLI(t, tree, "sync", "apply", "--stack", "prod", "--json")
	})
	if err != nil {
		t.Fatalf("apply: %v\n%s", err, out)
	}
	if n := strings.Count(stderr, "Note: moved orders.sql → sales_orders.sql"); n != 1 {
		t.Errorf("the move was noted %d times, want once:\n%s", n, stderr)
	}
}
