package commands

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/ronjatech/ronja-cli/internal/markers"
	"github.com/ronjatech/ronja-cli/internal/wfdir"
)

// References BY ADDRESS, resolved through the other folder's claim
// for the stack of the same name. Every test asserts on what reached the fake
// server, or on the refusal reason a script branches on.

const addressTestFolderID = "0b6f7c2e-3a41-4f0e-9d4c-7a5e2b9c1d08"

// addressFixture is a tree holding an ADOPTED pipeline folder at
// pipelines/sales (never pushed here — its claim is staged on the server) and a
// workflow folder at workflows/report that reads `orders` from it by address.
type addressFixture struct {
	f        *fakeInstance
	tree     string
	workflow string
	pipeline string
}

func newAddressFixture(t *testing.T, pipelineTenant string) addressFixture {
	t.Helper()
	f := newFakeInstance(t)
	signIn(t, f)
	// The pipeline's table lives on this instance; the workflow fake's probe
	// knows only workflows and modules, so it is taught the one table.
	probe := f.managed.probe
	f.managed.probe = func(kind, id string) fakeRowFacts {
		if (kind == "table") && id == "table-orders" {
			return fakeRowFacts{Exists: true, FeatureID: "feat-sales", Root: true, Name: "orders"}
		}
		return probe(kind, id)
	}
	tree := t.TempDir()
	// A git work tree, as a repository is: an address may not leave it.
	if err := os.Mkdir(filepath.Join(tree, ".git"), 0o755); err != nil {
		t.Fatal(err)
	}
	pipeline := filepath.Join(tree, "pipelines", "sales")
	workflow := filepath.Join(tree, "workflows", "report")
	for _, dir := range []string{pipeline, workflow} {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	pm := &wfdir.Manifest{
		Kind: wfdir.KindPipeline, ID: addressTestFolderID, Title: "Sales",
		Stacks: map[string]wfdir.Stack{"dev": {URL: f.URL(), TenantID: pipelineTenant, FeatureID: "feat-sales"}},
	}
	if err := wfdir.SaveFolder(pipeline, pm, &wfdir.Lock{}); err != nil {
		t.Fatal(err)
	}
	wm := &wfdir.Manifest{
		Kind: wfdir.KindWorkflow, Title: "Report", Entrypoint: "main.py",
		Dependencies: map[string]wfdir.Dependency{
			"orders": {Kind: markers.KindTable, From: "../../pipelines/sales", Address: "orders.sql"},
		},
		Stacks: map[string]wfdir.Stack{"dev": {URL: f.URL(), TenantID: testTenantID, FeatureID: "feat-1"}},
	}
	if err := wfdir.SaveFolder(workflow, wm, &wfdir.Lock{}); err != nil {
		t.Fatal(err)
	}
	writeLocal(t, workflow, "main.py", "read(\"{{ ref('orders') }}\")\n")
	return addressFixture{f: f, tree: tree, workflow: workflow, pipeline: pipeline}
}

func (fx addressFixture) claimOrders() {
	fx.f.managed.stage("feat-sales", addressTestFolderID, "orders.sql", "table", "table-orders", "")
}

// TestAddressDependencyResolvesThroughTheOtherFoldersClaim: the alias goes out
// as the row the other folder's claim names — no bind anywhere — and comes back
// as the alias, so a second push is up to date. The manifest is stamped format
// version 7.
func TestAddressDependencyResolvesThroughTheOtherFoldersClaim(t *testing.T) {
	fx := newAddressFixture(t, testTenantID)
	fx.claimOrders()

	if out, err := runCLI(t, fx.workflow, "wf", "push", "--stack", "dev", "--json"); err != nil {
		t.Fatalf("push: %v\n%s", err, out)
	}
	if got := fx.f.FileContents(workflowIDOf(t, fx.workflow))["main.py"]; got != "read(\"{{ ref('table-orders') }}\")\n" {
		t.Fatalf("the server was sent %q, want the claimed row's id", got)
	}
	out, err := runCLI(t, fx.workflow, "wf", "push", "--stack", "dev", "--json")
	if err != nil {
		t.Fatalf("second push: %v\n%s", err, out)
	}
	if decodeJSON(t, out)["upToDate"] != true {
		t.Fatalf("second push was not up to date:\n%s", out)
	}
	if m := loadAnyManifest(t, fx.workflow); m.FormatVersion != 7 {
		t.Errorf("formatVersion = %d, want 7 for an address dependency", m.FormatVersion)
	}
	// The resolution read the OTHER folder's claims, in the other stack's feature.
	found := false
	for _, r := range fx.f.managed.requests {
		if r == "GET /api/v2/managed" {
			found = true
		}
	}
	if !found {
		t.Fatalf("no GET /managed was made: %v", fx.f.managed.requests)
	}
}

// TestAddressDependencyUnresolvedRefusesBeforeAnyWrite: the other folder has
// not been applied (no claim at the address), so the push refuses
// address_unresolved naming the folder and the address, and writes nothing.
// `sync status` and `sync check` report the same reason.
func TestAddressDependencyUnresolvedRefusesBeforeAnyWrite(t *testing.T) {
	fx := newAddressFixture(t, testTenantID)

	out, err := runCLI(t, fx.workflow, "wf", "push", "--stack", "dev", "--json")
	if err == nil {
		t.Fatalf("push succeeded with an unresolved address:\n%s", out)
	}
	for _, want := range []string{"../../pipelines/sales", `"orders.sql"`, "apply that folder first"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("refusal %q does not name %s", err, want)
		}
	}
	if len(fx.f.created) != 0 {
		t.Fatalf("an unresolved address created %d workflows", len(fx.f.created))
	}

	check, _ := runCLI(t, fx.workflow, "sync", "check", "--stack", "dev", "--json")
	if !strings.Contains(check, `"reason": "address_unresolved"`) || !strings.Contains(check, `"from": "../../pipelines/sales"`) {
		t.Fatalf("sync check did not report the address reason:\n%s", check)
	}
	status, _ := runCLI(t, fx.workflow, "sync", "status", "--stack", "dev", "--json")
	if !strings.Contains(status, `"reason": "address_unresolved"`) {
		t.Fatalf("sync status did not report the address reason:\n%s", status)
	}
	plan, _ := runCLI(t, fx.workflow, "sync", "apply", "--stack", "dev", "--dry-run", "--json")
	if !strings.Contains(plan, `"address_unresolved"`) {
		t.Fatalf("sync apply --dry-run did not report the address reason:\n%s", plan)
	}

	// Applying the other folder (its claim landing) is the whole remedy.
	fx.claimOrders()
	if out, err := runCLI(t, fx.workflow, "wf", "push", "--stack", "dev", "--json"); err != nil {
		t.Fatalf("push after the claim landed: %v\n%s", err, out)
	}
}

