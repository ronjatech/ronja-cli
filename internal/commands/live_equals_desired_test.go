package commands

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/ronjatech/ronja-cli/internal/api"
	"github.com/ronjatech/ronja-cli/internal/metricfile"
	"github.com/ronjatech/ronja-cli/internal/wfdir"
)

// LIVE EQUALS DESIRED IS NEVER DRIFT, leg by leg. Every test here stages the
// same history: this folder's own write landed on the server, and the anchor
// recording it was lost — the lock commit dropped, the CI checkout discarded, a
// crash between the publish and the anchor write. The server then holds exactly
// what the folder would write, and the guard used to call that somebody else's
// change.

func TestExcuseAgreedFiles(t *testing.T) {
	drift := wfdir.Diff{
		Added:    []string{"same.py", "theirs.py"},
		Modified: []string{"main.py", "moved.py"},
		Deleted:  []string{"gone-here-too.py", "gone-there.py"},
	}
	remote := map[string]string{"same.py": "s", "theirs.py": "t", "main.py": "mine", "moved.py": "theirs"}
	local := map[string]string{"same.py": "s", "main.py": "mine", "moved.py": "mine", "gone-there.py": "keep"}
	got, excused := excuseAgreedFiles(drift, remote, local)
	if excused != 3 {
		t.Errorf("excused = %d, want 3 (same.py, main.py, gone-here-too.py)", excused)
	}
	if strings.Join(got.Added, ",") != "theirs.py" || strings.Join(got.Modified, ",") != "moved.py" ||
		strings.Join(got.Deleted, ",") != "gone-there.py" {
		t.Errorf("drift left = %+v", got)
	}
	if !filesAgree(map[string]string{"a": "1"}, map[string]string{"a": "1"}) ||
		filesAgree(map[string]string{"a": "1"}, map[string]string{"a": "2"}) ||
		filesAgree(map[string]string{"a": "1"}, map[string]string{"a": "1", "b": "2"}) {
		t.Error("filesAgree is not exact")
	}
}

// A WORKFLOW published from this folder whose head anchor was lost: a fresh
// checkout sees the head moved, and the live row holds exactly the folder's
// files. It pushes without --force, writes nothing, and re-anchors.
func TestWorkflowFreshCheckoutOverItsOwnPublishIsNotDrift(t *testing.T) {
	f := newFakeInstance(t)
	f.AddWorkflow(&api.Workflow{ID: "wf-1", Lifecycle: api.LifecycleLive, FeatureScope: "private"},
		api.WorkflowFile{Path: "main.py", Content: "live\n"})
	f.versions["wf-1"] = []api.Workflow{{ID: "wfv-1"}}
	signIn(t, f)
	out, err := runCLI(t, t.TempDir(), "wf", "clone", "wf-1", "--stack", "dev", "--json")
	if err != nil {
		t.Fatalf("clone: %v", err)
	}
	root := decodeJSON(t, out)["root"].(string)
	writeLocal(t, root, "main.py", "mine\n")
	// The folder's own publish landed — and its anchor did not.
	f.files["wf-1"][0].Content = "mine\n"
	f.versions["wf-1"] = []api.Workflow{{ID: "wfv-2"}, {ID: "wfv-1"}}
	freshCheckout(t, root)
	f.fileWrites = nil

	if _, err := runCLI(t, root, "wf", "push", "--json"); err != nil {
		t.Fatalf("a push over the folder's own publish was refused: %v", err)
	}
	if len(f.fileWrites) != 0 {
		t.Errorf("nothing differed, yet files were written: %+v", f.fileWrites)
	}
	if lock := readFile(t, wfdir.LockPath(root)); !strings.Contains(lock, `"wfv-2"`) {
		t.Errorf("the anchor was not re-taken:\n%s", lock)
	}
}

