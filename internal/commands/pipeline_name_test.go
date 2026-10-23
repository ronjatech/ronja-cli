package commands

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/ronjatech/ronja-cli/internal/api"
	"github.com/ronjatech/ronja-cli/internal/wfdir"
)

// A table's NAME as a declared, converging attribute: `-- @name` in the .sql
// header renames the SAME row in place, a rename made elsewhere is refused as
// drift rather than reverted, and a file that declares nothing never sends a
// name at all.
//
// The fake models the server, not the CLI's intent: a DRAFT's rename is stored
// unchecked, the COMMIT lands the draft's name on the live row and asks the
// feature's namespace there, and a live root row's rename is checked at once.

const salesOrders = "Sales orders"

// namedTableTree lays out a stack folder whose orders.sql is bound to a live
// table called `liveName`, with the given file content.
func namedTableTree(t *testing.T, f *fakePipelineInstance, liveName, content string) (tree, root string) {
	t.Helper()
	f.AddFeature("collection-1", "Sales pipeline", "private")
	f.AddTable(&api.Table{ID: "table-orders", Name: liveName, FeatureID: "collection-1", Code: "SELECT * FROM raw"})
	return applyPipelineTree(t, f, map[string]string{"orders.sql": content},
		map[string]string{"orders.sql": "table-orders"}, map[string]string{"orders.sql": "SELECT * FROM raw"})
}

func namedOrders(name, body string) string {
	return "-- @name " + name + "\n" + body
}

// forgetLocalBaseline is what a CI runner's checkout looks like: the committed
// lock, and no .ronja/ — so every file is pushed again.
func forgetLocalBaseline(t *testing.T, root string) {
	t.Helper()
	if err := os.RemoveAll(filepath.Join(root, ".ronja")); err != nil {
		t.Fatal(err)
	}
}

func lockedTableName(t *testing.T, root string) string {
	t.Helper()
	lock, err := wfdir.LoadLock(root)
	if err != nil {
		t.Fatal(err)
	}
	return lock.TableName("prod", "orders.sql")
}

func renamesSent(f *fakePipelineInstance) []string {
	var out []string
	for _, u := range f.updates {
		if u.Name != nil {
			out = append(out, u.ID+"="+*u.Name)
		}
	}
	return out
}

// ADAM'S CASE. Add `-- @name Sales orders` to orders.sql and apply: the same row
// is renamed in place — same id, nothing created — the applied name is recorded
// in the lock, and a second apply writes no rename at all.
func TestSyncApplyRenamesATableInPlaceFromItsNameHeader(t *testing.T) {
	f := newFakePipelineInstance(t)
	signInPipeline(t, f)
	tree, root := namedTableTree(t, f, "orders", namedOrders(salesOrders, "SELECT * FROM raw WHERE shipped"))

	out, err := runCLI(t, tree, "sync", "apply", "--stack", "prod", "--json")
	if code := exitCodeOf(err); code != syncExitClean {
		t.Fatalf("exit = %d (%v)\n%s", code, err, out)
	}
	if len(f.created) != 0 {
		t.Fatalf("a rename created a table: %+v", f.created)
	}
	if got := f.tables["table-orders"].Name; got != salesOrders {
		t.Fatalf("live table-orders is called %q, want %q", got, salesOrders)
	}
	if got := lockedTableName(t, root); got != salesOrders {
		t.Errorf("lock records name %q, want %q", got, salesOrders)
	}

	// The SECOND apply, from a fresh checkout so the file is visited again: the
	// live table already holds the declared name, so no rename is sent.
	forgetLocalBaseline(t, root)
	f.updates = nil
	out, err = runCLI(t, tree, "sync", "apply", "--stack", "prod", "--json")
	if code := exitCodeOf(err); code != syncExitClean {
		t.Fatalf("second apply: exit = %d (%v)\n%s", code, err, out)
	}
	if sent := renamesSent(f); len(sent) != 0 {
		t.Errorf("the second apply sent a rename the table already had: %v", sent)
	}
}

