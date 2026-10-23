package commands

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/ronjatech/ronja-cli/internal/api"
	"github.com/ronjatech/ronja-cli/internal/wfdir"
)

// The FOLDER ANCHOR (backend rmanaged/folder.go): the docs-sidecar baselines of
// an adopted folder live on an anchor of their own, which depends on no claim.
// They used to ride inside the anchor of the folder's first-sorting claim (the
// "docs host"), and every test here is one of the four ways that lost them, or
// one of the folder anchor's own arms. Each asserts on what the FAKE SERVER
// holds and was sent.

// docsOnlyPipelineFolder is an adopted pipeline folder that builds nothing: a
// feature whose one table is an integration table, documented by a sidecar.
func docsOnlyPipelineFolder(t *testing.T) (*fakePipelineInstance, string, string) {
	t.Helper()
	f := newFakePipelineInstance(t)
	signInPipeline(t, f)
	f.AddFeature("collection-1", "Sales pipeline", "private")
	f.AddTable(&api.Table{ID: "table-raw", Name: "Fortnox invoices", FeatureID: "collection-1", Kind: "integration",
		Description: "One row per invoice line.", DescriptionSource: api.DescriptionSourceUser})
	f.fields["table-raw"] = []string{"invoice_no", "amount"}
	tree := t.TempDir()
	root := filepath.Join(tree, "docs")
	if err := os.MkdirAll(root, 0o755); err != nil {
		t.Fatal(err)
	}
	if _, stderr, err := runPipelineCLI(t, root, "pipeline", "init", "--feature", "collection-1", "--stack", "prod"); err != nil {
		t.Fatalf("init: %v\n%s", err, stderr)
	}
	manifest := pipelineManifestOf(t, root)
	if !manifest.Adopted() {
		t.Fatal("init --stack minted no folder id")
	}
	manifest.Dependencies = map[string]wfdir.Dependency{"fortnox_invoices": {Kind: "table"}}
	stack := manifest.Stacks["prod"]
	stack.Bind = map[string]string{"fortnox_invoices": "table-raw"}
	manifest.Stacks["prod"] = stack
	if err := wfdir.SaveManifest(root, manifest); err != nil {
		t.Fatal(err)
	}
	writeSidecar(t, root, "fortnox_invoices", `{"description": "One row per Fortnox invoice line.", "columns": {"amount": "line total"}}`)
	return f, tree, root
}

// applyTree runs one `sync apply --stack prod` over a tree and answers its exit
// code and the one folder's line.
func applyTree(t *testing.T, tree, root string, extra ...string) (int, syncApplyFolderReport, string) {
	t.Helper()
	out, err := runCLI(t, tree, append([]string{"sync", "apply", "--stack", "prod", "--json"}, extra...)...)
	line := applyFolderLine(t, applyReportOf(t, out), filepath.Base(root))
	return exitCodeOf(err), line, out
}

// folderDocsOf answers the sidecar paths a folder anchor records.
func folderDocsOf(t *testing.T, m *fakeManaged, feature, folder string) map[string]wfdir.LockTableDocs {
	t.Helper()
	a := m.FolderAnchor(feature, folder)
	if a == nil {
		return nil
	}
	docs, err := wfdir.TableDocsIn(a.Applied)
	if err != nil {
		t.Fatal(err)
	}
	return docs
}

