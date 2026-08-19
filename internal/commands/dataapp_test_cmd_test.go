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

// `ronja app test` renders and REPORTS; it does not judge. Nearly every test
// below is about that distinction showing up in the exit code, because the exit
// code is the only part of this command a script reads.

// appTestFolder lays out a bound data-app folder whose files match the server's,
// so nothing under test trips the dirty-folder warning by accident.
func appTestFolder(t *testing.T, f *fakeAppInstance) (root string, app *api.DataApp) {
	t.Helper()
	app = f.AddApp(&api.DataApp{ID: "data_app-1"},
		api.DataAppFile{Path: "App.tsx", Content: "SOURCE"})
	signInApp(t, f)

	root = writeAppFolder(t, t.TempDir(), &wfdir.Manifest{Title: "Revenue explorer"},
		map[string]string{"App.tsx": "SOURCE"})
	m := appManifestOf(t, root)
	m.SetBinding(f.Key(), wfdir.Binding{DataAppID: app.ID, FeatureID: "feat-1"})
	if err := wfdir.SaveManifest(root, m); err != nil {
		t.Fatal(err)
	}
	return root, app
}

// ── The artefacts ───────────────────────────────────────────────────────────

// TestAppTestWritesReportAndFrames pins what lands on disk: the server's answer
// verbatim, every frame in capture order, and the representative frame under a
// second, stable name.
func TestAppTestWritesReportAndFrames(t *testing.T) {
	f := newFakeAppInstance(t)
	root, app := appTestFolder(t, f)

	firstPNG := []byte("\x89PNG-first-paint")
	settledPNG := []byte("\x89PNG-settled")
	f.AddFileBlob("file-1", "dataapp/preview/1.png", firstPNG)
	f.AddFileBlob("file-2", "dataapp/preview/2.png", settledPNG)
	f.AnswerPreviewResult(api.PreviewResult{
		Rendered: true, DurationMs: 1200, Viewport: "desktop", DataAppID: app.ID,
		Settle:        &api.PreviewSettle{Signal: "sdk"},
		Ops:           []api.PreviewOp{{Path: "/query", Ms: 40, OK: true}},
		Errors:        []api.PreviewRuntimeError{},
		ConsoleErrors: []api.PreviewConsoleError{},
		Screenshots: []api.PreviewScreenshot{
			{FileID: "file-1", Phase: "first_paint"},
			{FileID: "file-2", Phase: "settled", Selected: true},
		},
	})

	outDir := filepath.Join(t.TempDir(), "preview")
	out, err := runCLI(t, root, "app", "test", "--out-dir", outDir, "--json")
	if err != nil {
		t.Fatalf("app test: %v", err)
	}

	// report.json is the server's bytes, not a re-marshal.
	report := readFile(t, outDir, "report.json")
	var reported map[string]any
	if err := json.Unmarshal([]byte(report), &reported); err != nil {
		t.Fatalf("report.json is not JSON (%v):\n%s", err, report)
	}
	if reported["dataAppID"] != app.ID {
		t.Errorf("report.json should name the rendered row, got %v", reported["dataAppID"])
	}
	if _, ok := reported["ok"]; ok {
		t.Error("report.json carries an `ok` field — the preview surface has no verdict and must not grow one")
	}

	if got := readFile(t, outDir, "screenshot-1.png"); got != string(firstPNG) {
		t.Errorf("screenshot-1.png should hold the first frame, got %q", got)
	}
	if got := readFile(t, outDir, "screenshot-2.png"); got != string(settledPNG) {
		t.Errorf("screenshot-2.png should hold the second frame, got %q", got)
	}
	// The SELECTED frame, not the last one — selection is by phase, and a reader
	// that assumed position would be shown different pixels from the agent.
	if got := readFile(t, outDir, "screenshot.png"); got != string(settledPNG) {
		t.Errorf("screenshot.png should hold the selected frame, got %q", got)
	}

	for _, name := range []string{"report.json", "screenshot-1.png", "screenshot-2.png", "screenshot.png"} {
		info, err := os.Stat(filepath.Join(outDir, name))
		if err != nil {
			t.Fatalf("stat %s: %v", name, err)
		}
		if mode := info.Mode().Perm(); mode != 0o600 {
			t.Errorf("%s should be 0600 (it is tenant data), got %v", name, mode)
		}
	}

	var result appTestResult
	decodeJSONInto(t, out, &result)
	if result.DataAppID != app.ID {
		t.Errorf("--json should report the rendered row, got %q", result.DataAppID)
	}
	if len(result.Files) != 4 {
		t.Errorf("--json should list every file written, got %+v", result.Files)
	}
	if string(result.Preview) == "" || !strings.Contains(string(result.Preview), `"dataAppID"`) {
		t.Errorf("--json should carry the server envelope verbatim, got %s", result.Preview)
	}
}

// A field this CLI's hand-mirror predates must still reach report.json. That is
// the whole reason the report is written from the raw bytes: a re-marshal would
// drop it silently, and the reader would never know a newer instance had said
// more than the CLI understood.
func TestAppTestReportKeepsFieldsTheMirrorDoesNotKnow(t *testing.T) {
	f := newFakeAppInstance(t)
	root, app := appTestFolder(t, f)
	f.AnswerPreview(fakePreviewAnswer{Body: []byte(
		`{"rendered":true,"viewport":"desktop","ops":[],"errors":[],"consoleErrors":[],` +
			`"screenshots":[],"dataAppID":"` + app.ID + `","somethingNewer":{"n":7}}`)})

	outDir := t.TempDir()
	if _, err := runCLI(t, root, "app", "test", "--out-dir", outDir); err != nil {
		t.Fatalf("app test: %v", err)
	}
	if got := readFile(t, outDir, "report.json"); !strings.Contains(got, `"somethingNewer"`) {
		t.Errorf("report.json must be the server's bytes verbatim; got:\n%s", got)
	}
}

