package commands

import (
	"encoding/json"
	"maps"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/ronjatech/ronja-cli/internal/api"
	"github.com/ronjatech/ronja-cli/internal/config"
	"github.com/ronjatech/ronja-cli/internal/markers"
	"github.com/ronjatech/ronja-cli/internal/wfdir"
)

// The write gate, one test per rule. The unit tests drive
// judgeWorkflowTarget / gateWorkflowTargets over the targets validate reports;
// the end-to-end ones run each guarded path against the import fixture — an
// ADOPTED tree whose `main` stack deploys to collection-sales — and assert that
// a refusal leaves nothing written.

// gateFolder is an adopted workflow folder deploying stack `dev` to
// collection-dev, binding `ledger` (a write source) and `orders_daily` (an
// output) — and, in `main`, a write source dev does not bind.
func gateFolder() *folder {
	m := &wfdir.Manifest{ID: "fld-1", Dependencies: map[string]wfdir.Dependency{
		"ledger":       {Kind: markers.KindTable, Write: true},
		"orders_daily": {Kind: markers.KindTable, Output: true},
		"fx_rates":     {Kind: markers.KindTable},
		"contracts":    {Kind: markers.KindCodex, Write: true},
		"exports":      {Kind: markers.KindFolder, Write: true},
		"main_only":    {Kind: markers.KindTable, Write: true},
	}, Stacks: map[string]wfdir.Stack{
		"dev":  {FeatureID: "collection-dev"},
		"main": {FeatureID: "collection-sales", Bind: map[string]string{"main_only": "table-main-only"}},
	}}
	return &folder{Manifest: m, Stack: "dev", Binding: wfdir.Binding{FeatureID: "collection-dev"},
		Resolved: &config.Resolved{URL: "https://ronja.test"},
		Bind: map[string]string{
			"ledger": "table-dev-ledger", "orders_daily": "table-sales-daily", "fx_rates": "table-fx",
			"contracts": "cdx-dev", "exports": "/exports/dev/",
		}}
}

func tableTarget(id, feature string, readable bool) api.ValidateWriteTarget {
	return api.ValidateWriteTarget{Position: api.WritePositionOutputLiteral, TargetKind: api.WriteTargetTable,
		Path: "main.py", Line: 7, Call: "replaceTable", Arg: id, TargetID: id, Readable: readable, TargetFeatureID: feature}
}

// TestWriteGateRules: each class of write target, allowed or refused.
func TestWriteGateRules(t *testing.T) {
	codex := func(id, feature string, readable bool) api.ValidateWriteTarget {
		return api.ValidateWriteTarget{Position: api.WritePositionCodexMarker, TargetKind: api.WriteTargetCodex,
			Path: "main.py", Line: 3, Arg: id, TargetID: id, Readable: readable, TargetFeatureID: feature}
	}
	folderAt := func(p string) api.ValidateWriteTarget {
		return api.ValidateWriteTarget{Position: api.WritePositionFolderLiteral, TargetKind: api.WriteTargetFolder,
			Path: "main.py", Line: 9, Call: "saveFile", Arg: p, TargetPath: p}
	}
	cases := []struct {
		name    string
		target  api.ValidateWriteTarget
		refused bool
	}{
		{"a table in the stack's own feature", tableTarget("table-dev-x", "collection-dev", true), false},
		{"a table by name", api.ValidateWriteTarget{TargetKind: api.WriteTargetTable, Position: api.WritePositionMarker, Arg: "orders_daily", NameForm: true}, false},
		{"a write source bound in this stack", tableTarget("table-dev-ledger", "collection-finance", true), false},
		{"a table in another feature", tableTarget("table-other", "collection-finance", true), true},
		{"an unreadable table", tableTarget("table-hidden", "", false), true},
		{"an unreadable table that claims this feature", tableTarget("table-hidden", "collection-dev", false), true},
		{"an output bound to another feature's table", tableTarget("table-sales-daily", "collection-sales", true), true},
		{"a table only another stack binds", tableTarget("table-main-only", "collection-sales", true), true},
		{"a read source's table", tableTarget("table-fx", "collection-finance", true), true},
		{"a codex in the stack's own feature", codex("cdx-here", "collection-dev", true), false},
		{"a codex write source bound here", codex("cdx-dev", "collection-other", true), false},
		{"a codex in another feature", codex("cdx-fin", "collection-finance", true), true},
		{"an unreadable codex", codex("cdx-hidden", "", false), true},
		{"a folder bound here (spelled differently)", folderAt("/exports/dev"), false},
		{"a folder bound nowhere", folderAt("/exports/main"), true},
		{"a target kind this build does not know", api.ValidateWriteTarget{TargetKind: "mailbox", Arg: "mailbox-x", Path: "main.py", Line: 1}, true},
	}
	auth := gateFolder().writeAuthorityOf()
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			o, refused := judgeWorkflowTarget(auth, c.target)
			if refused != c.refused {
				t.Fatalf("refused = %v (%+v), want %v", refused, o, c.refused)
			}
		})
	}
}

// TestWriteGateRefusalNamesLineTableAndFeature: the refusal is one
// writes_outside_target naming every refused write by file and line, its id
// and — when readable — its feature.
func TestWriteGateRefusalNamesLineTableAndFeature(t *testing.T) {
	err := gateWorkflowTargets(gateFolder(), &api.ValidateResult{WriteTargets: []api.ValidateWriteTarget{
		tableTarget("table-dev-ledger", "collection-finance", true),
		tableTarget("table-other", "collection-finance", true),
		{Position: api.WritePositionMarker, TargetKind: api.WriteTargetTable, Path: "lib.py", Line: 2, Arg: "table-hidden", TargetID: "table-hidden"},
	}})
	r, ok := asManagedRefusal(err)
	if !ok || r.Reason != syncReasonWritesOutsideTarget {
		t.Fatalf("err = %v, want writes_outside_target", err)
	}
	for _, want := range []string{"main.py:7 (tools.replaceTable) writes the table table-other", "in feature collection-finance",
		"lib.py:2 writes the table table-hidden", "cannot read", `stack "dev"`, "collection-dev"} {
		if !strings.Contains(r.Detail, want) {
			t.Fatalf("refusal does not say %q:\n%s", want, r.Detail)
		}
	}
	if strings.Contains(r.Detail, "table-dev-ledger") {
		t.Fatalf("the refusal names a bound write source:\n%s", r.Detail)
	}
}

