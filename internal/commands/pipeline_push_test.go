package commands

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/ronjatech/ronja-cli/internal/api"
	"github.com/ronjatech/ronja-cli/internal/wfdir"
)

// `ronja pipeline push`.
//
// Four properties carry the whole command, and each is invisible in the output:
// the ORDER the files go up in, the row each write is ADDRESSED to, the two-leg
// drift guard, and the baseline left behind when something fails half way.

// editFile overwrites one file in a folder, which is what makes it dirty.
func editFile(t *testing.T, root, path, content string) {
	t.Helper()
	if err := wfdir.WriteFile(root, path, content); err != nil {
		t.Fatalf("write %s: %v", path, err)
	}
}

// syncOrder is the ids the fake was asked to build, in order.
func syncOrder(f *fakePipelineInstance) []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string{}, f.syncs...)
}

// TestPipelinePushEditsTheDraftAndBuildsIt: the golden path, and the two things
// it must never get wrong — writing the LIVE row instead of the draft (accepted,
// and silently lost the moment anybody commits), and omitting inputModels
// (server-side derivation fires only on an empty list, and a checked-out draft
// inherits its parent's, so an omitted field 400s the moment an edit adds an
// upstream).
func TestPipelinePushEditsTheDraftAndBuildsIt(t *testing.T) {
	f := newFakePipelineInstance(t)
	signInPipeline(t, f)
	root := seedBoundFolder(t, f)
	editFile(t, root, "revenue.sql", "SELECT 1 FROM {{ ref('table-orders') }}")

	out, stderr, err := runPipelineCLI(t, root, "pipeline", "push", "--json")
	if err != nil {
		t.Fatalf("push: %v\n%s", err, stderr)
	}
	payload := decodeJSON(t, out)
	files := payload["files"].([]any)
	if len(files) != 1 {
		t.Fatalf("want one pushed file, got %+v", files)
	}
	file := files[0].(map[string]any)
	if file["path"] != "revenue.sql" || file["outcome"] != pushOutcomePushed {
		t.Errorf("file = %+v", file)
	}
	if file["verdict"] != api.BuildVerdictOK {
		t.Errorf("verdict = %v", file["verdict"])
	}

	if len(f.updates) != 1 {
		t.Fatalf("want one PUT, got %+v", f.updates)
	}
	update := f.updates[0]
	// Addressed to the DRAFT, never to the live row.
	if update.ID == "table-revenue" {
		t.Fatal("push wrote the LIVE table; a commit would overwrite that from the draft")
	}
	if update.ID != file["draftID"] {
		t.Errorf("PUT went to %s, report says %v", update.ID, file["draftID"])
	}
	if update.Code != "SELECT 1 FROM {{ ref('table-orders') }}" {
		t.Errorf("code = %q", update.Code)
	}
	// Derived client-side from the refs in the file, and sent on every write.
	if len(update.InputModels) != 1 || update.InputModels[0] != "table-orders" {
		t.Errorf("inputModels = %+v", update.InputModels)
	}
	// The live table is untouched until publish.
	if f.CodeOf("table-revenue") != "SELECT * FROM {{ ref('table-orders') }}" {
		t.Errorf("the live table changed: %q", f.CodeOf("table-revenue"))
	}
	assertPipelineBaselineMatchesDisk(t, root, f.Key())
	// The draft pointer is recorded, so publish and discard can find it.
	inst := pipelineStateOf(t, root, f.Key())
	if inst.Tables["revenue.sql"].DraftID == "" {
		t.Errorf("no draft recorded: %+v", inst.Tables["revenue.sql"])
	}
}

// TestPipelinePushIsUpToDateWhenNothingChanged: a push with nothing to do must
// make no writes at all, and must not open a draft on a table just for looking.
func TestPipelinePushIsUpToDateWhenNothingChanged(t *testing.T) {
	f := newFakePipelineInstance(t)
	signInPipeline(t, f)
	root := seedBoundFolder(t, f)

	out, _, err := runPipelineCLI(t, root, "pipeline", "push", "--json")
	if err != nil {
		t.Fatalf("push: %v", err)
	}
	if decodeJSON(t, out)["upToDate"] != true {
		t.Errorf("payload = %s", out)
	}
	if len(f.checkouts) != 0 || len(f.updates) != 0 || len(f.syncs) != 0 {
		t.Errorf("an up-to-date push wrote something: checkouts=%v updates=%v syncs=%v",
			f.checkouts, f.updates, f.syncs)
	}
}

// TestPipelinePushBuildsInDependencyOrder: a downstream table built before its
// upstream has landed reads the upstream's PREVIOUS data, so the confidence
// report describes a state that never existed.
//
// The filenames are chosen so alphabetical order is the WRONG order — otherwise
// a push that never sorted at all would pass.
func TestPipelinePushBuildsInDependencyOrder(t *testing.T) {
	f := newFakePipelineInstance(t)
	signInPipeline(t, f)
	f.AddFeature("collection-1", "Sales", "private")
	f.AddTable(&api.Table{ID: "table-orders", Name: "Orders", FeatureID: "collection-1", Code: "SELECT 0"})
	f.AddTable(&api.Table{ID: "table-revenue", Name: "Revenue", FeatureID: "collection-1", Code: "SELECT 0"})

	root := t.TempDir()
	manifest := &wfdir.Manifest{Kind: wfdir.KindPipeline, Title: "Sales"}
	manifest.SetBinding(f.Key(), wfdir.Binding{
		FeatureID: "collection-1",
		Tables:    map[string]string{"a_revenue.sql": "table-revenue", "b_orders.sql": "table-orders"},
	})
	writePipelineFolder(t, root, manifest, map[string]string{
		"a_revenue.sql": "SELECT * FROM {{ ref('table-orders') }}",
		"b_orders.sql":  "SELECT * FROM raw",
	})

	if _, stderr, err := runPipelineCLI(t, root, "pipeline", "push"); err != nil {
		t.Fatalf("push: %v\n%s", err, stderr)
	}

	order := syncOrder(f)
	if len(order) != 2 {
		t.Fatalf("want two builds, got %+v", order)
	}
	first, second := f.tables[order[0]], f.tables[order[1]]
	if first == nil || second == nil {
		// Both drafts still exist (nothing was committed), so a nil here means
		// the ids recorded are not the ones that were built.
		t.Fatalf("built ids %v do not resolve to rows", order)
	}
	if first.ParentModelID != "table-orders" {
		t.Errorf("built %s (of %s) first; the upstream b_orders.sql must go first",
			order[0], first.ParentModelID)
	}
	if second.ParentModelID != "table-revenue" {
		t.Errorf("built %s (of %s) second", order[1], second.ParentModelID)
	}
}

// TestPipelinePushNotesAnUpstreamBuiltFromLive: draft-overlay chaining is a
// follow-up, so a downstream pushed alongside a dirty upstream was computed from
// the upstream as it is TODAY — not as it will be once the upstream is
// published. Silence there is a confidence report that quietly means something
// else.
func TestPipelinePushNotesAnUpstreamBuiltFromLive(t *testing.T) {
	f := newFakePipelineInstance(t)
	signInPipeline(t, f)
	root := seedBoundFolder(t, f)
	editFile(t, root, "orders.sql", "SELECT 2 FROM raw")
	editFile(t, root, "revenue.sql", "SELECT 2 FROM {{ ref('table-orders') }}")

	out, stderr, err := runPipelineCLI(t, root, "pipeline", "push", "--json")
	if err != nil {
		t.Fatalf("push: %v\n%s", err, stderr)
	}
	if !strings.Contains(stderr, "built against the LIVE orders.sql") {
		t.Errorf("no upstream note on stderr:\n%s", stderr)
	}
	payload := decodeJSON(t, out)
	found := false
	for _, entry := range payload["files"].([]any) {
		file := entry.(map[string]any)
		if file["path"] != "revenue.sql" {
			continue
		}
		notes, _ := file["notes"].([]any)
		for _, note := range notes {
			if note == "validated against live orders.sql" {
				found = true
			}
		}
	}
	if !found {
		t.Errorf("the note is not machine-readable:\n%s", out)
	}
}

// TestPipelinePushRefusesACycle: a loop has no build order at all, and the
// server would refuse it later with a message about one table.
func TestPipelinePushRefusesACycle(t *testing.T) {
	f := newFakePipelineInstance(t)
	signInPipeline(t, f)
	root := seedBoundFolder(t, f)
	editFile(t, root, "orders.sql", "SELECT * FROM {{ ref('table-revenue') }}")
	editFile(t, root, "revenue.sql", "SELECT * FROM {{ ref('table-orders') }}")

	_, _, err := runPipelineCLI(t, root, "pipeline", "push")
	if err == nil {
		t.Fatal("a reference cycle must be refused")
	}
	if !strings.Contains(err.Error(), "loop") ||
		!strings.Contains(err.Error(), "orders.sql") || !strings.Contains(err.Error(), "revenue.sql") {
		t.Errorf("the refusal does not name the cycle: %v", err)
	}
	if len(f.syncs) != 0 {
		t.Errorf("a refused push still built %v", f.syncs)
	}
}

// TestPipelinePushRefusesAPositionalRefOnDisk: the one silently destructive
// path. A file on disk has no stored input list, so the server would resolve the
// marker against the SORTED list this push derives — and the SQL would start
// reading a different table with the build succeeding.
func TestPipelinePushRefusesAPositionalRefOnDisk(t *testing.T) {
	f := newFakePipelineInstance(t)
	signInPipeline(t, f)
	root := seedBoundFolder(t, f)
	editFile(t, root, "revenue.sql", "SELECT * FROM {{ ref('0') }}")

	_, _, err := runPipelineCLI(t, root, "pipeline", "push")
	if err == nil {
		t.Fatal("a positional ref on disk must refuse the push")
	}
	if !strings.Contains(err.Error(), "revenue.sql") || !strings.Contains(err.Error(), "ref('0')") {
		t.Errorf("the refusal names neither the file nor the ref: %v", err)
	}
	if len(f.updates) != 0 {
		t.Errorf("a refused push still wrote %+v", f.updates)
	}
}

