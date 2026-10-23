package commands

import (
	"encoding/json"
	"path/filepath"
	"regexp"
	"strings"
	"testing"

	"github.com/ronjatech/ronja-cli/internal/api"
	"github.com/ronjatech/ronja-cli/internal/markers"
	"github.com/ronjatech/ronja-cli/internal/wfdir"
)

// Coverage the import's first review asked for: each a position or a carrier
// the proof fixture did not hold, imported end to end into a tree whose
// `sync status` and `sync check` are clean and whose apply sends no write.

// TestImportCarriesChecksAndDocumentation: a derived table with an enabled
// health check and a `-- @table` documentation header imports to its .sql file
// (header included) and its checks/<stem>.json, and the tree applies clean. A
// docs sidecar added afterwards for a table the pipeline reads (its data
// source) deploys once and then never again.
func TestImportCarriesChecksAndDocumentation(t *testing.T) {
	f := importFixture(t)
	const documented = "-- @table One row per cleaned order.\nSELECT * FROM {{ ref('table-raw-orders') }}"
	f.pipe.mu.Lock()
	f.pipe.tables["table-orders-clean"].Code = documented
	f.pipe.mu.Unlock()
	f.pipe.AddCheck("table-orders-clean", api.TableCheck{Name: "Has rows", Expression: "count(*) > 0", Enabled: true})
	f.pipe.AddCheck("table-orders-clean", api.TableCheck{Name: "Off", Expression: "false", Enabled: false})
	dir, _ := runImport(t, f)

	root := filepath.Join(dir, "pipelines/sales")
	if sql := readFile(t, filepath.Join(root, "orders_clean.sql")); !strings.HasPrefix(sql, "-- @table One row per cleaned order.\n") || !strings.Contains(sql, "{{ ref('raw_orders') }}") {
		t.Fatalf("orders_clean.sql:\n%s", sql)
	}
	checks := readFile(t, filepath.Join(root, "checks/orders_clean.json"))
	if !strings.Contains(checks, "Has rows") || strings.Contains(checks, `"Off"`) {
		t.Fatalf("checks/orders_clean.json holds:\n%s", checks)
	}
	assertTreeClean(t, f, dir)

	writeSidecar(t, root, "raw_orders", `{"description": "One row per order, as the shop sent it."}`)
	if out, err := runCLI(t, dir, "sync", "apply", "--stack", "main", "--json"); err != nil {
		t.Fatalf("apply the sidecar: %v\n%s", err, out)
	}
	f.pipe.mu.Lock()
	described := f.pipe.tables["table-raw-orders"].Description
	f.pipe.mu.Unlock()
	if described != "One row per order, as the shop sent it." {
		t.Fatalf("the sidecar did not deploy: %q", described)
	}
	assertTreeClean(t, f, dir)
}

// TestImportWithAStackFlag: `--stack dev` names the stack every folder
// declares, every claim carries it, the report says it, and that stack is the
// one that applies clean.
func TestImportWithAStackFlag(t *testing.T) {
	f := importFixture(t)
	dir, report := runImport(t, f, "--stack", "dev")
	if report.Stack != "dev" {
		t.Fatalf("report stack = %q", report.Stack)
	}
	for _, folder := range report.Folders {
		m := loadAnyManifest(t, filepath.Join(dir, folder.Path))
		if _, ok := m.Stacks["dev"]; !ok || len(m.Stacks) != 1 || m.Stacks["dev"].FeatureID != importFeature {
			t.Fatalf("%s declares stacks %v, want dev alone", folder.Path, m.Stacks)
		}
	}
	for _, d := range report.DataSources {
		if d.Stack != "dev" {
			t.Fatalf("data source %+v is not bound in dev", d)
		}
	}
	f.managed.mu.Lock()
	for _, c := range f.managed.claims {
		if c.StackLabel != "dev" {
			t.Errorf("claim %s %s carries stack label %q", c.Kind, c.ResourceID, c.StackLabel)
		}
	}
	f.managed.mu.Unlock()
	status, err := runCLI(t, dir, "sync", "status", "--stack", "dev", "--json")
	if err != nil || decodeJSON(t, status)["verdict"] != verdictClean {
		t.Fatalf("sync status --stack dev: %v\n%s", err, status)
	}
	f.resetRequests()
	if out, err := runCLI(t, dir, "sync", "apply", "--stack", "dev", "--json"); err != nil {
		t.Fatalf("sync apply --stack dev: %v\n%s", err, out)
	}
	if writes := f.writes(); len(writes) != 0 {
		t.Fatalf("the first apply of a --stack dev import sent %v", writes)
	}
}

