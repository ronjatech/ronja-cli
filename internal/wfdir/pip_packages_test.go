package wfdir

import (
	"os"
	"reflect"
	"strings"
	"testing"
)

func TestPipPackagesThreeStatesRoundTrip(t *testing.T) {
	for _, tc := range []struct {
		name    string
		compact string
		manages bool
		want    []string
	}{
		{"absent", `{"kind":"workflow","title":"x","entrypoint":"main.py","instances":[]}`, false, nil},
		{"empty", `{"kind":"workflow","title":"x","entrypoint":"main.py","pipPackages":[],"instances":[]}`, true, []string{}},
		{"list", `{"formatVersion":8,"kind":"workflow","title":"x","entrypoint":"main.py","pipPackages":["google-auth"],"instances":[]}`, true, []string{"google-auth"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			body := canonicalManifest(t, tc.compact)
			if got := loadSave(t, body); got != body {
				t.Errorf("round trip changed the file\n got:\n%s\nwant:\n%s", got, body)
			}
			root := t.TempDir()
			write(t, root, ManifestName, body)
			m, err := LoadManifest(root, WorkflowKind)
			if err != nil {
				t.Fatalf("load: %v", err)
			}
			if m.ManagesPipPackages() != tc.manages {
				t.Errorf("ManagesPipPackages = %v, want %v", m.ManagesPipPackages(), tc.manages)
			}
			if tc.manages && !reflect.DeepEqual(m.DeclaredPipPackages(), tc.want) {
				t.Errorf("DeclaredPipPackages = %#v, want %#v", m.DeclaredPipPackages(), tc.want)
			}
		})
	}
}

func TestDeclaredPipPackagesAreCleanedAsTheServerCleansThem(t *testing.T) {
	m := &Manifest{}
	m.SetPipPackages([]string{" google-auth ", "", "requests", "google-auth", "  "})
	want := []string{"google-auth", "requests"}
	if got := m.DeclaredPipPackages(); !reflect.DeepEqual(got, want) {
		t.Errorf("DeclaredPipPackages = %#v, want %#v", got, want)
	}
}

func TestANonEmptyPipPackagesListStampsVersionEight(t *testing.T) {
	m := &Manifest{Kind: KindWorkflow, Title: "x", Entrypoint: "main.py"}
	m.SetPipPackages(nil)
	if got := m.StampedFormatVersion(); got != 1 {
		t.Errorf("an empty list stamps %d, want 1 (no version needed)", got)
	}
	m.SetPipPackages([]string{"google-auth"})
	if got := m.StampedFormatVersion(); got != manifestVersionPipPackages {
		t.Errorf("a declared list stamps %d, want %d", got, manifestVersionPipPackages)
	}
	m.ID = "folder-1"
	if got := m.StampedFormatVersion(); got != manifestVersionPipPackages {
		t.Errorf("a declared list beside a folder id stamps %d, want the higher %d", got, manifestVersionPipPackages)
	}
}

func TestAPipPackagesManifestIsRefusedByAVersionSevenBuild(t *testing.T) {
	root := t.TempDir()
	m := &Manifest{Kind: KindWorkflow, Title: "x", Entrypoint: "main.py"}
	m.SetPipPackages([]string{"google-auth"})
	if err := SaveManifest(root, m); err != nil {
		t.Fatalf("save: %v", err)
	}
	raw, err := os.ReadFile(ManifestPath(root))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(string(raw), "{\n  \"formatVersion\": 8,") {
		t.Errorf("a declared list must lead with formatVersion 8:\n%s", raw)
	}
	err = checkFormatVersion(ManifestPath(root), raw, manifestVersionAddressDeps)
	if err == nil || !strings.Contains(err.Error(), "format version 8") {
		t.Fatalf("a version-7 build = %v, want a format-version refusal", err)
	}
	if _, err := LoadManifest(root, WorkflowKind); err != nil {
		t.Errorf("this build refuses the manifest it just wrote: %v", err)
	}
}

func TestAnUnderStampedPipPackagesListLoadsAndIsRefusedAtAcceptance(t *testing.T) {
	load := func(t *testing.T, compact string) *Manifest {
		t.Helper()
		root := t.TempDir()
		write(t, root, ManifestName, canonicalManifest(t, compact))
		m, err := LoadManifest(root, WorkflowKind)
		if err != nil {
			t.Fatalf("an under-stamped folder must still open: %v", err)
		}
		return m
	}
	const want = `ronja.json declares pipPackages but its formatVersion is 1: set "formatVersion": 8, so an older ronja refuses this folder instead of deploying it without its packages`

	m := load(t, `{"kind":"workflow","title":"x","entrypoint":"main.py","pipPackages":["requests"],"instances":[]}`)
	if err := m.CheckDeclarations(); err == nil || err.Error() != want {
		t.Errorf("CheckDeclarations() = %v, want %q", err, want)
	}

	m = load(t, `{"formatVersion":3,"kind":"workflow","title":"x","entrypoint":"main.py","pipPackages":["requests"],"instances":[]}`)
	if err := m.CheckDeclarations(); err == nil || !strings.Contains(err.Error(), "its formatVersion is 3: set \"formatVersion\": 8") {
		t.Errorf("CheckDeclarations() = %v, want the declared version named", err)
	}

	m = load(t, `{"kind":"workflow","title":"x","entrypoint":"main.py","pipPackages":[],"instances":[]}`)
	if err := m.CheckDeclarations(); err != nil {
		t.Errorf("an empty list at version 1 was refused: %v", err)
	}

	m = load(t, `{"formatVersion":8,"kind":"workflow","title":"x","entrypoint":"main.py","pipPackages":["requests"],"instances":[]}`)
	if err := m.CheckDeclarations(); err != nil {
		t.Errorf("a list at version 8 was refused: %v", err)
	}

	m = load(t, `{"kind":"workflow","title":"x","entrypoint":"main.py","pipPackages":["requests"],"dependencies":{"orders":{"kind":"table"}},"instances":[]}`)
	if err := m.CheckDeclarations(); err == nil || err.Error() != want {
		t.Errorf("a version-1 folder with dependencies and a list: CheckDeclarations() = %v, want the one refusal naming 8", err)
	}
}
