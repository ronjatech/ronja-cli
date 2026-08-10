package wfdir

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/ronjatech/ronja-cli/internal/api"
)

// write is a test helper for laying out a folder.
func write(t *testing.T, root, rel, body string) {
	t.Helper()
	full := filepath.Join(root, filepath.FromSlash(rel))
	if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
		t.Fatalf("mkdir for %s: %v", rel, err)
	}
	if err := os.WriteFile(full, []byte(body), 0o644); err != nil {
		t.Fatalf("write %s: %v", rel, err)
	}
}

// The three states of Manifest.Parameters, which the pointer exists to keep
// apart. Collapsing "not managed" into "declares none" would make the first push
// from any pre-parameters folder CLEAR the workflow's declaration, so the
// absent case surviving a load/save round trip is the guarantee that matters.
func TestManifestParametersHaveThreeStates(t *testing.T) {
	t.Run("absent stays absent", func(t *testing.T) {
		root := t.TempDir()
		write(t, root, ManifestName, `{"kind":"workflow","title":"T","entrypoint":"main.py","instances":[]}`)
		m, err := LoadManifest(root, WorkflowKind)
		if err != nil {
			t.Fatalf("load: %v", err)
		}
		if m.ManagesParameters() {
			t.Fatalf("a manifest with no parameters key reports as managing them")
		}
		// And a save must not invent the key, or one round trip through any
		// command would silently opt the folder in.
		if err := SaveManifest(root, m); err != nil {
			t.Fatalf("save: %v", err)
		}
		raw, err := os.ReadFile(ManifestPath(root))
		if err != nil {
			t.Fatalf("read back: %v", err)
		}
		if strings.Contains(string(raw), "parameters") {
			t.Errorf("save added a parameters key to an unmanaged manifest: %s", raw)
		}
	})

	t.Run("empty list is managed and survives", func(t *testing.T) {
		root := t.TempDir()
		write(t, root, ManifestName, `{"kind":"workflow","title":"T","entrypoint":"main.py","parameters":[],"instances":[]}`)
		m, err := LoadManifest(root, WorkflowKind)
		if err != nil {
			t.Fatalf("load: %v", err)
		}
		if !m.ManagesParameters() {
			t.Fatalf("an explicit [] reports as unmanaged")
		}
		if got := m.DeclaredParameters(); len(got) != 0 {
			t.Errorf("declared = %+v, want none", got)
		}
		if err := SaveManifest(root, m); err != nil {
			t.Fatalf("save: %v", err)
		}
		again, err := LoadManifest(root, WorkflowKind)
		if err != nil {
			t.Fatalf("reload: %v", err)
		}
		if !again.ManagesParameters() {
			t.Errorf("the empty declaration was lost on a round trip")
		}
	})

	t.Run("declared set round trips", func(t *testing.T) {
		root := t.TempDir()
		m := &Manifest{Kind: KindWorkflow, Title: "T", Entrypoint: "main.py"}
		m.SetParameters([]api.WorkflowParameter{{Name: "upto", Type: "number", Required: true}})
		if err := SaveManifest(root, m); err != nil {
			t.Fatalf("save: %v", err)
		}
		again, err := LoadManifest(root, WorkflowKind)
		if err != nil {
			t.Fatalf("reload: %v", err)
		}
		got := again.DeclaredParameters()
		if len(got) != 1 || got[0].Name != "upto" || got[0].Type != "number" || !got[0].Required {
			t.Errorf("declared = %+v, want the parameter as written", got)
		}
	})

	// SetParameters(nil) means "managed, declaring none" — the state `wf init`
	// puts a fresh folder in — not "stop managing".
	t.Run("SetParameters(nil) marks it managed", func(t *testing.T) {
		m := &Manifest{Kind: KindWorkflow}
		m.SetParameters(nil)
		if !m.ManagesParameters() {
			t.Errorf("SetParameters(nil) left the folder unmanaged")
		}
	})
}

