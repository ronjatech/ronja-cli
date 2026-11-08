package commands

import (
	"encoding/json"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"testing"

	"github.com/ronjatech/ronja-cli/internal/wfdir"
)

var mintedIDPattern = regexp.MustCompile(`[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}`)

// committedTree is every committed file under root (the local .ronja/ left
// out), with the instance URL and minted folder ids made comparable.
func committedTree(t *testing.T, root, url string) string {
	t.Helper()
	var out []string
	err := filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() && d.Name() == wfdir.StateDirName {
			return filepath.SkipDir
		}
		if d.IsDir() {
			return nil
		}
		rel, _ := filepath.Rel(root, path)
		body := strings.ReplaceAll(readFile(t, path), url, "URL")
		out = append(out, rel+"\n"+mintedIDPattern.ReplaceAllString(body, "FOLDER-ID"))
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	sort.Strings(out)
	return strings.Join(out, "\n---\n")
}

// requireNoSpaceRecorded fails when any ronja.json under root records a Space
// or needs format version 9.
func requireNoSpaceRecorded(t *testing.T, root string) {
	t.Helper()
	_ = filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() || d.Name() != wfdir.ManifestName {
			return err
		}
		body := readFile(t, path)
		var probe struct {
			FormatVersion int `json:"formatVersion"`
		}
		if err := json.Unmarshal([]byte(body), &probe); err != nil {
			t.Fatal(err)
		}
		if strings.Contains(body, `"spaceID"`) || strings.Contains(body, `"space"`) || probe.FormatVersion > 8 {
			t.Errorf("%s records a Space or format %d:\n%s", path, probe.FormatVersion, body)
		}
		return nil
	})
}

// A feature in the organization's default Space, renamed "Produktion", is
// imported exactly as on an instance with no Spaces at all: every folder on
// stack "main", no Space recorded, the same bytes at the same format version.
func TestAMainImportIsWrittenAsBeforeSpaces(t *testing.T) {
	imported := func(spaces bool) string {
		f := importFixture(t)
		if spaces {
			f.featureSpace = map[string]string{importFeature: "space-main"}
			f.wf.spaceNames = map[string]string{"space-main": "Produktion"}
			f.wf.defaultSpace = "space-main"
		}
		dir, report := runImport(t, f)
		if report.Stack != "main" {
			t.Fatalf("stack = %q, want main", report.Stack)
		}
		requireNoSpaceRecorded(t, dir)
		return committedTree(t, dir, f.URL())
	}
	if before, after := imported(false), imported(true); before != after {
		t.Fatalf("an import from the default Space differs from one with no Spaces:\nwithout Spaces:\n%s\nwith:\n%s", before, after)
	}
}

// `init --stack main` on a feature in the default Space writes what it writes
// on an instance with no Spaces, at the same format version.
func TestAMainInitOnAStackIsWrittenAsBeforeSpaces(t *testing.T) {
	written := func(spaces bool) string {
		f := newFakeInstance(t)
		if spaces {
			f.featureSpace = map[string]string{"feat-1": "space-main"}
			f.spaceNames = map[string]string{"space-main": "Produktion"}
			f.defaultSpace = "space-main"
		}
		signIn(t, f)
		root := filepath.Join(t.TempDir(), "report")
		if err := os.Mkdir(root, 0o755); err != nil {
			t.Fatal(err)
		}
		if _, err := runCLI(t, root, "wf", "init", "--feature", "feat-1", "--stack", "main"); err != nil {
			t.Fatalf("init: %v", err)
		}
		requireNoSpaceRecorded(t, root)
		return committedTree(t, root, f.URL())
	}
	if before, after := written(false), written(true); before != after {
		t.Fatalf("init --stack main in the default Space differs from one with no Spaces:\nwithout Spaces:\n%s\nwith:\n%s", before, after)
	}
}

