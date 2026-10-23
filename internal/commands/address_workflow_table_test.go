package commands

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/ronjatech/ronja-cli/internal/api"
	"github.com/ronjatech/ronja-cli/internal/markers"
	"github.com/ronjatech/ronja-cli/internal/wfdir"
)

// A table another folder's WORKFLOW writes, named by address: "from" the
// workflow folder, "address" its dependency declared "write" or "output".
// Resolved per stack of the same name to the table that dependency writes
// there — never to a table of that name in another feature, which is how a
// second stack's reader came to read production.

const producerFolderID = "5d0a7c4e-1b2f-4c3d-8e9f-0a1b2c3d4e5f"

type workflowTableFixture struct {
	f      *fakeInstance
	reader string
}

// newWorkflowTableFixture is a tree holding an ADOPTED producer workflow at
// workflows/compute — deploying stacks dev (feat-dev) and prod (feat-prod),
// its output weekly_revenue bound in prod only and its write target ledger
// bound in both — and a reader workflow at workflows/report naming address.
func newWorkflowTableFixture(t *testing.T, address string) workflowTableFixture {
	t.Helper()
	f := newFakeInstance(t)
	signIn(t, f)
	// The producer's workflows live on this instance, in their stacks' features.
	probe := f.managed.probe
	f.managed.probe = func(kind, id string) fakeRowFacts {
		if feature, ok := map[string]string{"wf-compute-dev": "feat-dev", "wf-compute-prod": "feat-prod"}[id]; ok && kind == "workflow" {
			return fakeRowFacts{Exists: true, FeatureID: feature, Root: true, Name: "Compute"}
		}
		return probe(kind, id)
	}
	tree := t.TempDir()
	if err := os.Mkdir(filepath.Join(tree, ".git"), 0o755); err != nil {
		t.Fatal(err)
	}
	producer := filepath.Join(tree, "workflows", "compute")
	reader := filepath.Join(tree, "workflows", "report")
	stack := func(feature string, bind map[string]string) wfdir.Stack {
		return wfdir.Stack{URL: f.URL(), TenantID: testTenantID, FeatureID: feature, Bind: bind}
	}
	pm := &wfdir.Manifest{
		Kind: wfdir.KindWorkflow, ID: producerFolderID, Title: "Compute", Entrypoint: "main.py",
		Dependencies: map[string]wfdir.Dependency{
			"weekly_revenue": {Kind: markers.KindTable, Output: true},
			"ledger":         {Kind: markers.KindTable, Write: true},
			"raw":            {Kind: markers.KindTable},
		},
		Stacks: map[string]wfdir.Stack{
			"dev":  stack("feat-dev", map[string]string{"ledger": "table-ledger-dev", "raw": "table-raw"}),
			"prod": stack("feat-prod", map[string]string{"weekly_revenue": "table-wr-prod", "ledger": "table-ledger-prod", "raw": "table-raw"}),
		},
	}
	if err := os.MkdirAll(producer, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := wfdir.SaveFolder(producer, pm, &wfdir.Lock{}); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(reader, 0o755); err != nil {
		t.Fatal(err)
	}
	rm := &wfdir.Manifest{
		Kind: wfdir.KindWorkflow, Title: "Report", Entrypoint: "main.py",
		Dependencies: map[string]wfdir.Dependency{
			"revenue": {Kind: markers.KindTable, From: "../compute", Address: address},
		},
		Stacks: map[string]wfdir.Stack{"dev": stack("feat-1", nil), "prod": stack("feat-1", nil)},
	}
	if err := wfdir.SaveFolder(reader, rm, &wfdir.Lock{}); err != nil {
		t.Fatal(err)
	}
	writeLocal(t, reader, "main.py", "read(\"{{ ref('revenue') }}\")\n")
	// The producer's workflow in each stack, as its applies claimed it.
	f.managed.stage("feat-dev", producerFolderID, "", "workflow", "wf-compute-dev", "")
	f.managed.stage("feat-prod", producerFolderID, "", "workflow", "wf-compute-prod", "")
	// Production's output, made by its runs — the table an org-wide name
	// match finds first, and the one the dev stack must never read.
	f.featureTables["feat-prod"] = []*api.TableListItem{{ID: "table-wr-prod", FeatureID: "feat-prod", Name: "weekly_revenue",
		WorkflowID: "wf-compute-prod", WorkflowLogicalName: "weekly_revenue"}}
	return workflowTableFixture{f: f, reader: reader}
}

func (fx workflowTableFixture) sent(t *testing.T) string {
	t.Helper()
	return fx.f.FileContents(workflowIDOf(t, fx.reader))["main.py"]
}

// The reader follows each stack's own output: bound in prod, the table the
// dev workflow's runs made in dev's feature — and before the dev workflow has
// run, a refusal naming the run, never production's table.
func TestAddressOfAWorkflowOutputFollowsEachStack(t *testing.T) {
	fx := newWorkflowTableFixture(t, "weekly_revenue")

	out, err := runCLI(t, fx.reader, "wf", "push", "--stack", "dev", "--json")
	if err == nil {
		t.Fatalf("push resolved an output the dev workflow has not made yet:\n%s", out)
	}
	for _, want := range []string{"ronja wf run --stack dev", `"weekly_revenue"`, "feat-dev"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("refusal %q does not name %s", err, want)
		}
	}
	if len(fx.f.created) != 0 {
		t.Fatalf("the refused push created %d workflows", len(fx.f.created))
	}
	if status, _ := runCLI(t, fx.reader, "sync", "status", "--stack", "dev", "--json"); !strings.Contains(status, `"reason": "address_unresolved"`) {
		t.Fatalf("sync status did not report the address reason:\n%s", status)
	}

	// The dev workflow runs once: its output now exists in dev's feature.
	fx.f.featureTables["feat-dev"] = []*api.TableListItem{
		{ID: "table-other-dev", FeatureID: "feat-dev", Name: "weekly_revenue"},
		{ID: "table-wr-dev", FeatureID: "feat-dev", Name: "weekly_revenue (1)", WorkflowID: "wf-compute-dev", WorkflowLogicalName: "weekly_revenue"},
	}
	if out, err := runCLI(t, fx.reader, "wf", "push", "--stack", "dev", "--json"); err != nil {
		t.Fatalf("push on dev: %v\n%s", err, out)
	}
	if got := fx.sent(t); got != "read(\"{{ ref('table-wr-dev') }}\")\n" {
		t.Fatalf("dev's reader was sent %q, want dev's own output", got)
	}
	if out, err := runCLI(t, fx.reader, "wf", "push", "--stack", "dev", "--json"); err != nil || decodeJSON(t, out)["upToDate"] != true {
		t.Fatalf("the second push on dev was not up to date: %v\n%s", err, out)
	}
}