func TestManifestRoundTrip(t *testing.T) {
	root := t.TempDir()
	in := &Manifest{
		Kind:       KindWorkflow,
		Title:      "Monthly report",
		Entrypoint: "main.py",
		Instances: []Instance{
			{URL: "http://localhost:8080", TenantID: "ten-dev", Binding: Binding{FeatureID: "feat-local"}},
			{URL: "https://app.ronja.tech", TenantID: "ten-acme", Binding: Binding{WorkflowID: "wf-1", FeatureID: "feat-1"}},
		},
	}
	if err := SaveManifest(root, in); err != nil {
		t.Fatalf("save: %v", err)
	}
	out, err := LoadManifest(root, WorkflowKind)
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	if !reflect.DeepEqual(in, out) {
		t.Fatalf("round trip mismatch:\n in: %+v\nout: %+v", in, out)
	}

	// A binding that has never been pushed carries a feature but no workflow —
	// the state `wf init` leaves behind, and the one push must treat as "create
	// it" rather than "look it up".
	local, ok, err := out.Binding(InstanceKey{URL: "http://localhost:8080", TenantID: "ten-dev"})
	if err != nil || !ok {
		t.Fatalf("expected a binding for the local instance: ok=%v err=%v", ok, err)
	}
	if local.WorkflowID != "" || local.FeatureID != "feat-local" {
		t.Fatalf("unexpected unpushed binding: %+v", local)
	}
	if _, ok, _ := out.Binding(InstanceKey{URL: "https://staging.example", TenantID: "ten-dev"}); ok {
		t.Fatal("an unbound host must report absent, not zero-valued")
	}
	// The organization is half the key: the right instance under the wrong
	// organization is NOT this folder's binding, and adopting it would send one
	// organization's workflow id under the other's credential.
	if _, ok, _ := out.Binding(InstanceKey{URL: "https://app.ronja.tech", TenantID: "ten-northwind"}); ok {
		t.Fatal("a different organization on the same instance must not match")
	}
	// workflowID is omitempty, so an unpushed binding must not serialize one.
	raw, err := os.ReadFile(ManifestPath(root))
	if err != nil {
		t.Fatalf("read manifest: %v", err)
	}
	var shape struct {
		Instances []map[string]any `json:"instances"`
	}
	if err := json.Unmarshal(raw, &shape); err != nil {
		t.Fatalf("parse manifest: %v", err)
	}
	if _, present := shape.Instances[0]["workflowID"]; present {
		t.Fatal("an unpushed binding must omit workflowID entirely")
	}
}

func TestLoadManifestRefusesUnknownKind(t *testing.T) {
	root := t.TempDir()
	write(t, root, ManifestName, `{"kind":"dataapp","title":"x","instances":[]}`)
	if _, err := LoadManifest(root, WorkflowKind); err == nil {
		t.Fatal("expected a non-workflow manifest to be refused")
	}
	// A future `ronja app` folder must not be silently treated as a workflow.
	write(t, root, ManifestName, `{"title":"x"}`)
	_, err := LoadManifest(root, WorkflowKind)
	if err == nil {
		t.Fatal("expected a kindless manifest to be refused")
	}
	// An ABSENT kind gets its own message: `describes a "" folder` reads like a
	// bug in the CLI rather than a field the manifest is missing.
	if !strings.Contains(err.Error(), "has no kind") {
		t.Errorf("kindless manifest error = %v, want one saying the kind is missing", err)
	}
}

func TestLoadManifestDefaultsEntrypoint(t *testing.T) {
	root := t.TempDir()
	write(t, root, ManifestName, `{"kind":"workflow","title":"x"}`)
	m, err := LoadManifest(root, WorkflowKind)
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	if m.Entrypoint != DefaultEntrypoint {
		t.Fatalf("entrypoint = %q, want %q", m.Entrypoint, DefaultEntrypoint)
	}
	if len(m.Instances) != 0 {
		t.Fatalf("instances = %+v, want none for a manifest that declares none", m.Instances)
	}
}

func TestFindRootWalksUp(t *testing.T) {
	root := t.TempDir()
	write(t, root, ManifestName, `{"kind":"workflow","title":"x"}`)
	deep := filepath.Join(root, "lib", "nested")
	if err := os.MkdirAll(deep, 0o755); err != nil {
		t.Fatal(err)
	}
	found, err := FindRoot(deep)
	if err != nil {
		t.Fatalf("find: %v", err)
	}
	// The temp dir may be a symlink (/var -> /private/var on macOS), so compare
	// resolved paths rather than the raw strings.
	wantResolved, _ := filepath.EvalSymlinks(root)
	gotResolved, _ := filepath.EvalSymlinks(found)
	if gotResolved != wantResolved {
		t.Fatalf("root = %s, want %s", gotResolved, wantResolved)
	}

	if _, err := FindRoot(t.TempDir()); !errors.Is(err, ErrNoManifest) {
		t.Fatalf("expected ErrNoManifest, got %v", err)
	}
}

