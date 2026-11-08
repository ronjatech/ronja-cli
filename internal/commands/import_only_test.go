package commands

import (
	"encoding/json"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"

	"github.com/ronjatech/ronja-cli/internal/api"
	"github.com/ronjatech/ronja-cli/internal/wfdir"
)

func runImportOnly(t *testing.T, f *fakeImportInstance, ids ...string) (string, importReport) {
	t.Helper()
	dir := filepath.Join(t.TempDir(), "repo")
	args := []string{"import", "feature", dir, "--json", "--stack", "main"}
	for _, id := range ids {
		args = append(args, "--only", id)
	}
	out, err := runCLI(t, t.TempDir(), args...)
	if err != nil {
		t.Fatalf("import --only: %v\n%s", err, out)
	}
	var report importReport
	if err := json.Unmarshal([]byte(out), &report); err != nil {
		t.Fatalf("decode the report: %v\n%s", err, out)
	}
	return dir, report
}

func importedIDs(r importReport) []string {
	var out []string
	for _, f := range r.Folders {
		for _, res := range f.Resources {
			out = append(out, res.ID)
		}
	}
	sort.Strings(out)
	return out
}

// --only a workflow carries what it needs inside Sales: its module, the
// derived table it reads (in the pipeline folder), and the automation that
// runs it. The app, the metric over the table and the agent automation that
// only references the app stay out. Every row outside Sales, or not a kind a
// folder holds, is a data source bound in the stack; the automation carried
// is reported as firing in both places.
func TestImportOnlyCarriesARowsClosureInsideItsFeature(t *testing.T) {
	f := importFixture(t)

	_, report := runImportOnly(t, f, "workflow-report")
	if report.FeatureID != importFeature {
		t.Fatalf("feature = %s, want the row's own %s", report.FeatureID, importFeature)
	}
	if got, want := strings.Join(importedIDs(report), " "), "module-helpers sjob-nightly table-orders-clean workflow-report"; got != want {
		t.Fatalf("imported %s, want %s", got, want)
	}
	sources := map[string]string{}
	for _, d := range report.DataSources {
		sources[d.BoundTo] = d.Access
	}
	for id, access := range map[string]string{"table-raw-orders": "read", "table-fx-rates": "read", "secret-crm": "side_effect", "table-ledger": "write"} {
		if sources[id] != access {
			t.Errorf("%s: data source access %q, want %q (sources %v)", id, sources[id], access, sources)
		}
	}
	if len(report.AlsoFires) != 1 || report.AlsoFires[0].ID != "sjob-nightly" {
		t.Fatalf("alsoFires = %+v, want the nightly automation", report.AlsoFires)
	}
}

// --only an app pulls in the derived table it reads; the workflow it does not
// name and the automations that do not run it stay out, and a workflow of
// another feature it may run is a run dependency.
func TestImportOnlyAnAppLeavesWhatItDoesNotNeed(t *testing.T) {
	f := importFixture(t)

	_, report := runImportOnly(t, f, "data_app-dash")
	if got, want := strings.Join(importedIDs(report), " "), "data_app-dash table-orders-clean"; got != want {
		t.Fatalf("imported %s, want %s", got, want)
	}
	run := false
	for _, d := range report.DataSources {
		if d.BoundTo == "workflow-close" && d.Access == "run" {
			run = true
		}
	}
	if !run {
		t.Fatalf("workflow-close is not a run dependency: %+v", report.DataSources)
	}
	if len(report.AlsoFires) != 0 {
		t.Fatalf("alsoFires = %+v, want none", report.AlsoFires)
	}
}

// Rows of two features cannot be one tree's import.
func TestImportOnlyRefusesRowsOfTwoFeatures(t *testing.T) {
	f := importFixture(t)
	dir := filepath.Join(t.TempDir(), "repo")

	_, err := runCLI(t, t.TempDir(), "import", "feature", dir, "--only", "workflow-report", "--only", "workflow-close")
	if err == nil || !strings.Contains(err.Error(), "--only names rows of 2 features") ||
		!strings.Contains(err.Error(), "workflow-close in "+financeFeature) {
		t.Fatalf("--only across features: %v", err)
	}
	for _, r := range f.requests {
		if strings.HasPrefix(r, "POST") {
			t.Fatalf("a refused import wrote: %s", r)
		}
	}
}

