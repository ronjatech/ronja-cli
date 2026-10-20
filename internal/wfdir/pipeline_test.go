package wfdir

import (
	"encoding/json"
	"os"
	"strings"
	"testing"
)

// ── The kind registry ───────────────────────────────────────────────────────

// Iterates the registry rather than a literal list, so a new kindByName entry
// is checked the moment it is added.
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

func TestLoadManifestRoutesEveryWrongKindToTheRightCommand(t *testing.T) {
	// From the registry, for TestEveryKindNamesItsOwnCommand's reason.
	var kinds []Kind
	for _, kind := range kindByName {
		kinds = append(kinds, kind)
	}
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
			if strings.Contains(err.Error(), "use `"+openedAs.Command+"`") {
				t.Errorf("a %s folder opened as %s points back at %q, the command that refused it: %v",
					onDisk.Name, openedAs.Name, openedAs.Command, err)
			}
		}
	}
}

// A manifest with no `kind` is hand-written or truncated, so the fix-it line
// names the command the caller ran.
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

// A pipeline folder has no single row: an answer would be an empty WorkflowID,
// which a push reads as "never pushed here" and acts on by creating everything again.
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
	if id, err := (Binding{WorkflowID: "wf-1"}).ResourceID(WorkflowKind); err != nil || id != "wf-1" {
		t.Errorf("ResourceID(WorkflowKind) = %q, %v", id, err)
	}
}

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

// omitempty keeps a workflow's committed manifest free of a pipeline-only key.
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

// Binding is a value type with a map field, so a WithTable that wrote through
// would reach into the Manifest entry the copy came from.
func TestWithTableCopiesTheMap(t *testing.T) {
	original := Binding{Tables: map[string]string{"a.sql": "table-a"}}
	updated := original.WithTable("b.sql", "table-b")

	if _, leaked := original.Tables["b.sql"]; leaked {
		t.Error("WithTable wrote through into the receiver's map")
	}
	if updated.Tables["a.sql"] != "table-a" || updated.Tables["b.sql"] != "table-b" {
		t.Errorf("WithTable lost an entry: %+v", updated.Tables)
	}
	// A nil map is the ordinary first-push state.
	fresh := Binding{}.WithTable("c.sql", "table-c")
	if fresh.Tables["c.sql"] != "table-c" {
		t.Errorf("WithTable on a nil map: %+v", fresh.Tables)
	}
}

// ── The baseline's table state ──────────────────────────────────────────────

// Push resumes its own draft from DraftID; losing it would make every push
// check out a second draft, which the server refuses.
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
	// An absent DraftID must come back empty rather than inheriting a neighbour's.
	if got := inst.Tables["orders.sql"]; got.TableID != "table-orders" || got.DraftID != "" {
		t.Errorf("orders.sql did not round-trip: %+v", got)
	}
}

// An absent "tables" key means "not tracked", a different claim from a
// present-but-empty map.
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

	loaded, err := LoadState(root)
	if err != nil {
		t.Fatal(err)
	}
	if tables := loaded.For(key).Tables; tables != nil {
		t.Errorf("an unmanaged baseline must read back nil, got %+v", tables)
	}
}

// ── The syncable rule ───────────────────────────────────────────────────────

// The .sql rule applies inside the walk, not after it: a pipeline folder can
// hold a venv/ or node_modules/, and hashing them to discard is minutes of I/O.
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
	// The extension rule must not prune directories, which would hide nested .sql.
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

	all, err := Enumerate(root, WorkflowKind)
	if err != nil {
		t.Fatalf("enumerate as a workflow: %v", err)
	}
	if _, ok := all.Files["README.md"]; !ok {
		t.Errorf("the .sql rule leaked into another kind: %+v", all.Files)
	}
}

// A clone of a file the walk never returns becomes a phantom deletion the next
// push performs server-side.
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
	for _, path := range []string{"orders.sql", "staging/orders.SQL"} {
		if reason := NotSyncable(path, PipelineKind); reason != "" {
			t.Errorf("NotSyncable(%q) = %q, want it syncable", path, reason)
		}
		if err := CheckLocalPaths([]string{path}, PipelineKind); err != nil {
			t.Errorf("CheckLocalPaths(%q) = %v, want nil", path, err)
		}
	}
}
