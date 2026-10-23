package commands

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/ronjatech/ronja-cli/internal/markers"
	"github.com/ronjatech/ronja-cli/internal/wfdir"
)

// A DOCS SIDECAR IS A WRITE. `tables/<alias>.json` writes its table's
// description and column prose, and in an adopted folder the stack may write
// only its own feature — so a sidecar whose table lives in another feature is
// refused writes_outside_target unless the stack binds it as a write-capable
// data source ("write": true). Refused before anything is written by a
// single-folder push, by `sync apply`, and by its dry run.
func TestADocsSidecarOutsideTheStacksFeatureIsGated(t *testing.T) {
	fx := pipelineIdempotenceFixture(t)
	adoptFixture(t, fx)
	if code, line, _, out := applyOnce(t, fx); code != syncExitClean {
		t.Fatalf("first apply: exit %d %+v\n%s", code, line, out)
	}
	// The documented table is somebody else's: it lives in another feature.
	f := fx.pipeline
	f.mu.Lock()
	f.AddFeature("collection-other", "Finance", "organization")
	f.tables["table-raw"].FeatureID = "collection-other"
	f.mu.Unlock()
	writeSidecar(t, fx.root, "fortnox_invoices", `{"description": "Rewritten from the repository.", "columns": {"amount": "line total"}}`)
	docsWrites := func() []string {
		var out []string
		for _, w := range writesIn(fx.requests()) {
			if strings.Contains(w, "table-raw") {
				out = append(out, w)
			}
		}
		return out
	}

	fx.resetRequests()
	_, stderr, err := runPipelineCLI(t, fx.root, "pipeline", "push")
	if err == nil || !strings.Contains(err.Error(), "documents the table table-raw") || !strings.Contains(err.Error(), "collection-other") {
		t.Fatalf("push: the sidecar outside the stack's feature was not refused: %v\n%s", err, stderr)
	}
	if w := docsWrites(); len(w) != 0 {
		t.Fatalf("the refused push sent %v", w)
	}

	fx.resetRequests()
	out, _ := runCLI(t, fx.tree, "sync", "apply", "--stack", "prod", "--dry-run", "--json")
	if line := applyFolderLine(t, applyReportOf(t, out), filepath.Base(fx.root)); line.Reason != syncReasonWritesOutsideTarget {
		t.Fatalf("dry run: %s / %s — %s", line.Verdict, line.Reason, line.Detail)
	}
	code, line, _, out := applyOnce(t, fx)
	if code == syncExitClean || line.Reason != syncReasonWritesOutsideTarget {
		t.Fatalf("apply: exit %d, %s / %s — %s\n%s", code, line.Verdict, line.Reason, line.Detail, out)
	}
	if w := docsWrites(); len(w) != 0 {
		t.Fatalf("the refused apply sent %v", w)
	}

	// Bound on purpose as a write source of this stack: admitted, with a note.
	m, err := wfdir.LoadManifest(fx.root, wfdir.PipelineKind)
	if err != nil {
		t.Fatal(err)
	}
	m.Dependencies["fortnox_invoices"] = wfdir.Dependency{Kind: "table", Write: true}
	if err := wfdir.SaveManifest(fx.root, m); err != nil {
		t.Fatal(err)
	}
	fx.resetRequests()
	_, stderr, err = runPipelineCLI(t, fx.root, "pipeline", "push")
	if err != nil {
		t.Fatalf("push with the sidecar's table bound as a write source: %v\n%s", err, stderr)
	}
	if !strings.Contains(stderr, "outside") || !strings.Contains(stderr, `"fortnox_invoices" is bound for this stack on purpose`) {
		t.Fatalf("the admitted write outside the feature was not noted:\n%s", stderr)
	}
	if got := f.tables["table-raw"].Description; got != "Rewritten from the repository." {
		t.Fatalf("the admitted sidecar was not written: description = %q", got)
	}
}

// A classic folder (no folder id) is checked exactly as before: its sidecar is
// written wherever its table lives.
func TestADocsSidecarInAClassicFolderIsNotGated(t *testing.T) {
	fx := pipelineIdempotenceFixture(t)
	f := fx.pipeline
	f.mu.Lock()
	f.AddFeature("collection-other", "Finance", "organization")
	f.tables["table-raw"].FeatureID = "collection-other"
	f.mu.Unlock()
	writeSidecar(t, fx.root, "fortnox_invoices", `{"description": "Rewritten from the repository.", "columns": {"amount": "line total"}}`)
	if _, stderr, err := runPipelineCLI(t, fx.root, "pipeline", "push"); err != nil {
		t.Fatalf("classic push: %v\n%s", err, stderr)
	}
	if got := f.tables["table-raw"].Description; got != "Rewritten from the repository." {
		t.Fatalf("description = %q", got)
	}
}