func TestStateRoundTrip(t *testing.T) {
	root := t.TempDir()
	s := &State{}
	key := InstanceKey{URL: "https://app.ronja.tech", TenantID: "ten-acme"}
	s.Set(key, &InstanceState{
		SourceID:          "wf-draft-1",
		SourceLifecycle:   "draft",
		BaselineUpdatedAt: "2026-07-28T10:00:00Z",
		Title:             "Monthly report",
		Entrypoint:        "main.py",
		Files: map[string]FileState{
			"main.py": {SHA256: HashString("print(1)"), UpdatedAt: "2026-07-28T10:00:00Z"},
		},
	})
	if err := SaveState(root, s); err != nil {
		t.Fatalf("save: %v", err)
	}
	out, err := LoadState(root)
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	if !reflect.DeepEqual(s, out) {
		t.Fatalf("round trip mismatch:\n in: %+v\nout: %+v", s.Instances, out.Instances)
	}
	unknown := InstanceKey{URL: "https://nowhere.example", TenantID: "ten-acme"}
	if out.For(unknown) != nil {
		t.Fatal("an unknown host must have no baseline")
	}
	// A nil baseline must still be diffable — that is the every-file-is-new case.
	if got := len(out.For(unknown).Hashes()); got != 0 {
		t.Fatalf("nil baseline hashes = %d, want 0", got)
	}

	// The baseline is per-user and must never reach the customer's git.
	ignore, err := os.ReadFile(filepath.Join(root, StateDirName, GitignoreName))
	if err != nil {
		t.Fatalf("read .ronja/.gitignore: %v", err)
	}
	if string(ignore) != "*\n" {
		t.Fatalf(".gitignore = %q, want %q", string(ignore), "*\n")
	}
}

// Clear is per-binding, which is the whole reason it exists: a folder bound to
// staging and production keeps a baseline each, and dropping one must leave the
// other exactly as it was. Wiping both is not a cosmetic slip — an absent
// baseline is indistinguishable from "never synced here", so the next push
// against a non-empty remote trips the drift guard and offers --force.
func TestStateClearLeavesOtherBindingsAlone(t *testing.T) {
	s := &State{}
	staging := InstanceKey{URL: "https://staging.ronja.tech", TenantID: "ten-acme"}
	prod := InstanceKey{URL: "https://app.ronja.tech", TenantID: "ten-acme"}
	s.Set(staging, &InstanceState{SourceID: "wf-staging-1"})
	s.Set(prod, &InstanceState{SourceID: "wf-prod-1", Files: map[string]FileState{
		"main.py": {SHA256: HashString("print(1)")},
	}})

	s.Clear(staging)

	if got := s.For(staging); got != nil {
		// nil, not an empty entry: that is what "never synced here" looks like,
		// and it is what this instance now is.
		t.Errorf("cleared baseline = %+v, want nil", got)
	}
	kept := s.For(prod)
	if kept == nil || kept.SourceID != "wf-prod-1" || kept.Files["main.py"].SHA256 != HashString("print(1)") {
		t.Errorf("production's baseline was disturbed: %+v", kept)
	}
	if len(s.Instances) != 1 {
		t.Errorf("instances = %d, want the one surviving binding", len(s.Instances))
	}

	// Clearing a binding that has no baseline is a no-op, not a panic — a
	// discard can perfectly well run in a folder that never synced here.
	s.Clear(InstanceKey{URL: "https://nowhere.example", TenantID: "ten-acme"})
	if len(s.Instances) != 1 {
		t.Errorf("instances = %d after clearing an absent key, want 1", len(s.Instances))
	}
}