// The stack every declaring command names for the default Space is "main",
// whatever that Space is called; outside it, the stack is named after its
// Space and records it.
func TestTheDefaultSpacesStackIsNamedMainWhateverItIsCalled(t *testing.T) {
	t.Run("bind --feature", func(t *testing.T) {
		f := spacedSearch(t)
		f.spaceNames["space-main"] = "Produktion"
		root := bindCmdFolderOn(t, f, "dev", testTenantID, `{}`)
		if _, err := runCLI(t, root, "bind", "--feature", "col-main", "--yes"); err != nil {
			t.Fatalf("bind --feature: %v", err)
		}
		if main, ok := loadStacks(t, root)["main"]; !ok || main.FeatureID != "col-main" || main.SpaceID != "" || main.Space != "" {
			t.Fatalf("stacks = %+v, want main on col-main with no Space recorded", loadStacks(t, root))
		}
	})
	t.Run("sync stack add", func(t *testing.T) {
		f := newFakeCopyInstance(t)
		f.spaceNames["space-main"] = "Produktion"
		tree := t.TempDir()
		root := devOnlyTreeFolder(t, f, tree, "space-dev")
		if out, err := runCLI(t, tree, "sync", "stack", "add", "--feature", "col-main", "--yes"); err != nil {
			t.Fatalf("sync stack add: %v\n%s", err, out)
		}
		if main, ok := loadStacks(t, root)["main"]; !ok || main.FeatureID != "col-main" || main.SpaceID != "" {
			t.Fatalf("stacks = %+v, want main on col-main with no Space recorded", loadStacks(t, root))
		}
	})
}

// devOnlyTreeFolder is a workflow folder under tree whose one stack is "dev"
// on col-dev, recording spaceID (none when it is "").
func devOnlyTreeFolder(t *testing.T, f *fakeCopyInstance, tree, spaceID string) string {
	t.Helper()
	root := stackTreeFolder(t, f, tree, "workflows/report", `{}`, `{}`, "print('hi')\n")
	m, err := wfdir.LoadManifest(root, wfdir.WorkflowKind)
	if err != nil {
		t.Fatal(err)
	}
	dev := wfdir.Stack{URL: f.URL(), TenantID: testTenantID, FeatureID: "col-dev", SpaceID: spaceID}
	if spaceID != "" {
		dev.Space = "Dev"
	}
	m.Stacks = map[string]wfdir.Stack{"dev": dev}
	if err := wfdir.SaveManifest(root, m); err != nil {
		t.Fatal(err)
	}
	return root
}

// A stack that names its feature and no Space gains the Space when the
// feature is outside the default Space, so re-running the declaring line
// records it once; a stack in the default Space is left exactly as it is.
func TestReDeclaringAStackOnItsFeatureRecordsItsSpaceOutsideTheDefault(t *testing.T) {
	t.Run("bind", func(t *testing.T) {
		f := spacedSearch(t)
		root := bindCmdFolderOn(t, f, "dev", testTenantID, `{}`)
		out, err := runCLI(t, root, "bind", "--stack", "dev", "--feature", "col-dev", "--yes")
		if err != nil {
			t.Fatalf("bind: %v", err)
		}
		if dev := stackOf(t, root, "dev"); dev.SpaceID != "space-dev" || dev.Space != "Dev" {
			t.Fatalf("dev = %+v, want Space Dev recorded (%s)", dev, out)
		}
	})
	t.Run("sync stack add", func(t *testing.T) {
		f := newFakeCopyInstance(t)
		tree := t.TempDir()
		root := devOnlyTreeFolder(t, f, tree, "")
		out, err := runCLI(t, tree, "sync", "stack", "add", "dev", "--feature", "col-dev", "--yes")
		if err != nil {
			t.Fatalf("sync stack add: %v\n%s", err, out)
		}
		if dev := stackOf(t, root, "dev"); dev.SpaceID != "space-dev" || !strings.Contains(out, "Space recorded: Dev") {
			t.Fatalf("dev = %+v, report:\n%s", dev, out)
		}
	})
	t.Run("default Space", func(t *testing.T) {
		f := spacedSearch(t)
		root := bindCmdFolder(t, f, `{}`, "")
		before := readFile(t, root, wfdir.ManifestName)
		if _, err := runCLI(t, root, "bind", "--stack", "prod", "--feature", "col-1", "--yes"); err != nil {
			t.Fatalf("bind: %v", err)
		}
		if after := readFile(t, root, wfdir.ManifestName); after != before {
			t.Fatalf("a default-Space stack was rewritten:\n%s", after)
		}
	})
}
