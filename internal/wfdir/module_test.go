package wfdir

import (
	"strings"
	"testing"
)

// The folder-model half of the module loop. The COMMAND half lives in
// internal/commands/module_test.go; what is checked here is the thing a command
// cannot check for itself — that ONE Kind decides which files are syncable, and
// that both entry points (Enumerate and CheckLocalPaths) read it identically.
//
// That equivalence is not a tidiness property. A path one keeps and the other
// drops leaves a baseline claiming a file no later walk returns: `status`
// invents a deletion and the next push acts on it by deleting the file
// server-side. See the doc on Kind.

// TestModuleKindSyncsPythonOnly pins the .py restriction at the level that
// enforces it — the Kind — rather than at each caller.
func TestModuleKindSyncsPythonOnly(t *testing.T) {
	for _, tc := range []struct {
		path string
		want bool
	}{
		{"__init__.py", true},
		{"tracker.py", true},
		{"sub/helpers.py", true},
		// Case-insensitive, because `.PY` is an ordinary thing to find in a repo
		// that has been through Windows.
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

// TestModuleEnumerateSkipsNonPython is the walk half of the rule, and it also
// pins that the skips are REPORTED: a .md file the author believes is part of
// the module and which the CLI silently ignores is the worst kind of surprise.
func TestModuleEnumerateSkipsNonPython(t *testing.T) {
	root := t.TempDir()
	write(t, root, "__init__.py", "")
	write(t, root, "tracker.py", "x = 1")
	write(t, root, "sub/nested.py", "y = 2")
	write(t, root, "README.md", "# docs")
	write(t, root, "fixtures/data.csv", "a,b\n")
	// Compiled output: excluded by EXTENSION, not by a __pycache__ SkipDirs
	// entry — which is why ModuleKind declares none. Worth pinning, because
	// adding a SkipDirs entry later would be a second rule saying the same
	// thing, and the two could disagree.
	write(t, root, "__pycache__/tracker.cpython-311.pyc", "\x00\x01")
	// Folder machinery: never syncable, never reported.
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

// TestModuleCheckLocalPathsRefusesNonPython is the OTHER side of the same rule,
// and the one that actually protects a file. A `.md` the server somehow holds
// would be written by clone once and then invisible to every walk — a phantom
// deletion the next push acts on. The two entry points must answer identically.
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

// TestLoadManifestRefusesWrongKindForModule pins the guard that stops `ronja
// module push` overwriting a workflow's Python with a module's — which is
// exactly the shape it would destroy work in, since both kinds are .py files
// plus a ronja.json and are otherwise indistinguishable.
func TestLoadManifestRefusesWrongKindForModule(t *testing.T) {
	root := t.TempDir()
	write(t, root, ManifestName, `{"kind":"workflow","title":"Report","instances":[]}`)
	_, err := LoadManifest(root, ModuleKind)
	if err == nil {
		t.Fatal("expected a workflow folder to be refused by a module command")
	}
	// Names the command that WOULD work: a wrong kind is almost always the right
	// folder and the wrong verb.
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
	// A module has no entrypoint, and ModuleKind's empty DefaultEntrypoint is
	// what keeps LoadManifest from inventing "main.py" for it.
	if m.Entrypoint != "" {
		t.Fatalf("entrypoint = %q, want empty — a module has nothing to run", m.Entrypoint)
	}
}

// TestModuleBindingRoundTrip pins the third id field: a module's binding must
// not be answered with (or written into) a workflow's, which is the silent
// wrong answer Binding.ResourceID exists to make unrepresentable.
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

// TestLockModuleRebindDropsAnchor pins the invariant on LockStack.HeadVersionID
// for the new field: a pointer is only ever compared against the row it was
// read from, so a stack repointed at a different module drops it. Kept, it
// would be a version id read from one module and compared against another's
// history — a guard that refuses every push and names a version nobody
// recognises.
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
	// And the binding itself round-trips through the lock, which is what a stack
	// folder's push reads instead of instances[].
	if got := l.Binding("dev").ModuleID; got != "module-2" {
		t.Fatalf("Lock.Binding ModuleID = %q, want module-2", got)
	}
}
