package commands

import (
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"github.com/ronjatech/ronja-cli/internal/api"
	"github.com/ronjatech/ronja-cli/internal/wfdir"
)

// ── The runtime a workflow folder declares ──────────────────────────────────
//
// `runtime` used to be a create-only key: the server had no patch path, so a
// manifest declaring 2 against an existing runtime-1 workflow was silently
// ignored and the author never learned that the Durable code they had just
// pushed would run under v1 semantics.
//
// The server now takes a ONE-WAY upgrade, which gives the folder two jobs and
// exactly two: carry 1 → 2 up on the ordinary metadata patch, and REFUSE the
// downgrade before it writes anything. Both are here.

// setManifestRuntime marks a folder as declaring a runtime, the way a
// hand-edited ronja.json (or `wf init --runtime 2`) does.
func setManifestRuntime(t *testing.T, root string, runtime int) {
	t.Helper()
	m, err := wfdir.LoadManifest(root, wfdir.WorkflowKind)
	if err != nil {
		t.Fatalf("load manifest: %v", err)
	}
	m.Runtime = runtime
	if err := wfdir.SaveManifest(root, m); err != nil {
		t.Fatalf("save manifest: %v", err)
	}
}

// The upgrade: the folder says Durable, the row says standard, and the push
// carries it. This is the whole feature from the CLI's side.
func TestPushSendsTheRuntimeUpgrade(t *testing.T) {
	f := newFakeInstance(t)
	f.AddWorkflow(&api.Workflow{ID: "wf-1", Lifecycle: api.LifecycleLive, RuntimeVersion: wfdir.RuntimeDefault},
		api.WorkflowFile{Path: "main.py", Content: "M\n"})
	signIn(t, f)
	root := cloneFolder(t, f, "wf-1")
	setManifestRuntime(t, root, wfdir.RuntimeDurable)
	writeLocal(t, root, "main.py", "M2\n")

	out, err := runCLI(t, root, "wf", "push", "--json")
	if err != nil {
		t.Fatalf("push: %v", err)
	}
	// The push targets the DRAFT it checked out, which is where the upgrade has
	// to land: committing it is what publishes the flip onto the live workflow,
	// with the code change that needs it.
	draftID := decodeJSON(t, out)["draftID"].(string)
	if got := f.runtimePatches[draftID]; got != wfdir.RuntimeDurable {
		t.Fatalf("runtimeVersion patched on %s = %d, want 2 (patches: %v)", draftID, got, f.runtimePatches)
	}
	report := decodeJSON(t, out)
	if got := report["runtimeUpgradedFrom"]; got != float64(wfdir.RuntimeDefault) {
		t.Errorf("runtimeUpgradedFrom = %v, want 1", got)
	}
	if got := report["runtimeVersion"]; got != float64(wfdir.RuntimeDurable) {
		t.Errorf("runtimeVersion = %v, want 2", got)
	}
}

// A folder that already agrees with the row sends nothing. Without this the
// patch would be non-empty on every push of every Durable workflow, costing a
// round trip and reporting a metadata change that never happened.
func TestPushDoesNotResendAMatchingRuntime(t *testing.T) {
	f := newFakeInstance(t)
	f.AddWorkflow(&api.Workflow{ID: "wf-1", Lifecycle: api.LifecycleLive, RuntimeVersion: wfdir.RuntimeDurable},
		api.WorkflowFile{Path: "main.py", Content: "M\n"})
	signIn(t, f)
	root := cloneFolder(t, f, "wf-1")
	writeLocal(t, root, "main.py", "M2\n")

	if _, err := runCLI(t, root, "wf", "push", "--json"); err != nil {
		t.Fatalf("push: %v", err)
	}
	if len(f.runtimePatches) != 0 {
		t.Fatalf("push re-sent the runtime: %v", f.runtimePatches)
	}
}

