package wfdir

import (
	"strings"
	"testing"
)

func TestModuleKindSyncsPythonOnly(t *testing.T) {
	for _, tc := range []struct {
		path string
		want bool
	}{
		{"__init__.py", true},
		{"tracker.py", true},
		{"sub/helpers.py", true},
		{"Tracker.PY", true},
		{"README.md", false},
		{"data.csv", false},
		{"tracker.pyc", false},
		{"Makefile", false},
	} {
		if got := ModuleKind.Syncable(tc.path); got != tc.want {
			t.Errorf("ModuleKind.Syncable(%q) = %v, want %v", tc.path, got, tc.want)
		}
	}
}

func TestModuleEnumerateSkipsNonPython(t *testing.T) {
	root := t.TempDir()
	write(t, root, "__init__.py", "")
	write(t, root, "tracker.py", "x = 1")
	write(t, root, "sub/nested.py", "y = 2")
	write(t, root, "README.md", "# docs")
	write(t, root, "fixtures/data.csv", "a,b\n")
	// Excluded by extension, not by a __pycache__ SkipDirs entry, which would be
	// a second rule that could disagree.
	write(t, root, "__pycache__/tracker.cpython-311.pyc", "\x00\x01")
	write(t, root, ManifestName, `{"kind":"module"}`)
	write(t, root, LockName, `{"stacks":{}}`)
	write(t, root, StateDirName+"/"+StateFileName, `{}`)

	e, err := Enumerate(root, ModuleKind)
	if err != nil {
		t.Fatalf("enumerate: %v", err)
	}
	want := map[string]string{
		"__init__.py":   HashString(""),
		"tracker.py":    HashString("x = 1"),
		"sub/nested.py": HashString("y = 2"),
	}
	if len(e.Files) != len(want) {
		t.Fatalf("files = %v, want %v", e.Files, want)
	}
	for path, hash := range want {
		if e.Files[path] != hash {
			t.Errorf("files[%q] = %q, want %q", path, e.Files[path], hash)
		}
	}
	skipped := map[string]string{}
	for _, s := range e.Skipped {
		skipped[s.Path] = s.Reason
	}
	for _, path := range []string{"README.md", "fixtures/data.csv", "__pycache__/tracker.cpython-311.pyc"} {
		if _, ok := skipped[path]; !ok {
			t.Errorf("%s was dropped without being reported (skipped: %v)", path, skipped)
		}
	}
	for _, path := range []string{ManifestName, LockName} {
		if _, ok := skipped[path]; ok {
			t.Errorf("%s is the folder's own machinery and must not be reported as the user's mistake", path)
		}
	}
}

// A `.md` the server somehow holds would be cloned once and then read as a
// phantom deletion the next push acts on.
func TestModuleCheckLocalPathsRefusesNonPython(t *testing.T) {
	err := CheckLocalPaths([]string{"__init__.py", "notes.md"}, ModuleKind)
	if err == nil {
		t.Fatal("expected a non-.py remote path to be refused for a module folder")
	}
	if !strings.Contains(err.Error(), "notes.md") {
		t.Fatalf("the refusal must name the offending path, got: %v", err)
	}
	if err := CheckLocalPaths([]string{"__init__.py", "sub/tracker.py"}, ModuleKind); err != nil {
		t.Fatalf("a .py-only set must be layable out: %v", err)
	}
}

// A workflow folder and a module folder are both .py files plus a ronja.json,
// so the manifest kind is all that stops one overwriting the other.
func TestLoadManifestRefusesWrongKindForModule(t *testing.T) {
	root := t.TempDir()
	write(t, root, ManifestName, `{"kind":"workflow","title":"Report","instances":[]}`)
	_, err := LoadManifest(root, ModuleKind)
	if err == nil {
		t.Fatal("expected a workflow folder to be refused by a module command")
	}
	// A wrong kind is almost always the right folder and the wrong verb.
	if !strings.Contains(err.Error(), WorkflowKind.Command) {
		t.Fatalf("the refusal must name %q, got: %v", WorkflowKind.Command, err)
	}

	moduleRoot := t.TempDir()
	write(t, moduleRoot, ManifestName, `{"kind":"module","name":"emab_tracker","title":"Tracker","instances":[]}`)
	if _, err := LoadManifest(moduleRoot, WorkflowKind); err == nil {
		t.Fatal("expected a module folder to be refused by a workflow command")
	} else if !strings.Contains(err.Error(), ModuleKind.Command) {
		t.Fatalf("the refusal must name %q, got: %v", ModuleKind.Command, err)
	}

	m, err := LoadManifest(moduleRoot, ModuleKind)
	if err != nil {
		t.Fatalf("a module folder must open for a module command: %v", err)
	}
	if m.Name != "emab_tracker" {
		t.Fatalf("name = %q, want emab_tracker", m.Name)
	}
	// ModuleKind's empty DefaultEntrypoint keeps LoadManifest from inventing "main.py".
	if m.Entrypoint != "" {
		t.Fatalf("entrypoint = %q, want empty — a module has nothing to run", m.Entrypoint)
	}
}

func TestModuleBindingRoundTrip(t *testing.T) {
	b, err := Binding{}.WithResourceID(ModuleKind, "module-1")
	if err != nil {
		t.Fatalf("WithResourceID: %v", err)
	}
	if b.ModuleID != "module-1" {
		t.Fatalf("ModuleID = %q, want module-1", b.ModuleID)
	}
	if b.WorkflowID != "" || b.DataAppID != "" {
		t.Fatalf("a module id must not land in another kind's field: %+v", b)
	}
	got, err := b.ResourceID(ModuleKind)
	if err != nil {
		t.Fatalf("ResourceID: %v", err)
	}
	if got != "module-1" {
		t.Fatalf("ResourceID = %q, want module-1", got)
	}
	if id, err := b.ResourceID(WorkflowKind); err != nil || id != "" {
		t.Fatalf("a module binding must read as unbound for a workflow, got (%q, %v)", id, err)
	}
}

// A head version is only ever compared against the row it was read from; kept
// across a rebind, it would refuse every push to the new module.
func TestLockModuleRebindDropsAnchor(t *testing.T) {
	l := &Lock{}
	l.SetBinding("dev", Binding{ModuleID: "module-1"})
	l.SetHeadVersion("dev", "cmv-1")
	if got := l.HeadVersion("dev"); got != "cmv-1" {
		t.Fatalf("HeadVersion = %q, want cmv-1", got)
	}
	l.SetBinding("dev", Binding{ModuleID: "module-2"})
	if got := l.HeadVersion("dev"); got != "" {
		t.Fatalf("rebinding to another module must clear the anchor, got %q", got)
	}
	if got := l.Binding("dev").ModuleID; got != "module-2" {
		t.Fatalf("Lock.Binding ModuleID = %q, want module-2", got)
	}
}
