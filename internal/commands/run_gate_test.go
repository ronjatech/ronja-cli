package commands

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/ronjatech/ronja-cli/internal/api"
	"github.com/ronjatech/ronja-cli/internal/markers"
	"github.com/ronjatech/ronja-cli/internal/wfdir"
)

// The RUN-EDGE half of the write gate, one test per position: a
// workflow's `{{ workflow() }}` and `{{ agent() }}`, an app's
// allowedWorkflowIDs and allowedAgentIDs, an automation's workflow and
// saved_agent actions and its `workflow` references. Each target outside the
// stack's feature is refused runs_outside_target until a dependency declared
// "run": true binds it in that stack; a target in the stack's own feature, or
// one this repository manages there, passes without it.

// runGatedTree is gatedTree plus what the run edges name: an agent and a
// workflow of the stack's own feature (Sales), and an agent of Finance.
// Finance's workflow, workflow-close, is in the import fixture.
func runGatedTree(t *testing.T) (*fakeImportInstance, string) {
	t.Helper()
	f, dir := gatedTree(t)
	f.wf.agentIDs["agent-fin"], f.wf.agentFeatures["agent-fin"] = true, financeFeature
	f.wf.agentIDs["agent-sales"], f.wf.agentFeatures["agent-sales"] = true, importFeature
	f.wf.AddWorkflow(&api.Workflow{ID: "workflow-sales-other", Lifecycle: api.LifecycleLive, FeatureID: importFeature,
		FeatureScope: "organization", Title: "Other sales job", RuntimeVersion: wfdir.RuntimeDefault},
		api.WorkflowFile{Path: "main.py", Content: "print('other')\n"})
	return f, dir
}

// assertRunRefused fails unless err is the gate's run refusal naming every
// want, and nothing but validate was sent.
func assertRunRefused(t *testing.T, f *fakeImportInstance, out string, err error, wants ...string) {
	t.Helper()
	if err == nil {
		t.Fatalf("the push went through:\n%s", out)
	}
	r, ok := asManagedRefusal(err)
	if !ok || r.Reason != syncReasonRunsOutsideTarget {
		t.Fatalf("err = %v, want runs_outside_target", err)
	}
	for _, want := range append([]string{"would run outside it", `"run": true`}, wants...) {
		if !strings.Contains(err.Error(), want) {
			t.Fatalf("the refusal does not say %q:\n%v", want, err)
		}
	}
	if writes := workflowWrites(f); len(writes) != 0 {
		t.Fatalf("a refused push sent %v", writes)
	}
}

// declare adds dependencies to a folder and binds them in its main stack.
func declare(t *testing.T, root string, deps map[string]wfdir.Dependency, bind map[string]string) {
	t.Helper()
	m := loadAnyManifest(t, root)
	if m.Dependencies == nil {
		m.Dependencies = map[string]wfdir.Dependency{}
	}
	for alias, dep := range deps {
		m.Dependencies[alias] = dep
	}
	st := m.Stacks["main"]
	if st.Bind == nil {
		st.Bind = map[string]string{}
	}
	for alias, id := range bind {
		st.Bind[alias] = id
	}
	m.Stacks["main"] = st
	if err := wfdir.SaveManifest(root, m); err != nil {
		t.Fatal(err)
	}
}

func replaceIn(t *testing.T, path, old, new string) {
	t.Helper()
	body := readFile(t, path)
	if !strings.Contains(body, old) {
		t.Fatalf("%s holds no %q:\n%s", path, old, body)
	}
	if err := os.WriteFile(path, []byte(strings.Replace(body, old, new, 1)), 0o644); err != nil {
		t.Fatal(err)
	}
}

