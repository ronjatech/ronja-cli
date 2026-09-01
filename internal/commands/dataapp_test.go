package commands

import (
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/ronjatech/ronja-cli/internal/api"
	"github.com/ronjatech/ronja-cli/internal/wfdir"
)

// ── The draft-resolution invariant ──────────────────────────────────────────

// TestAppStatusReadsTheDraftNotTheLiveApp is the regression for the deviation
// that has no workflow equivalent.
//
// GET /dataapp/:id/files answers with exactly the row named (it once silently
// answered with the caller's open draft, which is why this guard exists). A
// command that addressed the LIVE id while meaning the draft would record
// them in the baseline under the live app's identity, and every later drift
// comparison would be against a row it never named.
//
// The assertion is therefore about the REQUEST, not the outcome: with a draft
// open, no file read may be addressed to the live app. An outcome assertion
// would pass by coincidence here, since the server hands back the same bytes
// either way — which is exactly what makes the bug invisible.
func TestAppStatusReadsTheDraftNotTheLiveApp(t *testing.T) {
	f := newFakeAppInstance(t)
	live := f.AddApp(&api.DataApp{ID: "data_app-1"},
		api.DataAppFile{Path: "App.tsx", Content: "LIVE"})
	draft := f.AddDraft(live.ID, "data_app-draft-1",
		api.DataAppFile{Path: "App.tsx", Content: "DRAFT"})
	signInApp(t, f)

	root := writeAppFolder(t, t.TempDir(), &wfdir.Manifest{Title: "Revenue explorer"},
		map[string]string{"App.tsx": "DRAFT"})
	m := appManifestOf(t, root)
	m.SetBinding(f.Key(), wfdir.Binding{DataAppID: live.ID, FeatureID: "feat-1"})
	if err := wfdir.SaveManifest(root, m); err != nil {
		t.Fatal(err)
	}

	out, err := runCLI(t, root, "app", "status", "--json")
	if err != nil {
		t.Fatalf("status: %v", err)
	}

	if slices.Contains(f.FileReads, live.ID) {
		t.Errorf("status read the LIVE app's files (%v) while a draft was open — the server would have answered with the draft's bytes anyway, so the baseline would name the wrong row",
			f.FileReads)
	}
	if !slices.Contains(f.FileReads, draft.ID) {
		t.Errorf("status never read the draft's files; reads were %v", f.FileReads)
	}

	var report appStatusReport
	decodeJSONInto(t, out, &report)
	if report.Remote.ComparedAgainst == nil || report.Remote.ComparedAgainst.ID != draft.ID {
		t.Errorf("drift should be measured against the draft %s, got %+v", draft.ID, report.Remote.ComparedAgainst)
	}
}

// TestAppCloneReadsTheDraftNotTheLiveApp is the same invariant for clone, which
// has its own reason to reach for files.
func TestAppCloneReadsTheDraftNotTheLiveApp(t *testing.T) {
	f := newFakeAppInstance(t)
	live := f.AddApp(&api.DataApp{ID: "data_app-1"},
		api.DataAppFile{Path: "App.tsx", Content: "LIVE"})
	draft := f.AddDraft(live.ID, "data_app-draft-1",
		api.DataAppFile{Path: "App.tsx", Content: "DRAFT"})
	signInApp(t, f)

	dir := t.TempDir() + "/cloned"
	if _, err := runCLI(t, t.TempDir(), "app", "clone", live.ID, dir); err != nil {
		t.Fatalf("clone: %v", err)
	}
	if slices.Contains(f.FileReads, live.ID) {
		t.Errorf("clone read the LIVE app's files (%v) while a draft was open", f.FileReads)
	}
	if got := readFile(t, dir, "App.tsx"); got != "DRAFT" {
		t.Errorf("clone should have written the draft's content, got %q", got)
	}
	// The BINDING names the stable identity even though the files came from the
	// draft: a draft id dies at commit and this file lives in git.
	m := appManifestOf(t, dir)
	binding, _, _ := m.Binding(f.Key())
	if binding.DataAppID != live.ID {
		t.Errorf("binding should name the live app %s, got %q", live.ID, binding.DataAppID)
	}
	// The BASELINE names the row the bytes actually came from.
	if base := appStateOf(t, dir).For(f.Key()); base == nil || base.SourceID != draft.ID {
		t.Errorf("baseline should record the draft %s as its source, got %+v", draft.ID, base)
	}
}

// `app init` must leave a folder that renders — the starter it writes has to
// carry the mount call, since that is the entire reason it writes one.
func TestAppInitScaffoldsAMountingEntrypoint(t *testing.T) {
	f := newFakeAppInstance(t)
	signInApp(t, f)
	root := t.TempDir()

	if _, err := runCLI(t, root, "app", "init", "--feature", "feat-1", "--title", "Revenue explorer"); err != nil {
		t.Fatalf("init: %v", err)
	}

	body, err := os.ReadFile(filepath.Join(root, "App.tsx"))
	if err != nil {
		t.Fatalf("init must scaffold an entrypoint: %v", err)
	}
	src := string(body)
	// The mount call, which is the whole point.
	if !strings.Contains(src, `createRoot(document.getElementById("app"))`) {
		t.Errorf("the starter must mount itself, got:\n%s", src)
	}
	if !strings.Contains(src, "Revenue explorer") {
		t.Errorf("the starter should use the given title, got:\n%s", src)
	}
}

// An existing entrypoint is the author's. Overwriting it would be the worst
// thing an init command could do, so this pins the refusal.
func TestAppInitNeverOverwritesAnExistingEntrypoint(t *testing.T) {
	f := newFakeAppInstance(t)
	signInApp(t, f)
	root := t.TempDir()
	mine := "// mine, do not touch\n"
	if err := os.WriteFile(filepath.Join(root, "App.tsx"), []byte(mine), 0o644); err != nil {
		t.Fatal(err)
	}

	if _, err := runCLI(t, root, "app", "init", "--feature", "feat-1"); err != nil {
		t.Fatalf("init: %v", err)
	}

	body, err := os.ReadFile(filepath.Join(root, "App.tsx"))
	if err != nil {
		t.Fatal(err)
	}
	if string(body) != mine {
		t.Errorf("init overwrote an existing entrypoint; got:\n%s", body)
	}
}

// The manifest's "capabilities" vocabulary lives in the command, statically, on
// both surfaces an author meets: `ronja app init --help` describes each name,
// and the post-init report — printed at the one moment they are about to write
// the manifest — enumerates them. The authoritative set is the backend's
// dataAppAllowedCaps minus its auto-granted members (rscripttoken/dataapp.go,
// which carries its own pin); the CLI is a separate module, so this is the
// CLI-side half of the drift net.
//
// The help is matched as TABLE ROWS rather than substrings — "ai" alone matches
// half the English words in it — and the report as the joined list it prints,
// which is built from declarableCapabilities and so covers whatever that holds.
func TestAppInitNamesTheDeclarableCapabilities(t *testing.T) {
	long := newDataAppInitCmd().Long
	for _, capability := range declarableCapabilities {
		row := "\n  " + capability + "  "
		if !strings.Contains(long, row) {
			t.Errorf("`ronja app init --help` no longer carries the capability table row for %q — the manifest vocabulary must live in the command", capability)
		}
	}

	f := newFakeAppInstance(t)
	signInApp(t, f)
	out, err := runCLI(t, t.TempDir(), "app", "init", "--feature", "feat-1", "--title", "Revenue explorer")
	if err != nil {
		t.Fatalf("init: %v", err)
	}
	if names := strings.Join(declarableCapabilities, ", "); !strings.Contains(out, names) {
		t.Errorf("the post-init report no longer names the capabilities (%s) — an author about to write the manifest reads this, not --help. Got:\n%s",
			names, out)
	}
}

// ── Push ────────────────────────────────────────────────────────────────────

