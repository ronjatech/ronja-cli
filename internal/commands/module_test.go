package commands

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/ronjatech/ronja-cli/internal/api"
	"github.com/ronjatech/ronja-cli/internal/wfdir"
)

// The command half of the module loop.
//
// What is exercised here is the layer the wfdir tests cannot reach: which row a
// push writes to, what the drift guard decides, what the baseline may honestly
// claim, and what each verb prints under --json. The fakeInstance harness models
// the WORKFLOW surface, so the server-driven paths that need a module endpoint
// are exercised through the pure decision functions the commands call — which is
// where the interesting refusals actually live, and which is what a fake would
// have been standing in for anyway.

// moduleTestFolder writes a module folder bound to one instance, with an
// optional baseline, and returns its root.
func moduleTestFolder(t *testing.T, key wfdir.InstanceKey, binding wfdir.Binding, files map[string]string) string {
	t.Helper()
	root := t.TempDir()
	m := &wfdir.Manifest{Kind: wfdir.KindModule, Name: "emab_tracker", Title: "EMAB tracker"}
	m.SetBinding(key, binding)
	if err := wfdir.SaveManifest(root, m); err != nil {
		t.Fatalf("save manifest: %v", err)
	}
	for path, body := range files {
		if err := wfdir.WriteFile(root, path, body); err != nil {
			t.Fatalf("write %s: %v", path, err)
		}
	}
	return root
}

// moduleFolderFor opens a folder the way the commands do, without touching the
// network: the binding is matched against the key handed in.
func moduleFolderFor(t *testing.T, root string, key wfdir.InstanceKey, baseline *wfdir.InstanceState) *folder {
	t.Helper()
	manifest, err := wfdir.LoadManifest(root, wfdir.ModuleKind)
	if err != nil {
		t.Fatalf("load manifest: %v", err)
	}
	state := &wfdir.State{}
	if baseline != nil {
		state.Set(key, baseline)
	}
	b, _, err := manifest.Binding(key)
	if err != nil {
		t.Fatalf("read binding: %v", err)
	}
	return &folder{
		Root: root, Kind: wfdir.ModuleKind, Manifest: manifest,
		Lock: &wfdir.Lock{}, State: state, Key: key, Binding: b, Bound: b.ModuleID != "",
	}
}

func moduleKey() wfdir.InstanceKey {
	return wfdir.InstanceKey{URL: "https://example.test", TenantID: testTenantID}
}

// TestModuleInitWritesFolder pins the shape `module init` leaves behind: a
// manifest naming the package, a seeded __init__.py, an empty baseline, and NO
// entrypoint key — a module has nothing to run, and a manifest that named one
// would invite a run verb.
func TestModuleInitWritesFolder(t *testing.T) {
	f := newFakeInstance(t)
	signIn(t, f)
	// A profile that RECORDS its organization, so init needs no /me round trip —
	// which is what lets this test exercise the whole command with a server that
	// models no module routes at all.
	writeProfile(t, "test", f.URL(), testTenantID, "test-token")
	t.Setenv("RONJA_TOKEN", "")
	t.Setenv("RONJA_URL", "")

	root := t.TempDir()
	out, err := runCLI(t, root, "module", "init", "emab_tracker",
		"--feature", "collection-1", "--json")
	if err != nil {
		t.Fatalf("module init: %v", err)
	}
	payload := decodeJSON(t, out)
	for key, want := range map[string]any{
		"kind":         wfdir.KindModule,
		"name":         "emab_tracker",
		"title":        "emab_tracker",
		"featureID":    "collection-1",
		"bound":        true,
		"created":      false,
		"seededInitPy": true,
	} {
		if payload[key] != want {
			t.Errorf("%s = %v, want %v", key, payload[key], want)
		}
	}

	manifest, err := wfdir.LoadManifest(root, wfdir.ModuleKind)
	if err != nil {
		t.Fatalf("load manifest: %v", err)
	}
	if manifest.Name != "emab_tracker" {
		t.Fatalf("manifest name = %q", manifest.Name)
	}
	// The KEY, not the resolved value: LoadManifest falls back to the Kind's
	// DefaultEntrypoint, which is empty for a module, so the assertion has to be
	// against the file rather than against the struct.
	raw, err := os.ReadFile(wfdir.ManifestPath(root))
	if err != nil {
		t.Fatalf("read manifest: %v", err)
	}
	if strings.Contains(string(raw), "entrypoint") {
		t.Fatalf("a module manifest must not name an entrypoint:\n%s", raw)
	}
	if _, err := os.Stat(filepath.Join(root, "__init__.py")); err != nil {
		t.Fatalf("init must seed __init__.py: %v", err)
	}
}

// TestModuleInitRefusesBadName pins the local half of the name rule: a
// character Python will never accept is refused at the moment it is typed,
// rather than at the first push of a manifest a colleague has already pulled.
func TestModuleInitRefusesBadName(t *testing.T) {
	for _, name := range []string{"emab-tracker", "emab.tracker", "2fast", "emab tracker"} {
		if err := checkLocalModuleName(name); err == nil {
			t.Errorf("expected %q to be refused as a package name", name)
		}
	}
	for _, name := range []string{"emab_tracker", "_private", "tracker2", "Tracker"} {
		if err := checkLocalModuleName(name); err != nil {
			t.Errorf("%q is a valid identifier and must be accepted: %v", name, err)
		}
	}
}