// TestRunGateWorkflowMarker: a `{{ workflow() }}` naming a Finance workflow is
// refused, naming the line, the workflow and its feature; one of the stack's
// own feature passes; the Finance one bound through a "run": true dependency
// is admitted.
func TestRunGateWorkflowMarker(t *testing.T) {
	f, dir := runGatedTree(t)
	root := filepath.Join(dir, "workflows/sales-report")
	main := filepath.Join(root, "main.py")
	appendLine(t, main, `tools.runWorkflow({{ workflow('workflow-close') }})`)
	f.resetRequests()
	out, err := runCLI(t, root, "wf", "push")
	assertRunRefused(t, f, out, err, "main.py:7 runs the workflow workflow-close (Close books)", "in feature "+financeFeature)

	replaceIn(t, main, "workflow-close", "workflow-sales-other")
	if out, err := runCLI(t, root, "wf", "push"); err != nil {
		t.Fatalf("a workflow of the stack's own feature was refused: %v\n%s", err, out)
	}

	replaceIn(t, main, "{{ workflow('workflow-sales-other') }}", "{{ workflow('close') }}")
	declare(t, root, map[string]wfdir.Dependency{"close": {Kind: markers.KindWorkflow, Run: true}}, map[string]string{"close": "workflow-close"})
	if out, err := runCLI(t, root, "wf", "push"); err != nil {
		t.Fatalf("a run source bound in this stack was refused: %v\n%s", err, out)
	}
	if got := f.wf.FileContents(f.wf.draftOf["workflow-report"])["main.py"]; !strings.Contains(got, "{{ workflow('workflow-close') }}") {
		t.Fatalf("the bound run source did not go out as its id:\n%s", got)
	}
}

// TestRunGateWorkflowMarkerWithoutRunIsRefused: a dependency bound to the
// Finance workflow but NOT declared "run": true admits nothing — the bind
// alone is not the deliberate per-stack decision a run edge needs.
func TestRunGateWorkflowMarkerWithoutRunIsRefused(t *testing.T) {
	f, dir := runGatedTree(t)
	root := filepath.Join(dir, "workflows/sales-report")
	appendLine(t, filepath.Join(root, "main.py"), `tools.runWorkflow({{ workflow('close') }})`)
	declare(t, root, map[string]wfdir.Dependency{"close": {Kind: markers.KindWorkflow}}, map[string]string{"close": "workflow-close"})
	f.resetRequests()
	out, err := runCLI(t, root, "wf", "push")
	assertRunRefused(t, f, out, err, "main.py:7 runs the workflow workflow-close")
}

// TestRunGateAgentMarker: the same rule for `{{ agent() }}`.
func TestRunGateAgentMarker(t *testing.T) {
	f, dir := runGatedTree(t)
	root := filepath.Join(dir, "workflows/sales-report")
	main := filepath.Join(root, "main.py")
	appendLine(t, main, `tools.run_agent({{ agent('agent-fin') }})`)
	f.resetRequests()
	out, err := runCLI(t, root, "wf", "push")
	assertRunRefused(t, f, out, err, "main.py:7 runs the agent agent-fin", "in feature "+financeFeature)

	replaceIn(t, main, "agent-fin", "agent-sales")
	if out, err := runCLI(t, root, "wf", "push"); err != nil {
		t.Fatalf("an agent of the stack's own feature was refused: %v\n%s", err, out)
	}

	replaceIn(t, main, "{{ agent('agent-sales') }}", "{{ agent('triage') }}")
	declare(t, root, map[string]wfdir.Dependency{"triage": {Kind: markers.KindAgent, Run: true}}, map[string]string{"triage": "agent-fin"})
	if out, err := runCLI(t, root, "wf", "push"); err != nil {
		t.Fatalf("an agent run source bound in this stack was refused: %v\n%s", err, out)
	}
}

// TestRunGateAppAllowedWorkflows: the imported app may run Finance's workflow
// because import declared it "run": true and bound it in main; without the
// flag the same bind is refused.
func TestRunGateAppAllowedWorkflows(t *testing.T) {
	f, dir := runGatedTree(t)
	root := filepath.Join(dir, "apps/sales-dashboard")
	appendLine(t, filepath.Join(root, "App.tsx"), "// a change")
	if out, err := runCLI(t, root, "app", "push"); err != nil {
		t.Fatalf("the imported app's run source was refused: %v\n%s", err, out)
	}

	m := loadAnyManifest(t, root)
	m.Dependencies["close_books"] = wfdir.Dependency{Kind: markers.KindWorkflow}
	if err := wfdir.SaveManifest(root, m); err != nil {
		t.Fatal(err)
	}
	appendLine(t, filepath.Join(root, "App.tsx"), "// another change")
	f.resetRequests()
	out, err := runCLI(t, root, "app", "push")
	assertRunRefused(t, f, out, err, "access.allowedWorkflowIDs[0] runs the workflow workflow-close (Close books)", "in feature "+financeFeature)
}