// TestPipelinePushRefusesDriftOnTheLiveTable is drift-guard leg (a): a colleague
// committed while we were away, and pushing would silently revert them.
//
// There are no per-write compare-and-swap preconditions on this API, so this
// guard is the WHOLE protection.
func TestPipelinePushRefusesDriftOnTheLiveTable(t *testing.T) {
	f := newFakePipelineInstance(t)
	signInPipeline(t, f)
	root := seedBoundFolder(t, f)
	editFile(t, root, "orders.sql", "SELECT mine FROM raw")
	f.tables["table-orders"].Code = "SELECT theirs FROM raw"

	_, stderr, err := runPipelineCLI(t, root, "pipeline", "push")
	if err == nil {
		t.Fatal("drift on the live table must refuse the push")
	}
	if !strings.Contains(stderr, "table-orders") || !strings.Contains(stderr, "the live table") {
		t.Errorf("the refusal does not name what moved:\n%s", stderr)
	}
	if len(f.updates) != 0 {
		t.Errorf("a refused push still wrote %+v", f.updates)
	}
	if len(f.checkouts) != 0 {
		t.Errorf("a refused push still opened a draft: %v", f.checkouts)
	}
}

// TestPipelinePushRefusesDriftInYourOwnDraft is drift-guard leg (b), and it is
// not paranoia: the chat agent's editDerivedTable works in exactly this row, so
// without this leg a web or chat edit to your OWN draft vanishes silently under
// the PUT. The live table here is untouched, so leg (a) passes and only leg (b)
// can catch it.
func TestPipelinePushRefusesDriftInYourOwnDraft(t *testing.T) {
	f := newFakePipelineInstance(t)
	signInPipeline(t, f)
	root := seedBoundFolder(t, f)
	editFile(t, root, "orders.sql", "SELECT mine FROM raw")
	f.AddDraft("table-orders", "table-draft-9", "SELECT what the chat agent wrote")

	_, stderr, err := runPipelineCLI(t, root, "pipeline", "push")
	if err == nil {
		t.Fatal("an edited draft must refuse the push")
	}
	if !strings.Contains(stderr, "table-draft-9") {
		t.Errorf("the refusal does not name the draft:\n%s", stderr)
	}
	if len(f.updates) != 0 {
		t.Errorf("a refused push still wrote %+v", f.updates)
	}
}

// TestPipelinePushForceOverwritesDrift: --force means "overwrite the remote with
// what I have", and says what it is overwriting.
func TestPipelinePushForceOverwritesDrift(t *testing.T) {
	f := newFakePipelineInstance(t)
	signInPipeline(t, f)
	root := seedBoundFolder(t, f)
	editFile(t, root, "orders.sql", "SELECT mine FROM raw")
	f.tables["table-orders"].Code = "SELECT theirs FROM raw"

	out, stderr, err := runPipelineCLI(t, root, "pipeline", "push", "--force", "--json")
	if err != nil {
		t.Fatalf("push --force: %v\n%s", err, stderr)
	}
	if !strings.Contains(stderr, "--force") {
		t.Errorf("--force overwrote silently:\n%s", stderr)
	}
	files := decodeJSON(t, out)["files"].([]any)
	if len(files) != 1 || files[0].(map[string]any)["outcome"] != pushOutcomePushed {
		t.Errorf("files = %+v", files)
	}
	if len(f.updates) != 1 || f.updates[0].Code != "SELECT mine FROM raw" {
		t.Errorf("updates = %+v", f.updates)
	}
}

// TestPipelinePushRecordsANewTableBeforeAnythingElse: CreateTable returns a LIVE
// row — colleagues can see it the moment it exists — so a push that died before
// saving the manifest would leave a table nobody can find, and the next push
// would create a second one beside it.
//
// The failure is injected on the request that immediately follows the create,
// which is the narrowest window the binding has to survive.
func TestPipelinePushRecordsANewTableBeforeAnythingElse(t *testing.T) {
	f := newFakePipelineInstance(t)
	signInPipeline(t, f)
	root := seedBoundFolder(t, f)
	editFile(t, root, "margins.sql", "SELECT * FROM {{ ref('table-orders') }}")
	// The create mints table-new-1; the very next call is the draft read.
	f.failDraftGet["table-new-1"] = 500

	_, _, err := runPipelineCLI(t, root, "pipeline", "push")
	if err == nil {
		t.Fatal("a push whose draft read failed must exit non-zero")
	}
	if len(f.created) != 1 {
		t.Fatalf("want one create, got %+v", f.created)
	}
	binding := pipelineBindingOf(t, root, f.Key())
	if binding.Tables["margins.sql"] != "table-new-1" {
		t.Fatalf("the manifest does not hold the new table's binding: %+v", binding.Tables)
	}
	// And the created row carries what the file declared, so the row's lineage is
	// what the folder says rather than whatever a regex found.
	created := f.created[0]
	if created.Kind != api.TableKindDerived || created.Engine != api.TableEngineDuckDB {
		t.Errorf("created = %+v", created)
	}
	if len(created.InputModels) != 1 || created.InputModels[0] != "table-orders" {
		t.Errorf("inputModels = %+v", created.InputModels)
	}
	if created.Name != "margins" {
		t.Errorf("name = %q, want the file stem", created.Name)
	}
}

// TestPipelinePushAdoptsATableWhoseCreateTimedOut: a create whose request died
// on a deadline may well have committed — the deadline was ours, the transaction
// was the server's.
//
// This loop has no delete verb, so treating it as a plain failure is the one
// mistake it cannot undo: the table stays unbound, and the next push creates a
// SECOND live table with the same name beside the first.
func TestPipelinePushAdoptsATableWhoseCreateTimedOut(t *testing.T) {
	f := newFakePipelineInstance(t)
	signInPipeline(t, f)
	root := seedBoundFolder(t, f)
	editFile(t, root, "margins.sql", "SELECT * FROM {{ ref('table-orders') }}")
	landOnceThenTimeOut(t, "POST", "/api/v2/feature/model")

	out, stderr, err := runPipelineCLI(t, root, "pipeline", "push", "--json")
	if err != nil {
		t.Fatalf("push: %v\n%s", err, stderr)
	}
	if len(f.created) != 1 {
		t.Fatalf("the timed-out create was retried: %+v", f.created)
	}
	binding := pipelineBindingOf(t, root, f.Key())
	if binding.Tables["margins.sql"] != "table-new-1" {
		t.Fatalf("the table that landed was not adopted: %+v", binding.Tables)
	}
	file := decodeJSON(t, out)["files"].([]any)[0].(map[string]any)
	if file["outcome"] != pushOutcomePushed || file["created"] != true {
		t.Errorf("file = %+v\n%s", file, stderr)
	}
	// And the baseline describes the row that was READ BACK, not the file that
	// hoped to be it: a second push of the same bytes is an ordinary no-op push,
	// where a fingerprint taken on faith would show as drift on the very next run.
	if _, _, err := runPipelineCLI(t, root, "pipeline", "push"); err != nil {
		t.Fatalf("the adopted table's baseline does not describe the server's row: %v", err)
	}
	if len(f.created) != 1 {
		t.Errorf("the second push created another table: %+v", f.created)
	}
}

// TestPipelinePushWaitsOutABuildThatIsAlreadyRunning: the state an interrupted
// push leaves behind — Ctrl-C stops the CLI listening, never the build — and the
// server refuses a sync while one is in flight.
//
// Treating that refusal as a plain failure breaks the resume the interrupt
// handling exists for: the re-run writes its SQL and then dies on the very next
// step, every time, until the build happens to be over.
func TestPipelinePushWaitsOutABuildThatIsAlreadyRunning(t *testing.T) {
	f := newFakePipelineInstance(t)
	signInPipeline(t, f)
	root := seedBoundFolder(t, f)
	editFile(t, root, "orders.sql", "SELECT resumed FROM raw")
	f.syncBusy["table-orders"] = 1

	out, stderr, err := runPipelineCLI(t, root, "pipeline", "push", "orders.sql", "--json")
	if err != nil {
		t.Fatalf("push: %v\n%s", err, stderr)
	}
	file := decodeJSON(t, out)["files"].([]any)[0].(map[string]any)
	if file["outcome"] != pushOutcomePushed {
		t.Fatalf("file = %+v\n%s", file, stderr)
	}
	if !strings.Contains(stderr, "already running") {
		t.Errorf("the wait was silent:\n%s", stderr)
	}
	// And the sync is RE-ISSUED rather than joined: the build that was running
	// started before this push wrote its bytes, so its data describes the
	// previous SQL.
	draftID := file["draftID"].(string)
	if n := countSyncs(f, draftID); n != 1 {
		t.Errorf("the draft was synced %d times after the refusal, want 1", n)
	}
	if f.CodeOf(draftID) != "SELECT resumed FROM raw" {
		t.Errorf("the draft does not hold this push's SQL: %q", f.CodeOf(draftID))
	}
}

// TestPipelinePushRefusesASyncItCannotJoin: a 400 from /sync is not always a
// build in flight — a committed version snapshot earns the same status — and
// waiting on one would sit out a deadline for nothing.
func TestPipelinePushRefusesASyncItCannotJoin(t *testing.T) {
	f := newFakePipelineInstance(t)
	signInPipeline(t, f)
	root := seedBoundFolder(t, f)
	editFile(t, root, "orders.sql", "SELECT resumed FROM raw")
	f.failSync["table-draft-1"] = 400

	out, _, err := runPipelineCLI(t, root, "pipeline", "push", "orders.sql", "--json")
	if err == nil {
		t.Fatal("a refused sync of a row that is not building must exit non-zero")
	}
	file := decodeJSON(t, out)["files"].([]any)[0].(map[string]any)
	if file["outcome"] != pushOutcomeRefused {
		t.Fatalf("file = %+v", file)
	}
}

// TestPipelinePushDoesNotAdoptASameNamedTableItDidNotCreate: a table name is not
// unique within a feature, so a create that timed out AND failed can still find
// exactly one row carrying the name it sent — a table that has been there for a
// year, or a colleague's.
//
// Adopting it binds this file to somebody else's row and records THIS file's
// hash as the server's state, so both drift legs read clean and the next publish
// overwrites their SQL without a word.
func TestPipelinePushDoesNotAdoptASameNamedTableItDidNotCreate(t *testing.T) {
	f := newFakePipelineInstance(t)
	signInPipeline(t, f)
	root := seedBoundFolder(t, f)
	f.AddTable(&api.Table{ID: "table-theirs", Name: "margins", FeatureID: "collection-1",
		Code: "SELECT theirs FROM raw"})
	editFile(t, root, "margins.sql", "SELECT * FROM {{ ref('table-orders') }}")
	failOnceWithATimeout(t, "POST", "/api/v2/feature/model")

	out, _, err := runPipelineCLI(t, root, "pipeline", "push", "--json")
	if err == nil {
		t.Fatal("adopting a same-named table this push did not create must be refused")
	}
	file := decodeJSON(t, out)["files"].([]any)[0].(map[string]any)
	if file["outcome"] != pushOutcomeRefused {
		t.Fatalf("file = %+v", file)
	}
	if !strings.Contains(file["error"].(string), "holds SQL this push did not send") {
		t.Errorf("the refusal does not say why the one match was rejected: %v", file["error"])
	}
	binding := pipelineBindingOf(t, root, f.Key())
	if id, bound := binding.Tables["margins.sql"]; bound {
		t.Errorf("the file was bound to %s, which this push did not create", id)
	}
	if f.CodeOf("table-theirs") != "SELECT theirs FROM raw" {
		t.Errorf("somebody else's SQL was overwritten: %q", f.CodeOf("table-theirs"))
	}
}