// TestAppPushCreatesDraftAppAndSyncs covers the first push: no app exists, so
// one is created — as a parentless DRAFT, the same shape a workflow is created
// in — the binding is recorded, and the files land on it.
func TestAppPushCreatesDraftAppAndSyncs(t *testing.T) {
	f := newFakeAppInstance(t)
	signInApp(t, f)

	root := writeAppFolder(t, t.TempDir(),
		&wfdir.Manifest{Title: "Revenue explorer"},
		map[string]string{
			"App.tsx":       "export default function App(){ return <Chart/>; }",
			"lib/chart.tsx": "export const Chart = () => <div/>;",
		})
	m := appManifestOf(t, root)
	m.SetBinding(f.Key(), wfdir.Binding{FeatureID: "feat-1"})
	m.SetAccess(api.DataAppAccess{AllowedTableIDs: []string{"table-abc"}})
	if err := wfdir.SaveManifest(root, m); err != nil {
		t.Fatal(err)
	}

	out, err := runCLI(t, root, "app", "push", "--json")
	if err != nil {
		t.Fatalf("push: %v", err)
	}
	var result appPushResult
	decodeJSONInto(t, out, &result)

	if !result.Created {
		t.Error("first push should report that it created the app")
	}
	if result.DataAppID == "" || result.DraftID == "" {
		t.Fatalf("expected both ids, got %+v", result)
	}
	if len(f.created) != 1 {
		t.Fatalf("expected exactly one create, got %d", len(f.created))
	}
	// The allowlists go up WITH the create rather than in a later patch: the
	// compiler checks secret references against allowed_secret_ids on the very
	// first file write, so the grants have to be on the row before any file is.
	if got := f.created[0].AllowedTableIDs; len(got) != 1 || got[0] != "table-abc" {
		t.Errorf("create should carry the manifest's allowlist, got %v", got)
	}
	// …and the grants are REPORTED even though no patch was needed. A first push
	// is when every grant is new, so the one command that shows privileges must
	// not be silent exactly then.
	if len(result.AccessChanges) != 1 || len(result.AccessChanges[0].Added) != 1 ||
		result.AccessChanges[0].Added[0] != "table-abc" {
		t.Errorf("a first push must report what it granted, got %+v", result.AccessChanges)
	}
	// The binding is recorded, so a second push finds the app rather than
	// creating another.
	binding, _, _ := appManifestOf(t, root).Binding(f.Key())
	if binding.DataAppID != result.DataAppID {
		t.Errorf("manifest binding %q should name the created app %q", binding.DataAppID, result.DataAppID)
	}
	if got := f.FileContents(result.DraftID); len(got) != 2 {
		t.Errorf("expected both files on the draft, got %v", got)
	}
	if result.Compiles == nil || !*result.Compiles {
		t.Errorf("a clean push should report compiles=true, got %+v", result.Compiles)
	}
}

// A first push must not check out a draft: POST /dataapp already returns a
// parentless draft, and the app id and the draft id are the same row for the
// app's whole life.
//
// Asserted as a REQUEST count rather than through the result, because the bug
// this guards against is invisible in the result — a stray checkout would
// return a perfectly good draft, every file would land on it, and the only
// symptom would be the thing that made data apps diverge from workflows in the
// first place: a live, empty row left behind under a different id.
func TestAppFirstPushDoesNotCheckOutADraft(t *testing.T) {
	f := newFakeAppInstance(t)
	signInApp(t, f)

	root := writeAppFolder(t, t.TempDir(),
		&wfdir.Manifest{Title: "Revenue explorer"},
		map[string]string{"App.tsx": "export default function App(){ return <div/>; }"})
	m := appManifestOf(t, root)
	m.SetBinding(f.Key(), wfdir.Binding{FeatureID: "feat-1"})
	if err := wfdir.SaveManifest(root, m); err != nil {
		t.Fatal(err)
	}

	out, err := runCLI(t, root, "app", "push", "--json")
	if err != nil {
		t.Fatalf("push: %v", err)
	}
	var first appPushResult
	decodeJSONInto(t, out, &first)
	if f.checkouts != 0 {
		t.Errorf("a first push made %d checkout(s); the created row IS the draft", f.checkouts)
	}
	if first.DraftID != first.DataAppID {
		t.Errorf("draft id %q and app id %q must be the same row before publish",
			first.DraftID, first.DataAppID)
	}

	// A SECOND push, against the still-unpublished draft, must not check out
	// either: inspectAppTarget sees lifecycle=draft and edits it in place.
	if _, err := runCLI(t, root, "app", "push", "--json"); err != nil {
		t.Fatalf("second push: %v", err)
	}
	if f.checkouts != 0 {
		t.Errorf("re-pushing an unpublished draft made %d checkout(s); it is already the draft", f.checkouts)
	}
}

// TestAppPushWritesEntrypointLast pins the ordering.
//
// The reverse of `wf push`, and for a concrete reason: every data-app file write
// recompiles the whole bundle, so writing App.tsx last means a fresh app's
// intermediate compiles fail before the S3 upload rather than after it. The
// workflow rule that puts the entrypoint first — a rename needs the new file to
// exist before the row can name it — does not apply, because a data app's
// entrypoint cannot be renamed at all.
func TestAppPushWritesEntrypointLast(t *testing.T) {
	f := newFakeAppInstance(t)
	signInApp(t, f)

	root := writeAppFolder(t, t.TempDir(),
		&wfdir.Manifest{Title: "App"},
		map[string]string{
			"App.tsx":       "entry",
			"lib/chart.tsx": "chart",
			"lib/util.tsx":  "util",
		})
	m := appManifestOf(t, root)
	m.SetBinding(f.Key(), wfdir.Binding{FeatureID: "feat-1"})
	if err := wfdir.SaveManifest(root, m); err != nil {
		t.Fatal(err)
	}
	if _, err := runCLI(t, root, "app", "push", "--json"); err != nil {
		t.Fatalf("push: %v", err)
	}

	var writes []string
	for _, req := range f.Requests {
		if method, path, _ := strings.Cut(req, " "); method == "PUT" && strings.Contains(path, "/files/") {
			writes = append(writes, path[strings.Index(path, "/files/")+len("/files/"):])
		}
	}
	if len(writes) != 3 {
		t.Fatalf("expected 3 file writes, got %v", writes)
	}
	if writes[len(writes)-1] != "App.tsx" {
		t.Errorf("the entrypoint must be written LAST, got order %v", writes)
	}
}

// TestAppPushSurvivesIntermediateCompileFailure is the behaviour the backend's
// 200-with-compileError exists for.
//
// Pushing a multi-file app walks through states that cannot compile — App.tsx
// importing a module that is one request away — so a compile failure on a write
// must not stop the sync. The file is saved either way; only the final verdict
// decides whether the result is publishable.
func TestAppPushSurvivesIntermediateCompileFailure(t *testing.T) {
	f := newFakeAppInstance(t)
	signInApp(t, f)
	// The first file written reports a compile failure. The push must carry on
	// and finish the sync.
	f.compileFails["lib/chart.tsx"] = "App.tsx:1:0: unresolved import \"./chart\""

	root := writeAppFolder(t, t.TempDir(),
		&wfdir.Manifest{Title: "App"},
		map[string]string{"App.tsx": "entry", "lib/chart.tsx": "chart"})
	m := appManifestOf(t, root)
	m.SetBinding(f.Key(), wfdir.Binding{FeatureID: "feat-1"})
	if err := wfdir.SaveManifest(root, m); err != nil {
		t.Fatal(err)
	}

	out, err := runCLI(t, root, "app", "push", "--json")
	if err != nil {
		t.Fatalf("an intermediate compile failure must not fail the push: %v", err)
	}
	var result appPushResult
	decodeJSONInto(t, out, &result)
	if len(result.Pushed) != 2 {
		t.Errorf("both files should have been pushed despite the compile failure, got %v", result.Pushed)
	}
	if got := f.FileContents(result.DraftID); got["App.tsx"] != "entry" || got["lib/chart.tsx"] != "chart" {
		t.Errorf("both files should be on the draft, got %v", got)
	}
	// The closing validate is the verdict, and it is clean here.
	if result.Compiles == nil || !*result.Compiles {
		t.Errorf("the final validate should decide the verdict, got %+v", result.Compiles)
	}
}

// TestAppPushReportsFinalCompileFailure is the other half: when the FINAL state
// does not compile, the push reports it and exits non-zero — but still records
// what it wrote, because the files really are saved.
func TestAppPushReportsFinalCompileFailure(t *testing.T) {
	f := newFakeAppInstance(t)
	signInApp(t, f)
	f.validateFails = "App.tsx:2:0: Unexpected end of file"

	root := writeAppFolder(t, t.TempDir(),
		&wfdir.Manifest{Title: "App"},
		map[string]string{"App.tsx": "broken"})
	m := appManifestOf(t, root)
	m.SetBinding(f.Key(), wfdir.Binding{FeatureID: "feat-1"})
	if err := wfdir.SaveManifest(root, m); err != nil {
		t.Fatal(err)
	}

	out, err := runCLI(t, root, "app", "push", "--json")
	if err == nil {
		t.Fatal("a push whose result does not compile must exit non-zero")
	}
	var result appPushResult
	decodeJSONInto(t, out, &result)
	if result.Compiles == nil || *result.Compiles {
		t.Errorf("expected compiles=false, got %+v", result.Compiles)
	}
	if result.CompileError == nil || !strings.Contains(result.CompileError.Message, "Unexpected end of file") {
		t.Errorf("expected the compiler's message to be reported, got %+v", result.CompileError)
	}
	// The bytes landed, so the baseline must say so — otherwise the next push
	// reads the author's own work as somebody else's drift.
	if got := f.FileContents(result.DraftID)["App.tsx"]; got != "broken" {
		t.Errorf("the file should still be saved, got %q", got)
	}
	if base := appStateOf(t, root).For(f.Key()); base == nil || len(base.Files) != 1 {
		t.Errorf("the baseline should record the pushed file, got %+v", base)
	}
}

