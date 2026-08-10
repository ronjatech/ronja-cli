package wfdir

import (
	"os"
	"strings"
	"testing"

	"github.com/ronjatech/ronja-cli/internal/api"
)

// ── Exclusion symmetry ──────────────────────────────────────────────────────

// TestExclusionsAreSymmetric is the guard against the phantom-deletion class.
//
// A baseline is a PROMISE: .ronja/state.json says "these paths are on disk with
// these hashes", and every later command believes it. So if Enumerate excludes a
// path that CheckLocalPaths would accept, a clone writes the file, never
// enumerates it again, reads it as a LOCAL DELETION on the next status — and the
// next push acts on that by deleting it server-side.
//
// The two therefore have to answer identically for every path, and that is what
// this asserts directly rather than testing each side in isolation.
func TestExclusionsAreSymmetric(t *testing.T) {
	paths := []string{
		"App.tsx",
		"lib/chart.tsx",
		"node_modules/react/index.js",
		"dist/bundle.js",
		"build/out.js",
		".env",
		".config/thing.tsx",
		"ronja.json",
		".ronja/state.json",
		"src/build/helper.tsx", // a SkipDir name at depth
		"main.py",
	}
	for _, kind := range []Kind{WorkflowKind, DataAppKind} {
		for _, path := range paths {
			excluded := StructuralExclusion(path, kind) != ""
			// CheckLocalPaths refuses a set it cannot lay out; a single excluded
			// path is exactly such a set.
			refused := CheckLocalPaths([]string{path}, kind) != nil
			if excluded != refused {
				t.Errorf("%s / %q: Enumerate excludes=%v but CheckLocalPaths refuses=%v — a mismatch here is a baseline that claims a file is on disk which the walk will never see again",
					kind.Name, path, excluded, refused)
			}
		}
	}
}

// TestDataAppExcludesBuildOutput: a TSX folder plausibly holds node_modules/ or
// dist/. Without the exclusion, the valid-path files inside them DO get pushed
// and the server's 100-file cap catches it late, by count, naming nothing
// useful.
//
// The exclusion is REPORTED, not silent — a file the user believes is part of
// the app and which the CLI quietly ignores is the worst kind of surprise.
func TestDataAppExcludesBuildOutput(t *testing.T) {
	root := t.TempDir()
	write(t, root, "App.tsx", "export default function App(){}")
	write(t, root, "node_modules/react/index.js", "module.exports = {}")
	write(t, root, "dist/bundle.js", "// generated")
	write(t, root, "build/out.js", "// generated")

	enumeration, err := Enumerate(root, DataAppKind)
	if err != nil {
		t.Fatalf("Enumerate: %v", err)
	}
	if len(enumeration.Files) != 1 {
		t.Errorf("only the source file should be syncable, got %v", enumeration.Files)
	}
	// Reported once BY DIRECTORY rather than once per file: a dependency tree
	// would otherwise drown the report it is meant to inform.
	var reported []string
	for _, s := range enumeration.Skipped {
		reported = append(reported, s.Path)
	}
	for _, want := range []string{"node_modules/", "dist/", "build/"} {
		if !containsString(reported, want) {
			t.Errorf("%s should be reported as skipped, got %v", want, reported)
		}
	}
	if len(enumeration.Skipped) != 3 {
		t.Errorf("expected one entry per directory, got %v", reported)
	}
}

// TestWorkflowDoesNotExcludeBuildOutput: the exclusions are per-kind, and a
// Python folder has no conventional build output — excluding a directory nobody
// has would only be a rule to trip over.
func TestWorkflowDoesNotExcludeBuildOutput(t *testing.T) {
	root := t.TempDir()
	write(t, root, "main.py", "print(1)")
	write(t, root, "build/helper.py", "print(2)")

	enumeration, err := Enumerate(root, WorkflowKind)
	if err != nil {
		t.Fatalf("Enumerate: %v", err)
	}
	if len(enumeration.Files) != 2 {
		t.Errorf("a workflow folder syncs build/, got %v", enumeration.Files)
	}
}

// TestCheckLocalPathsRefusesRemoteBuildOutput is the clone-side half.
//
// dist/bundle.js is a perfectly legal path SERVER-side (the grammar allows it),
// so an app carrying one would be cloned onto disk and then be invisible to
// every later command. Refused before a byte is written, with the remedy named.
func TestCheckLocalPathsRefusesRemoteBuildOutput(t *testing.T) {
	err := CheckLocalPaths([]string{"App.tsx", "dist/bundle.js"}, DataAppKind)
	if err == nil {
		t.Fatal("a remote dist/ file cannot be held in a synced folder and must be refused")
	}
	if !strings.Contains(err.Error(), "dist/bundle.js") {
		t.Errorf("the refusal should name the offending path, got %v", err)
	}
	if !strings.Contains(err.Error(), "data app") {
		t.Errorf("the refusal should name the kind, got %v", err)
	}
}

// ── Kind gating ─────────────────────────────────────────────────────────────

// TestLoadManifestRefusesTheOtherKind: the two folder types are indistinguishable
// by shape, so this refusal is what stops `ronja wf push` creating a workflow out
// of a data app's TSX — and the reverse, which would overwrite an app's source
// with Python.
func TestLoadManifestRefusesTheOtherKind(t *testing.T) {
	root := t.TempDir()
	if err := SaveManifest(root, &Manifest{Kind: KindDataApp, Title: "App", Entrypoint: "App.tsx"}); err != nil {
		t.Fatal(err)
	}
	if _, err := LoadManifest(root, DataAppKind); err != nil {
		t.Fatalf("the matching kind must load: %v", err)
	}
	_, err := LoadManifest(root, WorkflowKind)
	if err == nil {
		t.Fatal("a data-app folder must not load as a workflow")
	}
	// Names the command that WOULD work: a wrong kind is almost always the right
	// folder and the wrong verb.
	if !strings.Contains(err.Error(), "ronja app") {
		t.Errorf("the refusal should point at `ronja app`, got %v", err)
	}
}

