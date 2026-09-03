package commands

import (
	"testing"

	"github.com/ronjatech/ronja-cli/internal/api"
	"github.com/ronjatech/ronja-cli/internal/wfdir"
)

// A clone of a Durable workflow must record the runtime in the manifest: the
// folder may later be pushed into a NEW workflow elsewhere, and an absent key
// leaves that create on whatever runtime the instance defaults to — which is not
// the one this code was written for, in either direction. The key is written for
// every runtime the instance actually reports, the standard one included; only a
// row that reports NOTHING leaves it absent.
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

// Negative control: a row that reports NO runtime writes no key. An instance
// too old to have the field has told us nothing, and a folder must not declare a
// guess on its behalf. Asserting on the raw field (not the accessor, which
// defaults absent to 1) is what would catch a regression to always-writing.
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