// TestAppPushRecordsFilesThatLandedDespiteAFailedWrite is the regression for a
// bug a live run found that no fake would have.
//
// rdataapp.UpsertFile COMMITS the file and only then recompiles and uploads the
// bundle, out of transaction. So a failure from that second half — the real case
// was an S3 misconfiguration — comes back as a 500 from a write that already
// landed. Treating it as "did not land" left the baseline claiming the file was
// absent while the server held it, and the retry then reported the author's own
// half-finished push as somebody else's drift and demanded --force.
//
// The fix is to ASK after any failed write. This pins that the baseline ends up
// describing the server, so the retry is an ordinary push.
func TestAppPushRecordsFilesThatLandedDespiteAFailedWrite(t *testing.T) {
	f := newFakeAppInstance(t)
	signInApp(t, f)
	// A 500 AFTER the row was written: the fake's putFile has already run by the
	// time this fires, exactly as the server's post-commit publish does.
	f.failPutAfterWrite = map[string]int{"App.tsx": 500}

	root := writeAppFolder(t, t.TempDir(), &wfdir.Manifest{Title: "App"},
		map[string]string{"App.tsx": "entry", "lib/chart.tsx": "chart"})
	m := appManifestOf(t, root)
	m.SetBinding(f.Key(), wfdir.Binding{FeatureID: "feat-1"})
	if err := wfdir.SaveManifest(root, m); err != nil {
		t.Fatal(err)
	}

	out, err := runCLI(t, root, "app", "push", "--json")
	if err == nil {
		t.Fatal("a genuine write failure must fail the push")
	}
	var result appPushResult
	decodeJSONInto(t, out, &result)

	// The server holds both files — the entrypoint's write committed before the
	// failure — so the baseline must say so.
	base := appStateOf(t, root).For(f.Key())
	if base == nil {
		t.Fatal("expected a baseline to be recorded after a partial push")
	}
	if _, ok := base.Files["App.tsx"]; !ok {
		t.Errorf("the baseline must record App.tsx, which DID land: %v", base.Files)
	}

	// And the retry is an ordinary push, not a drift refusal.
	f.failPutAfterWrite = nil
	if _, err := runCLI(t, root, "app", "push", "--json"); err != nil {
		t.Fatalf("the retry must not be refused as drift: %v", err)
	}
}

// TestAppPushNeverAcknowledgesContentItDidNotWrite is the other half of the rule
// above, and the more dangerous half.
//
// Asking after EVERY failed write is what the reconcile used to do, and it wrote
// whatever came back into the baseline. What comes back is the server's CURRENT
// content — under --force, precisely the colleague's change being forced past.
// Acknowledged, it disarms the drift guard: the next plain push sees nothing to
// refuse and overwrites that change in silence, which is the one thing a
// baseline must never do.
//
// Two routes reach it, and both are covered here. A 4xx is not uncertain at all
// — the server considered the write and refused it before the
// commit-then-recompile sequence began — so there is nothing to go and ask. And
// an answer that IS uncertain can still come back holding content that is not
// what we tried to write, which settles it just as firmly: the write missed.
func TestAppPushNeverAcknowledgesContentItDidNotWrite(t *testing.T) {
	cases := []struct {
		name   string
		status int
	}{
		// The server refused the write; there is nothing to ask about.
		{"a rejected write", 403},
		// Uncertain, so the CLI asks — and the answer is somebody else's content
		// rather than what it tried to write, so the write did not land.
		{"an uncertain write that did not land", 503},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			f := newFakeAppInstance(t)
			live := f.AddApp(&api.DataApp{ID: "data_app-1"})
			draft := f.AddDraft(live.ID, "data_app-draft-1",
				api.DataAppFile{Path: "App.tsx", Content: "entry"},
				api.DataAppFile{Path: "lib/chart.tsx", Content: "COLLEAGUE"})
			signInApp(t, f)

			root := writeAppFolder(t, t.TempDir(), &wfdir.Manifest{Title: "App"},
				map[string]string{"App.tsx": "entry", "lib/chart.tsx": "mine"})
			m := appManifestOf(t, root)
			m.SetBinding(f.Key(), wfdir.Binding{DataAppID: live.ID, FeatureID: "feat-1"})
			if err := wfdir.SaveManifest(root, m); err != nil {
				t.Fatal(err)
			}
			// A baseline recording what this folder last synced, so the colleague's
			// edit to lib/chart.tsx is drift — the drift --force runs past.
			state := &wfdir.State{}
			state.Set(f.Key(), baselineFromApp(draft, []api.DataAppFile{
				{Path: "App.tsx", Content: "entry"},
				{Path: "lib/chart.tsx", Content: "what I last synced"},
			}))
			if err := wfdir.SaveState(root, state); err != nil {
				t.Fatal(err)
			}
			// Fails BEFORE the row is written, unlike failPutAfterWrite.
			f.failPut = map[string]int{"lib/chart.tsx": tc.status}

			out, err := runCLI(t, root, "app", "push", "--force", "--json")
			if err == nil {
				t.Fatal("a failed write must fail the push")
			}
			var result appPushResult
			decodeJSONInto(t, out, &result)
			if slices.Contains(result.Pushed, "lib/chart.tsx") {
				t.Errorf("the file never landed, so the report must not list it as pushed: %+v", result)
			}

			base := appStateOf(t, root).For(f.Key())
			if base == nil {
				t.Fatal("expected a baseline to be recorded after a partial push")
			}
			if base.Files["lib/chart.tsx"].SHA256 == wfdir.HashString("COLLEAGUE") {
				t.Error("the baseline acknowledged the colleague's content for a file this push never wrote — the next push would overwrite it in silence")
			}

			// The consequence, pinned end to end: with the failure lifted, the retry
			// WITHOUT --force must still see the colleague's change and refuse.
			f.failPut = map[string]int{}
			if _, err := runCLI(t, root, "app", "push", "--json"); err == nil {
				t.Fatal("the retry must still be refused as drift — the colleague's file was never acknowledged")
			}
			if got := f.FileContents(draft.ID)["lib/chart.tsx"]; got != "COLLEAGUE" {
				t.Errorf("the colleague's file was silently overwritten, got %q", got)
			}
		})
	}
}

// TestAppPushReportsTheFileAnUncertainWriteLeftBehind pins the REPORT against
// the baseline beside it.
//
// A file the reconcile finds on the server is written into the baseline, so the
// report has to name it too. It used to be recorded and not listed, and the
// closing "the draft holds what is listed above" was then wrong about the one
// file the push was least sure of.
func TestAppPushReportsTheFileAnUncertainWriteLeftBehind(t *testing.T) {
	f := newFakeAppInstance(t)
	signInApp(t, f)
	// A 500 AFTER the row was written — the post-commit publish, which is what
	// makes the outcome genuinely uncertain.
	f.failPutAfterWrite = map[string]int{"App.tsx": 500}

	root := writeAppFolder(t, t.TempDir(), &wfdir.Manifest{Title: "App"},
		map[string]string{"App.tsx": "entry", "lib/chart.tsx": "chart"})
	m := appManifestOf(t, root)
	m.SetBinding(f.Key(), wfdir.Binding{FeatureID: "feat-1"})
	if err := wfdir.SaveManifest(root, m); err != nil {
		t.Fatal(err)
	}

	out, err := runCLI(t, root, "app", "push", "--json")
	if err == nil {
		t.Fatal("a genuine write failure must fail the push")
	}
	var result appPushResult
	decodeJSONInto(t, out, &result)

	if !slices.Contains(result.Pushed, "App.tsx") {
		t.Errorf("App.tsx landed and is in the baseline, so the report must list it: %+v", result)
	}
	base := appStateOf(t, root).For(f.Key())
	if base == nil {
		t.Fatal("expected a baseline to be recorded after a partial push")
	}
	// Report and baseline describe the same set, in both directions.
	for _, path := range result.Pushed {
		if _, ok := base.Files[path]; !ok {
			t.Errorf("the report claims %s was pushed but the baseline does not record it: %v", path, base.Files)
		}
	}
	if len(base.Files) != len(result.Pushed) {
		t.Errorf("the baseline records %v but the report lists %v", base.Files, result.Pushed)
	}
}