// The downgrade. Refused BEFORE any write — the point is that pushing v1 code at
// a Durable row never happens, not that it is reported afterwards.
func TestPushRefusesARuntimeDowngrade(t *testing.T) {
	f := newFakeInstance(t)
	f.AddWorkflow(&api.Workflow{ID: "wf-1", Lifecycle: api.LifecycleLive, RuntimeVersion: wfdir.RuntimeDurable},
		api.WorkflowFile{Path: "main.py", Content: "M\n"})
	signIn(t, f)
	root := cloneFolder(t, f, "wf-1")
	setManifestRuntime(t, root, wfdir.RuntimeDefault)
	writeLocal(t, root, "main.py", "M2\n")

	_, err := runCLI(t, root, "wf", "push")
	if err == nil {
		t.Fatal("push accepted a runtime downgrade")
	}
	for _, want := range []string{"one-way", "runtime"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("refusal missing %q: %v", want, err)
		}
	}
	if len(f.fileWrites) != 0 {
		t.Fatalf("the refusal came after writing files: %+v", f.fileWrites)
	}
}

// An instance that reports no runtime at all predates this contract. It has no
// opinion to disagree with, so neither the upgrade nor the refusal applies —
// pushing must behave exactly as it did before the field existed.
func TestPushIgnoresARuntimeTheInstanceDidNotReport(t *testing.T) {
	f := newFakeInstance(t)
	f.AddWorkflow(&api.Workflow{ID: "wf-1", Lifecycle: api.LifecycleLive},
		api.WorkflowFile{Path: "main.py", Content: "M\n"})
	signIn(t, f)
	root := cloneFolder(t, f, "wf-1")
	setManifestRuntime(t, root, wfdir.RuntimeDurable)
	writeLocal(t, root, "main.py", "M2\n")

	if _, err := runCLI(t, root, "wf", "push", "--json"); err != nil {
		t.Fatalf("push: %v", err)
	}
	if len(f.runtimePatches) != 0 {
		t.Fatalf("push guessed at a runtime the server never reported: %v", f.runtimePatches)
	}
}

// `wf status` answers "what would a push do" before you run one, and the runtime
// is now one of the answers — in BOTH directions, since one of them is a refusal
// rather than a change.
func TestStatusReportsRuntimeDrift(t *testing.T) {
	t.Run("an upgrade a push would apply", func(t *testing.T) {
		f := newFakeInstance(t)
		f.AddWorkflow(&api.Workflow{ID: "wf-1", Lifecycle: api.LifecycleLive, RuntimeVersion: wfdir.RuntimeDefault},
			api.WorkflowFile{Path: "main.py", Content: "M\n"})
		signIn(t, f)
		root := cloneFolder(t, f, "wf-1")
		setManifestRuntime(t, root, wfdir.RuntimeDurable)

		out, err := runCLI(t, root, "wf", "status", "--json")
		if err != nil {
			t.Fatalf("status: %v", err)
		}
		runtime := decodeJSON(t, out)["remote"].(map[string]any)["runtime"].(map[string]any)
		if runtime["local"] != float64(2) || runtime["remote"] != float64(1) {
			t.Fatalf("runtime report = %v, want local 2 / remote 1", runtime)
		}
		if _, refused := runtime["refused"]; refused {
			t.Errorf("an upgrade is reported as refused: %v", runtime)
		}
	})

	t.Run("a downgrade a push would refuse", func(t *testing.T) {
		f := newFakeInstance(t)
		f.AddWorkflow(&api.Workflow{ID: "wf-1", Lifecycle: api.LifecycleLive, RuntimeVersion: wfdir.RuntimeDurable},
			api.WorkflowFile{Path: "main.py", Content: "M\n"})
		signIn(t, f)
		root := cloneFolder(t, f, "wf-1")
		setManifestRuntime(t, root, wfdir.RuntimeDefault)

		out, err := runCLI(t, root, "wf", "status", "--json")
		if err != nil {
			t.Fatalf("status: %v", err)
		}
		runtime := decodeJSON(t, out)["remote"].(map[string]any)["runtime"].(map[string]any)
		if runtime["refused"] != true {
			t.Fatalf("a downgrade is not reported as refused: %v", runtime)
		}
	})

	t.Run("silent when they agree", func(t *testing.T) {
		f := newFakeInstance(t)
		f.AddWorkflow(&api.Workflow{ID: "wf-1", Lifecycle: api.LifecycleLive, RuntimeVersion: wfdir.RuntimeDurable},
			api.WorkflowFile{Path: "main.py", Content: "M\n"})
		signIn(t, f)
		root := cloneFolder(t, f, "wf-1")

		out, err := runCLI(t, root, "wf", "status", "--json")
		if err != nil {
			t.Fatalf("status: %v", err)
		}
		if got, ok := decodeJSON(t, out)["remote"].(map[string]any)["runtime"]; ok {
			t.Fatalf("status reports runtime drift where there is none: %v", got)
		}
	})
}

