package wfdir

import (
	"os"
	"strings"
	"testing"

	"github.com/ronjatech/ronja-cli/internal/api"
)

// ── Exclusion symmetry ──────────────────────────────────────────────────────

// ⚠️ Enumerate and CheckLocalPaths must agree on every path: one Enumerate
// excludes but a clone writes reads as a local deletion, which the next push
// deletes server-side.
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
			refused := CheckLocalPaths([]string{path}, kind) != nil
			if excluded != refused {
				t.Errorf("%s / %q: Enumerate excludes=%v but CheckLocalPaths refuses=%v — a mismatch here is a baseline that claims a file is on disk which the walk will never see again",
					kind.Name, path, excluded, refused)
			}
		}
	}
}

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
	// Reported once per directory, not per file.
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

// dist/bundle.js is legal server-side, so a clone would write it and every
// later command would never see it.
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

// The folder kinds are indistinguishable by shape; this refusal is what stops a
// workflow push from overwriting an app with Python, or the reverse.
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
	if !strings.Contains(err.Error(), "ronja app") {
		t.Errorf("the refusal should point at `ronja app`, got %v", err)
	}
}

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

// Absent is not "grants nothing": treating it as an empty declaration would
// revoke the app's access on the next push.
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
	got := m.DeclaredAccess().AllowedTableIDs
	if len(got) != 2 || got[0] != "table-a" {
		t.Errorf("declared tables should be sorted, got %v", got)
	}
}

// The access block's json tags are ronja.json's committed file format, so a
// rename breaks existing manifests.
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

	// Asserted literally, since people read and diff the committed file.
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

func TestBindingResourceIDIsPerKind(t *testing.T) {
	b, err := Binding{}.WithResourceID(DataAppKind, "data_app-1")
	if err != nil {
		t.Fatalf("WithResourceID(DataAppKind): %v", err)
	}
	if b.DataAppID != "data_app-1" || b.WorkflowID != "" {
		t.Errorf("a data-app binding must set only dataAppID, got %+v", b)
	}
	if got, err := b.ResourceID(DataAppKind); err != nil || got != "data_app-1" {
		t.Errorf("ResourceID(DataAppKind) = %q, %v", got, err)
	}
	if got, err := b.ResourceID(WorkflowKind); err != nil || got != "" {
		t.Errorf("a data-app binding has no workflow id, got %q, %v", got, err)
	}
}

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
