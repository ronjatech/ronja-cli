package commands

import (
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/ronjatech/ronja-cli/internal/api"
	"github.com/ronjatech/ronja-cli/internal/wfdir"
)

// `ronja app test` in a folder with no app, or with --scratch, sends the folder
// as a test app and renders that, leaving the folder's binding, app and draft
// alone.

func unboundAppFolder(t *testing.T, dir string) string {
	t.Helper()
	return writeAppFolder(t, dir, &wfdir.Manifest{Title: "Revenue explorer"},
		map[string]string{"App.tsx": "export default () => <b>A & B</b>;"})
}

// runAppTestCapturing is runCLI with the command's stderr as well.
func runAppTestCapturing(t *testing.T, root string, args ...string) (stdout, stderr string, err error) {
	t.Helper()
	stderr = captureStderr(t, func() {
		stdout, err = runCLI(t, root, append([]string{"app", "test", "--out-dir", t.TempDir()}, args...)...)
	})
	return stdout, stderr, err
}

func expiryHint() string {
	return "Only you and admins can open it. Deleted " + scratchExpiry.Local().Format("2006-01-02 15:04") +
		" unless you run `ronja app test` in this folder again."
}

func TestAppTestInAnUnboundFolderMakesATestApp(t *testing.T) {
	f := newFakeAppInstance(t)
	signInApp(t, f)
	root := unboundAppFolder(t, t.TempDir())
	manifest := readFile(t, root, wfdir.ManifestName)

	_, stderr, err := runAppTestCapturing(t, root)
	if err != nil {
		t.Fatalf("app test in an unbound folder: %v\n%s", err, stderr)
	}
	if len(f.scratchSent) != 1 {
		t.Fatalf("sent %d test apps, want 1", len(f.scratchSent))
	}
	sent := f.scratchSent[0]
	if sent.ID != "" || sent.Title != "Revenue explorer" || sent.Access == nil ||
		len(sent.Files) != 1 || sent.Files[0].Path != "App.tsx" || !strings.Contains(sent.Files[0].Content, "<b>A & B</b>") {
		t.Errorf("sent %+v, want the folder as its push would write it, with no id", sent)
	}
	if !slices.Equal(f.previewTargets, []string{"data_app-test-1"}) {
		t.Errorf("rendered %v, want the test app", f.previewTargets)
	}
	for _, line := range []string{
		"Test app:  " + fakeFrontendOrigin + "/apps/data_app-test-1",
		expiryHint(),
		"No app was made. To make one, run `ronja app push`.",
	} {
		if !strings.Contains(stderr, line) {
			t.Errorf("stderr lacks %q:\n%s", line, stderr)
		}
	}
	if len(f.created) != 0 {
		t.Errorf("an app was created: %+v", f.created)
	}
	if got := readFile(t, root, wfdir.ManifestName); got != manifest {
		t.Errorf("the manifest changed:\n%s", got)
	}
	if id := wfdir.LoadScratchApp(root, f.Key()); id != "data_app-test-1" {
		t.Errorf(".ronja/scratch.json holds %q, want the test app", id)
	}
}