// The human report says which automations fire in both places, with what to
// do about it.
func TestImportOnlyWarnsThatACarriedAutomationFiresInBothPlaces(t *testing.T) {
	importFixture(t)
	dir := filepath.Join(t.TempDir(), "repo")

	out, err := runCLI(t, t.TempDir(), "import", "feature", dir, "--only", "workflow-report", "--stack", "main")
	if err != nil {
		t.Fatalf("import --only: %v\n%s", err, out)
	}
	for _, want := range []string{"Automations that will fire in both places (1)", "switch it off in the web app if it should stop", "nightly (sjob-nightly)"} {
		if !strings.Contains(out, want) {
			t.Errorf("report does not say %q:\n%s", want, out)
		}
	}
}

// --only an app carries the workflow that fills the dynamic table it reads,
// though the workflow writes it by id and owns nothing, and says why it was
// carried. The table itself is a data source, as in a whole-feature import.
func TestImportOnlyCarriesTheWorkflowThatWritesATableTheAppReads(t *testing.T) {
	f := importFixture(t)
	f.pipe.AddTable(&api.Table{ID: "table-chat", Name: "chat_rows", FeatureID: importFeature, Kind: "dynamic"})
	f.wf.AddWorkflow(&api.Workflow{ID: "workflow-refill", Lifecycle: api.LifecycleLive, FeatureID: importFeature,
		FeatureScope: "organization", Title: "Refill chat rows", RuntimeVersion: wfdir.RuntimeQuery},
		api.WorkflowFile{Path: "main.py", Content: "df = load()\ntools.replaceTable(df, \"table-chat\")\n"})
	f.wf.versions["workflow-refill"] = []api.Workflow{{ID: "wfv-2"}}
	f.app.AddApp(&api.DataApp{ID: "data_app-chat", Lifecycle: api.LifecycleLive, Name: "Chat board", FeatureID: importFeature,
		DataAppAccess: api.DataAppAccess{AllowedTableIDs: []string{"table-chat"}}},
		api.DataAppFile{Path: "App.tsx", Content: "export default function App() { return null }\n"})
	f.app.versions["data_app-chat"] = []api.DataApp{{ID: "dav-2"}}
	chat := api.ReferencePosition{TargetKind: "table", TargetID: "table-chat", Readable: true, TargetName: "chat_rows",
		TargetFeatureID: importFeature, TargetTableKind: "dynamic"}
	read, write := chat, chat
	read.Position, read.Direction, read.Field = api.RefPosAppAccess, api.RefDirRead, "allowedTableIDs[0]"
	write.Position, write.Direction, write.Arg, write.Path, write.Line, write.Call = api.RefPosOutputLiteral, api.RefDirWrite, "table-chat", "main.py", 2, "replaceTable"
	f.inv.add(importFeature, api.ReferenceResource{Kind: "table", ID: "table-chat", Name: "chat_rows", TableKind: "dynamic"})
	f.inv.add(importFeature, api.ReferenceResource{Kind: "workflow", ID: "workflow-refill", Name: "Refill chat rows", References: []api.ReferencePosition{write}})
	f.inv.add(importFeature, api.ReferenceResource{Kind: "dataapp", ID: "data_app-chat", Name: "Chat board", References: []api.ReferencePosition{read}})

	_, report := runImportOnly(t, f, "data_app-chat")
	if got, want := strings.Join(importedIDs(report), " "), "data_app-chat workflow-refill"; got != want {
		t.Fatalf("imported %s, want %s", got, want)
	}
	if len(report.WritersCarried) != 1 || report.WritersCarried[0].ID != "workflow-refill" || strings.Join(report.WritersCarried[0].Writes, ",") != "table-chat" {
		t.Fatalf("writersCarried = %+v, want workflow-refill for table-chat", report.WritersCarried)
	}
}