// TestImportAnOptionsQuery: a workflow parameter whose optionsQuery reads a
// table the pipeline builds is written by alias — the address — and the tree
// still applies clean, so the optionsQuery the server holds reads back equal.
func TestImportAnOptionsQuery(t *testing.T) {
	f := importFixture(t)
	f.wf.mu.Lock()
	f.wf.workflows["workflow-report"].Parameters = []api.WorkflowParameter{{
		Name: "region", Type: "select", OptionsQuery: "SELECT DISTINCT region FROM {{ ref('table-orders-clean') }}",
	}}
	f.wf.mu.Unlock()
	f.inv.mu.Lock()
	for i := range f.inv.resources[importFeature] {
		res := &f.inv.resources[importFeature][i]
		if res.ID == "workflow-report" {
			res.References = append(res.References, api.ReferencePosition{Position: api.RefPosOptionsQueryRef, Direction: api.RefDirRead,
				TargetKind: "table", TargetID: "table-orders-clean", Readable: true, TargetName: "orders_clean", Arg: "table-orders-clean", Field: "region"})
		}
	}
	f.inv.mu.Unlock()
	dir, report := runImport(t, f)
	m := loadAnyManifest(t, filepath.Join(dir, "workflows/sales-report"))
	params := m.DeclaredParameters()
	if len(params) != 1 || params[0].OptionsQuery != "SELECT DISTINCT region FROM {{ ref('orders_clean') }}" {
		t.Fatalf("parameters = %+v", params)
	}
	if dep := m.Dependencies["orders_clean"]; dep.From != "../../pipelines/sales" || dep.Address != "orders_clean.sql" {
		t.Fatalf("orders_clean = %+v, want the address", dep)
	}
	assertOnlyReportedLiterals(t, dir, report)
	assertTreeClean(t, f, dir)
}

