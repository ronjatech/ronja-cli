package commands

import (
	"encoding/json"
	"path/filepath"
	"strings"
	"testing"

	"github.com/ronjatech/ronja-cli/internal/api"
	"github.com/ronjatech/ronja-cli/internal/dbdir"
	"github.com/ronjatech/ronja-cli/internal/wfdir"
)

// setupFixture is the import fixture's instance plus a live app in its Sales
// feature that runs nothing outside it, and an empty directory to work in.
func setupFixture(t *testing.T) (*fakeImportInstance, string) {
	t.Helper()
	f := importFixture(t)
	f.app.AddApp(&api.DataApp{ID: "data_app-kpi", Name: "KPIs", FeatureID: importFeature,
		DataAppAccess: api.DataAppAccess{AllowedTableIDs: []string{"table-orders-clean"}}},
		api.DataAppFile{Path: "App.tsx", Content: "export default function App() { return null }\n"})
	f.app.versions["data_app-kpi"] = []api.DataApp{{ID: "dav-kpi-1"}}
	return f, t.TempDir()
}

// A folder `app init` wrote, which names its feature and no stack, proposes
// in one command: propose adds stack "main" for that feature, adopts the
// folder, says both and that ronja.json changed, then proposes it.
func TestProposeSetsUpAnInitializedFolderInOneCommand(t *testing.T) {
	f, repo := setupFixture(t)
	if out, err := runCLI(t, repo, "app", "init", "new", "--feature", importFeature, "--title", "New"); err != nil {
		t.Fatalf("init: %v\n%s", err, out)
	}
	dir := filepath.Join(repo, "new")
	if m := loadAnyManifest(t, dir); m.ID != "" || len(m.Stacks) != 0 {
		t.Fatalf("init wrote a stack or a folder id, so this test proves nothing: %+v", m)
	}

	out, err := runPropose_(t, dir)
	if err != nil {
		t.Fatalf("propose: %v\n%s", err, out)
	}
	for _, want := range []string{
		"Set up this folder for change lists first, once:",
		`1. added stack "main" (feature ` + importFeature + `) to ronja.json`,
		`2. adopted it on stack "main": folder id `,
		"ronja.json changed: commit it",
		"Proposed change list changelist-1: 1 created.",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("propose does not say %q:\n%s", want, out)
		}
	}
	m := loadAnyManifest(t, dir)
	if m.ID == "" || m.Stacks["main"].FeatureID != importFeature || len(m.Instances) != 0 {
		t.Fatalf("ronja.json after propose: id %q, stacks %+v, instances %+v", m.ID, m.Stacks, m.Instances)
	}
	if n := len(f.changeLists.proposals); n != 1 {
		t.Fatalf("%d proposals, want 1", n)
	}
	p := f.changeLists.proposals[0]
	if copied := archiveFiles(t, p.archive)[wfdir.ManifestName]; !strings.Contains(copied, m.ID) || !strings.Contains(copied, `"main"`) {
		t.Errorf("the copy's ronja.json does not carry the folder id and stack main:\n%s", copied)
	}

	// Set up once: the next propose sets up nothing.
	out, err = runPropose_(t, dir, "--json")
	if err != nil {
		t.Fatalf("propose again: %v\n%s", err, out)
	}
	var report proposeReport
	decodeJSONInto(t, out, &report)
	if report.Setup != nil {
		t.Fatalf("the second propose set the folder up again: %+v", report.Setup)
	}
}