// TestPipelinePushDoesNotAdoptATableAnotherFileIsBoundTo: two files in one
// folder can carry the same stem — `eu/margins.sql` and `us/margins.sql` — and
// so send the same table name. A reconcile that matched on the name alone would
// point the second file at the first one's table, and every later push would be
// the two of them overwriting each other.
func TestPipelinePushDoesNotAdoptATableAnotherFileIsBoundTo(t *testing.T) {
	f := newFakePipelineInstance(t)
	signInPipeline(t, f)
	root := seedBoundFolder(t, f)
	f.AddTable(&api.Table{ID: "table-eu-margins", Name: "margins", FeatureID: "collection-1",
		Code: "SELECT * FROM {{ ref('table-orders') }}"})
	bindPipelineTable(t, root, f.Key(), "eu/margins.sql", "table-eu-margins")
	editFile(t, root, "eu/margins.sql", "SELECT * FROM {{ ref('table-orders') }}")
	editFile(t, root, "us/margins.sql", "SELECT * FROM {{ ref('table-orders') }}")
	failOnceWithATimeout(t, "POST", "/api/v2/feature/model")

	out, _, err := runPipelineCLI(t, root, "pipeline", "push", "us/margins.sql", "--json")
	if err == nil {
		t.Fatal("adopting a table another file is bound to must be refused")
	}
	file := decodeJSON(t, out)["files"].([]any)[0].(map[string]any)
	if file["outcome"] != pushOutcomeRefused {
		t.Fatalf("file = %+v", file)
	}
	binding := pipelineBindingOf(t, root, f.Key())
	if binding.Tables["us/margins.sql"] == "table-eu-margins" {
		t.Fatalf("two files were bound to one table: %+v", binding.Tables)
	}
}

// TestPipelinePushRefusesWhenATimedOutCreateCannotBeReconciled: the create
// timed out and nothing landed, so there is nothing to adopt — and the refusal
// has to say what to check, because the author cannot tell the two apart from
// the error alone.
func TestPipelinePushRefusesWhenATimedOutCreateCannotBeReconciled(t *testing.T) {
	f := newFakePipelineInstance(t)
	signInPipeline(t, f)
	root := seedBoundFolder(t, f)
	editFile(t, root, "margins.sql", "SELECT * FROM {{ ref('table-orders') }}")
	failOnceWithATimeout(t, "POST", "/api/v2/feature/model")

	out, _, err := runPipelineCLI(t, root, "pipeline", "push", "--json")
	if err == nil {
		t.Fatal("a create that timed out and left nothing behind must exit non-zero")
	}
	if len(f.created) != 0 {
		t.Fatalf("the request never reached the fake, so nothing was created: %+v", f.created)
	}
	file := decodeJSON(t, out)["files"].([]any)[0].(map[string]any)
	if file["outcome"] != pushOutcomeRefused {
		t.Fatalf("file = %+v", file)
	}
	if !strings.Contains(file["error"].(string), "timed out") {
		t.Errorf("the refusal does not name the timeout: %v", file["error"])
	}
	// Nothing bound, so a retry creates the table rather than adopting a guess.
	binding := pipelineBindingOf(t, root, f.Key())
	if _, bound := binding.Tables["margins.sql"]; bound {
		t.Errorf("a create that did not land was bound anyway: %+v", binding.Tables)
	}
}

// TestPipelinePushRefusesANewFileWithNoFeature: creating a table needs somewhere
// to put it, and refusing before the first request costs nothing.
func TestPipelinePushRefusesANewFileWithNoFeature(t *testing.T) {
	f := newFakePipelineInstance(t)
	signInPipeline(t, f)
	root := t.TempDir()
	manifest := &wfdir.Manifest{Kind: wfdir.KindPipeline, Title: "Sales"}
	manifest.SetBinding(f.Key(), wfdir.Binding{})
	writePipelineFolder(t, root, manifest, map[string]string{"orders.sql": "SELECT 1"})

	_, _, err := runPipelineCLI(t, root, "pipeline", "push")
	if err == nil {
		t.Fatal("a new file with no feature must be refused")
	}
	if !strings.Contains(err.Error(), "featureID") {
		t.Errorf("refusal = %v", err)
	}
	if len(f.created) != 0 {
		t.Errorf("a refused push still created %+v", f.created)
	}
}

// TestPipelinePushKeepsGoingAfterAFailedBuild: a failed build is not the end of
// the push — the draft holds the work, the live table is untouched, the
// remaining files are still attempted, and the command exits non-zero.
func TestPipelinePushKeepsGoingAfterAFailedBuild(t *testing.T) {
	f := newFakePipelineInstance(t)
	signInPipeline(t, f)
	root := seedBoundFolder(t, f)
	editFile(t, root, "orders.sql", "SELECT broken FROM raw")
	editFile(t, root, "revenue.sql", "SELECT 3 FROM {{ ref('table-orders') }}")
	f.syncOutcome["table-orders"] = "build_failed"
	f.syncMessage["table-orders"] = "The transformation is impossible: raw has no column `broken`."

	out, _, err := runPipelineCLI(t, root, "pipeline", "push", "--json")
	if err == nil {
		t.Fatal("a failed build must exit non-zero")
	}
	payload := decodeJSON(t, out)
	files := payload["files"].([]any)
	if len(files) != 2 {
		t.Fatalf("the push stopped early: %+v", files)
	}
	first := files[0].(map[string]any)
	if first["path"] != "orders.sql" || first["outcome"] != pushOutcomeBuildFailed {
		t.Errorf("first = %+v", first)
	}
	// The reason is passed through verbatim: for a derived table it is the SQL
	// writer's own explanation, and it is the most useful thing a failed push
	// prints.
	if !strings.Contains(first["error"].(string), "no column `broken`") {
		t.Errorf("error = %v", first["error"])
	}
	// The draft survives, so the author can fix the SQL and push again.
	if first["draftID"] == "" {
		t.Error("the failed draft was not reported")
	}
	if second := files[1].(map[string]any); second["outcome"] != pushOutcomePushed {
		t.Errorf("the second file was not attempted: %+v", second)
	}

	// The failed file's CONTENT baseline must NOT advance — the server does not
	// hold a working build of those bytes — but its draft pointer must, or the
	// retry cannot find the draft that holds the work.
	inst := pipelineStateOf(t, root, f.Key())
	if inst.Files["orders.sql"].SHA256 == wfdir.HashString("SELECT broken FROM raw") {
		t.Error("a failed build advanced the content baseline")
	}
	if inst.Tables["orders.sql"].DraftID == "" {
		t.Error("the failed draft was not recorded in the baseline")
	}
	// The live table is untouched: a draft build never changes live data.
	if f.CodeOf("table-orders") != "SELECT * FROM raw" {
		t.Errorf("the live table changed: %q", f.CodeOf("table-orders"))
	}
}

// TestPipelinePushReportsFailedStale: the trap buildVerdict exists for. A live
// table whose rebuild FAILED stays `ready` and goes on serving its previous
// data, so `status` alone reports a failure as a success.
func TestPipelinePushReportsFailedStale(t *testing.T) {
	f := newFakePipelineInstance(t)
	signInPipeline(t, f)
	root := seedBoundFolder(t, f)
	editFile(t, root, "orders.sql", "SELECT broken FROM raw")
	f.syncOutcome["table-orders"] = "failed_stale"

	out, _, err := runPipelineCLI(t, root, "pipeline", "push", "--json")
	if err == nil {
		t.Fatal("a ready-with-an-error build must exit non-zero")
	}
	file := decodeJSON(t, out)["files"].([]any)[0].(map[string]any)
	if file["verdict"] != api.BuildVerdictFailedStale {
		t.Errorf("verdict = %v, want %s", file["verdict"], api.BuildVerdictFailedStale)
	}
	if file["outcome"] != pushOutcomeBuildFailed {
		t.Errorf("outcome = %v", file["outcome"])
	}
}

// TestPipelinePushRendersTheConfidenceReport: the point of building a draft is
// that you can see what landing it would do before anything ships.
func TestPipelinePushRendersTheConfidenceReport(t *testing.T) {
	f := newFakePipelineInstance(t)
	signInPipeline(t, f)
	root := seedBoundFolder(t, f)
	editFile(t, root, "orders.sql", "SELECT 4 FROM raw")
	f.fields["table-orders"] = []string{"id", "total"}
	f.rowCounts["table-orders"] = 100
	// The draft the push will check out.
	f.fields["table-draft-1"] = []string{"id", "total", "margin"}
	f.rowCounts["table-draft-1"] = 120

	out, stderr, err := runPipelineCLI(t, root, "pipeline", "push", "--json")
	if err != nil {
		t.Fatalf("push: %v\n%s", err, stderr)
	}
	file := decodeJSON(t, out)["files"].([]any)[0].(map[string]any)
	review := file["review"].(map[string]any)
	added := review["fieldsAdded"].([]any)
	if len(added) != 1 || added[0] != "margin" {
		t.Errorf("fieldsAdded = %+v", added)
	}
	if review["draftRowCount"] != float64(120) || review["liveRowCount"] != float64(100) {
		t.Errorf("row counts = %+v", review)
	}
	// The sample is rendered by the human report alone, so --json pays for none
	// of it: a duckdb read is an LLM routing call and, on a big table, a Batch
	// job, and this run would have thrown the answer away.
	if len(f.queries) != 0 {
		t.Errorf("a --json push read samples nobody prints: %+v", f.queries)
	}
}

// TestPipelinePushReadsSampleRowsForTheHumanReport: the other half of the same
// rule — the report that DOES print a sample still reads one, once per pushed
// file, and only after a successful sync (a draft that was checked out and never
// synced has no partitions of its own, which the fake asserts independently).
func TestPipelinePushReadsSampleRowsForTheHumanReport(t *testing.T) {
	f := newFakePipelineInstance(t)
	signInPipeline(t, f)
	root := seedBoundFolder(t, f)
	editFile(t, root, "orders.sql", "SELECT 4 FROM raw")

	if _, stderr, err := runPipelineCLI(t, root, "pipeline", "push"); err != nil {
		t.Fatalf("push: %v\n%s", err, stderr)
	}
	if len(f.queries) != 1 {
		t.Fatalf("queries = %+v", f.queries)
	}
	if !strings.Contains(f.queries[0], "LIMIT 5") || !strings.Contains(f.queries[0], "table-draft-1") {
		t.Errorf("sample query = %q", f.queries[0])
	}
}