// TestAppPushSaysSoWhenItCannotFindOutWhatLanded covers the third outcome: the
// write was uncertain AND the read that would have settled it failed too.
//
// The baseline stays conservative — it claims nothing it did not acknowledge —
// but the report must not print the confident "the draft holds what is listed
// above" over the top of a file nobody knows the fate of. It used to.
func TestAppPushSaysSoWhenItCannotFindOutWhatLanded(t *testing.T) {
	f := newFakeAppInstance(t)
	signInApp(t, f)
	f.failPutAfterWrite = map[string]int{"App.tsx": 500}
	// And the reconciling read fails as well, so nothing is learnt.
	f.failGetFile = map[string]int{"App.tsx": 500}

	root := writeAppFolder(t, t.TempDir(), &wfdir.Manifest{Title: "App"},
		map[string]string{"App.tsx": "entry", "lib/chart.tsx": "chart"})
	m := appManifestOf(t, root)
	m.SetBinding(f.Key(), wfdir.Binding{FeatureID: "feat-1"})
	if err := wfdir.SaveManifest(root, m); err != nil {
		t.Fatal(err)
	}

	// The HUMAN report, because the sentence is the thing under test.
	out, err := runCLI(t, root, "app", "push")
	if err == nil {
		t.Fatal("a genuine write failure must fail the push")
	}
	if !strings.Contains(out, "unknown  App.tsx") {
		t.Errorf("the file whose fate is unknown must be listed as such:\n%s", out)
	}
	if !strings.Contains(out, "except the file(s) marked unknown") {
		t.Errorf("the report must not claim the draft holds what is listed above:\n%s", out)
	}

	base := appStateOf(t, root).For(f.Key())
	if base == nil {
		t.Fatal("expected a baseline to be recorded after a partial push")
	}
	if _, ok := base.Files["App.tsx"]; ok {
		t.Errorf("the baseline must not claim a file the CLI could not confirm: %v", base.Files)
	}
	if _, ok := base.Files["lib/chart.tsx"]; !ok {
		t.Errorf("the file that did land must still be recorded: %v", base.Files)
	}
}

// TestAppPushReconcilesAnUncertainDelete covers the DELETE half of the
// reconcile, where the SAME 404 answer means the opposite of what it means for a
// write: a file that is not on the server is a deletion that LANDED, where for a
// PUT it is one that missed. A branch that read the answer the other way round
// would still look right against whichever quadrant it was checked against, so
// both are staged here.
//
// rdataapp.DeleteFile removes the row and recompiles the bundle afterwards, out
// of transaction, which is what makes the outcome uncertain in the first place —
// the same reason a PUT's is.
func TestAppPushReconcilesAnUncertainDelete(t *testing.T) {
	cases := []struct {
		name string
		// stage makes the DELETE of lib/old.tsx fail. The difference between the
		// two is whether the row was removed before it did.
		stage func(*fakeAppInstance)
		// landed is what the reconcile must establish, and what the report, the
		// baseline and the server then all have to agree on.
		landed bool
	}{
		{
			name:   "the deletion landed",
			stage:  func(f *fakeAppInstance) { f.failDeleteAfterWrite = map[string]int{"lib/old.tsx": 500} },
			landed: true,
		},
		{
			name:   "the deletion missed",
			stage:  func(f *fakeAppInstance) { f.failDelete = map[string]int{"lib/old.tsx": 500} },
			landed: false,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			f := newFakeAppInstance(t)
			live := f.AddApp(&api.DataApp{ID: "data_app-1"})
			draft := f.AddDraft(live.ID, "data_app-draft-1",
				api.DataAppFile{Path: "App.tsx", Content: "entry"},
				api.DataAppFile{Path: "lib/old.tsx", Content: "old"})
			signInApp(t, f)

			// The folder no longer holds lib/old.tsx, so the push deletes it.
			root := writeAppFolder(t, t.TempDir(), &wfdir.Manifest{Title: live.Name},
				map[string]string{"App.tsx": "entry"})
			m := appManifestOf(t, root)
			m.SetBinding(f.Key(), wfdir.Binding{DataAppID: live.ID, FeatureID: "feat-1"})
			if err := wfdir.SaveManifest(root, m); err != nil {
				t.Fatal(err)
			}
			// A baseline that already describes the draft exactly, so nothing here
			// is drift and the deletion is the only thing this push does.
			state := &wfdir.State{}
			state.Set(f.Key(), baselineFromApp(draft, []api.DataAppFile{
				{Path: "App.tsx", Content: "entry"},
				{Path: "lib/old.tsx", Content: "old"},
			}))
			if err := wfdir.SaveState(root, state); err != nil {
				t.Fatal(err)
			}
			tc.stage(f)

			out, err := runCLI(t, root, "app", "push", "--json")
			if err == nil {
				t.Fatal("a failed delete must fail the push")
			}
			var result appPushResult
			decodeJSONInto(t, out, &result)
			if len(result.Uncertain) != 0 {
				t.Errorf("the reconciling read succeeded, so nothing is unknown: %+v", result.Uncertain)
			}

			_, stillThere := f.FileContents(draft.ID)["lib/old.tsx"]
			if stillThere == tc.landed {
				t.Fatalf("the fake staged the wrong half: file present = %v, deletion landed = %v", stillThere, tc.landed)
			}
			if got := slices.Contains(result.Deleted, "lib/old.tsx"); got != tc.landed {
				t.Errorf("report lists it as deleted = %v, want %v — the report has to describe what the server holds", got, tc.landed)
			}
			base := appStateOf(t, root).For(f.Key())
			if base == nil {
				t.Fatal("expected a baseline to be recorded after a partial push")
			}
			if _, recorded := base.Files["lib/old.tsx"]; recorded == tc.landed {
				t.Errorf("baseline records it = %v, want %v: %v", recorded, !tc.landed, base.Files)
			}

			// The consequence either way: the baseline describes the server, so the
			// retry is an ordinary push rather than a drift refusal about the CLI's
			// own half-finished work.
			f.failDeleteAfterWrite, f.failDelete = nil, map[string]int{}
			if _, err := runCLI(t, root, "app", "push", "--json"); err != nil {
				t.Fatalf("the retry must not be refused: %v", err)
			}
			if _, ok := f.FileContents(draft.ID)["lib/old.tsx"]; ok {
				t.Error("the retry left the deleted file on the server")
			}
		})
	}
}

// TestAppPushSyncsAccessBeforeFiles pins the ORDER of the allowlist patch.
//
// The bundle compiler validates a secret reference against allowed_secret_ids,
// so a file that reaches the server before its grant does fails to compile for a
// reason that is nowhere in the file.
func TestAppPushSyncsAccessBeforeFiles(t *testing.T) {
	f := newFakeAppInstance(t)
	live := f.AddApp(&api.DataApp{ID: "data_app-1"},
		api.DataAppFile{Path: "App.tsx", Content: "old"})
	signInApp(t, f)

	root := writeAppFolder(t, t.TempDir(), &wfdir.Manifest{Title: "Revenue explorer"},
		map[string]string{"App.tsx": "new"})
	m := appManifestOf(t, root)
	m.SetBinding(f.Key(), wfdir.Binding{DataAppID: live.ID, FeatureID: "feat-1"})
	m.SetAccess(api.DataAppAccess{AllowedSecretIDs: []string{"secret-1"}})
	if err := wfdir.SaveManifest(root, m); err != nil {
		t.Fatal(err)
	}
	// A baseline that matches the live file, so the push is not refused as drift.
	state := &wfdir.State{}
	state.Set(f.Key(), baselineFromApp(live, []api.DataAppFile{{Path: "App.tsx", Content: "old"}}))
	if err := wfdir.SaveState(root, state); err != nil {
		t.Fatal(err)
	}

	out, err := runCLI(t, root, "app", "push", "--json")
	if err != nil {
		t.Fatalf("push: %v", err)
	}
	var result appPushResult
	decodeJSONInto(t, out, &result)

	patchIdx, writeIdx := -1, -1
	for i, req := range f.Requests {
		method, path, _ := strings.Cut(req, " ")
		if method == "PUT" && !strings.Contains(path, "/files/") && patchIdx < 0 {
			patchIdx = i
		}
		if method == "PUT" && strings.Contains(path, "/files/") && writeIdx < 0 {
			writeIdx = i
		}
	}
	if patchIdx < 0 {
		t.Fatalf("expected an allowlist patch, requests were %v", f.Requests)
	}
	if writeIdx >= 0 && patchIdx > writeIdx {
		t.Errorf("the allowlist patch must precede the file writes; requests were %v", f.Requests)
	}
	// Reported per id, not counted: this is a privilege change.
	if len(result.AccessChanges) != 1 || len(result.AccessChanges[0].Added) != 1 ||
		result.AccessChanges[0].Added[0] != "secret-1" {
		t.Errorf("expected the granted secret to be named, got %+v", result.AccessChanges)
	}
}

// A folder still holding `ronja app test` output from a CLI that wrote it into
// the root is refused, is told which primitive it is being refused for, and is
// told what those files look like.
//
// The names are deliberately NOT excluded from the walk: a user's own
// report.json is their file and must sync, or be refused out loud, rather than
// disappearing under a rule about a name. So the hint arrives after the refusal
// has already fired, and points at where the artefacts go now.
func TestAppPushRefusesAppTestArtefactsAndNamesThem(t *testing.T) {
	f := newFakeAppInstance(t)
	root, _ := appTestFolder(t, f)
	if err := os.WriteFile(filepath.Join(root, "screenshot.png"), []byte("\x89PNG\x00\x00"), 0o600); err != nil {
		t.Fatal(err)
	}

	_, err := runCLI(t, root, "app", "push")
	if err == nil {
		t.Fatal("a folder holding a PNG cannot be synced")
	}
	for _, want := range []string{
		"a data app holds source code",
		"screenshot.png",
		"ronja app test",
		filepath.Join(".ronja", "test"),
	} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("the refusal should carry %q, got: %v", want, err)
		}
	}
	if strings.Contains(err.Error(), "a workflow holds") {
		t.Errorf("a data app is not a workflow, and the refusal must not say so: %v", err)
	}
}

