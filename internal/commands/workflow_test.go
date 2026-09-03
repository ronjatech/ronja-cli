package commands

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"github.com/ronjatech/ronja-cli/internal/api"
	"github.com/ronjatech/ronja-cli/internal/config"
	"github.com/ronjatech/ronja-cli/internal/wfdir"
)

// --- clone -----------------------------------------------------------------

func TestCloneWritesFolderManifestAndBaseline(t *testing.T) {
	f := newFakeInstance(t)
	f.AddWorkflow(
		&api.Workflow{ID: "wf-1", Lifecycle: api.LifecycleLive, Title: "Monthly report"},
		api.WorkflowFile{Path: "main.py", Content: "print('hi')\n"},
		api.WorkflowFile{Path: "lib/helpers.py", Content: "X = 1\n"},
	)
	signIn(t, f)
	parent := t.TempDir()

	out, err := runCLI(t, parent, "wf", "clone", "wf-1", "--json")
	if err != nil {
		t.Fatalf("clone: %v", err)
	}
	payload := decodeJSON(t, out)

	root := payload["root"].(string)
	if filepath.Base(root) != "monthly-report" {
		t.Errorf("default directory = %s, want a slug of the title", filepath.Base(root))
	}
	if payload["files"].(float64) != 2 {
		t.Errorf("files = %v, want 2", payload["files"])
	}
	if payload["clonedFromDraft"].(bool) {
		t.Error("clonedFromDraft is true for a workflow with no draft")
	}

	// The manifest records the STABLE workflow id, keyed by the resolved host.
	manifest, err := wfdir.LoadManifest(root, wfdir.WorkflowKind)
	if err != nil {
		t.Fatalf("load manifest: %v", err)
	}
	binding, bound, _ := manifest.Binding(f.Key())
	if !bound || binding.WorkflowID != "wf-1" || binding.FeatureID != "feat-1" {
		t.Errorf("binding = %+v (bound %v), want wf-1/feat-1", binding, bound)
	}
	if manifest.Title != "Monthly report" || manifest.Entrypoint != "main.py" {
		t.Errorf("manifest = %+v", manifest)
	}

	// The files themselves, including the nested one.
	if got := readFile(t, root, "main.py"); got != "print('hi')\n" {
		t.Errorf("main.py = %q", got)
	}
	if got := readFile(t, root, "lib", "helpers.py"); got != "X = 1\n" {
		t.Errorf("lib/helpers.py = %q", got)
	}

	assertBaselineMatchesDisk(t, root, f.Key())

	// .ronja/ excludes itself, so a colleague never inherits this checkout's
	// per-user baseline through git.
	if got := readFile(t, root, ".ronja", ".gitignore"); strings.TrimSpace(got) != "*" {
		t.Errorf(".ronja/.gitignore = %q, want \"*\"", got)
	}
}

// The critical case: a path the folder cannot hold must stop the clone, not be
// skipped. A skipped file still got a baseline entry, so status reported it as
// locally deleted and a push would have deleted it server-side.
func TestCloneRefusesUnwritablePath(t *testing.T) {
	f := newFakeInstance(t)
	f.AddWorkflow(
		&api.Workflow{ID: "wf-1", Lifecycle: api.LifecycleLive},
		api.WorkflowFile{Path: "main.py", Content: "ok\n"},
		api.WorkflowFile{Path: "../escape.py", Content: "bad\n"},
	)
	signIn(t, f)
	parent := t.TempDir()

	_, err := runCLI(t, parent, "wf", "clone", "wf-1", "dest")
	if err == nil {
		t.Fatal("clone succeeded with an unwritable path")
	}
	if !strings.Contains(err.Error(), "../escape.py") {
		t.Errorf("error does not name the offending path: %v", err)
	}
	assertNothingWritten(t, filepath.Join(parent, "dest"))
}

// The server's path grammar allows `.` in a segment, so `.env` is a perfectly
// legal workflow file path — and one the folder walk will never return. Cloning
// it would write the file, record it in the baseline, and then never see it
// again: `wf status` invents a deletion and `wf push` performs one, server-side,
// on a file the user never touched. Refused for the same reason a case
// collision is: a folder whose baseline is wrong from birth is not recoverable
// by anything the CLI does next.
func TestCloneRefusesAPathTheWalkCanNeverReturn(t *testing.T) {
	for _, path := range []string{".env", ".config/settings.py", "ronja.json"} {
		t.Run(path, func(t *testing.T) {
			f := newFakeInstance(t)
			f.AddWorkflow(
				&api.Workflow{ID: "wf-1", Lifecycle: api.LifecycleLive},
				api.WorkflowFile{Path: "main.py", Content: "ok\n"},
				api.WorkflowFile{Path: path, Content: "hidden\n"},
			)
			signIn(t, f)
			parent := t.TempDir()

			_, err := runCLI(t, parent, "wf", "clone", "wf-1", "dest")
			if err == nil {
				t.Fatal("clone succeeded with a path the walk can never return")
			}
			if !strings.Contains(err.Error(), path) {
				t.Errorf("error does not name the offending path: %v", err)
			}
			assertNothingWritten(t, filepath.Join(parent, "dest"))
		})
	}
}

// Two rows differing only by case are ONE file on macOS and Windows: the second
// write destroys the first, and the baseline then claims both are on disk.
func TestCloneRefusesCaseCollision(t *testing.T) {
	f := newFakeInstance(t)
	f.AddWorkflow(
		&api.Workflow{ID: "wf-1", Lifecycle: api.LifecycleLive},
		api.WorkflowFile{Path: "Main.py", Content: "first\n"},
		api.WorkflowFile{Path: "main.py", Content: "second\n"},
	)
	signIn(t, f)
	parent := t.TempDir()

	_, err := runCLI(t, parent, "wf", "clone", "wf-1", "dest")
	if err == nil {
		t.Fatal("clone succeeded with a case collision")
	}
	if !strings.Contains(err.Error(), "Main.py") || !strings.Contains(err.Error(), "main.py") {
		t.Errorf("error does not name both paths: %v", err)
	}
	// The check is unconditional, so a Linux CI run refuses the same set a Mac
	// would — a folder that only breaks on someone else's laptop is worse.
	assertNothingWritten(t, filepath.Join(parent, "dest"))
}