// TestModuleValidateJSON pins the --json shape of the local validate pass, and
// the two things it must catch: a missing __init__.py, and a non-.py file being
// SKIPPED rather than treated as a problem (a README in the folder is fine, it
// simply is not part of the module).
func TestModuleValidateJSON(t *testing.T) {
	f := newFakeInstance(t)
	signIn(t, f)

	root := moduleTestFolder(t, f.Key(), wfdir.Binding{FeatureID: "collection-1"}, map[string]string{
		"tracker.py": "x = 1",
		"README.md":  "# docs",
	})
	out, err := runCLI(t, root, "module", "validate", "--json")
	if err == nil {
		t.Fatal("expected a folder with no __init__.py to be refused")
	}
	payload := decodeJSON(t, out)
	if payload["ok"] != false {
		t.Fatalf("ok = %v, want false", payload["ok"])
	}
	problems, _ := payload["problems"].([]any)
	if len(problems) != 1 || !strings.Contains(problems[0].(string), "__init__.py") {
		t.Fatalf("problems = %v, want one naming __init__.py", problems)
	}
	// The README is reported as SKIPPED, not as a problem: a module folder is an
	// ordinary repo directory, and refusing one for holding docs would be a rule
	// nobody could satisfy.
	skipped, _ := payload["skipped"].([]any)
	if len(skipped) != 1 {
		t.Fatalf("skipped = %v, want the README alone", skipped)
	}

	if err := wfdir.WriteFile(root, "__init__.py", ""); err != nil {
		t.Fatalf("write __init__.py: %v", err)
	}
	out, err = runCLI(t, root, "module", "validate", "--json")
	if err != nil {
		t.Fatalf("a complete folder must validate: %v (%s)", err, out)
	}
	payload = decodeJSON(t, out)
	if payload["ok"] != true {
		t.Fatalf("ok = %v, want true (%v)", payload["ok"], payload["problems"])
	}
	if payload["files"] != float64(2) {
		t.Fatalf("files = %v, want 2 — the .py files only", payload["files"])
	}

	// The marker rule, which is the refusal a helper moved OUT of a workflow
	// folder walks into: the copy it came from may well have carried one.
	if err := wfdir.WriteFile(root, "tracker.py", "rows = tools.query(\"{{ ref('table-x') }}\")\n"); err != nil {
		t.Fatalf("write tracker.py: %v", err)
	}
	out, err = runCLI(t, root, "module", "validate", "--json")
	if err == nil {
		t.Fatal("a module file carrying a marker must be refused")
	}
	payload = decodeJSON(t, out)
	problems, _ = payload["problems"].([]any)
	if len(problems) != 1 {
		t.Fatalf("problems = %v, want the marker alone", problems)
	}
	for _, want := range []string{"tracker.py", "{{ ref(...) }}", "marker-free"} {
		if !strings.Contains(problems[0].(string), want) {
			t.Errorf("the refusal must mention %q, got %q", want, problems[0])
		}
	}
	// Dead text is not a violation, matching the server: a docstring that shows
	// a consumer how to call this module is documentation.
	if err := wfdir.WriteFile(root, "tracker.py",
		"\"\"\"Call me from a workflow that reads {{ ref('table-x') }}.\"\"\"\n"); err != nil {
		t.Fatalf("write tracker.py: %v", err)
	}
	if _, err = runCLI(t, root, "module", "validate", "--json"); err != nil {
		t.Fatalf("a marker inside a docstring must validate: %v", err)
	}
}

// TestModuleCloneRefusesNonEmptyDirectory pins the refusal that happens before
// a single request: "that folder is not empty" does not become truer after
// three round trips, and this is the form CI uses.
func TestModuleCloneRefusesNonEmptyDirectory(t *testing.T) {
	f := newFakeInstance(t)
	signIn(t, f)

	dest := t.TempDir()
	if err := os.WriteFile(filepath.Join(dest, "already-here.py"), []byte("x = 1"), 0o644); err != nil {
		t.Fatalf("seed destination: %v", err)
	}
	_, err := runCLI(t, t.TempDir(), "module", "clone", "module-1", dest)
	if err == nil {
		t.Fatal("expected a clone into a non-empty directory to be refused")
	}
	if !strings.Contains(err.Error(), "not empty") {
		t.Fatalf("the refusal must say the directory is not empty, got: %v", err)
	}
}

// TestModuleDriftGuardVerdicts drives the pure drift guard through the four
// states that decide whether a push proceeds and whether the per-file
// compare-and-swap is armed.
//
// The equivalence being pinned is the whole rule on filePreconditions: the
// preconditions are armed EXACTLY where the guard vouched for the baseline, and
// suppressed everywhere it was bypassed.
func TestModuleDriftGuardVerdicts(t *testing.T) {
	key := moduleKey()
	root := moduleTestFolder(t, key, wfdir.Binding{ModuleID: "module-1", FeatureID: "collection-1"}, nil)
	target := &api.Module{ID: "module-draft", ParentModuleID: "module-1",
		Name: "emab_tracker", Title: "EMAB tracker", Lifecycle: api.LifecycleDraft}
	remote := map[string]string{"__init__.py": "", "tracker.py": "x = 1"}

	baseline := &wfdir.InstanceState{
		SourceID: "module-draft", Name: "emab_tracker", Title: "EMAB tracker",
		Files: map[string]wfdir.FileState{
			"__init__.py": {SHA256: wfdir.HashString("")},
			"tracker.py":  {SHA256: wfdir.HashString("x = 1")},
		},
	}

	t.Run("clean baseline arms preconditions", func(t *testing.T) {
		f := moduleFolderFor(t, root, key, baseline)
		verdict, err := checkModuleDrift(f, target, remote, false, headAgreement{})
		if err != nil {
			t.Fatalf("clean baseline must not refuse: %v", err)
		}
		if !verdict.BaselineClean || verdict.Bypassed {
			t.Fatalf("verdict = %+v, want clean and not bypassed", verdict)
		}
		if verdict.Preconditions["tracker.py"] != wfdir.HashString("x = 1") {
			t.Fatalf("preconditions must be the baseline's hashes, got %v", verdict.Preconditions)
		}
	})

	t.Run("remote moved refuses", func(t *testing.T) {
		f := moduleFolderFor(t, root, key, baseline)
		moved := map[string]string{"__init__.py": "", "tracker.py": "x = 2"}
		if _, err := checkModuleDrift(f, target, moved, false, headAgreement{}); err == nil {
			t.Fatal("expected drift against the baseline to be refused")
		} else if !strings.Contains(err.Error(), "tracker.py") {
			t.Fatalf("the refusal must name what moved, got: %v", err)
		}
	})

	t.Run("force bypasses and disarms preconditions", func(t *testing.T) {
		f := moduleFolderFor(t, root, key, baseline)
		moved := map[string]string{"__init__.py": "", "tracker.py": "x = 2"}
		verdict, err := checkModuleDrift(f, target, moved, true, headAgreement{})
		if err != nil {
			t.Fatalf("--force must proceed: %v", err)
		}
		if !verdict.Bypassed || len(verdict.Preconditions) != 0 {
			t.Fatalf("verdict = %+v, want bypassed with no preconditions", verdict)
		}
	})

	t.Run("no baseline vouches on the committed anchor", func(t *testing.T) {
		f := moduleFolderFor(t, root, key, nil)
		head := headAgreement{Recorded: "cmv-1", Current: "cmv-1", AnchoredDraft: true}
		verdict, err := checkModuleDrift(f, target, remote, false, head)
		if err != nil {
			t.Fatalf("a vouching anchor must let a fresh checkout through: %v", err)
		}
		if !verdict.HeadVouched {
			t.Fatalf("verdict = %+v, want HeadVouched", verdict)
		}
		// Armed on the REMOTE listing rather than bypassed — the one place they
		// differ, and what makes a CI push safer than the --force it replaces.
		if verdict.Preconditions["tracker.py"] != wfdir.HashString("x = 1") {
			t.Fatalf("preconditions = %v, want the remote listing's hashes", verdict.Preconditions)
		}
	})

	t.Run("no baseline and a moved head refuses without suggesting force", func(t *testing.T) {
		f := moduleFolderFor(t, root, key, nil)
		head := headAgreement{Recorded: "cmv-1", Current: "cmv-2", AnchoredDraft: true}
		_, err := checkModuleDrift(f, target, remote, false, head)
		if err == nil {
			t.Fatal("expected a moved head to be refused")
		}
		for _, want := range []string{"cmv-1", "cmv-2", "ronja module clone"} {
			if !strings.Contains(err.Error(), want) {
				t.Errorf("the refusal must mention %q, got: %v", want, err)
			}
		}
		if strings.Contains(err.Error(), "--force") {
			t.Fatalf("a moved head must not advertise --force — it would overwrite the publish just found: %v", err)
		}
	})

	t.Run("crashed first push resumes", func(t *testing.T) {
		f := moduleFolderFor(t, root, key, nil)
		fresh := &api.Module{ID: "module-1", Lifecycle: api.LifecycleDraft, Name: "emab_tracker"}
		seeded := map[string]string{"__init__.py": ""}
		verdict, err := checkModuleDrift(f, fresh, seeded, false, headAgreement{})
		if err != nil {
			t.Fatalf("an untouched first push must resume rather than refuse: %v", err)
		}
		if !verdict.Bypassed {
			t.Fatalf("verdict = %+v, want bypassed", verdict)
		}
	})
}