// ── The exit-code matrix ────────────────────────────────────────────────────

// An app full of runtime errors exits ZERO: the harness ran and answered the
// question. This is the property the whole command is built around, so it is
// asserted before anything that overrides it.
func TestAppTestExitsZeroWhenTheAppHasErrors(t *testing.T) {
	f := newFakeAppInstance(t)
	root, app := appTestFolder(t, f)
	f.AnswerPreviewResult(api.PreviewResult{
		Rendered: true, Viewport: "desktop", DataAppID: app.ID,
		Ops: []api.PreviewOp{},
		Errors: []api.PreviewRuntimeError{
			{Message: "Cannot read properties of undefined", Source: "App.tsx", Line: 12, Column: 3},
		},
		ConsoleErrors: []api.PreviewConsoleError{{Message: "boom"}},
		Screenshots:   []api.PreviewScreenshot{},
	})

	out, err := runCLI(t, root, "app", "test", "--out-dir", t.TempDir())
	if err != nil {
		t.Fatalf("a render that saw errors is still a render — expected exit zero, got %v", err)
	}
	if !strings.Contains(out, "App.tsx:12:3") {
		t.Errorf("the report should name the source-mapped frame, got:\n%s", out)
	}
}

// --fail-on-errors is the opt-in that makes the same answer a CI failure.
func TestAppTestFailOnErrorsExitsNonZero(t *testing.T) {
	f := newFakeAppInstance(t)
	root, app := appTestFolder(t, f)
	f.AnswerPreviewResult(api.PreviewResult{
		Rendered: true, Viewport: "desktop", DataAppID: app.ID,
		Ops:           []api.PreviewOp{},
		Errors:        []api.PreviewRuntimeError{{Message: "boom"}},
		ConsoleErrors: []api.PreviewConsoleError{},
		Screenshots:   []api.PreviewScreenshot{},
	})

	if _, err := runCLI(t, root, "app", "test", "--out-dir", t.TempDir(), "--fail-on-errors"); err == nil {
		t.Fatal("--fail-on-errors should exit non-zero on a render that reported errors")
	}
}

// A bundle that did not build comes back 200 with an EMPTY errors list, so a
// gate reading errors alone would pass the most broken state a data app can be
// in. --fail-on-errors counts diagnostics too.
func TestAppTestFailOnErrorsCatchesACompileFailure(t *testing.T) {
	f := newFakeAppInstance(t)
	root, app := appTestFolder(t, f)
	f.AnswerPreviewResult(api.PreviewResult{
		Viewport: "desktop", DataAppID: app.ID,
		Ops: []api.PreviewOp{}, Errors: []api.PreviewRuntimeError{},
		ConsoleErrors: []api.PreviewConsoleError{},
		CompileDiagnostics: []api.CompileDiagnostic{
			{Message: "Unexpected \"}\"", Line: 4, Column: 1, File: "App.tsx"},
		},
		Screenshots: []api.PreviewScreenshot{},
	})

	// Without the flag it is still an observation, and still exits zero.
	out, err := runCLI(t, root, "app", "test", "--out-dir", t.TempDir())
	if err != nil {
		t.Fatalf("a compile failure is an observation — expected exit zero, got %v", err)
	}
	if !strings.Contains(out, "did not build") {
		t.Errorf("the report should say the bundle did not build, got:\n%s", out)
	}
	if _, err := runCLI(t, root, "app", "test", "--out-dir", t.TempDir(), "--fail-on-errors"); err == nil {
		t.Fatal("--fail-on-errors should exit non-zero when the bundle did not build")
	}
}

// CONSOLE errors alone must NOT trip the flag, and that exclusion is a decision
// rather than an oversight: a console message is noisy, an app can log one on
// purpose, and a --fail-on-errors that fired on a third-party library's warning
// would be switched off and never switched back on. Without this test the
// exclusion is one word in a comment.
func TestAppTestFailOnErrorsIgnoresConsoleErrors(t *testing.T) {
	f := newFakeAppInstance(t)
	root, app := appTestFolder(t, f)
	f.AnswerPreviewResult(api.PreviewResult{
		Rendered: true, Viewport: "desktop", DataAppID: app.ID,
		Ops: []api.PreviewOp{}, Errors: []api.PreviewRuntimeError{},
		ConsoleErrors: []api.PreviewConsoleError{
			{Message: "React does not recognize the `dataFoo` prop"},
			{Message: "a deprecation notice from a chart library"},
		},
		Screenshots: []api.PreviewScreenshot{},
	})

	out, err := runCLI(t, root, "app", "test", "--out-dir", t.TempDir(), "--fail-on-errors")
	if err != nil {
		t.Fatalf("console errors alone must not fail the command: %v", err)
	}
	if !strings.Contains(out, "Console errors:  2") {
		t.Errorf("they must still be COUNTED in the report, got:\n%s", out)
	}
}

// An HTTP failure means the harness never answered, so nothing about the app is
// known — non-zero, unconditionally.
func TestAppTestHarnessFailureExitsNonZero(t *testing.T) {
	f := newFakeAppInstance(t)
	root, _ := appTestFolder(t, f)
	f.AnswerPreview(fakePreviewAnswer{Status: 500, Body: []byte(`{"error":"the render harness is not configured"}`)})

	if _, err := runCLI(t, root, "app", "test", "--out-dir", t.TempDir()); err == nil {
		t.Fatal("a failed preview call must exit non-zero — nothing about the app was observed")
	}
}

