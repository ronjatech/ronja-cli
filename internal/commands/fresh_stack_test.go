package commands

import (
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/ronjatech/ronja-cli/internal/api"
	"github.com/ronjatech/ronja-cli/internal/wfdir"
)

// A fresh stack stood up by ONE `sync apply --allow-create`, against the import
// fixture: every reference between imported
// resources resolves, in memory, to the copy the same run made, in dependency
// order — modules, pipelines in build order, every workflow created before any
// workflow's code is written, then apps and automations.

// addStack declares `name` in every folder of an imported tree, deploying to
// feature with that folder's binds — the hand-written step a `stack add`
// command would take over (see BL-6179). A folder named in onFeature deploys
// there instead.
func addStack(t *testing.T, dir, name, feature string, binds map[string]map[string]string, onFeature map[string]string) {
	t.Helper()
	err := filepath.WalkDir(dir, func(path string, d os.DirEntry, err error) error {
		if err != nil || d.IsDir() || d.Name() != wfdir.ManifestName {
			return err
		}
		root := filepath.Dir(path)
		rel, _ := filepath.Rel(dir, root)
		rel = filepath.ToSlash(rel)
		m := loadAnyManifest(t, root)
		main := m.Stacks["main"]
		target := feature
		if other, ok := onFeature[rel]; ok {
			target = other
		}
		m.Stacks[name] = wfdir.Stack{URL: main.URL, TenantID: main.TenantID, FeatureID: target, Bind: binds[rel]}
		return wfdir.SaveManifest(root, m)
	})
	if err != nil {
		t.Fatal(err)
	}
}

// freshStackFixture imports the fixture feature, makes the output literal the
// portable `{{ write() }}` the import report asks for, and declares a `dev`
// stack on an empty second feature of the same organization: read sources
// bound to the ORIGINAL rows, the write source to a row of the new feature,
// and the module — whose package name is unique in the organization — deployed
// to the one feature that holds it.
func freshStackFixture(t *testing.T) (*fakeImportInstance, string, importReport) {
	t.Helper()
	return freshStackFixtureFrom(t, importFixture(t), nil)
}