func TestAppTestScratchSendsTheFolderAndKeepsTheID(t *testing.T) {
	f := newFakeAppInstance(t)
	signInApp(t, f)
	root := unboundAppFolder(t, t.TempDir())

	if _, stderr, err := runAppTestCapturing(t, root, "--scratch"); err != nil {
		t.Fatalf("first run: %v\n%s", err, stderr)
	}

	out, stderr, err := runAppTestCapturing(t, root, "--scratch", "--json")
	if err != nil {
		t.Fatalf("second run: %v\n%s", err, stderr)
	}
	if got := f.scratchSent[1].ID; got != "data_app-test-1" {
		t.Errorf("the second run sent id %q, want the first run's test app", got)
	}
	if !slices.Equal(f.previewTargets, []string{"data_app-test-1", "data_app-test-1"}) {
		t.Errorf("rendered %v, want the same test app twice", f.previewTargets)
	}
	var result appTestResult
	decodeJSONInto(t, out, &result)
	if s := result.Scratch; s == nil || s.ID != "data_app-test-1" || s.URL != fakeFrontendOrigin+"/apps/data_app-test-1" || !s.ExpiresAt.Equal(scratchExpiry) {
		t.Errorf("--json scratch is %+v, want {id, url, expiresAt}", result.Scratch)
	}
	if strings.Contains(stderr, "new link") {
		t.Errorf("a kept test app said it was replaced:\n%s", stderr)
	}

	f.scratchApps["data_app-test-1"] = false
	_, stderr, err = runAppTestCapturing(t, root, "--scratch")
	if err != nil {
		t.Fatalf("third run: %v\n%s", err, stderr)
	}
	if !strings.Contains(stderr, "Your earlier test app is gone or in another Space; this is a new link.") ||
		!strings.Contains(stderr, "/apps/data_app-test-2") {
		t.Errorf("a replaced test app should say its link is new:\n%s", stderr)
	}
	if _, stderr, err := runAppTestCapturing(t, root, "--scratch"); err != nil {
		t.Fatalf("fourth run: %v\n%s", err, stderr)
	}
	if got := f.scratchSent[3].ID; got != "data_app-test-2" {
		t.Errorf("after a replacement the folder sent %q, want the new test app", got)
	}
}

func TestAppTestScratchLeavesTheBindingAlone(t *testing.T) {
	f := newFakeAppInstance(t)
	root, app := appTestFolder(t, f)
	manifest := readFile(t, root, wfdir.ManifestName)
	f.Requests = nil

	_, stderr, err := runAppTestCapturing(t, root, "--scratch")
	if err != nil {
		t.Fatalf("app test --scratch: %v\n%s", err, stderr)
	}
	if !slices.Equal(f.previewTargets, []string{"data_app-test-1"}) {
		t.Errorf("rendered %v, want the test app, not the bound app", f.previewTargets)
	}
	for _, r := range f.Requests {
		if strings.Contains(r, app.ID) && !strings.HasPrefix(r, http.MethodGet+" ") {
			t.Errorf("the bound app was written: %s", r)
		}
	}
	if got := readFile(t, root, wfdir.ManifestName); got != manifest {
		t.Errorf("the manifest changed:\n%s", got)
	}
	if strings.Contains(stderr, "No app was made") {
		t.Errorf("a bound folder has an app, so says nothing about making one:\n%s", stderr)
	}
}

// A bound folder whose access lives in Ronja tests with that access: the
// person's draft's when there is one, else the live app's.
func TestAppTestScratchTakesTheBoundAppsAccessWhenTheFolderDeclaresNone(t *testing.T) {
	for _, withDraft := range []bool{false, true} {
		t.Run(fmt.Sprintf("draft=%v", withDraft), func(t *testing.T) {
			f := newFakeAppInstance(t)
			root, app := appTestFolder(t, f)
			app.AllowedTableIDs = []string{"table-live"}
			want := "table-live"
			if withDraft {
				draft := f.AddDraft(app.ID, "data_app-draft-1", api.DataAppFile{Path: "App.tsx", Content: "SOURCE"})
				draft.AllowedTableIDs = []string{"table-draft"}
				want = "table-draft"
			}

			if _, stderr, err := runAppTestCapturing(t, root, "--scratch"); err != nil {
				t.Fatalf("app test --scratch: %v\n%s", err, stderr)
			}
			if got := f.scratchSent[0].Access; got == nil || !slices.Equal(got.AllowedTableIDs, []string{want}) {
				t.Errorf("sent access %+v, want the tables [%s]", got, want)
			}
		})
	}
}

func TestAppTestScratchSendsTheFoldersOwnAccessWhenItDeclaresOne(t *testing.T) {
	f := newFakeAppInstance(t)
	root, app := appTestFolder(t, f)
	app.AllowedTableIDs = []string{"table-live"}
	m := appManifestOf(t, root)
	m.Access = &api.DataAppAccess{AllowedTableIDs: []string{"table-declared"}}
	if err := wfdir.SaveManifest(root, m); err != nil {
		t.Fatal(err)
	}
	f.Requests = nil

	if _, stderr, err := runAppTestCapturing(t, root, "--scratch"); err != nil {
		t.Fatalf("app test --scratch: %v\n%s", err, stderr)
	}
	if got := f.scratchSent[0].Access; got == nil || !slices.Equal(got.AllowedTableIDs, []string{"table-declared"}) {
		t.Errorf("sent access %+v, want the folder's declared tables", got)
	}
	for _, r := range f.Requests {
		if strings.Contains(r, app.ID) {
			t.Errorf("a folder that declares its access read the bound app: %s", r)
		}
	}
}