// ── Busy ────────────────────────────────────────────────────────────────────

// A 429 is honoured ONCE. The retry is the difference between failing a CI job
// for a two-second queue and adding load to a pool already at capacity.
func TestAppTestRetriesOnceWhenBusy(t *testing.T) {
	f := newFakeAppInstance(t)
	root, app := appTestFolder(t, f)
	shrinkBusyRetry(t)

	f.AnswerPreviewBusy("the shared render pool is at capacity")
	f.AnswerPreviewResult(api.PreviewResult{
		Rendered: true, Viewport: "desktop", DataAppID: app.ID,
		Ops: []api.PreviewOp{}, Errors: []api.PreviewRuntimeError{},
		ConsoleErrors: []api.PreviewConsoleError{}, Screenshots: []api.PreviewScreenshot{},
	})

	if _, err := runCLI(t, root, "app", "test", "--out-dir", t.TempDir()); err != nil {
		t.Fatalf("a busy answer followed by a good one should succeed: %v", err)
	}
	if f.previewCalls != 2 {
		t.Errorf("expected exactly one retry, got %d preview calls", f.previewCalls)
	}
}

// Busy through the retry is a HARNESS-AVAILABILITY failure, and its message must
// not read like an app verdict — a reader who mistook it for one would start
// changing code that was never rendered.
func TestAppTestBusyThroughTheRetryFailsDistinctly(t *testing.T) {
	f := newFakeAppInstance(t)
	root, _ := appTestFolder(t, f)
	shrinkBusyRetry(t)
	f.AnswerPreviewBusy("the shared render pool is at capacity")

	var err error
	stderr := captureStderr(t, func() {
		_, err = runCLI(t, root, "app", "test", "--out-dir", t.TempDir())
	})

	if err == nil {
		t.Fatal("a harness that stayed busy must exit non-zero")
	}
	if f.previewCalls != 2 {
		t.Errorf("expected the initial call plus one retry, got %d", f.previewCalls)
	}
	if !strings.Contains(stderr, "Nothing about your app was observed") {
		t.Errorf("the busy failure must say nothing was observed, so it is not read as an app verdict; got:\n%s", stderr)
	}
	// The app-error path never prints this, which is what makes the two
	// distinguishable to a script tailing stderr.
	if strings.Contains(stderr, "--fail-on-errors") {
		t.Errorf("a busy failure must not be reported as an app error; got:\n%s", stderr)
	}
}

// shrinkBusyRetry collapses the retry wait so the busy tests cost no wall time.
func shrinkBusyRetry(t *testing.T) {
	t.Helper()
	delay, ceiling := previewBusyRetryDelay, previewBusyRetryCap
	previewBusyRetryDelay, previewBusyRetryCap = time.Millisecond, time.Millisecond
	t.Cleanup(func() { previewBusyRetryDelay, previewBusyRetryCap = delay, ceiling })
}

// ── Steps ───────────────────────────────────────────────────────────────────

// No --steps, no steps. A step executes for real with viewer authority, so the
// absence of the flag has to mean the absence of the field on the wire — not a
// plan the CLI thought would be helpful.
func TestAppTestSendsNoStepsWithoutTheFlag(t *testing.T) {
	f := newFakeAppInstance(t)
	root, _ := appTestFolder(t, f)

	if _, err := runCLI(t, root, "app", "test", "--out-dir", t.TempDir()); err != nil {
		t.Fatalf("app test: %v", err)
	}
	if len(f.previewRequests) != 1 {
		t.Fatalf("expected one preview call, got %d", len(f.previewRequests))
	}
	if got := f.previewRequests[0].Steps; len(got) != 0 {
		t.Errorf("no --steps means no steps on the wire, got %+v", got)
	}
	// And the call is addressed to the row the folder is bound to; the server
	// decides for itself whether to substitute the caller's draft.
	if f.previewTargets[0] != "data_app-1" {
		t.Errorf("preview should address the bound app, got %q", f.previewTargets[0])
	}
}

func TestAppTestSendsTheStepsFile(t *testing.T) {
	f := newFakeAppInstance(t)
	root, _ := appTestFolder(t, f)

	plan := filepath.Join(t.TempDir(), "steps.json")
	if err := os.WriteFile(plan, []byte(
		`[{"action":"fill","selector":"#q","value":"EU"},{"action":"click","text":"Refresh"},{"action":"screenshot"}]`), 0o600); err != nil {
		t.Fatal(err)
	}

	if _, err := runCLI(t, root, "app", "test", "--out-dir", t.TempDir(), "--steps", plan); err != nil {
		t.Fatalf("app test: %v", err)
	}
	got := f.previewRequests[0].Steps
	if len(got) != 3 {
		t.Fatalf("expected the three declared steps, got %+v", got)
	}
	if got[0].Action != "fill" || got[0].Selector != "#q" || got[0].Value != "EU" {
		t.Errorf("the fill step did not survive the round trip: %+v", got[0])
	}
	if got[1].Text != "Refresh" {
		t.Errorf("the click step's text selector did not survive: %+v", got[1])
	}
}

