package commands

import (
	"bytes"
	"compress/gzip"
	"encoding/json"
	"io"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/ronjatech/ronja-cli/internal/api"
	"github.com/ronjatech/ronja-cli/internal/wfdir"
)

const proposedApp = "apps/sales-dashboard"

// editProposedApp changes the fixture's app the way an agent does before it
// proposes: its entrypoint, a new file naming a table by its alias, and its
// description.
func editProposedApp(t *testing.T, dir string) {
	t.Helper()
	root := filepath.Join(dir, proposedApp)
	writeLocal(t, root, "App.tsx", readFile(t, root, "App.tsx")+"// the agent's change\n")
	writeLocal(t, root, "kit/books.ts", "export const books = \"{{ ref('orders_clean') }}\";\n")
	m := loadAnyManifest(t, root)
	description := "Sales, with books"
	m.Description = &description
	if err := wfdir.SaveManifest(root, m); err != nil {
		t.Fatal(err)
	}
}

func liveAppIn(t *testing.T, f *fakeImportInstance, feature string) *api.DataApp {
	t.Helper()
	for _, app := range f.app.apps {
		if app.FeatureID == feature && app.Lifecycle == api.LifecycleLive {
			return app
		}
	}
	t.Fatalf("no live app in %s", feature)
	return nil
}

// TestPreviewIsWhatThePushWrites: the draft `ronja propose` uploads for an
// aliased app is exactly what `ronja sync apply` then writes to it, files
// after alias resolution, access and settings; and the dry run's
// previewDigest is that draft's.
func TestPreviewIsWhatThePushWrites(t *testing.T) {
	f, dir := proposeFixture(t)
	editProposedApp(t, dir)

	out, err := runCLI(t, dir, "sync", "apply", "--stack", "dev", "--dry-run", "--allow-create", "--json")
	if err != nil {
		t.Fatalf("dry run: %v\n%s", err, out)
	}
	var dry syncApplyReport
	if err := json.Unmarshal([]byte(out), &dry); err != nil {
		t.Fatal(err)
	}
	if out, err := runPropose_(t, dir, "--stack", "dev"); err != nil {
		t.Fatalf("propose: %v\n%s", err, out)
	}
	sent := f.changeLists.proposals[0].previews
	if sent == nil || len(sent.Apps) != 1 {
		t.Fatalf("propose sent previews %+v, want the one app it changes", sent)
	}
	preview := sent.Apps[0]
	if sent.CLIVersion != resolveVersion() {
		t.Errorf("the previews name ronja %q, want %q", sent.CLIVersion, resolveVersion())
	}
	for _, line := range dry.Folders {
		switch {
		case line.Path == proposedApp && line.PreviewDigest != previewDigest(preview):
			t.Errorf("the dry run's previewDigest %q is not the draft's %q", line.PreviewDigest, previewDigest(preview))
		case line.Path != proposedApp && line.PreviewDigest != "":
			t.Errorf("%s (%s) reports a previewDigest", line.Path, line.Kind)
		}
	}

	f.app.fileWrites = nil
	if report, err := applyStack(t, dir, "dev", "--allow-create"); err != nil || report.Verdict != verdictApplied {
		t.Fatalf("apply: %v\n%s", err, describeApply(report))
	}
	live := liveAppIn(t, f, devFeature)
	drafted := map[string]string{}
	for _, file := range preview.Files {
		drafted[file.Path] = file.Content
	}
	if got := f.app.FileContents(live.ID); !mapsEqual(got, drafted) {
		t.Errorf("the live app holds %v\nits draft showed %v", got, drafted)
	}
	if !strings.Contains(drafted["kit/books.ts"], "{{ ref('table-") || strings.Contains(drafted["kit/books.ts"], "orders_clean") {
		t.Errorf("the draft's kit/books.ts is not in wire form: %q", drafted["kit/books.ts"])
	}
	pushed := 0
	for _, w := range f.app.fileWrites {
		if w.Method == "PUT" {
			pushed++
			if _, ok := drafted[w.Path]; !ok {
				t.Errorf("the push wrote %s, which the draft does not hold", w.Path)
			}
		}
	}
	if pushed == 0 {
		t.Error("the push wrote no file; the comparison proves nothing")
	}
	if preview.Access == nil || !slices.Equal(preview.Access.Normalized().AllowedWorkflowIDs, live.DataAppAccess.Normalized().AllowedWorkflowIDs) ||
		!slices.Equal(preview.Access.Normalized().AllowedTableIDs, live.DataAppAccess.Normalized().AllowedTableIDs) {
		t.Errorf("the draft's access %+v, the live app's %+v", preview.Access, live.DataAppAccess)
	}
	if preview.Title != live.Name || preview.Description == nil || *preview.Description != live.Description {
		t.Errorf("the draft says %q / %v, the live app %q / %q", preview.Title, preview.Description, live.Name, live.Description)
	}
}

func mapsEqual(a, b map[string]string) bool {
	if len(a) != len(b) {
		return false
	}
	for k, v := range a {
		if w, ok := b[k]; !ok || w != v {
			return false
		}
	}
	return true
}