// TestManagedDocsOnlyFolderKeepsItsDriftGuardWithoutALock: a folder that
// builds nothing has no claim at all, so there was no docs host to carry its
// baselines — a lock-less runner wrote its sidecar over prose edited in the
// app. The folder anchor depends on no claim.
func TestManagedDocsOnlyFolderKeepsItsDriftGuardWithoutALock(t *testing.T) {
	f, tree, root := docsOnlyPipelineFolder(t)
	removeLock(t, root)
	if code, line, out := applyTree(t, tree, root); code != syncExitClean {
		t.Fatalf("first lock-less apply: exit %d %+v\n%s", code, line, out)
	}
	if got := f.description("table-raw"); got != "One row per Fortnox invoice line." {
		t.Fatalf("the sidecar was not applied: %q", got)
	}
	if len(f.managed.Claims("collection-1")) != 0 {
		t.Fatalf("a docs-only folder claimed something: %+v", f.managed.Claims("collection-1"))
	}
	docs := folderDocsOf(t, f.managed, "collection-1", folderIDOf(t, root))
	if rec := docs["tables/fortnox_invoices.json"]; rec.TableID != "table-raw" || rec.MetaSHA256 == "" {
		t.Fatalf("the folder anchor records %+v", docs)
	}

	f.setDescription("table-raw", "Rewritten in the app.")
	writeSidecar(t, root, "fortnox_invoices", `{"description": "One row per invoice line, from Fortnox.", "columns": {"amount": "line total"}}`)
	removeLock(t, root)
	code, line, out := applyTree(t, tree, root)
	if code == syncExitClean {
		t.Fatalf("prose rewritten in the app, applied over on a lock-less run: %+v\n%s", line, out)
	}
	if got := f.description("table-raw"); got != "Rewritten in the app." {
		t.Fatalf("the app's prose was overwritten: %q", got)
	}
}

// TestManagedManySidecarsLoseNoBaseline: past about forty sidecars the docs
// host's anchor overflowed its 12 KiB budget, the records were left off it —
// and the merge then ERASED the server's copy. The folder anchor carries them
// all, and a lock-less run reads every one back.
func TestManagedManySidecarsLoseNoBaseline(t *testing.T) {
	const n = 60
	fx := pipelineIdempotenceFixture(t)
	f, root := fx.pipeline, fx.root
	manifest := pipelineManifestOf(t, root)
	stack := manifest.Stacks["prod"]
	last := ""
	for i := range n {
		alias := fmt.Sprintf("extra_%02d_with_a_long_enough_alias_to_be_realistic", i)
		id := fmt.Sprintf("table-extra-%02d", i)
		f.AddTable(&api.Table{ID: id, Name: alias, FeatureID: "collection-1", Kind: "integration", Description: "old"})
		f.fields[id] = []string{"amount"}
		manifest.Dependencies[alias] = wfdir.Dependency{Kind: "table"}
		stack.Bind[alias] = id
		writeSidecar(t, root, alias, fmt.Sprintf(`{"description": "Sidecar %d.", "columns": {"amount": "line total"}}`, i))
		last = id
	}
	manifest.Stacks["prod"] = stack
	if err := wfdir.SaveManifest(root, manifest); err != nil {
		t.Fatal(err)
	}
	adoptFixture(t, fx)
	removeLock(t, root)
	if code, line, _, out := applyOnce(t, fx); code != syncExitClean {
		t.Fatalf("apply: exit %d %+v\n%s", code, line, out)
	}
	docs := folderDocsOf(t, f.managed, "collection-1", folderIDOf(t, root))
	if len(docs) != n+1 {
		t.Fatalf("the folder anchor records %d sidecars, want %d", len(docs), n+1)
	}
	for p, rec := range docs {
		if rec.MetaSHA256 == "" || rec.DeclaredSHA256 == "" {
			t.Fatalf("%s recorded without a baseline: %+v", p, rec)
		}
	}
	// A lock-less re-run reads every one back: the last sidecar's table, edited
	// in the app, is refused rather than written over.
	f.setDescription(last, "Rewritten in the app.")
	removeLock(t, root)
	if code, line, _, out := applyOnce(t, fx); code == syncExitClean {
		t.Fatalf("prose rewritten in the app, applied over: %+v\n%s", line, out)
	}
	if got := f.description(last); got != "Rewritten in the app." {
		t.Fatalf("the app's prose was overwritten: %q", got)
	}
}