// A mistyped key is refused rather than sent. Decoded leniently, `"selctor"`
// becomes a click with no selector, which the harness reports as a step that
// could not find its target — a fact about the APP, for a typo in a file.
func TestAppTestRefusesAMalformedStepsFile(t *testing.T) {
	f := newFakeAppInstance(t)
	root, _ := appTestFolder(t, f)
	dir := t.TempDir()

	cases := []struct {
		name string
		body string
		want string
	}{
		{"not JSON at all", `{ this is not json`, "must hold a JSON ARRAY"},
		{"a bare object", `{"action":"click","text":"Refresh"}`, "must hold a JSON ARRAY"},
		{"a mistyped key", `[{"action":"click","selctor":"#q"}]`, "selctor"},
		{"an empty plan", `[]`, "names no steps"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			plan := filepath.Join(dir, "steps.json")
			if err := os.WriteFile(plan, []byte(tc.body), 0o600); err != nil {
				t.Fatal(err)
			}
			_, err := runCLI(t, root, "app", "test", "--out-dir", dir, "--steps", plan)
			if err == nil {
				t.Fatal("a plan that cannot be read must be refused, not guessed at")
			}
			if !strings.Contains(err.Error(), tc.want) {
				t.Errorf("the refusal should name what is wrong (%q), got: %v", tc.want, err)
			}
			if f.previewCalls != 0 {
				t.Errorf("nothing should have been rendered; %d preview calls were made", f.previewCalls)
			}
		})
	}
}

// A missing file is named, not swallowed.
func TestAppTestRefusesAMissingStepsFile(t *testing.T) {
	f := newFakeAppInstance(t)
	root, _ := appTestFolder(t, f)

	_, err := runCLI(t, root, "app", "test", "--steps", filepath.Join(t.TempDir(), "nope.json"))
	if err == nil {
		t.Fatal("a --steps file that does not exist must be refused")
	}
	if !strings.Contains(err.Error(), "interaction plan") {
		t.Errorf("the refusal should say what could not be read, got: %v", err)
	}
}

// ── Refusals that cost no round trip ────────────────────────────────────────

// A workflow folder is not a data-app folder. The manifest's `kind` is the only
// thing that tells the three folder shapes apart, and LoadManifest's refusal has
// to reach the caller rather than being swallowed into a confusing later error.
func TestAppTestRefusesAWorkflowFolder(t *testing.T) {
	f := newFakeAppInstance(t)
	signInApp(t, f)

	root := t.TempDir()
	if err := wfdir.SaveManifest(root, &wfdir.Manifest{
		Kind: wfdir.KindWorkflow, Title: "Monthly close", Entrypoint: "main.py",
	}); err != nil {
		t.Fatal(err)
	}

	_, err := runCLI(t, root, "app", "test", "--out-dir", t.TempDir())
	if err == nil {
		t.Fatal("`app test` inside a workflow folder must be refused")
	}
	if f.previewCalls != 0 {
		t.Errorf("nothing should have been rendered, got %d preview calls", f.previewCalls)
	}
}

// An unknown viewport is refused locally, because the SERVER resolves one to
// desktop rather than failing — so a typo would otherwise come back as a
// perfectly successful answer about the wrong layout.
func TestAppTestRefusesAnUnknownViewport(t *testing.T) {
	f := newFakeAppInstance(t)
	root, _ := appTestFolder(t, f)

	_, err := runCLI(t, root, "app", "test", "--viewport", "mobil", "--out-dir", t.TempDir())
	if err == nil {
		t.Fatal("an unknown --viewport must be refused rather than silently rendered as desktop")
	}
	if f.previewCalls != 0 {
		t.Errorf("the refusal should cost no round trip, got %d preview calls", f.previewCalls)
	}
}

// An unbound folder has nothing to render, and says which command creates it.
func TestAppTestRefusesAnUnboundFolder(t *testing.T) {
	f := newFakeAppInstance(t)
	signInApp(t, f)
	root := writeAppFolder(t, t.TempDir(), &wfdir.Manifest{Title: "Revenue explorer"},
		map[string]string{"App.tsx": "SOURCE"})

	_, err := runCLI(t, root, "app", "test", "--out-dir", t.TempDir())
	if err == nil {
		t.Fatal("an unbound folder has no app to render")
	}
	if !strings.Contains(err.Error(), "ronja app push") {
		t.Errorf("the refusal should name the command that creates the app, got: %v", err)
	}
}

// The render budget reaches the wire in milliseconds — the flag is a duration
// because that is what a person types, and the endpoint takes an integer.
func TestAppTestSendsTheRenderBudgetInMilliseconds(t *testing.T) {
	f := newFakeAppInstance(t)
	root, _ := appTestFolder(t, f)

	if _, err := runCLI(t, root, "app", "test", "--timeout", "8s", "--route", "orders",
		"--viewport", "mobile", "--out-dir", t.TempDir()); err != nil {
		t.Fatalf("app test: %v", err)
	}
	req := f.previewRequests[0]
	if req.TimeoutMs != 8000 {
		t.Errorf("--timeout 8s should arrive as 8000ms, got %d", req.TimeoutMs)
	}
	if req.ViewportPreset != "mobile" {
		t.Errorf("--viewport should reach the wire, got %q", req.ViewportPreset)
	}
	// The route is passed through untouched; the server owns the '#' rule, and a
	// client that normalised it would be a second opinion about the grammar.
	if req.Route != "orders" {
		t.Errorf("--route should reach the wire verbatim, got %q", req.Route)
	}
}