// TestRunGateAppAllowedAgents: an agent in the app's access is a run edge too.
func TestRunGateAppAllowedAgents(t *testing.T) {
	f, dir := runGatedTree(t)
	root := filepath.Join(dir, "apps/sales-dashboard")
	setAgents := func(entry string) {
		m := loadAnyManifest(t, root)
		access := m.DeclaredAccess()
		access.AllowedAgentIDs = []string{entry}
		m.Access = &access
		if err := wfdir.SaveManifest(root, m); err != nil {
			t.Fatal(err)
		}
	}
	setAgents("agent-fin")
	f.resetRequests()
	out, err := runCLI(t, root, "app", "push")
	assertRunRefused(t, f, out, err, "access.allowedAgentIDs[0] runs the agent agent-fin", "in feature "+financeFeature)

	setAgents("agent-sales")
	if out, err := runCLI(t, root, "app", "push"); err != nil {
		t.Fatalf("an agent of the stack's own feature was refused: %v\n%s", err, out)
	}

	setAgents("triage")
	declare(t, root, map[string]wfdir.Dependency{"triage": {Kind: markers.KindAgent, Run: true}}, map[string]string{"triage": "agent-fin"})
	if out, err := runCLI(t, root, "app", "push"); err != nil {
		t.Fatalf("an agent run source bound in this stack was refused: %v\n%s", err, out)
	}
}

// TestRunGateAutomationWorkflowAction: a workflow action running Finance's
// workflow is refused until its dependency is a bound run source; the imported
// nightly automation, which runs the folder's own workflow by address, passes.
func TestRunGateAutomationWorkflowAction(t *testing.T) {
	f, dir := runGatedTree(t)
	root := filepath.Join(dir, "automations/sales")
	nightly := filepath.Join(root, "nightly.json")
	replaceIn(t, nightly, `"workflowID": "sales_report"`, `"workflowID": "close"`)
	declare(t, root, map[string]wfdir.Dependency{"close": {Kind: markers.KindWorkflow}}, map[string]string{"close": "workflow-close"})
	f.resetRequests()
	out, err := runCLI(t, root, "automation", "push")
	assertRunRefused(t, f, out, err, "nightly.json → action.config.workflowID runs the workflow workflow-close", "in feature "+financeFeature)

	declare(t, root, map[string]wfdir.Dependency{"close": {Kind: markers.KindWorkflow, Run: true}}, nil)
	if out, err := runCLI(t, root, "automation", "push"); err != nil {
		t.Fatalf("a run source bound in this stack was refused: %v\n%s", err, out)
	}
}

// TestRunGateAutomationSavedAgentAction: a saved_agent action is a run edge.
func TestRunGateAutomationSavedAgentAction(t *testing.T) {
	f, dir := runGatedTree(t)
	root := filepath.Join(dir, "automations/sales")
	writeLocal(t, root, "weekly-triage.json", `{
  "triggerKind": "cron",
  "cronExpr": "0 9 * * 1",
  "timezone": "UTC",
  "enabled": false,
  "action": {"kind": "saved_agent", "config": {"agentID": "triage", "prompt": "Triage the week"}}
}
`)
	declare(t, root, map[string]wfdir.Dependency{"triage": {Kind: markers.KindAgent}}, map[string]string{"triage": "agent-fin"})
	f.resetRequests()
	out, err := runCLI(t, root, "automation", "push")
	assertRunRefused(t, f, out, err, "weekly-triage.json → action.config.agentID runs the agent agent-fin", "in feature "+financeFeature)

	declare(t, root, map[string]wfdir.Dependency{"triage": {Kind: markers.KindAgent, Run: true}}, nil)
	if out, err := runCLI(t, root, "automation", "push"); err != nil {
		t.Fatalf("an agent run source bound in this stack was refused: %v\n%s", err, out)
	}
}