// The first propose, which sets the folder up, plans exactly what a later one
// plans: a sibling migrations folder bound without --stack is in both lists.
func TestProposeSetupPlansWhatALaterProposePlans(t *testing.T) {
	f, repo := setupFixture(t)
	if out, err := runCLI(t, repo, "app", "init", "new", "--feature", importFeature, "--title", "New"); err != nil {
		t.Fatalf("init: %v\n%s", err, out)
	}
	db := filepath.Join(repo, "db", "crm")
	writeLocal(t, db, "migrations/0001_leads.sql", "CREATE TABLE leads (id int);\n")
	writeLocal(t, db, "migrations/0002_score.sql", "ALTER TABLE leads ADD score int;\n")
	if err := (&dbdir.Binding{}).SaveStack(db, wfdir.InstanceKey{URL: f.URL(), TenantID: testTenantID}, "", "mdb-1"); err != nil {
		t.Fatal(err)
	}
	f.changeLists.applied["0001_leads"] = true

	var plans []string
	for run := 1; run <= 2; run++ {
		out, err := runPropose_(t, repo, "--json")
		if err != nil {
			t.Fatalf("propose %d: %v\n%s", run, err, out)
		}
		var report proposeReport
		decodeJSONInto(t, out, &report)
		if (report.Setup != nil) != (run == 1) || len(report.Notes) != 0 {
			t.Fatalf("propose %d: setup %+v, notes %q", run, report.Setup, report.Notes)
		}
		p := f.changeLists.proposals[len(f.changeLists.proposals)-1]
		var plan struct {
			Folders []struct {
				Path string `json:"path"`
			} `json:"folders"`
			Databases []proposeDatabase `json:"databases"`
		}
		if err := json.Unmarshal(p.plan, &plan); err != nil {
			t.Fatal(err)
		}
		if len(plan.Databases) != 1 || plan.Databases[0].DatabaseID != "mdb-1" || len(plan.Databases[0].Migrations) != 1 {
			t.Fatalf("propose %d planned databases %+v, want db/crm's pending migration", run, plan.Databases)
		}
		set, _ := json.Marshal(plan)
		plans = append(plans, string(set)+" stack="+p.stack)
	}
	if plans[0] != plans[1] {
		t.Fatalf("the first propose planned\n%s\nand the second\n%s", plans[0], plans[1])
	}
}

// A cloned folder proposes in one command too, whichever shape the clone
// wrote: propose claims the app it was cloned from, and with a stack already
// in ronja.json it only adopts.
func TestProposeSetsUpAClonedFolderInOneCommand(t *testing.T) {
	for _, tc := range []struct {
		name  string
		clone []string
	}{
		{"no stack", nil},
		{"a stack", []string{"--stack", "main"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f, repo := setupFixture(t)
			if out, err := runCLI(t, repo, append([]string{"app", "clone", "data_app-kpi", "kpi"}, tc.clone...)...); err != nil {
				t.Fatalf("clone: %v\n%s", err, out)
			}
			dir := filepath.Join(repo, "kpi")
			if loadAnyManifest(t, dir).ID != "" {
				t.Fatal("clone wrote a folder id, so this test proves nothing")
			}
			writeLocal(t, dir, "App.tsx", "export default function App() { return 'local' }\n")

			out, err := runPropose_(t, dir, "--json")
			if err != nil {
				t.Fatalf("propose: %v\n%s", err, out)
			}
			var report proposeReport
			decodeJSONInto(t, out, &report)
			s := report.Setup
			if s == nil || s.Stack != "main" || s.FeatureID != importFeature || s.Claimed != 1 || s.StackAdded != (tc.clone == nil) {
				t.Fatalf("setup = %+v", s)
			}
			if m := loadAnyManifest(t, dir); m.ID == "" || m.ID != s.FolderID {
				t.Fatalf("ronja.json's id = %q, the report's %q", m.ID, s.FolderID)
			}
			claimed := false
			for _, c := range f.managed.Claims(importFeature) {
				claimed = claimed || c.ResourceID == "data_app-kpi"
			}
			if !claimed {
				t.Fatal("the cloned app was not claimed for the folder")
			}
			if report.Status != "pending" || report.Counts.Modified != 1 || len(f.changeLists.proposals) != 1 {
				t.Fatalf("report = %+v, %d proposals", report, len(f.changeLists.proposals))
			}
		})
	}
}