// TestPipelinePushReportsALineageChange: repointing what a table READS is the
// one change a commit can make with nothing in the SQL diff to look at — a
// positional ref resolves by index into the declared input list — so the
// confidence report names it rather than leaving the reader to compare two lists
// nobody printed.
func TestPipelinePushReportsALineageChange(t *testing.T) {
	f := newFakePipelineInstance(t)
	signInPipeline(t, f)
	root := seedBoundFolder(t, f)
	// The live row declares one upstream; the edit swaps it for another. Both are
	// outside the folder, so this is a lineage change and not a reordering of the
	// build.
	f.tables["table-orders"].InputModels = []string{"table-legacy"}
	editFile(t, root, "orders.sql", "SELECT * FROM {{ ref('table-raw') }}")

	out, stderr, err := runPipelineCLI(t, root, "pipeline", "push", "--json")
	if err != nil {
		t.Fatalf("push: %v\n%s", err, stderr)
	}
	review := decodeJSON(t, out)["files"].([]any)[0].(map[string]any)["review"].(map[string]any)
	added := review["inputsAdded"].([]any)
	if len(added) != 1 || added[0] != "table-raw" {
		t.Errorf("inputsAdded = %+v", added)
	}
	removed := review["inputsRemoved"].([]any)
	if len(removed) != 1 || removed[0] != "table-legacy" {
		t.Errorf("inputsRemoved = %+v", removed)
	}

	// And the human report prints it as one signed line: a swap is one fact, and
	// splitting it across two labels reads as two.
	out, stderr, err = runPipelineCLI(t, root, "pipeline", "push", "orders.sql")
	if err != nil {
		t.Fatalf("push: %v\n%s", err, stderr)
	}
	if !strings.Contains(out, "Inputs: +table-raw, -table-legacy") {
		t.Errorf("the lineage change is not in the report:\n%s", out)
	}
}

// TestPipelinePushSurvivesAMissingConfidenceReport: the review route lives in
// the `admin` scope group while the rest of the loop is `data`, so a token
// scoped to `data` alone runs everything except this. The build succeeded and
// the work is safe; the report is how you decide whether to publish it, not part
// of getting there.
func TestPipelinePushSurvivesAMissingConfidenceReport(t *testing.T) {
	f := newFakePipelineInstance(t)
	signInPipeline(t, f)
	root := seedBoundFolder(t, f)
	editFile(t, root, "orders.sql", "SELECT 5 FROM raw")
	f.failReview["table-draft-1"] = 403

	out, stderr, err := runPipelineCLI(t, root, "pipeline", "push", "--json")
	if err != nil {
		t.Fatalf("a missing confidence report must not fail the push: %v\n%s", err, stderr)
	}
	file := decodeJSON(t, out)["files"].([]any)[0].(map[string]any)
	if file["outcome"] != pushOutcomePushed {
		t.Errorf("outcome = %v", file["outcome"])
	}
	if file["review"] != nil {
		t.Errorf("review = %+v", file["review"])
	}
	if !strings.Contains(stderr, "admin:read") {
		t.Errorf("the scope quirk was not named:\n%s", stderr)
	}
	assertPipelineBaselineMatchesDisk(t, root, f.Key())
}

// TestPipelinePushSurvivesAFailedSampleRead: a FAILED query is HTTP 200 with the
// failure in a field, which nothing in the transport layer flags. Samples are
// garnish; treating one as a gate would fail a push that has already landed.
func TestPipelinePushSurvivesAFailedSampleRead(t *testing.T) {
	f := newFakePipelineInstance(t)
	signInPipeline(t, f)
	root := seedBoundFolder(t, f)
	editFile(t, root, "orders.sql", "SELECT 6 FROM raw")
	f.queryError = "no data available for this table"

	// The human report, because it is the only one that asks for a sample at all.
	out, stderr, err := runPipelineCLI(t, root, "pipeline", "push")
	if err != nil {
		t.Fatalf("a failed sample read must not fail the push: %v\n%s", err, stderr)
	}
	if !strings.Contains(out, "orders.sql") {
		t.Errorf("report = %s", out)
	}
	if !strings.Contains(stderr, "no sample rows") {
		t.Errorf("the degradation was not reported:\n%s", stderr)
	}
}

// TestPipelinePushResumesAnExistingDraft: CheckoutTable is NOT idempotent — a
// second checkout while a draft is open is an error — which is the whole reason
// push reads GET :id/draft first.
func TestPipelinePushResumesAnExistingDraft(t *testing.T) {
	f := newFakePipelineInstance(t)
	signInPipeline(t, f)
	root := seedBoundFolder(t, f)
	// A draft holding exactly what the baseline recorded: the author's own,
	// unmodified, from an earlier push. Leg (b) has nothing to say about it.
	f.AddDraft("table-orders", "table-draft-4", "SELECT * FROM raw")
	editFile(t, root, "orders.sql", "SELECT 7 FROM raw")

	out, stderr, err := runPipelineCLI(t, root, "pipeline", "push", "--json")
	if err != nil {
		t.Fatalf("push: %v\n%s", err, stderr)
	}
	if len(f.checkouts) != 0 {
		t.Errorf("push checked out a second draft: %v", f.checkouts)
	}
	if len(f.updates) != 1 || f.updates[0].ID != "table-draft-4" {
		t.Errorf("updates = %+v", f.updates)
	}
	if decodeJSON(t, out)["files"].([]any)[0].(map[string]any)["draftID"] != "table-draft-4" {
		t.Errorf("payload = %s", out)
	}
}

// TestPipelinePushNamedPathsOnly: naming a path pushes it whether or not it
// looks changed, which is how somebody recovers from a draft they edited
// elsewhere — and it must not drag the rest of the folder along.
func TestPipelinePushNamedPathsOnly(t *testing.T) {
	f := newFakePipelineInstance(t)
	signInPipeline(t, f)
	root := seedBoundFolder(t, f)
	editFile(t, root, "orders.sql", "SELECT 8 FROM raw")
	editFile(t, root, "revenue.sql", "SELECT 8 FROM {{ ref('table-orders') }}")

	out, stderr, err := runPipelineCLI(t, root, "pipeline", "push", "revenue.sql", "--json")
	if err != nil {
		t.Fatalf("push: %v\n%s", err, stderr)
	}
	files := decodeJSON(t, out)["files"].([]any)
	if len(files) != 1 || files[0].(map[string]any)["path"] != "revenue.sql" {
		t.Fatalf("files = %+v", files)
	}
	if len(f.updates) != 1 || f.updates[0].Code != "SELECT 8 FROM {{ ref('table-orders') }}" {
		t.Errorf("updates = %+v", f.updates)
	}
	// orders.sql is still dirty afterwards, which is what "only this one" means.
	inst := pipelineStateOf(t, root, f.Key())
	if inst.Files["orders.sql"].SHA256 == wfdir.HashString("SELECT 8 FROM raw") {
		t.Error("a narrowed push advanced another file's baseline")
	}
}

// TestPipelinePushRefusesANonSQLPath: the pushable set is .sql and nothing else,
// so naming a README has to say why rather than silently do nothing.
func TestPipelinePushRefusesANonSQLPath(t *testing.T) {
	f := newFakePipelineInstance(t)
	signInPipeline(t, f)
	root := seedBoundFolder(t, f)

	_, _, err := runPipelineCLI(t, root, "pipeline", "push", "README.md")
	if err == nil {
		t.Fatal("a non-SQL path must be refused")
	}
	if !strings.Contains(err.Error(), ".sql") {
		t.Errorf("refusal = %v", err)
	}
}

// TestPipelinePushLeavesADeletedFilesTableAlone: there is no delete verb here on
// purpose, and quietly dropping a colleague's table because somebody deleted a
// file in a merge is exactly what a sync loop must not do.
func TestPipelinePushLeavesADeletedFilesTableAlone(t *testing.T) {
	f := newFakePipelineInstance(t)
	signInPipeline(t, f)
	root := seedBoundFolder(t, f)
	if err := removeFile(root, "revenue.sql"); err != nil {
		t.Fatal(err)
	}

	_, stderr, err := runPipelineCLI(t, root, "pipeline", "push")
	if err != nil {
		t.Fatalf("push: %v\n%s", err, stderr)
	}
	if !strings.Contains(stderr, "revenue.sql is gone from this folder") {
		t.Errorf("the deletion was not reported:\n%s", stderr)
	}
	if f.tables["table-revenue"] == nil {
		t.Fatal("push deleted the table behind a removed file")
	}
}

// removeFile deletes one file from a folder.
func removeFile(root, path string) error {
	return os.Remove(filepath.Join(root, filepath.FromSlash(path)))
}

// --- the baseline is per ROW ------------------------------------------------
//
// Four SEQUENCES, each running two or more mutating commands, because a
// single-command test cannot see this class of bug at all: every one of them
// passed with one shared fingerprint, and every one of them refused the second
// command as drift on a row nobody had touched.
//
// What they pin down is wfdir.TableState's invariant — a hash is only ever
// compared against the row it was taken from. Leg (a) of the drift guard reads
// the LIVE table, so it may only be compared against a fingerprint taken from
// the live table; leg (b) reads YOUR DRAFT, so only against one taken from that
// draft. A single hash cannot be both, and the four ordinary workflows below are
// exactly where the difference shows.

