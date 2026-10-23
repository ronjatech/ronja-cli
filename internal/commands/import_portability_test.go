package commands

import (
	"encoding/json"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/ronjatech/ronja-cli/internal/api"
	"github.com/ronjatech/ronja-cli/internal/markers"
	"github.com/ronjatech/ronja-cli/internal/wfdir"
)

// One test per row of the portability pass's classification (a mutation test
// for each row), each driven ONLY by inventory positions,
// and each asserting the form the reference is written in: the dependency
// declared, what the importing stack binds, and what is reported.

// portabilityFixture is a pipeline folder (two derived tables, a metric), a
// workflow, an app, a module and an automation folder, planned but not written.
type portabilityFixture struct {
	imp                          *featureImport
	pipeline, wf, app, mod, auto *importFolder
	byID                         map[string]*importRow
}

func newPortabilityFixture(t *testing.T) portabilityFixture {
	t.Helper()
	base := t.TempDir()
	imp := &featureImport{feature: &api.Feature{ID: "collection-sales", Name: "Sales"}, stack: "main"}
	fx := portabilityFixture{imp: imp, byID: map[string]*importRow{},
		pipeline: newImportFolder(wfdir.PipelineKind, "pipelines/sales", base),
		wf:       newImportFolder(wfdir.WorkflowKind, "workflows/report", base),
		app:      newImportFolder(wfdir.DataAppKind, "apps/dash", base),
		mod:      newImportFolder(wfdir.ModuleKind, "modules/helpers", base),
		auto:     newImportFolder(wfdir.AutomationKind, "automations/sales", base),
	}
	add := func(f *importFolder, kind, id, name, address string) {
		row := &importRow{res: api.ReferenceResource{Kind: kind, ID: id, Name: name}, folder: f, address: address}
		f.rows = append(f.rows, row)
		fx.byID[id] = row
	}
	add(fx.pipeline, "table", "table-orders", "orders", "orders.sql")
	add(fx.pipeline, "table", "table-daily-sum", "daily_sum", "daily_sum.sql")
	add(fx.pipeline, "metric", "table-aov", "aov", "metrics/aov.json")
	fx.pipeline.taken["orders"], fx.pipeline.taken["daily_sum"], fx.pipeline.taken["aov"] = true, true, true
	add(fx.wf, "workflow", "workflow-report", "Report", "")
	add(fx.app, "dataapp", "data_app-dash", "Dash", "")
	add(fx.mod, "module", "module-helpers", "helpers", "")
	add(fx.auto, "automation", "sjob-nightly", "nightly", "nightly.json")
	return fx
}

// ref builds one readable reference position.
func ref(pos, dir, kind, id, name string, mod ...func(*api.ReferencePosition)) api.ReferencePosition {
	p := api.ReferencePosition{Position: pos, Direction: dir, TargetKind: kind, TargetID: id, Readable: true, TargetName: name}
	for _, m := range mod {
		m(&p)
	}
	return p
}

func (fx portabilityFixture) refs(f *importFolder, refs ...api.ReferencePosition) {
	f.rows[0].res.References = append(f.rows[0].res.References, refs...)
}

func (fx portabilityFixture) run(f *importFolder) { fx.imp.portability(f, fx.byID) }