// TestAddressDependencyAcrossTargetsIsRefused: the other folder's stack of the
// same name points at another organization, so the address must not resolve —
// even though a claim exists — and the refusal is address_stack_mismatch.
func TestAddressDependencyAcrossTargetsIsRefused(t *testing.T) {
	fx := newAddressFixture(t, "ten-elsewhere")
	fx.claimOrders()

	_, err := runCLI(t, fx.workflow, "wf", "push", "--stack", "dev", "--json")
	if err == nil || !strings.Contains(err.Error(), "never across organizations") {
		t.Fatalf("an address across targets was not refused: %v", err)
	}
	if len(fx.f.created) != 0 {
		t.Fatalf("created %d workflows", len(fx.f.created))
	}
	check, _ := runCLI(t, fx.workflow, "sync", "check", "--stack", "dev", "--json")
	if !strings.Contains(check, `"reason": "address_stack_mismatch"`) {
		t.Fatalf("sync check did not report address_stack_mismatch:\n%s", check)
	}
}

// TestAddressDependencyNeedsAnAdoptedFolder: a folder with no id keeps no
// server state, so nothing can be found by address in it.
func TestAddressDependencyNeedsAnAdoptedFolder(t *testing.T) {
	fx := newAddressFixture(t, testTenantID)
	fx.claimOrders()
	pm := loadAnyManifest(t, fx.pipeline)
	pm.ID = ""
	if err := wfdir.SaveManifest(fx.pipeline, pm); err != nil {
		t.Fatal(err)
	}
	_, err := runCLI(t, fx.workflow, "wf", "push", "--stack", "dev", "--json")
	if err == nil || !strings.Contains(err.Error(), "sync adopt") {
		t.Fatalf("an address into a legacy folder was not refused: %v", err)
	}
}

// TestAddressDependencyRefusesABindBesideIt: one question, two answers.
func TestAddressDependencyRefusesABindBesideIt(t *testing.T) {
	fx := newAddressFixture(t, testTenantID)
	fx.claimOrders()
	m := loadAnyManifest(t, fx.workflow)
	st := m.Stacks["dev"]
	st.Bind = map[string]string{"orders": "table-other"}
	m.Stacks["dev"] = st
	if err := wfdir.SaveManifest(fx.workflow, m); err != nil {
		t.Fatal(err)
	}
	_, err := runCLI(t, fx.workflow, "wf", "push", "--stack", "dev", "--json")
	if err == nil || !strings.Contains(err.Error(), "declares by address") {
		t.Fatalf("a bind beside an address dependency was not refused: %v", err)
	}
}

