package commands

import (
	"encoding/json"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"testing"

	"github.com/ronjatech/ronja-cli/internal/api"
	"github.com/ronjatech/ronja-cli/internal/markers"
	"github.com/ronjatech/ronja-cli/internal/wfdir"
)

// `ronja import feature`, end to end against one fake instance (the import
// fixture): a feature holding
//
//   - a connector table and a derived table reading it, plus a metric over the
//     derived table;
//   - a workflow reading the derived table, a secret and a table in ANOTHER
//     feature, writing its own output by marker and a foreign table by output
//     literal, and importing a module;
//   - a module;
//   - an app whose access lists the derived table and whose TSX carries one id
//     literal;
//   - an automation running the workflow, and an agent automation referencing
//     the app, its own feature and an MCP server — a workflow action carries
//     no references, so "an automation running the workflow and referencing the
//     app" is two automations.

// The workflow writes in the two shapes the server scans
// (backend/engine/pymarkers): a `{{ write() }}` marker, and an OUTPUT LITERAL — the
// table argument of tools.replaceTable is its SECOND positional (extract.go
// pickTableIDArg :237-245), so `tools.replaceTable(fx, "table-ledger")`.
const (
	importFeature   = "collection-sales"
	financeFeature  = "collection-finance"
	importWorkflowP = "import {{ module('module-helpers') }} as helpers\n" +
		"orders = tools.query(\"SELECT * FROM {{ ref('table-orders-clean') }}\")\n" +
		"fx = tools.query(\"SELECT * FROM {{ ref('table-fx-rates') }}\")\n" +
		"token = \"{{ secret('secret-crm', 'token') }}\"\n" +
		"tools.replaceTable(orders, {{ write('table-daily') }})\n" +
		"tools.replaceTable(fx, \"table-ledger\")\n"
	importAppTSX = "import { useQueue } from './kit'\n" +
		"export default function App() {\n" +
		"  const q = useQueue({ tableID: \"table-orders-clean\" })\n" +
		"  return null\n}\n"
)

// importFixture stages the feature on a fake instance and signs in to it.
func importFixture(t *testing.T) *fakeImportInstance {
	t.Helper()
	f := newFakeImportInstance(t)
	signInTo(t, f.URL())
	// An Admin, as the managed fake's caller is: a table or metric claim needs
	// the role, and the import reads it before planning.
	f.wf.privilegeLevel = adminPrivilegeLevel

	f.pipe.AddFeature(importFeature, "Sales", "organization")
	f.pipe.AddFeature(financeFeature, "Finance", "organization")
	f.inv.features[importFeature] = "Sales"
	f.pipe.AddTable(&api.Table{ID: "table-raw-orders", Name: "raw_orders", FeatureID: importFeature, Kind: "integration"})
	f.pipe.AddTable(&api.Table{ID: "table-orders-clean", Name: "orders_clean", FeatureID: importFeature,
		Code: "SELECT * FROM {{ ref('table-raw-orders') }}", InputModels: []string{"table-raw-orders"}})
	f.pipe.AddMetric("table-aov", "aov", string(canonicalRecipe(t, json.RawMessage(
		strings.Replace(aovRecipe, `"source":"orders"`, `"source":"table-orders-clean"`, 1))))).FeatureID = importFeature
	f.pipe.AddTable(&api.Table{ID: "table-daily", Name: "orders_daily", FeatureID: importFeature, Kind: "dynamic"})
	f.pipe.AddTable(&api.Table{ID: "table-fx-rates", Name: "fx_rates", FeatureID: financeFeature, Kind: "integration"})
	f.pipe.AddTable(&api.Table{ID: "table-ledger", Name: "ledger", FeatureID: financeFeature, Kind: "dynamic"})

	f.wf.AddWorkflow(&api.Workflow{ID: "workflow-report", Lifecycle: api.LifecycleLive, FeatureID: importFeature,
		FeatureScope: "organization", Title: "Sales report", RuntimeVersion: wfdir.RuntimeQuery},
		api.WorkflowFile{Path: "main.py", Content: importWorkflowP})
	f.wf.versions["workflow-report"] = []api.Workflow{{ID: "wfv-1"}}
	f.wf.AddModule(&api.Module{ID: "module-helpers", Lifecycle: api.LifecycleLive, Name: "sales_helpers", Title: "Sales helpers", FeatureID: importFeature},
		api.ModuleFile{Path: moduleInitFile, Content: fakeModuleInitSeed},
		api.ModuleFile{Path: "money.py", Content: "def round2(x):\n    return round(x, 2)\n"})
	f.wf.mod.versions["module-helpers"] = []api.Module{{ID: "modv-1"}}

	// A workflow of ANOTHER feature the app may run: a run edge out of Sales,
	// which import declares "run": true and binds in main.
	f.wf.AddWorkflow(&api.Workflow{ID: "workflow-close", Lifecycle: api.LifecycleLive, FeatureID: financeFeature,
		FeatureScope: "organization", Title: "Close books", RuntimeVersion: wfdir.RuntimeQuery},
		api.WorkflowFile{Path: "main.py", Content: "print('close')\n"})
	f.app.AddApp(&api.DataApp{ID: "data_app-dash", Lifecycle: api.LifecycleLive, Name: "Sales dashboard", FeatureID: importFeature,
		DataAppAccess: api.DataAppAccess{AllowedTableIDs: []string{"table-orders-clean"}, AllowedWorkflowIDs: []string{"workflow-close"}}},
		api.DataAppFile{Path: "App.tsx", Content: importAppTSX})
	f.app.versions["data_app-dash"] = []api.DataApp{{ID: "dav-1"}}
	f.app.featureScope[importFeature] = "organization"

	f.mcpServers["mcpsrv-crm"] = true
	f.auto.AddAutomation(&api.Automation{ID: "sjob-nightly", FeatureID: importFeature, Name: "nightly",
		TriggerKind: api.TriggerKindCron, CronExpr: "0 2 * * *", Timezone: "UTC", Enabled: true,
		Action: &api.AutomationAction{ID: "act-n", ScheduledJobID: "sjob-nightly", Kind: api.ActionKindWorkflow,
			Config: api.AutomationActionConfig{WorkflowID: "workflow-report"}}})
	f.auto.AddAutomation(&api.Automation{ID: "sjob-assistant", FeatureID: importFeature, Name: "Sales assistant",
		TriggerKind: api.TriggerKindCron, CronExpr: "0 8 * * 1", Timezone: "UTC", Enabled: true, Prompt: "Summarise the week",
		Action: &api.AutomationAction{ID: "act-a", ScheduledJobID: "sjob-assistant", Kind: api.ActionKindAgent,
			Config: api.AutomationActionConfig{References: []api.AutomationReference{
				{Kind: "dataapp", ResourceID: "data_app-dash"},
				{Kind: "feature", ResourceID: importFeature},
				{Kind: "mcpserver", ResourceID: "mcpsrv-crm"},
			}}}})

	// The inventory, as erefs derives it (derive.go): positions, directions and
	// what each readable target carries.
	sales := func(kind, id, name, tableKind string) api.ReferencePosition {
		return api.ReferencePosition{TargetKind: kind, TargetID: id, Readable: true, TargetName: name,
			TargetFeatureID: importFeature, TargetTableKind: tableKind}
	}
	with := func(p api.ReferencePosition, pos, dir string, mod func(*api.ReferencePosition)) api.ReferencePosition {
		p.Position, p.Direction = pos, dir
		if mod != nil {
			mod(&p)
		}
		return p
	}
	f.inv.add(importFeature, api.ReferenceResource{Kind: "table", ID: "table-raw-orders", Name: "raw_orders", TableKind: "integration"})
	f.inv.add(importFeature, api.ReferenceResource{Kind: "table", ID: "table-orders-clean", Name: "orders_clean", TableKind: "derived",
		References: []api.ReferencePosition{with(sales("table", "table-raw-orders", "raw_orders", "integration"), api.RefPosRefMarker, api.RefDirRead,
			func(p *api.ReferencePosition) { p.Arg, p.Field, p.Line = "table-raw-orders", "code", 1 })}})
	f.inv.add(importFeature, api.ReferenceResource{Kind: "table", ID: "table-daily", Name: "orders_daily", TableKind: "dynamic",
		OwnerWorkflowID: "workflow-report", LogicalName: "orders_daily"})
	f.inv.add(importFeature, api.ReferenceResource{Kind: "metric", ID: "table-aov", Name: "aov",
		References: []api.ReferencePosition{with(sales("table", "table-orders-clean", "orders_clean", "derived"), api.RefPosMetricSource, api.RefDirRead,
			func(p *api.ReferencePosition) { p.Field = "metricRecipe.source" })}})
	f.inv.add(importFeature, api.ReferenceResource{Kind: "workflow", ID: "workflow-report", Name: "Sales report",
		References: []api.ReferencePosition{
			with(sales("module", "module-helpers", "sales_helpers", ""), api.RefPosModuleMarker, api.RefDirRead,
				func(p *api.ReferencePosition) { p.Arg, p.Path, p.Line = "module-helpers", "main.py", 1 }),
			with(sales("table", "table-orders-clean", "orders_clean", "derived"), api.RefPosRefMarker, api.RefDirRead,
				func(p *api.ReferencePosition) { p.Arg, p.Path, p.Line = "table-orders-clean", "main.py", 2 }),
			with(api.ReferencePosition{TargetKind: "table", TargetID: "table-fx-rates", Readable: true, TargetName: "fx_rates",
				TargetFeatureID: financeFeature, TargetTableKind: "integration"}, api.RefPosRefMarker, api.RefDirRead,
				func(p *api.ReferencePosition) { p.Arg, p.Path, p.Line = "table-fx-rates", "main.py", 3 }),
			with(api.ReferencePosition{TargetKind: "secret", TargetID: "secret-crm", Readable: true, TargetName: "crm"},
				api.RefPosSecretMarker, api.RefDirSideEffect,
				func(p *api.ReferencePosition) {
					p.Arg, p.Path, p.Line, p.SecretField = "secret-crm", "main.py", 4, "token"
				}),
			with(sales("table", "table-daily", "orders_daily", "dynamic"), api.RefPosWriteMarker, api.RefDirWrite,
				func(p *api.ReferencePosition) {
					p.Arg, p.Path, p.Line, p.OwnOutput, p.LogicalName = "table-daily", "main.py", 5, true, "orders_daily"
				}),
			with(api.ReferencePosition{TargetKind: "table", TargetID: "table-ledger", Readable: true, TargetName: "ledger",
				TargetFeatureID: financeFeature, TargetTableKind: "dynamic"}, api.RefPosOutputLiteral, api.RefDirWrite,
				func(p *api.ReferencePosition) {
					p.Arg, p.Path, p.Line, p.Call = "table-ledger", "main.py", 6, "replaceTable"
				}),
		}})
	f.inv.add(importFeature, api.ReferenceResource{Kind: "dataapp", ID: "data_app-dash", Name: "Sales dashboard",
		References: []api.ReferencePosition{
			with(sales("table", "table-orders-clean", "orders_clean", "derived"), api.RefPosAppAccess, api.RefDirRead,
				func(p *api.ReferencePosition) { p.Field = "allowedTableIDs[0]" }),
			with(api.ReferencePosition{TargetKind: "workflow", TargetID: "workflow-close", Readable: true, TargetName: "Close books",
				TargetFeatureID: financeFeature}, api.RefPosAppAccess, api.RefDirRun,
				func(p *api.ReferencePosition) { p.Field = "allowedWorkflowIDs[0]" }),
			with(sales("table", "table-orders-clean", "orders_clean", "derived"), api.RefPosAppLiteral, api.RefDirRead,
				func(p *api.ReferencePosition) {
					p.Arg, p.Path, p.Line, p.Call = "table-orders-clean", "App.tsx", 3, "useQueue"
				}),
		}})
	f.inv.add(importFeature, api.ReferenceResource{Kind: "module", ID: "module-helpers", Name: "sales_helpers"})
	f.inv.add(importFeature, api.ReferenceResource{Kind: "automation", ID: "sjob-nightly", Name: "nightly",
		References: []api.ReferencePosition{with(sales("workflow", "workflow-report", "Sales report", ""), api.RefPosAutomationAction, api.RefDirRun,
			func(p *api.ReferencePosition) { p.Field = "action.workflowID" })}})
	f.inv.add(importFeature, api.ReferenceResource{Kind: "automation", ID: "sjob-assistant", Name: "Sales assistant",
		References: []api.ReferencePosition{
			with(sales("dataapp", "data_app-dash", "Sales dashboard", ""), api.RefPosAutomationRef, api.RefDirRead,
				func(p *api.ReferencePosition) { p.Field = "references[0]" }),
			// A reference-set feature is read_write: the run expands it to every
			// table of the feature and may write each one (erefs
			// referenceSetDirection).
			with(api.ReferencePosition{TargetKind: "feature", TargetID: importFeature, Readable: true, TargetName: "Sales",
				TargetFeatureID: importFeature}, api.RefPosAutomationRef, api.RefDirReadWrite,
				func(p *api.ReferencePosition) { p.Field = "references[1]" }),
			with(api.ReferencePosition{TargetKind: "mcpserver", TargetID: "mcpsrv-crm"}, api.RefPosAutomationRef, api.RefDirSideEffect,
				func(p *api.ReferencePosition) { p.Field = "references[2]" }),
		}})
	return f
}