// Row 1 — a read of a table the SAME pipeline folder builds becomes its sibling
// stem: no dependency at all, and the rewritten SQL (and a metric's source)
// name the file.
func TestPortabilitySiblingStem(t *testing.T) {
	fx := newPortabilityFixture(t)
	fx.pipeline.rows[1].res.References = []api.ReferencePosition{
		ref(api.RefPosRefMarker, api.RefDirRead, "table", "table-orders", "orders")}
	fx.pipeline.rows[2].res.References = []api.ReferencePosition{
		ref(api.RefPosMetricSource, api.RefDirRead, "table", "table-orders", "orders")}
	fx.run(fx.pipeline)
	if len(fx.pipeline.deps) != 0 {
		t.Fatalf("a sibling read declared %+v, want nothing", fx.pipeline.deps)
	}
	fx.pipeline.pipeline = &pipelineCloneSet{
		sources: []cloneSource{
			{Path: "orders.sql", TableID: "table-orders", Code: "SELECT 1"},
			{Path: "daily_sum.sql", TableID: "table-daily-sum", Code: "SELECT * FROM {{ ref('table-orders') }}", LiveCode: "SELECT * FROM {{ ref('table-orders') }}"},
		},
		metrics: []metricCloneSource{{Path: "metrics/aov.json", MetricID: "table-aov",
			Body: "{\n  \"recipe\": {\"source\": \"table-orders\", \"value\": \"x\"}\n}\n"}},
	}
	if err := fx.imp.rewrite(fx.pipeline); err != nil {
		t.Fatal(err)
	}
	if got := fx.pipeline.pipeline.sources[1]; got.Code != "SELECT * FROM {{ ref('orders') }}" || got.LiveCode != got.Code {
		t.Fatalf("sibling ref written as %+v, want the stem", got)
	}
	if body := fx.pipeline.pipeline.metrics[0].Body; !strings.Contains(body, `"source": "orders"`) {
		t.Fatalf("the metric source is %s, want the stem", body)
	}
}

// Row 2 — a read, or a run, of anything imported into ANOTHER folder becomes an
// address dependency: a table, a metric, a workflow, an app and a module.
func TestPortabilityAddressDependency(t *testing.T) {
	fx := newPortabilityFixture(t)
	fx.refs(fx.wf,
		ref(api.RefPosRefMarker, api.RefDirRead, "table", "table-orders", "orders"),
		ref(api.RefPosModuleMarker, api.RefDirRead, "module", "module-helpers", "helpers"))
	fx.refs(fx.app,
		ref(api.RefPosAppAccess, api.RefDirRead, "metric", "table-aov", "aov"),
		ref(api.RefPosAppAccess, api.RefDirRun, "workflow", "workflow-report", "Report"))
	fx.refs(fx.auto,
		ref(api.RefPosAutomationAction, api.RefDirRun, "workflow", "workflow-report", "Report"),
		ref(api.RefPosAutomationRef, api.RefDirRead, "dataapp", "data_app-dash", "Dash"))
	for _, f := range []*importFolder{fx.wf, fx.app, fx.auto} {
		fx.run(f)
	}
	assertDeps(t, "workflow", fx.wf.deps, map[string]wfdir.Dependency{
		"orders":  {Kind: markers.KindTable, From: "../../pipelines/sales", Address: "orders.sql"},
		"helpers": {Kind: markers.KindModule, From: "../../modules/helpers"},
	})
	assertDeps(t, "app", fx.app.deps, map[string]wfdir.Dependency{
		"aov":    {Kind: markers.KindTable, From: "../../pipelines/sales", Address: "metrics/aov.json"},
		"report": {Kind: markers.KindWorkflow, From: "../../workflows/report"},
	})
	assertDeps(t, "automation", fx.auto.deps, map[string]wfdir.Dependency{
		"report": {Kind: markers.KindWorkflow, From: "../../workflows/report"},
		"dash":   {Kind: markers.KindDataApp, From: "../../apps/dash"},
	})
	for _, f := range []*importFolder{fx.wf, fx.app, fx.auto} {
		if len(f.bind) != 0 {
			t.Fatalf("%s binds %v — an address needs no bind", f.Rel, f.bind)
		}
	}
	// The codec the folder is written through still resolves the address.
	if id, ok := fx.wf.codec().resolve(markers.KindTable, "orders"); !ok || id != "table-orders" {
		t.Fatalf("the import codec resolves orders to %q", id)
	}
}