// freshStackFixtureFrom is freshStackFixture over a fixture the caller staged,
// with extra binds merged into the `dev` stack's.
func freshStackFixtureFrom(t *testing.T, f *fakeImportInstance, extra map[string]map[string]string) (*fakeImportInstance, string, importReport) {
	t.Helper()
	f.app.privilegeLevel = adminPrivilegeLevel
	f.auto.privilegeLevel = adminPrivilegeLevel
	f.pipe.privilegeLevel = adminPrivilegeLevel
	f.wf.mod.nameTaken["sales_helpers"] = true
	dir, report := runImport(t, f)
	if err := os.Mkdir(filepath.Join(dir, ".git"), 0o755); err != nil {
		t.Fatal(err)
	}
	f.pipe.AddFeature(devFeature, "Sales dev", "organization")
	f.app.featureScope[devFeature] = "organization"
	f.pipe.AddTable(&api.Table{ID: "table-dev-ledger", Name: "ledger", FeatureID: devFeature, Kind: "dynamic"})

	main := filepath.Join(dir, "workflows/sales-report/main.py")
	body := strings.Replace(readFile(t, main), `tools.replaceTable(fx, "table-ledger")`, `tools.replaceTable(fx, {{ write('ledger') }})`, 1)
	if err := os.WriteFile(main, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
	// A CI checkout: .ronja/ is git-ignored, so the run starts with no local
	// baseline — which is also what keeps the main stack's (keyed by instance
	// and organization, not by stack) from being read for dev's rows.
	dropLocalState(t, dir)
	binds := map[string]map[string]string{
		"pipelines/sales":        {"raw_orders": "table-raw-orders"},
		"workflows/sales-report": {"fx_rates": "table-fx-rates", "crm": "secret-crm", "ledger": "table-dev-ledger"},
		"automations/sales":      {"mcpserver_crm": "mcpsrv-crm"},
		// The app's run edge out of the feature, bound on purpose for dev to
		// the original workflow ("run": true).
		"apps/sales-dashboard": {"close_books": "workflow-close"},
	}
	for folder, more := range extra {
		if binds[folder] == nil {
			binds[folder] = map[string]string{}
		}
		for alias, id := range more {
			binds[folder][alias] = id
		}
	}
	addStack(t, dir, "dev", devFeature, binds, map[string]string{"modules/sales_helpers": importFeature})
	return f, dir, report
}

// TestImportedTreeStandsUpAFreshStackInOneApply is the import proof's step 2:
// the imported tree applied with --allow-create into a second, empty feature of
// the same organization. Every reference between imported resources points at
// the new copies, every data source at the original rows, a second apply writes
// nothing, and the import report named the one TSX literal.
func TestImportedTreeStandsUpAFreshStackInOneApply(t *testing.T) {
	f, dir, imported := freshStackFixture(t)

	// The import report named the TSX literal the app still carries.
	tsx := false
	for _, l := range imported.Literals {
		if l.Folder == "apps/sales-dashboard" && l.Path == "App.tsx" && l.Line == 3 && l.TargetID == "table-orders-clean" {
			tsx = true
		}
	}
	if !tsx {
		t.Fatalf("the import report does not name the TSX literal: %+v", imported.Literals)
	}

	report, err := applyStack(t, dir, "dev", "--allow-create")
	if err != nil || report.Verdict != verdictApplied {
		t.Fatalf("the fresh stack did not go up in one apply: %v\n%s", err, describeApply(report))
	}
	// Dependency order: the module and the pipeline before the workflow that
	// reads them, the app and the automations after it.
	if got := strings.Join(report.Sequence, ","); got != "modules/sales_helpers,pipelines/sales,workflows/sales-report,apps/sales-dashboard,automations/sales" {
		t.Fatalf("sequence = %s", got)
	}

	rows := devRows(f, devFeature)
	clean, metric := rows.tables["orders_clean"], rows.tables["aov"]
	if clean == nil || metric == nil || len(rows.workflows) != 1 || len(rows.apps) != 1 || len(rows.automations) != 2 {
		t.Fatalf("the dev feature holds tables %v, %d workflows, %d apps, %d automations", keysOf(rows.tables), len(rows.workflows), len(rows.apps), len(rows.automations))
	}
	wf, app := rows.workflows[0], rows.apps[0]

	// References between imported resources → the new copies.
	if !strings.Contains(clean.Code, "{{ ref('table-raw-orders') }}") {
		t.Fatalf("the dev table does not read the original connector table:\n%s", clean.Code)
	}
	if !strings.Contains(string(metric.MetricRecipe), `"`+clean.ID+`"`) {
		t.Fatalf("the dev metric's source is not the dev table %s: %s", clean.ID, metric.MetricRecipe)
	}
	code := f.wf.FileContents(wf.ID)["main.py"]
	for _, want := range []string{
		"{{ ref('" + clean.ID + "') }}",                           // the dev copy of orders_clean, by address
		"{{ module('module-helpers') }}",                          // the organization's one module
		"{{ ref('table-fx-rates') }}",                             // read source → the original
		"{{ secret('secret-crm', 'token') }}",                     // side effect → the original
		"{{ write('orders_daily') }}",                             // own output, by name, in dev
		"tools.replaceTable(fx, {{ write('table-dev-ledger') }})", // write source → the dev row
	} {
		if !strings.Contains(code, want) {
			t.Fatalf("the dev workflow does not hold %s:\n%s", want, code)
		}
	}
	if len(app.AllowedTableIDs) != 1 || app.AllowedTableIDs[0] != clean.ID {
		t.Fatalf("the dev app's access = %v, want the dev table %s", app.AllowedTableIDs, clean.ID)
	}
	nightly, assistant := rows.automations["nightly"], rows.automations["Sales assistant"]
	if nightly == nil || nightly.Action == nil || nightly.Action.Config.WorkflowID != wf.ID {
		t.Fatalf("the dev nightly automation runs %+v, want the dev workflow %s", nightly, wf.ID)
	}
	if assistant == nil || assistant.Action == nil {
		t.Fatalf("no dev assistant automation: %+v", rows.automations)
	}
	refs := map[string]string{}
	for _, ref := range assistant.Action.Config.References {
		refs[ref.Kind] = ref.ResourceID
	}
	if refs["dataapp"] != app.ID || refs["feature"] != devFeature || refs["mcpserver"] != "mcpsrv-crm" {
		t.Fatalf("the dev assistant references %v, want the dev app %s, the dev feature and the original MCP server", refs, app.ID)
	}

	// Every create carried its claim: the dev feature's claims name each row.
	claimed := map[string]bool{}
	for _, c := range f.managed.Claims(devFeature) {
		claimed[c.ResourceID] = true
	}
	for _, id := range []string{clean.ID, metric.ID, wf.ID, app.ID, nightly.ID, assistant.ID} {
		if !claimed[id] {
			t.Fatalf("%s was created without a claim in the dev feature", id)
		}
	}
	// Nothing in the original feature was written.
	if live := f.wf.FileContents("workflow-report")["main.py"]; live != importWorkflowP {
		t.Fatalf("the original workflow changed:\n%s", live)
	}

	// Applying an unchanged tree twice: the second apply sends no write.
	f.resetRequests()
	again, err := applyStack(t, dir, "dev", "--allow-create")
	if err != nil || again.Verdict != verdictApplied {
		t.Fatalf("the second apply: %v\n%s", err, describeApply(again))
	}
	if writes := f.writes(); len(writes) != 0 {
		t.Fatalf("the second apply sent %v\n%s", writes, describeApply(again))
	}
}

// TestSlipScenarioIsRefusedBeforeAnyWrite is the slip scenario the write gate
// exists for, as a test. Import Sales from Main, whose workflow also writes a
// Sales (Main) table by OUTPUT LITERAL; hand-add a `dev` stack on a second
// feature; bind the read sources to the original rows and the write sources to
// rows of the new feature — the natural fix for "binds nothing" — and run
// `sync apply --stack dev --allow-create`. The literal still names Main's
// table, so the workflow is refused writes_outside_target before anything is
// written for it: no row created, no code pushed. Disable the gate and this
// test watches the Dev deployment publish code that writes Main's table on its
// first run.
func TestSlipScenarioIsRefusedBeforeAnyWrite(t *testing.T) {
	f := importFixture(t)
	f.app.privilegeLevel, f.auto.privilegeLevel, f.pipe.privilegeLevel = adminPrivilegeLevel, adminPrivilegeLevel, adminPrivilegeLevel
	f.wf.mod.nameTaken["sales_helpers"] = true
	// The Main table the workflow writes by literal, and its inventory line.
	f.pipe.AddTable(&api.Table{ID: "table-targets", Name: "targets", FeatureID: importFeature, Kind: "dynamic"})
	f.wf.files["workflow-report"][0].Content = importWorkflowP + "tools.appendToTable(orders, \"table-targets\")\n"
	f.inv.mu.Lock()
	for i := range f.inv.resources[importFeature] {
		res := &f.inv.resources[importFeature][i]
		if res.ID == "workflow-report" {
			res.References = append(res.References, api.ReferencePosition{Position: api.RefPosOutputLiteral, Direction: api.RefDirWrite,
				TargetKind: "table", TargetID: "table-targets", Readable: true, TargetName: "targets", TargetFeatureID: importFeature,
				TargetTableKind: "dynamic", Arg: "table-targets", Path: "main.py", Line: 7, Call: "appendToTable"})
		}
	}
	f.inv.mu.Unlock()
	dir, imported := runImport(t, f)
	named := false
	for _, l := range imported.Literals {
		if l.Path == "main.py" && l.Line == 7 && l.TargetID == "table-targets" {
			named = true
		}
	}
	if !named {
		t.Fatalf("the import report did not name the output literal: %+v", imported.Literals)
	}
	dropLocalState(t, dir)
	f.pipe.AddFeature(devFeature, "Sales dev", "organization")
	f.app.featureScope[devFeature] = "organization"
	f.pipe.AddTable(&api.Table{ID: "table-dev-ledger", Name: "ledger", FeatureID: devFeature, Kind: "dynamic"})
	f.pipe.AddTable(&api.Table{ID: "table-dev-targets", Name: "targets", FeatureID: devFeature, Kind: "dynamic"})
	writeAliases := loadAnyManifest(t, filepath.Join(dir, "workflows/sales-report")).Dependencies
	if !writeAliases["targets"].Write || !writeAliases["ledger"].Write {
		t.Fatalf("the import did not declare the write sources: %+v", writeAliases)
	}
	addStack(t, dir, "dev", devFeature, map[string]map[string]string{
		"pipelines/sales": {"raw_orders": "table-raw-orders"},
		"workflows/sales-report": {"fx_rates": "table-fx-rates", "crm": "secret-crm",
			"ledger": "table-dev-ledger", "targets": "table-dev-targets"},
		"automations/sales":    {"mcpserver_crm": "mcpsrv-crm"},
		"apps/sales-dashboard": {"close_books": "workflow-close"},
	}, map[string]string{"modules/sales_helpers": importFeature})

	f.resetRequests()
	report, err := applyStack(t, dir, "dev", "--allow-create")
	// Nothing was written for the workflow: no row, no code — in either feature.
	if wfs := devRows(f, devFeature).workflows; len(wfs) != 0 {
		for _, wf := range wfs {
			t.Errorf("the Dev deployment created %s, whose code reads:\n%s", wf.ID, f.wf.FileContents(wf.ID)["main.py"])
		}
		t.Fatal("the Dev deployment would write Main's table on its first run")
	}
	if err == nil {
		t.Fatalf("the slip scenario applied:\n%s", describeApply(report))
	}
	var wfLine *syncApplyFolderReport
	for i := range report.Folders {
		if report.Folders[i].Path == "workflows/sales-report" {
			wfLine = &report.Folders[i]
		}
	}
	if wfLine == nil || wfLine.Verdict != verdictRefused || wfLine.Reason != syncReasonWritesOutsideTarget {
		t.Fatalf("the workflow was not refused writes_outside_target:\n%s", describeApply(report))
	}
	for _, want := range []string{"main.py:7 (tools.appendToTable) writes the table table-targets (targets)", "in feature " + importFeature,
		"main.py:6 (tools.replaceTable) writes the table table-ledger (ledger)", "in feature " + financeFeature} {
		if !strings.Contains(wfLine.Detail, want) {
			t.Fatalf("the refusal does not say %q:\n%s", want, wfLine.Detail)
		}
	}
	for _, w := range f.writes() {
		if strings.HasPrefix(w, "POST /api/v2/workflow") && w != "POST /api/v2/workflow/validate" || strings.Contains(w, "workflow-report") {
			t.Fatalf("the refused workflow was written to: %s", w)
		}
	}
	// What runs the refused workflow by address is refused with it; the rest
	// of the stack, which writes nothing outside it, went up.
	for _, line := range report.Folders {
		switch line.Path {
		case "automations/sales":
			if line.Verdict != verdictRefused || line.Reason != syncReasonAddressUnresolved {
				t.Fatalf("the automation running the refused workflow: %s / %s", line.Verdict, line.Reason)
			}
		case "pipelines/sales", "apps/sales-dashboard", "modules/sales_helpers":
			if line.Verdict != verdictApplied {
				t.Fatalf("%s: %s / %s — %s", line.Path, line.Verdict, line.Reason, line.Detail)
			}
		}
	}
}

// TestApplyWarnsWhenAModuleIsPublishedIntoAnotherFeature is the shared-module
// warning: the dev stack deploys its module folder to the feature that holds
// the organization's one copy, while every other folder goes to dev — so
// applying dev publishes the module production imports. Said in the report
// (--json and human), never refused; an apply of `main`, where every folder
// shares the module's feature, says nothing.
func TestApplyWarnsWhenAModuleIsPublishedIntoAnotherFeature(t *testing.T) {
	_, dir, _ := freshStackFixture(t)
	report, err := applyStack(t, dir, "dev", "--allow-create", "--dry-run")
	if err != nil {
		t.Fatalf("dry-run: %v\n%s", err, describeApply(report))
	}
	if len(report.Warnings) != 1 {
		t.Fatalf("warnings = %+v, want the one shared_module", report.Warnings)
	}
	w := report.Warnings[0]
	for _, want := range []string{"single module", `"sales_helpers"`, "into feature " + importFeature, devFeature, "production"} {
		if w.Reason != syncWarningSharedModule || w.Path != "modules/sales_helpers" || !strings.Contains(w.Detail, want) {
			t.Fatalf("warning %+v does not say %q", w, want)
		}
	}
	human, err := runCLI(t, dir, "sync", "apply", "--stack", "dev", "--allow-create", "--dry-run")
	if err != nil || !strings.Contains(human, "WARNING (shared_module): modules/sales_helpers") {
		t.Fatalf("the human report does not carry the warning: %v\n%s", err, human)
	}
	// ACTION FIRST: one sentence saying what happens and what to do, then the
	// why on lines of its own, indented under it.
	first, why, _ := strings.Cut(w.Detail, "\n")
	if !strings.Contains(first, "single module") || !strings.Contains(first, "production") || !strings.Contains(first, "apply it only if you mean that change for every feature") || strings.Contains(first, "last published with") {
		t.Fatalf("the warning does not lead with what happens and what to do: %q", first)
	}
	if !strings.Contains(human, "\n      "+strings.ReplaceAll(why, "\n", "\n      ")+"\n") {
		t.Fatalf("the explanation is not indented under the warning's first line:\n%s", human)
	}
	// Once dev is deployed, the module is unchanged: the run publishes nothing
	// to it, and the warning says so rather than "this publishes" — and says
	// when a change would reach a workflow (it pins a version).
	if deployed, err := applyStack(t, dir, "dev", "--allow-create"); err != nil {
		t.Fatalf("apply dev: %v\n%s", err, describeApply(deployed))
	}
	for _, args := range [][]string{{"--dry-run"}, nil} {
		again, err := applyStack(t, dir, "dev", args...)
		if err != nil || len(again.Warnings) != 1 {
			t.Fatalf("apply dev %v again: %+v (%v)", args, again.Warnings, err)
		}
		w := again.Warnings[0].Detail
		for _, want := range []string{"changes in this run", "into feature " + importFeature, "production", "last published with", "next saved"} {
			if !strings.Contains(w, want) {
				t.Fatalf("apply %v: the warning about an unchanged module does not say %q: %s", args, want, w)
			}
		}
		if strings.Contains(w, "This publishes") {
			t.Fatalf("apply %v: the warning about an unchanged module says it publishes: %s", args, w)
		}
	}
	// A change to it is a planned publish, and said as one.
	writeLocal(t, filepath.Join(dir, "modules", "sales_helpers"), "extra.py", "X = 1\n")
	changed, err := applyStack(t, dir, "dev", "--dry-run")
	if err != nil || len(changed.Warnings) != 1 || !strings.Contains(changed.Warnings[0].Detail, "This publishes") ||
		!strings.Contains(changed.Warnings[0].Detail, "last published with") {
		t.Fatalf("the warning about a changed module: %+v (%v)", changed.Warnings, err)
	}

	// From a checkout of its own, as every stack of one organization is
	// deployed (BL-4e71): this one's local baselines are dev's now, and the
	// dry run, like the apply, refuses main's live workflow against them.
	for _, folder := range []string{"workflows/sales-report", "apps/sales-dashboard", "modules/sales_helpers", "pipelines/sales", "automations/sales"} {
		freshCheckout(t, filepath.Join(dir, folder))
	}
	main, err := applyStack(t, dir, "main", "--dry-run")
	if err != nil || len(main.Warnings) != 0 {
		t.Fatalf("apply --stack main warned %+v (%v)", main.Warnings, err)
	}
}

// TestFreshStackCreatesAnAppThatRunsItsOwnWorkflowByAddress: an app whose
// access allows a workflow of its own feature is imported with an ADDRESS
// dependency on that workflow's folder. In a fresh stack the address is still
// pending when the plan is gated (the workflow is created by the same run), and
// the plan-time gate must not read the unresolved alias as a foreign workflow
// it cannot read: one apply stands the stack up, and the dev app allows the
// dev workflow.
func TestFreshStackCreatesAnAppThatRunsItsOwnWorkflowByAddress(t *testing.T) {
	f := importFixture(t)
	f.app.apps["data_app-dash"].AllowedWorkflowIDs = []string{"workflow-close", "workflow-report"}
	for i, r := range f.inv.resources[importFeature] {
		if r.ID == "data_app-dash" {
			f.inv.resources[importFeature][i].References = append(r.References, api.ReferencePosition{
				TargetKind: "workflow", TargetID: "workflow-report", Readable: true, TargetName: "Sales report",
				TargetFeatureID: importFeature, Position: api.RefPosAppAccess, Direction: api.RefDirRun, Field: "allowedWorkflowIDs[1]"})
		}
	}
	f, dir, _ := freshStackFixtureFrom(t, f, nil)
	if body := readFile(t, filepath.Join(dir, "apps/sales-dashboard", wfdir.ManifestName)); !strings.Contains(body, `"from": "../../workflows/sales-report"`) {
		t.Fatalf("the import did not declare the app's own workflow by address:\n%s", body)
	}
	report, err := applyStack(t, dir, "dev", "--allow-create")
	if err != nil || report.Verdict != verdictApplied {
		t.Fatalf("the fresh stack did not go up in one apply: %v\n%s", err, describeApply(report))
	}
	rows := devRows(f, devFeature)
	if len(rows.workflows) != 1 || len(rows.apps) != 1 {
		t.Fatalf("the dev feature holds %d workflows and %d apps", len(rows.workflows), len(rows.apps))
	}
	wf, app := rows.workflows[0], rows.apps[0]
	if !slices.Contains(app.AllowedWorkflowIDs, wf.ID) || slices.Contains(app.AllowedWorkflowIDs, "workflow-report") {
		t.Fatalf("the dev app allows %v, want the dev workflow %s and not the original", app.AllowedWorkflowIDs, wf.ID)
	}
}

// TestSharedModuleWarningOnAModuleTheRunDidNotPublish: the warning is built
// after the run, and for a module folder the run REFUSED (here, its publish
// was refused) it must not say "this publishes".
func TestSharedModuleWarningOnAModuleTheRunDidNotPublish(t *testing.T) {
	f, dir, _ := freshStackFixture(t)
	if deployed, err := applyStack(t, dir, "dev", "--allow-create"); err != nil {
		t.Fatalf("apply dev: %v\n%s", err, describeApply(deployed))
	}
	writeLocal(t, filepath.Join(dir, "modules", "sales_helpers"), "extra.py", "X = 1\n")
	f.wf.mod.failCommit = 403
	report, _ := applyStack(t, dir, "dev")
	for _, l := range report.Folders {
		if l.Path == "modules/sales_helpers" && l.Verdict == verdictApplied {
			t.Fatalf("the module was published though its commit was refused: %+v", l)
		}
	}
	if len(report.Warnings) != 1 {
		t.Fatalf("warnings = %+v", report.Warnings)
	}
	w := report.Warnings[0].Detail
	if strings.Contains(w, "This publishes") || !strings.Contains(w, "did not publish") || !strings.Contains(w, "production") {
		t.Fatalf("the warning about a module this run did not publish: %s", w)
	}
}

// Two stacks of one organization share a checkout's local baselines
// (BL-4e71): once dev is applied, main's workflow is compared with dev's
// record, and the apply refuses it. The dry run says so too, in the apply's
// words — it does not promise a push the apply then refuses.
func TestDryRunRefusesASecondStackOfTheSameOrganizationInASharedCheckout(t *testing.T) {
	_, dir, _ := freshStackFixture(t)
	if deployed, err := applyStack(t, dir, "dev", "--allow-create"); err != nil {
		t.Fatalf("apply dev: %v\n%s", err, describeApply(deployed))
	}
	lineOf := func(r syncApplyReport) syncApplyFolderReport {
		for _, line := range r.Folders {
			if line.Path == "workflows/sales-report" {
				return line
			}
		}
		t.Fatalf("no workflow line:\n%s", describeApply(r))
		return syncApplyFolderReport{}
	}
	preview, _ := applyStack(t, dir, "main", "--dry-run")
	applied, _ := applyStack(t, dir, "main")
	dry, real := lineOf(preview), lineOf(applied)
	if real.Verdict != verdictRefused || !strings.Contains(real.Detail, `since stack "main" last applied it`) {
		t.Fatalf("apply main: %s / %s — %s", real.Verdict, real.Reason, real.Detail)
	}
	if dry.Verdict != real.Verdict || dry.Reason != real.Reason || dry.Detail != real.Detail {
		t.Fatalf("the dry run says %s / %s %q\nthe apply says %s / %s %q", dry.Verdict, dry.Reason, dry.Detail, real.Verdict, real.Reason, real.Detail)
	}
}