// ── Runtime 3, and what a report says about it ──────────────────────────────
//
// The CLI already BRANCHED correctly on a third runtime (every comparison is
// `>=` or `<`); what it did not do was say anything true about one. Five print
// sites each spelled a runtime's meaning inline, four of them hardcoding
// "(Durable)", so a runtime-3 push offered `wf test --resume` as though that
// were the whole story.
//
// Every assertion below is on the EXACT rendered line, never on the absence of
// the word "Durable": runtime 3 IS durable, its label says so on purpose, and a
// substring assertion here would assert nothing.

// The create summary says what the runtime it just stamped MEANS, and the two
// journaling runtimes do not mean the same thing: v2's line offers the resume
// invocation, v3's states where a table comes from.
func TestCreateSummaryDescribesTheRuntimeItStamped(t *testing.T) {
	const (
		durableLine = "    runtime  2 — steps are journaled; a failed run resumes with `ronja wf test --resume`\n"
		queryLine   = "    runtime  3 — steps are journaled; tables are read only through `tools.query` — the container holds no table credential\n"
	)

	t.Run("runtime 2 is unchanged", func(t *testing.T) {
		f := newFakeInstance(t)
		signIn(t, f)
		dir := t.TempDir()
		if _, err := runCLI(t, dir, "wf", "init", "--feature", "feat-1", "--runtime", "2"); err != nil {
			t.Fatalf("init: %v", err)
		}
		out, err := runCLI(t, dir, "wf", "push")
		if err != nil {
			t.Fatalf("push: %v", err)
		}
		if !strings.Contains(out, durableLine) {
			t.Errorf("the runtime-2 create summary changed:\n%s", out)
		}
	})

	t.Run("runtime 3 says what runtime 3 is", func(t *testing.T) {
		f := newFakeInstance(t)
		signIn(t, f)
		dir := t.TempDir()
		if _, err := runCLI(t, dir, "wf", "init", "--feature", "feat-1", "--runtime", "3"); err != nil {
			t.Fatalf("init: %v", err)
		}
		out, err := runCLI(t, dir, "wf", "push")
		if err != nil {
			t.Fatalf("push: %v", err)
		}
		if !strings.Contains(out, queryLine) {
			t.Errorf("the runtime-3 create summary does not describe runtime 3:\n%s", out)
		}
		if strings.Contains(out, durableLine) {
			t.Errorf("the runtime-3 create summary printed runtime 2's sentence:\n%s", out)
		}
		// The create body has to carry it too, or the summary is describing a
		// runtime the server never got.
		if len(f.created) != 1 || f.created[0].RuntimeVersion != wfdir.RuntimeQuery {
			t.Errorf("create sent %+v, want runtimeVersion 3", f.created)
		}
	})
}

// The `wf init` report names the runtime it stamped from the same one place, so
// a folder's first screen and its first push agree about what it is.
func TestInitReportNamesTheRuntimeItStamped(t *testing.T) {
	for _, tc := range []struct {
		runtime int
		want    string
	}{
		{2, "  Runtime:    2 — steps are journaled; a failed run resumes with `ronja wf test --resume`\n"},
		{3, "  Runtime:    3 — steps are journaled; tables are read only through `tools.query` — the container holds no table credential\n"},
	} {
		f := newFakeInstance(t)
		signIn(t, f)
		dir := t.TempDir()
		out, err := runCLI(t, dir, "wf", "init", "--feature", "feat-1", "--runtime", strconv.Itoa(tc.runtime))
		if err != nil {
			t.Fatalf("init --runtime %d: %v", tc.runtime, err)
		}
		if !strings.Contains(out, tc.want) {
			t.Errorf("the init report does not describe runtime %d:\n%s", tc.runtime, out)
		}
	}
}

