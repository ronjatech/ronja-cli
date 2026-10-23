package wfdir

import (
	"os"
	"sort"
	"strings"
	"testing"
)

// A python manifest — no language, or an explicit "python" — round-trips byte
// for byte and is never stamped: the language key costs no existing folder a
// newer CLI.
func TestPythonManifestRoundTripsUnchanged(t *testing.T) {
	for _, compact := range []string{
		`{"kind":"workflow","title":"x","entrypoint":"main.py","runtime":3,"instances":[]}`,
		`{"kind":"workflow","title":"x","language":"python","entrypoint":"main.py","runtime":3,"instances":[]}`,
	} {
		body := canonicalManifest(t, compact)
		if got := loadSave(t, body); got != body {
			t.Errorf("a python manifest must round-trip unchanged\n got:\n%s\nwant:\n%s", got, body)
		}
	}
}

// A js manifest is stamped format version 4, leading the file, and reads back
// as js with main.js as its default entrypoint.
func TestJSManifestStampsVersionFour(t *testing.T) {
	root := t.TempDir()
	write(t, root, ManifestName, canonicalManifest(t, `{"kind":"workflow","title":"x","language":"js","runtime":4,"instances":[]}`))
	m, err := LoadManifest(root, WorkflowKind)
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	if m.Entrypoint != "main.js" {
		t.Errorf("entrypoint = %q, want main.js for a js folder that names none", m.Entrypoint)
	}
	if err := SaveManifest(root, m); err != nil {
		t.Fatalf("save: %v", err)
	}
	raw, err := os.ReadFile(ManifestPath(root))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(string(raw), "{\n  \"formatVersion\": 4,") {
		t.Errorf("a js manifest must lead with formatVersion 4:\n%s", raw)
	}
	if m.StampedFormatVersion() != manifestVersionLanguage {
		t.Errorf("StampedFormatVersion = %d, want %d", m.StampedFormatVersion(), manifestVersionLanguage)
	}
	reloaded, err := LoadManifest(root, WorkflowKind)
	if err != nil {
		t.Fatalf("reload: %v", err)
	}
	if !reloaded.IsJS() {
		t.Errorf("language = %q after a round trip", reloaded.Language)
	}
}

// What the bump buys: a build that reads only up to format version 3 — every
// CLI before JavaScript workflows — refuses a js folder rather than reading it
// as Python.
func TestJSManifestIsRefusedByAVersionThreeBuild(t *testing.T) {
	root := t.TempDir()
	write(t, root, ManifestName, canonicalManifest(t, `{"kind":"workflow","title":"x","language":"js","runtime":4,"instances":[]}`))
	m, err := LoadManifest(root, WorkflowKind)
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	if err := SaveManifest(root, m); err != nil {
		t.Fatalf("save: %v", err)
	}
	raw, err := os.ReadFile(ManifestPath(root))
	if err != nil {
		t.Fatal(err)
	}
	err = checkFormatVersion(ManifestPath(root), raw, manifestVersionDependencies)
	if err == nil || !strings.Contains(err.Error(), "format version 4") {
		t.Fatalf("a version-3 build = %v, want a format-version refusal", err)
	}
}

func TestLanguageRefusals(t *testing.T) {
	for _, tc := range []struct {
		name    string
		compact string
		kind    Kind
		want    string
	}{
		{"unknown language", `{"kind":"workflow","title":"x","language":"ts","instances":[]}`, WorkflowKind, `which this CLI does not know`},
		{"js on runtime 3", `{"kind":"workflow","title":"x","language":"js","runtime":3,"instances":[]}`, WorkflowKind, "runs on runtime 4 only"},
		{"language on a pipeline", `{"kind":"pipeline","title":"x","language":"js","instances":[]}`, PipelineKind, "only a workflow folder has one"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			root := t.TempDir()
			write(t, root, ManifestName, canonicalManifest(t, tc.compact))
			_, err := LoadManifest(root, tc.kind)
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("load = %v, want %q", err, tc.want)
			}
		})
	}
}

func TestForManifestNarrowsOnlyAJSWorkflow(t *testing.T) {
	js := &Manifest{Kind: KindWorkflow, Language: LanguageJS}
	py := &Manifest{Kind: KindWorkflow}
	if got := WorkflowKind.ForManifest(js); got.DefaultEntrypoint != "main.js" || len(got.SkipDirs) == 0 {
		t.Errorf("a js workflow opened as %+v, want WorkflowJSKind", got)
	}
	if got := WorkflowKind.ForManifest(py); got.DefaultEntrypoint != "main.py" || len(got.SkipDirs) != 0 {
		t.Errorf("a python workflow opened as %+v, want WorkflowKind unchanged", got)
	}
	if got := DataAppKind.ForManifest(js); got.Name != KindDataApp {
		t.Errorf("a data app narrowed to %+v", got)
	}
}

// The JavaScript kind syncs everything a python workflow folder would, minus
// node_modules/, dist/ and build/; the python kind keeps syncing them.
func TestJSKindSkipsNodeModulesDistAndBuild(t *testing.T) {
	root := t.TempDir()
	for path, body := range map[string]string{
		"main.js":                      "export async function main() {}\n",
		"lib/util.mjs":                 "export const x = 1\n",
		"package.json":                 "{}\n",
		"package-lock.json":            "{}\n",
		"queries/orders.sql":           "SELECT 1\n",
		"node_modules/lodash/index.js": "module.exports = {}\n",
		"dist/bundle.js":               "x\n",
		"build/out.js":                 "x\n",
	} {
		write(t, root, path, body)
	}

	js, err := Enumerate(root, WorkflowJSKind)
	if err != nil {
		t.Fatal(err)
	}
	if got, want := sortedKeys(js.Files), []string{"lib/util.mjs", "main.js", "package-lock.json", "package.json", "queries/orders.sql"}; strings.Join(got, ",") != strings.Join(want, ",") {
		t.Errorf("js kind synced %v, want %v", got, want)
	}
	var skipped []string
	for _, s := range js.Skipped {
		skipped = append(skipped, s.Path)
	}
	if strings.Join(skipped, ",") != "build/,dist/,node_modules/" {
		t.Errorf("js kind skipped %v, want build/, dist/ and node_modules/", skipped)
	}

	py, err := Enumerate(root, WorkflowKind)
	if err != nil {
		t.Fatal(err)
	}
	if len(py.Files) != 8 {
		t.Errorf("python kind synced %v, want every file, as before", sortedKeys(py.Files))
	}
}

func sortedKeys(m map[string]string) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

// The CLI reads a JavaScript source as the server does, by a case-sensitive
// suffix.
func TestIsJSSourceIsCaseSensitive(t *testing.T) {
	for path, want := range map[string]bool{"main.js": true, "lib/a.mjs": true, "main.JS": false, "main.Mjs": false, "main.json": false} {
		if got := IsJSSource(path); got != want {
			t.Errorf("IsJSSource(%q) = %v, want %v", path, got, want)
		}
	}
}