// Row 3 — `{{ write('table-X') }}` where X is this workflow's OWN output is
// written as its logical name, declared an output and bound here.
func TestPortabilityOwnOutputNameForm(t *testing.T) {
	fx := newPortabilityFixture(t)
	fx.refs(fx.wf, ref(api.RefPosWriteMarker, api.RefDirWrite, "table", "table-daily", "orders_daily",
		func(p *api.ReferencePosition) { p.OwnOutput, p.LogicalName = true, "orders_daily" }))
	fx.run(fx.wf)
	assertDeps(t, "workflow", fx.wf.deps, map[string]wfdir.Dependency{"orders_daily": {Kind: markers.KindTable, Output: true}})
	if fx.wf.bind["orders_daily"] != "table-daily" {
		t.Fatalf("bind = %v", fx.wf.bind)
	}
	if got := fx.wf.codec().toDisk("{{ write('table-daily') }}"); got != "{{ write('orders_daily') }}" {
		t.Fatalf("the own output is written %q, want the name form", got)
	}
}

// Row 4 — every other write position becomes a WRITE-CAPABLE data source bound
// here: a write marker naming a table the workflow does not own, a codex it
// may upsert into, and a write whose target this credential cannot read (which
// must be treated as outside the feature).
func TestPortabilityWriteCapableDataSource(t *testing.T) {
	fx := newPortabilityFixture(t)
	fx.refs(fx.wf,
		ref(api.RefPosWriteMarker, api.RefDirWrite, "table", "table-events", "events"),
		ref(api.RefPosCodexMarker, api.RefDirReadWrite, "codex", "cdx-docs", "docs"),
		api.ReferencePosition{Position: api.RefPosWriteMarker, Direction: api.RefDirWrite, TargetKind: "table", TargetID: "table-hidden9"},
		// An imported table WRITTEN is still a write target, never an address.
		ref(api.RefPosWriteMarker, api.RefDirWrite, "table", "table-orders", "orders"))
	fx.run(fx.wf)
	assertDeps(t, "workflow", fx.wf.deps, map[string]wfdir.Dependency{
		"events":        {Kind: markers.KindTable, Write: true},
		"docs":          {Kind: markers.KindCodex, Write: true},
		"table_hidden9": {Kind: markers.KindTable, Write: true},
		"orders":        {Kind: markers.KindTable, Write: true},
	})
	if fx.wf.bind["table_hidden9"] != "table-hidden9" || fx.wf.bind["events"] != "table-events" {
		t.Fatalf("bind = %v", fx.wf.bind)
	}
	for _, d := range fx.imp.sources {
		if d.Access != "write" || d.Stack != "main" {
			t.Fatalf("data source %+v is not a write bound in main", d)
		}
	}
}

// Row 5 — a read of anything NOT imported becomes a read data source bound
// here: a connector table in the feature, another feature's table, a note, and
// a target this credential cannot read (bound to the literal id).
func TestPortabilityReadDataSource(t *testing.T) {
	fx := newPortabilityFixture(t)
	fx.refs(fx.wf,
		ref(api.RefPosRefMarker, api.RefDirRead, "table", "table-raw", "Raw Orders"),
		ref(api.RefPosRefMarker, api.RefDirRead, "table", "table-fx", "fx rates"),
		api.ReferencePosition{Position: api.RefPosRefMarker, Direction: api.RefDirRead, TargetKind: "table", TargetID: "table-secretive"})
	fx.refs(fx.auto, ref(api.RefPosAutomationRef, api.RefDirRead, "note", "note-policy", "Policy"))
	fx.run(fx.wf)
	fx.run(fx.auto)
	assertDeps(t, "workflow", fx.wf.deps, map[string]wfdir.Dependency{
		"raw_orders":      {Kind: markers.KindTable},
		"fx_rates":        {Kind: markers.KindTable},
		"table_secretive": {Kind: markers.KindTable},
	})
	if fx.wf.bind["table_secretive"] != "table-secretive" || fx.wf.bind["raw_orders"] != "table-raw" {
		t.Fatalf("bind = %v", fx.wf.bind)
	}
	assertDeps(t, "automation", fx.auto.deps, map[string]wfdir.Dependency{"policy": {Kind: markers.KindNote}})
}