// A folder whose ronja.json names no feature on this instance is refused,
// naming the one command that adds its stack, and nothing is written.
func TestProposeRefusesAFolderWithNoFeatureNamingTheCommand(t *testing.T) {
	f, repo := setupFixture(t)
	dir := filepath.Join(repo, "elsewhere")
	writeLocal(t, dir, "App.tsx", "export default function App() { return null }\n")
	writeLocal(t, dir, wfdir.ManifestName, `{
  "kind": "dataapp",
  "title": "Elsewhere",
  "entrypoint": "App.tsx",
  "instances": [{"url": "https://other.example.com", "tenantID": "ten-other", "featureID": "collection-other"}]
}
`)
	before := readManifestBytes(t, dir)
	f.resetRequests()

	out, err := runPropose_(t, dir)
	if err == nil {
		t.Fatalf("propose of a folder with no feature here succeeded:\n%s", out)
	}
	if !strings.Contains(err.Error(), "Run `ronja sync stack add --feature <feature-id>` there") {
		t.Fatalf("the refusal does not name the command: %v", err)
	}
	if after := readManifestBytes(t, dir); after != before {
		t.Fatalf("ronja.json changed:\n%s", after)
	}
	if len(f.writes()) != 0 || len(f.changeLists.proposals) != 0 {
		t.Fatalf("propose sent %v", f.writes())
	}
}

// In a tree, a folder with no stack is not set up: it is refused, naming the
// propose that sets it up on its own.
func TestProposeRefusesATreeNamingTheProposeThatSetsUpEachFolder(t *testing.T) {
	f, repo := setupFixture(t)
	for _, name := range []string{"one", "two"} {
		if out, err := runCLI(t, repo, "app", "init", name, "--feature", importFeature); err != nil {
			t.Fatalf("init: %v\n%s", err, out)
		}
	}
	f.resetRequests()

	out, err := runPropose_(t, repo)
	if err == nil {
		t.Fatalf("propose of a tree of folders with no stack succeeded:\n%s", out)
	}
	for _, name := range []string{"one", "two"} {
		if !strings.Contains(err.Error(), name+": run `ronja propose` there, on its own: it adds the folder's stack and adopts it") {
			t.Fatalf("the refusal does not name the command for %s: %v", name, err)
		}
		if m := loadAnyManifest(t, filepath.Join(repo, name)); m.ID != "" || len(m.Stacks) != 0 {
			t.Fatalf("%s was set up: %+v", name, m)
		}
	}
	if len(f.writes()) != 0 {
		t.Fatalf("propose sent %v", f.writes())
	}
}

// An app changed in Ronja since it was cloned (here through the chat's own
// tools: a draft, committed) is refused, never overwritten. The refusal names
// `app pull` and proposing again, never a push that deploys without approval,
// and propose still says ronja.json changed, since it set the folder up.
func TestProposeRefusesAnAppChangedInRonjaSinceItWasCloned(t *testing.T) {
	f, repo := setupFixture(t)
	if out, err := runCLI(t, repo, "app", "clone", "data_app-kpi", "kpi"); err != nil {
		t.Fatalf("clone: %v\n%s", err, out)
	}
	dir := filepath.Join(repo, "kpi")
	f.app.AddDraft("data_app-kpi", "data_app-kpi-draft", api.DataAppFile{Path: "App.tsx", Content: "export default function App() { return 'chat' }\n"})
	f.app.commitDraft("data_app-kpi-draft")
	writeLocal(t, dir, "App.tsx", "export default function App() { return 'local' }\n")

	out, err := runPropose_(t, dir)
	if err == nil {
		t.Fatalf("propose over a change made in Ronja succeeded:\n%s", out)
	}
	msg := err.Error()
	for _, want := range []string{
		"the live data app changed in Ronja since stack \"main\" last applied it — run `ronja app pull` to take it into this folder",
		"changed there  App.tsx",
		"A change list never overwrites a change made in Ronja",
		"then propose again",
	} {
		if !strings.Contains(msg, want) {
			t.Errorf("the refusal does not say %q:\n%s", want, msg)
		}
	}
	if strings.Contains(msg, "push --force") {
		t.Errorf("the refusal names a push that deploys without approval:\n%s", msg)
	}
	// The clone named no stack, so naming one moved its app id into the lock.
	if !strings.Contains(out, "ronja.json and ronja.lock.json changed: commit them") {
		t.Errorf("propose does not say ronja.json and ronja.lock.json changed:\n%s", out)
	}
	if len(f.changeLists.proposals) != 0 {
		t.Fatal("a list overwriting a change made in Ronja was uploaded")
	}
	if live := f.app.FileContents("data_app-kpi")["App.tsx"]; !strings.Contains(live, "chat") {
		t.Fatalf("the live app changed: %s", live)
	}
}