// A frame that cannot be fetched is a warning, not a failure: the observation is
// already in hand, and losing the command over a missing picture would throw
// away the answer for its illustration.
func TestAppTestSurvivesAFrameItCannotFetch(t *testing.T) {
	f := newFakeAppInstance(t)
	root, app := appTestFolder(t, f)
	f.AddFileBlob("file-2", "dataapp/preview/2.png", []byte("\x89PNG-settled"))
	f.AnswerPreviewResult(api.PreviewResult{
		Rendered: true, Viewport: "desktop", DataAppID: app.ID,
		Ops: []api.PreviewOp{}, Errors: []api.PreviewRuntimeError{},
		ConsoleErrors: []api.PreviewConsoleError{},
		Screenshots: []api.PreviewScreenshot{
			{FileID: "file-gone", Phase: "first_paint"},
			{FileID: "file-2", Phase: "settled", Selected: true},
		},
	})

	outDir := t.TempDir()
	var err error
	stderr := captureStderr(t, func() {
		_, err = runCLI(t, root, "app", "test", "--out-dir", outDir)
	})
	if err != nil {
		t.Fatalf("an unfetchable frame must not fail the command: %v", err)
	}
	if !strings.Contains(stderr, "could not fetch frame 1") {
		t.Errorf("the missing frame should be warned about on stderr, got:\n%s", stderr)
	}
	if got := readFile(t, outDir, "screenshot-2.png"); got != "\x89PNG-settled" {
		t.Errorf("the frames that did arrive must still be written, got %q", got)
	}
	if _, err := os.Stat(filepath.Join(outDir, "screenshot-1.png")); !os.IsNotExist(err) {
		t.Errorf("a frame that never arrived must not leave a file behind (%v)", err)
	}
}

// ── The frame fetch ─────────────────────────────────────────────────────────

// TestAppTestPrefersTheFrameKey pins the ONE-CALL path end to end.
//
// A preview hands back both an id and a storage key, and only the key leads to a
// route this command's own scope can reach: /file/download/:key is data-scoped
// like the preview itself, while resolving an id goes through a group carrying
// no scope annotation at all, which a scoped token is refused on. Both legs end
// in the same bytes on disk, so the only observable difference is which requests
// were made — which is why the harness counts them.
func TestAppTestPrefersTheFrameKey(t *testing.T) {
	f := newFakeAppInstance(t)
	root, app := appTestFolder(t, f)
	f.AddFileBlob("file-1", "toolresult/preview/a b.png", []byte("\x89PNG-settled"))
	f.AnswerPreviewResult(api.PreviewResult{
		Rendered: true, Viewport: "desktop", DataAppID: app.ID,
		Ops: []api.PreviewOp{}, Errors: []api.PreviewRuntimeError{},
		ConsoleErrors: []api.PreviewConsoleError{},
		Screenshots: []api.PreviewScreenshot{
			// The realistic shape: the server sends both.
			{FileID: "file-1", FileKey: "toolresult/preview/a b.png", Phase: "settled", Selected: true},
		},
	})

	outDir := t.TempDir()
	if _, err := runCLI(t, root, "app", "test", "--out-dir", outDir); err != nil {
		t.Fatalf("app test: %v", err)
	}
	if got := readFile(t, outDir, "screenshot.png"); got != "\x89PNG-settled" {
		t.Errorf("the frame should have been fetched by key, got %q", got)
	}
	if f.fileRowCalls != 0 {
		t.Errorf("the id-resolving fallback must not run when a key was handed over (%d calls) — a scoped token cannot reach that route at all", f.fileRowCalls)
	}
	// The key contains a slash AND a space; both survived the round trip, which
	// is what escaping the whole key into one path segment is for.
	if f.fileDownloadCalls != 1 {
		t.Errorf("expected exactly one download call, got %d", f.fileDownloadCalls)
	}
}

// A frame carrying ONLY a key is fetchable — the key is the preferred field —
// so it must not be skipped for want of an id.
func TestAppTestFetchesAKeyOnlyFrame(t *testing.T) {
	f := newFakeAppInstance(t)
	root, app := appTestFolder(t, f)
	f.AddFileBlob("file-1", "toolresult/preview/1.png", []byte("\x89PNG-keyonly"))
	f.AnswerPreviewResult(api.PreviewResult{
		Rendered: true, Viewport: "desktop", DataAppID: app.ID,
		Ops: []api.PreviewOp{}, Errors: []api.PreviewRuntimeError{},
		ConsoleErrors: []api.PreviewConsoleError{},
		Screenshots: []api.PreviewScreenshot{
			{FileKey: "toolresult/preview/1.png", Phase: "settled", Selected: true},
		},
	})

	outDir := t.TempDir()
	if _, err := runCLI(t, root, "app", "test", "--out-dir", outDir); err != nil {
		t.Fatalf("app test: %v", err)
	}
	if got := readFile(t, outDir, "screenshot-1.png"); got != "\x89PNG-keyonly" {
		t.Errorf("a key-only frame must still be written, got %q", got)
	}
	if got := readFile(t, outDir, "screenshot.png"); got != "\x89PNG-keyonly" {
		t.Errorf("…and still be the representative frame, got %q", got)
	}
}

// ── What is NOT written ─────────────────────────────────────────────────────

// A render that captured nothing writes no screenshot at all, and says which of
// the two reasons applies.
func TestAppTestWritesNoFrameWhenNoneWereCaptured(t *testing.T) {
	f := newFakeAppInstance(t)
	root, app := appTestFolder(t, f)
	f.AnswerPreviewResult(api.PreviewResult{
		Rendered: true, Viewport: "desktop", DataAppID: app.ID,
		Ops: []api.PreviewOp{}, Errors: []api.PreviewRuntimeError{},
		ConsoleErrors: []api.PreviewConsoleError{},
		Screenshots:   []api.PreviewScreenshot{},
	})

	outDir := t.TempDir()
	out, err := runCLI(t, root, "app", "test", "--out-dir", outDir)
	if err != nil {
		t.Fatalf("app test: %v", err)
	}
	if !strings.Contains(out, "No frames were captured") {
		t.Errorf("the report should say no frames were captured, got:\n%s", out)
	}
	for _, name := range []string{"screenshot.png", "screenshot-1.png"} {
		if _, err := os.Stat(filepath.Join(outDir, name)); !os.IsNotExist(err) {
			t.Errorf("%s must not exist when nothing was captured (%v)", name, err)
		}
	}
}