// TestModuleMetadataDriftAndPatch pins the two halves of the metadata guard:
// what a push would CHANGE, and what it would OVERWRITE.
func TestModuleMetadataDriftAndPatch(t *testing.T) {
	manifest := &wfdir.Manifest{Kind: wfdir.KindModule, Name: "emab_tracker", Title: "EMAB tracker"}
	row := &api.Module{ID: "module-1", Name: "old_name", Title: "Old title", Lifecycle: api.LifecycleLive}

	patch := moduleMetadataPatch(manifest, row)
	if patch.Name != "emab_tracker" || patch.Title != "EMAB tracker" {
		t.Fatalf("patch = %+v, want both fields", patch)
	}
	// An UNDECLARED field is never sent: "" means "this folder does not manage
	// it", not "clear it".
	unmanaged := &wfdir.Manifest{Kind: wfdir.KindModule}
	if p := moduleMetadataPatch(unmanaged, row); !p.Empty() {
		t.Fatalf("an unmanaged manifest must patch nothing, got %+v", p)
	}

	// Drift needs BOTH halves: the remote differs from the baseline (something
	// moved) AND the folder declares the field (we would overwrite it).
	//
	// ⚠️ The row handed in is the LIVE one — the row PUT /module/:id patches and
	// the only row whose name and title mean anything. A draft's copies are
	// taken at checkout and never patched, so feeding those here would report
	// the reader's own last title edit as a colleague's change and miss the
	// colleague's actual rename. See moduleIdentityRow.
	baseline := &wfdir.InstanceState{Name: "emab_tracker", Title: "EMAB tracker"}
	renamedLive := &api.Module{ID: "module-1", Lifecycle: api.LifecycleLive,
		Name: "renamed_by_colleague", Title: "EMAB tracker"}
	got := moduleMetadataDriftOf(baseline, renamedLive, manifest)
	if len(got) != 1 || got[0].Field != "package name" {
		t.Fatalf("drift = %+v, want one package-name entry", got)
	}
	// The other direction, and the false-positive half of the same bug: a title
	// this folder itself pushed onto the live row is NOT drift the next time
	// round, however stale the open draft's copy of it is.
	patchedLive := &api.Module{ID: "module-1", Lifecycle: api.LifecycleLive,
		Name: "emab_tracker", Title: "EMAB tracker"}
	staleDraft := &api.Module{ID: "module-draft", ParentModuleID: "module-1",
		Lifecycle: api.LifecycleDraft, Name: "emab_tracker", Title: "The title before the edit"}
	if got := moduleMetadataDriftOf(baseline, patchedLive, manifest); len(got) != 0 {
		t.Fatalf("the live row agrees with the baseline, so there is no drift: %+v", got)
	}
	if got := moduleMetadataDriftOf(baseline, staleDraft, manifest); len(got) == 0 {
		t.Fatal("sanity: the draft's stale copy DOES differ — which is why the guard must not read it")
	}
	drifted := renamedLive
	// A baseline with no metadata recorded at all is a state file written before
	// the guard existed. It reports NO drift rather than turning every such
	// folder into a refusal.
	if got := moduleMetadataDriftOf(nil, drifted, manifest); got != nil {
		t.Fatalf("a nil baseline must report no drift, got %+v", got)
	}
	if got := moduleMetadataDriftOf(&wfdir.InstanceState{}, drifted, manifest); len(got) != 0 {
		t.Fatalf("an unrecorded baseline must report no drift, got %+v", got)
	}
}

// TestModuleMetadataDeferredWhileUnpublished pins the one thing this loop
// cannot do: PUT /module/:id takes the LIVE row, so a manifest that has moved on
// since an unpublished module's create is reported rather than sent to a route
// that would refuse it.
func TestModuleMetadataDeferredWhileUnpublished(t *testing.T) {
	key := moduleKey()
	root := moduleTestFolder(t, key, wfdir.Binding{ModuleID: "module-1"}, nil)
	f := moduleFolderFor(t, root, key, nil)

	draft := &api.Module{ID: "module-1", Name: "old_name", Title: "Old title", Lifecycle: api.LifecycleDraft}
	patch, deferred := moduleMetadataPatchFor(f, existingModuleTarget{Module: draft}, draft)
	if !patch.Empty() {
		t.Fatalf("an unpublished module must not be patched, got %+v", patch)
	}
	if deferred == "" || !strings.Contains(deferred, "publish") {
		t.Fatalf("the deferral must say what resolves it, got %q", deferred)
	}

	// A LIVE module patches normally, and reports no deferral.
	live := &api.Module{ID: "module-1", Name: "old_name", Title: "Old title", Lifecycle: api.LifecycleLive}
	patch, deferred = moduleMetadataPatchFor(f, existingModuleTarget{Module: live}, live)
	if deferred != "" {
		t.Fatalf("a live module must not defer, got %q", deferred)
	}
	if patch.Name != "emab_tracker" {
		t.Fatalf("patch = %+v, want the manifest's name", patch)
	}

	// A module this push just CREATED already carries the manifest's values —
	// they rode the create body — so there is nothing to patch and nothing to
	// defer.
	patch, deferred = moduleMetadataPatchFor(f, existingModuleTarget{}, live)
	if !patch.Empty() || deferred != "" {
		t.Fatalf("a fresh create must patch nothing and defer nothing, got (%+v, %q)", patch, deferred)
	}
}

