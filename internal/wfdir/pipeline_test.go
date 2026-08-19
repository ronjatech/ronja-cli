package wfdir

import (
	"encoding/json"
	"os"
	"strings"
	"testing"
)

// ── The kind registry ───────────────────────────────────────────────────────

// TestEveryKindNamesItsOwnCommand: Kind.Command is the ONE mapping from a folder
// kind to the CLI command that drives it, and it replaced two binary if/else
// spellings of the same thing (wfdir.commandFor and commands.kindCommand), both
// of which would have routed a third kind to `ronja wf`.
//
// The test is on the registry rather than on a list of literals because a fourth
// kind is added by adding a kindByName entry, and that is exactly the moment the
// question has to be answered.
func TestEveryKindNamesItsOwnCommand(t *testing.T) {
	seen := map[string]string{}
	for name, kind := range kindByName {
		if kind.Name != name {
			t.Errorf("kindByName[%q] holds a Kind named %q", name, kind.Name)
		}
		if kind.Label == "" {
			t.Errorf("kind %q has no Label", name)
		}
		if kind.Command == "" {
			t.Errorf("kind %q has no Command — a wrong-kind refusal would name nothing", name)
		}
		if other, dup := seen[kind.Command]; dup {
			t.Errorf("kinds %q and %q both claim the command %q", other, name, kind.Command)
		}
		seen[kind.Command] = name
	}
	if _, ok := kindByName[KindPipeline]; !ok {
		t.Fatal("KindPipeline is missing from kindByName — LoadManifest cannot name `ronja pipeline`")
	}
}

// TestLoadManifestRoutesEveryWrongKindToTheRightCommand walks the full matrix:
// a folder of each kind, opened as each OTHER kind, must refuse AND name the
// command that would have worked.
//
// The third kind is the reason this is a matrix rather than the one-direction
// check the two-kind world got away with. Under the old binary branches a
// pipeline folder opened as a workflow refused correctly by accident (the kinds
// differ) while pointing at `ronja wf` — the command the caller had just run.
func TestLoadManifestRoutesEveryWrongKindToTheRightCommand(t *testing.T) {
	kinds := []Kind{WorkflowKind, DataAppKind, PipelineKind}
	for _, onDisk := range kinds {
		for _, openedAs := range kinds {
			root := t.TempDir()
			if err := SaveManifest(root, &Manifest{Kind: onDisk.Name, Title: "X"}); err != nil {
				t.Fatal(err)
			}
			_, err := LoadManifest(root, openedAs)
			if onDisk.Name == openedAs.Name {
				if err != nil {
					t.Errorf("a %s folder must load as %s: %v", onDisk.Name, openedAs.Name, err)
				}
				continue
			}
			if err == nil {
				t.Errorf("a %s folder must not load as a %s", onDisk.Name, openedAs.Name)
				continue
			}
			if !strings.Contains(err.Error(), onDisk.Command) {
				t.Errorf("a %s folder opened as %s should point at %q, got: %v",
					onDisk.Name, openedAs.Name, onDisk.Command, err)
			}
			// And it must NOT point at the command that just refused it.
			if strings.Contains(err.Error(), "use `"+openedAs.Command+"`") {
				t.Errorf("a %s folder opened as %s points back at %q, the command that refused it: %v",
					onDisk.Name, openedAs.Name, openedAs.Command, err)
			}
		}
	}
}