// TestWriteGateRefusesAnInstanceThatReportsNoWriteTargets: no writeTargets
// key is not "writes nothing".
func TestWriteGateRefusesAnInstanceThatReportsNoWriteTargets(t *testing.T) {
	r, ok := asManagedRefusal(gateWorkflowTargets(gateFolder(), &api.ValidateResult{}))
	if !ok || r.Reason != syncReasonNoWriteTargets {
		t.Fatalf("a result without writeTargets passed the gate, or was refused under another reason: %v", r)
	}
	// Actionable: it says what is wrong with the instance, not only that the
	// answer was missing.
	for _, want := range []string{"does not report write targets", "older than this CLI", "no writeTargets", "Nothing was written"} {
		if !strings.Contains(r.Detail, want) {
			t.Fatalf("the refusal does not say %q:\n%s", want, r.Detail)
		}
	}
	if err := gateWorkflowTargets(gateFolder(), &api.ValidateResult{WriteTargets: []api.ValidateWriteTarget{}}); err != nil {
		t.Fatalf("a candidate writing nothing was refused: %v", err)
	}
}

// ── End to end, on the imported tree ─────────────────────────────────────────

// gatedTree imports the fixture feature and adds what the gate tests write to:
// a Finance table nothing binds, a Sales table, and codexes in both features.
func gatedTree(t *testing.T) (*fakeImportInstance, string) {
	t.Helper()
	f := importFixture(t)
	f.pipe.AddTable(&api.Table{ID: "table-budget", Name: "budget", FeatureID: financeFeature, Kind: "dynamic"})
	f.pipe.AddTable(&api.Table{ID: "table-targets", Name: "targets", FeatureID: importFeature, Kind: "dynamic"})
	f.pipe.AddMetric("table-fin-metric", "fin_metric", string(canonicalRecipe(t, json.RawMessage(aovRecipe)))).FeatureID = financeFeature
	f.codexes["cdx-fin"] = api.Codex{ID: "cdx-fin", FeatureID: financeFeature, Name: "Contracts"}
	f.codexes["cdx-sales"] = api.Codex{ID: "cdx-sales", FeatureID: importFeature, Name: "Playbook"}
	dir, _ := runImport(t, f)
	// Under git, as a repository is: a single-folder command resolves its
	// address dependencies only inside one.
	if err := os.Mkdir(filepath.Join(dir, ".git"), 0o755); err != nil {
		t.Fatal(err)
	}
	return f, dir
}

func appendLine(t *testing.T, path, line string) {
	t.Helper()
	body, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, append(body, []byte(line+"\n")...), 0o644); err != nil {
		t.Fatal(err)
	}
}

// workflowWrites are the requests that change a workflow: everything but a
// read and the validate that persists nothing.
func workflowWrites(f *fakeImportInstance) []string {
	var out []string
	for _, w := range f.writes() {
		if w == "POST /api/v2/workflow/validate" {
			continue
		}
		out = append(out, w)
	}
	return out
}

// assertGateRefused fails unless err is the gate's refusal naming every want,
// and nothing but validate was sent.
func assertGateRefused(t *testing.T, f *fakeImportInstance, out string, err error, wants ...string) {
	t.Helper()
	if err == nil {
		t.Fatalf("the push went through:\n%s", out)
	}
	prefix := []string{"would write outside it"}
	if len(wants) == 1 && wants[0] == "no writeTargets" {
		prefix = nil
	}
	for _, want := range append(prefix, wants...) {
		if !strings.Contains(err.Error(), want) {
			t.Fatalf("the refusal does not say %q:\n%v", want, err)
		}
	}
	if writes := workflowWrites(f); len(writes) != 0 {
		t.Fatalf("a refused push sent %v", writes)
	}
}

// TestWriteGateRefusesAWorkflowPushWritingAnotherFeature: an output literal
// naming a Finance table no write source binds is refused before the push
// writes anything; the bound one (ledger) and the own output pass.
func TestWriteGateRefusesAWorkflowPushWritingAnotherFeature(t *testing.T) {
	f, dir := gatedTree(t)
	root := filepath.Join(dir, "workflows/sales-report")
	appendLine(t, filepath.Join(root, "main.py"), `tools.appendToTable(fx, "table-budget")`)
	f.resetRequests()
	out, err := runCLI(t, root, "wf", "push")
	assertGateRefused(t, f, out, err, "main.py:7 (tools.appendToTable) writes the table table-budget (budget)", "in feature "+financeFeature)
}

// TestWriteGateAdmitsTheStacksOwnWrites: an edit to a workflow whose writes are
// its own output, a Sales table and the bound write source pushes — the gate
// read every target and refused none.
func TestWriteGateAdmitsTheStacksOwnWrites(t *testing.T) {
	f, dir := gatedTree(t)
	root := filepath.Join(dir, "workflows/sales-report")
	appendLine(t, filepath.Join(root, "main.py"), `tools.appendToTable(orders, "table-targets")`)
	if out, err := runCLI(t, root, "wf", "push"); err != nil {
		t.Fatalf("push: %v\n%s", err, out)
	}
	last := f.wf.validated[len(f.wf.validated)-1]
	if len(last.Files) != 1 || !strings.Contains(last.Files[0].Content, "table-ledger") || !strings.Contains(last.Files[0].Content, "table-daily") {
		t.Fatalf("validate was not posted the whole folder in wire form: %+v", last.Files)
	}
}

// TestWriteGateRefusesAnUnreadableTarget: a table this credential cannot read
// is outside — nothing can say where it lands.
func TestWriteGateRefusesAnUnreadableTarget(t *testing.T) {
	f, dir := gatedTree(t)
	f.pipe.AddTable(&api.Table{ID: "table-hidden", Name: "hidden", FeatureID: importFeature, Kind: "dynamic"})
	f.unreadable["table-hidden"] = true
	root := filepath.Join(dir, "workflows/sales-report")
	appendLine(t, filepath.Join(root, "main.py"), `tools.appendToTable(fx, "table-hidden")`)
	f.resetRequests()
	out, err := runCLI(t, root, "wf", "push")
	assertGateRefused(t, f, out, err, "table-hidden", "cannot read")
}