// TestRunGateAutomationWorkflowReference: a `workflow` in an agent action's
// reference set is a run edge.
func TestRunGateAutomationWorkflowReference(t *testing.T) {
	f, dir := runGatedTree(t)
	root := filepath.Join(dir, "automations/sales")
	replaceIn(t, filepath.Join(root, "sales-assistant.json"), `"references": [`,
		`"references": [`+"\n"+`    {"kind": "workflow", "resourceID": "close"},`)
	declare(t, root, map[string]wfdir.Dependency{"close": {Kind: markers.KindWorkflow}}, map[string]string{"close": "workflow-close"})
	f.resetRequests()
	out, err := runCLI(t, root, "automation", "push")
	assertRunRefused(t, f, out, err, "sales-assistant.json → references[0] runs the workflow workflow-close", "in feature "+financeFeature)

	declare(t, root, map[string]wfdir.Dependency{"close": {Kind: markers.KindWorkflow, Run: true}}, nil)
	if out, err := runCLI(t, root, "automation", "push"); err != nil {
		t.Fatalf("a run source bound in this stack was refused: %v\n%s", err, out)
	}
}

// TestRunGateAdmitsAnAddressDependency: a row this repository manages in the
// stack is never outside it, whatever feature that folder's stack names — so
// an address dependency needs no "run", and the gate asks nothing about it.
func TestRunGateAdmitsAnAddressDependency(t *testing.T) {
	f := gateFolder()
	f.Manifest.Dependencies["peer"] = wfdir.Dependency{Kind: markers.KindWorkflow, From: "../peer"}
	f.addressBind = map[string]string{"peer": "workflow-peer"}
	// A nil client: an address dependency must be decided without a request.
	if err := gateRunEdges(context.Background(), nil, f, []runEdge{{Where: "main.py:1", Kind: markers.KindWorkflow, ID: "workflow-peer"}}); err != nil {
		t.Fatalf("a run of a row this repository manages was refused: %v", err)
	}
}

// TestBindNeverInfersARunTarget: `ronja bind` never proposes a "run": true
// dependency, however exact the match — the row of that name is the one the
// first stack runs.
func TestBindNeverInfersARunTarget(t *testing.T) {
	f := importFixture(t)
	dir, _ := runImport(t, f)
	f.pipe.AddFeature(devFeature, "Sales dev", "organization")
	root := filepath.Join(dir, "apps/sales-dashboard")
	m := loadAnyManifest(t, root)
	main := m.Stacks["main"]
	m.Stacks["dev"] = wfdir.Stack{URL: main.URL, TenantID: main.TenantID, FeatureID: devFeature}
	if err := wfdir.SaveManifest(root, m); err != nil {
		t.Fatal(err)
	}
	f.searchHits = []api.SearchHit{{Kind: api.SearchKindWorkflow, ID: "workflow-close", Title: "close_books", Score: api.SearchScoreExact}}
	out, _ := runCLI(t, root, "bind", "--stack", "dev", "--yes", "--json")
	var result bindResult
	if err := json.Unmarshal([]byte(out), &result); err != nil {
		t.Fatalf("%v\n%s", err, out)
	}
	for _, p := range result.Proposed {
		if p.Alias == "close_books" {
			t.Fatalf("bind proposed the run target close_books → %s for dev", p.ID)
		}
	}
	said := false
	for _, u := range result.Unresolved {
		if u.Alias == "close_books" && u.Reason == bindReasonRunTarget && strings.Contains(u.Detail, "bound by hand, per stack") {
			said = true
		}
	}
	if !said {
		t.Fatalf("bind did not report close_books as a run target bound by hand: %+v", result.Unresolved)
	}
	if id, ok := loadAnyManifest(t, root).Stacks["dev"].Bind["close_books"]; ok {
		t.Fatalf("dev binds the run target close_books to %s", id)
	}
}