// TestLoadManifestNoKindNamesTheCallersOwnCommand: a manifest with no `kind` is
// a hand-written or truncated file, so the fix-it line names the command the
// caller ran — which for a pipeline folder must not be `ronja wf init`.
func TestLoadManifestNoKindNamesTheCallersOwnCommand(t *testing.T) {
	root := t.TempDir()
	if err := os.WriteFile(ManifestPath(root), []byte(`{"title":"X"}`+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	_, err := LoadManifest(root, PipelineKind)
	if err == nil {
		t.Fatal("a manifest with no kind must be refused")
	}
	if !strings.Contains(err.Error(), "ronja pipeline init") {
		t.Errorf("the refusal should name `ronja pipeline init`, got %v", err)
	}
}

// TestPipelineManifestToleratesAnEmptyEntrypoint: a pipeline folder has no
// entrypoint, and LoadManifest falls back to Kind.DefaultEntrypoint — itself
// empty here. Nothing may invent "main.py" on the way through.
func TestPipelineManifestToleratesAnEmptyEntrypoint(t *testing.T) {
	root := t.TempDir()
	if err := SaveManifest(root, &Manifest{Kind: KindPipeline, Title: "Sales pipeline"}); err != nil {
		t.Fatal(err)
	}
	m, err := LoadManifest(root, PipelineKind)
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	if m.Entrypoint != "" {
		t.Errorf("a pipeline folder must keep an empty entrypoint, got %q", m.Entrypoint)
	}
}

// ── The multi-resource binding ──────────────────────────────────────────────

// TestPipelineBindingRefusesTheSingleIDHelpers: ResourceID / WithResourceID are
// how kind-agnostic code asks "which row is this folder", and a pipeline folder
// has no answer to that question.
//
// The refusal is the whole point of the change: before it, both helpers were
// `if dataapp { … } else { workflow }`, so a pipeline folder would have READ an
// empty WorkflowID (indistinguishable from "never pushed here", which a push
// acts on by creating everything again) and WRITTEN a table id into a field
// nothing would ever look for.
func TestPipelineBindingRefusesTheSingleIDHelpers(t *testing.T) {
	b := Binding{FeatureID: "collection-1", Tables: map[string]string{"orders.sql": "table-1"}}

	if _, err := b.ResourceID(PipelineKind); err == nil {
		t.Error("ResourceID(PipelineKind) must refuse, not answer with WorkflowID")
	}
	got, err := b.WithResourceID(PipelineKind, "table-2")
	if err == nil {
		t.Error("WithResourceID(PipelineKind) must refuse")
	}
	if got.WorkflowID != "" || got.DataAppID != "" {
		t.Errorf("a refused WithResourceID must write nothing, got %+v", got)
	}
	// And the two kinds that DO have a single row still answer.
	if id, err := (Binding{WorkflowID: "wf-1"}).ResourceID(WorkflowKind); err != nil || id != "wf-1" {
		t.Errorf("ResourceID(WorkflowKind) = %q, %v", id, err)
	}
}

// TestBindingTablesRoundTrip: the Tables map is what a pipeline folder is bound
// BY, so it has to survive the committed file — and it must not appear at all
// in a workflow or data-app manifest.
func TestBindingTablesRoundTrip(t *testing.T) {
	root := t.TempDir()
	key := InstanceKey{URL: "https://cloud.ronja.tech", TenantID: "tenant-1"}
	m := &Manifest{Kind: KindPipeline, Title: "Sales"}
	m.SetBinding(key, Binding{
		FeatureID: "collection-9",
		Tables: map[string]string{
			"staging/orders.sql": "table-orders",
			"revenue.sql":        "table-revenue",
		},
	})
	if err := SaveManifest(root, m); err != nil {
		t.Fatal(err)
	}

	// The committed file is one people read and diff, so the key is asserted
	// literally rather than only through a decode.
	body, err := os.ReadFile(ManifestPath(root))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(body), `"tables"`) {
		t.Errorf("manifest is missing \"tables\":\n%s", body)
	}

	loaded, err := LoadManifest(root, PipelineKind)
	if err != nil {
		t.Fatal(err)
	}
	binding, ok, err := loaded.Binding(key)
	if err != nil || !ok {
		t.Fatalf("binding not found: ok=%v err=%v", ok, err)
	}
	if binding.FeatureID != "collection-9" {
		t.Errorf("featureID = %q", binding.FeatureID)
	}
	if len(binding.Tables) != 2 ||
		binding.Tables["staging/orders.sql"] != "table-orders" ||
		binding.Tables["revenue.sql"] != "table-revenue" {
		t.Errorf("tables did not round-trip: %+v", binding.Tables)
	}
}

// TestBindingTablesIsOmittedWhenEmpty: omitempty is what keeps a workflow's
// committed manifest byte-identical to what it was before pipelines existed.
func TestBindingTablesIsOmittedWhenEmpty(t *testing.T) {
	root := t.TempDir()
	key := InstanceKey{URL: "https://cloud.ronja.tech"}
	m := &Manifest{Kind: KindWorkflow, Title: "W", Entrypoint: "main.py"}
	m.SetBinding(key, Binding{WorkflowID: "wf-1", FeatureID: "collection-1"})
	if err := SaveManifest(root, m); err != nil {
		t.Fatal(err)
	}
	body, err := os.ReadFile(ManifestPath(root))
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(body), "tables") {
		t.Errorf("a workflow manifest must carry no \"tables\" key:\n%s", body)
	}
}