// Frames that ALL failed to fetch is a different fact from no frames, and it
// sends a reader somewhere else: one is about the render, the other about this
// machine's reach to the instance's file storage. Conflating them was the whole
// bug.
func TestAppTestDistinguishesUnfetchableFramesFromNone(t *testing.T) {
	f := newFakeAppInstance(t)
	root, app := appTestFolder(t, f)
	f.AnswerPreviewResult(api.PreviewResult{
		Rendered: true, Viewport: "desktop", DataAppID: app.ID,
		Ops: []api.PreviewOp{}, Errors: []api.PreviewRuntimeError{},
		ConsoleErrors: []api.PreviewConsoleError{},
		Screenshots: []api.PreviewScreenshot{
			{FileID: "file-gone", FileKey: "toolresult/gone.png", Phase: "settled", Selected: true},
		},
	})

	outDir := t.TempDir()
	var out string
	var err error
	captureStderr(t, func() {
		out, err = runCLI(t, root, "app", "test", "--out-dir", outDir)
	})
	if err != nil {
		t.Fatalf("app test: %v", err)
	}
	if strings.Contains(out, "No frames were captured") {
		t.Errorf("frames WERE captured — they could not be fetched, and saying otherwise points at the wrong thing:\n%s", out)
	}
	if !strings.Contains(out, "none of them could be fetched") {
		t.Errorf("the report should say the frames could not be fetched, got:\n%s", out)
	}
}

// A filmstrip with no frame marked `selected` writes the numbered frames and NO
// screenshot.png — a stable name pointing at an arbitrary frame would be a
// picture nobody chose.
func TestAppTestWritesNoSelectedFrameWhenNoneIsMarked(t *testing.T) {
	f := newFakeAppInstance(t)
	root, app := appTestFolder(t, f)
	f.AddFileBlob("file-1", "toolresult/1.png", []byte("\x89PNG-one"))
	f.AnswerPreviewResult(api.PreviewResult{
		Rendered: true, Viewport: "desktop", DataAppID: app.ID,
		Ops: []api.PreviewOp{}, Errors: []api.PreviewRuntimeError{},
		ConsoleErrors: []api.PreviewConsoleError{},
		Screenshots: []api.PreviewScreenshot{
			{FileID: "file-1", FileKey: "toolresult/1.png", Phase: "settled"},
		},
	})

	outDir := t.TempDir()
	if _, err := runCLI(t, root, "app", "test", "--out-dir", outDir); err != nil {
		t.Fatalf("app test: %v", err)
	}
	if got := readFile(t, outDir, "screenshot-1.png"); got != "\x89PNG-one" {
		t.Errorf("the captured frame must still be written, got %q", got)
	}
	if _, err := os.Stat(filepath.Join(outDir, "screenshot.png")); !os.IsNotExist(err) {
		t.Errorf("screenshot.png must not be written when no frame is marked selected (%v)", err)
	}
}

// The stale-artefact bug: a second run that captures nothing must not leave the
// FIRST run's picture sitting beside a report.json describing a different
// render. A wrong picture is the one artefact here that misleads in silence.
func TestAppTestClearsThePreviousRunsFrames(t *testing.T) {
	f := newFakeAppInstance(t)
	root, app := appTestFolder(t, f)
	f.AddFileBlob("file-1", "toolresult/1.png", []byte("\x89PNG-first-run"))
	f.AnswerPreviewResult(api.PreviewResult{
		Rendered: true, Viewport: "desktop", DataAppID: app.ID,
		Ops: []api.PreviewOp{}, Errors: []api.PreviewRuntimeError{},
		ConsoleErrors: []api.PreviewConsoleError{},
		Screenshots: []api.PreviewScreenshot{
			{FileID: "file-1", FileKey: "toolresult/1.png", Phase: "settled", Selected: true},
		},
	})
	// The second answer captured nothing at all.
	f.AnswerPreviewResult(api.PreviewResult{
		Rendered: true, Viewport: "desktop", DataAppID: app.ID,
		Ops: []api.PreviewOp{}, Errors: []api.PreviewRuntimeError{},
		ConsoleErrors: []api.PreviewConsoleError{},
		Screenshots:   []api.PreviewScreenshot{},
	})

	outDir := t.TempDir()
	if _, err := runCLI(t, root, "app", "test", "--out-dir", outDir); err != nil {
		t.Fatalf("first run: %v", err)
	}
	// A file this command does NOT own, to prove the sweep is by name.
	keep := filepath.Join(outDir, "notes.md")
	if err := os.WriteFile(keep, []byte("mine"), 0o600); err != nil {
		t.Fatal(err)
	}

	if _, err := runCLI(t, root, "app", "test", "--out-dir", outDir); err != nil {
		t.Fatalf("second run: %v", err)
	}
	for _, name := range []string{"screenshot.png", "screenshot-1.png"} {
		if _, err := os.Stat(filepath.Join(outDir, name)); !os.IsNotExist(err) {
			t.Errorf("%s survived a run that captured nothing — it is a picture of the PREVIOUS render (%v)", name, err)
		}
	}
	if got := readFile(t, outDir, "notes.md"); got != "mine" {
		t.Errorf("--out-dir may be a directory somebody keeps things in; only this command's own names may be removed (got %q)", got)
	}
}

// ── Untrusted text on the terminal ──────────────────────────────────────────