// TestLoadManifestDefaultsEntrypointPerKind.
func TestLoadManifestDefaultsEntrypointPerKind(t *testing.T) {
	for _, tc := range []struct {
		kind Kind
		want string
	}{
		{WorkflowKind, "main.py"},
		{DataAppKind, "App.tsx"},
	} {
		root := t.TempDir()
		if err := SaveManifest(root, &Manifest{Kind: tc.kind.Name, Title: "X"}); err != nil {
			t.Fatal(err)
		}
		m, err := LoadManifest(root, tc.kind)
		if err != nil {
			t.Fatalf("%s: %v", tc.kind.Name, err)
		}
		if m.Entrypoint != tc.want {
			t.Errorf("%s entrypoint = %q, want %q", tc.kind.Name, m.Entrypoint, tc.want)
		}
	}
}

// ── The Access three-state ──────────────────────────────────────────────────

// TestManifestAccessThreeState pins the distinction the pointer exists for.
//
// Absent is NOT "grants nothing". A folder created before allowlists were part
// of the manifest — or one deliberately leaving them to the web UI — must have
// push leave them alone, because treating absent as an empty declaration would
// REVOKE the app's access on the first push after upgrading.
func TestManifestAccessThreeState(t *testing.T) {
	var m Manifest

	if m.ManagesAccess() {
		t.Error("an absent access block must not read as managed")
	}
	if !m.DeclaredAccess().IsEmpty() {
		t.Error("an unmanaged folder reads as granting nothing, for display only")
	}

	m.SetAccess(api.DataAppAccess{})
	if !m.ManagesAccess() {
		t.Error("an explicitly empty declaration IS managed — it means 'grants nothing'")
	}

	m.SetAccess(api.DataAppAccess{AllowedTableIDs: []string{"table-b", "table-a"}})
	if !m.ManagesAccess() {
		t.Error("a populated declaration is managed")
	}
	// Normalized on the way in, so a committed manifest never round-trips a
	// different spelling of the same grant.
	got := m.DeclaredAccess().AllowedTableIDs
	if len(got) != 2 || got[0] != "table-a" {
		t.Errorf("declared tables should be sorted, got %v", got)
	}
}

// TestManifestAccessSurvivesRoundTrip: the access block's json tags are part of
// the ronja.json FILE FORMAT, not just the wire, so a rename is a breaking
// change to committed manifests. This is what would catch one.
func TestManifestAccessRoundTrip(t *testing.T) {
	root := t.TempDir()
	m := &Manifest{Kind: KindDataApp, Title: "App", Entrypoint: "App.tsx"}
	m.SetAccess(api.DataAppAccess{
		AllowedTableIDs:    []string{"table-1"},
		AllowedSecretIDs:   []string{"secret-1"},
		AllowedAgentIDs:    []string{"agent-1"},
		AllowedWorkflowIDs: []string{"workflow-1"},
		AllowedCodexIDs:    []string{"codex-1"},
		AllowedMetricIDs:   []string{"table-metric-1"},
		Capabilities:       []string{"query_ronja"},
	})
	if err := SaveManifest(root, m); err != nil {
		t.Fatal(err)
	}

	// The committed file is one people read and diff, so the keys are asserted
	// literally rather than only through a decode.
	body, err := os.ReadFile(ManifestPath(root))
	if err != nil {
		t.Fatal(err)
	}
	for _, key := range []string{
		`"access"`, `"allowedTableIDs"`, `"allowedSecretIDs"`, `"allowedAgentIDs"`,
		`"allowedWorkflowIDs"`, `"allowedCodexIDs"`, `"allowedMetricIDs"`, `"capabilities"`,
	} {
		if !strings.Contains(string(body), key) {
			t.Errorf("manifest is missing %s:\n%s", key, body)
		}
	}

	loaded, err := LoadManifest(root, DataAppKind)
	if err != nil {
		t.Fatal(err)
	}
	if !api.SameAccess(loaded.DeclaredAccess(), m.DeclaredAccess()) {
		t.Errorf("access did not round-trip: %+v vs %+v", loaded.DeclaredAccess(), m.DeclaredAccess())
	}
}

// TestBindingResourceIDIsPerKind: one manifest can only ever carry one of the
// two ids, and the accessor is what keeps callers from reaching for the wrong
// field.
func TestBindingResourceIDIsPerKind(t *testing.T) {
	b := Binding{}.WithResourceID(DataAppKind, "data_app-1")
	if b.DataAppID != "data_app-1" || b.WorkflowID != "" {
		t.Errorf("a data-app binding must set only dataAppID, got %+v", b)
	}
	if got := b.ResourceID(DataAppKind); got != "data_app-1" {
		t.Errorf("ResourceID(DataAppKind) = %q", got)
	}
	if got := b.ResourceID(WorkflowKind); got != "" {
		t.Errorf("a data-app binding has no workflow id, got %q", got)
	}
}

// TestSlugFallsBackPerKind: a title made entirely of characters the slug drops
// still has to produce a directory name.
func TestSlugFallsBackPerKind(t *testing.T) {
	if got := Slug("📊", KindDataApp); got != KindDataApp {
		t.Errorf("Slug fallback = %q, want %q", got, KindDataApp)
	}
	if got := Slug("Revenue explorer", KindDataApp); got != "revenue-explorer" {
		t.Errorf("Slug = %q", got)
	}
}

func containsString(haystack []string, needle string) bool {
	for _, s := range haystack {
		if s == needle {
			return true
		}
	}
	return false
}