// TestPipelinePushSendsTheWireFingerprintAsThePrecondition is the same
// invariant one layer out, and the reason TableState carries a THIRD hash.
//
// The server's baseCodeSha256 is compared against what the ROW stores, and what
// the row stores is the WIRE form — the file with its sibling stems and declared
// aliases substituted for ids. DraftSHA256 is the DISK form, so sending it would
// 409 every push from any folder that spells a ref by name, which is the
// ordinary pipeline folder. The two hashes here are deliberately DIFFERENT
// values: a test whose file happened to be pure id-ref SQL would pass under
// either implementation and would be pinning nothing.
func TestPipelinePushSendsTheWireFingerprintAsThePrecondition(t *testing.T) {
	// A SIBLING STEM, not an id. On disk this is `orders`; on the wire it is
	// `table-orders`, and the fake stores what it was sent. The folder is seeded
	// in stem form from the start, because switching a file from id form to stem
	// form is itself a change leg (a) reports — a different property, with its
	// own tests.
	const disk = "SELECT * FROM {{ ref('orders') }}"
	const wire = "SELECT * FROM {{ ref('table-orders') }}"

	f := newFakePipelineInstance(t)
	signInPipeline(t, f)
	seedFeature(f, "private")
	root := t.TempDir()
	files := map[string]string{"orders.sql": "SELECT * FROM raw", "revenue.sql": disk}
	manifest := &wfdir.Manifest{Kind: wfdir.KindPipeline, Title: "Sales pipeline"}
	manifest.SetBinding(f.Key(), wfdir.Binding{
		FeatureID: "collection-1",
		Tables:    map[string]string{"orders.sql": "table-orders", "revenue.sql": "table-revenue"},
	})
	writePipelineFolder(t, root, manifest, files)
	writePipelineBaseline(t, root, f.Key(), "collection-1", files,
		map[string]wfdir.TableState{
			"orders.sql":  {TableID: "table-orders"},
			"revenue.sql": {TableID: "table-revenue"},
		})

	editFile(t, root, "revenue.sql", disk+" WHERE ok")
	if _, stderr, err := runPipelineCLI(t, root, "pipeline", "push", "revenue.sql"); err != nil {
		t.Fatalf("first push: %v\n%s", err, stderr)
	}
	state := pipelineStateOf(t, root, f.Key()).Tables["revenue.sql"]
	if state.DraftSHA256 != wfdir.HashString(disk+" WHERE ok") {
		t.Errorf("disk fingerprint = %q, want the bytes on disk", state.DraftSHA256)
	}
	if state.DraftWireSHA256 != wfdir.HashString(wire+" WHERE ok") {
		t.Errorf("wire fingerprint = %q, want the bytes that went over the PUT", state.DraftWireSHA256)
	}
	if state.DraftWireSHA256 == state.DraftSHA256 {
		t.Fatal("the two fingerprints are equal, so this test cannot tell the implementations apart")
	}

	// The second push is where a disk-form precondition dies: the fake compares
	// the digest against what it STORES, exactly as the server does, so a client
	// that sent DraftSHA256 here is refused with a 409 on a draft only it has
	// ever written to.
	editFile(t, root, "revenue.sql", disk+" WHERE ok AND more")
	out, stderr, err := runPipelineCLI(t, root, "pipeline", "push", "revenue.sql", "--json")
	if err != nil {
		t.Fatalf("a second push of a folder that names a sibling by its stem must not 409: %v\n%s", err, stderr)
	}
	if file := decodeJSON(t, out)["files"].([]any)[0].(map[string]any); file["outcome"] != pushOutcomePushed {
		t.Fatalf("file = %+v\n%s", file, stderr)
	}
	// And it really did assert something — an implementation that quietly sent no
	// precondition at all would also pass everything above.
	last := f.updates[len(f.updates)-1]
	if last.BaseCodeSha256 == nil {
		t.Fatal("the second push sent no precondition, so nothing was asserted")
	}
	if *last.BaseCodeSha256 != wfdir.HashString(wire+" WHERE ok") {
		t.Errorf("precondition = %q, want the wire fingerprint of the previous push", *last.BaseCodeSha256)
	}
}

// TestPipelinePushForceDropsThePrecondition: --force is the author saying they
// have seen what is on the server and mean to write over it. Sending the
// precondition anyway would make the flag refuse the one thing it exists to
// permit — and the drift it is being used to overrule is very often exactly the
// draft edit the precondition would catch.
func TestPipelinePushForceDropsThePrecondition(t *testing.T) {
	f := newFakePipelineInstance(t)
	signInPipeline(t, f)
	root := seedBoundFolder(t, f)

	editFile(t, root, "orders.sql", "SELECT 1 FROM raw")
	if _, stderr, err := runPipelineCLI(t, root, "pipeline", "push", "orders.sql"); err != nil {
		t.Fatalf("first push: %v\n%s", err, stderr)
	}
	// Somebody edits the draft from the web app. Without --force this is refused
	// by leg (b) of the client-side guard before the write is even attempted.
	f.tables[f.draftOf["table-orders"]].Code = "SELECT theirs FROM raw"
	editFile(t, root, "orders.sql", "SELECT 2 FROM raw")
	if _, _, err := runPipelineCLI(t, root, "pipeline", "push", "orders.sql"); err == nil {
		t.Fatal("a draft edited on the server must be refused without --force")
	}
	if _, stderr, err := runPipelineCLI(t, root, "pipeline", "push", "orders.sql", "--force"); err != nil {
		t.Fatalf("--force must be able to overwrite: %v\n%s", err, stderr)
	}
	if last := f.updates[len(f.updates)-1]; last.BaseCodeSha256 != nil {
		t.Errorf("--force sent a precondition (%q), which the server would refuse", *last.BaseCodeSha256)
	}
}

// TestPipelinePushRefusesADraftEditedBetweenTheReadAndTheWrite: the gap layer 1
// exists to close. The client-side guard compares the draft at one instant; the
// write happens at another, and an edit landing in between used to win silently.
func TestPipelinePushRefusesADraftEditedBetweenTheReadAndTheWrite(t *testing.T) {
	f := newFakePipelineInstance(t)
	signInPipeline(t, f)
	root := seedBoundFolder(t, f)

	editFile(t, root, "orders.sql", "SELECT 1 FROM raw")
	if _, stderr, err := runPipelineCLI(t, root, "pipeline", "push", "orders.sql"); err != nil {
		t.Fatalf("first push: %v\n%s", err, stderr)
	}
	// The recorded fingerprint still describes the previous write, so leg (b)
	// waves this through — the state a folder is in when the chat agent edits the
	// draft while the push is in flight. Only the server's precondition can see it.
	draftID := f.draftOf["table-orders"]
	f.stateAfterRead = func() { f.tables[draftID].Code = "SELECT theirs FROM raw" }
	editFile(t, root, "orders.sql", "SELECT 2 FROM raw")

	_, stderr, err := runPipelineCLI(t, root, "pipeline", "push", "orders.sql")
	if err == nil {
		t.Fatalf("the write landed over an edit nobody had seen:\n%s", stderr)
	}
	if !strings.Contains(stderr, "nothing was written") {
		t.Errorf("the refusal did not say the draft was left alone:\n%s", stderr)
	}
	if !strings.Contains(stderr, "--force") {
		t.Errorf("the refusal did not say what to do next:\n%s", stderr)
	}
	if f.CodeOf(draftID) != "SELECT theirs FROM raw" {
		t.Errorf("the draft was overwritten anyway: %q", f.CodeOf(draftID))
	}
}

// TestPipelinePushEditThenPushAgain: the commonest sequence there is, and the
// one a shared fingerprint refused outright — after the first push the baseline
// held the bytes written into the DRAFT, and leg (a) compared them against the
// live table, which had never moved and never held them.
func TestPipelinePushEditThenPushAgain(t *testing.T) {
	f := newFakePipelineInstance(t)
	signInPipeline(t, f)
	root := seedBoundFolder(t, f)

	editFile(t, root, "orders.sql", "SELECT 1 FROM raw")
	if _, stderr, err := runPipelineCLI(t, root, "pipeline", "push"); err != nil {
		t.Fatalf("first push: %v\n%s", err, stderr)
	}
	editFile(t, root, "orders.sql", "SELECT 2 FROM raw")

	out, stderr, err := runPipelineCLI(t, root, "pipeline", "push", "--json")
	if err != nil {
		t.Fatalf("a second push after an edit must not be refused as drift: %v\n%s", err, stderr)
	}
	file := decodeJSON(t, out)["files"].([]any)[0].(map[string]any)
	if file["outcome"] != pushOutcomePushed {
		t.Fatalf("file = %+v\n%s", file, stderr)
	}
	// Same draft, resumed, and holding the newest bytes.
	if len(f.checkouts) != 1 {
		t.Errorf("checkouts = %v, want the first push's only", f.checkouts)
	}
	if f.CodeOf(file["draftID"].(string)) != "SELECT 2 FROM raw" {
		t.Errorf("the draft holds %q", f.CodeOf(file["draftID"].(string)))
	}
	// And the two fingerprints are of the two different rows, which is the whole
	// point: the draft's is what we wrote, the live table's is what it still has.
	state := pipelineStateOf(t, root, f.Key()).Tables["orders.sql"]
	if state.DraftSHA256 != wfdir.HashString("SELECT 2 FROM raw") {
		t.Errorf("draft fingerprint = %q, want the bytes just written", state.DraftSHA256)
	}
	if state.LiveSHA256 != wfdir.HashString("SELECT * FROM raw") {
		t.Errorf("live fingerprint = %q, want the untouched live SQL", state.LiveSHA256)
	}
}

// TestPipelinePushFixesAFailedBuildAndPushesAgain: the second sequence. A failed
// build leaves the draft holding the bytes of the failed attempt, so leg (b) has
// to be measured against THOSE — not against the last successful sync, which is
// what the content baseline records and which the draft has moved past.
//
// The fix must not need --force. Being told "the SQL changed on the server" when
// what changed is the SQL you yourself pushed sixty seconds ago is the kind of
// refusal that teaches people to pass --force by reflex.
func TestPipelinePushFixesAFailedBuildAndPushesAgain(t *testing.T) {
	f := newFakePipelineInstance(t)
	signInPipeline(t, f)
	root := seedBoundFolder(t, f)

	editFile(t, root, "orders.sql", "SELECT broken FROM raw")
	f.syncOutcome["table-orders"] = "build_failed"
	if _, _, err := runPipelineCLI(t, root, "pipeline", "push"); err == nil {
		t.Fatal("a failed build must exit non-zero")
	}
	// The content baseline stayed put — the server holds no working build of
	// those bytes — so the fixed file is still a target.
	inst := pipelineStateOf(t, root, f.Key())
	if inst.Files["orders.sql"].SHA256 == wfdir.HashString("SELECT broken FROM raw") {
		t.Fatal("a failed build advanced the content baseline")
	}
	if inst.Tables["orders.sql"].DraftSHA256 != wfdir.HashString("SELECT broken FROM raw") {
		t.Fatalf("the draft fingerprint must record what the draft HOLDS: %+v", inst.Tables["orders.sql"])
	}

	delete(f.syncOutcome, "table-orders")
	editFile(t, root, "orders.sql", "SELECT fixed FROM raw")
	out, stderr, err := runPipelineCLI(t, root, "pipeline", "push", "--json")
	if err != nil {
		t.Fatalf("the fix must push without --force: %v\n%s", err, stderr)
	}
	file := decodeJSON(t, out)["files"].([]any)[0].(map[string]any)
	if file["outcome"] != pushOutcomePushed {
		t.Errorf("file = %+v\n%s", file, stderr)
	}
	if strings.Contains(stderr, "changed on the server") {
		t.Errorf("the fix was reported as somebody else's drift:\n%s", stderr)
	}
}