// A state file written before the metadata fields existed has to keep parsing:
// they are additive, and a folder that has been synced for months must not start
// failing to load because the CLI learned to record two more things about the
// row. An absent title reads as "not recorded", which the push's metadata guard
// treats as no guard rather than as an empty title.
func TestLoadStateWithoutMetadataStillParses(t *testing.T) {
	root := t.TempDir()
	if err := os.MkdirAll(filepath.Join(root, StateDirName), 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	legacy := `{"instances":[{"url":"https://app.ronja.tech","tenantID":"ten-acme","state":{"sourceID":"wf-1","files":{"main.py":{"sha256":"abc"}}}}]}`
	if err := os.WriteFile(StatePath(root), []byte(legacy), 0o644); err != nil {
		t.Fatalf("write: %v", err)
	}

	out, err := LoadState(root)
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	inst := out.For(InstanceKey{URL: "https://app.ronja.tech", TenantID: "ten-acme"})
	if inst == nil || inst.SourceID != "wf-1" || inst.Files["main.py"].SHA256 != "abc" {
		t.Fatalf("baseline = %+v", inst)
	}
	if inst.Title != "" || inst.Entrypoint != "" {
		t.Errorf("metadata = %q/%q, want both absent", inst.Title, inst.Entrypoint)
	}
}

func TestLoadStateMissingIsEmpty(t *testing.T) {
	// A colleague who cloned the repo from git has ronja.json and no .ronja/.
	out, err := LoadState(t.TempDir())
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	if len(out.Instances) != 0 {
		t.Fatalf("expected an empty state, got %+v", out.Instances)
	}
}

func TestWriteAtomicLeavesNoDebris(t *testing.T) {
	root := t.TempDir()
	if err := writeAtomic(filepath.Join(root, "f.json"), []byte("first"), 0o644); err != nil {
		t.Fatalf("write: %v", err)
	}
	// Overwriting must replace in place, not append or fail, and must not leave
	// the temp file behind for the enumerator to pick up later.
	if err := writeAtomic(filepath.Join(root, "f.json"), []byte("second"), 0o644); err != nil {
		t.Fatalf("rewrite: %v", err)
	}
	body, err := os.ReadFile(filepath.Join(root, "f.json"))
	if err != nil {
		t.Fatal(err)
	}
	if string(body) != "second" {
		t.Fatalf("content = %q, want %q", string(body), "second")
	}
	entries, err := os.ReadDir(root)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 1 {
		t.Fatalf("expected exactly one file, got %d", len(entries))
	}
}

func TestValidatePath(t *testing.T) {
	tests := []struct {
		name string
		path string
		ok   bool
	}{
		{name: "simple", path: "main.py", ok: true},
		{name: "nested", path: "lib/helpers.py", ok: true},
		{name: "dashes and underscores", path: "lib/my-helper_2.py", ok: true},
		{name: "max depth", path: "a/b/c/d/e/f/g/h.py", ok: true},
		{name: "empty", path: ""},
		{name: "leading slash", path: "/main.py"},
		{name: "trailing slash", path: "lib/"},
		{name: "double slash", path: "lib//x.py"},
		{name: "traversal", path: "../secrets.py"},
		{name: "traversal mid-path", path: "lib/../../x.py"},
		{name: "encoded traversal", path: "lib/%2e%2e/x.py"},
		{name: "space", path: "my script.py"},
		{name: "unicode", path: "rapport-över.py"},
		{name: "too deep", path: "a/b/c/d/e/f/g/h/i.py"},
		{name: "null byte", path: "main\x00.py"},
		{name: "empty segment", path: "lib/"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			err := ValidatePath(tc.path)
			if tc.ok && err != nil {
				t.Fatalf("ValidatePath(%q) = %v, want ok", tc.path, err)
			}
			if !tc.ok && err == nil {
				t.Fatalf("ValidatePath(%q) = nil, want an error", tc.path)
			}
		})
	}
	long := ""
	for len(long) <= MaxPathLength {
		long += "a"
	}
	if err := ValidatePath(long); err == nil {
		t.Fatal("expected an over-long path to be refused")
	}
}