// The runtime-3 starter states v3's one added rule. The body is the shared
// durable template and makes no table access at all, which is what keeps it
// pushable exactly as written under the v3 marker rules.
func TestQueryRuntimeScaffoldStatesTheTableRule(t *testing.T) {
	f := newFakeInstance(t)
	signIn(t, f)
	dir := t.TempDir()

	if _, err := runCLI(t, dir, "wf", "init", "--feature", "feat-1", "--runtime", "3"); err != nil {
		t.Fatalf("init --runtime 3: %v", err)
	}
	scaffold := readFile(t, dir, "main.py")
	if want := "# Durable workflow (runtime 3). Every @tools.step result is journaled, so\n"; !strings.HasPrefix(scaffold, want) {
		t.Errorf("the runtime-3 scaffold does not open with %q:\n%s", want, scaffold)
	}
	const rule = "#\n# Read a Ronja table ONLY with tools.query(\"… FROM {{ ref('tbl-id') }} …\") — the\n" +
		"# marker goes inside the SQL string, and the container holds no credential for\n" +
		"# the table's files.\n"
	if !strings.Contains(scaffold, rule) {
		t.Errorf("the runtime-3 scaffold does not state the table rule:\n%s", scaffold)
	}
	if !strings.Contains(scaffold, "for order_id in load_orders():") {
		t.Errorf("the runtime-3 scaffold is not the durable template:\n%s", scaffold)
	}
}

// The upgrade line names the runtime being moved TO, and 2 → 3 is a real upgrade
// the server takes. It used to print "(Durable)" whatever the target was.
func TestUpgradeLineNamesRuntimeThree(t *testing.T) {
	f := newFakeInstance(t)
	f.AddWorkflow(&api.Workflow{ID: "wf-1", Lifecycle: api.LifecycleLive, RuntimeVersion: wfdir.RuntimeDurable},
		api.WorkflowFile{Path: "main.py", Content: "M\n"})
	signIn(t, f)
	root := cloneFolder(t, f, "wf-1")
	setManifestRuntime(t, root, wfdir.RuntimeQuery)
	writeLocal(t, root, "main.py", "M2\n")

	out, err := runCLI(t, root, "wf", "push")
	if err != nil {
		t.Fatalf("push: %v", err)
	}
	const want = "    runtime  2 → 3 (Durable, tables via tools.query) — one way; takes effect for live runs at `ronja wf publish`\n"
	if !strings.Contains(out, want) {
		t.Errorf("the upgrade line does not name runtime 3:\n%s", out)
	}
	// describePatch renders the same patch on the SUCCESS half of the report, and
	// is the string a stopping push puts in its error — no other test reaches it
	// for the runtime.
	if !strings.Contains(out, "updated  the runtime to 3 (Durable, tables via tools.query)\n") {
		t.Errorf("the metadata line does not name runtime 3:\n%s", out)
	}
}

// The downgrade is still refused — and the refusal has to name the runtime the
// SERVER is on, or it tells the author to set a value it just mis-described.
func TestPushRefusesADowngradeFromRuntimeThree(t *testing.T) {
	f := newFakeInstance(t)
	f.AddWorkflow(&api.Workflow{ID: "wf-1", Lifecycle: api.LifecycleLive, RuntimeVersion: wfdir.RuntimeQuery},
		api.WorkflowFile{Path: "main.py", Content: "M\n"})
	signIn(t, f)
	root := cloneFolder(t, f, "wf-1")
	setManifestRuntime(t, root, wfdir.RuntimeDurable)
	writeLocal(t, root, "main.py", "M2\n")

	_, err := runCLI(t, root, "wf", "push")
	if err == nil {
		t.Fatal("push accepted a downgrade from runtime 3")
	}
	const want = "this folder declares runtime 2, but wf-1 is already runtime 3 (Durable, tables via tools.query) on the server — the runtime is a one-way upgrade and cannot be lowered."
	if !strings.Contains(err.Error(), want) {
		t.Errorf("the refusal misnames the server's runtime: %v", err)
	}
	if len(f.fileWrites) != 0 {
		t.Fatalf("the refusal came after writing files: %+v", f.fileWrites)
	}
}