// TestPipelineCloneFromDraftThenPush: the third sequence, and the clearest case
// that one fingerprint cannot describe two rows. A clone prefers your open draft
// — so what is on disk is the DRAFT's SQL while the live table holds something
// else — and the very first push then compared the live table against the
// draft's bytes and refused a folder that had done nothing wrong.
func TestPipelineCloneFromDraftThenPush(t *testing.T) {
	f := newFakePipelineInstance(t)
	signInPipeline(t, f)
	f.AddFeature("collection-1", "Sales", "private")
	f.AddTable(&api.Table{ID: "table-orders", Name: "Orders", FeatureID: "collection-1",
		Code: "SELECT live FROM raw"})
	f.AddDraft("table-orders", "table-draft-8", "SELECT drafted FROM raw")

	dir := t.TempDir()
	if _, stderr, err := runPipelineCLI(t, dir, "pipeline", "clone", "collection-1", "out"); err != nil {
		t.Fatalf("clone: %v\n%s", err, stderr)
	}
	root := filepath.Join(dir, "out")
	// The stem is the table's name verbatim, case included — see FileSlug.
	if got := readFile(t, root, "Orders.sql"); got != "SELECT drafted FROM raw" {
		t.Fatalf("the clone did not prefer the draft: %q", got)
	}
	// Two rows, two fingerprints, and they differ — which is precisely the state
	// a single hash could not represent.
	state := pipelineStateOf(t, root, f.Key()).Tables["Orders.sql"]
	if state.DraftSHA256 != wfdir.HashString("SELECT drafted FROM raw") {
		t.Errorf("draft fingerprint = %q", state.DraftSHA256)
	}
	if state.LiveSHA256 != wfdir.HashString("SELECT live FROM raw") {
		t.Errorf("live fingerprint = %q", state.LiveSHA256)
	}

	editFile(t, root, "Orders.sql", "SELECT edited FROM raw")
	out, stderr, err := runPipelineCLI(t, root, "pipeline", "push", "--json")
	if err != nil {
		t.Fatalf("the first push after a draft-preferring clone must not be refused: %v\n%s", err, stderr)
	}
	file := decodeJSON(t, out)["files"].([]any)[0].(map[string]any)
	if file["outcome"] != pushOutcomePushed {
		t.Errorf("file = %+v\n%s", file, stderr)
	}
	if file["draftID"] != "table-draft-8" {
		t.Errorf("push did not resume the cloned draft: %v", file["draftID"])
	}
}

// TestPipelineCloneOfLossyDraftThenPush pins the OTHER half of the clone's
// fingerprint contract: the WIRE one it deliberately leaves EMPTY.
//
// recordDraftWrite's comment says exactly why it must stay empty — a clone READ
// somebody's draft rather than writing it, and the disk form it kept cannot be
// turned back into the bytes that row stores, because canonicalDisk is LOSSY: a
// positional ref and an id ref both land on the same stem. Recording a
// fingerprint we cannot vouch for would 409 the first push against a difference
// nobody made.
//
// Nothing was pinning it. TestPipelineCloneFromDraftThenPush clones ref-free
// SQL, where disk form and wire form are the same bytes, so writing the disk
// hash into the wire slot passed every assertion there — the B1 bug class
// arriving through the clone door instead of the push one.
//
// So: a draft whose stored SQL addresses its input POSITIONALLY. On disk the
// clone writes the resolved `{{ ref('table-orders') }}`; the row still holds
// `{{ ref('0') }}`, and the two do not hash alike. The push that follows must
// send NO precondition at all.
func TestPipelineCloneOfLossyDraftThenPush(t *testing.T) {
	f := newFakePipelineInstance(t)
	signInPipeline(t, f)
	f.AddFeature("collection-1", "Sales", "private")
	f.AddTable(&api.Table{ID: "table-orders", Name: "Orders", FeatureID: "collection-1",
		Code: "SELECT live FROM raw"})
	f.AddTable(&api.Table{ID: "table-revenue", Name: "Revenue", FeatureID: "collection-1",
		Code: "SELECT live FROM {{ ref('0') }}", InputModels: []string{"table-orders"}})
	f.AddDraft("table-revenue", "table-draft-9", "SELECT drafted FROM {{ ref('0') }}")

	dir := t.TempDir()
	if _, stderr, err := runPipelineCLI(t, dir, "pipeline", "clone", "collection-1", "out"); err != nil {
		t.Fatalf("clone: %v\n%s", err, stderr)
	}
	root := filepath.Join(dir, "out")
	// The premise of the whole case: what the clone put on disk and what the row
	// holds are DIFFERENT bytes. If this stopped holding, everything below would
	// go back to proving nothing.
	if got := readFile(t, root, "Revenue.sql"); got != "SELECT drafted FROM {{ ref('table-orders') }}" {
		t.Fatalf("the clone did not resolve the positional ref onto disk: %q", got)
	}
	if f.CodeOf("table-draft-9") != "SELECT drafted FROM {{ ref('0') }}" {
		t.Fatalf("the row no longer holds the positional form, so disk and wire no longer differ: %q",
			f.CodeOf("table-draft-9"))
	}
	state := pipelineStateOf(t, root, f.Key()).Tables["Revenue.sql"]
	if state.DraftWireSHA256 != "" {
		t.Fatalf("a clone READ that draft, it did not write it — there are no sent bytes to vouch for, "+
			"so the wire fingerprint must be empty; got %q", state.DraftWireSHA256)
	}

	// Edited in the id form the clone wrote, so the LOCAL drift guard has
	// nothing to say and the only thing left that can refuse this push is the
	// server's precondition — which is the point of the case.
	editFile(t, root, "Revenue.sql", "SELECT edited FROM {{ ref('table-orders') }}")
	_, stderr, err := runPipelineCLI(t, root, "pipeline", "push", "--json")
	if err != nil {
		t.Fatalf("the first push after a clone must write unconditionally, not 409 against a hash of "+
			"bytes the row never held: %v\n%s", err, stderr)
	}
	var wrote *recordedTableUpdate
	for i := range f.updates {
		if f.updates[i].ID == "table-draft-9" {
			wrote = &f.updates[i]
		}
	}
	if wrote == nil {
		t.Fatalf("the push never wrote the cloned draft: %+v", f.updates)
	}
	// ABSENT, not the empty string: the server answers 400 to "" (a precondition
	// that promises a check and performs none), so the two are not
	// interchangeable and only one of them means "write unconditionally".
	if wrote.BaseCodeSha256 != nil {
		t.Errorf("the push sent baseCodeSha256=%q; a clone has no wire-form fingerprint to send",
			*wrote.BaseCodeSha256)
	}
	if wrote.Code != "SELECT edited FROM {{ ref('table-orders') }}" {
		t.Errorf("the push sent %q, not the wire form", wrote.Code)
	}
}

// TestPipelineDiscardThenPushReStages: the fourth sequence, and the only one
// whose old behaviour contradicted a message the CLI itself prints. `discard`
// promises "push again to open fresh drafts"; with the content baseline left
// describing the discarded draft, the local file read as unchanged and the next
// push answered "Up to date" with no draft on the server at all.
func TestPipelineDiscardThenPushReStages(t *testing.T) {
	f := newFakePipelineInstance(t)
	signInPipeline(t, f)
	root := seedBoundFolder(t, f)

	editFile(t, root, "orders.sql", "SELECT staged FROM raw")
	if _, stderr, err := runPipelineCLI(t, root, "pipeline", "push"); err != nil {
		t.Fatalf("push: %v\n%s", err, stderr)
	}
	if _, stderr, err := runPipelineCLI(t, root, "pipeline", "discard", "--yes"); err != nil {
		t.Fatalf("discard: %v\n%s", err, stderr)
	}
	if len(f.discarded) != 1 {
		t.Fatalf("discarded = %+v", f.discarded)
	}

	out, stderr, err := runPipelineCLI(t, root, "pipeline", "push", "--json")
	if err != nil {
		t.Fatalf("push after discard: %v\n%s", err, stderr)
	}
	payload := decodeJSON(t, out)
	if payload["upToDate"] == true {
		t.Fatalf("push reported up to date with no draft on the server:\n%s", out)
	}
	file := payload["files"].([]any)[0].(map[string]any)
	if file["outcome"] != pushOutcomePushed {
		t.Fatalf("file = %+v\n%s", file, stderr)
	}
	// A FRESH draft, holding the local file again.
	if len(f.checkouts) != 2 {
		t.Errorf("checkouts = %v, want one per push", f.checkouts)
	}
	if f.CodeOf(file["draftID"].(string)) != "SELECT staged FROM raw" {
		t.Errorf("the new draft holds %q", f.CodeOf(file["draftID"].(string)))
	}
}

// TestPipelinePushRefusesARebinding: the baseline records WHICH ROW it was taken
// from, and this is what that is for. A manifest repointed at another table — by
// hand, by a merge, by a second clone — would otherwise inherit the previous
// table's fingerprints, and the next push would compare a colleague's table
// against a hash from something else entirely and conclude nothing had changed.
func TestPipelinePushRefusesARebinding(t *testing.T) {
	f := newFakePipelineInstance(t)
	signInPipeline(t, f)
	root := seedBoundFolder(t, f)
	// The file is repointed at the other table; the baseline still describes the
	// first one.
	manifest := pipelineManifestOf(t, root)
	binding := pipelineBindingOf(t, root, f.Key())
	manifest.SetBinding(f.Key(), binding.WithTable("orders.sql", "table-revenue"))
	if err := wfdir.SaveManifest(root, manifest); err != nil {
		t.Fatal(err)
	}
	editFile(t, root, "orders.sql", "SELECT 9 FROM raw")

	_, stderr, err := runPipelineCLI(t, root, "pipeline", "push")
	if err == nil {
		t.Fatal("a repointed binding must be refused, not silently trusted")
	}
	if !strings.Contains(stderr, "table-revenue") || !strings.Contains(stderr, "table-orders") {
		t.Errorf("the refusal does not name both rows:\n%s", stderr)
	}
	if len(f.updates) != 0 {
		t.Errorf("a refused push still wrote %+v", f.updates)
	}
	// --force accepts the new binding, discarding a baseline that describes
	// another table rather than comparing against it.
	if _, stderr, err := runPipelineCLI(t, root, "pipeline", "push", "--force"); err != nil {
		t.Fatalf("push --force: %v\n%s", err, stderr)
	}
	if len(f.updates) != 1 {
		t.Errorf("updates = %+v", f.updates)
	}
}