// TestAppPushRefusesEntrypointMismatch: a data app's entrypoint is immutable, so
// a manifest naming a different one can never sync. Refused with the only remedy
// there is, rather than silently writing the author's code into a file they are
// not looking at.
func TestAppPushRefusesEntrypointMismatch(t *testing.T) {
	f := newFakeAppInstance(t)
	live := f.AddApp(&api.DataApp{ID: "data_app-1", Entrypoint: "App.tsx"},
		api.DataAppFile{Path: "App.tsx", Content: "x"})
	signInApp(t, f)

	root := writeAppFolder(t, t.TempDir(),
		&wfdir.Manifest{Title: "App", Entrypoint: "Main.tsx"},
		map[string]string{"Main.tsx": "x"})
	m := appManifestOf(t, root)
	m.SetBinding(f.Key(), wfdir.Binding{DataAppID: live.ID, FeatureID: "feat-1"})
	if err := wfdir.SaveManifest(root, m); err != nil {
		t.Fatal(err)
	}

	_, err := runCLI(t, root, "app", "push")
	if err == nil {
		t.Fatal("expected a push against a mismatched entrypoint to be refused")
	}
	if !strings.Contains(err.Error(), "cannot be changed") {
		t.Errorf("the refusal should say the entrypoint is immutable, got %v", err)
	}
}

// TestAppPushRefusesAnUnknownCapability covers the one manifest field nothing
// on the server validates.
//
// "capabilities" is a closed vocabulary that is FILTERED at token-mint time
// rather than checked on the way in, so a typo used to push green, publish
// green, show no drift, and surface weeks later as a "capability not granted"
// in one viewer's console with nothing pointing at the manifest. The refusal
// has to happen before the first request — asserted as the absence of any
// /dataapp call, since a push that refused only after creating the app would
// still have left a row behind.
func TestAppPushRefusesAnUnknownCapability(t *testing.T) {
	f := newFakeAppInstance(t)
	signInApp(t, f)

	root := writeAppFolder(t, t.TempDir(), &wfdir.Manifest{Title: "App"},
		map[string]string{"App.tsx": "x"})
	m := appManifestOf(t, root)
	m.SetBinding(f.Key(), wfdir.Binding{FeatureID: "feat-1"})
	m.SetAccess(api.DataAppAccess{
		AllowedTableIDs: []string{"table-abc"},
		Capabilities:    []string{"ai", "upload_fil"},
	})
	if err := wfdir.SaveManifest(root, m); err != nil {
		t.Fatal(err)
	}

	_, err := runCLI(t, root, "app", "push")
	if err == nil {
		t.Fatal("expected a push declaring a capability that does not exist to be refused")
	}
	if !strings.Contains(err.Error(), "upload_fil\"") {
		t.Errorf("the refusal must name the offending entry, got %v", err)
	}
	for _, capability := range declarableCapabilities {
		if !strings.Contains(err.Error(), capability) {
			t.Errorf("the refusal must name the valid capabilities (missing %q), got %v", capability, err)
		}
	}
	// The stale-CLI case, said out loud: the vocabulary is closed but not
	// frozen, and a CLI older than the server must not sound certain.
	if !strings.Contains(err.Error(), "update the CLI") {
		t.Errorf("the refusal should explain what to do if the server knows a capability this CLI does not, got %v", err)
	}
	for _, request := range f.Requests {
		if strings.Contains(request, "/dataapp") {
			t.Errorf("the refusal is local — nothing should have been sent, got requests %v", f.Requests)
			break
		}
	}
}

// The other half of the refusal: every name that IS in the vocabulary passes,
// and so does declaring none. An over-eager guard here would be worse than none
// at all — it would refuse the exact manifest `ronja app init --help` tells the
// author to write.
func TestAppPushAcceptsTheDeclarableCapabilities(t *testing.T) {
	for _, tc := range []struct {
		name         string
		capabilities []string
	}{
		{"all four", declarableCapabilities},
		{"none declared", []string{}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newFakeAppInstance(t)
			signInApp(t, f)

			root := writeAppFolder(t, t.TempDir(), &wfdir.Manifest{Title: "App"},
				map[string]string{"App.tsx": "x"})
			m := appManifestOf(t, root)
			m.SetBinding(f.Key(), wfdir.Binding{FeatureID: "feat-1"})
			m.SetAccess(api.DataAppAccess{
				AllowedTableIDs: []string{"table-abc"},
				Capabilities:    tc.capabilities,
			})
			if err := wfdir.SaveManifest(root, m); err != nil {
				t.Fatal(err)
			}

			if _, err := runCLI(t, root, "app", "push"); err != nil {
				t.Fatalf("push: %v", err)
			}
			if len(f.created) != 1 {
				t.Fatalf("expected the app to be created, got %d creates", len(f.created))
			}
			if got := f.created[0].Capabilities; !slices.Equal(got, slices.Sorted(slices.Values(tc.capabilities))) {
				t.Errorf("the create should carry the declared capabilities %v, got %v", tc.capabilities, got)
			}
		})
	}
}

// TestAppPushRefusesDrift covers the guard, and --force overriding it.
func TestAppPushRefusesDrift(t *testing.T) {
	f := newFakeAppInstance(t)
	live := f.AddApp(&api.DataApp{ID: "data_app-1"})
	f.AddDraft(live.ID, "data_app-draft-1",
		api.DataAppFile{Path: "App.tsx", Content: "changed in the web builder"})
	signInApp(t, f)

	root := writeAppFolder(t, t.TempDir(), &wfdir.Manifest{Title: "App"},
		map[string]string{"App.tsx": "mine"})
	m := appManifestOf(t, root)
	m.SetBinding(f.Key(), wfdir.Binding{DataAppID: live.ID, FeatureID: "feat-1"})
	if err := wfdir.SaveManifest(root, m); err != nil {
		t.Fatal(err)
	}
	// A baseline recording a DIFFERENT content, so the remote has moved since.
	state := &wfdir.State{}
	state.Set(f.Key(), baselineFromApp(f.apps["data_app-draft-1"],
		[]api.DataAppFile{{Path: "App.tsx", Content: "what I last synced"}}))
	if err := wfdir.SaveState(root, state); err != nil {
		t.Fatal(err)
	}

	if _, err := runCLI(t, root, "app", "push"); err == nil {
		t.Fatal("expected a push over server-side changes to be refused")
	}
	if _, err := runCLI(t, root, "app", "push", "--force"); err != nil {
		t.Fatalf("--force should push anyway: %v", err)
	}
	if got := f.FileContents("data_app-draft-1")["App.tsx"]; got != "mine" {
		t.Errorf("--force should have overwritten the draft, got %q", got)
	}
}

// TestAppPushExplainsCreateRefusals: rdataapp.Create answers a private feature
// the caller does not own with a deliberately opaque "feature not found", which
// is right for the API and wrong as the last thing a CLI says.
func TestAppPushExplainsCreateRefusals(t *testing.T) {
	cases := []struct {
		name    string
		message string
		want    string
	}{
		{"shared feature", "admin required to create a data app in a shared feature", "needs an admin"},
		{"private feature", "feature not found", "another user's private feature"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			f := newFakeAppInstance(t)
			f.failCreate = 400
			f.failCreateMessage = tc.message
			signInApp(t, f)

			root := writeAppFolder(t, t.TempDir(), &wfdir.Manifest{Title: "App"},
				map[string]string{"App.tsx": "x"})
			m := appManifestOf(t, root)
			m.SetBinding(f.Key(), wfdir.Binding{FeatureID: "feat-1"})
			if err := wfdir.SaveManifest(root, m); err != nil {
				t.Fatal(err)
			}

			_, err := runCLI(t, root, "app", "push")
			if err == nil {
				t.Fatal("expected the create refusal to fail the push")
			}
			if !strings.Contains(err.Error(), tc.want) {
				t.Errorf("expected the message to explain %q, got %v", tc.want, err)
			}
		})
	}
}

// ── Publish ─────────────────────────────────────────────────────────────────

// TestAppPublishRefusesNonCompilingDraft: committing a draft that does not
// compile would leave the live app serving its previous bundle while claiming to
// be the new code. The server refuses it; the CLI refuses it first, with the
// diagnostics.
func TestAppPublishRefusesNonCompilingDraft(t *testing.T) {
	f := newFakeAppInstance(t)
	live := f.AddApp(&api.DataApp{ID: "data_app-1"})
	f.AddDraft(live.ID, "data_app-draft-1", api.DataAppFile{Path: "App.tsx", Content: "broken"})
	f.validateFails = "App.tsx:2:0: Unexpected end of file"
	signInApp(t, f)

	root := appFolderBoundTo(t, f, live.ID, map[string]string{"App.tsx": "broken"})

	_, err := runCLI(t, root, "app", "publish")
	if err == nil {
		t.Fatal("expected publish to refuse a draft that does not compile")
	}
	if !strings.Contains(err.Error(), "does not compile") {
		t.Errorf("expected a compile refusal, got %v", err)
	}
	if len(f.committed) != 0 {
		t.Errorf("nothing should have been committed, got %v", f.committed)
	}
}

