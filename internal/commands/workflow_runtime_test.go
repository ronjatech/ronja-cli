package commands

import (
	"net/http"
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
// The server now takes a ONE-WAY upgrade, to 3 only (runtime 1 and 2 are
// retired), which gives the folder three jobs: carry the raise to 3 up as its
// own request after the files land, REFUSE the downgrade before it writes
// anything, and refuse the moves onto a retired runtime — a create on 1 or 2, a
// raise from 1 to 2 — just as early. All are here.

// setManifestRuntime marks a folder as declaring a runtime, the way a
// hand-edited ronja.json (or `wf clone` of a legacy workflow) does.
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

// The upgrade: the folder says 3, the row says 1, and the push carries it. This
// is the whole feature from the CLI's side.
func TestPushSendsTheRuntimeUpgrade(t *testing.T) {
	f := newFakeInstance(t)
	f.AddWorkflow(&api.Workflow{ID: "wf-1", Lifecycle: api.LifecycleLive, RuntimeVersion: wfdir.RuntimeDefault},
		api.WorkflowFile{Path: "main.py", Content: "M\n"})
	signIn(t, f)
	root := cloneFolder(t, f, "wf-1")
	setManifestRuntime(t, root, wfdir.RuntimeQuery)
	writeLocal(t, root, "main.py", "M2\n")

	out, err := runCLI(t, root, "wf", "push", "--json")
	if err != nil {
		t.Fatalf("push: %v", err)
	}
	// The push targets the DRAFT it checked out, which is where the upgrade has
	// to land: committing it is what publishes the flip onto the live workflow,
	// with the code change that needs it.
	draftID := decodeJSON(t, out)["draftID"].(string)
	if got := f.runtimePatches[draftID]; got != wfdir.RuntimeQuery {
		t.Fatalf("runtimeVersion patched on %s = %d, want 3 (patches: %v)", draftID, got, f.runtimePatches)
	}
	report := decodeJSON(t, out)
	if got := report["runtimeUpgradedFrom"]; got != float64(wfdir.RuntimeDefault) {
		t.Errorf("runtimeUpgradedFrom = %v, want 1", got)
	}
	if got := report["runtimeVersion"]; got != float64(wfdir.RuntimeQuery) {
		t.Errorf("runtimeVersion = %v, want 3", got)
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
		setManifestRuntime(t, root, wfdir.RuntimeQuery)

		out, err := runCLI(t, root, "wf", "status", "--json")
		if err != nil {
			t.Fatalf("status: %v", err)
		}
		runtime := decodeJSON(t, out)["remote"].(map[string]any)["runtime"].(map[string]any)
		if runtime["local"] != float64(3) || runtime["remote"] != float64(1) {
			t.Fatalf("runtime report = %v, want local 3 / remote 1", runtime)
		}
		if _, refused := runtime["refused"]; refused {
			t.Errorf("an upgrade is reported as refused: %v", runtime)
		}
	})

	// 1 → 2 is not an upgrade a push makes any more: runtime 2 is retired, and
	// status must not call a folder pushable that the push then refuses.
	t.Run("a raise to retired 2 a push would refuse", func(t *testing.T) {
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
		if runtime["refused"] != true {
			t.Fatalf("a raise to retired runtime 2 is not reported as refused: %v", runtime)
		}
		text, err := runCLI(t, root, "wf", "status")
		if err != nil {
			t.Fatalf("status: %v", err)
		}
		if !strings.Contains(text, "refused   runtime: 2 here, 1 there — runtime 1 and 2 are retired, so the only upgrade is to 3; a push is refused") {
			t.Errorf("status does not say why the raise is refused:\n%s", text)
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

// The create summary says what the runtime it just stamped MEANS: v3's line
// states where a table comes from, and is not v2's resume sentence (a workflow
// can no longer be created on 2, but the label table still describes one).
func TestCreateSummaryDescribesTheRuntimeItStamped(t *testing.T) {
	const (
		durableLine = "    runtime  2 — steps are journaled; a failed run resumes with `ronja wf test --resume`\n"
		queryLine   = "    runtime  3 — steps are journaled; tables are read only through `tools.query` — the container holds no table credential\n"
	)

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
	const rule = "#\n# Read a Ronja table ONLY with tools.query(\"… FROM {{ ref('table-…') }} …\") — the\n" +
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

// Runtime 1 and 2 are retired, so `wf init` refuses to set up a folder that
// could never be created — in the instance's words, before a byte is written —
// while its own default is 3, so a plain init never meets the refusal.
func TestInitRefusesARetiredRuntime(t *testing.T) {
	f := newFakeInstance(t)
	signIn(t, f)

	for _, value := range []string{"1", "2"} {
		dir := t.TempDir()
		_, err := runCLI(t, dir, "wf", "init", "--feature", "feat-1", "--runtime", value)
		if err == nil {
			t.Fatalf("init accepted --runtime %s", value)
		}
		for _, want := range []string{"runtime 1 and 2 are retired", "Omit --runtime (new workflows are runtime 3)", "tools.query"} {
			if !strings.Contains(err.Error(), want) {
				t.Errorf("--runtime %s refusal does not say %q: %v", value, want, err)
			}
		}
		if _, statErr := os.Stat(filepath.Join(dir, wfdir.ManifestName)); !os.IsNotExist(statErr) {
			t.Errorf("--runtime %s: a manifest was written despite the refusal", value)
		}
		if _, statErr := os.Stat(filepath.Join(dir, "main.py")); !os.IsNotExist(statErr) {
			t.Errorf("--runtime %s: a scaffold was written despite the refusal", value)
		}
	}
}

// The flag's own default is 3 (B2): a check that ran on the flag's VALUE
// whether or not it was passed refused a plain `wf init` when the default was
// 1. A plain init still writes no runtime key — the instance chooses, and its
// choice is 3 — and the help says 3.
func TestInitDefaultsToRuntimeThree(t *testing.T) {
	f := newFakeInstance(t)
	signIn(t, f)
	dir := t.TempDir()

	out, err := runCLI(t, dir, "wf", "init", "--feature", "feat-1", "--json")
	if err != nil {
		t.Fatalf("a plain init was refused: %v", err)
	}
	if got := decodeJSON(t, out)["runtime"]; got != float64(wfdir.RuntimeQuery) {
		t.Errorf("a plain init reports runtime %v, want 3", got)
	}
	if raw := readFile(t, dir, wfdir.ManifestName); strings.Contains(raw, `"runtime"`) {
		t.Errorf("a plain init pinned a runtime the author never named: %s", raw)
	}
	// What --help prints as "(default N)" is the flag's DefValue.
	if def := newWorkflowInitCmd().Flags().Lookup("runtime").DefValue; def != "3" {
		t.Errorf("--runtime's default is %s, want 3 — --help would show it", def)
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

// ── Runtime 1 and 2 are retired ─────────────────────────────────────────────
//
// The instance refuses a create on 1 or 2 and a raise from 1 to 2, and checks a
// raise to 3 against the file set the row holds when the raise arrives. So the
// folder refuses the first two before it writes anything, and sends the raise
// ALONE and LAST — after the files, the metadata and the deletions.

// The raise is its own PUT, after the deletions: sent with the metadata, before
// a legacy file this push deletes was gone, the server's v3 check would read it.
func TestPushSendsTheRuntimeRaiseAloneAfterTheDeletes(t *testing.T) {
	f := newFakeInstance(t)
	f.AddWorkflow(&api.Workflow{ID: "wf-1", Title: "Old", Lifecycle: api.LifecycleLive, RuntimeVersion: wfdir.RuntimeDefault},
		api.WorkflowFile{Path: "main.py", Content: "M\n"},
		api.WorkflowFile{Path: "legacy.py", Content: "L\n"})
	signIn(t, f)
	root := cloneFolder(t, f, "wf-1")
	setManifestRuntime(t, root, wfdir.RuntimeQuery)
	m, err := wfdir.LoadManifest(root, wfdir.WorkflowKind)
	if err != nil {
		t.Fatalf("load manifest: %v", err)
	}
	m.Title = "New"
	if err := wfdir.SaveManifest(root, m); err != nil {
		t.Fatalf("save manifest: %v", err)
	}
	writeLocal(t, root, "main.py", "M2\n")
	if err := os.Remove(filepath.Join(root, "legacy.py")); err != nil {
		t.Fatal(err)
	}

	out, err := runCLI(t, root, "wf", "push", "--json")
	if err != nil {
		t.Fatalf("push: %v", err)
	}
	draftID := decodeJSON(t, out)["draftID"].(string)

	if len(f.workflowPatches) != 2 {
		t.Fatalf("push sent %d PUT :id, want 2 (the metadata, then the raise): %+v", len(f.workflowPatches), f.workflowPatches)
	}
	if meta := f.workflowPatches[0]; meta.Title != "New" || meta.RuntimeVersion != 0 {
		t.Errorf("the metadata PUT = %+v, want the title and no runtime", meta)
	}
	if raise := f.workflowPatches[1]; raise != (api.WorkflowPatch{RuntimeVersion: wfdir.RuntimeQuery}) {
		t.Errorf("the raise PUT = %+v, want runtimeVersion 3 and nothing else", raise)
	}
	lastDelete, lastPut := -1, -1
	for i, req := range f.Requests {
		switch req {
		case "DELETE /api/v2/workflow/" + draftID + "/files/legacy.py":
			lastDelete = i
		case "PUT /api/v2/workflow/" + draftID:
			lastPut = i
		}
	}
	if lastDelete < 0 || lastPut < lastDelete {
		t.Errorf("the raise was not sent after the deletion (delete at %d, last PUT :id at %d):\n%s",
			lastDelete, lastPut, strings.Join(f.Requests, "\n"))
	}
}

// B3: a push whose ONLY change is the raise is not "up to date". Before the
// raise had its own request, the up-to-date check asked only the metadata patch.
func TestARuntimeOnlyPushIsNotUpToDate(t *testing.T) {
	f := newFakeInstance(t)
	f.AddWorkflow(&api.Workflow{ID: "wf-1", Lifecycle: api.LifecycleLive, RuntimeVersion: wfdir.RuntimeDurable},
		api.WorkflowFile{Path: "main.py", Content: "M\n"})
	signIn(t, f)
	root := cloneFolder(t, f, "wf-1")
	// A first push checks the draft out, so the second one starts clean.
	writeLocal(t, root, "main.py", "M2\n")
	if _, err := runCLI(t, root, "wf", "push"); err != nil {
		t.Fatalf("first push: %v", err)
	}
	setManifestRuntime(t, root, wfdir.RuntimeQuery)

	out, err := runCLI(t, root, "wf", "push", "--json")
	if err != nil {
		t.Fatalf("runtime-only push: %v", err)
	}
	report := decodeJSON(t, out)
	if report["upToDate"] == true {
		t.Errorf("a push that raised the runtime reported itself up to date: %v", report)
	}
	if report["runtimeVersion"] != float64(wfdir.RuntimeQuery) {
		t.Errorf("runtimeVersion = %v, want 3", report["runtimeVersion"])
	}
	draftID := report["draftID"].(string)
	if f.runtimePatches[draftID] != wfdir.RuntimeQuery {
		t.Errorf("the raise was not sent: %v", f.runtimePatches)
	}
}

// A refused raise stops the push with the files it landed recorded, and the
// retry sends the raise and nothing else — the baseline records no runtime, so
// nothing there claims a raise that never happened.
func TestARefusedRuntimeRaiseIsRetriedAlone(t *testing.T) {
	f := newFakeInstance(t)
	f.AddWorkflow(&api.Workflow{ID: "wf-1", Lifecycle: api.LifecycleLive, RuntimeVersion: wfdir.RuntimeDefault},
		api.WorkflowFile{Path: "main.py", Content: "M\n"},
		api.WorkflowFile{Path: "legacy.py", Content: "L\n"})
	signIn(t, f)
	root := cloneFolder(t, f, "wf-1")
	setManifestRuntime(t, root, wfdir.RuntimeQuery)
	writeLocal(t, root, "main.py", "M2\n")
	if err := os.Remove(filepath.Join(root, "legacy.py")); err != nil {
		t.Fatal(err)
	}

	f.failRuntimePatch = http.StatusBadRequest
	if _, err := runCLI(t, root, "wf", "push"); err == nil {
		t.Fatal("a push whose raise was refused exited zero")
	} else if !strings.Contains(err.Error(), "the raise was refused") {
		t.Errorf("the push does not report the server's refusal: %v", err)
	}
	if len(f.runtimePatches) != 0 {
		t.Fatalf("the refused raise was recorded as applied: %v", f.runtimePatches)
	}

	f.failRuntimePatch = 0
	f.fileWrites = nil
	f.workflowPatches = nil
	out, err := runCLI(t, root, "wf", "push", "--json")
	if err != nil {
		t.Fatalf("retry: %v", err)
	}
	if len(f.fileWrites) != 0 {
		t.Errorf("the retry re-sent files the first push landed — its baseline did not record them: %+v", f.fileWrites)
	}
	if len(f.workflowPatches) != 1 || f.workflowPatches[0] != (api.WorkflowPatch{RuntimeVersion: wfdir.RuntimeQuery}) {
		t.Errorf("the retry sent %+v, want the raise alone", f.workflowPatches)
	}
	if got := decodeJSON(t, out)["runtimeUpgradedFrom"]; got != float64(wfdir.RuntimeDefault) {
		t.Errorf("runtimeUpgradedFrom = %v, want 1", got)
	}
}

// 1 → 2 is refused by the folder, before a file is written: the instance would
// refuse the raise anyway, and refusing it there would leave the files landed.
func TestPushRefusesARaiseToARetiredRuntime(t *testing.T) {
	f := newFakeInstance(t)
	f.AddWorkflow(&api.Workflow{ID: "wf-1", Lifecycle: api.LifecycleLive, RuntimeVersion: wfdir.RuntimeDefault},
		api.WorkflowFile{Path: "main.py", Content: "M\n"})
	signIn(t, f)
	root := cloneFolder(t, f, "wf-1")
	setManifestRuntime(t, root, wfdir.RuntimeDurable)
	writeLocal(t, root, "main.py", "M2\n")

	_, err := runCLI(t, root, "wf", "push")
	if err == nil {
		t.Fatal("push accepted a raise from runtime 1 to retired runtime 2")
	}
	for _, want := range []string{"runtime 1 and 2 are retired", `Set "runtime": 3`, "tools.query"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("refusal missing %q: %v", want, err)
		}
	}
	if len(f.fileWrites) != 0 || len(f.workflowPatches) != 0 {
		t.Fatalf("the refusal came after a write: files %+v, patches %+v", f.fileWrites, f.workflowPatches)
	}
}

// A folder declaring 1 or 2 cannot CREATE a workflow — a hand-edited manifest,
// or a cloned legacy folder pushed somewhere it is not bound. Refused before the
// create, so no workflow is left behind for the push to have failed on.
func TestPushRefusesToCreateOnARetiredRuntime(t *testing.T) {
	for _, runtime := range []int{wfdir.RuntimeDefault, wfdir.RuntimeDurable} {
		f := newFakeInstance(t)
		signIn(t, f)
		dir := t.TempDir()
		if _, err := runCLI(t, dir, "wf", "init", "--feature", "feat-1"); err != nil {
			t.Fatalf("init: %v", err)
		}
		setManifestRuntime(t, dir, runtime)
		writeLocal(t, dir, "main.py", "print('x')\n")

		_, err := runCLI(t, dir, "wf", "push")
		if err == nil {
			t.Fatalf("push created a workflow on retired runtime %d", runtime)
		}
		for _, want := range []string{"runtime 1 and 2 are retired", "cannot be created", `Set "runtime": 3`} {
			if !strings.Contains(err.Error(), want) {
				t.Errorf("runtime %d refusal missing %q: %v", runtime, want, err)
			}
		}
		if len(f.created) != 0 || len(f.fileWrites) != 0 {
			t.Fatalf("runtime %d: the refusal came after a write: created %+v, files %+v", runtime, f.created, f.fileWrites)
		}
	}
}