// The SAME history for a DATA APP.
func TestAppFreshCheckoutOverItsOwnPublishIsNotDrift(t *testing.T) {
	f := newFakeAppInstance(t)
	f.AddApp(&api.DataApp{ID: "app-1", Lifecycle: api.LifecycleLive},
		api.DataAppFile{Path: "App.tsx", Content: "live\n"})
	f.versions["app-1"] = []api.DataApp{{ID: "dav-1"}}
	signInApp(t, f)
	out, err := runCLI(t, t.TempDir(), "app", "clone", "app-1", "--stack", "dev", "--json")
	if err != nil {
		t.Fatalf("clone: %v", err)
	}
	root := decodeJSON(t, out)["root"].(string)
	writeLocal(t, root, "App.tsx", "mine\n")
	f.files["app-1"][0].Content = "mine\n"
	f.versions["app-1"] = []api.DataApp{{ID: "dav-2"}, {ID: "dav-1"}}
	freshCheckout(t, root)

	if _, err := runCLI(t, root, "app", "push", "--no-validate"); err != nil {
		t.Fatalf("a push over the folder's own publish was refused: %v", err)
	}
}

// A WORKFLOW with a local baseline whose file moved on the server TO what the
// folder holds: no drift, no write.
func TestWorkflowPushExcusesAFileThatMovedToTheLocalBytes(t *testing.T) {
	f := newFakeInstance(t)
	f.AddWorkflow(&api.Workflow{ID: "wf-1", Lifecycle: api.LifecycleLive, FeatureScope: "private"},
		api.WorkflowFile{Path: "main.py", Content: "live\n"})
	signIn(t, f)
	root := cloneFolder(t, f, "wf-1")
	writeLocal(t, root, "main.py", "mine\n")
	f.files["wf-1"][0].Content = "mine\n"
	if _, err := runCLI(t, root, "wf", "push", "--json"); err != nil {
		t.Fatalf("a file that already holds the local bytes was refused as drift: %v", err)
	}
}

// MODULE: the same two legs, at the guard, plus the title leg that asked only
// "did it move".
func TestModuleDriftExcusesLiveEqualsDesired(t *testing.T) {
	key := moduleKey()
	root := moduleTestFolder(t, key, wfdir.Binding{ModuleID: "module-1", FeatureID: "collection-1"}, nil)
	target := &api.Module{ID: "module-draft", ParentModuleID: "module-1",
		Name: "emab_tracker", Title: "EMAB tracker", Lifecycle: api.LifecycleDraft}
	local := map[string]string{"__init__.py": "", "tracker.py": "x = 2"}
	moved := map[string]string{"__init__.py": "", "tracker.py": "x = 2"}
	baseline := &wfdir.InstanceState{
		SourceID: "module-draft", Name: "emab_tracker", Title: "Old title",
		Files: map[string]wfdir.FileState{
			"__init__.py": {SHA256: wfdir.HashString("")},
			"tracker.py":  {SHA256: wfdir.HashString("x = 1")},
		},
	}
	f := moduleFolderFor(t, root, key, baseline)
	f.Manifest.Title = "EMAB tracker"
	if _, err := checkModuleDrift(f, target, moved, local, false, headAgreement{}); err != nil {
		t.Fatalf("a file and a title that already hold what the folder declares were refused: %v", err)
	}
	f = moduleFolderFor(t, root, key, nil)
	f.Manifest.Title = "EMAB tracker"
	f.Manifest.Name = "emab_tracker"
	head := headAgreement{Recorded: "cmv-1", Current: "cmv-2", AnchoredDraft: true}
	verdict, err := checkModuleDrift(f, target, moved, local, false, head)
	if err != nil || !verdict.HeadVouched {
		t.Fatalf("a moved head over identical files was refused: %+v %v", verdict, err)
	}
}