// Every message printed here is text the APP produced. An ESC starts an ANSI
// sequence that repaints lines already on screen — so a failing render could
// print itself a clean one — and bidi overrides and zero-width characters
// reorder or hide what is left. A terminal gives a reader nothing to tell that
// apart from the CLI's own words.
func TestAppTestDisarmsAppAuthoredText(t *testing.T) {
	f := newFakeAppInstance(t)
	root, app := appTestFolder(t, f)
	f.AnswerPreviewResult(api.PreviewResult{
		Rendered: true, Viewport: "desktop", DataAppID: app.ID,
		Ops: []api.PreviewOp{},
		Errors: []api.PreviewRuntimeError{
			{Message: "boom\x1b[2K\x1b[1A  Rendered: yes‮gnorw", Source: "App​tsx", Line: 1, Column: 1},
		},
		ConsoleErrors: []api.PreviewConsoleError{},
		Screenshots:   []api.PreviewScreenshot{},
		Hint:          "the app said \x1b[31mnothing useful\x1b[0m",
	})

	out, err := runCLI(t, root, "app", "test", "--out-dir", t.TempDir())
	if err != nil {
		t.Fatalf("app test: %v", err)
	}
	for _, forbidden := range []string{"\x1b", "‮", "​"} {
		if strings.Contains(out, forbidden) {
			t.Errorf("%q reached the terminal — app text must be disarmed before it is printed:\n%q", forbidden, out)
		}
	}
	// The message itself is still readable; disarming is not redaction.
	if !strings.Contains(out, "boom") || !strings.Contains(out, "nothing useful") {
		t.Errorf("the message must survive its own sanitisation, got:\n%s", out)
	}
}

// The server frames every app-authored message in <untrusted_app_output> so a
// model reading the JSON knows it is evidence rather than direction. A terminal
// reader needs no such frame — nothing here is being handed to a model — and
// printing the tags would leave them reading markup around their own error.
// report.json keeps them; the summary does not.
func TestAppTestStripsTheUntrustedEnvelopeForDisplay(t *testing.T) {
	f := newFakeAppInstance(t)
	root, app := appTestFolder(t, f)
	f.AnswerPreviewResult(api.PreviewResult{
		Rendered: true, Viewport: "desktop", DataAppID: app.ID,
		Ops: []api.PreviewOp{},
		Errors: []api.PreviewRuntimeError{
			{Message: "<untrusted_app_output>Cannot read properties of undefined</untrusted_app_output>",
				Source: "App.tsx", Line: 270, Column: 28},
		},
		ConsoleErrors: []api.PreviewConsoleError{},
		Screenshots:   []api.PreviewScreenshot{},
		// A tag family the CLI was not written for. Matching by <untrusted_*>
		// prefix rather than by name is what keeps a new server-side envelope
		// from showing up as markup in front of a reader.
		Hint: "<untrusted_page_text>the page showed nothing</untrusted_page_text>",
	})

	outDir := t.TempDir()
	out, err := runCLI(t, root, "app", "test", "--out-dir", outDir)
	if err != nil {
		t.Fatalf("app test: %v", err)
	}
	if strings.Contains(out, "untrusted_") {
		t.Errorf("the envelope must not reach the terminal:\n%s", out)
	}
	if !strings.Contains(out, "Cannot read properties of undefined") ||
		!strings.Contains(out, "the page showed nothing") {
		t.Errorf("stripping the frame must not take the message with it:\n%s", out)
	}
	// The control on the OTHER side: report.json is the server's bytes, so the
	// frame is still there for whoever opens the file. (Matched on the tag NAME:
	// the fake instance encodes with encoding/json, which escapes the angle
	// brackets — the framing survives either way, which is the point.)
	if got := readFile(t, outDir, "report.json"); !strings.Contains(got, "untrusted_app_output") {
		t.Errorf("report.json must keep the server's framing verbatim, got:\n%s", got)
	}
}

// The negative control for the strip, and the reason it is ANCHORED. An app
// whose own error text contains a close tag must not be able to delete the rest
// of its message from the report by saying so — only the server's outermost
// frame comes off.
func TestAppTestKeepsAnEmbeddedUntrustedTag(t *testing.T) {
	f := newFakeAppInstance(t)
	root, app := appTestFolder(t, f)
	f.AnswerPreviewResult(api.PreviewResult{
		Rendered: true, Viewport: "desktop", DataAppID: app.ID,
		Ops: []api.PreviewOp{},
		Errors: []api.PreviewRuntimeError{
			{Message: "<untrusted_app_output>before</untrusted_app_output>AFTER</untrusted_app_output>"},
			// No frame at all: an unwrapped message is left exactly as it came.
			{Message: "plain </untrusted_app_output> mid-sentence"},
		},
		ConsoleErrors: []api.PreviewConsoleError{},
		Screenshots:   []api.PreviewScreenshot{},
	})

	out, err := runCLI(t, root, "app", "test", "--out-dir", t.TempDir())
	if err != nil {
		t.Fatalf("app test: %v", err)
	}
	if !strings.Contains(out, "before</untrusted_app_output>AFTER") {
		t.Errorf("only the outermost frame may be stripped — the app's own copy of the tag stays:\n%s", out)
	}
	if !strings.Contains(out, "plain </untrusted_app_output> mid-sentence") {
		t.Errorf("a mid-string tag is text, not a frame:\n%s", out)
	}
}