// TestManagedDocsBaselinesSurviveTheirFormerHost: the docs host was the
// folder's first-sorting claim. A host in a refusal state that the folder
// declares "removed" was released WITH the records on it, on a run with no lock
// to carry them over. A host refused a claim at adopt (unclaimable) left the
// records off every anchor. Neither touches the folder anchor.
func TestManagedDocsBaselinesSurviveTheirFormerHost(t *testing.T) {
	t.Run("the former host is trashed and removed", func(t *testing.T) {
		fx := pipelineIdempotenceFixture(t)
		adoptFixture(t, fx)
		if code, line, _, out := applyOnce(t, fx); code != syncExitClean {
			t.Fatalf("first apply: exit %d %+v\n%s", code, line, out)
		}
		folder := folderIDOf(t, fx.root)
		if docs := folderDocsOf(t, fx.pipeline.managed, "collection-1", folder); docs["tables/fortnox_invoices.json"].MetaSHA256 == "" {
			t.Fatalf("no baseline recorded: %+v", docs)
		}
		// metrics/aov.json sorts first: it was the docs host. It goes to the
		// trash, and the folder stops managing it.
		m := fx.pipeline.managed
		m.mu.Lock()
		m.trashed["metric-aov"] = true
		m.mu.Unlock()
		if err := os.Remove(filepath.Join(fx.root, "metrics", "aov.json")); err != nil {
			t.Fatal(err)
		}
		manifest := pipelineManifestOf(t, fx.root)
		manifest.Removed = []string{"metrics/aov.json"}
		if manifest.FormatVersion < 6 {
			manifest.FormatVersion = 6
		}
		if err := wfdir.SaveManifest(fx.root, manifest); err != nil {
			t.Fatal(err)
		}
		fx.pipeline.setDescription("table-raw", "Rewritten in the app.")
		removeLock(t, fx.root)
		if code, line, _, out := applyOnce(t, fx); code == syncExitClean {
			t.Fatalf("prose rewritten in the app, applied over once the host was released: %+v\n%s", line, out)
		}
		if got := fx.pipeline.description("table-raw"); got != "Rewritten in the app." {
			t.Fatalf("the app's prose was overwritten: %q", got)
		}
		if m.ClaimAt("collection-1", folder, "metrics/aov.json") != nil {
			t.Fatal("the removed address is still claimed")
		}
		if docs := folderDocsOf(t, m, "collection-1", folder); docs["tables/fortnox_invoices.json"].MetaSHA256 == "" {
			t.Fatalf("the docs baseline went with its former host: %+v", docs)
		}
	})
	t.Run("the first address cannot be claimed", func(t *testing.T) {
		fx := pipelineIdempotenceFixture(t)
		// A classic deploy first, so the lock records the sidecar's baseline.
		if code, line, _, out := applyOnce(t, fx); code != syncExitClean {
			t.Fatalf("classic apply: exit %d %+v\n%s", code, line, out)
		}
		if id, _, _ := mustLoadLock(t, fx.root).TableDocsSeen("prod", "tables/fortnox_invoices.json"); id != "table-raw" {
			t.Fatal("the classic apply recorded no docs baseline")
		}
		// The metric — the first address, the old host — is already managed
		// from another folder, so adopt refuses its claim and skips its anchor.
		fx.pipeline.managed.stage("collection-1", "00000000-0000-4000-8000-000000000001", "metrics/aov.json", "metric", "metric-aov", "")
		out, _ := runCLI(t, fx.tree, "sync", "adopt", "--stack", "prod", "--json")
		if !strings.Contains(out, "metrics/aov.json") {
			t.Fatalf("adopt did not report the refused address:\n%s", out)
		}
		folder := folderIDOf(t, fx.root)
		if docs := folderDocsOf(t, fx.pipeline.managed, "collection-1", folder); docs["tables/fortnox_invoices.json"].TableID != "table-raw" {
			t.Fatalf("the docs baseline was not recorded beside a skipped first address: %+v", docs)
		}
		if c := fx.pipeline.managed.ClaimAt("collection-1", folder, "orders.sql"); c == nil || c.Applied[wfdir.LegacyDocsAnchorKey] != nil {
			t.Fatalf("the next claim's anchor = %+v", c)
		}
	})
}