// TestPipelinePushScopesThePositionalRefGuardToWhatItSends: a positional ref in
// a file this run is not pushing cannot mis-resolve anything, and refusing the
// whole run for it lets one stale file hold every other table hostage.
func TestPipelinePushScopesThePositionalRefGuardToWhatItSends(t *testing.T) {
	f := newFakePipelineInstance(t)
	signInPipeline(t, f)
	root := seedBoundFolder(t, f)
	editFile(t, root, "revenue.sql", "SELECT * FROM {{ ref('0') }}")
	editFile(t, root, "orders.sql", "SELECT 10 FROM raw")

	out, stderr, err := runPipelineCLI(t, root, "pipeline", "push", "orders.sql", "--json")
	if err != nil {
		t.Fatalf("a positional ref in an unpushed file must not block a narrowed push: %v\n%s", err, stderr)
	}
	files := decodeJSON(t, out)["files"].([]any)
	if len(files) != 1 || files[0].(map[string]any)["path"] != "orders.sql" {
		t.Fatalf("files = %+v", files)
	}
	// And a full push, which WOULD send it, still refuses.
	if _, _, err := runPipelineCLI(t, root, "pipeline", "push"); err == nil {
		t.Fatal("a push that sends the file must still refuse it")
	}
}

// TestPipelinePushPrunesAPhantomBaseline: a file gone from disk but still BOUND
// is reported and left alone (there is no delete verb here). One that has left
// the bindings too is a file this folder no longer manages at all, and keeping
// its baseline entry means `status` reports a deletion for ever with no way to
// clear it short of deleting the whole baseline.
func TestPipelinePushPrunesAPhantomBaseline(t *testing.T) {
	f := newFakePipelineInstance(t)
	signInPipeline(t, f)
	root := seedBoundFolder(t, f)
	if err := removeFile(root, "revenue.sql"); err != nil {
		t.Fatal(err)
	}
	manifest := pipelineManifestOf(t, root)
	binding := pipelineBindingOf(t, root, f.Key())
	delete(binding.Tables, "revenue.sql")
	manifest.SetBinding(f.Key(), binding)
	if err := wfdir.SaveManifest(root, manifest); err != nil {
		t.Fatal(err)
	}
	editFile(t, root, "orders.sql", "SELECT 11 FROM raw")

	_, stderr, err := runPipelineCLI(t, root, "pipeline", "push")
	if err != nil {
		t.Fatalf("push: %v\n%s", err, stderr)
	}
	if !strings.Contains(stderr, "dropped the local baseline for revenue.sql") {
		t.Errorf("the prune was not reported:\n%s", stderr)
	}
	inst := pipelineStateOf(t, root, f.Key())
	if _, still := inst.Files["revenue.sql"]; still {
		t.Errorf("the phantom survived: %+v", inst.Files)
	}
	if _, still := inst.Tables["revenue.sql"]; still {
		t.Errorf("the phantom table state survived: %+v", inst.Tables)
	}
	// The table itself is untouched: pruning is a local bookkeeping act.
	if f.tables["table-revenue"] == nil {
		t.Error("pruning a baseline entry deleted the table")
	}
}

// --- the "no data found" diagnosis -------------------------------------------

// seedNoDataFolder is seedBoundFolder with revenue.sql edited and its build
// scripted to fail with the server's bare "no data found", plus whatever table
// state the case is actually about.
//
// The upstream's state is the whole variable: with a draft recorded, its rows
// are unpublished and the failure has a fix; without one, the same message means
// something else entirely.
func seedNoDataFolder(t *testing.T, f *fakePipelineInstance, upstream wfdir.TableState) string {
	t.Helper()
	root := seedBoundFolder(t, f)
	writePipelineBaseline(t, root, f.Key(), "collection-1",
		map[string]string{
			"orders.sql":  "SELECT * FROM raw",
			"revenue.sql": "SELECT * FROM {{ ref('table-orders') }}",
		},
		map[string]wfdir.TableState{
			"orders.sql":  upstream,
			"revenue.sql": {TableID: "table-revenue"},
		})
	editFile(t, root, "revenue.sql", "SELECT 1 FROM {{ ref('table-orders') }}")
	f.syncOutcome["table-revenue"] = "build_failed"
	f.syncMessage["table-revenue"] = "no data found"
	return root
}

// TestPipelinePushDiagnosesNoDataFromAnUnpublishedUpstream: the chain-bootstrap
// failure, which is the reason this hint exists.
//
// A draft builds against its inputs' LIVE data, so a downstream that reads an
// upstream whose rows are still sitting in an unpublished draft builds against
// an empty table. The server says "no data found" and names nothing — and the
// reader's own file is the last place the answer is.
func TestPipelinePushDiagnosesNoDataFromAnUnpublishedUpstream(t *testing.T) {
	f := newFakePipelineInstance(t)
	signInPipeline(t, f)
	root := seedNoDataFolder(t, f, wfdir.TableState{TableID: "table-orders", DraftID: "table-draft-7"})

	out, stderr, err := runPipelineCLI(t, root, "pipeline", "push", "--json")
	if err == nil {
		t.Fatal("a failed build must exit non-zero")
	}
	file := decodeJSON(t, out)["files"].([]any)[0].(map[string]any)
	if file["path"] != "revenue.sql" || file["outcome"] != pushOutcomeBuildFailed {
		t.Fatalf("file = %+v", file)
	}
	hint, _ := file["hint"].(string)
	// It has to name the FILE and the command, not just say that something is
	// unpublished: the whole failure is that the reader cannot tell which of
	// their files the server is talking about.
	if !strings.Contains(hint, "orders.sql") || !strings.Contains(hint, "ronja pipeline publish orders.sql") {
		t.Errorf("hint = %q", hint)
	}
	if strings.Contains(hint, "transient") {
		t.Errorf("an unpublished upstream was diagnosed as the race: %q", hint)
	}
	// The server's own message survives untouched beside it — the hint is
	// advice, never a replacement for what actually failed.
	if !strings.Contains(file["error"].(string), "no data found") {
		t.Errorf("error = %v", file["error"])
	}
	if !strings.Contains(stderr, "ronja pipeline publish orders.sql") {
		t.Errorf("the hint was not reported on stderr:\n%s", stderr)
	}
	// Diagnosed, not retried: a retry that happened to succeed would hide the
	// cause, and one sync is what the report describes.
	if n := len(f.syncs); n != 1 {
		t.Errorf("the push synced %d times, want 1 — the hint must not retry", n)
	}
}

// TestPipelinePushDiagnosesNoDataAsPossiblyTransient: the same message with
// nothing unpublished behind it.
//
// It appears transiently right after an upstream IS published (issue #4607), so
// the honest answer is "push again" plus the one other thing it can mean —
// rather than telling somebody to publish a draft they do not have.
func TestPipelinePushDiagnosesNoDataAsPossiblyTransient(t *testing.T) {
	f := newFakePipelineInstance(t)
	signInPipeline(t, f)
	root := seedNoDataFolder(t, f, wfdir.TableState{TableID: "table-orders"})

	out, stderr, err := runPipelineCLI(t, root, "pipeline", "push", "--json")
	if err == nil {
		t.Fatal("a failed build must exit non-zero")
	}
	file := decodeJSON(t, out)["files"].([]any)[0].(map[string]any)
	hint, _ := file["hint"].(string)
	if !strings.Contains(hint, "transient") || !strings.Contains(hint, "push again") {
		t.Errorf("hint = %q", hint)
	}
	if strings.Contains(hint, "pipeline publish") {
		t.Errorf("an upstream with no draft was reported as unpublished: %q", hint)
	}
	if !strings.Contains(stderr, "transient") {
		t.Errorf("the hint was not reported on stderr:\n%s", stderr)
	}
}

// TestPipelineNoDataHintIgnoresOtherFailures: the hint is added to ONE message.
// Every other build failure already explains itself, and appending advice about
// upstream drafts to a syntax error would be noise in the one place people read
// carefully.
func TestPipelineNoDataHintIgnoresOtherFailures(t *testing.T) {
	binding := wfdir.Binding{Tables: map[string]string{"orders.sql": "table-orders"}}
	inst := &wfdir.InstanceState{
		Tables: map[string]wfdir.TableState{"orders.sql": {TableID: "table-orders", DraftID: "table-draft-7"}},
	}
	got := pipelineNoDataHint("Parser Error: syntax error at or near \"SELCT\"",
		[]string{"table-orders"}, binding, inst, nil)
	if got != "" {
		t.Errorf("hint on an ordinary failure = %q", got)
	}
	// And the same inputs with the message it IS for.
	if got := pipelineNoDataHint("no data found", []string{"table-orders"}, binding, inst, nil); got == "" {
		t.Error("no hint on the message this exists for")
	}
	// A table id that is not a file in this folder cannot be published from
	// here, so it must not be named as if it could.
	outside := pipelineNoDataHint("no data found", []string{"table-elsewhere"}, binding, inst, nil)
	if strings.Contains(outside, "pipeline publish") {
		t.Errorf("an input outside the folder was reported as publishable: %q", outside)
	}
}

// TestPipelinePushPersistsAPruneWithNothingElseToDo: the prune is a real edit to
// the baseline, and the path it is MOST likely to be taken on is the one where
// there is nothing else to push — somebody removed a file and its binding, and
// everything else is up to date.
//
// Left unsaved there, the entry survived the process: the note was printed on
// every run for ever, `status` kept inventing the same deletion, and the only
// way out was deleting the whole baseline by hand.
func TestPipelinePushPersistsAPruneWithNothingElseToDo(t *testing.T) {
	f := newFakePipelineInstance(t)
	signInPipeline(t, f)
	root := seedBoundFolder(t, f)
	if err := removeFile(root, "revenue.sql"); err != nil {
		t.Fatal(err)
	}
	manifest := pipelineManifestOf(t, root)
	binding := pipelineBindingOf(t, root, f.Key())
	delete(binding.Tables, "revenue.sql")
	manifest.SetBinding(f.Key(), binding)
	if err := wfdir.SaveManifest(root, manifest); err != nil {
		t.Fatal(err)
	}
	// No edit: orders.sql matches its baseline, so the push has nothing to send.

	out, stderr, err := runPipelineCLI(t, root, "pipeline", "push", "--json")
	if err != nil {
		t.Fatalf("push: %v\n%s", err, stderr)
	}
	if decodeJSON(t, out)["upToDate"] != true {
		t.Fatalf("this push had nothing to send: %s", out)
	}
	if !strings.Contains(stderr, "dropped the local baseline for revenue.sql") {
		t.Errorf("the prune was not reported:\n%s", stderr)
	}
	inst := pipelineStateOf(t, root, f.Key())
	if _, still := inst.Files["revenue.sql"]; still {
		t.Errorf("the prune was not persisted: %+v", inst.Files)
	}
	if _, still := inst.Tables["revenue.sql"]; still {
		t.Errorf("the pruned table state was not persisted: %+v", inst.Tables)
	}
	// The second run is the whole point: a prune that only happened in memory
	// reports itself again, for ever.
	_, stderr, err = runPipelineCLI(t, root, "pipeline", "push")
	if err != nil {
		t.Fatalf("second push: %v\n%s", err, stderr)
	}
	if strings.Contains(stderr, "dropped the local baseline") {
		t.Errorf("the prune was reported a second time:\n%s", stderr)
	}
}