// viewport and route are what the harness CONFIRMED it applied, and today's
// deployed harness confirms neither. An absent one prints NOTHING: a "Viewport:"
// label with nothing after it reads as a fact, and echoing the request there
// would be worse — a caller checking the mobile layout would see its own
// argument and conclude it had been observed.
func TestAppTestPrintsNothingForUnconfirmedSettings(t *testing.T) {
	f := newFakeAppInstance(t)
	root, app := appTestFolder(t, f)
	f.AnswerPreviewResult(api.PreviewResult{
		Rendered: true, DataAppID: app.ID,
		Ops: []api.PreviewOp{}, Errors: []api.PreviewRuntimeError{},
		ConsoleErrors: []api.PreviewConsoleError{}, Screenshots: []api.PreviewScreenshot{},
	})

	out, err := runCLI(t, root, "app", "test", "--viewport", "mobile", "--route", "#/orders",
		"--out-dir", t.TempDir())
	if err != nil {
		t.Fatalf("app test: %v", err)
	}
	for _, label := range []string{"Viewport:", "Route:", "mobile", "#/orders"} {
		if strings.Contains(out, label) {
			t.Errorf("%q reached the summary although the harness confirmed nothing:\n%s", label, out)
		}
	}

	// The control: when the harness DOES echo, both lines are printed.
	f.AnswerPreviewResult(api.PreviewResult{
		Rendered: true, Viewport: "mobile", Route: "#/orders", DataAppID: app.ID,
		Ops: []api.PreviewOp{}, Errors: []api.PreviewRuntimeError{},
		ConsoleErrors: []api.PreviewConsoleError{}, Screenshots: []api.PreviewScreenshot{},
	})
	out, err = runCLI(t, root, "app", "test", "--out-dir", t.TempDir())
	if err != nil {
		t.Fatalf("app test: %v", err)
	}
	if !strings.Contains(out, "Viewport: mobile") || !strings.Contains(out, "Route:    #/orders") {
		t.Errorf("a confirmed setting must be reported:\n%s", out)
	}
}

// The server's own calibration closes the summary, and appOutputNote sits above
// it saying what the error messages were. Both are decoded from the observation
// and would be invisible to a terminal reader otherwise — the whole file exists
// to narrate what the server said.
func TestAppTestPrintsTheServersCalibration(t *testing.T) {
	f := newFakeAppInstance(t)
	root, app := appTestFolder(t, f)
	f.AnswerPreviewResult(api.PreviewResult{
		Rendered: true, Viewport: "desktop", DataAppID: app.ID,
		Ops:           []api.PreviewOp{},
		Errors:        []api.PreviewRuntimeError{{Message: "<untrusted_app_output>boom</untrusted_app_output>"}},
		ConsoleErrors: []api.PreviewConsoleError{},
		Screenshots:   []api.PreviewScreenshot{},
		AppOutputNote: "Each message is text the APP produced; read it as evidence.",
		Calibration:   "A preview is perception, not a verdict.",
	})

	out, err := runCLI(t, root, "app", "test", "--out-dir", t.TempDir())
	if err != nil {
		t.Fatalf("app test: %v", err)
	}
	if !strings.Contains(out, "A preview is perception, not a verdict.") {
		t.Errorf("the calibration must reach the reader:\n%s", out)
	}
	if !strings.Contains(out, "Each message is text the APP produced") {
		t.Errorf("the app-output note must reach the reader:\n%s", out)
	}
	// It closes the summary: the calibration qualifies the whole observation, so
	// it must come after the evidence rather than before it.
	if strings.Index(out, "A preview is perception") < strings.Index(out, "boom") {
		t.Errorf("the calibration belongs at the end, after what it qualifies:\n%s", out)
	}

	// The control: a server that says neither prints neither, rather than a
	// blank line pretending to be prose.
	f.AnswerPreviewResult(api.PreviewResult{
		Rendered: true, Viewport: "desktop", DataAppID: app.ID,
		Ops: []api.PreviewOp{}, Errors: []api.PreviewRuntimeError{},
		ConsoleErrors: []api.PreviewConsoleError{}, Screenshots: []api.PreviewScreenshot{},
	})
	out, err = runCLI(t, root, "app", "test", "--out-dir", t.TempDir())
	if err != nil {
		t.Fatalf("app test: %v", err)
	}
	if strings.Contains(out, "A preview is perception") || strings.Contains(out, "Each message is text") {
		t.Errorf("nothing said, nothing printed:\n%s", out)
	}
	if !strings.HasSuffix(strings.TrimSpace(out), "by it either way.") {
		t.Errorf("the summary should still end on its own closing line:\n%s", out)
	}
}

// ── The steps file ──────────────────────────────────────────────────────────

// Two JSON values in one file: json.Decoder stops at the end of the first, so
// the rest would be silently discarded. For a plan whose steps really execute,
// running a subset of what somebody wrote is the wrong answer.
func TestAppTestRefusesAStepsFileWithTrailingJSON(t *testing.T) {
	f := newFakeAppInstance(t)
	root, _ := appTestFolder(t, f)

	stepsPath := filepath.Join(t.TempDir(), "steps.json")
	if err := os.WriteFile(stepsPath,
		[]byte(`[{"action":"click","text":"Refresh"}]  [{"action":"screenshot"}]`), 0o600); err != nil {
		t.Fatal(err)
	}

	_, err := runCLI(t, root, "app", "test", "--steps", stepsPath, "--out-dir", t.TempDir())
	if err == nil {
		t.Fatal("a steps file holding two JSON values must be refused, not half-run")
	}
	if !strings.Contains(err.Error(), "more than one JSON value") {
		t.Errorf("the refusal should name the problem, got: %v", err)
	}
	if f.previewCalls != 0 {
		t.Errorf("nothing should have been sent: %d preview calls", f.previewCalls)
	}
}