// ── The create default is the SERVER's, and it is no longer 1 ───────────────

// A folder that declares no runtime lets the server choose, and the server now
// chooses 3. The folder records what was chosen, because `runtime` is a
// COMMITTED key and the folder is the only lasting statement of the runtime its
// code is written for — a folder later pushed into a NEW workflow elsewhere
// would otherwise create whatever that instance's default happens to be.
// (An absent key is no longer a second-push brick: checkRuntimeDrift reads it
// as declaring nothing. The write-back is about identity, not about that.)
func TestCreateRecordsTheRuntimeTheServerStamped(t *testing.T) {
	f := newFakeInstance(t)
	f.createRuntime = wfdir.RuntimeQuery
	signIn(t, f)
	dir := t.TempDir()

	if _, err := runCLI(t, dir, "wf", "init", "--feature", "feat-1"); err != nil {
		t.Fatalf("init: %v", err)
	}
	if raw := readFile(t, dir, wfdir.ManifestName); strings.Contains(raw, "runtime") {
		t.Fatalf("init wrote a runtime key without being asked for one: %s", raw)
	}
	// `wf init` with no --runtime scaffolds for the runtime the create will
	// produce; this pins the push, not the scaffold, so the entrypoint is
	// overwritten with a body of the test's own.
	writeLocal(t, dir, "main.py", "print('one')\n")

	out, err := runCLI(t, dir, "wf", "push")
	if err != nil {
		t.Fatalf("push: %v", err)
	}
	if len(f.created) != 1 || f.created[0].RuntimeVersion != 0 {
		t.Fatalf("create declared a runtime the folder never did: %+v", f.created)
	}
	// A create rehearsed against runtime 0 would rehearse a save that checks
	// nothing runtime-scoped — a different save from the one about to happen.
	if len(f.validated) == 0 || f.validated[len(f.validated)-1].RuntimeVersion != wfdir.RuntimeCreateDefault {
		t.Errorf("the validate before a create did not name the runtime it will be created on: %+v", f.validated)
	}
	// The summary names what the SERVER stamped, not what the folder guessed.
	const want = "    runtime  3 — steps are journaled; tables are read only through `tools.query` — the container holds no table credential\n"
	if !strings.Contains(out, want) {
		t.Errorf("the create summary does not name the runtime the server stamped:\n%s", out)
	}
	m, err := wfdir.LoadManifest(dir, wfdir.WorkflowKind)
	if err != nil {
		t.Fatalf("load manifest: %v", err)
	}
	if m.Runtime != wfdir.RuntimeQuery {
		t.Errorf("manifest runtime = %d, want %d recorded from the created row", m.Runtime, wfdir.RuntimeQuery)
	}

	// The point of recording it: the SECOND push is an ordinary no-op, not a
	// refusal about a runtime nobody chose to lower.
	writeLocal(t, dir, "main.py", "print('two')\n")
	if _, err := runCLI(t, dir, "wf", "push"); err != nil {
		t.Fatalf("the second push of a folder that never declared a runtime: %v", err)
	}
}

// An instance too old to report a runtime at create leaves the manifest exactly
// as it was — the write-back records a real answer, never a guess.
func TestCreateLeavesTheManifestAloneWhenTheServerReportsNoRuntime(t *testing.T) {
	f := newFakeInstance(t)
	signIn(t, f)
	dir := t.TempDir()

	if _, err := runCLI(t, dir, "wf", "init", "--feature", "feat-1"); err != nil {
		t.Fatalf("init: %v", err)
	}
	writeLocal(t, dir, "main.py", "print('one')\n")
	if _, err := runCLI(t, dir, "wf", "push"); err != nil {
		t.Fatalf("push: %v", err)
	}
	if raw := readFile(t, dir, wfdir.ManifestName); strings.Contains(raw, "runtime") {
		t.Errorf("a push invented a runtime key against an instance that reported none: %s", raw)
	}
}