// Bound in the producer's stack, an output — and a write target, which is
// always bound — resolves to that binding.
func TestAddressOfAWorkflowTableFollowsTheProducersBinding(t *testing.T) {
	for address, want := range map[string]string{"weekly_revenue": "table-wr-prod", "ledger": "table-ledger-prod"} {
		t.Run(address, func(t *testing.T) {
			fx := newWorkflowTableFixture(t, address)
			if out, err := runCLI(t, fx.reader, "wf", "push", "--stack", "prod", "--json"); err != nil {
				t.Fatalf("push on prod: %v\n%s", err, out)
			}
			if got := fx.sent(t); got != "read(\"{{ ref('"+want+"') }}\")\n" {
				t.Fatalf("prod's reader was sent %q, want %s", got, want)
			}
		})
	}
}

// The address must be a table the workflow writes: a dependency it does not
// declare, or one it only reads, is refused by name.
func TestAddressOfAWorkflowTableMustBeOneItWrites(t *testing.T) {
	for _, address := range []string{"nope", "raw"} {
		t.Run(address, func(t *testing.T) {
			fx := newWorkflowTableFixture(t, address)
			out, err := runCLI(t, fx.reader, "wf", "push", "--stack", "prod", "--json")
			if err == nil {
				t.Fatalf("push resolved %q:\n%s", address, out)
			}
			if !strings.Contains(err.Error(), `"write": true or "output": true`) {
				t.Errorf("refusal %q does not say what the address must name", err)
			}
			check, _ := runCLI(t, fx.reader, "sync", "check", "--stack", "prod", "--json")
			if !strings.Contains(check, `"reason": "`+syncReasonAddressNotWritten+`"`) {
				t.Errorf("sync check did not report %s:\n%s", syncReasonAddressNotWritten, check)
			}
		})
	}
}

