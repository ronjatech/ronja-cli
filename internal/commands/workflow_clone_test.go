package commands

import (
	"testing"

	"github.com/ronjatech/ronja-cli/internal/api"
	"github.com/ronjatech/ronja-cli/internal/wfdir"
)

// A pushed clone may create a NEW workflow elsewhere, and an absent runtime key
// leaves that create on the instance's default; only a row reporting no runtime
// leaves it absent.
func TestCloneRecordsTheDurableRuntimeInTheManifest(t *testing.T) {
	f := newFakeInstance(t)
	signIn(t, f)
	f.AddWorkflow(&api.Workflow{
		ID: "wf-durable", Lifecycle: api.LifecycleLive, Title: "Durable pipeline",
		Entrypoint: "main.py", FeatureID: "feat-1", RuntimeVersion: 2,
	}, api.WorkflowFile{Path: "main.py", Content: "print('hi')\n"})

	out, err := runCLI(t, t.TempDir(), "wf", "clone", "wf-durable", "--json")
	if err != nil {
		t.Fatalf("clone: %v", err)
	}
	payload := decodeJSON(t, out)
	root := payload["root"].(string)
	if got := payload["runtimeVersion"].(float64); got != 2 {
		t.Errorf("json runtimeVersion = %v, want 2", got)
	}

	manifest, err := wfdir.LoadManifest(root, wfdir.WorkflowKind)
	if err != nil {
		t.Fatalf("load manifest: %v", err)
	}
	if got := manifest.RuntimeVersion(); got != wfdir.RuntimeDurable {
		t.Errorf("manifest runtime = %d, want %d — a repointed push of this folder would create a v1 workflow around v2 code", got, wfdir.RuntimeDurable)
	}
}

// An older instance has told us nothing, so the folder must not guess. Asserted
// on the raw field, since the accessor defaults absent to 1.
func TestCloneOfAWorkflowReportingNoRuntimeWritesNoKey(t *testing.T) {
	f := newFakeInstance(t)
	signIn(t, f)
	f.AddWorkflow(&api.Workflow{
		ID: "wf-standard", Lifecycle: api.LifecycleLive, Title: "Plain report",
		Entrypoint: "main.py", FeatureID: "feat-1",
	}, api.WorkflowFile{Path: "main.py", Content: "print('hi')\n"})

	out, err := runCLI(t, t.TempDir(), "wf", "clone", "wf-standard", "--json")
	if err != nil {
		t.Fatalf("clone: %v", err)
	}
	root := decodeJSON(t, out)["root"].(string)

	manifest, err := wfdir.LoadManifest(root, wfdir.WorkflowKind)
	if err != nil {
		t.Fatalf("load manifest: %v", err)
	}
	if manifest.Runtime != 0 {
		t.Errorf("manifest.Runtime = %d, want 0 (key absent) for the standard runtime", manifest.Runtime)
	}
}