func TestCloneRefusesLifecyclesItCannotWorkWith(t *testing.T) {
	cases := []struct {
		lifecycle string
		wantIn    string
	}{
		{api.LifecycleVersion, "version snapshot"},
		{api.LifecycleArchived, "archived"},
		{api.LifecycleProposed, "proposal"},
	}
	for _, tc := range cases {
		t.Run(tc.lifecycle, func(t *testing.T) {
			f := newFakeInstance(t)
			f.AddWorkflow(&api.Workflow{ID: "wf-1", Lifecycle: tc.lifecycle})
			signIn(t, f)
			parent := t.TempDir()

			_, err := runCLI(t, parent, "wf", "clone", "wf-1", "dest")
			if err == nil || !strings.Contains(err.Error(), tc.wantIn) {
				t.Errorf("error = %v, want one naming %q", err, tc.wantIn)
			}
			assertNothingWritten(t, filepath.Join(parent, "dest"))
		})
	}
}

// Cloning live over an open draft would look like the user's web-builder edits
// had vanished, so the draft's files win — while the manifest still records the
// stable id, because a draft's id dies at commit and the manifest is committed.
func TestClonePrefersOpenDraft(t *testing.T) {
	f := newFakeInstance(t)
	f.AddWorkflow(
		&api.Workflow{ID: "wf-1", Lifecycle: api.LifecycleLive},
		api.WorkflowFile{Path: "main.py", Content: "live\n"},
	)
	f.AddDraft("wf-1", "wf-1-draft",
		api.WorkflowFile{Path: "main.py", Content: "draft\n"},
		api.WorkflowFile{Path: "extra.py", Content: "new in the draft\n"},
	)
	signIn(t, f)
	parent := t.TempDir()

	out, err := runCLI(t, parent, "wf", "clone", "wf-1", "dest", "--json")
	if err != nil {
		t.Fatalf("clone: %v", err)
	}
	payload := decodeJSON(t, out)
	if !payload["clonedFromDraft"].(bool) {
		t.Error("clonedFromDraft = false, want true")
	}
	if payload["sourceID"] != "wf-1-draft" {
		t.Errorf("sourceID = %v, want the draft", payload["sourceID"])
	}
	// The BINDING is the stable id even though the files came from the draft.
	if payload["workflowID"] != "wf-1" {
		t.Errorf("workflowID = %v, want the stable live id", payload["workflowID"])
	}

	root := filepath.Join(parent, "dest")
	if got := readFile(t, root, "main.py"); got != "draft\n" {
		t.Errorf("main.py = %q, want the draft's content", got)
	}
	if got := readFile(t, root, "extra.py"); got != "new in the draft\n" {
		t.Errorf("extra.py = %q", got)
	}
	assertBaselineMatchesDisk(t, root, f.Key())

	// The baseline names the row the files came from, not the binding.
	state, err := wfdir.LoadState(root)
	if err != nil {
		t.Fatalf("load state: %v", err)
	}
	if base := state.For(f.Key()); base == nil || base.SourceID != "wf-1-draft" {
		t.Errorf("baseline source = %+v, want wf-1-draft", base)
	}
}

// A non-empty destination is not made truer by three round trips, and CI names
// its directory explicitly — so the refusal must precede every request.
func TestCloneChecksNamedTargetBeforeAnyRequest(t *testing.T) {
	f := newFakeInstance(t)
	f.AddWorkflow(&api.Workflow{ID: "wf-1", Lifecycle: api.LifecycleLive},
		api.WorkflowFile{Path: "main.py", Content: "x\n"})
	signIn(t, f)
	parent := t.TempDir()

	occupied := filepath.Join(parent, "dest")
	if err := os.MkdirAll(occupied, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(occupied, "something.txt"), []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}

	_, err := runCLI(t, parent, "wf", "clone", "wf-1", "dest")
	if err == nil || !strings.Contains(err.Error(), "not empty") {
		t.Fatalf("error = %v, want a not-empty refusal", err)
	}
	if len(f.Requests) != 0 {
		t.Errorf("made %d request(s) before rejecting the target: %v", len(f.Requests), f.Requests)
	}
}

// --- init ------------------------------------------------------------------

// MarkFlagRequired only asserts the flag was PASSED, so the empty value has to
// be caught in RunE or the folder is bound to no feature at all.
func TestInitRejectsEmptyFeature(t *testing.T) {
	f := newFakeInstance(t)
	signIn(t, f)

	for _, value := range []string{"", "   "} {
		dir := t.TempDir()
		_, err := runCLI(t, dir, "wf", "init", "--feature", value)
		if err == nil {
			t.Fatalf("init accepted --feature %q", value)
		}
		if !strings.Contains(err.Error(), "--feature") {
			t.Errorf("error does not mention the flag: %v", err)
		}
		if _, statErr := os.Stat(filepath.Join(dir, wfdir.ManifestName)); !os.IsNotExist(statErr) {
			t.Error("a manifest was written despite the refusal")
		}
	}
}

// init and status describe the same folder, so `bound` cannot mean two things.
// It carries status's meaning — "there is an entry for this instance" — and
// `created` carries the one init used to overload it with.
func TestInitAndStatusAgreeOnBound(t *testing.T) {
	f := newFakeInstance(t)
	signIn(t, f)
	dir := t.TempDir()

	out, err := runCLI(t, dir, "wf", "init", "--feature", "feat-1", "--json")
	if err != nil {
		t.Fatalf("init: %v", err)
	}
	initPayload := decodeJSON(t, out)
	if initPayload["bound"] != true {
		t.Errorf("init bound = %v, want true (the folder has an entry for this instance)", initPayload["bound"])
	}
	if initPayload["created"] != false {
		t.Errorf("init created = %v, want false (nothing exists server-side)", initPayload["created"])
	}

	out, err = runCLI(t, dir, "wf", "status", "--json")
	if err != nil {
		t.Fatalf("status: %v", err)
	}
	statusPayload := decodeJSON(t, out)
	if statusPayload["bound"] != initPayload["bound"] {
		t.Errorf("status bound = %v, init bound = %v — the two commands disagree about one folder",
			statusPayload["bound"], initPayload["bound"])
	}
	// No workflow id on either side: the first push is what creates one.
	if _, present := statusPayload["workflowID"]; present {
		t.Errorf("status reports a workflowID before the first push: %v", statusPayload["workflowID"])
	}
	remote := statusPayload["remote"].(map[string]any)
	if !strings.Contains(remote["notCheckedReason"].(string), "no workflow exists") {
		t.Errorf("notCheckedReason = %v", remote["notCheckedReason"])
	}
}

func TestInitDerivesTitleFromNonASCIIFilename(t *testing.T) {
	// Byte-slicing base[:1] splits the "ö" and produces U+FFFD.
	if got := deriveTitle("scripts/över_allt.py", "/tmp/x"); got != "Över allt" {
		t.Errorf("deriveTitle = %q, want %q", got, "Över allt")
	}
	if got := deriveTitle("", "/tmp/månads-rapport"); got != "Månads rapport" {
		t.Errorf("deriveTitle = %q, want %q", got, "Månads rapport")
	}
	if got := deriveTitle("scripts/monthly_report.py", "/tmp/x"); got != "Monthly report" {
		t.Errorf("deriveTitle = %q, want %q", got, "Monthly report")
	}
}