// A bound output must be a table of the stack's own feature: a stack block
// copied from prod binds prod's table, and the dev reader would read
// production. Refused address_stack_mismatch, naming the binding and the
// feature, before anything is written.
func TestAddressOfABoundOutputOutsideTheStacksFeatureIsRefused(t *testing.T) {
	fx := newWorkflowTableFixture(t, "weekly_revenue")
	producer := filepath.Join(filepath.Dir(fx.reader), "compute")
	m, err := wfdir.LoadManifest(producer, wfdir.WorkflowKind)
	if err != nil {
		t.Fatal(err)
	}
	dev := m.Stacks["dev"]
	dev.Bind["weekly_revenue"] = "table-wr-prod"
	m.Stacks["dev"] = dev
	if err := wfdir.SaveManifest(producer, m); err != nil {
		t.Fatal(err)
	}
	out, err := runCLI(t, fx.reader, "wf", "push", "--stack", "dev", "--json")
	if err == nil {
		t.Fatalf("push resolved dev's reader to production's output:\n%s", out)
	}
	for _, want := range []string{"table-wr-prod", "feat-dev", `"weekly_revenue"`} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("refusal %q does not name %s", err, want)
		}
	}
	if len(fx.f.created) != 0 {
		t.Fatalf("the refused push created %d workflows", len(fx.f.created))
	}
	if check, _ := runCLI(t, fx.reader, "sync", "check", "--stack", "dev", "--json"); !strings.Contains(check, `"reason": "`+syncReasonAddressStackMismatch+`"`) {
		t.Fatalf("sync check did not report %s:\n%s", syncReasonAddressStackMismatch, check)
	}

	// dev's own copy, bound there, is read.
	dev.Bind["weekly_revenue"] = "table-wr-dev"
	m.Stacks["dev"] = dev
	if err := wfdir.SaveManifest(producer, m); err != nil {
		t.Fatal(err)
	}
	fx.f.featureTables["feat-dev"] = []*api.TableListItem{{ID: "table-wr-dev", FeatureID: "feat-dev", Name: "weekly_revenue"}}
	if out, err := runCLI(t, fx.reader, "wf", "push", "--stack", "dev", "--json"); err != nil {
		t.Fatalf("push on dev: %v\n%s", err, out)
	}
	if got := fx.sent(t); got != "read(\"{{ ref('table-wr-dev') }}\")\n" {
		t.Fatalf("dev's reader was sent %q", got)
	}
}