// Row 6 — a secret and a mailbox are SIDE-EFFECT data sources; an agent and a
// workflow NOT imported are RUN data sources, declared "run": true
// — all bound here only.
func TestPortabilitySideEffectDataSource(t *testing.T) {
	fx := newPortabilityFixture(t)
	fx.refs(fx.wf,
		ref(api.RefPosSecretMarker, api.RefDirSideEffect, "secret", "secret-crm", "crm"),
		ref(api.RefPosMailboxMarker, api.RefDirSideEffect, "mailbox", "mailbox-ops", "ops@acme"),
		ref(api.RefPosAgentMarker, api.RefDirRun, "agent", "agent-triage", "triage"),
		ref(api.RefPosWorkflowMarker, api.RefDirRun, "workflow", "workflow-elsewhere", "Elsewhere"))
	fx.run(fx.wf)
	want := map[string]string{"crm": "side_effect", "ops_acme": "side_effect", "triage": "run", "elsewhere": "run"}
	if len(fx.imp.sources) != len(want) {
		t.Fatalf("sources = %+v", fx.imp.sources)
	}
	for _, d := range fx.imp.sources {
		if want[d.Alias] != d.Access || fx.wf.bind[d.Alias] != d.BoundTo {
			t.Fatalf("data source %+v, want access %q bound in main", d, want[d.Alias])
		}
	}
	if fx.wf.deps["elsewhere"].From != "" {
		t.Fatal("a workflow outside the feature was declared by address")
	}
	for alias, run := range map[string]bool{"crm": false, "ops_acme": false, "triage": true, "elsewhere": true} {
		if fx.wf.deps[alias].Run != run {
			t.Fatalf("%s: run = %v, want %v", alias, fx.wf.deps[alias].Run, run)
		}
	}
}

// Row 7 — an automation's references of kind dataapp, mcpserver and feature:
// the app is an address (row 2 covers it), the MCP server a data source, the
// automation's OWN feature "@feature" — no dependency, the stack resolves it —
// and ANOTHER feature a write source: the inventory reports a reference-set
// feature read_write, because the run is given every table of it.
func TestPortabilityAutomationReferenceKinds(t *testing.T) {
	fx := newPortabilityFixture(t)
	fx.refs(fx.auto,
		ref(api.RefPosAutomationRef, api.RefDirSideEffect, "mcpserver", "mcpsrv-crm", "CRM"),
		ref(api.RefPosAutomationRef, api.RefDirReadWrite, "feature", "collection-sales", "Sales"),
		ref(api.RefPosAutomationRef, api.RefDirReadWrite, "feature", "collection-finance", "Finance"))
	fx.run(fx.auto)
	assertDeps(t, "automation", fx.auto.deps, map[string]wfdir.Dependency{
		"crm":     {Kind: markers.KindMCPServer},
		"finance": {Kind: markers.KindFeature, Write: true},
	})
	for _, d := range fx.imp.sources {
		if d.Alias == "finance" && (d.Access != "write" || d.BoundTo != "collection-finance") {
			t.Fatalf("the Finance feature is %+v, want a write source bound to it", d)
		}
	}
	row := &api.Automation{ID: "sjob-nightly", Name: "nightly", TriggerKind: api.TriggerKindCron, CronExpr: "0 1 * * *", Enabled: true,
		Action: &api.AutomationAction{Kind: api.ActionKindAgent, Config: api.AutomationActionConfig{References: []api.AutomationReference{
			{Kind: "mcpserver", ResourceID: "mcpsrv-crm"}, {Kind: "feature", ResourceID: "collection-sales"}, {Kind: "feature", ResourceID: "collection-finance"}}}}}
	body, err := fx.imp.automationFileFor(fx.auto, "nightly.json", row)
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{`"resourceID": "crm"`, `"resourceID": "@feature"`, `"resourceID": "finance"`} {
		if !strings.Contains(body, want) {
			t.Fatalf("the file does not hold %s:\n%s", want, body)
		}
	}
}