func TestEnumerate(t *testing.T) {
	root := t.TempDir()
	write(t, root, "main.py", "print('hi')")
	write(t, root, "lib/helpers.py", "x = 1")
	write(t, root, "README.md", "# docs")
	// Folder machinery: never syncable, never reported.
	write(t, root, ManifestName, `{"kind":"workflow"}`)
	write(t, root, StateDirName+"/"+StateFileName, `{}`)
	write(t, root, StateDirName+"/"+GitignoreName, "*\n")
	// Dot-directories are skipped wholesale — a repo's .git is enormous and
	// full of paths the server would reject — but REPORTED, once, by directory.
	write(t, root, ".git/objects/ab/cdef", "binary")
	// A dotfile is reported too: the server's path grammar would accept
	// ".env" perfectly happily, so a user who thinks it is part of the
	// workflow has to be told it is not rather than left to discover it as a
	// phantom deletion.
	write(t, root, ".env", "SECRET=1")
	// Path-invalid: reported, because a file the user believes is part of the
	// workflow and which we silently ignore is the worst kind of surprise.
	write(t, root, "my script.py", "x = 2")

	e, err := Enumerate(root, WorkflowKind)
	if err != nil {
		t.Fatalf("enumerate: %v", err)
	}
	want := map[string]string{
		"main.py":        HashString("print('hi')"),
		"lib/helpers.py": HashString("x = 1"),
		"README.md":      HashString("# docs"),
	}
	if !reflect.DeepEqual(e.Files, want) {
		t.Fatalf("files = %v, want %v", e.Files, want)
	}
	// Every exclusion but the folder's own machinery is named. The dot-dir is
	// ONE entry, not one per object inside it.
	gotSkipped := map[string]string{}
	for _, s := range e.Skipped {
		gotSkipped[s.Path] = s.Reason
	}
	for _, path := range []string{".env", ".git/", "my script.py"} {
		if _, ok := gotSkipped[path]; !ok {
			t.Errorf("skipped = %+v, want it to name %s", e.Skipped, path)
		}
	}
	if len(gotSkipped) != 3 {
		t.Fatalf("skipped = %+v, want exactly .env, .git/ and my script.py (ronja.json and .ronja/ are the folder's own machinery)", e.Skipped)
	}
	if !strings.Contains(gotSkipped[".env"], "dot-name") {
		t.Errorf("reason for .env = %q, want it to say why", gotSkipped[".env"])
	}
}

// The rule Enumerate applies by shape and the rule CheckLocalPaths refuses by
// have to be THE SAME rule. A path one excludes and the other accepts is the
// phantom-deletion bug: clone writes the file, the walk never sees it again,
// status reports it deleted and push deletes it server-side.
func TestStructuralExclusionAndCheckLocalPathsAgree(t *testing.T) {
	excluded := []string{
		".env",
		".config/settings.py",
		"lib/.hidden.py",
		"a/b/.c/d.py",
		ManifestName,
		StateDirName + "/" + StateFileName,
	}
	for _, path := range excluded {
		t.Run(path, func(t *testing.T) {
			// The server would accept every one of these: `.` is a legal
			// segment character. That is precisely why the CLI has to refuse.
			if err := ValidatePath(path); err != nil {
				t.Fatalf("ValidatePath(%q) = %v, want nil — the premise of this test is that the SERVER allows these", path, err)
			}
			if StructuralExclusion(path, WorkflowKind) == "" {
				t.Fatalf("StructuralExclusion(%q) = \"\", want a reason", path)
			}
			err := CheckLocalPaths([]string{"main.py", path}, WorkflowKind)
			if err == nil {
				t.Fatalf("CheckLocalPaths accepted %q, which Enumerate will never return", path)
			}
			if !strings.Contains(err.Error(), path) {
				t.Errorf("error does not name the offending path: %v", err)
			}
			if !strings.Contains(err.Error(), "web builder") {
				t.Errorf("error gives the caller no way forward: %v", err)
			}
		})
	}
	for _, path := range []string{"main.py", "lib/helpers.py", "a.b.py", "README.md"} {
		if reason := StructuralExclusion(path, WorkflowKind); reason != "" {
			t.Errorf("StructuralExclusion(%q) = %q, want it syncable", path, reason)
		}
		if err := CheckLocalPaths([]string{path}, WorkflowKind); err != nil {
			t.Errorf("CheckLocalPaths(%q) = %v, want nil", path, err)
		}
	}
}