// A WEB-APP RENAME AFTERWARDS IS DRIFT, refused rather than reverted — and
// --force is the one-folder override that renames it back.
func TestPipelinePushRefusesANameRenamedElsewhere(t *testing.T) {
	f := newFakePipelineInstance(t)
	signInPipeline(t, f)
	tree, root := namedTableTree(t, f, "orders", namedOrders(salesOrders, "SELECT * FROM raw WHERE shipped"))
	if out, err := runCLI(t, tree, "sync", "apply", "--stack", "prod", "--json"); err != nil {
		t.Fatalf("apply: %v\n%s", err, out)
	}
	f.tables["table-orders"].Name = "Renamed in the web app"

	forgetLocalBaseline(t, root)
	f.updates = nil
	var out string
	var err error
	stderr := captureStderr(t, func() {
		out, err = runCLI(t, tree, "sync", "apply", "--stack", "prod", "--json")
	})
	if code := exitCodeOf(err); code != syncExitDrifted {
		t.Fatalf("exit = %d (%v), want %d\n%s", code, err, syncExitDrifted, out)
	}
	if got := f.tables["table-orders"].Name; got != "Renamed in the web app" {
		t.Fatalf("the web rename was reverted to %q", got)
	}
	if sent := renamesSent(f); len(sent) != 0 {
		t.Fatalf("a rename was sent over drift: %v", sent)
	}
	if !strings.Contains(stderr, `"Renamed in the web app"`) || !strings.Contains(stderr, "the name of table-orders changed") {
		t.Errorf("the refusal does not say the name moved, or what it is now:\n%s", stderr)
	}

	// --force renames it back, on the draft, and acknowledges the live name so
	// the next push does not re-report it before the publish lands.
	pushOut, err := runCLI(t, root, "pipeline", "push", "--stack", "prod", "--force", "--json")
	if err != nil {
		t.Fatalf("push --force: %v\n%s", err, pushOut)
	}
	if sent := renamesSent(f); len(sent) != 1 || !strings.HasSuffix(sent[0], "="+salesOrders) {
		t.Fatalf("renames sent = %v, want the declared name on the draft", sent)
	}
	if got := lockedTableName(t, root); got != "Renamed in the web app" {
		t.Errorf("lock records %q, want the live name --force acknowledged", got)
	}
	if _, err := runCLI(t, root, "pipeline", "publish", "--stack", "prod", "--json"); err != nil {
		t.Fatalf("publish: %v", err)
	}
	if got := f.tables["table-orders"].Name; got != salesOrders {
		t.Errorf("after publish the table is called %q, want %q", got, salesOrders)
	}
	if got := lockedTableName(t, root); got != salesOrders {
		t.Errorf("after publish the lock records %q, want %q", got, salesOrders)
	}
}

// A BUILD SERVER THAT NEVER COMMITS THE LOCK. Its first rename lands (live
// still holds the stem), but the name it set is recorded only in a lock it
// throws away, so its second rename compares the first new name against the
// stem and is refused as a rename made in Ronja. The refusal is kept — nothing
// can tell the two apart — but it has to name the way out: a local push with
// --force, then a commit of ronja.lock.json. `sync apply` has no --force.
func TestSyncApplySecondRenameWithoutACommittedLockNamesTheRemedy(t *testing.T) {
	f := newFakePipelineInstance(t)
	signInPipeline(t, f)
	tree, root := namedTableTree(t, f, "orders", namedOrders(salesOrders, "SELECT * FROM raw WHERE shipped"))
	lockPath := filepath.Join(root, wfdir.LockName)
	committedLock, err := os.ReadFile(lockPath)
	if err != nil {
		t.Fatal(err)
	}
	if out, err := runCLI(t, tree, "sync", "apply", "--stack", "prod", "--json"); err != nil {
		t.Fatalf("first apply: %v\n%s", err, out)
	}
	if err := os.WriteFile(lockPath, committedLock, 0o644); err != nil {
		t.Fatal(err)
	}
	forgetLocalBaseline(t, root)
	writeLocal(t, root, "orders.sql", namedOrders("Orders by day", "SELECT * FROM raw WHERE shipped"))

	var out string
	stderr := captureStderr(t, func() {
		out, err = runCLI(t, tree, "sync", "apply", "--stack", "prod", "--json")
	})
	if code := exitCodeOf(err); code != syncExitDrifted {
		t.Fatalf("exit = %d (%v), want %d\n%s", code, err, syncExitDrifted, out)
	}
	for _, want := range []string{"ronja pipeline push --stack prod --force", wfdir.LockName} {
		if !strings.Contains(stderr, want) {
			t.Errorf("the refusal does not name the remedy (%q):\n%s", want, stderr)
		}
	}
	if _, err := runCLI(t, root, "pipeline", "push", "--stack", "prod", "--force", "--json"); err != nil {
		t.Fatalf("the named remedy failed: %v", err)
	}
	if sent := renamesSent(f); len(sent) == 0 || !strings.HasSuffix(sent[len(sent)-1], "=Orders by day") {
		t.Errorf("renames sent = %v, want the second name", sent)
	}
}