// TestWithTableCopiesTheMap: Binding is a value type with a reference field, so
// a WithTable that wrote through would reach into the Manifest entry the copy
// came from — recording a binding that SaveManifest never persisted.
func TestWithTableCopiesTheMap(t *testing.T) {
	original := Binding{Tables: map[string]string{"a.sql": "table-a"}}
	updated := original.WithTable("b.sql", "table-b")

	if _, leaked := original.Tables["b.sql"]; leaked {
		t.Error("WithTable wrote through into the receiver's map")
	}
	if updated.Tables["a.sql"] != "table-a" || updated.Tables["b.sql"] != "table-b" {
		t.Errorf("WithTable lost an entry: %+v", updated.Tables)
	}
	// A nil map is the ordinary first-push state and must not panic.
	fresh := Binding{}.WithTable("c.sql", "table-c")
	if fresh.Tables["c.sql"] != "table-c" {
		t.Errorf("WithTable on a nil map: %+v", fresh.Tables)
	}
}

// ── The baseline's table state ──────────────────────────────────────────────

// TestTableStateRoundTrip: push resumes its own draft from this rather than
// spending a round trip, so a state file that lost DraftID would silently make
// every push check out a second draft (which the server refuses).
func TestTableStateRoundTrip(t *testing.T) {
	root := t.TempDir()
	key := InstanceKey{URL: "https://cloud.ronja.tech", TenantID: "tenant-1"}
	s := &State{}
	s.Set(key, &InstanceState{
		SourceID: "collection-9",
		Files:    map[string]FileState{"revenue.sql": {SHA256: "abc"}},
		Tables: map[string]TableState{
			"revenue.sql": {TableID: "table-revenue", DraftID: "table-draft-1"},
			"orders.sql":  {TableID: "table-orders"},
		},
	})
	if err := SaveState(root, s); err != nil {
		t.Fatal(err)
	}

	loaded, err := LoadState(root)
	if err != nil {
		t.Fatal(err)
	}
	inst := loaded.For(key)
	if inst == nil {
		t.Fatal("baseline not found")
	}
	if got := inst.Tables["revenue.sql"]; got.TableID != "table-revenue" || got.DraftID != "table-draft-1" {
		t.Errorf("revenue.sql did not round-trip: %+v", got)
	}
	// An absent DraftID is the ordinary "no open draft" state, not a decode
	// failure — and it must come back empty rather than inheriting a neighbour's.
	if got := inst.Tables["orders.sql"]; got.TableID != "table-orders" || got.DraftID != "" {
		t.Errorf("orders.sql did not round-trip: %+v", got)
	}
}

// TestTableStateIsAbsentWhenUnmanaged: the three-state rule. An ABSENT "tables"
// key means "this folder does not track table state" — every workflow and
// data-app baseline, and any pipeline baseline written before the key existed —
// which is not the same claim as a present-but-empty map.
func TestTableStateIsAbsentWhenUnmanaged(t *testing.T) {
	root := t.TempDir()
	key := InstanceKey{URL: "https://cloud.ronja.tech"}
	s := &State{}
	s.Set(key, &InstanceState{SourceID: "wf-1", Files: map[string]FileState{"main.py": {SHA256: "x"}}})
	if err := SaveState(root, s); err != nil {
		t.Fatal(err)
	}
	body, err := os.ReadFile(StatePath(root))
	if err != nil {
		t.Fatal(err)
	}
	var shape struct {
		Instances []struct {
			State map[string]json.RawMessage `json:"state"`
		} `json:"instances"`
	}
	if err := json.Unmarshal(body, &shape); err != nil {
		t.Fatal(err)
	}
	if len(shape.Instances) != 1 {
		t.Fatalf("expected one baseline, got %d", len(shape.Instances))
	}
	if _, present := shape.Instances[0].State["tables"]; present {
		t.Errorf("a workflow baseline must carry no \"tables\" key:\n%s", body)
	}

	// And an unmanaged baseline reads back as nil, which is what lets a caller
	// tell "not tracked" from "tracked, and there are none".
	loaded, err := LoadState(root)
	if err != nil {
		t.Fatal(err)
	}
	if tables := loaded.For(key).Tables; tables != nil {
		t.Errorf("an unmanaged baseline must read back nil, got %+v", tables)
	}
}