func TestInitCopiesEntrypointFromSource(t *testing.T) {
	f := newFakeInstance(t)
	signIn(t, f)
	dir := t.TempDir()
	src := filepath.Join(dir, "scripts", "report.py")
	if err := os.MkdirAll(filepath.Dir(src), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(src, []byte("print(1)\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	out, err := runCLI(t, dir, "wf", "init", "--from", "scripts/report.py", "--feature", "feat-1", "--json")
	if err != nil {
		t.Fatalf("init: %v", err)
	}
	payload := decodeJSON(t, out)
	if payload["title"] != "Report" {
		t.Errorf("title = %v, want Report", payload["title"])
	}
	if got := readFile(t, dir, "main.py"); got != "print(1)\n" {
		t.Errorf("main.py = %q", got)
	}
	// Copied, never moved: --from may be imported by something else.
	if got := readFile(t, src); got != "print(1)\n" {
		t.Errorf("source was not left in place: %q", got)
	}
	// And it is inside the folder, so it would be pushed too — say so.
	warnings, ok := payload["warnings"].([]any)
	if !ok || len(warnings) == 0 || !strings.Contains(warnings[0].(string), "scripts/report.py") {
		t.Errorf("warnings = %v, want one naming the duplicated source", payload["warnings"])
	}
}

// Re-running init after it failed on something later — a bad --feature, an
// unwritable manifest — must not be blocked by the file the first attempt
// already copied. A byte-identical destination is not a conflict, it is the same
// copy; only DIFFERENT content is the file worth refusing to eat.
func TestInitFromAcceptsAnIdenticalDestinationAndRefusesADifferentOne(t *testing.T) {
	f := newFakeInstance(t)
	signIn(t, f)

	dir := t.TempDir()
	writeLocal(t, dir, "scripts/report.py", "print(1)\n")
	writeLocal(t, dir, "main.py", "print(1)\n")
	if _, err := runCLI(t, dir, "wf", "init", "--from", "scripts/report.py", "--feature", "feat-1", "--json"); err != nil {
		t.Fatalf("init refused a destination it would have written verbatim: %v", err)
	}
	if got := readFile(t, dir, "main.py"); got != "print(1)\n" {
		t.Errorf("main.py = %q", got)
	}

	other := t.TempDir()
	writeLocal(t, other, "scripts/report.py", "print(1)\n")
	writeLocal(t, other, "main.py", "print(2)\n")
	if _, err := runCLI(t, other, "wf", "init", "--from", "scripts/report.py", "--feature", "feat-1", "--json"); err == nil {
		t.Fatal("init overwrote a main.py that was not the source")
	}
	if got := readFile(t, other, "main.py"); got != "print(2)\n" {
		t.Errorf("main.py = %q — the refused init wrote anyway", got)
	}
}

// The runtime is stamped when the first push CREATES the workflow and cannot be
// changed afterwards, so a value the instance cannot act on has to be refused
// at the flag rather than discovered later as a create-body rejection.
func TestInitRejectsAnUnknownRuntime(t *testing.T) {
	f := newFakeInstance(t)
	signIn(t, f)

	// 3 is a real runtime now; 4 is the next one this build has not heard of.
	for _, value := range []string{"0", "4", "-1"} {
		dir := t.TempDir()
		_, err := runCLI(t, dir, "wf", "init", "--feature", "feat-1", "--runtime", value)
		if err == nil {
			t.Fatalf("init accepted --runtime %s", value)
		}
		if !strings.Contains(err.Error(), "--runtime") {
			t.Errorf("error does not mention the flag: %v", err)
		}
		if _, statErr := os.Stat(filepath.Join(dir, wfdir.ManifestName)); !os.IsNotExist(statErr) {
			t.Error("a manifest was written despite the refusal")
		}
	}
}

// A runtime the CLI DOES know is accepted, writes the key, and scaffolds — the
// positive half of the refusal above. Runtime 3 is durable (a superset of 2), so
// it gets the same starter rather than an empty file.
func TestInitAcceptsTheQueryRuntime(t *testing.T) {
	f := newFakeInstance(t)
	signIn(t, f)
	dir := t.TempDir()

	out, err := runCLI(t, dir, "wf", "init", "--feature", "feat-1", "--runtime",
		strconv.Itoa(wfdir.RuntimeQuery), "--json")
	if err != nil {
		t.Fatalf("init --runtime %d: %v", wfdir.RuntimeQuery, err)
	}
	if payload := decodeJSON(t, out); payload["runtime"] != float64(wfdir.RuntimeQuery) {
		t.Errorf("reported runtime = %v want %d", payload["runtime"], wfdir.RuntimeQuery)
	}

	m, err := wfdir.LoadManifest(dir, wfdir.WorkflowKind)
	if err != nil {
		t.Fatalf("load the manifest the init just wrote: %v", err)
	}
	if m.RuntimeVersion() != wfdir.RuntimeQuery {
		t.Errorf("manifest runtime = %d want %d", m.RuntimeVersion(), wfdir.RuntimeQuery)
	}
	// v3 is Durable plus a withheld table credential, so everything that branches
	// on "does this journal every step" must say yes.
	if !m.IsDurable() {
		t.Error("runtime 3 must read as durable — it journals every step exactly like runtime 2")
	}
	if body := readFile(t, dir, "main.py"); !strings.Contains(body, "@tools.step") {
		t.Errorf("a durable runtime with no --from must scaffold a durable main.py, got %q", body)
	}
}

// A manifest declaring a runtime this build has never heard of is refused at
// LOAD, not at the first unattended run. Nothing used to validate it on push, so
// a hand-edited value sailed through and was modelled by IsDurable's `>=` on
// nothing but its number.
func TestLoadManifestRejectsAnUnknownRuntime(t *testing.T) {
	f := newFakeInstance(t)
	signIn(t, f)
	dir := t.TempDir()

	if _, err := runCLI(t, dir, "wf", "init", "--feature", "feat-1", "--json"); err != nil {
		t.Fatalf("init: %v", err)
	}
	raw := readFile(t, dir, wfdir.ManifestName)
	edited := strings.Replace(raw, "{", "{\n  \"runtime\": 4,", 1)
	if edited == raw {
		t.Fatal("could not hand-edit the manifest — this test needs re-aiming, not deleting")
	}
	writeLocal(t, dir, wfdir.ManifestName, edited)

	_, err := wfdir.LoadManifest(dir, wfdir.WorkflowKind)
	if err == nil {
		t.Fatal("a runtime this CLI does not know must be refused at load")
	}
	if !strings.Contains(err.Error(), "runtime") {
		t.Errorf("the refusal must name the runtime: %v", err)
	}
}

// A plain `wf init` — no --runtime at all — has to scaffold and report for the
// runtime it is ABOUT TO CREATE, which is RuntimeCreateDefault and not the
// flag's own default of 1. Reading the flag there gave the author an empty
// main.py and a report that named no runtime, while the first push created the
// workflow on 3: they then wrote a v1-shaped `con.execute("... {{ ref }} ...")`
// table read into a runtime that refuses it, and found out at the first run —
// on an automation, at 03:00.
//
// The manifest key stays ABSENT, and that half is load-bearing rather than
// incidental: it is what lets the first push write back whatever the server
// actually stamped, which is the fix for the second-push brick. Scaffolding for
// 3 must not pin the folder to 3.
func TestInitWithNoRuntimeFlagScaffoldsForTheCreateDefault(t *testing.T) {
	f := newFakeInstance(t)
	signIn(t, f)
	dir := t.TempDir()

	out, err := runCLI(t, dir, "wf", "init", "--feature", "feat-1", "--json")
	if err != nil {
		t.Fatalf("init: %v", err)
	}
	payload := decodeJSON(t, out)
	if payload["runtime"] != float64(wfdir.RuntimeCreateDefault) {
		t.Errorf("runtime = %v, want %d", payload["runtime"], wfdir.RuntimeCreateDefault)
	}
	if payload["scaffolded"] != true {
		t.Errorf("scaffolded = %v, want true", payload["scaffolded"])
	}
	if raw := readFile(t, dir, wfdir.ManifestName); strings.Contains(raw, "runtime") {
		t.Errorf("init pinned a runtime nobody asked for — the first push can no longer write back what the server stamped: %s", raw)
	}
	// The scaffold is the create default's, not merely "a durable one": under
	// runtime 3 the table rule is the one thing an author cannot infer from an
	// empty file, and it is exactly what the failure above turned on.
	scaffold := readFile(t, dir, "main.py")
	if !strings.Contains(scaffold, "@tools.step") {
		t.Errorf("a plain init wrote no durable scaffold:\n%s", scaffold)
	}
	if !strings.Contains(scaffold, "Read a Ronja table ONLY with tools.query") {
		t.Errorf("the scaffold does not state the table rule of the runtime it will be created on:\n%s", scaffold)
	}

	// And the human report says which runtime that is, in the same words the
	// --runtime 3 report uses.
	reportDir := t.TempDir()
	report, err := runCLI(t, reportDir, "wf", "init", "--feature", "feat-1")
	if err != nil {
		t.Fatalf("init (report): %v", err)
	}
	_, sentence := runtimeInfo(wfdir.RuntimeCreateDefault)
	want := fmt.Sprintf("  Runtime:    %d — %s\n", wfdir.RuntimeCreateDefault, sentence)
	if !strings.Contains(report, want) {
		t.Errorf("the init report does not name the runtime it will create (%q):\n%s", want, report)
	}
}

// --runtime 2 has to produce both halves: the manifest key the first push sends
// as runtimeVersion, and code written in the shapes a resume depends on. A
// durable workflow scaffolded from v1 code journals nothing, which is a silent
// failure — the run works, and the resume that was the point of it does not.
func TestInitDurableRuntimeWritesTheKeyAndAScaffold(t *testing.T) {
	f := newFakeInstance(t)
	signIn(t, f)
	dir := t.TempDir()

	out, err := runCLI(t, dir, "wf", "init", "--feature", "feat-1", "--runtime", "2", "--json")
	if err != nil {
		t.Fatalf("init: %v", err)
	}
	payload := decodeJSON(t, out)
	if payload["runtime"] != float64(wfdir.RuntimeDurable) {
		t.Errorf("runtime = %v, want %d", payload["runtime"], wfdir.RuntimeDurable)
	}
	if payload["scaffolded"] != true {
		t.Errorf("scaffolded = %v, want true", payload["scaffolded"])
	}

	manifest, err := wfdir.LoadManifest(dir, wfdir.WorkflowKind)
	if err != nil {
		t.Fatalf("load manifest: %v", err)
	}
	if !manifest.IsDurable() {
		t.Fatalf("manifest runtime = %d, want %d", manifest.RuntimeVersion(), wfdir.RuntimeDurable)
	}

	// The scaffold is checked by the PROPERTIES that make a resume work, not by
	// its prose: decorated steps, a keyless loop, and a journaled clock.
	scaffold := readFile(t, dir, "main.py")
	// The HEADER is the exception, and it is pinned rather than sampled: it is
	// the one part of the template that varies by runtime, and every property
	// checked below lives in the shared body — so a header that named the wrong
	// runtime, or leaked runtime 3's table rule into a runtime-2 folder (a rule
	// that does not apply to it), would pass every other assertion here.
	if want := "# Durable workflow (runtime 2). Every @tools.step result is journaled, so\n"; !strings.HasPrefix(scaffold, want) {
		t.Errorf("the durable scaffold does not open with %q:\n%s", want, scaffold)
	}
	if strings.Contains(scaffold, "Read a Ronja table ONLY") {
		t.Errorf("a runtime-2 scaffold carries runtime 3's table rule:\n%s", scaffold)
	}
	for _, want := range []string{"@tools.step", "for order_id in load_orders():", "tools.now()"} {
		if !strings.Contains(scaffold, want) {
			t.Errorf("the durable scaffold is missing %q:\n%s", want, scaffold)
		}
	}
	// CODE lines only: the comment naming datetime.now() is the constraint being
	// stated, and a substring check over the whole file would flag it.
	for _, line := range strings.Split(scaffold, "\n") {
		if strings.HasPrefix(strings.TrimSpace(line), "#") {
			continue
		}
		if strings.Contains(line, "datetime.now()") {
			t.Errorf("the durable scaffold reaches for the wall clock, which a resume cannot replay: %s", line)
		}
	}
	// Keys are DERIVED in the durable runtime. A scaffold carrying a hand-written
	// key would teach the one habit that breaks a loop's resume: two iterations
	// on one key.
	if strings.Contains(scaffold, `tools.step("`) {
		t.Errorf("the durable scaffold writes a key string:\n%s", scaffold)
	}
}

// `wf init --runtime 2` inside a directory that already holds main.py is the
// documented way to adopt a script you have. The scaffold is skipped, never
// written over the file.
func TestInitDurableRuntimeLeavesAnExistingEntrypointAlone(t *testing.T) {
	f := newFakeInstance(t)
	signIn(t, f)
	dir := t.TempDir()
	writeLocal(t, dir, "main.py", "print('mine')\n")

	out, err := runCLI(t, dir, "wf", "init", "--feature", "feat-1", "--runtime", "2", "--json")
	if err != nil {
		t.Fatalf("init: %v", err)
	}
	if payload := decodeJSON(t, out); payload["scaffolded"] != false {
		t.Errorf("scaffolded = %v, want false", payload["scaffolded"])
	}
	if got := readFile(t, dir, "main.py"); got != "print('mine')\n" {
		t.Errorf("main.py = %q — the scaffold overwrote an existing entrypoint", got)
	}
}

// --- status ----------------------------------------------------------------

func TestStatusCleanAfterClone(t *testing.T) {
	f, root := clonedFolder(t)

	out, err := runCLI(t, root, "wf", "status", "--json")
	if err != nil {
		t.Fatalf("status: %v", err)
	}
	report := decodeJSON(t, out)

	local := report["local"].(map[string]any)
	assertEmptyList(t, local, "added", "modified", "deleted")
	if local["unchanged"].(float64) != 2 {
		t.Errorf("unchanged = %v, want 2", local["unchanged"])
	}

	remote := report["remote"].(map[string]any)
	if remote["checked"] != true {
		t.Errorf("remote not checked: %v", remote)
	}
	if remote["lifecycle"] != api.LifecycleLive {
		t.Errorf("lifecycle = %v", remote["lifecycle"])
	}
	drift := remote["drift"].(map[string]any)
	assertEmptyList(t, drift, "added", "modified", "deleted")
	if _, present := remote["driftNotes"]; present {
		t.Errorf("clean status carries drift notes: %v", remote["driftNotes"])
	}
	_ = f
}

func TestStatusReportsLocalChanges(t *testing.T) {
	_, root := clonedFolder(t)

	if err := os.WriteFile(filepath.Join(root, "main.py"), []byte("edited\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "added.py"), []byte("new\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(filepath.Join(root, "lib", "helpers.py")); err != nil {
		t.Fatal(err)
	}

	out, err := runCLI(t, root, "wf", "status", "--json")
	if err != nil {
		t.Fatalf("status: %v", err)
	}
	local := decodeJSON(t, out)["local"].(map[string]any)
	assertList(t, local, "added", "added.py")
	assertList(t, local, "modified", "main.py")
	assertList(t, local, "deleted", "lib/helpers.py")
}

// A symlink is not workflow source, but a symlinked module is an ordinary thing
// to have — and one that silently vanishes from every push is the surprise
// Skipped exists to prevent.
func TestStatusReportsSkippedNonRegularFile(t *testing.T) {
	_, root := clonedFolder(t)

	target := filepath.Join(root, "main.py")
	if err := os.Symlink(target, filepath.Join(root, "linked.py")); err != nil {
		t.Skipf("symlinks unavailable: %v", err)
	}

	out, err := runCLI(t, root, "wf", "status", "--json")
	if err != nil {
		t.Fatalf("status: %v", err)
	}
	skipped, ok := decodeJSON(t, out)["skipped"].([]any)
	if !ok || len(skipped) != 1 {
		t.Fatalf("skipped = %v, want one entry", decodeJSON(t, out)["skipped"])
	}
	entry := skipped[0].(map[string]any)
	if entry["path"] != "linked.py" || !strings.Contains(entry["reason"].(string), "regular file") {
		t.Errorf("skipped entry = %v", entry)
	}
}

func TestStatusReportsRemoteDrift(t *testing.T) {
	f, root := clonedFolder(t)

	// Someone edits the same workflow in the web builder.
	f.files["wf-1"] = []api.WorkflowFile{
		{Path: "main.py", Content: "changed elsewhere\n"},
		{Path: "remote-only.py", Content: "added elsewhere\n"},
	}

	out, err := runCLI(t, root, "wf", "status", "--json")
	if err != nil {
		t.Fatalf("status: %v", err)
	}
	remote := decodeJSON(t, out)["remote"].(map[string]any)
	drift := remote["drift"].(map[string]any)
	assertList(t, drift, "added", "remote-only.py")
	assertList(t, drift, "modified", "main.py")
	assertList(t, drift, "deleted", "lib/helpers.py")
}

// Both notes must survive. The draft-check failure used to be overwritten by
// the baseline-mismatch note, so drift silently reported itself as measured
// against a row the caller had no reason to doubt.
func TestStatusKeepsEveryDriftNote(t *testing.T) {
	f := newFakeInstance(t)
	f.AddWorkflow(
		&api.Workflow{ID: "wf-1", Lifecycle: api.LifecycleLive},
		api.WorkflowFile{Path: "main.py", Content: "live\n"},
	)
	f.AddDraft("wf-1", "wf-1-draft", api.WorkflowFile{Path: "main.py", Content: "draft\n"})
	signIn(t, f)
	parent := t.TempDir()

	// Clone while the draft exists, so the baseline is taken from it.
	if _, err := runCLI(t, parent, "wf", "clone", "wf-1", "dest"); err != nil {
		t.Fatalf("clone: %v", err)
	}
	root := filepath.Join(parent, "dest")

	// Now the draft read breaks: status falls back to the live row, which is
	// BOTH "could not check for your draft" and "baseline came from elsewhere".
	f.failDraft = 500

	out, err := runCLI(t, root, "wf", "status", "--json")
	if err != nil {
		t.Fatalf("status: %v", err)
	}
	remote := decodeJSON(t, out)["remote"].(map[string]any)
	notes, ok := remote["driftNotes"].([]any)
	if !ok {
		t.Fatalf("driftNotes = %v, want a list", remote["driftNotes"])
	}
	joined := ""
	for _, n := range notes {
		joined += n.(string) + "\n"
	}
	if !strings.Contains(joined, "could not check for your open draft") {
		t.Errorf("the draft-check failure was lost:\n%s", joined)
	}
	if !strings.Contains(joined, "baseline came from wf-1-draft") {
		t.Errorf("the baseline mismatch was lost:\n%s", joined)
	}
}

// Signed out, status still delivers the local half — which is most of the
// value, and costs nothing.
func TestStatusDegradesWhenSignedOut(t *testing.T) {
	f, root := clonedFolder(t)
	signOut(t, f)

	out, err := runCLI(t, root, "wf", "status", "--json")
	if err != nil {
		t.Fatalf("status: %v", err)
	}
	report := decodeJSON(t, out)
	if report["local"].(map[string]any)["unchanged"].(float64) != 2 {
		t.Errorf("local half missing: %v", report["local"])
	}
	remote := report["remote"].(map[string]any)
	if remote["checked"] != false {
		t.Errorf("remote checked = %v, want false", remote["checked"])
	}
	if !strings.Contains(remote["notCheckedReason"].(string), "not signed in") {
		t.Errorf("notCheckedReason = %v", remote["notCheckedReason"])
	}
}

// ...and so does a status whose organization LOOKUP failed, which is the same
// promise one credential further along.
//
// The scenario is a CI pre-flight running `wf status --json` under a token that
// has since been rotated. Resolving the organization is a request, and a request
// can fail; making that fatal turned the one command you run when you are
// already suspicious into the one that stops answering. The local half needs no
// server and must arrive whatever the server says.
func TestStatusDegradesWhenTheOrganizationLookupFails(t *testing.T) {
	f, root := clonedFolder(t)
	f.failMe = 401
	f.Requests = nil // the clone's, not this command's

	out, err := runCLI(t, root, "wf", "status", "--json")
	if err != nil {
		t.Fatalf("status aborted on a failed organization lookup: %v", err)
	}
	report := decodeJSON(t, out)
	if report["local"].(map[string]any)["unchanged"].(float64) != 2 {
		t.Errorf("local half missing: %v", report["local"])
	}
	remote := report["remote"].(map[string]any)
	if remote["checked"] != false {
		t.Errorf("remote checked = %v, want false", remote["checked"])
	}
	reason, _ := remote["notCheckedReason"].(string)
	if !strings.Contains(reason, "which organization") {
		t.Errorf("notCheckedReason = %q, want the failed lookup named", reason)
	}
	// And it stopped there: reading the binding's workflow under a credential
	// whose organization is unknown is the adoption bug, not a degradation.
	if got := requestsMatching(f, "GET /api/v2/authentication/me"); len(f.Requests) != len(got) {
		t.Errorf("looked at the server anyway: %v", f.Requests)
	}

	// The other half of the same rule: a command that ACTS still refuses. There
	// the answer is load-bearing — without it the binding is matched on URL
	// alone, which is how another organization's ids reach this token.
	if _, err := runCLI(t, root, "wf", "push", "--json"); err == nil {
		t.Fatal("push proceeded without knowing which organization it was pushing to")
	}
}

// Signed out AND bound to two organizations on one instance: there is no way to
// tell which binding applies, so status must say so rather than show one
// organization's workflow as if it were this folder's.
//
// End-to-end rather than against the manifest alone, because the degradation is
// a WIRING property — the lookup's ambiguity error has to survive openFolder and
// reach the report. Unit-testing the lookup proves the rule exists, not that
// anything consults it.
func TestStatusRefusesToGuessBetweenOrganizations(t *testing.T) {
	f, root := clonedFolder(t)
	signOut(t, f)

	// A second organization's binding on the same instance, as a colleague in
	// another organization would have committed.
	manifest, err := wfdir.LoadManifest(root, wfdir.WorkflowKind)
	if err != nil {
		t.Fatalf("load manifest: %v", err)
	}
	manifest.SetBinding(
		wfdir.InstanceKey{URL: f.URL(), TenantID: "ten-other"},
		wfdir.Binding{WorkflowID: "wf-other", FeatureID: "feat-other"})
	if err := wfdir.SaveManifest(root, manifest); err != nil {
		t.Fatalf("save manifest: %v", err)
	}

	out, err := runCLI(t, root, "wf", "status", "--json")
	if err != nil {
		t.Fatalf("status: %v", err)
	}
	report := decodeJSON(t, out)
	if report["ambiguous"] != true {
		t.Errorf("ambiguous = %v, want true", report["ambiguous"])
	}
	// Neither organization's workflow may be presented as this folder's.
	if got := report["workflowID"]; got != nil {
		t.Errorf("workflowID = %v — one organization's binding was picked", got)
	}
	remote := report["remote"].(map[string]any)
	if !strings.Contains(remote["notCheckedReason"].(string), "several organizations") {
		t.Errorf("notCheckedReason = %v, want the ambiguity named", remote["notCheckedReason"])
	}
	// The local half still works — that is the whole point of degrading.
	if report["local"].(map[string]any)["unchanged"].(float64) != 2 {
		t.Errorf("local half missing: %v", report["local"])
	}
}

// ...and naming one resolves it, without needing a credential at all: the
// profile carries the organization, which is the half the lookup was missing.
func TestStatusDisambiguatesByProfile(t *testing.T) {
	f, root := clonedFolder(t)
	signOut(t, f)

	manifest, err := wfdir.LoadManifest(root, wfdir.WorkflowKind)
	if err != nil {
		t.Fatalf("load manifest: %v", err)
	}
	manifest.SetBinding(
		wfdir.InstanceKey{URL: f.URL(), TenantID: "ten-other"},
		wfdir.Binding{WorkflowID: "wf-other", FeatureID: "feat-other"})
	if err := wfdir.SaveManifest(root, manifest); err != nil {
		t.Fatalf("save manifest: %v", err)
	}
	writeProfile(t, "other", f.URL(), "ten-other", "")

	out, err := runCLI(t, root, "wf", "status", "--json", "--profile", "other")
	if err != nil {
		t.Fatalf("status: %v", err)
	}
	report := decodeJSON(t, out)
	if report["ambiguous"] != nil {
		t.Errorf("ambiguous = %v, want absent once a profile names the organization", report["ambiguous"])
	}
	if got := report["workflowID"]; got != "wf-other" {
		t.Errorf("workflowID = %v, want the named organization's wf-other", got)
	}
}

// rebindFolderTo replaces every instance entry with ONE, the way a colleague in
// another organization would have committed it.
func rebindFolderTo(t *testing.T, root string, key wfdir.InstanceKey, binding wfdir.Binding) {
	t.Helper()
	manifest, err := wfdir.LoadManifest(root, wfdir.WorkflowKind)
	if err != nil {
		t.Fatalf("load manifest: %v", err)
	}
	manifest.Instances = nil
	manifest.SetBinding(key, binding)
	if err := wfdir.SaveManifest(root, manifest); err != nil {
		t.Fatalf("save manifest: %v", err)
	}
}

// An ENVIRONMENT token — the documented CI credential — carries no organization,
// and the binding lookup falls back to matching on URL alone when it does not
// know one. So a folder committed by a colleague in ANOTHER organization used to
// be adopted whole: their workflow id, their feature id, sent under this token.
//
// End-to-end rather than against the lookup, because the defect was never in the
// lookup: InstanceKey has always refused to adopt another organization's entry.
// What was missing is that nothing ASKED which organization the token belongs to
// before the lookup ran.
func TestPushUnderAnEnvironmentTokenDoesNotAdoptAnotherOrganizationsBinding(t *testing.T) {
	f, root := clonedFolder(t)
	// Their workflow, on this instance, in their organization. Ours is ten-test.
	f.AddWorkflow(&api.Workflow{ID: "wf-other", Lifecycle: api.LifecycleLive, FeatureID: "feat-other"},
		api.WorkflowFile{Path: "main.py", Content: "THEIRS\n"})
	rebindFolderTo(t, root, wfdir.InstanceKey{URL: f.URL(), TenantID: "ten-other"},
		wfdir.Binding{WorkflowID: "wf-other", FeatureID: "feat-other"})
	writeLocal(t, root, "main.py", "OURS\n")

	_, err := runCLI(t, root, "wf", "push", "--json")
	if err == nil {
		t.Fatal("push accepted a folder bound only to another organization")
	}
	// Nothing of theirs may be touched, and nothing of ours created in their
	// feature — the failure this refusal exists to prevent is a WRITE, so the
	// requests are the assertion.
	for _, req := range f.Requests {
		if strings.Contains(req, "wf-other") && !strings.HasPrefix(req, "GET ") {
			t.Errorf("wrote to another organization's workflow: %v", f.Requests)
		}
	}
	if len(f.created) != 0 {
		t.Errorf("created a workflow anyway: %+v", f.created)
	}
	if got := f.files["wf-other"][0].Content; got != "THEIRS\n" {
		t.Errorf("their main.py = %q, want it untouched", got)
	}
	// And the refusal has to be about the ORGANIZATION. The old one said this
	// folder named no feature, which reads as "add featureID" — a key the entry
	// they are looking at already has.
	if !strings.Contains(err.Error(), "ten-other") || !strings.Contains(err.Error(), testTenantID) {
		t.Errorf("error = %v, want both organizations named", err)
	}
}

// The same adoption, one door along: a STORED profile with no organization.
//
// An environment token is not the only credential that arrives without one —
// login writes an empty tenantID for a user who belonged to no organization at
// the time, and deliberately never rewrites it, so the profile still carries
// none long after they joined one. Gating the lookup on the environment token
// alone left this profile matching on URL alone, which is the same adoption with
// a different credential in front of it.
func TestPushWithAnOrganizationlessProfileDoesNotAdoptAnotherOrganization(t *testing.T) {
	f, root := clonedFolder(t)
	f.AddWorkflow(&api.Workflow{ID: "wf-other", Lifecycle: api.LifecycleLive, FeatureID: "feat-other"},
		api.WorkflowFile{Path: "main.py", Content: "THEIRS\n"})
	rebindFolderTo(t, root, wfdir.InstanceKey{URL: f.URL(), TenantID: "ten-other"},
		wfdir.Binding{WorkflowID: "wf-other", FeatureID: "feat-other"})
	writeLocal(t, root, "main.py", "OURS\n")
	signOut(t, f)
	writeProfile(t, "orgless", f.URL(), "", "test-token")

	_, err := runCLI(t, root, "wf", "push", "--json")
	if err == nil {
		t.Fatal("push accepted a folder bound only to another organization")
	}
	for _, req := range f.Requests {
		if strings.Contains(req, "wf-other") && !strings.HasPrefix(req, "GET ") {
			t.Errorf("wrote to another organization's workflow: %v", f.Requests)
		}
	}
	if len(f.created) != 0 {
		t.Errorf("created a workflow anyway: %+v", f.created)
	}
	if got := f.files["wf-other"][0].Content; got != "THEIRS\n" {
		t.Errorf("their main.py = %q, want it untouched", got)
	}
	if !strings.Contains(err.Error(), "ten-other") || !strings.Contains(err.Error(), testTenantID) {
		t.Errorf("error = %v, want both organizations named", err)
	}
}

// A credential whose organization cannot be established never reads a binding.
//
// This used to be an AMBIGUITY test: two organizations in the folder, a
// credential naming none, and the push refusing by name. The lookup now runs for
// any credential that does not already name one, so the folder's two entries are
// no longer what decides it — the refusal happens one step earlier, at the
// question "which organization is this token in", and a user who is in none has
// nowhere to push whatever their folder says. What it still pins is the part
// that mattered: the push stops, nothing is written, and the diagnosis is not
// the old "add featureID" — advice to add a key the entry already carries.
func TestPushRefusesACredentialWithNoOrganization(t *testing.T) {
	f, root := clonedFolder(t)
	f.noTenant = true
	signOut(t, f)
	writeProfile(t, "orgless", f.URL(), "", "test-token")

	manifest, err := wfdir.LoadManifest(root, wfdir.WorkflowKind)
	if err != nil {
		t.Fatalf("load manifest: %v", err)
	}
	manifest.SetBinding(
		wfdir.InstanceKey{URL: f.URL(), TenantID: "ten-other"},
		wfdir.Binding{WorkflowID: "wf-other", FeatureID: "feat-other"})
	if err := wfdir.SaveManifest(root, manifest); err != nil {
		t.Fatalf("save manifest: %v", err)
	}

	_, err = runCLI(t, root, "wf", "push", "--json")
	if err == nil {
		t.Fatal("push proceeded under a credential belonging to no organization")
	}
	msg := err.Error()
	if !strings.Contains(msg, "no organization") {
		t.Errorf("error = %v, want the missing organization named", err)
	}
	// The old, wrong diagnosis. It must not come back.
	if strings.Contains(msg, "featureID") {
		t.Errorf("error = %v — still reported as a missing featureID", err)
	}
	if len(f.created) != 0 || len(requestsMatching(f, "PUT ")) != 0 {
		t.Errorf("wrote something anyway: %v", f.Requests)
	}
}

// The ambiguity refusal's own wording, unit-tested because it is now a backstop:
// an acting command resolves the organization before it gets here, or is refused
// by the lookup, so nothing routine reaches it. It stays because "the
// organization is unknown" must never reach a binding read — and if it ever does
// again, the message has to name the choices and the one selector, not send the
// reader back to featureID.
func TestRequireOneOrganizationNamesTheChoicesAndTheSelector(t *testing.T) {
	m := &wfdir.Manifest{Kind: wfdir.KindWorkflow}
	m.SetBinding(wfdir.InstanceKey{URL: "http://x.test", TenantID: "ten-a"},
		wfdir.Binding{WorkflowID: "wf-a", FeatureID: "feat-a"})
	m.SetBinding(wfdir.InstanceKey{URL: "http://x.test", TenantID: "ten-b"},
		wfdir.Binding{WorkflowID: "wf-b", FeatureID: "feat-b"})
	f := &folder{
		Root: t.TempDir(), Kind: wfdir.WorkflowKind, Manifest: m,
		Resolved: &config.Resolved{URL: "http://x.test"},
	}
	f.matchBinding()

	err := f.requireOneOrganization()
	if err == nil {
		t.Fatal("an unknown organization on a folder naming two was accepted")
	}
	msg := err.Error()
	for _, want := range []string{"ten-a", "ten-b", "--profile"} {
		if !strings.Contains(msg, want) {
			t.Errorf("error = %v, want it to name %s", err, want)
		}
	}
	if strings.Contains(msg, "featureID") {
		t.Errorf("error = %v — the old, wrong diagnosis", err)
	}
}

// featureIDFor's cross-organization wording belongs to ONE case: this folder has
// no entry here and names other organizations instead. A folder that IS bound
// here and merely left "featureID" out has a line to add the field to, and
// telling it the entries "name X instead" is the same species of wrong advice
// the cross-organization wording exists to remove.
func TestFeatureIDForNamesOtherOrganizationsOnlyWhenUnbound(t *testing.T) {
	build := func(bindHere bool) *folder {
		m := &wfdir.Manifest{Kind: wfdir.KindWorkflow}
		if bindHere {
			m.SetBinding(wfdir.InstanceKey{URL: "http://x.test", TenantID: testTenantID},
				wfdir.Binding{WorkflowID: "wf-1"})
		}
		m.SetBinding(wfdir.InstanceKey{URL: "http://x.test", TenantID: "ten-other"},
			wfdir.Binding{WorkflowID: "wf-other", FeatureID: "feat-other"})
		f := &folder{
			Root: t.TempDir(), Kind: wfdir.WorkflowKind, Manifest: m,
			Resolved: &config.Resolved{URL: "http://x.test", TenantID: testTenantID},
		}
		f.matchBinding()
		return f
	}
	// known carries no feature, so neither leg above the message can answer and
	// no request is made — nil client is the assertion that none is.
	known := &api.Workflow{ID: "wf-1"}

	t.Run("unbound", func(t *testing.T) {
		_, err := featureIDFor(t.Context(), nil, build(false), known)
		if err == nil {
			t.Fatal("a folder with no feature anywhere was accepted")
		}
		if !strings.Contains(err.Error(), "ten-other") {
			t.Errorf("error = %v, want the organization the entries DO name", err)
		}
	})
	t.Run("bound but missing the field", func(t *testing.T) {
		_, err := featureIDFor(t.Context(), nil, build(true), known)
		if err == nil {
			t.Fatal("a binding with no featureID was accepted")
		}
		if strings.Contains(err.Error(), "ten-other") {
			t.Errorf("error = %v — a bound folder was told its entries name another organization", err)
		}
		if !strings.Contains(err.Error(), "featureID") {
			t.Errorf("error = %v, want the field it is missing named", err)
		}
	})
}

func TestStatusReportsBrokenBinding(t *testing.T) {
	f, root := clonedFolder(t)
	delete(f.workflows, "wf-1")

	out, err := runCLI(t, root, "wf", "status", "--json")
	if err != nil {
		t.Fatalf("status: %v", err)
	}
	remote := decodeJSON(t, out)["remote"].(map[string]any)
	if !strings.Contains(remote["problem"].(string), "binding broken") {
		t.Errorf("problem = %v", remote["problem"])
	}
}

func TestStatusOutsideAWorkflowFolder(t *testing.T) {
	f := newFakeInstance(t)
	signIn(t, f)

	_, err := runCLI(t, t.TempDir(), "wf", "status")
	if err == nil || !strings.Contains(err.Error(), wfdir.ManifestName) {
		t.Errorf("error = %v, want one naming %s", err, wfdir.ManifestName)
	}
}

// --- helpers ---------------------------------------------------------------

// clonedFolder is the starting point most status tests want: a live workflow
// with two files, cloned into a folder, signed in and clean.
func clonedFolder(t *testing.T) (*fakeInstance, string) {
	t.Helper()
	f := newFakeInstance(t)
	f.AddWorkflow(
		&api.Workflow{ID: "wf-1", Lifecycle: api.LifecycleLive, Title: "Monthly report"},
		api.WorkflowFile{Path: "main.py", Content: "print('hi')\n"},
		api.WorkflowFile{Path: "lib/helpers.py", Content: "X = 1\n"},
	)
	signIn(t, f)
	parent := t.TempDir()
	if _, err := runCLI(t, parent, "wf", "clone", "wf-1", "dest"); err != nil {
		t.Fatalf("clone: %v", err)
	}
	return f, filepath.Join(parent, "dest")
}

// assertBaselineMatchesDisk is the invariant the critical fix exists to hold:
// the baseline claims a set of paths, and every later command believes it, so
// it must be EXACTLY what is on disk. A baseline entry with no file reads as a
// local deletion, which a push would carry out.
func assertBaselineMatchesDisk(t *testing.T, root string, key wfdir.InstanceKey) {
	t.Helper()
	state, err := wfdir.LoadState(root)
	if err != nil {
		t.Fatalf("load state: %v", err)
	}
	baseline := state.For(key)
	if baseline == nil {
		t.Fatalf("no baseline recorded for %s", key.URL)
	}
	enumeration, err := wfdir.Enumerate(root, wfdir.WorkflowKind)
	if err != nil {
		t.Fatalf("enumerate: %v", err)
	}
	for path, want := range baseline.Hashes() {
		got, ok := enumeration.Files[path]
		if !ok {
			t.Errorf("baseline claims %s, which is not on disk", path)
			continue
		}
		if got != want {
			t.Errorf("%s: baseline hash %s, disk hash %s", path, want, got)
		}
	}
	for path := range enumeration.Files {
		if _, ok := baseline.Files[path]; !ok {
			t.Errorf("%s is on disk but absent from the baseline", path)
		}
	}
}

// assertNothingWritten checks a refused clone left no folder behind. A partial
// one is worse than none: it has no manifest, so the next command cannot even
// explain itself.
func assertNothingWritten(t *testing.T, root string) {
	t.Helper()
	entries, err := os.ReadDir(root)
	if os.IsNotExist(err) {
		return
	}
	if err != nil {
		t.Fatalf("read %s: %v", root, err)
	}
	if len(entries) != 0 {
		names := make([]string, 0, len(entries))
		for _, e := range entries {
			names = append(names, e.Name())
		}
		t.Errorf("refused clone left %v in %s", names, root)
	}
}

func assertList(t *testing.T, diff map[string]any, field string, want ...string) {
	t.Helper()
	raw, _ := json.Marshal(diff[field])
	var got []string
	_ = json.Unmarshal(raw, &got)
	if strings.Join(got, ",") != strings.Join(want, ",") {
		t.Errorf("%s = %v, want %v", field, got, want)
	}
}

func assertEmptyList(t *testing.T, diff map[string]any, fields ...string) {
	t.Helper()
	for _, field := range fields {
		if list, ok := diff[field].([]any); ok && len(list) != 0 {
			t.Errorf("%s = %v, want empty", field, list)
		}
	}
}