// TestWriteGateRefusesAnOutputBoundToAnotherFeature: an `output` dependency is
// not a write source — bound to a table in another feature (a bind copied from
// another stack), its `{{ write() }}` is refused like any foreign table.
func TestWriteGateRefusesAnOutputBoundToAnotherFeature(t *testing.T) {
	f, dir := gatedTree(t)
	root := filepath.Join(dir, "workflows/sales-report")
	m := loadAnyManifest(t, root)
	st := m.Stacks["main"]
	st.Bind["orders_daily"] = "table-budget"
	m.Stacks["main"] = st
	if err := wfdir.SaveManifest(root, m); err != nil {
		t.Fatal(err)
	}
	f.resetRequests()
	out, err := runCLI(t, root, "wf", "push")
	assertGateRefused(t, f, out, err, "main.py:5 writes the table table-budget", financeFeature)
}

// TestWriteGateFolderWrites: a folder write scope is admitted only by a folder
// write source bound in this stack.
func TestWriteGateFolderWrites(t *testing.T) {
	f, dir := gatedTree(t)
	root := filepath.Join(dir, "workflows/sales-report")
	appendLine(t, filepath.Join(root, "main.py"), `tools.saveFile(b, "x.csv", folder="/exports/daily")`)
	f.resetRequests()
	out, err := runCLI(t, root, "wf", "push")
	assertGateRefused(t, f, out, err, "main.py:7 (tools.saveFile) writes into the folder /exports/daily", `"kind": "folder"`)

	m := loadAnyManifest(t, root)
	m.Dependencies["exports"] = wfdir.Dependency{Kind: markers.KindFolder, Write: true}
	st := m.Stacks["main"]
	st.Bind["exports"] = "/exports//daily/"
	m.Stacks["main"] = st
	if err := wfdir.SaveManifest(root, m); err != nil {
		t.Fatal(err)
	}
	if out, err := runCLI(t, root, "wf", "push"); err != nil {
		t.Fatalf("a folder write bound in this stack was refused: %v\n%s", err, out)
	}
}