func TestDiffHashes(t *testing.T) {
	baseline := map[string]string{
		"main.py":        "aaa",
		"lib/helpers.py": "bbb",
		"gone.py":        "ccc",
	}
	current := map[string]string{
		"main.py":        "aaa", // unchanged
		"lib/helpers.py": "zzz", // modified
		"new.py":         "ddd", // added
	}
	d := DiffHashes(current, baseline)
	if !reflect.DeepEqual(d.Added, []string{"new.py"}) {
		t.Fatalf("added = %v", d.Added)
	}
	if !reflect.DeepEqual(d.Modified, []string{"lib/helpers.py"}) {
		t.Fatalf("modified = %v", d.Modified)
	}
	if !reflect.DeepEqual(d.Deleted, []string{"gone.py"}) {
		t.Fatalf("deleted = %v", d.Deleted)
	}
	if d.Unchanged != 1 {
		t.Fatalf("unchanged = %d, want 1", d.Unchanged)
	}
	if !d.Dirty() || d.Total() != 3 {
		t.Fatalf("dirty=%v total=%d, want true/3", d.Dirty(), d.Total())
	}

	clean := DiffHashes(baseline, baseline)
	if clean.Dirty() || clean.Unchanged != 3 {
		t.Fatalf("identical maps must be clean: %+v", clean)
	}
	// Empty lists rather than nil, so --json emits [] and a consumer can index
	// without a null check.
	if clean.Added == nil || clean.Modified == nil || clean.Deleted == nil {
		t.Fatal("diff lists must be empty, not nil")
	}
}

func TestWriteFileRefusesEscape(t *testing.T) {
	root := t.TempDir()
	if err := WriteFile(root, "../escaped.py", "x"); err == nil {
		t.Fatal("expected a traversal path to be refused")
	}
	if _, err := os.Stat(filepath.Join(filepath.Dir(root), "escaped.py")); err == nil {
		t.Fatal("a file was written outside the folder")
	}
	if err := WriteFile(root, "lib/deep/ok.py", "x"); err != nil {
		t.Fatalf("write nested: %v", err)
	}
	if _, err := os.Stat(filepath.Join(root, "lib", "deep", "ok.py")); err != nil {
		t.Fatalf("nested file missing: %v", err)
	}
}

func TestSlug(t *testing.T) {
	tests := []struct{ in, want string }{
		{in: "Monthly report", want: "monthly-report"},
		{in: "  Spaced  Out  ", want: "spaced-out"},
		{in: "Sales/Revenue (v2)", want: "sales-revenue-v2"},
		{in: "already-slugged", want: "already-slugged"},
		{in: "ÅÄÖ", want: "workflow"},
		{in: "", want: "workflow"},
		{in: "---", want: "workflow"},
		{in: "2026 Q1", want: "2026-q1"},
	}
	for _, tc := range tests {
		if got := Slug(tc.in, KindWorkflow); got != tc.want {
			t.Fatalf("Slug(%q) = %q, want %q", tc.in, got, tc.want)
		}
	}
	long := ""
	for i := 0; i < 200; i++ {
		long += "a"
	}
	if got := Slug(long, KindWorkflow); len(got) > 60 {
		t.Fatalf("Slug of a long title = %d chars, want <= 60", len(got))
	}
}

func TestDirIsEmpty(t *testing.T) {
	root := t.TempDir()
	empty, err := DirIsEmpty(root)
	if err != nil || !empty {
		t.Fatalf("empty dir: %v %v", empty, err)
	}
	if empty, err := DirIsEmpty(filepath.Join(root, "absent")); err != nil || !empty {
		t.Fatalf("absent dir must count as empty: %v %v", empty, err)
	}
	write(t, root, "x.py", "1")
	if empty, err := DirIsEmpty(root); err != nil || empty {
		t.Fatalf("non-empty dir: %v %v", empty, err)
	}
}

// ronja.json is a committed file people edit by hand, and the two spellings
// they reach for are exactly the two NormalizeURL folds away. A raw map lookup
// misses both, and the folder then reports as unbound against the instance it
// is plainly bound to — which a first push would "fix" by creating a second
// workflow.
func TestBindingToleratesHandEditedHostKeys(t *testing.T) {
	m := &Manifest{
		Kind:  KindWorkflow,
		Title: "x",
		Instances: []Instance{
			{URL: "https://APP.ronja.tech/", TenantID: "ten-acme", Binding: Binding{WorkflowID: "wf-1"}},
		},
	}
	b, ok, _ := m.Binding(InstanceKey{URL: "https://app.ronja.tech", TenantID: "ten-acme"})
	if !ok || b.WorkflowID != "wf-1" {
		t.Errorf("Binding = %+v (%v), want wf-1 despite the case and trailing slash", b, ok)
	}
	if _, ok, _ := m.Binding(InstanceKey{URL: "https://other.ronja.tech", TenantID: "ten-acme"}); ok {
		t.Error("a different instance must not match")
	}
}