func TestACopiedFolderGetsItsOwnTestApp(t *testing.T) {
	f := newFakeAppInstance(t)
	signInApp(t, f)
	original := unboundAppFolder(t, t.TempDir())
	if _, stderr, err := runAppTestCapturing(t, original); err != nil {
		t.Fatalf("original: %v\n%s", err, stderr)
	}
	copied := filepath.Join(t.TempDir(), "copy")
	if err := os.CopyFS(copied, os.DirFS(original)); err != nil {
		t.Fatal(err)
	}

	for _, run := range []struct{ root, wantSent string }{
		{copied, ""}, {original, "data_app-test-1"}, {copied, "data_app-test-2"},
	} {
		if _, stderr, err := runAppTestCapturing(t, run.root); err != nil {
			t.Fatalf("%s: %v\n%s", run.root, err, stderr)
		}
		if got := f.scratchSent[len(f.scratchSent)-1].ID; got != run.wantSent {
			t.Errorf("%s sent %q, want %q", run.root, got, run.wantSent)
		}
	}
}

func TestAppTestScratchSaysAnOlderRonjaHasNoTestApps(t *testing.T) {
	for _, status := range []int{http.StatusNotFound, http.StatusRequestEntityTooLarge} {
		t.Run(fmt.Sprint(status), func(t *testing.T) {
			f := newFakeAppInstance(t)
			signInApp(t, f)
			sentence := "Space space-gone not found."
			f.scratchFail, f.scratchFailBody = status, `{"error":"`+sentence+`"}`
			_, _, err := runAppTestCapturing(t, unboundAppFolder(t, t.TempDir()))
			if err == nil || !strings.Contains(err.Error(), "does not support test apps yet") {
				t.Errorf("a %d answered %v, want the older-Ronja sentence", status, err)
			}
			if err == nil || !strings.HasPrefix(err.Error(), sentence) {
				t.Errorf("a %d answered %v, want the server's own sentence first", status, err)
			}
		})
	}
}

func TestAppTestScratchNamesWhenTheNextTestAppIsDeleted(t *testing.T) {
	f := newFakeAppInstance(t)
	signInApp(t, f)
	sentence := "You have 20 test apps, the most at once. The next one is deleted at 2026-11-25 09:41 UTC. Test again from one of their folders to reuse its app."
	f.scratchFail = http.StatusConflict
	f.scratchFailBody = `{"error":"` + sentence + `","code":"scratch_app_limit","nextExpiresAt":"2026-11-25T09:41:00Z"}`
	root := unboundAppFolder(t, t.TempDir())

	if _, _, err := runAppTestCapturing(t, root); err == nil || err.Error() != sentence {
		t.Errorf("the cap answered %v, want the server's sentence", err)
	}
	out, _, err := runAppTestCapturing(t, root, "--json")
	if err == nil {
		t.Fatal("the cap is a refusal under --json too")
	}
	if !strings.Contains(out, `"nextExpiresAt": "2026-11-25T09:41:00Z"`) || !strings.Contains(out, api.ScratchAppLimitCode) {
		t.Errorf("--json should carry the code and nextExpiresAt, got:\n%s", out)
	}
	if len(f.previewTargets) != 0 {
		t.Errorf("a refused push rendered %v", f.previewTargets)
	}
}