// Row 8 — an output literal and an app TSX literal are LEFT IN PLACE and named
// in the report; the output literal's table is declared write-capable anyway.
func TestPortabilityLiteralsAreReported(t *testing.T) {
	fx := newPortabilityFixture(t)
	fx.refs(fx.wf, ref(api.RefPosOutputLiteral, api.RefDirWrite, "table", "table-ledger", "ledger",
		func(p *api.ReferencePosition) { p.Path, p.Line, p.Call = "main.py", 7, "replaceTable" }))
	fx.refs(fx.app, ref(api.RefPosAppLiteral, api.RefDirSideEffect, "secret", "secret-api", "api",
		func(p *api.ReferencePosition) { p.Path, p.Line, p.Call = "App.tsx", 4, "queryExternal" }))
	fx.run(fx.wf)
	fx.run(fx.app)
	if len(fx.imp.literals) != 2 {
		t.Fatalf("literals = %+v", fx.imp.literals)
	}
	if l := fx.imp.literals[0]; l.Path != "main.py" || l.Line != 7 || l.Alias != "ledger" {
		t.Fatalf("output literal reported as %+v", l)
	}
	assertDeps(t, "workflow", fx.wf.deps, map[string]wfdir.Dependency{"ledger": {Kind: markers.KindTable, Write: true}})
	if l := fx.imp.literals[1]; l.Path != "App.tsx" || l.Line != 4 || l.TargetID != "secret-api" {
		t.Fatalf("app literal reported as %+v", l)
	}
}

func assertDeps(t *testing.T, what string, got, want map[string]wfdir.Dependency) {
	t.Helper()
	if len(got) != len(want) {
		t.Fatalf("%s declares %d dependencies, want %d: %+v", what, len(got), len(want), got)
	}
	for alias, w := range want {
		g, ok := got[alias]
		if !ok || g.Kind != w.Kind || g.From != w.From || g.Address != w.Address || g.Write != w.Write || g.Output != w.Output || g.Run != w.Run {
			t.Fatalf("%s: dependency %q = %+v (declared %v), want %+v; all: %+v", what, alias, g, ok, w, got)
		}
	}
}

// TestPortabilityOwnOutputKeepsItsName is M1: a foreign table that happens to
// be called like the workflow's own output, and whose id sorts first, must not
// take the output's name — the output would degrade into a write-capable data
// source bound per stack, written by an alias rather than by the name the
// runtime creates it under.
func TestPortabilityOwnOutputKeepsItsName(t *testing.T) {
	fx := newPortabilityFixture(t)
	fx.refs(fx.wf,
		ref(api.RefPosRefMarker, api.RefDirRead, "table", "table-aaa", "orders_daily"),
		ref(api.RefPosWriteMarker, api.RefDirWrite, "table", "table-daily", "orders_daily",
			func(p *api.ReferencePosition) { p.OwnOutput, p.LogicalName = true, "orders_daily" }))
	fx.run(fx.wf)
	assertDeps(t, "workflow", fx.wf.deps, map[string]wfdir.Dependency{
		"orders_daily":   {Kind: markers.KindTable, Output: true},
		"orders_daily_2": {Kind: markers.KindTable},
	})
	if fx.wf.bind["orders_daily"] != "table-daily" || fx.wf.bind["orders_daily_2"] != "table-aaa" {
		t.Fatalf("bind = %v", fx.wf.bind)
	}
}