// TestModuleAnchoredDraft pins which rows the committed head anchor may speak
// for. The parentless leg is not a relaxation of the guard — it is the guard
// applied to the right row — and the excluded case is the one it exists for.
func TestModuleAnchoredDraft(t *testing.T) {
	live := &api.Module{ID: "module-1", Lifecycle: api.LifecycleLive}
	for _, tc := range []struct {
		name string
		in   existingModuleTarget
		want bool
	}{
		{"nothing open, this push checks one out", existingModuleTarget{Module: live}, true},
		{"a parentless draft is its own head",
			existingModuleTarget{Module: &api.Module{ID: "module-1", Lifecycle: api.LifecycleDraft}}, true},
		{"a shared-feature proposal is its own head too",
			existingModuleTarget{Module: &api.Module{ID: "module-1", Lifecycle: api.LifecycleProposed}}, true},
		{"an edit shadow already open is NOT vouched for",
			existingModuleTarget{Module: live, Draft: &api.Module{ID: "module-draft", ParentModuleID: "module-1", Lifecycle: api.LifecycleDraft}}, false},
	} {
		if got := tc.in.AnchoredDraft(); got != tc.want {
			t.Errorf("%s: AnchoredDraft = %v, want %v", tc.name, got, tc.want)
		}
	}
}

// TestModuleBaselineRecordsTheRow pins that the baseline records what the
// SERVER held, name included — the guard is worthless if it records what we
// wanted the row to be, because it would then agree with itself forever.
func TestModuleBaselineRecordsTheRow(t *testing.T) {
	row := &api.Module{
		ID: "module-draft", Lifecycle: api.LifecycleDraft,
		Name: "emab_tracker", Title: "EMAB tracker", UpdatedAt: time.Unix(0, 0).UTC(),
	}
	inst := moduleBaselineFromLocal(row, row, map[string]string{"__init__.py": "", "tracker.py": "x = 1"})
	if inst.Name != "emab_tracker" || inst.Title != "EMAB tracker" {
		t.Fatalf("baseline = %+v, want the row's metadata", inst)
	}
	if inst.SourceID != "module-draft" || inst.SourceLifecycle != api.LifecycleDraft {
		t.Fatalf("baseline must name the row it describes, got %+v", inst)
	}
	if inst.Files["tracker.py"].SHA256 != wfdir.HashString("x = 1") {
		t.Fatalf("baseline files = %+v", inst.Files)
	}
	// Nothing workflow-shaped leaks in: the module baseline has no entrypoint,
	// no parameters and no zone, and a nil pointer is what reads as "not part of
	// this baseline" everywhere.
	if inst.Entrypoint != "" || inst.Parameters != nil || inst.ReportingTimezone != nil {
		t.Fatalf("a module baseline must carry no workflow fields, got %+v", inst)
	}

	// moduleBaselineDescribes is what stops a no-op push re-stamping the
	// baseline — and it has to notice a metadata change, not only a row change.
	if !moduleBaselineDescribes(inst, row, row) {
		t.Fatal("a baseline taken from a row must describe it")
	}
	renamed := *row
	renamed.Name = "renamed"
	if moduleBaselineDescribes(inst, row, &renamed) {
		t.Fatal("a renamed row must not read as already described")
	}

	// The two rows are genuinely different questions: the FILES came from the
	// draft, the NAME and TITLE live on the module. A baseline recorded from a
	// draft carrying the pre-patch title is the bug this split exists to stop.
	draft := &api.Module{
		ID: "module-draft", ParentModuleID: "module-1", Lifecycle: api.LifecycleDraft,
		Name: "emab_tracker", Title: "Title the draft was checked out with",
	}
	live := &api.Module{ID: "module-1", Lifecycle: api.LifecycleLive,
		Name: "emab_tracker", Title: "The title just patched onto the live row"}
	split := moduleBaselineFromLocal(draft, live, map[string]string{"__init__.py": ""})
	if split.SourceID != draft.ID {
		t.Fatalf("the baseline must name the row the FILES came from, got %q", split.SourceID)
	}
	if split.Title != live.Title {
		t.Fatalf("the baseline must record the LIVE row's title, got %q", split.Title)
	}
	if !moduleBaselineDescribes(split, draft, live) {
		t.Fatal("the baseline it just wrote must describe the same two rows")
	}
}

// TestModuleIdentityRow pins which row the metadata half of a push reads: the
// live module when there is one, the module itself while it is unpublished.
func TestModuleIdentityRow(t *testing.T) {
	live := &api.Module{ID: "module-1", Lifecycle: api.LifecycleLive, Title: "Live"}
	draft := &api.Module{ID: "module-draft", ParentModuleID: "module-1",
		Lifecycle: api.LifecycleDraft, Title: "Copied at checkout"}
	if got := moduleIdentityRow(existingModuleTarget{Module: live, Draft: draft}, nil, draft); got != live {
		t.Fatalf("an edit draft's identity is the LIVE row, got %+v", got)
	}
	unpublished := &api.Module{ID: "module-2", Lifecycle: api.LifecycleProposed, Title: "New"}
	if got := moduleIdentityRow(existingModuleTarget{Module: unpublished}, nil, unpublished); got != unpublished {
		t.Fatalf("an unpublished module IS its own identity row, got %+v", got)
	}
	// A module this push just created in a SHARED feature: the create answered
	// with a proposal, which is writable, so nothing was checked out and the two
	// rows are one.
	proposed := &api.Module{ID: "module-3", Lifecycle: api.LifecycleProposed}
	if got := moduleIdentityRow(existingModuleTarget{}, proposed, proposed); got != proposed {
		t.Fatalf("a fresh proposal's identity is the created row, got %+v", got)
	}
	// And in a PRIVATE feature, where the create comes back LIVE and the push
	// checks a draft out of it: the identity is the created MODULE, never the
	// draft the files went to. Reading `target` here would record the draft's
	// copied metadata as the module's, which is the false-drift bug the identity
	// row exists to prevent.
	createdLive := &api.Module{ID: "module-4", Lifecycle: api.LifecycleLive, Title: "Fresh"}
	freshDraft := &api.Module{ID: "module-4-draft", ParentModuleID: "module-4",
		Lifecycle: api.LifecycleDraft, Title: "Fresh"}
	if got := moduleIdentityRow(existingModuleTarget{}, createdLive, freshDraft); got != createdLive {
		t.Fatalf("a private create's identity is the created module, not its draft, got %+v", got)
	}
}