// Without the drop, a push against a hand-edited key would leave the manifest
// carrying that entry AND a canonical one, and which of the two answered a
// later lookup would depend on nothing legible.
func TestSetBindingReplacesEquivalentKey(t *testing.T) {
	m := &Manifest{
		Kind:  KindWorkflow,
		Title: "x",
		Instances: []Instance{
			{URL: "https://APP.ronja.tech/", TenantID: "ten-acme", Binding: Binding{WorkflowID: "old"}},
		},
	}
	m.SetBinding(InstanceKey{URL: "https://app.ronja.tech", TenantID: "ten-acme"}, Binding{WorkflowID: "new"})
	if len(m.Instances) != 1 {
		t.Fatalf("instances = %v, want exactly one entry", m.Instances)
	}
	if got := m.Instances[0]; got.WorkflowID != "new" || got.URL != "https://app.ronja.tech" {
		t.Errorf("canonical entry = %+v, want the new binding under the canonical URL", got)
	}
}

// The baseline written after a clone claims every one of these paths is on
// disk. A set that cannot all be written must be refused before anything is
// created, not skipped file by file.
func TestCheckLocalPaths(t *testing.T) {
	tests := []struct {
		name    string
		paths   []string
		wantErr string
	}{
		{name: "ordinary set", paths: []string{"main.py", "lib/helpers.py"}},
		{name: "case differs beyond the first letter", paths: []string{"aB.py", "ab.py"},
			wantErr: "capitalisation"},
		{name: "traversal", paths: []string{"main.py", "../escape.py"},
			wantErr: "cannot be written"},
		{name: "space in path", paths: []string{"my file.py"}, wantErr: "cannot be written"},
		{name: "case collision", paths: []string{"Main.py", "main.py"},
			wantErr: "capitalisation"},
		{name: "case collision in a directory segment", paths: []string{"Lib/a.py", "lib/b.py"}},
		{name: "same directory, different case", paths: []string{"lib/A.py", "lib/a.py"},
			wantErr: "capitalisation"},
		{name: "empty set", paths: nil},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := CheckLocalPaths(tt.paths, WorkflowKind)
			if tt.wantErr == "" {
				if err != nil {
					t.Fatalf("CheckLocalPaths = %v, want nil", err)
				}
				return
			}
			if err == nil {
				t.Fatalf("CheckLocalPaths = nil, want an error mentioning %q", tt.wantErr)
			}
			if !strings.Contains(err.Error(), tt.wantErr) {
				t.Errorf("error = %v, want one mentioning %q", err, tt.wantErr)
			}
			// The remedy is not something the CLI can perform, so the message
			// has to point somewhere.
			if !strings.Contains(err.Error(), "web builder") {
				t.Errorf("error gives the caller no way forward: %v", err)
			}
		})
	}
}

// A symlinked module is an ordinary thing to have in a repo. One that vanishes
// from every push with no output anywhere is exactly the surprise Skipped
// exists to prevent.
func TestEnumerateReportsNonRegularFiles(t *testing.T) {
	root := t.TempDir()
	write(t, root, "main.py", "x")
	write(t, root, ManifestName, `{"kind":"workflow"}`)
	if err := os.Symlink(filepath.Join(root, "main.py"), filepath.Join(root, "linked.py")); err != nil {
		t.Skipf("symlinks unavailable: %v", err)
	}
	// A DOTFILE symlink is ruled out by NAME, before the regular-file check —
	// so it is reported as a dot-name rather than as a broken link.
	if err := os.Symlink(filepath.Join(root, "main.py"), filepath.Join(root, ".env")); err != nil {
		t.Skipf("symlinks unavailable: %v", err)
	}

	e, err := Enumerate(root, WorkflowKind)
	if err != nil {
		t.Fatalf("enumerate: %v", err)
	}
	if _, ok := e.Files["linked.py"]; ok {
		t.Error("a symlink must not be syncable content")
	}
	reasons := map[string]string{}
	for _, s := range e.Skipped {
		reasons[s.Path] = s.Reason
	}
	if len(reasons) != 2 {
		t.Fatalf("skipped = %+v, want linked.py and .env", e.Skipped)
	}
	if !strings.Contains(reasons["linked.py"], "regular file") {
		t.Errorf("reason for linked.py = %q", reasons["linked.py"])
	}
	if !strings.Contains(reasons[".env"], "dot-name") {
		t.Errorf("reason for .env = %q", reasons[".env"])
	}
}