// TestAddressDependencyBelowItsFormatVersionIsRefused: a hand-written address
// dependency in a file that claims an older format would be read by an older
// CLI as an ordinary alias.
func TestAddressDependencyBelowItsFormatVersionIsRefused(t *testing.T) {
	fx := newAddressFixture(t, testTenantID)
	fx.claimOrders()
	raw, err := os.ReadFile(wfdir.ManifestPath(fx.workflow))
	if err != nil {
		t.Fatal(err)
	}
	var body map[string]any
	if err := json.Unmarshal(raw, &body); err != nil {
		t.Fatal(err)
	}
	body["formatVersion"] = 3
	out, _ := json.Marshal(body)
	if err := os.WriteFile(wfdir.ManifestPath(fx.workflow), out, 0o644); err != nil {
		t.Fatal(err)
	}
	_, err = runCLI(t, fx.workflow, "wf", "push", "--stack", "dev", "--json")
	if err == nil || !strings.Contains(err.Error(), `"formatVersion": 7`) {
		t.Fatalf("an address dependency below version 7 was not refused: %v", err)
	}
}

// TestCrossFolderTableReadsAreRecorded: a resolved table address is recorded
// on the folder as a build-ordering edge, naming the other folder and address.
func TestCrossFolderTableReadsAreRecorded(t *testing.T) {
	fx := newAddressFixture(t, testTenantID)
	fx.claimOrders()
	m := loadAnyManifest(t, fx.workflow)
	bind, errs, edges, _ := resolveAddressDeps(t.Context(), newClient(fx.f.URL(), "tok"), fx.workflow, m, "dev")
	if len(errs) != 0 || bind["orders"] != "table-orders" {
		t.Fatalf("bind = %v, errs = %v", bind, errs)
	}
	f := &folder{addressEdges: edges}
	reads := f.crossFolderTableReads()
	if len(reads) != 1 || reads[0].Address != "orders.sql" || reads[0].FromRoot != fx.pipeline {
		t.Fatalf("recorded edges = %+v", reads)
	}
}

// TestAddressDependencyMayNotLeaveTheRepository is M3: "from" is confined to
// the repository the folder belongs to — its git work tree, or without one the
// tree `ronja sync` walks, or the folder itself — so a committed folder cannot
// resolve an alias through some other folder on the runner's disk.
func TestAddressDependencyMayNotLeaveTheRepository(t *testing.T) {
	fx := newAddressFixture(t, testTenantID)
	fx.claimOrders()
	// A folder OUTSIDE the work tree, adopted and claimed exactly like the
	// pipeline inside it: only the boundary tells them apart.
	outside := filepath.Join(filepath.Dir(fx.tree), "outside-"+filepath.Base(fx.tree))
	if err := os.MkdirAll(outside, 0o755); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(outside) })
	pm := loadAnyManifest(t, fx.pipeline)
	if err := wfdir.SaveFolder(outside, pm, &wfdir.Lock{}); err != nil {
		t.Fatal(err)
	}
	m := loadAnyManifest(t, fx.workflow)
	rel, err := filepath.Rel(fx.workflow, outside)
	if err != nil {
		t.Fatal(err)
	}
	m.Dependencies["orders"] = wfdir.Dependency{Kind: markers.KindTable, From: filepath.ToSlash(rel), Address: "orders.sql"}
	for _, from := range []string{filepath.ToSlash(rel), "../../../outside"} {
		m.Dependencies["orders"] = wfdir.Dependency{Kind: markers.KindTable, From: from, Address: "orders.sql"}
		bind, errs, _, _ := resolveAddressDeps(t.Context(), newClient(fx.f.URL(), "tok"), fx.workflow, m, "dev")
		if bind["orders"] != "" || errs["orders"].Reason != syncReasonAddressOutsideRepository {
			t.Fatalf("from %q: bind = %v, errs = %v", from, bind, errs)
		}
	}

	// Without git: the tree a sync walks is the boundary, and the folder itself
	// when nothing walks one.
	if err := os.Remove(filepath.Join(fx.tree, ".git")); err != nil {
		t.Fatal(err)
	}
	m.Dependencies["orders"] = wfdir.Dependency{Kind: markers.KindTable, From: "../../pipelines/sales", Address: "orders.sql"}
	if bind, errs, _, _ := resolveAddressDeps(withAddressWalkRoot(t.Context(), fx.tree), newClient(fx.f.URL(), "tok"), fx.workflow, m, "dev"); bind["orders"] != "table-orders" {
		t.Fatalf("inside the walked tree: bind = %v, errs = %v", bind, errs)
	}
	if _, errs, _, _ := resolveAddressDeps(t.Context(), newClient(fx.f.URL(), "tok"), fx.workflow, m, "dev"); errs["orders"].Reason != syncReasonAddressOutsideRepository {
		t.Fatalf("no git and no walk: errs = %v", errs)
	}
}