// A table named by address in a WORKFLOW folder ("from" that folder,
// "address" its write target or output) is a READ of what that workflow
// writes: it confers no authority to write the table. A docs sidecar on it is
// gated like a sidecar on any other table — admitted in the stack's own
// feature, or bound by this folder's own write source — and refused
// writes_outside_target otherwise. It was admitted as a row this repository
// manages, so a stack whose workflow writes another team's table rewrote that
// table's prose, without even the note an outside write earns.
func TestADocsSidecarOnAWorkflowsTableByAddressIsGated(t *testing.T) {
	fx := pipelineIdempotenceFixture(t)
	adoptFixture(t, fx)
	if code, line, _, out := applyOnce(t, fx); code != syncExitClean {
		t.Fatalf("first apply: exit %d %+v\n%s", code, line, out)
	}
	f := fx.pipeline
	f.mu.Lock()
	f.AddFeature("collection-other", "Finance", "organization")
	f.tables["table-raw"].FeatureID = "collection-other"
	f.mu.Unlock()

	// The workflow folder writes the finance team's table on prod, bound there
	// on purpose. It sits in the same repository, outside the synced tree.
	repo := filepath.Dir(fx.tree)
	if err := os.Mkdir(filepath.Join(repo, ".git"), 0o755); err != nil {
		t.Fatal(err)
	}
	m := pipelineManifestOf(t, fx.root)
	prod := m.Stacks["prod"]
	producer := filepath.Join(repo, "compute")
	if err := os.MkdirAll(producer, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := wfdir.SaveFolder(producer, &wfdir.Manifest{
		Kind: wfdir.KindWorkflow, ID: producerFolderID, Title: "Compute", Entrypoint: "main.py",
		Dependencies: map[string]wfdir.Dependency{"ledger": {Kind: markers.KindTable, Write: true}},
		Stacks: map[string]wfdir.Stack{"prod": {URL: prod.URL, TenantID: prod.TenantID, FeatureID: prod.FeatureID,
			Bind: map[string]string{"ledger": "table-raw"}}},
	}, &wfdir.Lock{}); err != nil {
		t.Fatal(err)
	}
	// The pipeline names that table by address, and documents it.
	m.Dependencies["fortnox_invoices"] = wfdir.Dependency{Kind: markers.KindTable, From: "../../compute", Address: "ledger"}
	delete(prod.Bind, "fortnox_invoices")
	m.Stacks["prod"] = prod
	if err := wfdir.SaveManifest(fx.root, m); err != nil {
		t.Fatal(err)
	}
	writeSidecar(t, fx.root, "fortnox_invoices", `{"description": "Rewritten from the repository.", "columns": {"amount": "line total"}}`)

	fx.resetRequests()
	_, stderr, err := runPipelineCLI(t, fx.root, "pipeline", "push", "--stack", "prod")
	if err == nil || !strings.Contains(err.Error(), "documents the table table-raw") || !strings.Contains(err.Error(), "collection-other") {
		t.Fatalf("push: the sidecar on another feature's table was not refused: %v\n%s", err, stderr)
	}
	out, _ := runCLI(t, fx.tree, "sync", "apply", "--stack", "prod", "--dry-run", "--json")
	if line := applyFolderLine(t, applyReportOf(t, out), filepath.Base(fx.root)); line.Reason != syncReasonWritesOutsideTarget {
		t.Fatalf("dry run: %s / %s — %s", line.Verdict, line.Reason, line.Detail)
	}
	code, line, _, out := applyOnce(t, fx)
	if code == syncExitClean || line.Reason != syncReasonWritesOutsideTarget {
		t.Fatalf("apply: exit %d, %s / %s — %s\n%s", code, line.Verdict, line.Reason, line.Detail, out)
	}
	for _, w := range writesIn(fx.requests()) {
		if strings.Contains(w, "table-raw") {
			t.Fatalf("the refused apply sent %s", w)
		}
	}
	if got := f.tables["table-raw"].Description; got != "One row per invoice line." {
		t.Fatalf("the finance table's description was rewritten: %q", got)
	}

	// In the stack's own feature, the same sidecar is admitted.
	f.mu.Lock()
	f.tables["table-raw"].FeatureID = prod.FeatureID
	f.mu.Unlock()
	if code, line, _, out := applyOnce(t, fx); code != syncExitClean {
		t.Fatalf("apply in the stack's own feature: exit %d %+v\n%s", code, line, out)
	}
	if got := f.tables["table-raw"].Description; got != "Rewritten from the repository." {
		t.Fatalf("the sidecar was not written in the stack's own feature: %q", got)
	}
}