// TestImportAutomationTriggerFields: a table trigger watching a table the
// pipeline builds and a connector table, and a mailbox trigger, are written by
// alias — the address, a read data source, a side-effect data source — and the
// tree applies clean.
func TestImportAutomationTriggerFields(t *testing.T) {
	f := importFixture(t)
	f.auto.AddAutomation(&api.Automation{ID: "sjob-watch", FeatureID: importFeature, Name: "on orders",
		TriggerKind: api.TriggerKindTable, WatchedTableIDs: []string{"table-orders-clean", "table-raw-orders"}, Enabled: true,
		Action: &api.AutomationAction{ID: "act-w", ScheduledJobID: "sjob-watch", Kind: api.ActionKindWorkflow,
			Config: api.AutomationActionConfig{WorkflowID: "workflow-report"}}})
	f.auto.AddAutomation(&api.Automation{ID: "sjob-mail", FeatureID: importFeature, Name: "on mail",
		TriggerKind: api.TriggerKindMailbox, MailboxID: "mailbox-ops", Enabled: true, Prompt: "Triage it",
		Action: &api.AutomationAction{ID: "act-m", ScheduledJobID: "sjob-mail", Kind: api.ActionKindAgent}})
	trigger := func(kind, id, name, field string) api.ReferencePosition {
		return api.ReferencePosition{Position: api.RefPosAutomationTrigger, Direction: api.RefDirRead, TargetKind: kind, TargetID: id,
			Readable: true, TargetName: name, Field: field}
	}
	f.inv.add(importFeature, api.ReferenceResource{Kind: "automation", ID: "sjob-watch", Name: "on orders", References: []api.ReferencePosition{
		trigger("table", "table-orders-clean", "orders_clean", "watchedTableIDs[0]"),
		trigger("table", "table-raw-orders", "raw_orders", "watchedTableIDs[1]"),
		{Position: api.RefPosAutomationAction, Direction: api.RefDirRun, TargetKind: "workflow", TargetID: "workflow-report", Readable: true, TargetName: "Sales report", Field: "action.workflowID"},
	}})
	f.inv.add(importFeature, api.ReferenceResource{Kind: "automation", ID: "sjob-mail", Name: "on mail", References: []api.ReferencePosition{
		trigger("mailbox", "mailbox-ops", "ops", "mailboxID"),
	}})
	dir, report := runImport(t, f)
	auto := loadAnyManifest(t, filepath.Join(dir, "automations/sales"))
	assertDeps(t, "automations", auto.Dependencies, map[string]wfdir.Dependency{
		"sales_report":    {Kind: markers.KindWorkflow, From: "../../workflows/sales-report"},
		"sales_dashboard": {Kind: markers.KindDataApp, From: "../../apps/sales-dashboard"},
		"mcpserver_crm":   {Kind: markers.KindMCPServer},
		"orders_clean":    {Kind: markers.KindTable, From: "../../pipelines/sales", Address: "orders_clean.sql"},
		"raw_orders":      {Kind: markers.KindTable},
		"ops":             {Kind: markers.KindMailbox},
	})
	if bind := auto.Stacks["main"].Bind; bind["raw_orders"] != "table-raw-orders" || bind["ops"] != "mailbox-ops" {
		t.Fatalf("binds = %v", bind)
	}
	var watch struct {
		WatchedTableIDs []string `json:"watchedTableIds"`
	}
	if err := json.Unmarshal([]byte(readFile(t, filepath.Join(dir, "automations/sales/on-orders.json"))), &watch); err != nil {
		t.Fatal(err)
	}
	if strings.Join(watch.WatchedTableIDs, ",") != "orders_clean,raw_orders" {
		t.Fatalf("watchedTableIds = %v", watch.WatchedTableIDs)
	}
	if mail := readFile(t, filepath.Join(dir, "automations/sales/on-mail.json")); !strings.Contains(mail, `"mailboxID": "ops"`) {
		t.Fatalf("on-mail.json:\n%s", mail)
	}
	assertOnlyReportedLiterals(t, dir, report)

	// Clean, and an apply sends nothing. `sync check` cannot vouch for a
	// mailbox (there is no read-one-mailbox route), so its answer is unknown
	// for that reference alone.
	status, err := runCLI(t, dir, "sync", "status", "--stack", "main", "--json")
	if err != nil || decodeJSON(t, status)["verdict"] != verdictClean {
		t.Fatalf("sync status: %v\n%s", err, status)
	}
	check, _ := runCLI(t, dir, "sync", "check", "--stack", "main", "--json")
	if reasons := regexpReasons.FindAllStringSubmatch(check, -1); len(reasons) != 1 || reasons[0][1] != "mailbox_unverifiable" {
		t.Fatalf("sync check found more than the unverifiable mailbox:\n%s", check)
	}
	f.resetRequests()
	if out, err := runCLI(t, dir, "sync", "apply", "--stack", "main", "--json"); err != nil {
		t.Fatalf("sync apply: %v\n%s", err, out)
	}
	if writes := f.writes(); len(writes) != 0 {
		t.Fatalf("the first apply sent %v", writes)
	}
}

var regexpReasons = regexp.MustCompile(`"reason": "([a-z_]+)"`)