// PIPELINE leg (a): the lock's live fingerprint is stale, and the live table
// holds exactly the file's SQL. No refusal, and the fingerprint is re-taken.
func TestPipelinePushReanchorsALiveTableThatHoldsTheFile(t *testing.T) {
	f := newFakePipelineInstance(t)
	signInPipeline(t, f)
	f.AddFeature("collection-1", "Sales pipeline", "private")
	f.AddTable(&api.Table{ID: "table-orders", Name: "orders", FeatureID: "collection-1", Code: "SELECT * FROM raw WHERE shipped"})
	_, root := applyPipelineTree(t, f,
		map[string]string{"orders.sql": "SELECT * FROM raw WHERE shipped"},
		map[string]string{"orders.sql": "table-orders"},
		map[string]string{"orders.sql": "SELECT * FROM raw"})
	// A draft already open, so the push forks nothing: the re-anchor has to come
	// from the agreement itself, not from a fresh checkout's.
	f.AddDraft("table-orders", "table-draft-x", "SELECT * FROM raw WHERE shipped")

	if out, err := runCLI(t, root, "pipeline", "push", "--stack", "prod", "--json"); err != nil {
		t.Fatalf("a table that already holds the file was refused as drift: %v\n%s", err, out)
	}
	lock, _ := wfdir.LoadLock(root)
	if got := lock.TableLive("prod", "orders.sql"); got != wfdir.HashString("SELECT * FROM raw WHERE shipped") {
		t.Errorf("live fingerprint = %q, want it re-taken", got)
	}
}

// PIPELINE leg (c): the recorded documentation fingerprint is stale, and the
// live table already says what the header declares.
func TestPipelinePushExcusesDocumentationTheTableAlreadyHolds(t *testing.T) {
	f := newFakePipelineInstance(t)
	signInPipeline(t, f)
	f.AddFeature("collection-1", "Sales pipeline", "private")
	content := "-- @table One row per order.\nSELECT * FROM raw"
	table := f.AddTable(&api.Table{ID: "table-orders", Name: "orders", FeatureID: "collection-1", Code: content})
	table.Description = "One row per order."
	_, root := applyPipelineTree(t, f, map[string]string{"orders.sql": content},
		map[string]string{"orders.sql": "table-orders"}, nil)
	lock, _ := wfdir.LoadLock(root)
	lock.SetTableMeta("prod", "orders.sql", "table-orders", "a-stale-fingerprint")
	if err := wfdir.SaveLock(root, lock); err != nil {
		t.Fatal(err)
	}
	if out, err := runCLI(t, root, "pipeline", "push", "--stack", "prod", "--json"); err != nil {
		t.Fatalf("documentation the table already holds was refused as drift: %v\n%s", err, out)
	}
}

// METRIC recipe leg: the recorded live fingerprint is stale, and the live
// recipe is exactly what the file resolves to.
func TestPipelinePushReanchorsAMetricThatHoldsTheFile(t *testing.T) {
	f := newFakePipelineInstance(t)
	signInPipeline(t, f)
	seedFeature(f, "private")
	parsed, err := metricfile.Parse([]byte(metricFileBody(aovRecipe)))
	if err != nil {
		t.Fatal(err)
	}
	resolved, err := parsed.Resolve("table-orders")
	if err != nil {
		t.Fatal(err)
	}
	f.AddMetric("table-aov", "aov", string(resolved))
	_, root := metricApplyTree(t, f, "")
	lock, _ := wfdir.LoadLock(root)
	lock.SetMetricSeen("prod", aovMetricPath, "table-aov", "a-stale-fingerprint", "")
	if err := wfdir.SaveLock(root, lock); err != nil {
		t.Fatal(err)
	}
	if out, err := runCLI(t, root, "pipeline", "push", "--stack", "prod", "--json"); err != nil {
		t.Fatalf("a metric that already holds the file was refused as drift: %v\n%s", err, out)
	}
}