// TestSyncUnmanageReturnsTheDocsBaselinesToTheLock: the lock is the classic
// pattern's only state. Unmanage writes the folder anchor's records into it
// before releasing anything, then releases the folder anchor too.
func TestSyncUnmanageReturnsTheDocsBaselinesToTheLock(t *testing.T) {
	f, tree, root := docsOnlyPipelineFolder(t)
	removeLock(t, root)
	if code, line, out := applyTree(t, tree, root); code != syncExitClean {
		t.Fatalf("apply: exit %d %+v\n%s", code, line, out)
	}
	folder := folderIDOf(t, root)
	want := folderDocsOf(t, f.managed, "collection-1", folder)["tables/fortnox_invoices.json"]
	removeLock(t, root)
	if out, err := runCLI(t, tree, "sync", "unmanage", "--stack", "prod", "--json"); err != nil {
		t.Fatalf("unmanage: %v\n%s", err, out)
	}
	if id, meta, declared := mustLoadLock(t, root).TableDocsSeen("prod", "tables/fortnox_invoices.json"); id != want.TableID || meta != want.MetaSHA256 || declared != want.DeclaredSHA256 {
		t.Fatalf("the lock records %s %s %s, want %+v", id, meta, declared, want)
	}
	if f.managed.FolderAnchor("collection-1", folder) != nil {
		t.Fatal("the folder anchor was not released")
	}
	// The classic pattern now holds the guard.
	f.setDescription("table-raw", "Rewritten in the app.")
	if code, line, out := applyTree(t, tree, root); code == syncExitClean {
		t.Fatalf("prose rewritten in the app, applied over after unmanage: %+v\n%s", line, out)
	}
}

// TestManagedFolderAnchorArms: the CAS's two 409s and the 413.
func TestManagedFolderAnchorArms(t *testing.T) {
	t.Run("a revision that moved with the state unchanged is retried", func(t *testing.T) {
		f, tree, root := docsOnlyPipelineFolder(t)
		folder := folderIDOf(t, root)
		f.managed.mu.Lock()
		f.managed.folders[fakeFolderKey("collection-1", folder)] = &fakeFolderAnchor{FeatureID: "collection-1", FolderID: folder,
			Applied: map[string]any{"fromTheFuture": "keep"}, Revision: 3}
		f.managed.folderStaleOnce = true
		f.managed.mu.Unlock()
		if code, line, out := applyTree(t, tree, root); code != syncExitClean {
			t.Fatalf("exit %d %+v\n%s", code, line, out)
		}
		a := f.managed.FolderAnchor("collection-1", folder)
		if a.Applied["fromTheFuture"] != "keep" || a.Applied["tableDocs"] == nil {
			t.Fatalf("anchor = %+v — want the newer key kept and the docs recorded", a.Applied)
		}
	})
	t.Run("another apply's different state is surfaced, not overwritten", func(t *testing.T) {
		f, tree, root := docsOnlyPipelineFolder(t)
		folder := folderIDOf(t, root)
		other := map[string]any{"tableDocs": map[string]any{"tables/elsewhere.json": map[string]any{"tableID": "table-x", "metaSHA256": "sha-x"}}}
		f.managed.mu.Lock()
		f.managed.folders[fakeFolderKey("collection-1", folder)] = &fakeFolderAnchor{FeatureID: "collection-1", FolderID: folder,
			Applied: map[string]any{}, Revision: 1}
		f.managed.folderRaceOnce = other
		f.managed.mu.Unlock()
		code, line, out := applyTree(t, tree, root)
		if code == syncExitClean || line.Reason != syncReasonAnchorNotRecorded {
			t.Fatalf("exit %d %+v — want refused anchor_not_recorded\n%s", code, line, out)
		}
		if got := canonicalJSON(f.managed.FolderAnchor("collection-1", folder).Applied); got != canonicalJSON(other) {
			t.Fatalf("the other apply's anchor was written over: %s", got)
		}
	})
	t.Run("over the cap is a named refusal, and nothing is written", func(t *testing.T) {
		f, tree, root := docsOnlyPipelineFolder(t)
		f.managed.mu.Lock()
		f.managed.folderCap = 64
		f.managed.mu.Unlock()
		code, line, out := applyTree(t, tree, root)
		if code == syncExitClean || line.Reason != syncReasonFolderAnchorTooLarge || !strings.Contains(line.Detail, "Split them across more than one folder") {
			t.Fatalf("exit %d %+v — want refused folder_anchor_too_large\n%s", code, line, out)
		}
		if f.managed.FolderAnchor("collection-1", folderIDOf(t, root)) != nil {
			t.Fatal("an anchor over the cap was written")
		}
		// One refusal per command, not one per lock save.
		puts := 0
		for _, r := range f.managed.requests {
			if r == "PUT /api/v2/managed/folder/applied" {
				puts++
			}
		}
		if puts != 1 {
			t.Fatalf("%d folder-anchor writes for one refusal", puts)
		}
	})
	t.Run("a refused folder anchor does not stop the claims' anchors", func(t *testing.T) {
		fx := pipelineIdempotenceFixture(t)
		adoptFixture(t, fx)
		folder := folderIDOf(t, fx.root)
		before := fx.pipeline.managed.ClaimAt("collection-1", folder, "orders.sql").Applied["liveSHA256"]
		fx.edit(t)
		fx.pipeline.managed.mu.Lock()
		delete(fx.pipeline.managed.folders, fakeFolderKey("collection-1", folder))
		fx.pipeline.managed.folderCap = 1
		fx.pipeline.managed.mu.Unlock()
		code, line, _, out := applyOnce(t, fx)
		if code == syncExitClean || line.Reason != syncReasonFolderAnchorTooLarge {
			t.Fatalf("exit %d %+v — want refused folder_anchor_too_large\n%s", code, line, out)
		}
		if c := fx.pipeline.managed.ClaimAt("collection-1", folder, "orders.sql"); c == nil || c.Applied["liveSHA256"] == before {
			t.Fatalf("a refused folder anchor stopped the claim's anchor: %+v", c)
		}
	})
}