// TestModulePushResultJSONShape pins the --json contract of `module push`: the
// keys anything scripting the CLI reads, and the two that are module-specific.
func TestModulePushResultJSONShape(t *testing.T) {
	body, err := json.Marshal(&modulePushResult{
		Created: true, Proposed: true,
		ModuleID: "module-1", DraftID: "module-1",
		Pushed: []string{"__init__.py"}, Deleted: []string{},
		Target: "org on https://example.test",
	})
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	var payload map[string]any
	if err := json.Unmarshal(body, &payload); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	for _, key := range []string{"created", "proposed", "moduleID", "draftID", "pushed", "deleted", "unchanged", "upToDate", "conflict"} {
		if _, ok := payload[key]; !ok {
			t.Errorf("push --json must carry %q: %s", key, body)
		}
	}
	// Absent when there is nothing to say, so a caller can branch on presence.
	if _, ok := payload["metadataDeferred"]; ok {
		t.Errorf("metadataDeferred must be omitted when empty: %s", body)
	}
}

// TestModulePublishResultJSONShape pins the publish contract, and in particular
// the dependent count that makes the pinning rule sayable with a real number.
func TestModulePublishResultJSONShape(t *testing.T) {
	body, err := json.Marshal(&modulePublishResult{
		Outcome: outcomePublished, ModuleID: "module-1", DraftID: "module-draft",
		HeadVersionID: "cmv-2", DependentWorkflowCount: 3,
	})
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	var payload map[string]any
	if err := json.Unmarshal(body, &payload); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if payload["outcome"] != outcomePublished {
		t.Fatalf("outcome = %v", payload["outcome"])
	}
	if payload["dependentWorkflowCount"] != float64(3) {
		t.Fatalf("dependentWorkflowCount = %v, want 3", payload["dependentWorkflowCount"])
	}
	if payload["headVersionID"] != "cmv-2" {
		t.Fatalf("headVersionID = %v", payload["headVersionID"])
	}
}

// TestModuleUnclonableLifecycles pins the extra workable lifecycle a module has
// and a workflow does not: a brand-new module in a SHARED feature is created
// `proposed`, and a loop that refused it would refuse the row its own first
// push just made.
func TestModuleUnclonableLifecycles(t *testing.T) {
	for _, lifecycle := range []string{api.LifecycleLive, api.LifecycleDraft, api.LifecycleProposed} {
		if err := refuseUnclonableModule(&api.Module{ID: "module-1", Lifecycle: lifecycle}); err != nil {
			t.Errorf("%s must be workable: %v", lifecycle, err)
		}
	}
	for _, lifecycle := range []string{api.LifecycleVersion, api.LifecycleArchived, "nonsense"} {
		if err := refuseUnclonableModule(&api.Module{ID: "module-1", Lifecycle: lifecycle}); err == nil {
			t.Errorf("%s must be refused", lifecycle)
		}
	}
}

// ── Sequence tests ─────────────────────────────────────────────────────────
//
// Both of these drive the WHOLE command against the fake instance, one verb
// after another, because both bugs they pin are invisible to a unit test of any
// single call: what makes them go wrong is what the previous invocation wrote
// down.

// TestModulePushRenameSequence is the ordinary rename: publish, edit the title
// in the manifest, push, push again.
//
// The second push is the assertion. The metadata patch lands on the LIVE row —
// PUT /module/:id refuses a draft id — while the open draft keeps the copy of
// the title it was checked out with, so a baseline that recorded the DRAFT's
// metadata made the author's own edit read as "the draft changed on the server"
// on every later push. Nothing about that refusal is recoverable except --force,
// which is exactly the escape hatch a false refusal must not push people onto.
//
// The last step pins the other half of the same bug: a colleague's rename of the
// live row IS drift, and comparing the draft's stale copy would have missed it.
func TestModulePushRenameSequence(t *testing.T) {
	f := newFakeInstance(t)
	signInAsModuleAdmin(t, f)

	root := t.TempDir()
	if _, err := runCLI(t, root, "module", "init", "emab_tracker",
		"--feature", "collection-1", "--title", "EMAB tracker"); err != nil {
		t.Fatalf("module init: %v", err)
	}
	if err := wfdir.WriteFile(root, "tracker.py", "x = 1\n"); err != nil {
		t.Fatalf("write tracker.py: %v", err)
	}
	if _, err := runCLI(t, root, "module", "push", "--json"); err != nil {
		t.Fatalf("first push: %v", err)
	}
	if _, err := runCLI(t, root, "module", "publish", "--json"); err != nil {
		t.Fatalf("publish: %v", err)
	}

	if len(f.mod.created) != 1 || f.mod.created[0].Name != "emab_tracker" {
		t.Fatalf("the first push must create the module once, got %+v", f.mod.created)
	}
	live := f.mod.rows["module-new-1"]
	if live == nil || live.Lifecycle != api.LifecycleLive {
		t.Fatalf("the first publish must leave the module live, got %+v", live)
	}

	setModuleManifestTitle(t, root, "EMAB tracker v2")

	out, err := runCLI(t, root, "module", "push", "--json")
	if err != nil {
		t.Fatalf("the push carrying the rename must succeed: %v (%s)", err, out)
	}
	payload := decodeJSON(t, out)
	if payload["metadata"] != "the title" {
		t.Fatalf("metadata = %v, want the title patch: %s", payload["metadata"], out)
	}
	if live.Title != "EMAB tracker v2" {
		t.Fatalf("the LIVE row must hold the new title, got %q", live.Title)
	}

	// The push that used to be refused. Twice, because the second one is what
	// catches a baseline that was re-stamped from the draft on the way past.
	for i := 2; i <= 3; i++ {
		out, err = runCLI(t, root, "module", "push", "--json")
		if err != nil {
			t.Fatalf("push %d after the rename must not be refused as drift: %v (%s)", i, err, out)
		}
		payload = decodeJSON(t, out)
		if payload["upToDate"] != true {
			t.Fatalf("push %d should have had nothing to do: %s", i, out)
		}
	}
	// One patch, not one per push: a rename that "lands" again on every push
	// would be a folder fighting the server rather than a sync.
	if len(f.mod.patches) != 1 {
		t.Fatalf("the rename must land exactly once, got %d patches: %+v", len(f.mod.patches), f.mod.patches)
	}

	// And the false-negative half: a colleague renames the LIVE row, and the
	// next push has to refuse rather than silently push the old name back.
	live.Title = "Renamed by a colleague"
	if _, err = runCLI(t, root, "module", "push", "--json"); err == nil {
		t.Fatal("a rename on the live row is drift and must be refused")
	} else if !strings.Contains(err.Error(), "title") {
		t.Fatalf("the refusal must name what moved, got: %v", err)
	}
}