// TestSameRecipeComparesTheFileInTheServersStoredForm pins the equality every
// metric live-equals-desired leg asks: the stored row is the server's typed
// marshal (every key, `version` defaulted, `source` trimmed), so a hand-written
// file that leaves those out is still the same definition — and a key the row
// holds that the file's shape does not know still makes them different.
func TestSameRecipeComparesTheFileInTheServersStoredForm(t *testing.T) {
	stored := json.RawMessage(`{"version":1,"source":"table-orders","time":{"column":"","native_grain":""},"dimensions":null,"base_measures":[{"name":"revenue","agg":"sum","column":"amount"}],"value":"revenue"}`)
	for _, c := range []struct {
		name string
		file string
		want bool
	}{
		{"every defaultable key left out", `{"value":"revenue","source":" table-orders ","base_measures":[{"name":"revenue","agg":"sum","column":"amount"}]}`, true},
		{"spelled in full, other order", `{"base_measures":[{"column":"amount","agg":"sum","name":"revenue"}],"dimensions":null,"value":"revenue","version":1,"source":"table-orders","time":{"native_grain":"","column":""}}`, true},
		{"a different value", `{"value":"revenue * 2","source":"table-orders","base_measures":[{"name":"revenue","agg":"sum","column":"amount"}]}`, false},
		{"an empty dimension list is not an absent one", `{"value":"revenue","source":"table-orders","dimensions":[],"base_measures":[{"name":"revenue","agg":"sum","column":"amount"}]}`, false},
		{"not an object", `[1]`, false},
	} {
		if got := sameRecipe(stored, json.RawMessage(c.file)); got != c.want {
			t.Errorf("%s: sameRecipe = %v, want %v", c.name, got, c.want)
		}
	}
	newer := json.RawMessage(`{"version":1,"source":"table-orders","time":{"column":"","native_grain":""},"dimensions":null,"base_measures":[],"value":"revenue","filters":["x"]}`)
	if sameRecipe(newer, json.RawMessage(`{"value":"revenue","source":"table-orders","base_measures":[],"filters":["x"]}`)) {
		t.Error("a stored key this CLI's shape does not know must keep the two different")
	}
}

// …but ONLY when the metadata agrees too. Files that match over a moved head
// say nothing about a title somebody published, and a fresh checkout has no
// baseline to check that title against — so this still refuses.
func TestWorkflowFreshCheckoutOverAMovedHeadStillRefusesOtherMetadata(t *testing.T) {
	f := newFakeInstance(t)
	f.AddWorkflow(&api.Workflow{ID: "wf-1", Lifecycle: api.LifecycleLive, FeatureScope: "private", Title: "Nightly"},
		api.WorkflowFile{Path: "main.py", Content: "live\n"})
	f.versions["wf-1"] = []api.Workflow{{ID: "wfv-1"}}
	signIn(t, f)
	out, err := runCLI(t, t.TempDir(), "wf", "clone", "wf-1", "--stack", "dev", "--json")
	if err != nil {
		t.Fatalf("clone: %v", err)
	}
	root := decodeJSON(t, out)["root"].(string)
	m, err := wfdir.LoadManifest(root, wfdir.WorkflowKind)
	if err != nil {
		t.Fatal(err)
	}
	m.Title = "Somebody else's idea"
	if err := wfdir.SaveManifest(root, m); err != nil {
		t.Fatal(err)
	}
	f.versions["wf-1"] = []api.Workflow{{ID: "wfv-2"}, {ID: "wfv-1"}}
	freshCheckout(t, root)
	if _, err := runCLI(t, root, "wf", "push", "--json"); err == nil {
		t.Fatal("a moved head was walked past on matching files alone, with a title to write")
	}
}

// DOCS SIDECAR: the row's documentation moved since the recording — to exactly
// what the file declares. Nothing to overwrite, so no refusal.
func TestPipelinePushExcusesASidecarTheTableAlreadyAgreesWith(t *testing.T) {
	f := newFakePipelineInstance(t)
	signInPipeline(t, f)
	root := seedBoundFolder(t, f)
	f.AddTable(&api.Table{ID: "table-raw", Name: "Raw", FeatureID: "collection-1", Kind: "integration"})
	f.fields["table-raw"] = []string{"amount"}
	declareTableDependency(t, root, f.Key(), "raw", "table-raw")
	writeSidecar(t, root, "raw", `{"columns":{"amount":"ours"}}`)
	if _, stderr, err := runPipelineCLI(t, root, "pipeline", "push", "--json"); err != nil {
		t.Fatalf("first push: %v\n%s", err, stderr)
	}
	f.mu.Lock()
	f.columnDocs["table-raw"]["amount"] = "ours, reworded"
	f.mu.Unlock()
	writeSidecar(t, root, "raw", `{"columns":{"amount":"ours, reworded"}}`)
	if _, stderr, err := runPipelineCLI(t, root, "pipeline", "push", "--json"); err != nil {
		t.Fatalf("a row already saying what the file declares was refused as drift: %v\n%s", err, stderr)
	}
}