// TestPipelinePushIgnoresAFingerprintFromADifferentDraft: leg (b) of the drift
// guard compares your OWN draft against the fingerprint taken FROM THAT DRAFT —
// and a recorded id that is not the open one describes a row that no longer
// exists.
//
// The state is ordinary: a draft was pushed, somebody committed or discarded it
// from the web UI, and a fresh one was opened. Comparing the new draft against
// the old one's bytes refuses a push over a draft nobody has touched.
func TestPipelinePushIgnoresAFingerprintFromADifferentDraft(t *testing.T) {
	f := newFakePipelineInstance(t)
	signInPipeline(t, f)
	root := seedBoundFolder(t, f)
	// A fresh draft: byte-identical to its parent, because nobody has edited it.
	f.AddDraft("table-orders", "table-draft-new", "SELECT * FROM raw")
	// The baseline still points at the draft that came before it, whose contents
	// were something else entirely.
	writePipelineBaseline(t, root, f.Key(), "collection-1",
		map[string]string{"orders.sql": "SELECT * FROM raw"},
		map[string]wfdir.TableState{"orders.sql": {
			TableID:     "table-orders",
			DraftID:     "table-draft-old",
			DraftSHA256: wfdir.HashString("SELECT something the old draft held"),
		}})
	editFile(t, root, "orders.sql", "SELECT mine FROM raw")

	out, stderr, err := runPipelineCLI(t, root, "pipeline", "push", "--json")
	if err != nil {
		t.Fatalf("a fingerprint from a draft that is gone must not refuse the push: %v\n%s", err, stderr)
	}
	file := decodeJSON(t, out)["files"].([]any)[0].(map[string]any)
	if file["outcome"] != pushOutcomePushed || file["draftID"] != "table-draft-new" {
		t.Errorf("file = %+v\n%s", file, stderr)
	}
}

// TestPipelinePushForceRecordsTheLiveHashItOverwrote: --force is the author
// saying they have seen what is on the server and mean to write over it, so the
// acknowledgement is recorded.
//
// Without it the same difference is re-reported on every later push and --force
// becomes a permanent part of the command line — which trains it past the one
// time it means something.
func TestPipelinePushForceRecordsTheLiveHashItOverwrote(t *testing.T) {
	f := newFakePipelineInstance(t)
	signInPipeline(t, f)
	root := seedBoundFolder(t, f)
	editFile(t, root, "orders.sql", "SELECT mine FROM raw")
	f.tables["table-orders"].Code = "SELECT theirs FROM raw"

	if _, stderr, err := runPipelineCLI(t, root, "pipeline", "push", "--force"); err != nil {
		t.Fatalf("push --force: %v\n%s", err, stderr)
	}
	if got := pipelineStateOf(t, root, f.Key()).Tables["orders.sql"].LiveSHA256; got != wfdir.HashString("SELECT theirs FROM raw") {
		t.Errorf("live fingerprint = %q — the forced overwrite was not acknowledged", got)
	}

	// The next ordinary push must not demand --force again.
	editFile(t, root, "orders.sql", "SELECT mine again FROM raw")
	if _, stderr, err := runPipelineCLI(t, root, "pipeline", "push"); err != nil {
		t.Fatalf("the push after a forced one must not need --force: %v\n%s", err, stderr)
	}
}

// TestPipelinePushSkipsTheSampleOnALargeTable: a sample is an LLM routing call
// and, on anything this size, a Batch job — for five rows of garnish under a
// report whose verdict, schema delta and row counts are already complete.
func TestPipelinePushSkipsTheSampleOnALargeTable(t *testing.T) {
	f := newFakePipelineInstance(t)
	signInPipeline(t, f)
	root := seedBoundFolder(t, f)
	editFile(t, root, "orders.sql", "SELECT mine FROM raw")
	f.AddDraft("table-orders", "table-draft-big", "SELECT * FROM raw")
	f.rowCounts["table-draft-big"] = maxSampledRowCount + 1

	_, stderr, err := runPipelineCLI(t, root, "pipeline", "push")
	if err != nil {
		t.Fatalf("push: %v\n%s", err, stderr)
	}
	if len(f.queries) != 0 {
		t.Errorf("a large table was sampled anyway: %v", f.queries)
	}
	if !strings.Contains(stderr, "no sample rows for orders.sql") {
		t.Errorf("the skip was not reported:\n%s", stderr)
	}
	// A small table is still sampled — the guard is a size rule, not a removal.
	f.rowCounts["table-draft-big"] = 12
	editFile(t, root, "orders.sql", "SELECT mine again FROM raw")
	if _, stderr, err := runPipelineCLI(t, root, "pipeline", "push"); err != nil {
		t.Fatalf("second push: %v\n%s", err, stderr)
	}
	if len(f.queries) != 1 {
		t.Errorf("a small table must still be sampled: %v", f.queries)
	}
}

// A cross-organization `{{ ref }}` and a non-admin create both come back as 403
// from POST /feature/model, and the CLI used to report BOTH as "creating a NEW
// table needs an admin". Verified false in the field: with an admin token in the
// target organization, the refusal was rmodelv2.AssertTablesReadable declining a
// ref that belongs elsewhere — and rjerr.Forbiddenf withholds the id on purpose,
// so the message had nothing to go on and always guessed.
//
// The bar is `ronja wf validate`'s unresolved_ref finding: name the id, quote
// the marker, say what to do about it.
// TestPipelinePushNamesTheOrganizationWhenTheFolderBelongsToAnother pins the
// pipeline half of the same defect featureIDFor fixes for workflows: a folder
// cloned from another organization arrives here with entries that all have a
// featureID, so "add featureID to the <url> entry" describes a line the reader
// is looking straight at.
func TestPipelinePushNamesTheOrganizationWhenTheFolderBelongsToAnother(t *testing.T) {
	f := newFakePipelineInstance(t)
	signInPipeline(t, f)
	f.AddFeature("collection-1", "Sales", "private")

	root := t.TempDir()
	manifest := &wfdir.Manifest{Kind: wfdir.KindPipeline, Title: "Sales"}
	// Bound to a DIFFERENT organization on this same instance, with a featureID
	// present — which is what a clone from elsewhere looks like.
	manifest.SetBinding(wfdir.InstanceKey{URL: f.URL(), TenantID: "ten-elsewhere"},
		wfdir.Binding{FeatureID: "collection-elsewhere"})
	writePipelineFolder(t, root, manifest, map[string]string{
		"revenue.sql": "SELECT 1",
	})

	_, stderr, err := runPipelineCLI(t, root, "pipeline", "push")
	if err == nil {
		t.Fatal("push accepted a folder with no feature for this organization")
	}
	// This refusal is the command's own error, not a per-file stderr line, so
	// read both — the reader sees whichever the runner prints.
	said := err.Error() + stderr
	if !strings.Contains(said, "ten-elsewhere") {
		t.Errorf("refusal does not name the organization the folder DOES have:\n%s", said)
	}
	if strings.Contains(said, `Add "featureID" to the`) {
		t.Errorf("still tells the reader to add a featureID that is already there:\n%s", said)
	}
}

func TestPipelinePushNamesTheRefItCouldNotRead(t *testing.T) {
	f := newFakePipelineInstance(t)
	signInPipeline(t, f)
	f.AddFeature("collection-1", "Sales", "private")
	f.failCreate = 403

	root := t.TempDir()
	manifest := &wfdir.Manifest{Kind: wfdir.KindPipeline, Title: "Sales"}
	manifest.SetBinding(f.Key(), wfdir.Binding{FeatureID: "collection-1"})
	// table-elsewhere exists in no organization this token can see, which is what
	// a folder cloned from another organization carries in its SQL.
	writePipelineFolder(t, root, manifest, map[string]string{
		"revenue.sql": "SELECT * FROM {{ ref('table-elsewhere') }}",
	})

	_, stderr, err := runPipelineCLI(t, root, "pipeline", "push")
	if err == nil {
		t.Fatal("push accepted a table whose ref it cannot read")
	}
	if !strings.Contains(stderr, "table-elsewhere") {
		t.Errorf("refusal does not name the id:\n%s", stderr)
	}
	if !strings.Contains(stderr, "{{ ref('table-elsewhere') }}") {
		t.Errorf("refusal does not quote the marker:\n%s", stderr)
	}
	// The wrong guess must not survive beside the right answer.
	if strings.Contains(stderr, "needs an admin") {
		t.Errorf("still reported as an admin problem:\n%s", stderr)
	}
}

// ...and the admin explanation survives for the case where it IS the cause. A
// 403 whose refs all read fine is a create the caller is not allowed to make,
// and replacing one wrong guess with another would be no better than the bug.
func TestPipelinePushStillBlamesAdminWhenTheRefsAreFine(t *testing.T) {
	f := newFakePipelineInstance(t)
	signInPipeline(t, f)
	f.AddFeature("collection-1", "Sales", "private")
	f.AddTable(&api.Table{ID: "table-orders", Name: "Orders", FeatureID: "collection-1", Code: "SELECT 0"})
	f.failCreate = 403

	root := t.TempDir()
	manifest := &wfdir.Manifest{Kind: wfdir.KindPipeline, Title: "Sales"}
	manifest.SetBinding(f.Key(), wfdir.Binding{FeatureID: "collection-1"})
	writePipelineFolder(t, root, manifest, map[string]string{
		"revenue.sql": "SELECT * FROM {{ ref('table-orders') }}",
	})

	_, stderr, err := runPipelineCLI(t, root, "pipeline", "push")
	if err == nil {
		t.Fatal("push accepted a refused create")
	}
	if !strings.Contains(stderr, "needs an admin") {
		t.Errorf("the admin explanation was lost:\n%s", stderr)
	}
}