// TestPortabilityStructuredFieldsAreReportedNotCarried is M2: a binding held
// only in a structured field — an explicit secret, a binding column no line of
// the code explains, an input the SQL no longer reads, a folder write scope —
// is in no file the folder writes, so it is reported, with what changes where
// the tree creates the resource anew.
func TestPortabilityStructuredFieldsAreReportedNotCarried(t *testing.T) {
	fx := newPortabilityFixture(t)
	structured := func(kind, id, field string) api.ReferencePosition {
		return ref(api.RefPosStructuredField, api.RefDirSideEffect, kind, id, "", func(p *api.ReferencePosition) { p.Field = field })
	}
	fx.refs(fx.wf,
		structured("secret", "secret-extra", "explicitSecretIDs[0]"),
		structured("table", "table-old", "inputTableIDs[2]"),
		api.ReferencePosition{Position: api.RefPosStructuredField, Direction: api.RefDirWrite, TargetKind: "folder", TargetPath: "/reports", Arg: "/reports", Field: "fileScopes[0]"})
	fx.pipeline.rows[1].res.References = []api.ReferencePosition{structured("table", "table-gone", "inputModels[1]")}
	fx.run(fx.wf)
	fx.run(fx.pipeline)
	if len(fx.wf.deps) != 0 || len(fx.pipeline.deps) != 0 {
		t.Fatalf("a structured field declared a dependency: %+v %+v", fx.wf.deps, fx.pipeline.deps)
	}
	want := map[string]string{
		"explicitSecretIDs[0]": "runs without that secret",
		"inputTableIDs[2]":     "it is not bound",
		"fileScopes[0]":        "may not write there",
		"inputModels[1]":       "does not list it",
	}
	if len(fx.imp.notCarried) != len(want) {
		t.Fatalf("notCarried = %+v", fx.imp.notCarried)
	}
	for _, c := range fx.imp.notCarried {
		if !strings.Contains(c.Effect, want[c.Field]) || c.Folder == "" || c.ResourceID == "" {
			t.Fatalf("not carried %+v, want an effect naming %q", c, want[c.Field])
		}
	}
}

// TestRewriteMetricSourceKeepsTheClonesEncoding is M4: an imported metric file
// is the file `pipeline clone` writes with the one source value changed — the
// recipe's keys in the order the server gave them, the clone's indentation.
func TestRewriteMetricSourceKeepsTheClonesEncoding(t *testing.T) {
	recipe := func(source string) json.RawMessage {
		return json.RawMessage(`{"source":"` + source + `","measure":{"op":"sum","column":"amount"},"dimensions":["region"],"grain":"day"}`)
	}
	tags := []wfdir.AppliedTag{{ID: "tag-1", Name: "finance"}}
	cloned, err := renderMetricFile(&api.Table{MetricRecipe: recipe("table-orders"), Description: "Average order", DescriptionSource: api.DescriptionSourceUser}, tags)
	if err != nil {
		t.Fatal(err)
	}
	want, err := renderMetricFile(&api.Table{MetricRecipe: recipe("orders"), Description: "Average order", DescriptionSource: api.DescriptionSourceUser}, tags)
	if err != nil {
		t.Fatal(err)
	}
	got, err := rewriteMetricSource(cloned, func(id string) string {
		if id == "table-orders" {
			return "orders"
		}
		return id
	})
	if err != nil {
		t.Fatal(err)
	}
	if got != want {
		t.Fatalf("the rewritten file differs from the clone's encoding beyond its source:\n%s\nwant:\n%s", got, want)
	}
}

// TestPortabilityFolderWritesAreReported: a folder WRITE names a path in the
// organization's file system — no row, so nothing in the code is rewritten — and
// is left as written and named in the report, with its file and line, AND its
// path is declared as a folder write source bound in the importing stack, which
// is what the write gate admits it through; a folder READ is left alone
// silently and declares nothing.
func TestPortabilityFolderWritesAreReported(t *testing.T) {
	fx := newPortabilityFixture(t)
	fx.refs(fx.wf,
		api.ReferencePosition{Position: api.RefPosFileMarker, Direction: api.RefDirWrite, TargetKind: api.RefKindFolder,
			TargetPath: "/reports", Arg: "/reports", Path: "main.py", Line: 3, Readable: true},
		api.ReferencePosition{Position: api.RefPosFolderLiteral, Direction: api.RefDirWrite, TargetKind: api.RefKindFolder,
			TargetPath: "/reports", Arg: "/reports/", Path: "main.py", Line: 5, Call: "saveFile", Readable: true},
		api.ReferencePosition{Position: api.RefPosFileMarker, Direction: api.RefDirRead, TargetKind: api.RefKindFolder,
			TargetPath: "/inbox", Arg: "/inbox", Path: "main.py", Line: 4, Readable: true})
	fx.run(fx.wf)
	assertDeps(t, "workflow", fx.wf.deps, map[string]wfdir.Dependency{
		"folder_reports": {Kind: markers.KindFolder, Write: true},
	})
	if fx.wf.bind["folder_reports"] != "/reports" {
		t.Fatalf("bind = %v, want the folder bound to its path", fx.wf.bind)
	}
	if len(fx.imp.literals) != 2 {
		t.Fatalf("literals = %+v, want the two folder writes", fx.imp.literals)
	}
	if len(fx.imp.sources) != 1 || fx.imp.sources[0].Kind != markers.KindFolder || fx.imp.sources[0].Access != "write" || fx.imp.sources[0].BoundTo != "/reports" {
		t.Fatalf("sources = %+v, want the one folder write source", fx.imp.sources)
	}
	if l := fx.imp.literals[0]; l.Path != "main.py" || l.Line != 3 || l.Arg != "/reports" || l.TargetKind != api.RefKindFolder || !strings.Contains(l.Why, "folder write") {
		t.Fatalf("folder write reported as %+v", l)
	}
}

