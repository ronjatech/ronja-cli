package commands

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/ronjatech/ronja-cli/internal/wfdir"
)

// These walk wfdir.AllKinds() rather than a list, so a new kind cannot be added
// without folderKindAt and localWork knowing it; neither failure is loud.

func TestFolderKindAtRecognisesEveryRegisteredKind(t *testing.T) {
	for _, want := range wfdir.AllKinds() {
		t.Run(want.Name, func(t *testing.T) {
			root := t.TempDir()
			writeKindManifest(t, root, want.Name)

			got, known, err := folderKindAt(root)
			if err != nil {
				t.Fatalf("folderKindAt: %v", err)
			}
			if !known {
				t.Fatalf("a %s folder reads as an unknown kind — `ronja sync` scores it unreadable and the whole tree exits 2", want.Name)
			}
			if got.Name != want.Name {
				t.Errorf("folderKindAt = %q, want %q", got.Name, want.Name)
			}
		})
	}

	root := t.TempDir()
	writeKindManifest(t, root, "dashboard")
	if _, known, err := folderKindAt(root); err != nil || known {
		t.Errorf("an unregistered kind must read as unknown (known=%v, err=%v)", known, err)
	}
}

func TestLocalWorkNamesEveryRegisteredKind(t *testing.T) {
	for _, want := range wfdir.AllKinds() {
		t.Run(want.Name, func(t *testing.T) {
			dir := t.TempDir()
			writeKindManifest(t, dir, want.Name)
			t.Chdir(dir)

			found := localWork()
			if found == nil {
				t.Fatal("localWork saw nothing in a folder holding a manifest")
			}
			if found.Kind != want.Name {
				t.Fatalf("localWork read the folder as kind %q, want %q — the note would name another loop's commands", found.Kind, want.Name)
			}
		})
	}
}

func TestFolderKindLabelsCoversTheRegistry(t *testing.T) {
	labels := folderKindLabels()
	for _, kind := range wfdir.AllKinds() {
		if !strings.Contains(labels, kind.Label) {
			t.Errorf("folderKindLabels() = %q, missing %q", labels, kind.Label)
		}
	}
}

func writeKindManifest(t *testing.T, root, kind string) {
	t.Helper()
	body, err := json.Marshal(map[string]any{
		"kind": kind, "title": "X", "instances": []any{},
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, wfdir.ManifestName), append(body, '\n'), 0o600); err != nil {
		t.Fatalf("seed manifest: %v", err)
	}
}