// TestModulePushRefusesATakenNameAtCreate is where a private folder actually
// meets the package-name collision: at the CREATE, on the first push.
//
// A private-feature module is born LIVE (rmodule.Add stamps the drafter fields
// only on the shared branch), so the live-name uniqueness index fires at INSERT
// rather than at some later commit. Nothing is bound and nothing is written —
// the remedy is to rename in ronja.json and push again.
func TestModulePushRefusesATakenNameAtCreate(t *testing.T) {
	f := newFakeInstance(t)
	signInAsModuleUser(t, f)
	f.mod.nameTaken["emab_tracker"] = true

	root := t.TempDir()
	if _, err := runCLI(t, root, "module", "init", "emab_tracker",
		"--feature", "collection-1", "--title", "EMAB tracker"); err != nil {
		t.Fatalf("module init: %v", err)
	}
	_, err := runCLI(t, root, "module", "push", "--json")
	if err == nil {
		t.Fatal("a first push whose package name is taken must be refused")
	}
	if !strings.Contains(err.Error(), "already exists") {
		t.Fatalf("the refusal must carry the server's message, got: %v", err)
	}
	if len(f.fileWrites) != 0 {
		t.Fatalf("nothing may have been written, got %v", f.fileWrites)
	}
}

// TestModulePublishFirstPublishNameCollision is the first publish that 409s.
//
// A parentless commit has no base version to have moved, so what refuses it is
// the live-name uniqueness index — a conflict with NO parent behind it. The
// conflict path used to read the parent's id straight out of a nil pointer, so
// the crash was the first thing an author saw; and even reported correctly there
// is nothing for --overwrite-remote to overwrite, so it must not be offered.
//
// ⚠️ The parentless draft is SEEDED rather than produced by a push, and that is
// a statement about the server rather than a shortcut. rmodule.Add mints a live
// row in a private feature and a proposal in a shared one, so no create makes a
// parentless draft today — the branch this test covers is the CLI's half of
// rmodule's parentless commit, kept honest for the rows that do reach it.
func TestModulePublishFirstPublishNameCollision(t *testing.T) {
	f := newFakeInstance(t)
	signInAsModuleAdmin(t, f)
	// Somebody else's live module already holds the package name.
	f.mod.nameTaken["emab_tracker"] = true
	f.AddModule(&api.Module{
		ID: "module-new-1", Lifecycle: api.LifecycleDraft, Name: "emab_tracker",
		Title: "EMAB tracker", DrafterUserID: fakeCallerUserID,
	}, api.ModuleFile{Path: moduleInitFile, Content: fakeModuleInitSeed})

	root := moduleTestFolder(t, f.Key(),
		wfdir.Binding{ModuleID: "module-new-1", FeatureID: "collection-1"},
		map[string]string{moduleInitFile: fakeModuleInitSeed})

	out, err := runCLI(t, root, "module", "publish", "--json")
	if err == nil {
		t.Fatal("expected the first publish to be refused")
	}
	payload := decodeJSON(t, out)
	if payload["outcome"] != outcomeConflict {
		t.Fatalf("outcome = %v, want %q: %s", payload["outcome"], outcomeConflict, out)
	}
	// The server's own message, passed through rather than paraphrased.
	if problem, _ := payload["error"].(string); !strings.Contains(problem, "already exists") {
		t.Fatalf("the refusal must carry the server's message, got %q", problem)
	}
	if detail, _ := payload["detail"].(string); !strings.Contains(detail, "overwrite-remote") {
		t.Fatalf("the detail must say why an override does not apply, got %q", detail)
	}

	// --overwrite-remote changes nothing: there is no version of THIS module to
	// confirm, and the module holding the name is somebody else's.
	before := len(f.mod.commitConfirmations)
	if _, err = runCLI(t, root, "module", "publish", "--overwrite-remote", "--json"); err == nil {
		t.Fatal("--overwrite-remote must not turn a name collision into a publish")
	}
	attempts := f.mod.commitConfirmations[before:]
	if len(attempts) != 1 || attempts[0] != "" {
		t.Fatalf("a first publish must make ONE unconfirmed commit attempt, got %q", attempts)
	}
	if len(f.mod.committed) != 0 {
		t.Fatalf("nothing may have been committed, got %v", f.mod.committed)
	}
	// The draft is intact — that is the promise the refusal makes.
	if row := f.mod.rows["module-new-1"]; row == nil || row.Lifecycle != api.LifecycleDraft {
		t.Fatalf("the draft must survive the refusal, got %+v", row)
	}
}

