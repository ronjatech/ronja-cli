package commands

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/ronjatech/ronja-cli/internal/wfdir"
)

// The pipeline loop's notes and refusals that send somebody to a binding by
// hand have to name the file the binding is IN. On a named stack that is
// ronja.lock.json — the folder's ronja.json carries no "tables" at all — and the
// messages used to say ronja.json regardless, so the repair landed in a key
// nothing reads.

// A file gone from a STACK folder: the note names the lock, not the manifest.
func TestPipelinePushGoneFileNoteNamesTheLockOnAStack(t *testing.T) {
	f := newFakePipelineInstance(t)
	signInPipeline(t, f)
	seedFeature(f, "private")
	_, root := applyPipelineTree(t, f,
		map[string]string{"orders.sql": "SELECT * FROM raw WHERE shipped"},
		map[string]string{"orders.sql": "table-orders"}, nil)
	if out, err := runCLI(t, root, "pipeline", "push", "--stack", "prod", "--json"); err != nil {
		t.Fatalf("first push: %v\n%s", err, out)
	}
	if err := os.Remove(filepath.Join(root, "orders.sql")); err != nil {
		t.Fatal(err)
	}
	stderr := captureStderr(t, func() {
		_, _ = runCLI(t, root, "pipeline", "push", "--stack", "prod", "--json")
	})
	if !strings.Contains(stderr, "orders.sql is bound but gone") {
		t.Fatalf("no gone-file note:\n%s", stderr)
	}
	if !strings.Contains(stderr, wfdir.LockName) || strings.Contains(stderr, `"tables" in `+wfdir.ManifestName) {
		t.Errorf("the note does not send the reader to %s:\n%s", wfdir.LockName, stderr)
	}
}

// A create refused for want of the admin role, on a STACK folder: the advice
// to bind the table by hand names the lock and the lock's shape.
func TestPipelinePushCreateRefusalNamesTheLockOnAStack(t *testing.T) {
	f := newFakePipelineInstance(t)
	signInPipeline(t, f)
	seedFeature(f, "private")
	f.failCreate = 403
	_, root := applyPipelineTree(t, f,
		map[string]string{"refunds.sql": "SELECT * FROM raw WHERE refunded"}, nil, nil)

	out, _ := runCLI(t, root, "pipeline", "push", "--stack", "prod", "--json")
	result := &pipelinePushResult{}
	decodeJSONInto(t, out, result)
	if len(result.Files) != 1 || result.Files[0].Outcome != pushOutcomeRefused {
		t.Fatalf("files = %+v", result.Files)
	}
	msg := result.Files[0].Error
	if !strings.Contains(msg, wfdir.LockName) || !strings.Contains(msg, `"tableID"`) || !strings.Contains(msg, `"prod"`) {
		t.Errorf("the refusal does not say where a stack folder's binding goes: %q", msg)
	}
	if strings.Contains(msg, `"tables" in `+wfdir.ManifestName) {
		t.Errorf("the refusal still sends the reader to %s: %q", wfdir.ManifestName, msg)
	}
}

// And a LEGACY folder keeps the manifest wording, which is still true there.
func TestPipelinePushCreateRefusalNamesTheManifestOnALegacyFolder(t *testing.T) {
	f := newFakePipelineInstance(t)
	signInPipeline(t, f)
	seedFeature(f, "private")
	f.failCreate = 403
	root := t.TempDir()
	manifest := &wfdir.Manifest{Kind: wfdir.KindPipeline, Title: "Sales pipeline"}
	manifest.SetBinding(f.Key(), wfdir.Binding{FeatureID: "collection-1"})
	writePipelineFolder(t, root, manifest, map[string]string{"refunds.sql": "SELECT 1"})

	out, _ := runCLI(t, root, "pipeline", "push", "--json")
	result := &pipelinePushResult{}
	decodeJSONInto(t, out, result)
	if len(result.Files) != 1 || !strings.Contains(result.Files[0].Error, `"tables" in `+wfdir.ManifestName) {
		t.Errorf("files = %+v", result.Files)
	}
}