// TestProposePrintsTheDraftLink: after the list's own link, one line per app
// the server stored a draft for, or why it has none, and the same in --json. An older Ronja, which
// ignores the previews, gets no line, and a list that changes no app sends no
// previews.
func TestProposePrintsTheDraftLink(t *testing.T) {
	f, dir := proposeFixture(t)
	out, err := runPropose_(t, dir, "--stack", "dev")
	if err != nil {
		t.Fatalf("propose: %v\n%s", err, out)
	}
	if strings.Contains(out, "Draft ") || f.changeLists.proposals[0].previews != nil {
		t.Errorf("a list that changes no app sent previews %+v or printed a draft:\n%s", f.changeLists.proposals[0].previews, out)
	}

	editProposedApp(t, dir)
	m := loadAnyManifest(t, filepath.Join(dir, proposedApp))
	link := "  Draft " + m.Title + ": https://app.example.com/change-lists/changelist-1/draft/" + m.ID + "?org=" + testTenantID + "\n"
	out, err = runPropose_(t, dir, "--stack", "dev")
	if err != nil {
		t.Fatalf("propose: %v\n%s", err, out)
	}
	list := strings.Index(out, "https://app.example.com/change-lists/changelist-1?org=")
	draft := strings.Index(out, link)
	if list < 0 || draft < list || draft > strings.Index(out, "Describe it:") {
		t.Errorf("want the draft line %q after the list's link:\n%s", link, out)
	}

	out, err = runPropose_(t, dir, "--stack", "dev", "--json")
	if err != nil {
		t.Fatalf("propose --json: %v\n%s", err, out)
	}
	var report proposeReport
	if err := json.Unmarshal([]byte(out), &report); err != nil {
		t.Fatal(err)
	}
	if len(report.Drafts) != 1 || report.Drafts[0].FolderID != m.ID || report.Drafts[0].Title != m.Title || !strings.Contains(link, report.Drafts[0].URL) {
		t.Errorf("--json drafts %+v", report.Drafts)
	}

	f.changeLists.draftRefused = "the person this change list was proposed for cannot change " + m.Title + ", so its deploy would be refused"
	out, err = runPropose_(t, dir, "--stack", "dev")
	if err != nil {
		t.Fatalf("propose with the draft refused: %v\n%s", err, out)
	}
	if want := "  Draft " + m.Title + ": none, because " + f.changeLists.draftRefused + "\n"; !strings.Contains(out, want) {
		t.Errorf("want the refusal %q in place of the link:\n%s", want, out)
	}
	f.changeLists.draftRefused = ""

	f.changeLists.olderServer = true
	out, err = runPropose_(t, dir, "--stack", "dev")
	if err != nil {
		t.Fatalf("propose to an older Ronja: %v\n%s", err, out)
	}
	if strings.Contains(out, "Draft ") {
		t.Errorf("an older Ronja stored no draft, and propose printed one:\n%s", out)
	}
}

// TestPreviewDigestMatchesItsFixture holds previewDigest to the fixture the
// backend's TestCLIPreviewDigestMirrorsTheServer holds echangelist's to.
func TestPreviewDigestMatchesItsFixture(t *testing.T) {
	raw, err := os.ReadFile(filepath.Join("testdata", "preview_digest.json"))
	if err != nil {
		t.Fatal(err)
	}
	var cases []struct {
		Name    string     `json:"name"`
		Preview appPreview `json:"preview"`
		Digest  string     `json:"digest"`
	}
	if err := json.Unmarshal(raw, &cases); err != nil {
		t.Fatal(err)
	}
	if len(cases) < 2 {
		t.Fatalf("the fixture holds %d cases", len(cases))
	}
	for _, c := range cases {
		if got := previewDigest(c.Preview); got != c.Digest {
			t.Errorf("%s: previewDigest = %s, the fixture says %s — change it with echangelist.PreviewDigest", c.Name, got, c.Digest)
		}
	}
}

// The part carries an app's markup as written: escaped, each <, > and & is six
// bytes, and source heavy in markup would grow toward the server's cap.
func TestProposedPreviewsDoNotEscapeMarkup(t *testing.T) {
	folder := syncApplyFolderReport{Path: proposedApp, Kind: wfdir.KindDataApp, FolderID: "folder-1",
		Units: []syncApplyUnitReport{{Action: string(applyActionCreate)}},
		preview: &appPreview{FolderID: "folder-1", Title: "Board",
			Files: []appPreviewFile{{Path: "App.tsx", Content: "const a = <div>{x && y}</div>\n"}}}}
	gzipped, err := proposedPreviews([]syncApplyFolderReport{folder})
	if err != nil {
		t.Fatal(err)
	}
	gz, err := gzip.NewReader(bytes.NewReader(gzipped))
	if err != nil {
		t.Fatal(err)
	}
	raw, err := io.ReadAll(gz)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(raw), "<div>{x && y}</div>") {
		t.Errorf("the part escaped the app's markup: %s", raw)
	}
}

// A data app's file that is not UTF-8 text is refused by name, at push and at
// propose, before anything is sent: the wire would mangle it, and a change
// list's deploy would then refuse the app for differing from its draft.
func TestAnAppFileThatIsNotUTF8IsRefusedByName(t *testing.T) {
	if err := checkAppPushable(map[string]string{"App.tsx": "export {}\n", "kit/latin1.ts": "export const s = \"caf\xe9\";\n"}); err == nil ||
		!strings.Contains(err.Error(), "kit/latin1.ts") || !strings.Contains(err.Error(), "UTF-8") {
		t.Errorf("push: %v, want kit/latin1.ts refused as not UTF-8", err)
	}

	f, dir := proposeFixture(t)
	writeLocal(t, filepath.Join(dir, proposedApp), "kit/latin1.ts", "export const s = \"caf\xe9\";\n")
	out, err := runPropose_(t, dir, "--stack", "dev")
	if err == nil || !strings.Contains(err.Error()+out, "kit/latin1.ts") || !strings.Contains(err.Error()+out, "UTF-8") {
		t.Errorf("propose: %v, want kit/latin1.ts refused as not UTF-8:\n%s", err, out)
	}
	if n := len(f.changeLists.proposals); n != 0 {
		t.Errorf("propose sent %d proposals with a file the push would refuse", n)
	}
}