func TestAppTestScratchRefusesAnAppTooLargeToPropose(t *testing.T) {
	f := newFakeAppInstance(t)
	signInApp(t, f)
	files := map[string]string{"App.tsx": "export default () => null;"}
	for i := range 6 {
		files[fmt.Sprintf("data%d.ts", i)] = "export const s = \"" + strings.Repeat("a", 4<<20+500<<10) + "\";"
	}
	root := writeAppFolder(t, t.TempDir(), &wfdir.Manifest{Title: "Big"}, files)

	_, _, err := runAppTestCapturing(t, root)
	if err == nil || !strings.Contains(err.Error(), "larger than 24 MiB, the most a change list can carry; it could never be proposed") {
		t.Errorf("an app over the cap answered %v", err)
	}
	if len(f.scratchSent) != 0 {
		t.Errorf("an app over the cap was sent")
	}
}

// runAppTestInto is runAppTestCapturing with the out dir the caller reads.
func runAppTestInto(t *testing.T, root, outDir string, args ...string) (stdout, stderr string, err error) {
	t.Helper()
	stderr = captureStderr(t, func() {
		stdout, err = runCLI(t, root, append([]string{"app", "test", "--out-dir", outDir}, args...)...)
	})
	return stdout, stderr, err
}

func TestAppTestScratchRendersATestAppThatDoesNotBuild(t *testing.T) {
	f := newFakeAppInstance(t)
	signInApp(t, f)
	answer := []byte(`{"rendered":false,"durationMs":0,"ops":[],"errors":[],"consoleErrors":[],` +
		`"compileDiagnostics":[{"message":"Unexpected token","line":3,"column":7,"file":"App.tsx"}],` +
		`"hint":"The bundle did not build, so nothing was rendered. Fix the diagnostics above and preview again.",` +
		`"calibration":"rendered headless","dataAppID":"data_app-test-1","screenshots":[]}`)
	f.AnswerPreview(fakePreviewAnswer{Body: answer})
	outDir := t.TempDir()

	out, stderr, err := runAppTestInto(t, unboundAppFolder(t, t.TempDir()), outDir)
	if err != nil {
		t.Fatalf("app test: %v\n%s", err, stderr)
	}
	if !slices.Equal(f.previewTargets, []string{"data_app-test-1"}) {
		t.Errorf("rendered %v, want the test app through the render route", f.previewTargets)
	}
	if got := readFile(t, outDir, "report.json"); got != string(answer) {
		t.Errorf("report.json is not the route's answer verbatim:\n%s", got)
	}
	if !strings.Contains(out, "the bundle did not build") || !strings.Contains(out, "Unexpected token") {
		t.Errorf("the diagnostics should print:\n%s", out)
	}
	if !strings.Contains(stderr, "Test app:  ") {
		t.Errorf("the link prints even when the build failed:\n%s", stderr)
	}
}

func TestAppTestScratchFailsOnAMissingEntrypoint(t *testing.T) {
	f := newFakeAppInstance(t)
	signInApp(t, f)
	f.AnswerPreview(fakePreviewAnswer{Body: []byte(`{"rendered":false,"durationMs":0,"ops":[],"errors":[],"consoleErrors":[],` +
		`"compileDiagnostics":[{"message":"entrypoint \"App.tsx\" not found among data app files","line":0,"column":0}],` +
		`"hint":"Nothing was rendered because the entrypoint \"App.tsx\" has not been written yet — there is no app to preview until it exists. This is not a diagnostic to fix.",` +
		`"calibration":"rendered headless","dataAppID":"data_app-test-1","screenshots":[]}`)})

	_, stderr, err := runAppTestInto(t, unboundAppFolder(t, t.TempDir()), t.TempDir(), "--fail-on-errors")
	if err == nil {
		t.Fatalf("--fail-on-errors should bite on the route's one diagnostic:\n%s", stderr)
	}
	if !strings.Contains(stderr, "--fail-on-errors: ") {
		t.Errorf("the gate should say why it bit:\n%s", stderr)
	}
}

func TestNoFolderBindsToATestApp(t *testing.T) {
	err := refuseUnworkableApp(&api.DataApp{ID: "data_app-t", Lifecycle: "preview", Scratch: &api.DataAppScratch{ExpiresAt: scratchExpiry}})
	if err == nil || !strings.Contains(err.Error(), "data_app-t is a test app, made by `ronja app test`") || strings.Contains(err.Error(), "ronja update") {
		t.Fatalf("refuseUnworkableApp(a test app) = %v, want it named a test app", err)
	}
}