// TestPortabilityReadWriteReferenceSetTable: an automation's reference set may
// give an agent read_write on a table (referenceSetDirection). That is a write
// position, so even a table the pipeline imports is a WRITE-CAPABLE data source
// bound in this stack, never an address another stack would resolve to its own
// copy only by accident.
func TestPortabilityReadWriteReferenceSetTable(t *testing.T) {
	fx := newPortabilityFixture(t)
	fx.refs(fx.auto,
		ref(api.RefPosAutomationRef, api.RefDirReadWrite, "table", "table-orders", "orders"),
		ref(api.RefPosAutomationRef, api.RefDirReadWrite, "table", "table-ext", "ext"))
	fx.run(fx.auto)
	assertDeps(t, "automation", fx.auto.deps, map[string]wfdir.Dependency{
		"orders": {Kind: markers.KindTable, Write: true},
		"ext":    {Kind: markers.KindTable, Write: true},
	})
	if fx.auto.bind["orders"] != "table-orders" || fx.auto.bind["ext"] != "table-ext" {
		t.Fatalf("bind = %v", fx.auto.bind)
	}
	for _, d := range fx.imp.sources {
		if d.Access != "write" {
			t.Fatalf("data source %+v, want write", d)
		}
	}
}

// A read of a table a WORKFLOW folder of the tree writes — its own output, or a
// write source it binds — is named by address in that folder: "from" the
// workflow folder, "address" the dependency it writes the table through. A
// per-stack bind would have every later stack read this one's table.
func TestPortabilityReadOfATableAWorkflowWrites(t *testing.T) {
	fx := newPortabilityFixture(t)
	fx.refs(fx.wf,
		ref(api.RefPosWriteMarker, api.RefDirWrite, "table", "table-weekly", "weekly_revenue", func(p *api.ReferencePosition) {
			p.OwnOutput, p.LogicalName, p.Arg = true, "weekly_revenue", "table-weekly"
		}),
		ref(api.RefPosWriteMarker, api.RefDirWrite, "table", "table-ledger", "ledger"))
	fx.refs(fx.app,
		ref(api.RefPosAppAccess, api.RefDirRead, "table", "table-weekly", "weekly_revenue"),
		ref(api.RefPosAppAccess, api.RefDirRead, "table", "table-ledger", "ledger"))
	// The reader first: the import plans the workflow folders before it.
	fx.imp.planPortability([]*importFolder{fx.app, fx.wf}, fx.byID)
	assertDeps(t, "app", fx.app.deps, map[string]wfdir.Dependency{
		"weekly_revenue": {Kind: markers.KindTable, From: "../../workflows/report", Address: "weekly_revenue"},
		"ledger":         {Kind: markers.KindTable, From: "../../workflows/report", Address: "ledger"},
	})
	if len(fx.app.bind) != 0 {
		t.Fatalf("the app binds %v — an address needs no bind", fx.app.bind)
	}
	var addresses []string
	for _, a := range fx.imp.addresses {
		if a.Folder == fx.app.Rel {
			addresses = append(addresses, a.Alias+"@"+a.Address)
		}
	}
	if strings.Join(addresses, ",") != "weekly_revenue@weekly_revenue,ledger@ledger" && strings.Join(addresses, ",") != "ledger@ledger,weekly_revenue@weekly_revenue" {
		t.Fatalf("addresses reported = %v", addresses)
	}
}