// runImport imports the fixture feature into a fresh directory and decodes the
// report.
func runImport(t *testing.T, f *fakeImportInstance, args ...string) (string, importReport) {
	t.Helper()
	dir := filepath.Join(t.TempDir(), "repo")
	out, err := runCLI(t, t.TempDir(), append([]string{"import", "feature", importFeature, dir, "--json"}, args...)...)
	if err != nil {
		t.Fatalf("import: %v\n%s", err, out)
	}
	var report importReport
	if err := json.Unmarshal([]byte(out), &report); err != nil {
		t.Fatalf("decode the report: %v\n%s", err, out)
	}
	return dir, report
}

// TestImportLeavesNoUnreportedLiteral: no literal id is left
// where a stem, an address or an alias can be written, and every literal that
// IS left is named in the report — checked by scanning every source file the
// import wrote for anything id-shaped.
func TestImportLeavesNoUnreportedLiteral(t *testing.T) {
	f := importFixture(t)
	dir, report := runImport(t, f)
	assertOnlyReportedLiterals(t, dir, report)
}

// TestImportFeatureProof is the import proof: after `ronja import feature`,
// `sync status` is clean, `sync apply --stack main` sends no write, the report
// names exactly the two literals left in place, every reference between
// imported resources is a stem, an address or an own-output name, and every
// external reference is a data source bound in `main`.
func TestImportFeatureProof(t *testing.T) {
	f := importFixture(t)
	dir, report := runImport(t, f)

	// The tree.
	for _, rel := range []string{"pipelines/sales/orders_clean.sql", "pipelines/sales/metrics/aov.json",
		"workflows/sales-report/main.py", "apps/sales-dashboard/App.tsx", "modules/sales_helpers/money.py",
		"automations/sales/nightly.json", "automations/sales/sales-assistant.json"} {
		if _, err := os.Stat(filepath.Join(dir, filepath.FromSlash(rel))); err != nil {
			t.Fatalf("the import wrote no %s: %v", rel, err)
		}
	}
	// Every folder is adopted (format 6+) and names stack main, pinned to the
	// feature.
	for _, folder := range report.Folders {
		m := loadAnyManifest(t, filepath.Join(dir, folder.Path))
		if !m.Adopted() || m.Stacks["main"].FeatureID != importFeature {
			t.Fatalf("%s: adopted=%v stack main=%+v", folder.Path, m.Adopted(), m.Stacks["main"])
		}
	}
	// Every row claimed.
	if len(report.Claims) != 7 {
		t.Fatalf("claims = %d, want 7 (a table and a metric, the workflow, app and module, two automations): %+v", len(report.Claims), report.Claims)
	}
	for _, c := range report.Claims {
		if c.Outcome != api.ManagedOutcomeClaimed {
			t.Fatalf("claim %+v", c)
		}
	}

	// The report names EXACTLY the two literals left in place.
	if len(report.Literals) != 2 {
		t.Fatalf("literals = %+v, want the output literal and the TSX literal", report.Literals)
	}
	got := map[string]bool{}
	for _, l := range report.Literals {
		got[l.Folder+"/"+l.Path+":"+itoa(l.Line)+" "+l.TargetID] = true
	}
	for _, want := range []string{"workflows/sales-report/main.py:6 table-ledger", "apps/sales-dashboard/App.tsx:3 table-orders-clean"} {
		if !got[want] {
			t.Fatalf("the report does not name %s: %+v", want, report.Literals)
		}
	}

	// References between imported resources: stem, address or own output.
	sql := readFile(t, filepath.Join(dir, "pipelines/sales/orders_clean.sql"))
	if !strings.Contains(sql, "{{ ref('raw_orders') }}") {
		t.Fatalf("orders_clean.sql reads the connector table as %q, want the data source alias", sql)
	}
	if aov := readFile(t, filepath.Join(dir, "pipelines/sales/metrics/aov.json")); !strings.Contains(aov, `"source": "orders_clean"`) && !strings.Contains(aov, `"source":"orders_clean"`) {
		t.Fatalf("the metric's source is not its sibling's stem:\n%s", aov)
	}
	main := readFile(t, filepath.Join(dir, "workflows/sales-report/main.py"))
	for _, want := range []string{"{{ module('sales_helpers') }}", "{{ ref('orders_clean') }}", "{{ ref('fx_rates') }}",
		"{{ secret('crm', 'token') }}", "{{ write('orders_daily') }}", `tools.replaceTable(fx, "table-ledger")`} {
		if !strings.Contains(main, want) {
			t.Fatalf("main.py does not hold %s:\n%s", want, main)
		}
	}
	wm := loadAnyManifest(t, filepath.Join(dir, "workflows/sales-report"))
	wantDeps := map[string]wfdir.Dependency{
		"sales_helpers": {Kind: markers.KindModule, From: "../../modules/sales_helpers"},
		"orders_clean":  {Kind: markers.KindTable, From: "../../pipelines/sales", Address: "orders_clean.sql"},
		"fx_rates":      {Kind: markers.KindTable},
		"crm":           {Kind: markers.KindSecret},
		"orders_daily":  {Kind: markers.KindTable, Output: true},
		"ledger":        {Kind: markers.KindTable, Write: true},
	}
	assertDeps(t, "workflow", wm.Dependencies, wantDeps)
	if bind := wm.Stacks["main"].Bind; bind["fx_rates"] != "table-fx-rates" || bind["crm"] != "secret-crm" ||
		bind["ledger"] != "table-ledger" || bind["orders_daily"] != "table-daily" || bind["orders_clean"] != "" || bind["sales_helpers"] != "" {
		t.Fatalf("workflow binds in main = %v", bind)
	}
	am := loadAnyManifest(t, filepath.Join(dir, "apps/sales-dashboard"))
	assertDeps(t, "app", am.Dependencies, map[string]wfdir.Dependency{
		"orders_clean": {Kind: markers.KindTable, From: "../../pipelines/sales", Address: "orders_clean.sql"},
		// A run edge out of the feature: "run": true, bound in main.
		"close_books": {Kind: markers.KindWorkflow, Run: true},
	})
	if access := am.DeclaredAccess(); len(access.AllowedTableIDs) != 1 || access.AllowedTableIDs[0] != "orders_clean" ||
		len(access.AllowedWorkflowIDs) != 1 || access.AllowedWorkflowIDs[0] != "close_books" {
		t.Fatalf("app access = %+v, want the address alias and the run source", access)
	}
	if bind := am.Stacks["main"].Bind; bind["close_books"] != "workflow-close" {
		t.Fatalf("app binds in main = %v, want the run source bound to workflow-close", bind)
	}
	if tsx := readFile(t, filepath.Join(dir, "apps/sales-dashboard/App.tsx")); tsx != importAppTSX {
		t.Fatalf("App.tsx was rewritten:\n%s", tsx)
	}
	auto := loadAnyManifest(t, filepath.Join(dir, "automations/sales"))
	assertDeps(t, "automations", auto.Dependencies, map[string]wfdir.Dependency{
		"sales_report":    {Kind: markers.KindWorkflow, From: "../../workflows/sales-report"},
		"sales_dashboard": {Kind: markers.KindDataApp, From: "../../apps/sales-dashboard"},
		"mcpserver_crm":   {Kind: markers.KindMCPServer},
	})
	nightly := readFile(t, filepath.Join(dir, "automations/sales/nightly.json"))
	if !strings.Contains(nightly, `"workflowID": "sales_report"`) {
		t.Fatalf("nightly.json does not run the workflow by address:\n%s", nightly)
	}
	assistant := readFile(t, filepath.Join(dir, "automations/sales/sales-assistant.json"))
	for _, want := range []string{`"resourceID": "sales_dashboard"`, `"resourceID": "@feature"`, `"resourceID": "mcpserver_crm"`} {
		if !strings.Contains(assistant, want) {
			t.Fatalf("sales-assistant.json does not hold %s:\n%s", want, assistant)
		}
	}
	if len(report.Outputs) != 1 || report.Outputs[0].Alias != "orders_daily" || report.Outputs[0].BoundTo != "table-daily" {
		t.Fatalf("outputs = %+v", report.Outputs)
	}
	pm := loadAnyManifest(t, filepath.Join(dir, "pipelines/sales"))
	assertDeps(t, "pipeline", pm.Dependencies, map[string]wfdir.Dependency{"raw_orders": {Kind: markers.KindTable}})

	// No literal id left anywhere a stem, address or alias can be written —
	// every id-shaped string in a written source file is one the report names.
	assertOnlyReportedLiterals(t, dir, report)

	// The tree is clean, and an apply of it writes NOTHING.
	f.resetRequests()
	status, err := runCLI(t, dir, "sync", "status", "--stack", "main", "--json")
	if err != nil {
		t.Fatalf("sync status: %v\n%s", err, status)
	}
	if decodeJSON(t, status)["verdict"] != verdictClean {
		t.Fatalf("sync status is not clean:\n%s", status)
	}
	if writes := f.writes(); len(writes) != 0 {
		t.Fatalf("sync status wrote %v", writes)
	}
	// Every reference the tree makes resolves — the addresses through the
	// claims the import made.
	check, err := runCLI(t, dir, "sync", "check", "--stack", "main", "--json")
	if err != nil || decodeJSON(t, check)["verdict"] != verdictClean {
		t.Fatalf("sync check: %v\n%s", err, check)
	}
	// (check's POST /workflow/validate is a read that persists nothing.)
	f.resetRequests()
	apply, err := runCLI(t, dir, "sync", "apply", "--stack", "main", "--json")
	if err != nil {
		t.Fatalf("sync apply: %v\n%s", err, apply)
	}
	if writes := f.writes(); len(writes) != 0 {
		t.Fatalf("the first apply of an imported tree sent %v\n%s", writes, apply)
	}
}