// TestModuleNonAdminLoopEndToEnd is the loop as an ORDINARY USER runs it, which
// is the shape every one of these tests used to dodge by signing in as an admin.
//
// Two fixes are load-bearing here and both are asserted end to end:
//
//   - the OWN-DRAFT read leg. A first push creates a parentless draft, and every
//     verb after it resolves the module by id. Without rmodule's
//     AccessibleForAgentGlobal that read is a 404 for the very person who just
//     made the row, so the second push and `module publish` both fail with a
//     message about a module that "no longer exists".
//
//   - the metadata the COMMIT publishes. A title patch lands on the LIVE row,
//     but a commit overwrites the parent with the DRAFT's copies — so a title
//     edit followed by a publish used to REVERT the title, silently, to whatever
//     the draft was checked out with.
func TestModuleNonAdminLoopEndToEnd(t *testing.T) {
	f := newFakeInstance(t)
	signInAsModuleUser(t, f)

	root := t.TempDir()
	if _, err := runCLI(t, root, "module", "init", "emab_tracker",
		"--feature", "collection-1", "--title", "EMAB tracker"); err != nil {
		t.Fatalf("module init: %v", err)
	}
	if err := wfdir.WriteFile(root, "tracker.py", "x = 1\n"); err != nil {
		t.Fatalf("write tracker.py: %v", err)
	}

	// Push, then push AGAIN before publishing. The second one is the read that
	// used to 404: it resolves the binding through GET /module/:id, and the row
	// it names is still an unpublished draft.
	if _, err := runCLI(t, root, "module", "push", "--json"); err != nil {
		t.Fatalf("first push: %v", err)
	}
	out, err := runCLI(t, root, "module", "push", "--json")
	if err != nil {
		t.Fatalf("a non-admin must be able to push again before publishing: %v (%s)", err, out)
	}
	if payload := decodeJSON(t, out); payload["upToDate"] != true {
		t.Fatalf("the second push had nothing to do: %s", out)
	}

	if _, err := runCLI(t, root, "module", "publish", "--json"); err != nil {
		t.Fatalf("a non-admin must be able to publish their own first draft: %v", err)
	}
	live := f.mod.rows["module-new-1"]
	if live == nil || live.Lifecycle != api.LifecycleLive {
		t.Fatalf("the first publish must leave the module live, got %+v", live)
	}

	// ── The edit round, where the metadata revert used to happen ─────────────
	setModuleManifestTitle(t, root, "EMAB tracker v2")
	if err := wfdir.WriteFile(root, "tracker.py", "x = 2\n"); err != nil {
		t.Fatalf("edit tracker.py: %v", err)
	}
	if _, err := runCLI(t, root, "module", "push", "--json"); err != nil {
		t.Fatalf("the push carrying the rename must succeed: %v", err)
	}
	if live.Title != "EMAB tracker v2" {
		t.Fatalf("the LIVE row must hold the new title after the push, got %q", live.Title)
	}
	// Both rows were told, each exactly once: the live row so the change is
	// visible now, and the draft so the commit publishes it instead of the copy
	// it was checked out with.
	if len(f.mod.patches) != 1 || f.mod.patches[0].Title != "EMAB tracker v2" {
		t.Fatalf("the live row must be patched once, got %+v", f.mod.patches)
	}
	if len(f.mod.draftPatches) != 1 || f.mod.draftPatches[0].Title != "EMAB tracker v2" {
		t.Fatalf("the open draft must be patched once, got %+v", f.mod.draftPatches)
	}

	if _, err := runCLI(t, root, "module", "publish", "--json"); err != nil {
		t.Fatalf("publishing the edit: %v", err)
	}
	if live.Title != "EMAB tracker v2" {
		t.Fatalf("the publish REVERTED the title to %q — the commit overwrites the parent with the draft's copies, so the draft has to carry the intended metadata", live.Title)
	}
	if got := f.ModuleFileContents(live.ID)["tracker.py"]; got != "x = 2\n" {
		t.Fatalf("the published files must be the pushed ones, got %q", got)
	}

	// And the push AFTER the publish has nothing left to reconcile — a folder
	// that re-patched on every push would be fighting the server rather than
	// syncing with it.
	out, err = runCLI(t, root, "module", "push", "--json")
	if err != nil {
		t.Fatalf("the push after the publish must not be refused: %v (%s)", err, out)
	}
	if len(f.mod.patches) != 1 || len(f.mod.draftPatches) != 1 {
		t.Fatalf("no further metadata patch may land, got live=%+v draft=%+v", f.mod.patches, f.mod.draftPatches)
	}
}

// TestModuleNonAdminSharedFeatureLoop is the same loop in a SHARED feature,
// where the create lands as a PROPOSAL rather than as a live module.
//
// This is the shape that needs the own-draft READ leg. A private create comes
// back live, so every later read of the binding resolves a live row; a proposal
// does not exist for anybody but its proposer until an admin approves it, so the
// second push resolves the binding through GetForAgent's in-flight leg or not at
// all. Before it existed this test's second push died with "cannot read module".
//
// It stops at the publish on purpose: a shared module's commit is admin-only,
// and a proposal is ALREADY in front of the admins — the create filed it — so
// what a non-admin's `publish` does is say so. It must not call request-review:
// that is the governance engine's DRAFT transition and a proposal is not a
// draft, so the real server answers 400 and the non-admin create lane, which is
// the ordinary one in a shared feature, ended in a lifecycle error.
func TestModuleNonAdminSharedFeatureLoop(t *testing.T) {
	f := newFakeInstance(t)
	signInAsModuleUser(t, f)
	f.mod.sharedFeature = true

	root := t.TempDir()
	if _, err := runCLI(t, root, "module", "init", "acme_tracker",
		"--feature", "collection-1", "--title", "ACME tracker"); err != nil {
		t.Fatalf("module init: %v", err)
	}
	if err := wfdir.WriteFile(root, "tracker.py", "x = 1\n"); err != nil {
		t.Fatalf("write tracker.py: %v", err)
	}

	out, err := runCLI(t, root, "module", "push", "--json")
	if err != nil {
		t.Fatalf("first push: %v (%s)", err, out)
	}
	if payload := decodeJSON(t, out); payload["proposed"] != true {
		t.Fatalf("a shared-feature create must be reported as a proposal: %s", out)
	}
	proposal := f.mod.rows["module-new-1"]
	if proposal == nil || proposal.Lifecycle != api.LifecycleProposed {
		t.Fatalf("the create must land as a proposal, got %+v", proposal)
	}
	// The read that used to 404: the binding names a row nobody but its proposer
	// can see, and the proposer is who is asking.
	out, err = runCLI(t, root, "module", "push", "--json")
	if err != nil {
		t.Fatalf("a proposer must be able to push again at their own pending proposal: %v (%s)", err, out)
	}
	if payload := decodeJSON(t, out); payload["upToDate"] != true {
		t.Fatalf("the second push had nothing to do: %s", out)
	}

	out, err = runCLI(t, root, "module", "publish", "--json")
	if err != nil {
		t.Fatalf("publish of a pending proposal must report, not fail: %v (%s)", err, out)
	}
	if payload := decodeJSON(t, out); payload["outcome"] != "submitted_for_review" {
		t.Fatalf("a pending proposal is with the admins, and publish must say so: %s", out)
	}
	if len(f.reviewRequested) != 0 {
		t.Fatalf("a proposal is not a draft and has no review request to make, got %v", f.reviewRequested)
	}
	if len(f.mod.committed) != 0 {
		t.Fatalf("a non-admin must not have committed a shared module, got %v", f.mod.committed)
	}
}