// TestAppPublishCommitsOwnFeature is the ordinary path.
func TestAppPublishCommitsOwnFeature(t *testing.T) {
	f := newFakeAppInstance(t)
	live := f.AddApp(&api.DataApp{ID: "data_app-1"})
	draft := f.AddDraft(live.ID, "data_app-draft-1", api.DataAppFile{Path: "App.tsx", Content: "x"})
	f.featureScope["feat-1"] = "private"
	signInApp(t, f)

	root := appFolderBoundTo(t, f, live.ID, map[string]string{"App.tsx": "x"})

	out, err := runCLI(t, root, "app", "publish", "--json")
	if err != nil {
		t.Fatalf("publish: %v", err)
	}
	var result appPublishResult
	decodeJSONInto(t, out, &result)
	if result.Outcome != outcomePublished {
		t.Errorf("expected outcome %q, got %q", outcomePublished, result.Outcome)
	}
	if !slices.Contains(f.committed, draft.ID) {
		t.Errorf("expected the draft to be committed, got %v", f.committed)
	}
	// Where to look at it — on the FRONTEND origin the server reported, not the
	// API origin this client was pointed at. See link_test.go.
	if want := fakeFrontendOrigin + "/apps/" + live.ID; result.AppURL != want {
		t.Errorf("a published app should report where to look at it: got %q, want %q", result.AppURL, want)
	}
}

// TestAppPublishSubmitsSharedForReview is the two-outcome honesty: a non-admin
// cannot commit onto an org-scoped feature, and reporting "published" when what
// happened was "an admin now has a review request" is the lie this vocabulary
// exists to prevent.
func TestAppPublishSubmitsSharedForReview(t *testing.T) {
	f := newFakeAppInstance(t)
	live := f.AddApp(&api.DataApp{ID: "data_app-1"})
	draft := f.AddDraft(live.ID, "data_app-draft-1", api.DataAppFile{Path: "App.tsx", Content: "x"})
	f.featureScope["feat-1"] = "organization"
	f.privilegeLevel = 50 // an ordinary user
	signInApp(t, f)

	root := appFolderBoundTo(t, f, live.ID, map[string]string{"App.tsx": "x"})

	out, err := runCLI(t, root, "app", "publish", "--json")
	if err != nil {
		t.Fatalf("publish: %v", err)
	}
	var result appPublishResult
	decodeJSONInto(t, out, &result)
	if result.Outcome != outcomeSubmittedForReview {
		t.Errorf("expected outcome %q, got %q", outcomeSubmittedForReview, result.Outcome)
	}
	if !slices.Contains(f.reviewRequested, draft.ID) {
		t.Errorf("expected a review request for %s, got %v", draft.ID, f.reviewRequested)
	}
	if len(f.committed) != 0 {
		t.Errorf("a non-admin must not commit a shared app, got %v", f.committed)
	}
}

// TestAppPublishNoRequestReviewFails: CI wants an error, not a review request.
func TestAppPublishNoRequestReviewFails(t *testing.T) {
	f := newFakeAppInstance(t)
	live := f.AddApp(&api.DataApp{ID: "data_app-1"})
	f.AddDraft(live.ID, "data_app-draft-1", api.DataAppFile{Path: "App.tsx", Content: "x"})
	f.featureScope["feat-1"] = "organization"
	f.failCommit = 400
	signInApp(t, f)

	root := appFolderBoundTo(t, f, live.ID, map[string]string{"App.tsx": "x"})

	if _, err := runCLI(t, root, "app", "publish", "--no-request-review"); err == nil {
		t.Fatal("expected --no-request-review to fail rather than submit for review")
	}
	if len(f.reviewRequested) != 0 {
		t.Errorf("--no-request-review must not submit for review, got %v", f.reviewRequested)
	}
}

// ── Validate + discard ──────────────────────────────────────────────────────

func TestAppValidateReportsDiagnostics(t *testing.T) {
	f := newFakeAppInstance(t)
	live := f.AddApp(&api.DataApp{ID: "data_app-1"})
	f.AddDraft(live.ID, "data_app-draft-1", api.DataAppFile{Path: "App.tsx", Content: "broken"})
	f.validateFails = "App.tsx:2:0: Unexpected end of file"
	signInApp(t, f)

	root := appFolderBoundTo(t, f, live.ID, map[string]string{"App.tsx": "broken"})

	out, err := runCLI(t, root, "app", "validate", "--json")
	if err == nil {
		t.Fatal("a draft that does not compile must exit non-zero")
	}
	var result appValidateResult
	decodeJSONInto(t, out, &result)
	if result.Compiles {
		t.Error("expected compiles=false")
	}
	if result.CompileError == nil || !strings.Contains(result.CompileError.Message, "Unexpected end of file") {
		t.Errorf("expected the diagnostics, got %+v", result.CompileError)
	}
}

// An app that has never been published is a PARENTLESS draft: the row the
// binding names IS the draft. Publishing it must commit that same row.
//
// Every other publish test seeds a live app with a forked draft, so none of them
// covered this — and that gap let `app publish` ship a version that reported
// "you have no open draft" for an app whose every byte was sitting right there,
// which is what the first live run of the new create flow hit.
func TestAppPublishCommitsAnUnpublishedDraftInPlace(t *testing.T) {
	f := newFakeAppInstance(t)
	// Parentless: created by a first push, never published.
	f.AddApp(&api.DataApp{ID: "data_app-new-1", Lifecycle: api.LifecycleDraft},
		api.DataAppFile{Path: "App.tsx", Content: "x"})
	f.featureScope["feat-1"] = "private"
	signInApp(t, f)

	root := appFolderBoundTo(t, f, "data_app-new-1", map[string]string{"App.tsx": "x"})

	out, err := runCLI(t, root, "app", "publish", "--json")
	if err != nil {
		t.Fatalf("publish: %v", err)
	}
	var result appPublishResult
	decodeJSONInto(t, out, &result)
	if result.Outcome != outcomePublished {
		t.Errorf("outcome = %q, want %q", result.Outcome, outcomePublished)
	}
	// The row committed must be the app itself — not a draft forked from it.
	if len(f.committed) != 1 || f.committed[0] != "data_app-new-1" {
		t.Errorf("committed = %v, want [data_app-new-1]", f.committed)
	}
	if f.checkouts != 0 {
		t.Errorf("publish made %d checkout(s); an unpublished app is already the draft", f.checkouts)
	}
	if result.DraftID != result.DataAppID {
		t.Errorf("draft id %q and app id %q must be the same row", result.DraftID, result.DataAppID)
	}
}

// A FIRST publish into a shared feature by a non-admin does not go live: the
// server raises a proposal instead, and answers 200 while doing it.
//
// Two things could go wrong here and this pins both. The routing: publish must
// NOT pre-route a parentless draft to request-review the way it does an edit
// draft, because that route refuses a parentless draft outright — that
// pre-route is exactly what left this user with no path at all. And the
// reporting: the commit succeeded, so a client that reads only the status code
// says "published" about an app still sitting in an admin's inbox.
func TestAppPublishFirstPublishIntoSharedFeatureRaisesAProposal(t *testing.T) {
	f := newFakeAppInstance(t)
	f.AddApp(&api.DataApp{ID: "data_app-new-1", Lifecycle: api.LifecycleDraft},
		api.DataAppFile{Path: "App.tsx", Content: "x"})
	f.featureScope["feat-1"] = "organization"
	f.privilegeLevel = 50 // an ordinary user
	f.commitProposes = true
	signInApp(t, f)

	root := appFolderBoundTo(t, f, "data_app-new-1", map[string]string{"App.tsx": "x"})

	out, err := runCLI(t, root, "app", "publish", "--json")
	if err != nil {
		t.Fatalf("publish: %v", err)
	}
	var result appPublishResult
	decodeJSONInto(t, out, &result)
	if result.Outcome != outcomeSubmittedForReview {
		t.Errorf("outcome = %q, want %q — the app is not live, it is waiting on an admin",
			result.Outcome, outcomeSubmittedForReview)
	}
	if result.AppURL != "" {
		t.Errorf("a proposal has nothing to look at yet, got URL %q", result.AppURL)
	}
	// The commit is the ONLY call that may happen: pre-routing to
	// request-review is the dead end this replaced.
	if len(f.committed) != 1 || f.committed[0] != "data_app-new-1" {
		t.Errorf("committed = %v, want [data_app-new-1]", f.committed)
	}
	if len(f.reviewRequested) != 0 {
		t.Errorf("a never-published app has no review route; request-review was called with %v", f.reviewRequested)
	}
}