// THE RENAME ARM WITH A RECORDED BASELINE: the live table holds the name this
// folder last agreed with, so a new declared name is a rename to send — the
// stem plays no part once something is recorded.
func TestPipelinePushRenamesFromARecordedBaseline(t *testing.T) {
	f := newFakePipelineInstance(t)
	signInPipeline(t, f)
	_, root := namedTableTree(t, f, "Old orders", namedOrders(salesOrders, "SELECT * FROM raw"))
	lock, _ := wfdir.LoadLock(root)
	lock.SetTableName("prod", "orders.sql", "table-orders", "Old orders")
	if err := wfdir.SaveLock(root, lock); err != nil {
		t.Fatal(err)
	}
	if out, err := runCLI(t, root, "pipeline", "push", "--stack", "prod", "--json"); err != nil {
		t.Fatalf("push: %v\n%s", err, out)
	}
	if sent := renamesSent(f); len(sent) != 1 {
		t.Fatalf("renames sent = %v, want one", sent)
	}
}

// LIVE EQUALS DESIRED IS NEVER DRIFT. The lock records a name that is neither
// the live one nor the declared one — the anchor write was lost after a publish
// that DID land — and the live table already holds what the file declares. That
// is the folder's own work: no refusal, no rename, and the anchor is re-taken.
func TestPipelinePushReanchorsANameTheTableAlreadyHolds(t *testing.T) {
	f := newFakePipelineInstance(t)
	signInPipeline(t, f)
	_, root := namedTableTree(t, f, salesOrders, namedOrders(salesOrders, "SELECT * FROM raw WHERE shipped"))
	lock, _ := wfdir.LoadLock(root)
	lock.SetTableName("prod", "orders.sql", "table-orders", "A stale anchor")
	if err := wfdir.SaveLock(root, lock); err != nil {
		t.Fatal(err)
	}
	if out, err := runCLI(t, root, "pipeline", "push", "--stack", "prod", "--json"); err != nil {
		t.Fatalf("push refused its own published name: %v\n%s", err, out)
	}
	if sent := renamesSent(f); len(sent) != 0 {
		t.Errorf("renames sent = %v, want none", sent)
	}
	if got := lockedTableName(t, root); got != salesOrders {
		t.Errorf("lock records %q, want it re-anchored to %q", got, salesOrders)
	}
}

// AN ABSENT NAME IS NEVER WRITTEN. A file with no `-- @name` against a table
// renamed in the web app: the rename stands, nothing is sent, nothing refuses.
func TestPipelinePushNeverSendsAnUndeclaredName(t *testing.T) {
	f := newFakePipelineInstance(t)
	signInPipeline(t, f)
	_, root := namedTableTree(t, f, "Somebody's name", "SELECT * FROM raw WHERE shipped")
	if out, err := runCLI(t, root, "pipeline", "push", "--stack", "prod", "--json"); err != nil {
		t.Fatalf("push: %v\n%s", err, out)
	}
	if sent := renamesSent(f); len(sent) != 0 {
		t.Errorf("an undeclared name was sent: %v", sent)
	}
	if got := lockedTableName(t, root); got != "" {
		t.Errorf("an undeclared name was recorded: %q", got)
	}
}