// The import report's Addresses line names what an address resolves to as a
// phrase: the workflow in its folder, or the file in a folder.
func TestDescribeAddressTarget(t *testing.T) {
	for _, c := range []struct{ kind, from, address, want string }{
		{markers.KindWorkflow, "../../workflows/compute-weekly-revenue", "", "the workflow in ../../workflows/compute-weekly-revenue"},
		{markers.KindDataApp, "../../apps/dash", "", "the data app in ../../apps/dash"},
		{markers.KindTable, "../../pipelines/sales", "orders.sql", `"orders.sql" in ../../pipelines/sales`},
	} {
		if got := describeAddressTarget(c.kind, c.from, c.address); got != c.want {
			t.Errorf("describeAddressTarget(%s, %s, %q) = %q, want %q", c.kind, c.from, c.address, got, c.want)
		}
	}
}

// A WORKFLOW folder reading a table another workflow folder writes names it by
// address too, whichever of the two is planned first — and so do two
// workflows each reading what the other writes. The reader planned before its
// writer found nothing recorded and bound this stack's table, which every
// later stack would then read.
func TestPortabilityAWorkflowReadsAnotherWorkflowsTableInAnyOrder(t *testing.T) {
	fx := newPortabilityFixture(t)
	reader := newImportFolder(wfdir.WorkflowKind, "workflows/a-digest", filepath.Dir(filepath.Dir(fx.wf.Root)))
	reader.rows = []*importRow{{res: api.ReferenceResource{Kind: "workflow", ID: "workflow-digest", Name: "Digest"}, folder: reader}}
	fx.byID["workflow-digest"] = reader.rows[0]
	// The writer (workflows/report) writes weekly_revenue and reads the
	// digest's summary; the reader, sorted first, does the opposite.
	fx.refs(fx.wf,
		ref(api.RefPosWriteMarker, api.RefDirWrite, "table", "table-weekly", "weekly_revenue", func(p *api.ReferencePosition) {
			p.OwnOutput, p.LogicalName, p.Arg = true, "weekly_revenue", "table-weekly"
		}),
		ref(api.RefPosRefMarker, api.RefDirRead, "table", "table-summary", "summary"))
	fx.refs(reader,
		ref(api.RefPosWriteMarker, api.RefDirWrite, "table", "table-summary", "summary", func(p *api.ReferencePosition) {
			p.OwnOutput, p.LogicalName, p.Arg = true, "summary", "table-summary"
		}),
		ref(api.RefPosRefMarker, api.RefDirRead, "table", "table-weekly", "weekly_revenue"))

	fx.imp.planPortability([]*importFolder{reader, fx.wf}, fx.byID)

	assertDeps(t, "reader", reader.deps, map[string]wfdir.Dependency{
		"summary":        {Kind: markers.KindTable, Output: true},
		"weekly_revenue": {Kind: markers.KindTable, From: "../report", Address: "weekly_revenue"},
	})
	assertDeps(t, "writer", fx.wf.deps, map[string]wfdir.Dependency{
		"weekly_revenue": {Kind: markers.KindTable, Output: true},
		"summary":        {Kind: markers.KindTable, From: "../a-digest", Address: "summary"},
	})
	if _, bound := reader.bind["weekly_revenue"]; bound {
		t.Fatalf("the reader binds this stack's weekly_revenue: %v", reader.bind)
	}
	var addresses []string
	for _, a := range fx.imp.addresses {
		addresses = append(addresses, a.Folder+":"+a.Alias)
	}
	slices.Sort(addresses)
	if want := []string{"workflows/a-digest:weekly_revenue", "workflows/report:summary"}; !slices.Equal(addresses, want) {
		t.Fatalf("addresses reported = %v, want %v", addresses, want)
	}
}