// Discarding an unpublished app would delete the app, since there is no parent
// behind it. `discard` reads as "undo my edits", so it refuses rather than
// doing that quietly — the same refusal `wf discard` makes.
func TestAppDiscardRefusesAnUnpublishedDraft(t *testing.T) {
	f := newFakeAppInstance(t)
	f.AddApp(&api.DataApp{ID: "data_app-new-1", Lifecycle: api.LifecycleDraft},
		api.DataAppFile{Path: "App.tsx", Content: "x"})
	signInApp(t, f)

	root := appFolderBoundTo(t, f, "data_app-new-1", map[string]string{"App.tsx": "x"})

	_, err := runCLI(t, root, "app", "discard", "--yes")
	if err == nil {
		t.Fatal("expected discard to refuse an app that has never been published")
	}
	if !strings.Contains(err.Error(), "never been published") {
		t.Errorf("the refusal should say the app was never published, got %v", err)
	}
	// The refusal has to name the way OUT, or the caller's only remaining move
	// is the web UI — which a terminal-only caller does not have.
	if !strings.Contains(err.Error(), "--delete-app") {
		t.Errorf("the refusal should point at --delete-app, got %v", err)
	}
	if len(f.discarded) != 0 {
		t.Errorf("nothing should have been discarded, got %v", f.discarded)
	}
	if len(f.deletedApps) != 0 {
		t.Errorf("nothing should have been deleted without --delete-app, got %v", f.deletedApps)
	}
}

// --delete-app is the escape hatch for an abandoned first push: the app is
// deleted, local files survive, and the folder is unbound so the next push
// creates a fresh one in the same feature.
func TestAppDiscardDeleteAppRemovesAnUnpublishedApp(t *testing.T) {
	f := newFakeAppInstance(t)
	f.AddApp(&api.DataApp{ID: "data_app-new-1", Lifecycle: api.LifecycleDraft},
		api.DataAppFile{Path: "App.tsx", Content: "x"})
	signInApp(t, f)

	root := appFolderBoundTo(t, f, "data_app-new-1", map[string]string{"App.tsx": "x"})

	out, err := runCLI(t, root, "app", "discard", "--delete-app", "--yes", "--json")
	if err != nil {
		t.Fatalf("discard --delete-app: %v", err)
	}
	var result appDiscardResult
	decodeJSONInto(t, out, &result)
	if result.Outcome != outcomeResourceDeleted {
		t.Errorf("outcome = %q, want %q", result.Outcome, outcomeResourceDeleted)
	}
	if !slices.Contains(f.deletedApps, "data_app-new-1") {
		t.Errorf("the app should have been deleted, got %v", f.deletedApps)
	}

	// Local source is the author's and must survive — the whole point is that
	// the folder is reusable after a failed first push.
	if got := readFile(t, root, "App.tsx"); got != "x" {
		t.Errorf("local App.tsx should be untouched, got %q", got)
	}

	// Unbound from the app but still pointing at the feature, exactly as `init`
	// left it: the next push must create rather than look for a row that is gone.
	manifest, err := wfdir.LoadManifest(root, wfdir.DataAppKind)
	if err != nil {
		t.Fatalf("load manifest: %v", err)
	}
	binding, bound, err := manifest.Binding(f.Key())
	if err != nil {
		t.Fatalf("binding: %v", err)
	}
	if bound && binding.DataAppID != "" {
		t.Errorf("the folder should no longer name a data app, got %q", binding.DataAppID)
	}
	if bound && binding.FeatureID == "" {
		t.Error("the feature binding should survive so the next push recreates in place")
	}
	// The baseline has to go too: it described a row that no longer exists, and
	// leaving it would make the next push diff against a ghost.
	if base := appStateOf(t, root).For(f.Key()); base != nil && base.SourceID != "" {
		t.Errorf("baseline should be cleared, still names %q", base.SourceID)
	}
}

// --delete-app on a LIVE app is a caller error, not a licence to delete a
// published app. Refusing beats ignoring: the flag reads like it would.
func TestAppDiscardDeleteAppRefusesALiveApp(t *testing.T) {
	f := newFakeAppInstance(t)
	live := f.AddApp(&api.DataApp{ID: "data_app-1"}, api.DataAppFile{Path: "App.tsx", Content: "live"})
	f.AddDraft(live.ID, "data_app-draft-1", api.DataAppFile{Path: "App.tsx", Content: "draft"})
	signInApp(t, f)

	root := appFolderBoundTo(t, f, live.ID, map[string]string{"App.tsx": "draft"})

	_, err := runCLI(t, root, "app", "discard", "--delete-app", "--yes")
	if err == nil {
		t.Fatal("expected --delete-app to be refused on a live app")
	}
	if !strings.Contains(err.Error(), "never been published") {
		t.Errorf("the refusal should explain the flag's scope, got %v", err)
	}
	if len(f.deletedApps) != 0 {
		t.Errorf("a live app must never be deleted, got %v", f.deletedApps)
	}
	if len(f.discarded) != 0 {
		t.Errorf("the draft should not have been discarded either, got %v", f.discarded)
	}
}

func TestAppDiscardDropsDraftAndResetsBaseline(t *testing.T) {
	f := newFakeAppInstance(t)
	live := f.AddApp(&api.DataApp{ID: "data_app-1"}, api.DataAppFile{Path: "App.tsx", Content: "live"})
	draft := f.AddDraft(live.ID, "data_app-draft-1", api.DataAppFile{Path: "App.tsx", Content: "mine"})
	signInApp(t, f)

	root := appFolderBoundTo(t, f, live.ID, map[string]string{"App.tsx": "mine"})

	if _, err := runCLI(t, root, "app", "discard", "--yes", "--json"); err != nil {
		t.Fatalf("discard: %v", err)
	}
	if !slices.Contains(f.discarded, draft.ID) {
		t.Errorf("expected the draft to be discarded, got %v", f.discarded)
	}
	// The baseline must now describe the LIVE row: the draft it was taken from is
	// gone, and leaving it would make every local file read as drift against a
	// row that no longer exists.
	base := appStateOf(t, root).For(f.Key())
	if base == nil || base.SourceID != live.ID {
		t.Errorf("baseline should point at the live app %s, got %+v", live.ID, base)
	}
	// Local files are untouched.
	if got := readFile(t, root, "App.tsx"); got != "mine" {
		t.Errorf("discard must not touch local files, got %q", got)
	}
}

// A folder can be bound to several instances at once — localhost, staging and
// production is the shape this whole per-(instance, organization) keying exists
// for — and each keeps its OWN baseline. Deleting an abandoned draft on one of
// them must not touch the others'.
//
// The failure this guards is not cosmetic: a wiped baseline is indistinguishable
// from "never synced here", so the next push against a non-empty remote trips
// the drift guard and the way out it names is --force. Throwing away a staging
// draft would end with production overwritten without comparison.
func TestAppDeleteAppKeepsOtherInstancesBaselines(t *testing.T) {
	f := newFakeAppInstance(t)
	f.AddApp(&api.DataApp{ID: "data_app-new-1", Lifecycle: api.LifecycleDraft},
		api.DataAppFile{Path: "App.tsx", Content: "x"})
	signInApp(t, f)

	root := appFolderBoundTo(t, f, "data_app-new-1", map[string]string{"App.tsx": "x"})

	// Two baselines: this instance's abandoned draft, and a production one the
	// command has no business touching.
	other := wfdir.InstanceKey{URL: "https://prod.example.com", TenantID: "ten-prod"}
	state := appStateOf(t, root)
	state.Set(f.Key(), &wfdir.InstanceState{
		SourceID: "data_app-new-1", SourceLifecycle: api.LifecycleDraft,
		Files: map[string]wfdir.FileState{"App.tsx": {SHA256: wfdir.HashString("x")}},
	})
	state.Set(other, &wfdir.InstanceState{
		SourceID: "data_app-prod-9", SourceLifecycle: api.LifecycleLive,
		Title: "Revenue explorer", Entrypoint: "App.tsx",
		Files: map[string]wfdir.FileState{"App.tsx": {SHA256: wfdir.HashString("shipped"), UpdatedAt: "2026-08-01T09:00:00Z"}},
	})
	if err := wfdir.SaveState(root, state); err != nil {
		t.Fatalf("write state: %v", err)
	}
	before := marshalJSON(t, state.For(other))

	if _, err := runCLI(t, root, "app", "discard", "--delete-app", "--yes", "--json"); err != nil {
		t.Fatalf("discard --delete-app: %v", err)
	}

	after := appStateOf(t, root)
	if got := marshalJSON(t, after.For(other)); got != before {
		t.Errorf("production's baseline was rewritten by a discard against another instance:\n  before %s\n  after  %s", before, got)
	}
	// And this instance's is gone, so the folder reads as freshly initialised
	// here — every local file added, nothing to drift against.
	if base := after.For(f.Key()); base != nil {
		t.Errorf("the discarded instance's baseline should be gone, got %+v", base)
	}
}