// ── The syncable rule ───────────────────────────────────────────────────────

// TestPipelineWalkNeverReadsANonSQLFile: the .sql rule is applied INSIDE the
// walk, not after it.
//
// It is a cost property with a name: a pipeline folder is an ordinary repo
// directory, so it can hold a venv/ or a node_modules/, and hashing every file
// in one to throw the result away is minutes of I/O for a folder of six queries.
func TestPipelineWalkNeverReadsANonSQLFile(t *testing.T) {
	root := t.TempDir()
	write := func(path, body string) {
		t.Helper()
		if err := WriteFile(root, path, body); err != nil {
			t.Fatalf("write %s: %v", path, err)
		}
	}
	write("orders.sql", "SELECT 1")
	write("staging/orders_raw.sql", "SELECT 2")
	write("README.md", "# docs")
	write("venv/lib/pkg.py", "x = 1")

	enumeration, err := Enumerate(root, PipelineKind)
	if err != nil {
		t.Fatalf("enumerate: %v", err)
	}
	// A .sql file at DEPTH is still source: the extension rule must not be
	// implemented by pruning directories, which would hide the whole subtree.
	for _, want := range []string{"orders.sql", "staging/orders_raw.sql"} {
		if _, ok := enumeration.Files[want]; !ok {
			t.Errorf("%s is missing from the walk: %+v", want, enumeration.Files)
		}
	}
	for _, unwanted := range []string{"README.md", "venv/lib/pkg.py"} {
		if _, ok := enumeration.Files[unwanted]; ok {
			t.Errorf("%s was hashed by the walk: %+v", unwanted, enumeration.Files)
		}
	}

	// The same folder as a WORKFLOW takes everything, which is what makes this a
	// property of the Kind rather than of the walk.
	all, err := Enumerate(root, WorkflowKind)
	if err != nil {
		t.Fatalf("enumerate as a workflow: %v", err)
	}
	if _, ok := all.Files["README.md"]; !ok {
		t.Errorf("the .sql rule leaked into another kind: %+v", all.Files)
	}
}

// TestPipelineSyncableRuleIsTheSameOnBothSides: the walk and the layout check
// have to answer identically, or a clone writes a file the walk never returns —
// `status` invents a deletion and the next push acts on it by deleting the file
// server-side.
//
// This is the same agreement TestStructuralExclusionAndCheckLocalPathsAgree
// pins for the structural rules, asserted for the per-kind content rule that
// now travels beside them.
func TestPipelineSyncableRuleIsTheSameOnBothSides(t *testing.T) {
	for _, path := range []string{"notes.md", "Makefile", "queries/orders.txt", "orders.sql.bak"} {
		if err := ValidatePath(path); err != nil {
			t.Fatalf("ValidatePath(%q) = %v — the premise is that the SERVER allows these", path, err)
		}
		if NotSyncable(path, PipelineKind) == "" {
			t.Errorf("NotSyncable(%q) = \"\", want a reason", path)
		}
		if err := CheckLocalPaths([]string{"orders.sql", path}, PipelineKind); err == nil {
			t.Errorf("CheckLocalPaths accepted %q, which the walk will never return", path)
		}
	}
	// Case-insensitive: `.SQL` is an ordinary thing to find in a repo that has
	// been through Windows, and refusing to sync it while listing it nowhere is
	// the silent surprise Skipped exists to prevent.
	for _, path := range []string{"orders.sql", "staging/orders.SQL"} {
		if reason := NotSyncable(path, PipelineKind); reason != "" {
			t.Errorf("NotSyncable(%q) = %q, want it syncable", path, reason)
		}
		if err := CheckLocalPaths([]string{path}, PipelineKind); err != nil {
			t.Errorf("CheckLocalPaths(%q) = %v, want nil", path, err)
		}
	}
}