// `--runtime 1` has to WRITE the key. An absent key means "the instance
// chooses", and its choice is 3 — so a folder that asked for the standard
// runtime and recorded nothing would be created on 3, the flag having done
// nothing at all.
func TestInitPinsAnExplicitStandardRuntime(t *testing.T) {
	f := newFakeInstance(t)
	signIn(t, f)
	dir := t.TempDir()

	if _, err := runCLI(t, dir, "wf", "init", "--feature", "feat-1", "--runtime", "1"); err != nil {
		t.Fatalf("init --runtime 1: %v", err)
	}
	if raw := readFile(t, dir, wfdir.ManifestName); !strings.Contains(raw, `"runtime": 1`) {
		t.Errorf("`--runtime 1` did not pin the runtime in the manifest: %s", raw)
	}
	// And it stays runtime 1: no scaffold, and nothing said about journaling.
	if _, err := os.Stat(filepath.Join(dir, "main.py")); !os.IsNotExist(err) {
		t.Error("the standard runtime scaffolded a durable main.py")
	}
}

// A clone records the runtime of the workflow it cloned — the STANDARD runtime
// included. The folder is committed and may be pushed into a new workflow
// somewhere else, where an absent key means "whatever this instance creates on",
// which is 3 and need not be what the cloned code was written for.
func TestCloneRecordsEvenTheStandardRuntime(t *testing.T) {
	f := newFakeInstance(t)
	f.AddWorkflow(&api.Workflow{ID: "wf-1", Lifecycle: api.LifecycleLive, RuntimeVersion: wfdir.RuntimeDefault},
		api.WorkflowFile{Path: "main.py", Content: "M\n"})
	signIn(t, f)

	root := cloneFolder(t, f, "wf-1")
	if raw := readFile(t, root, wfdir.ManifestName); !strings.Contains(raw, `"runtime": 1`) {
		t.Errorf("the clone did not record the runtime it cloned: %s", raw)
	}
}

// The absent-key half of this lives with the other clone tests, as
// TestCloneOfAWorkflowReportingNoRuntimeWritesNoKey.

// ── A folder that declares nothing declares NOTHING ─────────────────────────

// The absent `runtime` key is not a declaration of runtime 1, and the push path
// has to read it the same way everywhere. It did not: RuntimeForValidate read an
// absent key as "no opinion" and deferred to the row, while checkRuntimeDrift
// read it through RuntimeVersion(), which substitutes RuntimeDefault — so an
// unpinned folder bound to a runtime-3 workflow was refused as a DOWNGRADE on
// every push, forever, until somebody hand-edited ronja.json. Drift is checked
// before validate, so nothing else in the push ever got a look in.
func TestPushFromAFolderDeclaringNoRuntime(t *testing.T) {
	f := newFakeInstance(t)
	f.AddWorkflow(&api.Workflow{ID: "wf-1", Lifecycle: api.LifecycleLive, RuntimeVersion: wfdir.RuntimeQuery},
		api.WorkflowFile{Path: "main.py", Content: "M\n"})
	signIn(t, f)
	root := cloneFolder(t, f, "wf-1")
	// `wf clone` writes the key, so the state under test has to be made: this is
	// the folder somebody committed before the write-back existed, or wrote by
	// hand, or produced with an older CLI.
	setManifestRuntime(t, root, 0)
	writeLocal(t, root, "main.py", "M2\n")

	if _, err := runCLI(t, root, "wf", "push", "--json"); err != nil {
		t.Fatalf("push from an unpinned folder was refused: %v", err)
	}
	if len(f.fileWrites) == 0 {
		t.Fatal("the push wrote no files")
	}
	// Nothing to patch either: the folder asked for no runtime, so a push must
	// not quietly move the row in either direction.
	if len(f.runtimePatches) != 0 {
		t.Errorf("the push patched a runtime the folder never declared: %v", f.runtimePatches)
	}
	// And the rehearsal names the ROW's runtime, which is the runtime this code
	// is about to run on. RuntimeForValidate already did this; the assertion is
	// here so the two readings stay one reading.
	if len(f.validated) == 0 || f.validated[len(f.validated)-1].RuntimeVersion != wfdir.RuntimeQuery {
		t.Errorf("the validate did not rehearse the row's runtime: %+v", f.validated)
	}
}

