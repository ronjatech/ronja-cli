package commands

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/ronjatech/ronja-cli/internal/wfdir"
)

// The two KIND-AGNOSTIC readers, walked against wfdir's registry.
//
// Both were hand-kept switches over three kinds, and the fourth was added
// without either. Neither failure is loud, and both mislead rather than refuse:
//
//   - folderKindAt reported an automation folder as `known=false`, which
//     syncWalk turns into `unreadable` — a whole tree exiting 2 over a folder
//     this build understands perfectly.
//   - localWork left Kind empty, and printLocalWork's fallback then announced
//     "this is a workflow folder" and printed the `ronja wf` verb list, which is
//     precisely the misrouting Kind.Command was introduced to stop, delivered in
//     the one message a lost caller reads.
//
// So this walks wfdir.AllKinds() rather than listing anything: a fifth kind
// cannot be added without answering for both readers.

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

	// The other half of `known`: a kind this build has genuinely never heard of
	// must still come back false, or the flag says nothing.
	root := t.TempDir()
	writeKindManifest(t, root, "dashboard")
	if _, known, err := folderKindAt(root); err != nil || known {
		t.Errorf("an unregistered kind must read as unknown (known=%v, err=%v)", known, err)
	}
}

// TestLocalWorkNamesEveryRegisteredKind: `ronja context` must never describe a
// folder as a kind it is not, and must never offer it another loop's verbs.
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

// TestFolderKindLabelsCoversTheRegistry pins the sentence `ronja sync` prints
// about what it walks. It said "workflow, data-app and pipeline" while walking
// four kinds, which is the sort of claim a reader checks their folder against
// and then goes looking for a different tool.
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
