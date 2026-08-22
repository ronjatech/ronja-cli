package commands

import (
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