// `wf status` answers "what would a push do", so it reads an absent key the same
// way: no declaration, nothing to disagree with, no refusal to announce.
func TestStatusReportsNoRuntimeDriftForAnUnpinnedFolder(t *testing.T) {
	f := newFakeInstance(t)
	f.AddWorkflow(&api.Workflow{ID: "wf-1", Lifecycle: api.LifecycleLive, RuntimeVersion: wfdir.RuntimeQuery},
		api.WorkflowFile{Path: "main.py", Content: "M\n"})
	signIn(t, f)
	root := cloneFolder(t, f, "wf-1")
	setManifestRuntime(t, root, 0)

	out, err := runCLI(t, root, "wf", "status", "--json")
	if err != nil {
		t.Fatalf("status: %v", err)
	}
	if got, ok := decodeJSON(t, out)["remote"].(map[string]any)["runtime"]; ok {
		t.Fatalf("status reported runtime drift against a folder that declares none: %v", got)
	}
}

// ── A write-back must never brick the folder it just wrote ──────────────────

// The day an instance's create default becomes a runtime this build has never
// heard of, a SUCCESSFUL push must not commit that number into ronja.json:
// LoadManifest refuses an unknown runtime, so the next command on the folder
// would hard-fail with nothing to do about it but upgrade the CLI or re-clone.
// An unknown runtime degrades to the behaviour that predates the write-back —
// record nothing — which leaves the folder unpinned and therefore workable.
func TestCreateDoesNotRecordARuntimeThisBuildCannotOpen(t *testing.T) {
	f := newFakeInstance(t)
	f.createRuntime = 99
	signIn(t, f)
	dir := t.TempDir()

	if _, err := runCLI(t, dir, "wf", "init", "--feature", "feat-1"); err != nil {
		t.Fatalf("init: %v", err)
	}
	writeLocal(t, dir, "main.py", "print('one')\n")
	if _, err := runCLI(t, dir, "wf", "push"); err != nil {
		t.Fatalf("push: %v", err)
	}

	if raw := readFile(t, dir, wfdir.ManifestName); strings.Contains(raw, "runtime") {
		t.Fatalf("the create wrote back a runtime this build cannot open: %s", raw)
	}
	// The point of not writing it: the folder still opens.
	if _, err := wfdir.LoadManifest(dir, wfdir.WorkflowKind); err != nil {
		t.Fatalf("the folder the push created cannot be opened again: %v", err)
	}
	if _, err := runCLI(t, dir, "wf", "status", "--json"); err != nil {
		t.Fatalf("status on the folder the push created: %v", err)
	}
}

// `wf clone` records the source's runtime for a different reason — identity, so
// the folder recreates the same runtime elsewhere — and the same bricking
// argument applies to it, so the same gate does.
func TestCloneDoesNotRecordARuntimeThisBuildCannotOpen(t *testing.T) {
	f := newFakeInstance(t)
	f.AddWorkflow(&api.Workflow{ID: "wf-1", Lifecycle: api.LifecycleLive, RuntimeVersion: 99},
		api.WorkflowFile{Path: "main.py", Content: "M\n"})
	signIn(t, f)
	root := cloneFolder(t, f, "wf-1")

	if raw := readFile(t, root, wfdir.ManifestName); strings.Contains(raw, "runtime") {
		t.Fatalf("the clone recorded a runtime this build cannot open: %s", raw)
	}
	if _, err := wfdir.LoadManifest(root, wfdir.WorkflowKind); err != nil {
		t.Fatalf("the folder the clone created cannot be opened again: %v", err)
	}
}