// TestWriteGateCodexWrites: a codex marker is a write (a run may upsert and
// delete); one in another feature is refused, one in the stack's feature
// passes.
func TestWriteGateCodexWrites(t *testing.T) {
	f, dir := gatedTree(t)
	root := filepath.Join(dir, "workflows/sales-report")
	main := filepath.Join(root, "main.py")
	appendLine(t, main, `cx = {{ codex('cdx-fin') }}`)
	f.resetRequests()
	out, err := runCLI(t, root, "wf", "push")
	assertGateRefused(t, f, out, err, "main.py:7 writes the codex cdx-fin (Contracts)", financeFeature)

	body := strings.Replace(readFile(t, main), "cdx-fin", "cdx-sales", 1)
	if err := os.WriteFile(main, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
	if out, err := runCLI(t, root, "wf", "push"); err != nil {
		t.Fatalf("a codex of the stack's own feature was refused: %v\n%s", err, out)
	}
}

// TestWriteGateRefusesWhenTheInstanceReportsNoWriteTargets: an instance older
// than validate's writeTargets cannot vouch for anything, so an adopted
// folder's push is refused — even under --no-validate, which does not skip
// the gate.
func TestWriteGateRefusesWhenTheInstanceReportsNoWriteTargets(t *testing.T) {
	f, dir := gatedTree(t)
	f.wf.noWriteTargets = true
	root := filepath.Join(dir, "workflows/sales-report")
	appendLine(t, filepath.Join(root, "main.py"), "# touched")
	f.resetRequests()
	out, err := runCLI(t, root, "wf", "push", "--no-validate")
	assertGateRefused(t, f, out, err, "no writeTargets")
}

// TestWriteGateLeavesAClassicFolderAlone: the same write outside the
// feature, in a folder with no folder id, pushes exactly as before.
func TestWriteGateLeavesAClassicFolderAlone(t *testing.T) {
	f, dir := gatedTree(t)
	root := filepath.Join(dir, "workflows/sales-report")
	m := loadAnyManifest(t, root)
	m.ID = ""
	if err := wfdir.SaveManifest(root, m); err != nil {
		t.Fatal(err)
	}
	appendLine(t, filepath.Join(root, "main.py"), `tools.appendToTable(fx, "table-budget")`)
	f.wf.noWriteTargets = true
	if out, err := runCLI(t, root, "wf", "push"); err != nil {
		t.Fatalf("a classic folder's push was gated: %v\n%s", err, out)
	}
	if !strings.Contains(f.wf.FileContents(f.wf.draftOf["workflow-report"])["main.py"], "table-budget") {
		t.Fatal("the classic push did not land")
	}
}

// draftWritingOutside opens a draft of the imported workflow and then edits it
// "in the web app" to write a Finance table — the draft the publish and the
// test would act on.
func draftWritingOutside(t *testing.T, f *fakeImportInstance, root string) string {
	t.Helper()
	appendLine(t, filepath.Join(root, "main.py"), "# a change")
	if out, err := runCLI(t, root, "wf", "push"); err != nil {
		t.Fatalf("push: %v\n%s", err, out)
	}
	draft := f.wf.draftOf["workflow-report"]
	if draft == "" {
		t.Fatal("no draft was opened")
	}
	content := f.wf.FileContents(draft)["main.py"] + "tools.appendToTable(fx, \"table-budget\")\n"
	f.wf.putFile(draft, "main.py", content)
	f.resetRequests()
	return draft
}

// TestWriteGateRefusesAPublishOfADraftWritingOutside: `wf publish` checks the
// draft the server holds, which the web app may have edited.
func TestWriteGateRefusesAPublishOfADraftWritingOutside(t *testing.T) {
	f, dir := gatedTree(t)
	root := filepath.Join(dir, "workflows/sales-report")
	draftWritingOutside(t, f, root)
	out, err := runCLI(t, root, "wf", "publish")
	assertGateRefused(t, f, out, err, "table-budget", financeFeature)
	if len(f.wf.committed) != 0 || len(f.wf.publishedIDs) != 0 || len(f.wf.reviewRequested) != 0 {
		t.Fatalf("a refused publish committed %v / published %v / requested review %v", f.wf.committed, f.wf.publishedIDs, f.wf.reviewRequested)
	}
}

// TestWriteGateRefusesATestRunOfADraftWritingOutside: `wf test` runs the
// draft, and a run is a write.
func TestWriteGateRefusesATestRunOfADraftWritingOutside(t *testing.T) {
	f, dir := gatedTree(t)
	root := filepath.Join(dir, "workflows/sales-report")
	draftWritingOutside(t, f, root)
	out, err := runCLI(t, root, "wf", "test", "--write-live")
	assertGateRefused(t, f, out, err, "table-budget")
}

// TestWriteGateAppCodexAccess: a codex in an app's access is written by the app
// (its token covers upsert and delete). Another feature's is refused, unless a
// codex write source binds it in this stack; the stack's own passes.
func TestWriteGateAppCodexAccess(t *testing.T) {
	f, dir := gatedTree(t)
	root := filepath.Join(dir, "apps/sales-dashboard")
	setCodexAccess := func(entry string, deps map[string]wfdir.Dependency, bind map[string]string) {
		m := loadAnyManifest(t, root)
		access := m.DeclaredAccess()
		access.AllowedCodexIDs = []string{entry}
		m.Access = &access
		for k, v := range deps {
			m.Dependencies[k] = v
		}
		st := m.Stacks["main"]
		if st.Bind == nil {
			st.Bind = map[string]string{}
		}
		for k, v := range bind {
			st.Bind[k] = v
		}
		m.Stacks["main"] = st
		if err := wfdir.SaveManifest(root, m); err != nil {
			t.Fatal(err)
		}
	}
	setCodexAccess("cdx-fin", nil, nil)
	f.resetRequests()
	out, err := runCLI(t, root, "app", "push")
	assertGateRefused(t, f, out, err, "access.allowedCodexIDs[0] writes the codex cdx-fin", financeFeature)

	setCodexAccess("cdx-sales", nil, nil)
	if out, err := runCLI(t, root, "app", "push"); err != nil {
		t.Fatalf("a codex of the stack's own feature was refused: %v\n%s", err, out)
	}

	setCodexAccess("contracts", map[string]wfdir.Dependency{"contracts": {Kind: markers.KindCodex, Write: true}}, map[string]string{"contracts": "cdx-fin"})
	if out, err := runCLI(t, root, "app", "push"); err != nil {
		t.Fatalf("a codex write source bound in this stack was refused: %v\n%s", err, out)
	}
}

// TestWriteGateAutomationReferenceSet: a table in an agent action's reference
// set may be written by the headless run. A Finance table is refused; a Sales
// table and a Finance METRIC (a recipe — nothing writes one) pass.
func TestWriteGateAutomationReferenceSet(t *testing.T) {
	f, dir := gatedTree(t)
	root := filepath.Join(dir, "automations/sales")
	path := filepath.Join(root, "sales-assistant.json")
	original := readFile(t, path)
	withRef := func(id string) {
		body := strings.Replace(original, `"references": [`, `"references": [`+"\n"+`    {"kind": "model", "resourceID": "`+id+`"},`, 1)
		if body == original {
			t.Fatalf("no references list in:\n%s", original)
		}
		if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	withRef("table-budget")
	f.resetRequests()
	out, err := runCLI(t, root, "automation", "push")
	assertGateRefused(t, f, out, err, "sales-assistant.json → references[0] writes the table table-budget (budget)", financeFeature)

	for _, id := range []string{"table-targets", "table-fin-metric"} {
		withRef(id)
		if out, err := runCLI(t, root, "automation", "push"); err != nil {
			t.Fatalf("a reference to %s was refused: %v\n%s", id, err, out)
		}
	}
}

// TestWriteGateRefusesAnAppPublishGrantingAnotherFeaturesCodex: `app publish`
// checks the access the DRAFT carries — the web app may have widened it since
// the folder pushed — before anything is committed.
func TestWriteGateRefusesAnAppPublishGrantingAnotherFeaturesCodex(t *testing.T) {
	f, dir := gatedTree(t)
	root := filepath.Join(dir, "apps/sales-dashboard")
	appendLine(t, filepath.Join(root, "App.tsx"), "// a change")
	if out, err := runCLI(t, root, "app", "push"); err != nil {
		t.Fatalf("push: %v\n%s", err, out)
	}
	draft := f.app.apps[f.app.draftOf["data_app-dash"]]
	if draft == nil {
		t.Fatal("no draft was opened")
	}
	draft.AllowedCodexIDs = append(draft.AllowedCodexIDs, "cdx-fin")
	f.resetRequests()
	out, err := runCLI(t, root, "app", "publish")
	assertGateRefused(t, f, out, err, "access.allowedCodexIDs[0] writes the codex cdx-fin", financeFeature)
	if len(f.app.committed) != 0 {
		t.Fatalf("a refused publish committed %v", f.app.committed)
	}
}

// withFinanceFeatureReference stages the fixture's agent automation with a
// second feature reference — to the sibling feature Finance — as the inventory
// reports it: read_write, because the run is given every table of Finance.
func withFinanceFeatureReference(f *fakeImportInstance) {
	f.auto.mu.Lock()
	row := f.auto.rows["sjob-assistant"]
	row.Action.Config.References = append(row.Action.Config.References, api.AutomationReference{Kind: "feature", ResourceID: financeFeature})
	f.auto.mu.Unlock()
	f.inv.mu.Lock()
	defer f.inv.mu.Unlock()
	for i := range f.inv.resources[importFeature] {
		res := &f.inv.resources[importFeature][i]
		if res.ID == "sjob-assistant" {
			res.References = append(res.References, api.ReferencePosition{Position: api.RefPosAutomationRef, Direction: api.RefDirReadWrite,
				TargetKind: "feature", TargetID: financeFeature, Readable: true, TargetName: "Finance", TargetFeatureID: financeFeature,
				Field: "references[3]"})
		}
	}
}

// featureAliasIn names the alias an automation folder declares for a feature
// reference, and its declaration.
func featureAliasIn(t *testing.T, root string) (string, wfdir.Dependency) {
	t.Helper()
	m := loadAnyManifest(t, root)
	for alias, dep := range m.Dependencies {
		if dep.Kind == markers.KindFeature {
			return alias, dep
		}
	}
	t.Fatalf("%s declares no feature dependency: %+v", root, m.Dependencies)
	return "", wfdir.Dependency{}
}

// TestWriteGateAutomationFeatureReference is the run-edge rule for a feature
// reference:
// an imported agent automation names the sibling feature Finance, which its run
// may write throughout. Import declares it a write source bound in `main`, so
// `main` still applies with nothing written; a `dev` stack that binds it to
// Finance WITHOUT "write": true is refused writes_outside_target before any
// automation is created, and with "write": true and the bind it is admitted.
func TestWriteGateAutomationFeatureReference(t *testing.T) {
	f := importFixture(t)
	withFinanceFeatureReference(f)
	autoRoot := "automations/sales"
	f, dir, imported := freshStackFixtureFrom(t, f, nil)
	alias, dep := featureAliasIn(t, filepath.Join(dir, autoRoot))
	if !dep.Write {
		t.Fatalf("the import declared the Finance feature %q as %+v, want a write source", alias, dep)
	}
	declared := false
	for _, s := range imported.DataSources {
		if s.Folder == autoRoot && s.Alias == alias && s.Access == "write" && s.BoundTo == financeFeature {
			declared = true
		}
	}
	if !declared {
		t.Fatalf("the import report does not name %s as a write source bound to %s: %+v", alias, financeFeature, imported.DataSources)
	}

	// The reviewer's scenario: dev binds the alias to Finance, and the
	// dependency does not say "write".
	root := filepath.Join(dir, autoRoot)
	m := loadAnyManifest(t, root)
	m.Dependencies[alias] = wfdir.Dependency{Kind: markers.KindFeature}
	st := m.Stacks["dev"]
	st.Bind[alias] = financeFeature
	m.Stacks["dev"] = st
	if err := wfdir.SaveManifest(root, m); err != nil {
		t.Fatal(err)
	}
	report, err := applyStack(t, dir, "dev", "--allow-create")
	if err == nil {
		t.Fatalf("dev applied an automation writing Finance:\n%s", describeApply(report))
	}
	var line *syncApplyFolderReport
	for i := range report.Folders {
		if report.Folders[i].Path == autoRoot {
			line = &report.Folders[i]
		}
	}
	if line == nil || line.Verdict != verdictRefused || line.Reason != syncReasonWritesOutsideTarget ||
		!strings.Contains(line.Detail, "sales-assistant.json → references[") || !strings.Contains(line.Detail, "every table of the feature "+financeFeature) {
		t.Fatalf("the automation was not refused writes_outside_target naming Finance:\n%s", describeApply(report))
	}
	if autos := devRows(f, devFeature).automations; len(autos) != 0 {
		t.Fatalf("a refused automation folder created %v in dev", keysOf(autos))
	}

	// Declared a write source, and bound in dev: a deliberate decision, admitted.
	m = loadAnyManifest(t, root)
	m.Dependencies[alias] = wfdir.Dependency{Kind: markers.KindFeature, Write: true}
	if err := wfdir.SaveManifest(root, m); err != nil {
		t.Fatal(err)
	}
	report, err = applyStack(t, dir, "dev", "--allow-create")
	if err != nil || report.Verdict != verdictApplied {
		t.Fatalf("a bound feature write source was refused: %v\n%s", err, describeApply(report))
	}
	assistant := devRows(f, devFeature).automations["Sales assistant"]
	if assistant == nil || assistant.Action == nil {
		t.Fatal("no dev assistant automation")
	}
	refs := map[string]bool{}
	for _, ref := range assistant.Action.Config.References {
		if ref.Kind == "feature" {
			refs[ref.ResourceID] = true
		}
	}
	if !refs[devFeature] || !refs[financeFeature] {
		t.Fatalf("the dev assistant references features %v, want its own and Finance", refs)
	}
}

// TestImportedFeatureWriteSourceAppliesToMainUnchanged: the importing stack
// binds the Finance feature as a write source, so `sync apply --stack main`
// over the freshly imported tree writes nothing.
func TestImportedFeatureWriteSourceAppliesToMainUnchanged(t *testing.T) {
	f := importFixture(t)
	withFinanceFeatureReference(f)
	dir, _ := runImport(t, f)
	f.resetRequests()
	report, err := applyStack(t, dir, "main")
	if err != nil || report.Verdict != verdictApplied {
		t.Fatalf("apply --stack main: %v\n%s", err, describeApply(report))
	}
	if writes := f.writes(); len(writes) != 0 {
		t.Fatalf("apply --stack main sent %v", writes)
	}
}

// TestWriteGateRefusesALiveRunWritingOutside: `wf run` runs the LIVE row, and
// a run is a write — so an adopted folder's run is gated on the live code as
// the server holds it. Here a colleague republished the workflow in the web
// app to write a Finance table; the folder itself is unchanged.
func TestWriteGateRefusesALiveRunWritingOutside(t *testing.T) {
	f, dir := gatedTree(t)
	root := filepath.Join(dir, "workflows/sales-report")
	f.wf.putFile("workflow-report", "main.py", f.wf.FileContents("workflow-report")["main.py"]+"tools.appendToTable(fx, \"table-budget\")\n")
	f.resetRequests()
	out, err := runCLI(t, root, "wf", "run")
	assertGateRefused(t, f, out, err, "main.py:7 (tools.appendToTable) writes the table table-budget (budget)", "in feature "+financeFeature)
}

// TestAnAdoptedFolderWhoseStackNamesNoFeatureIsRefused: a folder with an id
// whose stack lost its "featureID" would otherwise run legacy — unclaimed and
// ungated — and push code writing a Finance table without a word. Refused by
// name, with the remedy, by a single-folder push and by `sync apply`.
func TestAnAdoptedFolderWhoseStackNamesNoFeatureIsRefused(t *testing.T) {
	f, dir := gatedTree(t)
	root := filepath.Join(dir, "workflows/sales-report")
	m := loadAnyManifest(t, root)
	st := m.Stacks["main"]
	st.FeatureID = ""
	m.Stacks["main"] = st
	if err := wfdir.SaveManifest(root, m); err != nil {
		t.Fatal(err)
	}
	appendLine(t, filepath.Join(root, "main.py"), `tools.appendToTable(fx, "table-budget")`)
	f.resetRequests()
	out, err := runCLI(t, root, "wf", "push")
	if err == nil {
		t.Fatalf("an adopted folder with no feature pushed:\n%s", out)
	}
	r, ok := asManagedRefusal(err)
	if !ok || r.Reason != syncReasonNoFeature || !strings.Contains(r.Detail, `names no "featureID"`) || !strings.Contains(r.Detail, `"unmanaged": true`) {
		t.Fatalf("err = %v, want no_feature naming the remedy", err)
	}
	if writes := workflowWrites(f); len(writes) != 0 {
		t.Fatalf("the refused push sent %v", writes)
	}

	report, err := applyStack(t, dir, "main")
	if err == nil {
		t.Fatalf("apply deployed it:\n%s", describeApply(report))
	}
	for _, line := range report.Folders {
		if line.Path == "workflows/sales-report" && (line.Verdict != verdictRefused || line.Reason != syncReasonNoFeature) {
			t.Fatalf("apply: %s / %s — %s", line.Verdict, line.Reason, line.Detail)
		}
	}
	if writes := workflowWrites(f); len(writes) != 0 {
		t.Fatalf("apply sent %v for it", writes)
	}
}

// TestWriteGateNotesABindOutsideTheStacksFeature: an admitted write bind that
// lands in another feature is not refused but is said — and said with
// the other stack's name when that stack binds the same row, which is what a
// bind copied between stacks looks like.
func TestWriteGateNotesABindOutsideTheStacksFeature(t *testing.T) {
	_, dir := gatedTree(t)
	root := filepath.Join(dir, "workflows/sales-report")
	appendLine(t, filepath.Join(root, "main.py"), "# touched")
	var out string
	var err error
	stderr := captureStderr(t, func() { out, err = runCLI(t, root, "wf", "push") })
	if err != nil {
		t.Fatalf("push: %v\n%s", err, out)
	}
	want := "Note: main.py:6 (tools.replaceTable) writes the table table-ledger in feature " + financeFeature +
		`, outside stack "main"'s feature ` + importFeature + ` — admitted because "ledger" is bound for this stack on purpose`
	if !strings.Contains(stderr, want) {
		t.Fatalf("the push did not note the bind outside the feature:\n%s", stderr)
	}
	if strings.Contains(stderr, "binds the same row") {
		t.Fatalf("a bind no other stack shares was reported as shared:\n%s", stderr)
	}

	// A second stack binding the very same row.
	m := loadAnyManifest(t, root)
	main := m.Stacks["main"]
	bind := map[string]string{}
	for k, v := range main.Bind {
		bind[k] = v
	}
	m.Stacks["dev"] = wfdir.Stack{URL: main.URL, TenantID: main.TenantID, FeatureID: devFeature, Bind: bind}
	if err := wfdir.SaveManifest(root, m); err != nil {
		t.Fatal(err)
	}
	appendLine(t, filepath.Join(root, "main.py"), "# touched again")
	stderr = captureStderr(t, func() { out, err = runCLI(t, root, "wf", "push", "--stack", "main") })
	if err != nil {
		t.Fatalf("push: %v\n%s", err, out)
	}
	if !strings.Contains(stderr, `stack "dev" binds the same row, so this stack writes what that one does`) {
		t.Fatalf("the note does not name the stack binding the same row:\n%s", stderr)
	}
	// ACTION FIRST: the line that names what to check, then the why, indented.
	if !strings.Contains(stderr, `, which stack "dev" binds too — check the bind for "ledger" is meant for this stack.`+"\n          It was admitted because") {
		t.Fatalf("the note does not lead with what to check:\n%s", stderr)
	}
}

// TestApplyReportsGateNotesOnTheFolderLine: under `sync apply` the same notes
// land on the folder's line — here the dev app's run bind to the Finance
// workflow, which main binds too.
func TestApplyReportsGateNotesOnTheFolderLine(t *testing.T) {
	_, dir, _ := freshStackFixture(t)
	report, err := applyStack(t, dir, "dev", "--allow-create")
	if err != nil {
		t.Fatalf("apply: %v\n%s", err, describeApply(report))
	}
	for _, line := range report.Folders {
		if line.Path != "apps/sales-dashboard" {
			continue
		}
		for _, n := range line.Notes {
			if strings.Contains(n, "runs the workflow workflow-close (Close books) in feature "+financeFeature) &&
				strings.Contains(n, `stack "main" binds the same row`) {
				return
			}
		}
		t.Fatalf("the app's line carries no note on its run bind outside dev: %+v", line.Notes)
	}
	t.Fatal("no app line")
}

// TestDryRunGatesAnUpdate: a dry run previews the gate for an UPDATE too — the
// push that gates it never runs in a dry run, and a preview that said applied
// for a folder the apply then refuses is a green pull-request check of a
// deploy that fails after the merge.
func TestDryRunGatesAnUpdate(t *testing.T) {
	f, dir := gatedTree(t)
	appendLine(t, filepath.Join(dir, "workflows/sales-report", "main.py"), `tools.appendToTable(fx, "table-budget")`)
	f.resetRequests()
	report, err := applyStack(t, dir, "main", "--dry-run")
	if err == nil {
		t.Fatalf("the dry run passed a folder the apply refuses:\n%s", describeApply(report))
	}
	for _, line := range report.Folders {
		if line.Path == "workflows/sales-report" && line.Reason != syncReasonWritesOutsideTarget {
			t.Fatalf("dry run: %s / %s — %s", line.Verdict, line.Reason, line.Detail)
		}
	}
	if writes := workflowWrites(f); len(writes) != 0 {
		t.Fatalf("the dry run sent %v", writes)
	}
}

// ONE GATE POINT FOR EVERY KIND. A colleague's web edit makes the live
// workflow write a Finance table. Taken into the folder by hand (or by an
// older pull), it used to deploy green for ever: the push gated only after its
// "live already holds this folder" shortcut, so `sync apply` said "nothing to
// push" while `wf run` refused the same row. The gate now runs first.
func TestApplyGatesAFolderLiveAlreadyHolds(t *testing.T) {
	f, dir := gatedTree(t)
	root := filepath.Join(dir, "workflows/sales-report")
	line := `tools.appendToTable(fx, "table-budget")`
	f.wf.files["workflow-report"][0].Content += line + "\n"
	appendLine(t, filepath.Join(root, "main.py"), line)
	f.resetRequests()
	report, err := applyStack(t, dir, "main")
	if err == nil {
		t.Fatalf("a folder writing outside its feature applied because live already held it:\n%s", describeApply(report))
	}
	for _, l := range report.Folders {
		if l.Path == "workflows/sales-report" && l.Reason != syncReasonWritesOutsideTarget {
			t.Fatalf("apply: %s / %s — %s", l.Verdict, l.Reason, l.Detail)
		}
	}
}

// …and `pull` refuses to take such a live row in, before writing a file — a
// dry run included — for a workflow and for an automation, whose pull used to
// write the files and then end its gate refusal with "Nothing was written".
func TestPullRefusesALiveRowThatWritesOutside(t *testing.T) {
	t.Run("workflow", func(t *testing.T) {
		f, dir := gatedTree(t)
		root := filepath.Join(dir, "workflows/sales-report")
		f.wf.files["workflow-report"][0].Content += `tools.appendToTable(fx, "table-budget")` + "\n"
		f.wf.versions["workflow-report"] = append([]api.Workflow{{ID: "wfv-web", CommittedAt: webPublishedAt()}}, f.wf.versions["workflow-report"]...)
		before := snapshotFolder(t, root, false)
		for _, args := range [][]string{{"wf", "pull", "--force", "--dry-run"}, {"wf", "pull", "--force"}} {
			out, err := runCLI(t, root, args...)
			if r, ok := asManagedRefusal(err); !ok || r.Reason != syncReasonWritesOutsideTarget || !strings.Contains(r.Detail, "table-budget") {
				t.Fatalf("%v: %v\n%s", args, err, out)
			}
			if after := snapshotFolder(t, root, false); !maps.Equal(before, after) {
				t.Fatalf("%v changed the folder", args)
			}
		}
	})
	t.Run("automation", func(t *testing.T) {
		f, dir := gatedTree(t)
		root := filepath.Join(dir, "automations/sales")
		row := f.auto.rows["sjob-assistant"]
		row.Action.Config.References = append([]api.AutomationReference{{Kind: "model", ResourceID: "table-budget"}}, row.Action.Config.References...)
		row.UpdatedAt = row.UpdatedAt.Add(time.Hour)
		before := snapshotFolder(t, root, false)
		out, err := runCLI(t, root, "automation", "pull", "--force")
		if r, ok := asManagedRefusal(err); !ok || r.Reason != syncReasonWritesOutsideTarget || !strings.Contains(r.Detail, "table-budget") {
			t.Fatalf("automation pull: %v\n%s", err, out)
		}
		if after := snapshotFolder(t, root, false); !maps.Equal(before, after) {
			t.Fatal("the refused pull wrote the folder")
		}
	})
}

// RUN EDGES FAIL CLOSED. Validate leaves a workflow or agent the caller cannot
// reach out of `resolved` and reports it as a finding instead; read off
// `resolved` alone, the gate saw no edge at all and `wf run` started a live row
// that runs something nobody here can place in a feature.
func TestWriteGateRefusesARunEdgeTheCredentialCannotReach(t *testing.T) {
	f, dir := gatedTree(t)
	root := filepath.Join(dir, "workflows/sales-report")
	f.wf.putFile("workflow-report", "main.py", f.wf.FileContents("workflow-report")["main.py"]+"tools.runWorkflow(\"{{ workflow('workflow-hidden') }}\")\n")
	f.resetRequests()
	out, err := runCLI(t, root, "wf", "run")
	r, ok := asManagedRefusal(err)
	if !ok || r.Reason != syncReasonRunsOutsideTarget || !strings.Contains(r.Detail, "workflow-hidden") || !strings.Contains(r.Detail, "cannot read") {
		t.Fatalf("the unreachable run edge was not refused: %v\n%s", err, out)
	}
	if writes := workflowWrites(f); len(writes) != 0 {
		t.Fatalf("the refused run sent %v", writes)
	}
}

// A run-edge leg over the binding cap is resolved by nobody: refused.
func TestWriteGateRefusesARunEdgeLegOverTheCap(t *testing.T) {
	err := tooManyRunEdges(gateFolder(), &api.ValidateResult{Findings: []api.ValidateFinding{
		{Severity: api.SeverityError, Code: "too_many_bindings", Path: "workflowIDs"},
		{Severity: api.SeverityError, Code: "too_many_bindings", Path: "secretIDs"},
	}})
	r, ok := asManagedRefusal(err)
	if !ok || r.Reason != syncReasonRunsOutsideTarget || !strings.Contains(r.Detail, "workflowIDs runs more workflows or agents") || strings.Contains(r.Detail, "secretIDs") {
		t.Fatalf("err = %v", err)
	}
}

// THE OFF-SWITCH IS SAID OUT LOUD. Marking a stack "unmanaged", or deleting a
// folder's "id", turns the write gate off for it — one committed line a
// reviewer may not read as "this now writes Main". `sync apply` notes each
// folder it deploys unmanaged: one whose stack is marked so, and one with no
// id in a tree whose other folders are adopted. An all-classic tree gets no
// notes.
func TestApplyNotesAFolderRunningUnmanaged(t *testing.T) {
	_, dir := gatedTree(t)
	wfRoot := filepath.Join(dir, "workflows/sales-report")
	m := loadAnyManifest(t, wfRoot)
	st := m.Stacks["main"]
	st.Unmanaged = true
	m.Stacks["main"] = st
	if err := wfdir.SaveManifest(wfRoot, m); err != nil {
		t.Fatal(err)
	}
	appRoot := filepath.Join(dir, "apps/sales-dashboard")
	am := loadAnyManifest(t, appRoot)
	am.RemoveFolderID()
	if err := wfdir.SaveManifest(appRoot, am); err != nil {
		t.Fatal(err)
	}
	report, _ := applyStack(t, dir, "main", "--dry-run")
	notes := map[string]string{}
	for _, l := range report.Folders {
		notes[l.Path] = strings.Join(l.Notes, " | ")
	}
	if !strings.Contains(notes["workflows/sales-report"], `marked "unmanaged"`) || !strings.Contains(notes["workflows/sales-report"], "not checked") {
		t.Fatalf("the unmanaged stack was not noted: %q", notes["workflows/sales-report"])
	}
	if !strings.Contains(notes["apps/sales-dashboard"], "no folder id") {
		t.Fatalf("the folder without an id in an adopted tree was not noted: %q", notes["apps/sales-dashboard"])
	}
	if strings.Contains(notes["pipelines/sales"], "unmanaged") || strings.Contains(notes["pipelines/sales"], "no folder id") {
		t.Fatalf("an adopted folder was noted: %q", notes["pipelines/sales"])
	}
}

// TestAnAddressIntoAnotherFeatureIsNoted: a row this repository manages by
// address is admitted as a run edge — but when it lives outside the stack's
// feature (here the workflow folder's dev stack was copied from main and still
// names Main's feature), that is the copied-stack slip a bind would have been
// noted for, and it is noted too.
func TestAnAddressIntoAnotherFeatureIsNoted(t *testing.T) {
	_, dir, _ := freshStackFixture(t)
	root := filepath.Join(dir, "workflows/sales-report")
	m := loadAnyManifest(t, root)
	st := m.Stacks["dev"]
	st.FeatureID = importFeature
	m.Stacks["dev"] = st
	if err := wfdir.SaveManifest(root, m); err != nil {
		t.Fatal(err)
	}
	report, _ := applyStack(t, dir, "dev", "--allow-create", "--dry-run")
	for _, l := range report.Folders {
		if l.Path != "automations/sales" {
			continue
		}
		notes := strings.Join(l.Notes, " | ")
		if !strings.Contains(notes, "workflow-report") || !strings.Contains(notes, importFeature) || !strings.Contains(notes, "by address") {
			t.Fatalf("the run edge into Main's feature by address was not noted: %q (%s / %s)", notes, l.Verdict, l.Reason)
		}
		return
	}
	t.Fatal("no automations/sales line")
}

// TestWriteGateRefusesAnAppTestGrantingAnotherFeaturesCodex: `app test`
// renders the draft WITH its grants — and a render with --steps clicks real
// buttons — so, like `wf test`, it is gated on the access the draft carries.
func TestWriteGateRefusesAnAppTestGrantingAnotherFeaturesCodex(t *testing.T) {
	f, dir := gatedTree(t)
	root := filepath.Join(dir, "apps/sales-dashboard")
	appendLine(t, filepath.Join(root, "App.tsx"), "// a change")
	if out, err := runCLI(t, root, "app", "push"); err != nil {
		t.Fatalf("push: %v\n%s", err, out)
	}
	draft := f.app.apps[f.app.draftOf["data_app-dash"]]
	draft.AllowedCodexIDs = append(draft.AllowedCodexIDs, "cdx-fin")
	f.resetRequests()
	out, err := runCLI(t, root, "app", "test")
	if r, ok := asManagedRefusal(err); !ok || r.Reason != syncReasonWritesOutsideTarget || !strings.Contains(r.Detail, "cdx-fin") {
		t.Fatalf("app test of a draft granting another feature's codex: %v\n%s", err, out)
	}
	f.mu.Lock()
	requests := slices.Clone(f.requests)
	f.mu.Unlock()
	for _, r := range requests {
		if strings.HasSuffix(r, "/preview") {
			t.Fatalf("the refused test rendered the app: %s", r)
		}
	}
}

// TestAGateRefusalAfterSettleSaysWhatWasWritten: `sync apply` settles a
// folder's records (here a `moved` re-keys a claim) before its push runs the
// gate, so a gate refusal there must not end "Nothing was written": the
// records were. No row was.
func TestAGateRefusalAfterSettleSaysWhatWasWritten(t *testing.T) {
	_, dir := gatedTree(t)
	root := filepath.Join(dir, "automations/sales")
	if err := os.Rename(filepath.Join(root, "sales-assistant.json"), filepath.Join(root, "assistant.json")); err != nil {
		t.Fatal(err)
	}
	m := loadAnyManifest(t, root)
	m.Moved = []wfdir.MovedEntry{{From: "sales-assistant.json", To: "assistant.json"}}
	if err := wfdir.SaveManifest(root, m); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(root, "assistant.json")
	body := readFile(t, path)
	if err := os.WriteFile(path, []byte(strings.Replace(body, `"references": [`, `"references": [`+"\n"+`    {"kind": "model", "resourceID": "table-budget"},`, 1)), 0o644); err != nil {
		t.Fatal(err)
	}
	report, _ := applyStack(t, dir, "main")
	for _, l := range report.Folders {
		if l.Path != "automations/sales" {
			continue
		}
		if l.Reason != syncReasonWritesOutsideTarget {
			t.Fatalf("apply: %s / %s — %s", l.Verdict, l.Reason, l.Detail)
		}
		if strings.Contains(l.Detail, "Nothing was written") || !strings.Contains(l.Detail, "records in Ronja") {
			t.Fatalf("the refusal after the move was settled says: %s", l.Detail)
		}
		return
	}
	t.Fatal("no automations/sales line")
}

// TestWriteGateSendsAJSFolderLanguage: the server finds a js workflow's writes
// only when it is told the language — scanned as Python, main.js reports no
// write and the gate passes what it never saw.
func TestWriteGateSendsAJSFolderLanguage(t *testing.T) {
	js := &folder{Manifest: &wfdir.Manifest{Language: wfdir.LanguageJS}}
	if got := workflowGateInput(js, map[string]string{"main.js": ""}, nil, nil).Language; got != wfdir.LanguageJS {
		t.Fatalf("a js folder's gate validate sent language %q", got)
	}
	py := &folder{Manifest: &wfdir.Manifest{}}
	if got := workflowGateInput(py, map[string]string{"main.py": ""}, nil, nil).Language; got != "" {
		t.Fatalf("a python folder's gate validate sent language %q", got)
	}
}

// TestWriteGateSendsAJSRowsLanguage: `wf publish` gates the stored draft, and
// the draft's own language is what the server must scan it as.
func TestWriteGateSendsAJSRowsLanguage(t *testing.T) {
	f, dir := gatedTree(t)
	root := filepath.Join(dir, "workflows/sales-report")
	draft := draftWritingOutside(t, f, root)
	f.wf.workflows[draft].Language = wfdir.LanguageJS
	_, _ = runCLI(t, root, "wf", "publish")
	var sent []string
	for _, in := range f.wf.validated {
		for _, file := range in.Files {
			if strings.Contains(file.Content, "table-budget") {
				sent = append(sent, in.Language)
			}
		}
	}
	if len(sent) == 0 {
		t.Fatal("the publish never validated the draft")
	}
	for _, lang := range sent {
		if lang != wfdir.LanguageJS {
			t.Fatalf("the gate validated a js draft as language %q", lang)
		}
	}
}