// description and setDescription read and write a table's description on the
// pipeline fake, under its lock — an edit made in the web app.
func (f *fakePipelineInstance) description(id string) string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.tables[id].Description
}

func (f *fakePipelineInstance) setDescription(id, description string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.tables[id].Description = description
}

func (f *fakePipelineInstance) resetRequests() {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.Requests = nil
}

// TestManagedLegacyDocsOnAClaimMoveToTheFolderAnchor: a claim anchor an earlier
// build wrote still carries "tableDocs". With no folder anchor it is read once
// as the fallback, the next save records it on the folder anchor, and only
// then is it cleared from the claim.
func TestManagedLegacyDocsOnAClaimMoveToTheFolderAnchor(t *testing.T) {
	fx := pipelineIdempotenceFixture(t)
	adoptFixture(t, fx)
	if code, line, _, out := applyOnce(t, fx); code != syncExitClean {
		t.Fatalf("first apply: exit %d %+v\n%s", code, line, out)
	}
	m := fx.pipeline.managed
	folder := folderIDOf(t, fx.root)
	recorded := m.FolderAnchor("collection-1", folder).Applied["tableDocs"]
	// Rewind to what the docs host left behind: the records on the first
	// claim, and no folder anchor.
	m.mu.Lock()
	delete(m.folders, fakeFolderKey("collection-1", folder))
	host := m.byAddress("collection-1", folder, "metrics/aov.json")
	host.Applied[wfdir.LegacyDocsAnchorKey] = recorded
	m.mu.Unlock()
	fx.pipeline.setDescription("table-raw", "Rewritten in the app.")
	removeLock(t, fx.root)
	if code, line, _, out := applyOnce(t, fx); code == syncExitClean {
		t.Fatalf("the legacy baseline was not read: prose rewritten in the app, applied over: %+v\n%s", line, out)
	}
	if got := fx.pipeline.description("table-raw"); got != "Rewritten in the app." {
		t.Fatalf("the app's prose was overwritten: %q", got)
	}
	if docs := folderDocsOf(t, m, "collection-1", folder); docs["tables/fortnox_invoices.json"].TableID != "table-raw" {
		t.Fatalf("the legacy records were not moved onto the folder anchor: %+v", docs)
	}
	if c := m.ClaimAt("collection-1", folder, "metrics/aov.json"); c.Applied[wfdir.LegacyDocsAnchorKey] != nil {
		t.Fatalf("the legacy key stayed on the claim after the folder anchor took it: %v", c.Applied)
	}
}