// --only a row another folder manages is refused before anything is written,
// naming the row and the folder that holds it, and how to work with it there.
func TestImportOnlyRefusesARowAnotherFolderManages(t *testing.T) {
	f := importFixture(t)
	f.managed.stage(importFeature, addressTestFolderID, "", "dataapp", "data_app-dash", "github.com/acme/other//apps/dash")
	dir := filepath.Join(t.TempDir(), "repo")

	_, err := runCLI(t, t.TempDir(), "import", "feature", dir, "--only", "data_app-dash", "--stack", "main")
	if err == nil {
		t.Fatal("the import went through")
	}
	for _, want := range []string{"dataapp Sales dashboard (data_app-dash)", "folder " + addressTestFolderID,
		"`ronja sync stack add <stack> --feature <id>` there", "`ronja sync unmanage --stack prod`", "every row that folder claims"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("the refusal does not say %q:\n%v", want, err)
		}
	}
	for _, r := range f.requests {
		if strings.HasPrefix(r, "POST") {
			t.Fatalf("a refused import wrote: %s", r)
		}
	}
	if _, statErr := os.Stat(dir); !os.IsNotExist(statErr) {
		t.Fatalf("a refused import wrote the tree: %v", statErr)
	}
}

// --only a derived table, run by someone who is not an Admin, is refused with
// the reason rather than imported without it.
func TestImportOnlyRefusesATableANonAdminCannotManage(t *testing.T) {
	f := importFixture(t)
	f.wf.privilegeLevel = 50
	f.managed.admin = false
	dir := filepath.Join(t.TempDir(), "repo")

	_, err := runCLI(t, t.TempDir(), "import", "feature", dir, "--only", "table-orders-clean", "--stack", "main")
	if err == nil {
		t.Fatal("the import went through")
	}
	for _, want := range []string{"table orders_clean (table-orders-clean)", "Admin"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("the refusal does not say %q:\n%v", want, err)
		}
	}
	if _, statErr := os.Stat(dir); !os.IsNotExist(statErr) {
		t.Fatalf("a refused import wrote the tree: %v", statErr)
	}
}

// copiedFX makes the fixture's fx_rates a snapshot copy of a table named
// fx_master in another feature.
func copiedFX(f *fakeImportInstance) {
	f.pipe.AddTable(&api.Table{ID: "table-fx-master", Name: "fx_master", FeatureID: mainSpaceFeature, Kind: "integration"})
	f.pipe.tables["table-fx-rates"].CopiedFromModelID = "table-fx-master"
}

// A table data source that is a snapshot copy is marked with its source, by
// name in the human report and by id in --json.
func TestImportOnlyMarksATableThatIsACopy(t *testing.T) {
	f := importFixture(t)
	copiedFX(f)

	_, report := runImportOnly(t, f, "workflow-report")
	var copied []string
	for _, d := range report.DataSources {
		if d.CopiedFrom != "" {
			copied = append(copied, d.BoundTo+"<"+d.CopiedFrom)
		}
	}
	if got := strings.Join(copied, " "); got != "table-fx-rates<table-fx-master" {
		t.Fatalf("copiedFrom = %q, want fx_rates marked as a copy of table-fx-master: %+v", got, report.DataSources)
	}

	// A fresh instance: the import above claimed the rows.
	copiedFX(importFixture(t))
	out, err := runCLI(t, t.TempDir(), "import", "feature", filepath.Join(t.TempDir(), "repo"), "--only", "workflow-report", "--stack", "main")
	if err != nil {
		t.Fatalf("import --only: %v\n%s", err, out)
	}
	if !strings.Contains(out, "fx_rates → table-fx-rates, a copy of fx_master") {
		t.Fatalf("the human report does not mark the copy:\n%s", out)
	}
}

// A whole-feature import marks a copy too, and a copy whose source cannot be
// read is marked by the source's id.
func TestImportMarksATableThatIsACopy(t *testing.T) {
	f := importFixture(t)
	f.pipe.tables["table-fx-rates"].CopiedFromModelID = "table-gone"

	_, report := runImport(t, f)
	for _, d := range report.DataSources {
		if d.BoundTo == "table-fx-rates" && d.CopiedFrom == "table-gone" {
			return
		}
	}
	t.Fatalf("a whole-feature import does not mark fx_rates as a copy: %+v", report.DataSources)
}