// Two outputs of one workflow, read by one folder, cost one listing of the
// stack's feature.
func TestAddressesOfTwoOutputsListTheFeatureOnce(t *testing.T) {
	fx := newWorkflowTableFixture(t, "weekly_revenue")
	producer := filepath.Join(filepath.Dir(fx.reader), "compute")
	pm, err := wfdir.LoadManifest(producer, wfdir.WorkflowKind)
	if err != nil {
		t.Fatal(err)
	}
	pm.Dependencies["monthly_revenue"] = wfdir.Dependency{Kind: markers.KindTable, Output: true}
	if err := wfdir.SaveManifest(producer, pm); err != nil {
		t.Fatal(err)
	}
	rm, err := wfdir.LoadManifest(fx.reader, wfdir.WorkflowKind)
	if err != nil {
		t.Fatal(err)
	}
	rm.Dependencies["monthly"] = wfdir.Dependency{Kind: markers.KindTable, From: "../compute", Address: "monthly_revenue"}
	if err := wfdir.SaveManifest(fx.reader, rm); err != nil {
		t.Fatal(err)
	}
	writeLocal(t, fx.reader, "main.py", "read(\"{{ ref('revenue') }}\")\nread(\"{{ ref('monthly') }}\")\n")
	fx.f.featureTables["feat-dev"] = []*api.TableListItem{
		{ID: "table-wr-dev", FeatureID: "feat-dev", Name: "weekly_revenue", WorkflowID: "wf-compute-dev", WorkflowLogicalName: "weekly_revenue"},
		{ID: "table-mr-dev", FeatureID: "feat-dev", Name: "monthly_revenue", WorkflowID: "wf-compute-dev", WorkflowLogicalName: "monthly_revenue"},
	}
	fx.f.Requests = nil
	if out, err := runCLI(t, fx.reader, "wf", "push", "--stack", "dev", "--json"); err != nil {
		t.Fatalf("push on dev: %v\n%s", err, out)
	}
	if got := fx.sent(t); got != "read(\"{{ ref('table-wr-dev') }}\")\nread(\"{{ ref('table-mr-dev') }}\")\n" {
		t.Fatalf("dev's reader was sent %q", got)
	}
	listings := 0
	for _, r := range fx.f.Requests {
		if r == "GET /api/v2/feature/model" {
			listings++
		}
	}
	if listings != 1 {
		t.Fatalf("the push listed the feature's tables %d times, want once:\n%s", listings, strings.Join(fx.f.Requests, "\n"))
	}
}

// An address that resolves to nothing because nothing could look it up — a
// signed-out status — was not re-pointed, and status does not say it was.
func TestAnUnresolvedAddressIsNotARebind(t *testing.T) {
	fx := newWorkflowTableFixture(t, "weekly_revenue")
	if out, err := runCLI(t, fx.reader, "wf", "push", "--stack", "prod", "--json"); err != nil {
		t.Fatalf("push on prod: %v\n%s", err, out)
	}
	signOut(t, fx.f)
	out, err := runCLI(t, fx.reader, "wf", "status", "--stack", "prod", "--json")
	if err != nil {
		t.Fatalf("status: %v\n%s", err, out)
	}
	if rebound := decodeJSON(t, out)["rebound"]; rebound != nil {
		t.Fatalf("a signed-out status reports the address as re-pointed: %v", rebound)
	}
}

// A bound output is checked against the table itself, not the feature's
// listing: a hidden or archived table of the stack's own feature is not
// listed, and is still that stack's own output.
func TestAddressOfABoundOutputNotListedInItsFeatureIsRead(t *testing.T) {
	fx := newWorkflowTableFixture(t, "weekly_revenue")
	producer := filepath.Join(filepath.Dir(fx.reader), "compute")
	m, err := wfdir.LoadManifest(producer, wfdir.WorkflowKind)
	if err != nil {
		t.Fatal(err)
	}
	dev := m.Stacks["dev"]
	dev.Bind["weekly_revenue"] = "table-wr-dev-hidden"
	m.Stacks["dev"] = dev
	if err := wfdir.SaveManifest(producer, m); err != nil {
		t.Fatal(err)
	}
	fx.f.tableNames["table-wr-dev-hidden"] = "weekly_revenue"
	fx.f.tableFeature = map[string]string{"table-wr-dev-hidden": "feat-dev"}
	if out, err := runCLI(t, fx.reader, "wf", "push", "--stack", "dev", "--json"); err != nil {
		t.Fatalf("push on dev: %v\n%s", err, out)
	}
	if got := fx.sent(t); got != "read(\"{{ ref('table-wr-dev-hidden') }}\")\n" {
		t.Fatalf("dev's reader was sent %q", got)
	}
}