// A binding that names an EDIT SHADOW rather than the app is a hand-edited
// ronja.json, and --delete-app must not read it as "never published" — that
// would delete somebody's in-progress edits and unbind the folder from the live
// app, while reporting that nothing was ever published.
func TestAppDeleteAppRefusesADraftOfALiveApp(t *testing.T) {
	f := newFakeAppInstance(t)
	live := f.AddApp(&api.DataApp{ID: "data_app-1"}, api.DataAppFile{Path: "App.tsx", Content: "live"})
	shadow := f.AddDraft(live.ID, "data_app-draft-1", api.DataAppFile{Path: "App.tsx", Content: "mine"})
	signInApp(t, f)

	// Bound to the DRAFT's id, which no push writes but a hand edit can.
	root := appFolderBoundTo(t, f, shadow.ID, map[string]string{"App.tsx": "mine"})

	_, err := runCLI(t, root, "app", "discard", "--delete-app", "--yes")
	if err == nil {
		t.Fatal("expected a binding naming an edit shadow to be refused")
	}
	// The way out is the parent id, so the error has to name it.
	if !strings.Contains(err.Error(), live.ID) {
		t.Errorf("the refusal should name the app the draft belongs to, got %v", err)
	}
	if len(f.deletedApps) != 0 {
		t.Errorf("nothing should have been deleted, got %v", f.deletedApps)
	}
	if binding, _, _ := appManifestOf(t, root).Binding(f.Key()); binding.DataAppID != shadow.ID {
		t.Errorf("the binding should be untouched, got %q", binding.DataAppID)
	}
}

// ── Kind gating ─────────────────────────────────────────────────────────────

// TestAppCommandsRefuseWorkflowFolder: the two folder types are indistinguishable
// by shape, so without the kind gate `ronja app push` inside a workflow folder
// would overwrite an app's source with Python — or, here, try to.
func TestAppCommandsRefuseWorkflowFolder(t *testing.T) {
	f := newFakeAppInstance(t)
	signInApp(t, f)

	root := t.TempDir()
	m := &wfdir.Manifest{Kind: wfdir.KindWorkflow, Title: "A workflow", Entrypoint: "main.py"}
	if err := wfdir.SaveManifest(root, m); err != nil {
		t.Fatal(err)
	}

	_, err := runCLI(t, root, "app", "status")
	if err == nil {
		t.Fatal("expected `app status` to refuse a workflow folder")
	}
	// Names the command that WOULD work: a wrong kind is almost always the right
	// folder and the wrong verb.
	if !strings.Contains(err.Error(), "ronja wf") {
		t.Errorf("the refusal should point at `ronja wf`, got %v", err)
	}
}

// appFolderBoundTo lays out a folder already bound to a live app, with a
// baseline matching its draft — the ordinary "I have pushed before" state.
func appFolderBoundTo(t *testing.T, f *fakeAppInstance, liveID string, files map[string]string) string {
	t.Helper()
	root := writeAppFolder(t, t.TempDir(), &wfdir.Manifest{Title: "Revenue explorer"}, files)
	m := appManifestOf(t, root)
	m.SetBinding(f.Key(), wfdir.Binding{DataAppID: liveID, FeatureID: "feat-1"})
	if err := wfdir.SaveManifest(root, m); err != nil {
		t.Fatal(err)
	}
	draftID, ok := f.draftOf[liveID]
	if !ok {
		return root
	}
	var baseline []api.DataAppFile
	for path, content := range files {
		baseline = append(baseline, api.DataAppFile{Path: path, Content: content})
	}
	state := &wfdir.State{}
	state.Set(f.Key(), baselineFromApp(f.apps[draftID], baseline))
	if err := wfdir.SaveState(root, state); err != nil {
		t.Fatal(err)
	}
	return root
}

// The compile message the server sends is dataappbundle.CompileError.Error(),
// which locates every diagnostic with esbuild's own namespace prefix. That
// prefix names an internal bundler concept, so the author is handed
// `ronja-app:components/Today.tsx:37:38` for a file they know as
// `components/Today.tsx` — and the identifier check made this the common case
// rather than the rare one, because it reports a location for every hit.
func TestStripBundleNamespace(t *testing.T) {
	tests := []struct {
		name string
		in   string
		want string
	}{
		{
			name: "one located diagnostic",
			in:   `data app compile failed: ronja-app:components/Today.tsx:37:38: "useState" is not defined — it is neither imported nor declared. Import it from "react": import { useState } from "react"`,
			want: `data app compile failed: components/Today.tsx:37:38: "useState" is not defined — it is neither imported nor declared. Import it from "react": import { useState } from "react"`,
		},
		{
			name: "every occurrence, not just the first",
			in:   "data app compile failed: ronja-app:App.tsx:1:0: a; ronja-app:lib/queries.ts:9:4: b",
			want: "data app compile failed: App.tsx:1:0: a; lib/queries.ts:9:4: b",
		},
		{
			// The vendored SDK's namespace is the one thing that tells an author
			// the diagnostic is not about a file they can edit.
			name: "vendor namespace is preserved",
			in:   "data app compile failed: ronja-vendor:app/index.js:12:3: boom",
			want: "data app compile failed: ronja-vendor:app/index.js:12:3: boom",
		},
		{
			// Anchored on `:line:col:`, so prose that merely mentions the
			// namespace is left as the server wrote it.
			name: "prose mentioning the namespace is untouched",
			in:   "the ronja-app: namespace is an esbuild detail",
			want: "the ronja-app: namespace is an esbuild detail",
		},
		{
			name: "a message with no location at all",
			in:   "data app compile failed",
			want: "data app compile failed",
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := stripBundleNamespace(tc.in); got != tc.want {
				t.Errorf("stripBundleNamespace()\n got: %s\nwant: %s", got, tc.want)
			}
		})
	}
}

// `ronja app test` renders a diagnostic structurally, so its path arrives as
// its own field and the message regex has nothing to anchor on. Same two rules
// as above, asserted separately because the code is separate.
func TestStripBundleNamespacePath(t *testing.T) {
	tests := []struct {
		name string
		in   string
		want string
	}{
		{"app namespace comes off", "ronja-app:components/Today.tsx", "components/Today.tsx"},
		{"vendor namespace is preserved", "ronja-vendor:app/index.js", "ronja-vendor:app/index.js"},
		{"a bare path is untouched", "App.tsx", "App.tsx"},
		{"only a leading prefix counts", "src/ronja-app:x.tsx", "src/ronja-app:x.tsx"},
		{"an empty path stays empty", "", ""},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := stripBundleNamespacePath(tc.in); got != tc.want {
				t.Errorf("stripBundleNamespacePath(%q)\n got: %s\nwant: %s", tc.in, got, tc.want)
			}
		})
	}
}

// The end-to-end half for the test report, matching
// TestAppValidateReportDropsTheBundleNamespace below: a unit test on the helper
// would still pass if formatCompileDiagnostic stopped calling it.
func TestAppTestReportDropsTheBundleNamespace(t *testing.T) {
	got := formatCompileDiagnostic(api.CompileDiagnostic{
		File:    "ronja-app:components/Today.tsx",
		Line:    37,
		Column:  38,
		Message: `"useState" is not defined`,
	})
	if strings.Contains(got, "ronja-app:") {
		t.Errorf("the build-error line still names the bundler namespace: %s", got)
	}
	if !strings.HasPrefix(got, "components/Today.tsx:37:38") {
		t.Errorf("want the author's own path and position, got: %s", got)
	}
}

// The end-to-end half: the string really does reach stdout through the report,
// and it really is stripped there. A unit test on the helper alone would still
// pass if nothing called it.
func TestAppValidateReportDropsTheBundleNamespace(t *testing.T) {
	f := newFakeAppInstance(t)
	live := &api.DataApp{ID: "data_app-live-1", Lifecycle: api.LifecycleLive}
	f.AddApp(live, api.DataAppFile{Path: "App.tsx", Content: "x"})
	f.AddDraft(live.ID, "data_app-draft-1", api.DataAppFile{Path: "App.tsx", Content: "x"})
	f.validateFails = `data app compile failed: ronja-app:components/Today.tsx:37:38: "useState" is not defined — it is neither imported nor declared. Import it from "react": import { useState } from "react"`
	signInApp(t, f)

	root := appFolderBoundTo(t, f, live.ID, map[string]string{"App.tsx": "x"})

	out, err := runCLI(t, root, "app", "validate")
	if err == nil {
		t.Fatal("a draft that does not compile must exit non-zero")
	}
	if strings.Contains(out, "ronja-app:") {
		t.Errorf("the report still names the bundler's namespace:\n%s", out)
	}
	if !strings.Contains(out, "components/Today.tsx:37:38") {
		t.Errorf("the report lost the file location:\n%s", out)
	}
	if !strings.Contains(out, `"useState" is not defined`) {
		t.Errorf("the report lost the diagnostic:\n%s", out)
	}
}