// TestModulePushRefusesSomebodyElsesInFlightRow is the other half of the
// own-draft read leg: it admits the CALLER'S own in-flight row and nobody
// else's, and the refusal is a NOT-FOUND rather than a permission error.
//
// A peer must not be able to learn that a colleague has an unsubmitted draft, so
// the CLI gets the same answer for "somebody else's draft" as for "no such
// module" — which is exactly why its message names both causes.
func TestModulePushRefusesSomebodyElsesInFlightRow(t *testing.T) {
	f := newFakeInstance(t)
	signInAsModuleUser(t, f)
	f.AddModule(&api.Module{
		ID: "module-theirs", Lifecycle: api.LifecycleDraft, Name: "their_helpers",
		Title: "Their helpers", DrafterUserID: "usr-somebody-else",
	}, api.ModuleFile{Path: moduleInitFile, Content: fakeModuleInitSeed})

	root := moduleTestFolder(t, f.Key(), wfdir.Binding{ModuleID: "module-theirs", FeatureID: "collection-1"},
		map[string]string{moduleInitFile: "", "tracker.py": "x = 1\n"})

	_, err := runCLI(t, root, "module", "push", "--json")
	if err == nil {
		t.Fatal("a push at somebody else's unpublished draft must be refused")
	}
	if strings.Contains(strings.ToLower(err.Error()), "forbidden") ||
		strings.Contains(strings.ToLower(err.Error()), "permission") {
		t.Fatalf("the refusal must not disclose that the row exists, got: %v", err)
	}
	if !strings.Contains(err.Error(), "cannot read module module-theirs") {
		t.Fatalf("the refusal must name the binding it could not resolve, got: %v", err)
	}
	// Nothing was written — the read that failed happens before the first byte.
	if len(f.mod.created) != 0 || len(f.fileWrites) != 0 {
		t.Fatalf("nothing may have been written, got created=%v writes=%v", f.mod.created, f.fileWrites)
	}
}

// TestModulePushRefusesMarkersBeforeWriting pins the local marker guard's
// POSITION as much as its verdict.
//
// A module is marker-free, and the server refuses one file at a time — so a push
// that let the folder through would create the module, write files until it hit
// the offending one, and stop, leaving a half-written draft to explain a rule the
// folder broke before the first request. The refusal has to happen with the
// network untouched.
func TestModulePushRefusesMarkersBeforeWriting(t *testing.T) {
	f := newFakeInstance(t)
	signInAsModuleUser(t, f)

	root := t.TempDir()
	if _, err := runCLI(t, root, "module", "init", "emab_tracker",
		"--feature", "collection-1", "--title", "EMAB tracker"); err != nil {
		t.Fatalf("module init: %v", err)
	}
	if err := wfdir.WriteFile(root, "tracker.py", "rows = {{ ref('table-1') }}\n"); err != nil {
		t.Fatalf("write tracker.py: %v", err)
	}

	_, err := runCLI(t, root, "module", "push", "--json")
	if err == nil {
		t.Fatal("a module file carrying a Ronja marker must be refused")
	}
	if !strings.Contains(err.Error(), "tracker.py") || !strings.Contains(err.Error(), "{{ ref(...) }}") {
		t.Fatalf("the refusal must name the file and the marker, got: %v", err)
	}
	if !strings.Contains(err.Error(), "module validate") {
		t.Fatalf("the refusal must point at the command that lists them all, got: %v", err)
	}
	// The whole point of the guard's position: no module, no files, no binding.
	if len(f.mod.created) != 0 || len(f.fileWrites) != 0 {
		t.Fatalf("the refusal must precede every write, got created=%v writes=%v", f.mod.created, f.fileWrites)
	}
}

// setModuleManifestTitle edits the folder's manifest the way a person would,
// leaving the bindings and the lock file alone.
func setModuleManifestTitle(t *testing.T, root, title string) {
	t.Helper()
	manifest, err := wfdir.LoadManifest(root, wfdir.ModuleKind)
	if err != nil {
		t.Fatalf("load manifest: %v", err)
	}
	manifest.Title = title
	if err := wfdir.SaveManifest(root, manifest); err != nil {
		t.Fatalf("save manifest: %v", err)
	}
}

// A `dependencies` block in a module folder binds nothing and is refused by
// nobody: checkDependencies validates its shape, CheckDeclarations validates its
// kinds, and neither knows that the folder it is validating has no markers for
// an alias to fill. So an author who wrote one — most plausibly by copying a
// workflow's ronja.json when splitting a helper out of it — gets silence from
// every command and waits for a binding that is never coming.
//
// `module status` is where they are already looking, so it says so. It is a
// NOTE and not a problem: the push still works, and filing it under the key
// that means "your push will be refused" would send them to fix something that
// is not blocking them.
func TestModuleStatusFlagsAnInertDependenciesBlock(t *testing.T) {
	f := newFakeInstance(t)
	signIn(t, f)

	root := moduleTestFolder(t, f.Key(), wfdir.Binding{FeatureID: "collection-1"}, map[string]string{
		"__init__.py": "",
	})
	// Written straight into the committed file, because that is the only way one
	// gets there — no CLI verb produces it for a module.
	m, err := wfdir.LoadManifest(root, wfdir.ModuleKind)
	if err != nil {
		t.Fatalf("load manifest: %v", err)
	}
	m.Dependencies = map[string]wfdir.Dependency{"orders": {Kind: "table"}}
	if err := wfdir.SaveManifest(root, m); err != nil {
		t.Fatalf("save manifest: %v", err)
	}

	out, err := runCLI(t, root, "module", "status", "--json")
	if err != nil {
		t.Fatalf("module status: %v (%s)", err, out)
	}
	payload := decodeJSON(t, out)
	inert, _ := payload["inertDependencies"].([]any)
	if len(inert) != 1 || inert[0] != "orders" {
		t.Fatalf("inertDependencies = %v, want [orders]", payload["inertDependencies"])
	}
	// It must NOT be filed as the thing that stops a push — that key means
	// something else, and a push here succeeds.
	if problem, ok := payload["localProblem"]; ok {
		t.Errorf("an inert dependency was reported as a push-blocking problem: %v", problem)
	}

	// And a module folder with no such block carries no note at all, so the
	// ordinary status is unchanged down to the key.
	m.Dependencies = nil
	if err := wfdir.SaveManifest(root, m); err != nil {
		t.Fatalf("save manifest: %v", err)
	}
	out, err = runCLI(t, root, "module", "status", "--json")
	if err != nil {
		t.Fatalf("module status: %v (%s)", err, out)
	}
	if _, ok := decodeJSON(t, out)["inertDependencies"]; ok {
		t.Error("a folder declaring no dependencies grew an inertDependencies key")
	}
}