// A FOLDER WITH NO RECORDED NAME — one cloned before a clone recorded it — has
// only the file's stem as a baseline, and a live name that merely SLUGS to the
// stem is not it: "Sales orders" on Sales-orders.sql may as well be a
// colleague's punctuation-only rename, and reading it as the folder's own would
// revert it. The first `-- @name` is refused instead; declaring the current name
// once adopts it, after which the rename goes through.
func TestPipelinePushRefusesANameThatOnlySlugsToTheStem(t *testing.T) {
	f := newFakePipelineInstance(t)
	signInPipeline(t, f)
	f.AddFeature("collection-1", "Sales pipeline", "private")
	f.AddTable(&api.Table{ID: "table-orders", Name: salesOrders, FeatureID: "collection-1", Code: "SELECT * FROM raw"})
	_, root := applyPipelineTree(t, f,
		map[string]string{"Sales-orders.sql": namedOrders("Orders, all lines", "SELECT * FROM raw")},
		map[string]string{"Sales-orders.sql": "table-orders"}, map[string]string{"Sales-orders.sql": "SELECT * FROM raw"})
	out, err := runCLI(t, root, "pipeline", "push", "--stack", "prod", "--json")
	if err == nil {
		t.Fatalf("push renamed a table whose name was never recorded, on the strength of a slug:\n%s", out)
	}
	if sent := renamesSent(f); len(sent) != 0 {
		t.Fatalf("renames sent = %v, want none", sent)
	}

	// The way out the refusal names: declare the name it has now, push once.
	if err := wfdir.WriteFile(root, "Sales-orders.sql", namedOrders(salesOrders, "SELECT * FROM raw")); err != nil {
		t.Fatal(err)
	}
	if out, err := runCLI(t, root, "pipeline", "push", "--stack", "prod", "--json"); err != nil {
		t.Fatalf("declaring the current name: %v\n%s", err, out)
	}
	if err := wfdir.WriteFile(root, "Sales-orders.sql", namedOrders("Orders, all lines", "SELECT * FROM raw")); err != nil {
		t.Fatal(err)
	}
	if out, err := runCLI(t, root, "pipeline", "push", "--stack", "prod", "--json"); err != nil {
		t.Fatalf("rename after adopting the name: %v\n%s", err, out)
	}
	if sent := renamesSent(f); len(sent) != 1 || sent[0] == "" || !strings.HasSuffix(sent[0], "=Orders, all lines") {
		t.Errorf("renames sent = %v, want the declared name once", sent)
	}
}

// THE DRAFT LEG: our own open draft was renamed by somebody else (chat, the web
// builder), and the publish of it would land their name. Refused, like a draft
// whose SQL moved.
func TestPipelinePushRefusesADraftRenamedElsewhere(t *testing.T) {
	f := newFakePipelineInstance(t)
	signInPipeline(t, f)
	_, root := namedTableTree(t, f, "orders", namedOrders(salesOrders, "SELECT * FROM raw"))
	draft := f.AddDraft("table-orders", "table-draft-x", "SELECT * FROM raw")
	draft.Name = "Chat's idea"

	out, err := runCLI(t, root, "pipeline", "push", "--stack", "prod", "--json")
	if err == nil {
		t.Fatalf("push went ahead over a renamed draft:\n%s", out)
	}
	if sent := renamesSent(f); len(sent) != 0 {
		t.Errorf("renames sent = %v, want none", sent)
	}
	if !strings.Contains(out, "Chat's idea") {
		t.Errorf("the refusal does not name the draft's name:\n%s", out)
	}
}

// A NAME THE FEATURE ALREADY HOLDS is refused where the server asks the
// namespace — the commit — and the refusal names the header as the fix.
func TestPipelinePublishRefusesATakenDeclaredName(t *testing.T) {
	f := newFakePipelineInstance(t)
	signInPipeline(t, f)
	_, root := namedTableTree(t, f, "orders", namedOrders(salesOrders, "SELECT * FROM raw WHERE shipped"))
	f.AddTable(&api.Table{ID: "table-other", Name: salesOrders, FeatureID: "collection-1", Code: "SELECT 1"})

	if out, err := runCLI(t, root, "pipeline", "push", "--stack", "prod", "--json"); err != nil {
		t.Fatalf("push: %v\n%s", err, out)
	}
	out, err := runCLI(t, root, "pipeline", "publish", "--stack", "prod", "--json")
	if err == nil {
		t.Fatalf("publish landed a taken name:\n%s", out)
	}
	if !strings.Contains(out, "-- @name") {
		t.Errorf("the refusal does not point at the header:\n%s", out)
	}
	if got := f.tables["table-orders"].Name; got != "orders" {
		t.Errorf("the live table was renamed to %q", got)
	}
}

// A NEW FILE WITH A DECLARED NAME is created under that name, and the name is
// recorded with the binding — the stem is only what an UNDECLARED name gets.
func TestPipelinePushCreatesUnderTheDeclaredName(t *testing.T) {
	f := newFakePipelineInstance(t)
	signInPipeline(t, f)
	f.AddFeature("collection-1", "Sales pipeline", "private")
	_, root := applyPipelineTree(t, f,
		map[string]string{"orders.sql": namedOrders(salesOrders, "SELECT 1")}, nil, nil)
	if out, err := runCLI(t, root, "pipeline", "push", "--stack", "prod", "--json"); err != nil {
		t.Fatalf("push: %v\n%s", err, out)
	}
	if len(f.created) != 1 || f.created[0].Name != salesOrders {
		t.Fatalf("created = %+v, want one table called %q", f.created, salesOrders)
	}
	if got := lockedTableName(t, root); got != salesOrders {
		t.Errorf("lock records %q, want %q", got, salesOrders)
	}
}