func itoa(n int) string { return strconv.Itoa(n) }

// idShaped matches any id this CLI can name, quoted or not.
var idShaped = regexp.MustCompile(`(?:table|modelv2|secret|agent|cdx|mailbox|workflow|module|note|skill|data_app|mcpsrv|collection|col)-[A-Za-z0-9_-]+`)

// assertOnlyReportedLiterals walks every written SOURCE file — not ronja.json
// (whose ids are the stack's binds and feature) and not the lock or baseline —
// and fails on any id-shaped string that is not a literal the report names.
func assertOnlyReportedLiterals(t *testing.T, dir string, report importReport) {
	t.Helper()
	reported := map[string]bool{}
	for _, l := range report.Literals {
		reported[l.Folder+"/"+l.Path+":"+itoa(l.Line)+":"+l.TargetID] = true
	}
	err := filepath.WalkDir(dir, func(path string, d os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			if d.Name() == wfdir.StateDirName {
				return filepath.SkipDir
			}
			return nil
		}
		if d.Name() == wfdir.ManifestName || d.Name() == wfdir.LockName {
			return nil
		}
		rel, _ := filepath.Rel(dir, path)
		rel = filepath.ToSlash(rel)
		raw, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		for i, line := range strings.Split(string(raw), "\n") {
			for _, id := range idShaped.FindAllString(line, -1) {
				folder, file := splitFolder(report, rel)
				if !reported[folder+"/"+file+":"+itoa(i+1)+":"+id] {
					t.Errorf("%s:%d holds the literal id %s, which the report does not name", rel, i+1, id)
				}
			}
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
}

// splitFolder splits a tree-relative path into the folder that holds it and
// the file's path inside that folder.
func splitFolder(report importReport, rel string) (string, string) {
	for _, f := range report.Folders {
		if strings.HasPrefix(rel, f.Path+"/") {
			return f.Path, strings.TrimPrefix(rel, f.Path+"/")
		}
	}
	return "", rel
}

// TestImportChecksClaimsFirst: a row another folder already manages is not
// imported, references to it become data sources, and the report names it.
func TestImportChecksClaimsFirst(t *testing.T) {
	f := importFixture(t)
	f.managed.stage(importFeature, addressTestFolderID, "orders_clean.sql", "table", "table-orders-clean", "github.com/acme/other//pipes")
	dir, report := runImport(t, f)
	if len(report.ClaimedElsewhere) != 1 || report.ClaimedElsewhere[0].ID != "table-orders-clean" {
		t.Fatalf("claimedElsewhere = %+v", report.ClaimedElsewhere)
	}
	if _, err := os.Stat(filepath.Join(dir, "pipelines/sales/orders_clean.sql")); !os.IsNotExist(err) {
		t.Fatalf("a row claimed elsewhere was imported: %v", err)
	}
	wm := loadAnyManifest(t, filepath.Join(dir, "workflows/sales-report"))
	if dep := wm.Dependencies["orders_clean"]; dep.From != "" || wm.Stacks["main"].Bind["orders_clean"] != "table-orders-clean" {
		t.Fatalf("a reference to a row claimed elsewhere is %+v bound %q, want a data source", dep, wm.Stacks["main"].Bind["orders_clean"])
	}
	for _, c := range report.Claims {
		if c.ID == "table-orders-clean" {
			t.Fatalf("the import claimed a row another folder manages: %+v", c)
		}
	}
	// The first request after the inventory is the claims read — before any
	// row's content.
	reqs := f.requests
	inv, managed, content := -1, -1, -1
	for i, r := range reqs {
		switch {
		case inv < 0 && strings.HasSuffix(r, "/references"):
			inv = i
		case managed < 0 && r == "GET /api/v2/managed":
			managed = i
		case content < 0 && (strings.HasPrefix(r, "GET /api/v2/workflow/") || strings.HasPrefix(r, "GET /api/v2/feature/model/")):
			content = i
		}
	}
	if !(inv < managed && managed < content) {
		t.Fatalf("claims were not read first: inventory %d, managed %d, content %d", inv, managed, content)
	}
}

// TestImportPartialClaimFailureWithdrawsTheRow: a row the server REFUSES to
// claim is reported, the import exits non-zero, and the row leaves the tree —
// its folder (a single-row kind) or its file (a pipeline table, with its lock
// and baseline entries) — so nothing unbound is left for a later
// `--allow-create` to turn into a second row.
func TestImportPartialClaimFailureWithdrawsTheRow(t *testing.T) {
	f := importFixture(t)
	f.managed.denied["analytics:write"] = true
	f.managed.admin = false // tables and metrics are claimed by an Admin only
	dir := filepath.Join(t.TempDir(), "repo")
	out, err := runCLI(t, t.TempDir(), "import", "feature", importFeature, dir, "--json")
	if err == nil {
		t.Fatalf("an import with refused claims exited zero:\n%s", out)
	}
	var report importReport
	if err := json.Unmarshal([]byte(out), &report); err != nil {
		t.Fatalf("%v\n%s", err, out)
	}
	refused := map[string]bool{}
	for _, c := range report.Claims {
		if c.Outcome == api.ManagedOutcomeRefused {
			refused[c.ID] = true
		}
	}
	if len(refused) != 3 || !refused["data_app-dash"] || !refused["table-orders-clean"] || !refused["table-aov"] || report.Verdict != verdictRefused {
		t.Fatalf("claims = %+v, verdict %s", report.Claims, report.Verdict)
	}
	if _, err := os.Stat(filepath.Join(dir, "apps/sales-dashboard")); !os.IsNotExist(err) {
		t.Fatalf("the refused app's folder is still in the tree: %v", err)
	}
	for _, gone := range []string{"orders_clean.sql", "metrics/aov.json"} {
		if _, err := os.Stat(filepath.Join(dir, "pipelines/sales", gone)); !os.IsNotExist(err) {
			t.Fatalf("the refused %s is still in the tree: %v", gone, err)
		}
	}
	lock, err := wfdir.LoadLock(filepath.Join(dir, "pipelines/sales"))
	if err != nil {
		t.Fatal(err)
	}
	if id := lock.ManagedID("main", wfdir.PipelineKind, "orders_clean.sql"); id != "" {
		t.Fatalf("the lock still binds the refused table: %s", id)
	}
	if lock, _ := wfdir.LoadLock(filepath.Join(dir, "workflows/sales-report")); lock.ManagedID("main", wfdir.WorkflowKind, "") != "workflow-report" {
		t.Fatal("a row that was claimed lost its lock entry")
	}
}

// TestImportKeepsTheLockWhenTheClaimRequestFails: a claim REQUEST refused as a
// whole (here a feature the token may not write) says nothing about any row,
// so every row keeps its lock entry — the next deploy's lazy claim, or `sync
// adopt`, claims it — and the import exits non-zero naming the remedy.
func TestImportKeepsTheLockWhenTheClaimRequestFails(t *testing.T) {
	f := importFixture(t)
	f.managed.readOnlyFeatures[importFeature] = true
	dir := filepath.Join(t.TempDir(), "repo")
	out, err := runCLI(t, t.TempDir(), "import", "feature", importFeature, dir, "--json")
	if err == nil {
		t.Fatalf("an import whose claims all failed exited zero:\n%s", out)
	}
	var report importReport
	if err := json.Unmarshal([]byte(out), &report); err != nil {
		t.Fatalf("%v\n%s", err, out)
	}
	for _, c := range report.Claims {
		if c.Outcome != importOutcomeUnclaimed {
			t.Fatalf("claim %+v, want unclaimed", c)
		}
	}
	if !strings.Contains(strings.Join(report.Problems, "\n"), "sync adopt --stack main") {
		t.Fatalf("problems do not name the remedy: %v", report.Problems)
	}
	if lock, _ := wfdir.LoadLock(filepath.Join(dir, "workflows/sales-report")); lock.ManagedID("main", wfdir.WorkflowKind, "") != "workflow-report" {
		t.Fatal("a failed claim request dropped the workflow's lock entry")
	}
	if lock, _ := wfdir.LoadLock(filepath.Join(dir, "pipelines/sales")); lock.ManagedID("main", wfdir.PipelineKind, "orders_clean.sql") != "table-orders-clean" {
		t.Fatal("a failed claim request dropped the table's lock entry")
	}
}

// TestImportRefusesANonEmptyDestination, before a single request.
func TestImportRefusesANonEmptyDestination(t *testing.T) {
	f := importFixture(t)
	dir := t.TempDir()
	writeLocal(t, dir, "README.md", "hi\n")
	_, err := runCLI(t, dir, "import", "feature", importFeature, dir)
	if err == nil || !strings.Contains(err.Error(), "not empty") {
		t.Fatalf("a non-empty destination was not refused: %v", err)
	}
	if len(f.requests) != 0 {
		t.Fatalf("a refused import sent %v", f.requests)
	}
}

// TestImportIntoAFreshGitRepository: a directory holding only `.git` — just
// after `git init`, or a clone of an empty repository — is empty as far as
// import is concerned, and the tree lands beside it; `.git` plus anything
// else is still refused.
func TestImportIntoAFreshGitRepository(t *testing.T) {
	importFixture(t)
	dir := t.TempDir()
	if err := os.Mkdir(filepath.Join(dir, ".git"), 0o755); err != nil {
		t.Fatal(err)
	}
	writeLocal(t, dir, ".git/HEAD", "ref: refs/heads/main\n")
	if out, err := runCLI(t, dir, "import", "feature", importFeature, dir, "--json"); err != nil {
		t.Fatalf("import into a fresh git repository: %v\n%s", err, out)
	}
	if _, err := os.Stat(filepath.Join(dir, "workflows/sales-report", wfdir.ManifestName)); err != nil {
		t.Fatalf("the tree did not land beside .git: %v", err)
	}
	if got := readFile(t, filepath.Join(dir, ".git/HEAD")); got != "ref: refs/heads/main\n" {
		t.Fatalf(".git was touched: HEAD = %q", got)
	}

	importFixture(t)
	other := t.TempDir()
	if err := os.Mkdir(filepath.Join(other, ".git"), 0o755); err != nil {
		t.Fatal(err)
	}
	writeLocal(t, other, "README.md", "hi\n")
	if _, err := runCLI(t, other, "import", "feature", importFeature, other); err == nil || !strings.Contains(err.Error(), "not empty") {
		t.Fatalf("a repository with a file in it was not refused: %v", err)
	}
}

// TestImportSlugsAreCollisionSafe: two workflows whose names slug alike get two
// folders.
func TestImportSlugsAreCollisionSafe(t *testing.T) {
	f := importFixture(t)
	f.wf.AddWorkflow(&api.Workflow{ID: "workflow-report2", Lifecycle: api.LifecycleLive, FeatureID: importFeature,
		Title: "Sales Report!", RuntimeVersion: wfdir.RuntimeDefault}, api.WorkflowFile{Path: "main.py", Content: "x = 1\n"})
	f.wf.versions["workflow-report2"] = []api.Workflow{{ID: "wfv-2"}}
	f.inv.add(importFeature, api.ReferenceResource{Kind: "workflow", ID: "workflow-report2", Name: "Sales Report!"})
	dir, report := runImport(t, f)
	paths := map[string]string{}
	for _, folder := range report.Folders {
		if folder.Kind == wfdir.KindWorkflow {
			paths[folder.Resources[0].ID] = folder.Path
		}
	}
	if paths["workflow-report"] != "workflows/sales-report" || paths["workflow-report2"] != "workflows/sales-report-2" {
		t.Fatalf("workflow folders = %v", paths)
	}
	if got := readFile(t, filepath.Join(dir, "workflows/sales-report-2/main.py")); got != "x = 1\n" {
		t.Fatalf("the second folder holds %q", got)
	}
}

// TestImportWalksEveryPageOfTheInventory: the walk ends when `next` is absent —
// an EMPTY first page carrying a cursor and short pages after it — never when
// a page is short.
func TestImportWalksEveryPageOfTheInventory(t *testing.T) {
	f := importFixture(t)
	f.inv.shortPages, f.inv.emptyPageFirst = 2, true
	_, report := runImport(t, f)
	if f.inv.pagesServed < 5 {
		t.Fatalf("the inventory was read in %d pages", f.inv.pagesServed)
	}
	if len(report.Claims) != 7 {
		t.Fatalf("a paged walk imported %d rows, want 7", len(report.Claims))
	}
}

// TestImportRefusesAnInventoryWithOmittedKinds: a kind the token may not read
// would leave its rows out and their references unrewritten.
func TestImportRefusesAnInventoryWithOmittedKinds(t *testing.T) {
	f := importFixture(t)
	f.inv.omitted = []api.OmittedRefKind{{Kind: "secret", Reason: "insufficient_scope", Scope: "secrets"}}
	_, err := runCLI(t, t.TempDir(), "import", "feature", importFeature, filepath.Join(t.TempDir(), "r"))
	if err == nil || !strings.Contains(err.Error(), "secrets:read") {
		t.Fatalf("omitted kinds were not refused: %v", err)
	}
}

// TestImportResolvesAFeatureByNameAmongOrganizationFeatures: a name resolves to
// the ONE organization feature called exactly that; a private feature of the
// same name is not it.
func TestImportResolvesAFeatureByNameAmongOrganizationFeatures(t *testing.T) {
	f := importFixture(t)
	f.pipe.AddFeature("collection-mine", "Sales", "private")
	f.searchHits = []api.SearchHit{
		{Kind: "feature", ID: importFeature, Title: "Sales", Score: 3},
		{Kind: "feature", ID: "collection-mine", Title: "Sales", Score: 3},
		{Kind: "feature", ID: "collection-salesy", Title: "Salesy", Score: 2},
	}
	dir := filepath.Join(t.TempDir(), "repo")
	out, err := runCLI(t, t.TempDir(), "import", "feature", "Sales", dir, "--json")
	if err != nil {
		t.Fatalf("import by name: %v\n%s", err, out)
	}
	if decodeJSON(t, out)["featureID"] != importFeature {
		t.Fatalf("the name resolved to %v", decodeJSON(t, out)["featureID"])
	}
	_, err = runCLI(t, t.TempDir(), "import", "feature", "Nope", filepath.Join(t.TempDir(), "r"))
	if err == nil || !strings.Contains(err.Error(), "imported by its id") {
		t.Fatalf("an unknown name was not refused: %v", err)
	}
}

// TestImportFailingPartWayLeavesNothing: a failure while writing (here the
// second folder's files cannot be read) leaves the destination empty, so the
// import can simply be run again.
func TestImportFailingPartWayLeavesNothing(t *testing.T) {
	f := importFixture(t)
	f.fail["GET /api/v2/dataapp/data_app-dash/files"] = http.StatusInternalServerError
	parent := t.TempDir()
	dir := filepath.Join(parent, "repo")
	if _, err := runCLI(t, t.TempDir(), "import", "feature", importFeature, dir); err == nil {
		t.Fatal("an import whose app files could not be read succeeded")
	}
	entries, err := os.ReadDir(parent)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 0 {
		t.Fatalf("a failed import left %v behind", entries)
	}
	if writes := f.writes(); len(writes) != 0 {
		t.Fatalf("a failed import sent %v", writes)
	}
}

// TestImportListsAResourceOnce: a resource the inventory returns twice (a page
// boundary that moved under a concurrent write) is one folder, not two.
func TestImportListsAResourceOnce(t *testing.T) {
	f := importFixture(t)
	f.inv.add(importFeature, api.ReferenceResource{Kind: "module", ID: "module-helpers", Name: "sales_helpers"})
	f.inv.shortPages = 3
	_, report := runImport(t, f)
	if len(report.Claims) != 7 || len(report.Folders) != 5 {
		t.Fatalf("a repeated resource became %d claims in %d folders", len(report.Claims), len(report.Folders))
	}
}

// TestBindNeverInfersAnImportedWriteTarget is C1: import Sales, add a `dev`
// stack by hand, run `ronja bind --stack dev --yes`. The workflow's write-
// capable data source `ledger` is bound in main to the table it writes; the
// organization-wide search finds a table called exactly `ledger` — that very
// table — and binding it in dev would make dev's runs write production. Bind
// must leave it unbound, say why, and still bind the read it can answer.
func TestBindNeverInfersAnImportedWriteTarget(t *testing.T) {
	f := importFixture(t)
	dir, _ := runImport(t, f)
	f.pipe.AddFeature("collection-dev", "Sales dev", "organization")
	root := filepath.Join(dir, "workflows/sales-report")
	m := loadAnyManifest(t, root)
	main := m.Stacks["main"]
	m.Stacks["dev"] = wfdir.Stack{URL: main.URL, TenantID: main.TenantID, FeatureID: "collection-dev"}
	if err := wfdir.SaveManifest(root, m); err != nil {
		t.Fatal(err)
	}
	f.searchHits = []api.SearchHit{
		{Kind: api.SearchKindTable, ID: "table-ledger", Title: "ledger", Score: api.SearchScoreExact},
		{Kind: api.SearchKindTable, ID: "table-fx-rates", Title: "fx_rates", Score: api.SearchScoreExact},
	}
	out, _ := runCLI(t, root, "bind", "--stack", "dev", "--yes", "--json")
	var result bindResult
	if err := json.Unmarshal([]byte(out), &result); err != nil {
		t.Fatalf("%v\n%s", err, out)
	}
	for _, p := range result.Proposed {
		if p.Alias == "ledger" {
			t.Fatalf("bind proposed the write target ledger → %s for dev", p.ID)
		}
	}
	said := false
	for _, u := range result.Unresolved {
		if u.Alias == "ledger" && u.Reason == bindReasonWriteTarget && strings.Contains(u.Detail, "bound by hand, per stack") {
			said = true
		}
	}
	if !said {
		t.Fatalf("bind did not report ledger as a write target bound by hand: %+v", result.Unresolved)
	}
	after := loadAnyManifest(t, root)
	if id, ok := after.Stacks["dev"].Bind["ledger"]; ok {
		t.Fatalf("dev binds the write target ledger to %s", id)
	}
	if after.Stacks["dev"].Bind["fx_rates"] != "table-fx-rates" {
		t.Fatalf("the read was not bound in dev: %v", after.Stacks["dev"].Bind)
	}
	if after.Stacks["main"].Bind["ledger"] != "table-ledger" {
		t.Fatalf("main's write target binding changed: %v", after.Stacks["main"].Bind)
	}
}

// assertTreeClean runs `sync status` and `sync check` over an imported tree
// and fails unless both are clean — every address resolves, nothing drifts —
// and an apply of it sends no write.
func assertTreeClean(t *testing.T, f *fakeImportInstance, dir string) {
	t.Helper()
	status, err := runCLI(t, dir, "sync", "status", "--stack", "main", "--json")
	if err != nil || decodeJSON(t, status)["verdict"] != verdictClean {
		t.Fatalf("sync status of the imported tree: %v\n%s", err, status)
	}
	if strings.Contains(status, "address_") {
		t.Fatalf("sync status names an address problem:\n%s", status)
	}
	check, err := runCLI(t, dir, "sync", "check", "--stack", "main", "--json")
	if err != nil || decodeJSON(t, check)["verdict"] != verdictClean {
		t.Fatalf("sync check of the imported tree: %v\n%s", err, check)
	}
	f.resetRequests()
	apply, err := runCLI(t, dir, "sync", "apply", "--stack", "main", "--json")
	if err != nil {
		t.Fatalf("sync apply: %v\n%s", err, apply)
	}
	if writes := f.writes(); len(writes) != 0 {
		t.Fatalf("the first apply of the imported tree sent %v\n%s", writes, apply)
	}
}

// TestImportByANonAdminDeclaresTablesAsDataSources: a table or metric
// claim needs the Admin role, so a non-Admin's import plans the derived table
// and the metric as NOT imported — no pipeline folder, no claim it would be
// refused — and every reference to them as a data source bound in the
// importing stack. The tree is clean, the import exits zero, and the report
// says why the two were left out.
func TestImportByANonAdminDeclaresTablesAsDataSources(t *testing.T) {
	f := importFixture(t)
	f.wf.privilegeLevel = 50
	f.managed.admin = false
	dir, report := runImport(t, f)

	if _, err := os.Stat(filepath.Join(dir, "pipelines")); !os.IsNotExist(err) {
		t.Fatalf("a non-Admin's import wrote a pipeline folder: %v", err)
	}
	left := map[string]string{}
	for _, s := range report.NotImported {
		left[s.ID] = s.Reason
	}
	for _, id := range []string{"table-orders-clean", "table-aov"} {
		if left[id] != importReasonNeedsAdmin {
			t.Fatalf("notImported does not name %s as needing an Admin: %+v", id, report.NotImported)
		}
	}
	for _, c := range report.Claims {
		if c.Kind == "table" || c.Kind == "metric" || c.Outcome != api.ManagedOutcomeClaimed {
			t.Fatalf("claim %+v", c)
		}
	}
	if len(report.Claims) != 5 || len(report.Problems) != 0 || report.Verdict != verdictApplied {
		t.Fatalf("claims %+v, problems %v, verdict %s", report.Claims, report.Problems, report.Verdict)
	}
	wm := loadAnyManifest(t, filepath.Join(dir, "workflows/sales-report"))
	if dep := wm.Dependencies["orders_clean"]; dep.ByAddress() || dep.Kind != markers.KindTable || wm.Stacks["main"].Bind["orders_clean"] != "table-orders-clean" {
		t.Fatalf("the workflow reads the table as %+v bound %q, want a data source", dep, wm.Stacks["main"].Bind["orders_clean"])
	}
	am := loadAnyManifest(t, filepath.Join(dir, "apps/sales-dashboard"))
	if dep := am.Dependencies["orders_clean"]; dep.ByAddress() || am.Stacks["main"].Bind["orders_clean"] != "table-orders-clean" {
		t.Fatalf("the app's access names the table as %+v, want a data source", dep)
	}
	for _, a := range report.Addresses {
		if strings.HasPrefix(a.From, "../../pipelines") {
			t.Fatalf("the report lists an address into a pipeline that was not written: %+v", a)
		}
	}
	assertTreeClean(t, f, dir)
}

// TestImportRewritesTheDependentsOfARefusedRow: /me says Admin, and the
// server refuses the table, the metric and the app anyway. Each leaves the tree,
// and every folder that referenced one is planned and written again with that
// reference as a data source — the workflow's read of the table, the
// automation's reference to the app — so `sync status` of what is left is clean
// rather than full of addresses nothing will ever claim.
func TestImportRewritesTheDependentsOfARefusedRow(t *testing.T) {
	f := importFixture(t)
	f.managed.denied["analytics:write"] = true
	f.managed.admin = false
	dir := filepath.Join(t.TempDir(), "repo")
	out, _ := runCLI(t, t.TempDir(), "import", "feature", importFeature, dir, "--json")
	var report importReport
	if err := json.Unmarshal([]byte(out), &report); err != nil {
		t.Fatalf("%v\n%s", err, out)
	}
	if _, err := os.Stat(filepath.Join(dir, "pipelines/sales")); !os.IsNotExist(err) {
		t.Fatalf("the pipeline whose every row was refused is still in the tree: %v", err)
	}
	wm := loadAnyManifest(t, filepath.Join(dir, "workflows/sales-report"))
	if dep := wm.Dependencies["orders_clean"]; dep.ByAddress() || wm.Stacks["main"].Bind["orders_clean"] != "table-orders-clean" {
		t.Fatalf("the workflow still reads the refused table as %+v", dep)
	}
	auto := loadAnyManifest(t, filepath.Join(dir, "automations/sales"))
	if dep := auto.Dependencies["sales_dashboard"]; dep.ByAddress() || auto.Stacks["main"].Bind["sales_dashboard"] != "data_app-dash" {
		t.Fatalf("the automation still names the refused app as %+v", dep)
	}
	for _, a := range report.Addresses {
		if a.Folder == "workflows/sales-report" && a.Alias == "orders_clean" || a.Folder == "automations/sales" && a.Alias == "sales_dashboard" {
			t.Fatalf("the report still lists the withdrawn address %+v", a)
		}
	}
	for _, c := range report.Claims {
		if c.Outcome == api.ManagedOutcomeRefused && !strings.Contains(c.Detail, "every reference to it declared as a data source") {
			t.Fatalf("a refused claim does not say what became of it: %+v", c)
		}
	}
	assertTreeClean(t, f, dir)
}

// TestImportRewritesTheDependentsOfARefusedWorkflow: another folder claims the
// workflow between the import's claims read and its claim (resource_claimed).
// The workflow's folder leaves the tree, and the automation that ran it by
// address runs it as a data source bound in this stack instead.
func TestImportRewritesTheDependentsOfARefusedWorkflow(t *testing.T) {
	f := importFixture(t)
	f.managed.afterList = func(m *fakeManaged) {
		m.insert(importFeature, "workflow", "workflow-report", api.ManagedClaimInput{FolderID: addressTestFolderID, StackLabel: "prod", SourceLabel: "github.com/acme/other//wf"})
	}
	dir := filepath.Join(t.TempDir(), "repo")
	out, err := runCLI(t, t.TempDir(), "import", "feature", importFeature, dir, "--json")
	if err == nil {
		t.Fatalf("an import with a refused claim exited zero:\n%s", out)
	}
	if _, err := os.Stat(filepath.Join(dir, "workflows/sales-report")); !os.IsNotExist(err) {
		t.Fatalf("the refused workflow's folder is still in the tree: %v", err)
	}
	auto := loadAnyManifest(t, filepath.Join(dir, "automations/sales"))
	if dep := auto.Dependencies["sales_report"]; dep.ByAddress() || dep.Kind != markers.KindWorkflow || auto.Stacks["main"].Bind["sales_report"] != "workflow-report" {
		t.Fatalf("the automation runs the refused workflow as %+v bound %q, want a data source", dep, auto.Stacks["main"].Bind["sales_report"])
	}
	if nightly := readFile(t, filepath.Join(dir, "automations/sales/nightly.json")); !strings.Contains(nightly, `"workflowID": "sales_report"`) {
		t.Fatalf("nightly.json:\n%s", nightly)
	}
	assertTreeClean(t, f, dir)
}

// unclaimedProblems counts the report's problems that name each row.
func problemsNaming(report importReport, folder, address string) int {
	n := 0
	for _, p := range report.Problems {
		if strings.HasPrefix(p, folder+" "+describeAddressKey(address)) {
			n++
		}
	}
	return n
}

// TestImportKeepsARowTheServerFaultedOn: a per-item `internal_error` is
// a server fault on that one row, not a refusal of it ("retry the item"), so
// the row stays in the tree with its lock entry, is reported unclaimed once,
// and the remedy is `sync adopt` — and a code this build does not know is
// treated the same way, never as a reason to delete what was written.
func TestImportKeepsARowTheServerFaultedOn(t *testing.T) {
	f := importFixture(t)
	f.managed.itemCode = map[string]string{"workflow-report": api.ManagedCodeInternalError, "data_app-dash": "some_future_code"}
	dir := filepath.Join(t.TempDir(), "repo")
	out, err := runCLI(t, t.TempDir(), "import", "feature", importFeature, dir, "--json")
	if err == nil {
		t.Fatalf("an import with unclaimed rows exited zero:\n%s", out)
	}
	var report importReport
	if err := json.Unmarshal([]byte(out), &report); err != nil {
		t.Fatalf("%v\n%s", err, out)
	}
	for _, c := range report.Claims {
		switch c.ID {
		case "workflow-report", "data_app-dash":
			if c.Outcome != importOutcomeUnclaimed {
				t.Fatalf("a faulted claim is %+v, want unclaimed", c)
			}
		default:
			if c.Outcome != api.ManagedOutcomeClaimed {
				t.Fatalf("claim %+v", c)
			}
		}
	}
	if lock, _ := wfdir.LoadLock(filepath.Join(dir, "workflows/sales-report")); lock.ManagedID("main", wfdir.WorkflowKind, "") != "workflow-report" {
		t.Fatal("a faulted claim dropped the workflow's lock entry")
	}
	if _, err := os.Stat(filepath.Join(dir, "apps/sales-dashboard/App.tsx")); err != nil {
		t.Fatalf("a faulted claim removed the app: %v", err)
	}
	for _, folder := range []string{"workflows/sales-report", "apps/sales-dashboard"} {
		if n := problemsNaming(report, folder, ""); n != 1 {
			t.Fatalf("%s is named by %d problems, want 1: %v", folder, n, report.Problems)
		}
	}
	if !strings.Contains(strings.Join(report.Problems, "\n"), "sync adopt --stack main") {
		t.Fatalf("problems do not name the remedy: %v", report.Problems)
	}
	// The addresses into them still stand: they are this tree's rows.
	if dep := loadAnyManifest(t, filepath.Join(dir, "automations/sales")).Dependencies["sales_report"]; !dep.ByAddress() {
		t.Fatalf("the automation no longer runs the faulted workflow by address: %+v", dep)
	}
}

// TestImportReportsAnUnclaimedRowOnceAndAsItIs: after a claim request
// that failed as a whole, the anchor step must not claim the rows behind the
// report's back (here the retry would succeed: the report said "unclaimed" of
// rows the server then held) nor fail a second time with another message. The
// rows stay unclaimed — the report is true — and each is named once.
func TestImportReportsAnUnclaimedRowOnceAndAsItIs(t *testing.T) {
	f := importFixture(t)
	f.failOnce["POST /api/v2/managed/claim"] = http.StatusBadRequest
	dir := filepath.Join(t.TempDir(), "repo")
	out, err := runCLI(t, t.TempDir(), "import", "feature", importFeature, dir, "--json")
	if err == nil {
		t.Fatalf("an import whose claim request failed exited zero:\n%s", out)
	}
	var report importReport
	if err := json.Unmarshal([]byte(out), &report); err != nil {
		t.Fatalf("%v\n%s", err, out)
	}
	unclaimed := 0
	for _, c := range report.Claims {
		if c.Outcome != importOutcomeUnclaimed {
			t.Fatalf("claim %+v, want unclaimed", c)
		}
		unclaimed++
		if n := problemsNaming(report, c.Folder, c.Address); n != 1 {
			t.Fatalf("%s %s is named by %d problems, want 1: %v", c.Folder, c.Address, n, report.Problems)
		}
	}
	if unclaimed != 7 || len(report.Problems) != 7 {
		t.Fatalf("%d unclaimed rows, %d problems: %v", unclaimed, len(report.Problems), report.Problems)
	}
	f.managed.mu.Lock()
	held := len(f.managed.claims)
	f.managed.mu.Unlock()
	if held != 0 {
		t.Fatalf("the report says every row is unclaimed, and the server holds %d claims", held)
	}
}

// TestImportReportsAFeatureItMayNotClaimInOncePerRow is the report-once rule with a claim
// request that fails every time: one message per row, not a second one from
// the anchor step trying again.
func TestImportReportsAFeatureItMayNotClaimInOncePerRow(t *testing.T) {
	f := importFixture(t)
	f.managed.readOnlyFeatures[importFeature] = true
	dir := filepath.Join(t.TempDir(), "repo")
	out, _ := runCLI(t, t.TempDir(), "import", "feature", importFeature, dir, "--json")
	var report importReport
	if err := json.Unmarshal([]byte(out), &report); err != nil {
		t.Fatalf("%v\n%s", err, out)
	}
	for _, c := range report.Claims {
		if n := problemsNaming(report, c.Folder, c.Address); n != 1 {
			t.Fatalf("%s %s is named by %d problems, want 1: %v", c.Folder, c.Address, n, report.Problems)
		}
	}
	if len(report.Problems) != len(report.Claims) {
		t.Fatalf("%d problems for %d rows: %v", len(report.Problems), len(report.Claims), report.Problems)
	}
}

// TestImportIntoTheCurrentDirectoryKeepsIt: `ronja import feature Sales`
// with no dir writes into the current directory, and that directory — the one
// the shell stands in — is the same directory afterwards, not a new one renamed
// over a removed one.
func TestImportIntoTheCurrentDirectoryKeepsIt(t *testing.T) {
	importFixture(t)
	dir := t.TempDir()
	before, err := os.Stat(dir)
	if err != nil {
		t.Fatal(err)
	}
	if out, err := runCLI(t, dir, "import", "feature", importFeature, "--json"); err != nil {
		t.Fatalf("import into the current directory: %v\n%s", err, out)
	}
	after, err := os.Stat(dir)
	if err != nil {
		t.Fatal(err)
	}
	if !os.SameFile(before, after) {
		t.Fatal("the import replaced the current directory with another one")
	}
	if _, err := os.Stat(filepath.Join(dir, "workflows/sales-report/main.py")); err != nil {
		t.Fatalf("the tree is not in the current directory: %v", err)
	}
	entries, _ := os.ReadDir(dir)
	for _, e := range entries {
		if strings.HasPrefix(e.Name(), importStagingPrefix) {
			t.Fatalf("the import left its staging directory %s behind", e.Name())
		}
	}
}

// TestImportFailingPartWayLeavesTheDestinationEmpty: a write that fails (the
// app's files cannot be read), and a move into place that fails after some
// entries were already moved, each leave an existing destination as it was —
// the same directory, and empty.
func TestImportFailingPartWayLeavesTheDestinationEmpty(t *testing.T) {
	for _, tc := range []struct {
		name  string
		stage func(t *testing.T, f *fakeImportInstance)
	}{
		{"a write fails", func(_ *testing.T, f *fakeImportInstance) {
			f.fail["GET /api/v2/dataapp/data_app-dash/files"] = http.StatusInternalServerError
		}},
		{"a move fails", func(t *testing.T, _ *fakeImportInstance) {
			calls := 0
			importRename = func(from, to string) error {
				if calls++; calls == 3 {
					return os.ErrPermission
				}
				return os.Rename(from, to)
			}
			t.Cleanup(func() { importRename = os.Rename })
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := importFixture(t)
			tc.stage(t, f)
			dir := t.TempDir()
			before, _ := os.Stat(dir)
			if _, err := runCLI(t, dir, "import", "feature", importFeature); err == nil {
				t.Fatal("the import succeeded")
			}
			after, err := os.Stat(dir)
			if err != nil || !os.SameFile(before, after) {
				t.Fatalf("the destination was removed or replaced: %v", err)
			}
			if entries, _ := os.ReadDir(dir); len(entries) != 0 {
				t.Fatalf("a failed import left %v in the destination", entries)
			}
			if writes := f.writes(); len(writes) != 0 {
				t.Fatalf("a failed import sent %v", writes)
			}
		})
	}
}

// TestImportNamesAStaleStagingDirectory: a staging directory left by an import
// that was killed while writing is named, and left alone — it cannot be told
// from one an import is writing right now.
func TestImportNamesAStaleStagingDirectory(t *testing.T) {
	f := importFixture(t)
	dir := t.TempDir()
	stale := filepath.Join(dir, importStagingPrefix+"123")
	writeLocal(t, stale, "workflows/x/main.py", "x = 1\n")
	_, err := runCLI(t, dir, "import", "feature", importFeature)
	if err == nil || !strings.Contains(err.Error(), importStagingPrefix+"123") || !strings.Contains(err.Error(), "did not finish") {
		t.Fatalf("a stale staging directory was not named: %v", err)
	}
	if _, err := os.Stat(filepath.Join(stale, "workflows/x/main.py")); err != nil {
		t.Fatalf("the stale staging directory was touched: %v", err)
	}
	if len(f.requests) != 0 {
		t.Fatalf("a refused import sent %v", f.requests)
	}
}

// TestImportReportsWhatItDoesNotCarry is M2 end to end: the workflow holds an
// explicit secret and a binding column left from earlier code, and the table an
// input its SQL no longer reads. None is in a file, each is listed under
// notCarried — in --json and in the human report — and the tree is still clean.
func TestImportReportsWhatItDoesNotCarry(t *testing.T) {
	f := notCarriedFixture(t)
	dir, report := runImport(t, f)
	got := map[string]string{}
	for _, c := range report.NotCarried {
		got[c.ResourceID+" "+c.Field+" "+c.TargetID] = c.Effect
	}
	for key, effect := range map[string]string{
		"workflow-report explicitSecretIDs[0] secret-extra": "runs without that secret",
		"workflow-report inputTableIDs[2] table-old":        "is not bound",
		"table-orders-clean inputModels[1] table-gone":      "does not list it",
	} {
		if !strings.Contains(got[key], effect) {
			t.Fatalf("notCarried has no %s naming %q: %+v", key, effect, report.NotCarried)
		}
	}
	assertTreeClean(t, f, dir)

	notCarriedFixture(t)
	human, err := runCLI(t, t.TempDir(), "import", "feature", importFeature, filepath.Join(t.TempDir(), "again"))
	if err != nil {
		t.Fatalf("%v\n%s", err, human)
	}
	if !strings.Contains(human, "Not carried (3)") || !strings.Contains(human, "explicitSecretIDs[0]") {
		t.Fatalf("the human report does not list what was not carried:\n%s", human)
	}
}

func notCarriedFixture(t *testing.T) *fakeImportInstance {
	t.Helper()
	f := importFixture(t)
	f.inv.mu.Lock()
	for i := range f.inv.resources[importFeature] {
		res := &f.inv.resources[importFeature][i]
		switch res.ID {
		case "workflow-report":
			res.References = append(res.References,
				api.ReferencePosition{Position: api.RefPosStructuredField, Direction: api.RefDirSideEffect, TargetKind: "secret", TargetID: "secret-extra", Field: "explicitSecretIDs[0]"},
				api.ReferencePosition{Position: api.RefPosStructuredField, Direction: api.RefDirRead, TargetKind: "table", TargetID: "table-old", Field: "inputTableIDs[2]"})
		case "table-orders-clean":
			res.References = append(res.References,
				api.ReferencePosition{Position: api.RefPosStructuredField, Direction: api.RefDirRead, TargetKind: "table", TargetID: "table-gone", Field: "inputModels[1]"})
		}
	}
	f.inv.mu.Unlock()
	return f
}

// TestImportListsEveryResourceItLeftOut is M6: every resource the inventory
// lists and no folder holds — a connector table, a workflow's output, a saved
// agent, a note, a secret, a codex, an MCP server — is in notImported with why,
// in --json and in the human report.
func TestImportListsEveryResourceItLeftOut(t *testing.T) {
	fixture := func() *fakeImportInstance {
		f := importFixture(t)
		for _, res := range []api.ReferenceResource{
			{Kind: "agent", ID: "agent-triage", Name: "Triage"},
			{Kind: "note", ID: "note-policy", Name: "Policy"},
			{Kind: "secret", ID: "secret-crm", Name: "crm"},
			{Kind: "codex", ID: "cdx-docs", Name: "Docs"},
			{Kind: "mcpserver", ID: "mcpsrv-crm", Name: "CRM"},
		} {
			f.inv.add(importFeature, res)
		}
		return f
	}
	_, report := runImport(t, fixture())
	why := map[string]string{}
	for _, s := range report.NotImported {
		why[s.ID] = s.Reason
	}
	for id, want := range map[string]string{
		"table-raw-orders": "kind integration has no SQL",
		"table-daily":      "the output of workflow workflow-report",
		"agent-triage":     "saved agent is not a kind the tree carries",
		"note-policy":      "note is not a kind the tree carries",
		"secret-crm":       "never goes into a repository",
		"cdx-docs":         "codex is not a kind the tree carries",
		"mcpsrv-crm":       "MCP server connection is not a kind the tree carries",
	} {
		if !strings.Contains(why[id], want) {
			t.Fatalf("notImported gives %s as %q, want %q: %+v", id, why[id], want, report.NotImported)
		}
	}
	fixture()
	human, err := runCLI(t, t.TempDir(), "import", "feature", importFeature, filepath.Join(t.TempDir(), "again"))
	if err != nil {
		t.Fatalf("%v\n%s", err, human)
	}
	if !strings.Contains(human, "Not imported (7)") || !strings.Contains(human, "agent-triage") {
		t.Fatalf("the human report does not list what was left out:\n%s", human)
	}
}

// TestImportKeepsAnUnclaimedRowUnclaimedThroughTheRewrite: a folder with one
// row that got no claim answer (unclaimed) and one that was claimed, and which
// names a row the server refused, is written again by settle and re-opened —
// and the re-opened folder must still know the row is unclaimed, or its anchor
// step claims it behind the report's back.
func TestImportKeepsAnUnclaimedRowUnclaimedThroughTheRewrite(t *testing.T) {
	f := importFixture(t)
	f.managed.itemCode = map[string]string{"sjob-nightly": api.ManagedCodeInternalError}
	f.managed.itemCodeOnce = true
	f.managed.afterList = func(m *fakeManaged) {
		m.insert(importFeature, "dataapp", "data_app-dash", api.ManagedClaimInput{FolderID: addressTestFolderID, StackLabel: "prod", SourceLabel: "github.com/acme/other//app"})
	}
	dir := filepath.Join(t.TempDir(), "repo")
	out, err := runCLI(t, t.TempDir(), "import", "feature", importFeature, dir, "--json")
	if err == nil {
		t.Fatalf("an import with an unclaimed and a refused row exited zero:\n%s", out)
	}
	var report importReport
	if err := json.Unmarshal([]byte(out), &report); err != nil {
		t.Fatalf("%v\n%s", err, out)
	}
	root := filepath.Join(dir, "automations/sales")
	if am := loadAnyManifest(t, root); am.Dependencies["sales_dashboard"].ByAddress() {
		t.Fatal("the automations were not rewritten without the refused app — the scenario did not happen")
	}
	if c := f.managed.ClaimAt(importFeature, folderIDOf(t, root), "nightly.json"); c != nil {
		t.Fatalf("the report says nightly.json is unclaimed, and the server holds %+v", c)
	}
	if c := f.managed.ClaimAt(importFeature, folderIDOf(t, root), "sales-assistant.json"); c == nil {
		t.Fatal("the claimed automation is not claimed")
	}
}

// TestImportAgainstAnInstanceWithoutTheInventoryRefusesByName: an instance
// older than the reference inventory answers its route with the router's
// plain-text 404; the import says the instance is too old, not "not found".
func TestImportAgainstAnInstanceWithoutTheInventoryRefusesByName(t *testing.T) {
	f := importFixture(t)
	f.noInventoryRoute = true
	dir := filepath.Join(t.TempDir(), "repo")
	_, err := runCLI(t, t.TempDir(), "import", "feature", importFeature, dir)
	if err == nil || !strings.Contains(err.Error(), "older than this CLI") || !strings.Contains(err.Error(), "nothing was written") {
		t.Fatalf("import against an instance without the inventory: %v", err)
	}
	if _, statErr := os.Stat(dir); !os.IsNotExist(statErr) {
		t.Fatalf("the refused import left %s behind", dir)
	}
}
