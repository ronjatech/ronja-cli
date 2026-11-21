package commands

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"slices"
	"sort"
	"strings"
	"sync"
	"testing"

	"github.com/ronjatech/ronja-cli/internal/api"
	"github.com/ronjatech/ronja-cli/internal/dbdir"
	"github.com/ronjatech/ronja-cli/internal/markers"
	"github.com/ronjatech/ronja-cli/internal/wfdir"
)

// fakeChangeLists is POST /api/v2/change-list and POST
// /api/v2/database/:id/migrations as the server answers them (backend
// api/v2/changelist/handler.go, api/v2/database): the proposals received, and
// a ledger that holds the migrations named applied.
type fakeChangeLists struct {
	mu        sync.Mutex
	proposals []fakeProposal
	applied   map[string]bool
	// migrationPushes counts the non-dry-run migration requests.
	migrationPushes int
	// busy answers a proposal as a full instance does: 429 and Retry-After.
	busy bool
	// spaces is the Space GET /api/v2/database/:id names for each database.
	spaces map[string]string
	// migrationPaths is each migrations request's path as it was sent.
	migrationPaths []string
}

type fakeProposal struct {
	archive   []byte
	plan      json.RawMessage
	serverURL string
	stack     string
}

func newFakeChangeLists() *fakeChangeLists {
	return &fakeChangeLists{applied: map[string]bool{}, spaces: map[string]string{}}
}

func (c *fakeChangeLists) serve(w http.ResponseWriter, r *http.Request) bool {
	switch {
	case r.Method == http.MethodPost && r.URL.Path == "/api/v2/change-list":
		if c.busy {
			w.Header().Set("Retry-After", "5")
			http.Error(w, `{"error":"this Ronja is receiving as many change lists as it takes at once; propose again in a few seconds"}`, http.StatusTooManyRequests)
			return true
		}
		if err := r.ParseMultipartForm(64 << 20); err != nil {
			http.Error(w, `{"error":"not multipart"}`, http.StatusBadRequest)
			return true
		}
		file, _, err := r.FormFile("archive")
		if err != nil {
			http.Error(w, `{"error":"no archive"}`, http.StatusBadRequest)
			return true
		}
		archive, _ := io.ReadAll(file)
		p := fakeProposal{archive: archive, plan: json.RawMessage(r.FormValue("plan")),
			serverURL: r.FormValue("server_url"), stack: r.FormValue("stack")}
		c.mu.Lock()
		c.proposals = append(c.proposals, p)
		c.mu.Unlock()
		writeJSON(w, map[string]any{"id": "changelist-1", "status": "pending",
			"url": "https://app.example.com/change-lists/changelist-1?org=" + testTenantID})
		return true
	case r.Method == http.MethodGet && strings.HasPrefix(r.URL.Path, "/api/v2/database/") && !strings.Contains(strings.TrimPrefix(r.URL.Path, "/api/v2/database/"), "/"):
		id := strings.TrimPrefix(r.URL.Path, "/api/v2/database/")
		c.mu.Lock()
		space := c.spaces[id]
		c.mu.Unlock()
		writeJSON(w, map[string]any{"id": id, "name": id + "-name", "spaceID": space})
		return true
	case r.Method == http.MethodPost && strings.HasPrefix(r.URL.Path, "/api/v2/database/") && strings.HasSuffix(r.URL.Path, "/migrations"):
		var in api.MigrationSetInput
		if err := json.NewDecoder(r.Body).Decode(&in); err != nil {
			http.Error(w, `{"error":"bad body"}`, http.StatusBadRequest)
			return true
		}
		c.mu.Lock()
		defer c.mu.Unlock()
		c.migrationPaths = append(c.migrationPaths, r.URL.EscapedPath())
		if !in.DryRun {
			c.migrationPushes++
		}
		out := api.MigrationSetResult{DatabaseID: "mdb-1", DryRun: in.DryRun, Results: []api.MigrationResult{}, Notes: []string{}}
		for _, m := range in.Migrations {
			outcome := api.MigrationPending
			if c.applied[m.Name] {
				outcome = api.MigrationSkipped
			}
			out.Results = append(out.Results, api.MigrationResult{Name: m.Name, Outcome: outcome})
		}
		writeJSON(w, out)
		return true
	}
	return false
}

// proposeFixture is the fresh-stack tree (every kind: a module, a pipeline
// with tables and a metric, a workflow, a data app and automations), deployed
// once to its `dev` stack, then edited — the workflow's code changed and a new
// automation added — with a migrations folder bound to a managed database.
func proposeFixture(t *testing.T) (*fakeImportInstance, string) {
	t.Helper()
	f, dir, _ := freshStackFixture(t)
	if report, err := applyStack(t, dir, "dev", "--allow-create"); err != nil || report.Verdict != verdictApplied {
		t.Fatalf("the fixture's first deploy: %v\n%s", err, describeApply(report))
	}
	main := filepath.Join(dir, "workflows/sales-report/main.py")
	writeLocal(t, filepath.Dir(main), "main.py", readFile(t, main)+"\n# changed\n")
	automations := filepath.Join(dir, "automations/sales")
	entries, err := os.ReadDir(automations)
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range entries {
		if strings.HasSuffix(e.Name(), ".json") && e.Name() != wfdir.ManifestName && e.Name() != wfdir.LockName {
			writeLocal(t, automations, "weekly_copy.json", readFile(t, automations, e.Name()))
			break
		}
	}
	db := filepath.Join(dir, "db", "crm")
	writeLocal(t, db, "migrations/0001_leads.sql", "CREATE TABLE leads (id int);\n")
	writeLocal(t, db, "migrations/0002_score.sql", "ALTER TABLE leads ADD score int;\n")
	if err := (&dbdir.Binding{}).SaveStack(db, wfdir.InstanceKey{URL: f.URL(), TenantID: testTenantID}, "dev", "mdb-1"); err != nil {
		t.Fatal(err)
	}
	f.changeLists.applied["0001_leads"] = true
	f.resetRequests()
	return f, dir
}

// archiveFiles reads a proposal's copy back, by name.
func archiveFiles(t *testing.T, archive []byte) map[string]string {
	t.Helper()
	gz, err := gzip.NewReader(bytes.NewReader(archive))
	if err != nil {
		t.Fatalf("the copy is not gzip: %v", err)
	}
	tr := tar.NewReader(gz)
	out := map[string]string{}
	for {
		h, err := tr.Next()
		if errors.Is(err, io.EOF) {
			return out
		}
		if err != nil {
			t.Fatalf("read the copy: %v", err)
		}
		if h.Typeflag != tar.TypeReg {
			t.Fatalf("the copy holds %s, which is not a regular file", h.Name)
		}
		body, _ := io.ReadAll(tr)
		out[h.Name] = string(body)
	}
}

func runPropose_(t *testing.T, dir string, args ...string) (string, error) {
	t.Helper()
	return runCLI(t, dir, append([]string{"propose", "--no-browser"}, args...)...)
}

// TestProposeSendsOnlyReadsAndTheUpload: over a tree holding every kind and a
// migrations folder, propose sends GETs, the validate and the migrations dry
// run that save nothing, and the one upload — no push, publish, commit,
// automation write, claim or migrations push.
func TestProposeSendsOnlyReadsAndTheUpload(t *testing.T) {
	f, dir := proposeFixture(t)

	out, err := runPropose_(t, dir, "--stack", "dev", "--json")
	if err != nil {
		t.Fatalf("propose: %v\n%s", err, out)
	}
	var writes []string
	for _, w := range f.writes() {
		if w == "POST /api/v2/database/mdb-1/migrations" || w == "POST /api/v2/change-list" {
			continue
		}
		writes = append(writes, w)
	}
	if len(writes) > 0 {
		t.Fatalf("propose sent writes: %v", writes)
	}
	if f.changeLists.migrationPushes != 0 {
		t.Fatalf("propose pushed migrations %d times", f.changeLists.migrationPushes)
	}
	if n := len(f.changeLists.proposals); n != 1 {
		t.Fatalf("%d proposals were uploaded, want 1", n)
	}

	var report proposeReport
	decodeJSONInto(t, out, &report)
	if report.ID != "changelist-1" || report.Status != "pending" || !strings.Contains(report.URL, "/change-lists/changelist-1") {
		t.Fatalf("report = %+v", report)
	}
	if report.Approver != "dev@example.com" {
		t.Errorf("approver = %q, want the token's person", report.Approver)
	}
	if report.Counts.Modified == 0 || report.Counts.Created == 0 || report.Counts.Migrations != 1 {
		t.Errorf("counts = %+v, want the changed workflow modified, the new automation created and one migration", report.Counts)
	}

	p := f.changeLists.proposals[0]
	if p.serverURL != f.URL() || p.stack != "dev" {
		t.Errorf("server_url = %q, stack = %q", p.serverURL, p.stack)
	}
	var plan struct {
		Dir     string `json:"dir"`
		Folders []struct {
			Path     string `json:"path"`
			FolderID string `json:"folderID"`
			Verdict  string `json:"verdict"`
			Feature  string `json:"feature"`
			Units    []struct {
				Path   string `json:"path"`
				Action string `json:"action"`
			} `json:"units"`
		} `json:"folders"`
		Databases []struct {
			Dir        string `json:"dir"`
			DatabaseID string `json:"databaseID"`
			Migrations []struct {
				Name, SQL string
			} `json:"migrations"`
		} `json:"databases"`
	}
	if err := json.Unmarshal(p.plan, &plan); err != nil {
		t.Fatalf("plan: %v\n%s", err, p.plan)
	}
	kinds := map[string]bool{}
	folderIDs := map[string]bool{}
	for _, folder := range plan.Folders {
		if folder.Verdict != verdictApplied || folder.Feature == "" || folder.FolderID == "" {
			t.Errorf("folder %s: verdict %s, feature %q, folder id %q", folder.Path, folder.Verdict, folder.Feature, folder.FolderID)
		}
		if folderIDs[folder.FolderID] {
			t.Errorf("folder %s: folder id %s is another folder's too", folder.Path, folder.FolderID)
		}
		folderIDs[folder.FolderID] = true
		kinds[strings.Split(folder.Path, "/")[0]] = true
	}
	for _, kind := range []string{"modules", "pipelines", "workflows", "apps", "automations"} {
		if !kinds[kind] {
			t.Errorf("the plan has no %s folder: %s", kind, p.plan)
		}
	}
	if plan.Dir != "." || len(plan.Databases) != 1 || plan.Databases[0].Dir != "db/crm" || plan.Databases[0].DatabaseID != "mdb-1" ||
		len(plan.Databases[0].Migrations) != 1 || plan.Databases[0].Migrations[0].Name != "0002_score" ||
		plan.Databases[0].Migrations[0].SQL != "ALTER TABLE leads ADD score int;\n" {
		t.Errorf("the plan's databases = %+v", plan.Databases)
	}

	files := archiveFiles(t, p.archive)
	for _, want := range []string{
		"workflows/sales-report/main.py", "workflows/sales-report/ronja.json", "workflows/sales-report/ronja.lock.json",
		"automations/sales/weekly_copy.json", "db/crm/migrations/0002_score.sql",
	} {
		if _, ok := files[want]; !ok {
			t.Errorf("the copy has no %s", want)
		}
	}
	if files["workflows/sales-report/main.py"] != readFile(t, dir, "workflows/sales-report/main.py") {
		t.Error("the copy's main.py is not the folder's")
	}
}

// TestProposeUploadsNoCredentials: nothing under a dot-name is in the copy
// (.ronja/ state and bindings, .env, .git), no file holds the token, and a
// credential store inside the tree is refused rather than carried.
func TestProposeUploadsNoCredentials(t *testing.T) {
	f, dir := proposeFixture(t)
	writeLocal(t, dir, ".env", "RONJA_TOKEN=test-token\n")
	writeLocal(t, dir, "workflows/sales-report/.env", "SECRET=test-token\n")
	writeLocal(t, dir, "workflows/sales-report/.ronja/state.json", `{"token":"test-token"}`)
	writeLocal(t, dir, "apps/sales-dashboard/.npmrc", "//registry/:_authToken=test-token\n")

	out, err := runPropose_(t, dir, "--stack", "dev", "--json")
	if err != nil {
		t.Fatalf("propose: %v\n%s", err, out)
	}
	files := archiveFiles(t, f.changeLists.proposals[0].archive)
	names := make([]string, 0, len(files))
	for name, body := range files {
		names = append(names, name)
		for _, seg := range strings.Split(name, "/") {
			if strings.HasPrefix(seg, ".") {
				t.Errorf("the copy holds %s", name)
			}
		}
		if strings.Contains(body, "test-token") {
			t.Errorf("%s in the copy holds the token", name)
		}
	}
	sort.Strings(names)
	if len(names) == 0 {
		t.Fatal("the copy is empty")
	}

	// RONJA_CONFIG_DIR inside a folder of the tree: refused, nothing sent.
	creds := filepath.Join(dir, "workflows/sales-report/config")
	writeLocal(t, creds, "credentials.json", `{"token":"test-token"}`)
	t.Setenv("RONJA_CONFIG_DIR", creds)
	f.resetRequests()
	out, err = runPropose_(t, dir, "--stack", "dev")
	if err == nil || !strings.Contains(err.Error(), "credentials") {
		t.Fatalf("propose with the credential store in the tree: %v\n%s", err, out)
	}
	if len(f.changeLists.proposals) != 1 {
		t.Fatalf("a proposal carrying the credential store was uploaded")
	}
}

// TestProposeRefusesUnadoptedFolder: a folder whose state Ronja does not keep
// is refused by name, with the command that adopts it, and nothing is sent
// but reads.
func TestProposeRefusesUnadoptedFolder(t *testing.T) {
	f, dir := proposeFixture(t)
	root := filepath.Join(dir, "workflows/sales-report")
	m := loadAnyManifest(t, root)
	m.ID = ""
	if err := wfdir.SaveManifest(root, m); err != nil {
		t.Fatal(err)
	}

	out, err := runPropose_(t, dir, "--stack", "dev")
	if err == nil {
		t.Fatalf("propose of an unadopted folder succeeded:\n%s", out)
	}
	if !strings.Contains(err.Error(), "workflows/sales-report: run `ronja sync adopt --stack dev` there") {
		t.Fatalf("the refusal does not name the folder and the remedy: %v", err)
	}
	if len(f.changeLists.proposals) != 0 || len(f.writes()) != 0 {
		t.Fatalf("propose sent %v", f.writes())
	}

	// A folder with an id whose stack is unmanaged keeps no state there either.
	m.ID = "7a1b2c3d-4e5f-4a6b-8c7d-9e0f1a2b3c4d"
	stack := m.Stacks["dev"]
	stack.Unmanaged = true
	m.Stacks["dev"] = stack
	if err := wfdir.SaveManifest(root, m); err != nil {
		t.Fatal(err)
	}
	out, err = runPropose_(t, dir, "--stack", "dev")
	if err == nil || !strings.Contains(err.Error(), "workflows/sales-report: run `ronja sync adopt --stack dev` there") {
		t.Fatalf("propose of a folder whose stack is unmanaged: %v\n%s", err, out)
	}
	if len(f.changeLists.proposals) != 0 {
		t.Fatal("a list with an unmanaged folder was uploaded")
	}
}

// TestProposeRefusesUnknownFolder: a folder the dry run cannot vouch for (its
// stack is on another instance) fails propose by folder, with the dry run's own
// reason, and nothing is uploaded.
func TestProposeRefusesUnknownFolder(t *testing.T) {
	f, dir := proposeFixture(t)
	root := filepath.Join(dir, "apps/sales-dashboard")
	m := loadAnyManifest(t, root)
	elsewhere := m.Stacks["dev"]
	elsewhere.URL = "https://other.example.com"
	m.Stacks["dev"] = elsewhere
	if err := wfdir.SaveManifest(root, m); err != nil {
		t.Fatal(err)
	}

	out, err := runPropose_(t, dir, "--stack", "dev")
	if err == nil {
		t.Fatalf("propose with an unknown folder succeeded:\n%s", out)
	}
	if !strings.Contains(err.Error(), "apps/sales-dashboard: unknown ("+syncReasonStackElsewhere+")") {
		t.Fatalf("the refusal does not name the folder and the dry run's reason: %v", err)
	}
	if len(f.changeLists.proposals) != 0 {
		t.Fatal("a list that does not plan cleanly was uploaded")
	}
}

// TestProposeRefusesAnAddressOutsideTheTree: inside one git repository, a
// folder that names by address a folder outside the directory proposed is
// refused address_outside_repository: the copy would not hold it.
func TestProposeRefusesAnAddressOutsideTheTree(t *testing.T) {
	fx := newAddressFixture(t, testTenantID)
	fx.claimOrders()
	m := loadAnyManifest(t, fx.workflow)
	m.ID = "6f1c2d3e-4b5a-4c6d-8e7f-9a0b1c2d3e4f"
	if err := wfdir.SaveManifest(fx.workflow, m); err != nil {
		t.Fatal(err)
	}

	out, err := runCLI(t, fx.tree, "propose", "--no-browser", "--stack", "dev", "workflows")
	if err == nil || !strings.Contains(err.Error(), syncReasonAddressOutsideRepository) {
		t.Fatalf("propose of a subtree whose folder reads outside it: %v\n%s", err, out)
	}
	if len(fx.f.changeLists.proposals) != 0 {
		t.Fatal("a list whose copy lacks a folder it needs was uploaded")
	}

	// The control: `sync apply` of the same subtree resolves the address, its
	// boundary being the git work tree, so the refusal is propose's.
	out, _ = runCLI(t, fx.tree, "sync", "apply", "--dry-run", "--stack", "dev", "--json", "--dir", "workflows")
	var report syncApplyReport
	decodeJSONInto(t, out, &report)
	if len(report.Folders) != 1 || report.Folders[0].Reason == syncReasonAddressOutsideRepository {
		t.Fatalf("sync apply refused the address too:\n%s", out)
	}
}

// TestRunnerShapedCopyRefusesAnAddressThatEscapesIt is the deploy job's view:
// an unpacked copy in no git work tree, `sync apply --dry-run` with --dir set
// to the copy, as the job runs it with only HOME, RONJA_CONFIG_DIR, RONJA_URL
// and RONJA_TOKEN set, and a "from" that leads out of the copy is refused
// address_outside_repository rather than read.
func TestRunnerShapedCopyRefusesAnAddressThatEscapesIt(t *testing.T) {
	fx := newAddressFixture(t, testTenantID)
	fx.claimOrders()
	if err := os.Remove(filepath.Join(fx.tree, ".git")); err != nil {
		t.Fatal(err)
	}
	copyDir := filepath.Join(fx.tree, "copy")
	if err := os.Rename(filepath.Join(fx.tree, "workflows"), filepath.Join(copyDir, "workflows")); err != nil {
		if err := os.MkdirAll(copyDir, 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.Rename(filepath.Join(fx.tree, "workflows"), filepath.Join(copyDir, "workflows")); err != nil {
			t.Fatal(err)
		}
	}
	m := loadAnyManifest(t, filepath.Join(copyDir, "workflows/report"))
	m.Dependencies["orders"] = wfdir.Dependency{Kind: markers.KindTable, From: "../../../pipelines/sales", Address: "orders.sql"}
	if err := wfdir.SaveManifest(filepath.Join(copyDir, "workflows/report"), m); err != nil {
		t.Fatal(err)
	}
	t.Setenv("HOME", t.TempDir())
	t.Setenv("RONJA_CONFIG_DIR", t.TempDir())

	out, err := runCLI(t, t.TempDir(), "sync", "apply", "--json", "--allow-create", "--dir", copyDir, "--stack=dev", "--dry-run")
	if err == nil {
		t.Fatalf("the dry run of a copy reading outside itself succeeded:\n%s", out)
	}
	var report syncApplyReport
	decodeJSONInto(t, out, &report)
	if len(report.Folders) != 1 || report.Folders[0].Reason != syncReasonAddressOutsideRepository {
		t.Fatalf("want the folder refused %s:\n%s", syncReasonAddressOutsideRepository, out)
	}

	// The control: the same dependency inside the copy resolves.
	if err := os.Rename(filepath.Join(fx.tree, "pipelines"), filepath.Join(copyDir, "pipelines")); err != nil {
		t.Fatal(err)
	}
	m.Dependencies["orders"] = wfdir.Dependency{Kind: markers.KindTable, From: "../../pipelines/sales", Address: "orders.sql"}
	if err := wfdir.SaveManifest(filepath.Join(copyDir, "workflows/report"), m); err != nil {
		t.Fatal(err)
	}
	out, _ = runCLI(t, t.TempDir(), "sync", "apply", "--json", "--allow-create", "--dir", copyDir, "--stack=dev", "--dry-run")
	report = syncApplyReport{}
	decodeJSONInto(t, out, &report)
	for _, folder := range report.Folders {
		if folder.Reason == syncReasonAddressOutsideRepository {
			t.Fatalf("a dependency inside the copy was refused:\n%s", out)
		}
	}
}

// The copy holds exactly what a pipeline folder's push reads, beside the
// manifest and the lock: no README, no key file, nothing under .ronja/.
func TestProposePacksOnlyWhatThePushReads(t *testing.T) {
	f := newFakePipelineInstance(t)
	tree, root := applyPipelineTree(t, f, map[string]string{"orders.sql": "SELECT 1"},
		map[string]string{"orders.sql": "table-orders"}, map[string]string{"orders.sql": "SELECT 1"})
	writeLocal(t, root, "metrics/aov.json", metricFileBody(aovRecipe))
	writeLocal(t, root, "checks/orders.json", `{"checks":[]}`)
	writeLocal(t, root, "tables/fortnox.json", `{"description":"x"}`)
	writeLocal(t, root, "README.md", "how to run this\n")
	writeLocal(t, root, "keys/deploy.pem", "-----BEGIN PRIVATE KEY-----\n")
	writeLocal(t, root, ".ronja/state.json", `{}`)
	writeLocal(t, root, ".ronja/"+wfdir.ProposedFileName, `{"proposals":[]}`)

	found, err := discoverFolders(tree)
	if err != nil {
		t.Fatal(err)
	}
	archive, _, err := packProposal(tree, found, nil)
	if err != nil {
		t.Fatal(err)
	}
	var names []string
	for name := range archiveFiles(t, archive) {
		names = append(names, name)
	}
	sort.Strings(names)
	want := []string{"sales/checks/orders.json", "sales/metrics/aov.json", "sales/orders.sql",
		"sales/" + wfdir.ManifestName, "sales/" + wfdir.LockName, "sales/tables/fortnox.json"}
	sort.Strings(want)
	if !slices.Equal(names, want) {
		t.Fatalf("the copy holds %v, want %v", names, want)
	}
}

// Planning may send the workflow validate and a database's migrations route,
// which propose reaches only as a dry run, and no other write.
func TestProposeReadAllowAdmitsOnlyThePlanningWrites(t *testing.T) {
	for _, c := range []struct {
		method, path string
		want         bool
	}{
		{"POST", "workflow/validate", true},
		{"POST", "database/mdb-1/migrations", true},
		{"PUT", "database/mdb-1/migrations", false},
		{"POST", "database/mdb-1/migrations/x", false},
		{"POST", "database//migrations", false},
		{"POST", "workflow/wf-1/commit", false},
		{"POST", "change-list", false},
	} {
		if got := proposeReadAllow(c.method, c.path); got != c.want {
			t.Errorf("%s %s: %v, want %v", c.method, c.path, got, c.want)
		}
	}
}

// A Ronja receiving as many proposals as it takes answers 429: propose says
// nothing was proposed and to run it again.
func TestProposeSaysToRetryWhenTheInstanceIsBusy(t *testing.T) {
	f, dir := proposeFixture(t)
	f.changeLists.busy = true
	out, err := runPropose_(t, dir, "--stack", "dev")
	if err == nil || !strings.Contains(err.Error(), "nothing was proposed") || !strings.Contains(err.Error(), "again") {
		t.Fatalf("propose: %v\n%s", err, out)
	}
	for _, folder := range []string{"workflows/sales-report"} {
		if _, statErr := os.Stat(filepath.Join(dir, folder, ".ronja", "proposed.json")); statErr == nil {
			t.Errorf("%s recorded a proposal that was never made", folder)
		}
	}
}

// migrationsOnlyFolder is a release folder holding only migrations/ (no
// ronja.json), the first already applied, bound to boundTo unless it is empty.
func migrationsOnlyFolder(t *testing.T, f *fakeImportInstance, boundTo string) string {
	t.Helper()
	dir := t.TempDir()
	writeLocal(t, dir, "migrations/0001_leads.sql", "CREATE TABLE leads (id int);\n")
	writeLocal(t, dir, "migrations/0002_score.sql", "ALTER TABLE leads ADD score int;\n")
	if boundTo != "" {
		if err := (&dbdir.Binding{}).SaveStack(dir, wfdir.InstanceKey{URL: f.URL(), TenantID: testTenantID}, "", boundTo); err != nil {
			t.Fatal(err)
		}
	}
	f.changeLists.applied["0001_leads"] = true
	return dir
}

// proposedDatabases is the databases of the one plan uploaded, and its folders.
func proposedDatabases(t *testing.T, f *fakeImportInstance) ([]proposeDatabase, []json.RawMessage) {
	t.Helper()
	if n := len(f.changeLists.proposals); n != 1 {
		t.Fatalf("%d proposals were uploaded, want 1", n)
	}
	var plan struct {
		Folders   []json.RawMessage `json:"folders"`
		Databases []proposeDatabase `json:"databases"`
	}
	if err := json.Unmarshal(f.changeLists.proposals[0].plan, &plan); err != nil {
		t.Fatalf("plan: %v\n%s", err, f.changeLists.proposals[0].plan)
	}
	return plan.Databases, plan.Folders
}

// TestProposeFilesAMigrationsOnlyFolder: a folder holding only migrations/
// and its saved binding proposes the pending migration's exact SQL for the
// bound database, with no folders, and pushes nothing.
func TestProposeFilesAMigrationsOnlyFolder(t *testing.T) {
	f := newFakeImportInstance(t)
	signInTo(t, f.URL())
	dir := migrationsOnlyFolder(t, f, "mdb-1")

	out, err := runPropose_(t, dir, "--json")
	if err != nil {
		t.Fatalf("propose: %v\n%s", err, out)
	}
	if f.changeLists.migrationPushes != 0 {
		t.Fatalf("propose pushed migrations %d times", f.changeLists.migrationPushes)
	}
	databases, folders := proposedDatabases(t, f)
	if len(folders) != 0 {
		t.Errorf("the plan has folders: %s", folders)
	}
	if len(databases) != 1 || databases[0].Dir != "." || databases[0].DatabaseID != "mdb-1" ||
		len(databases[0].Migrations) != 1 || databases[0].Migrations[0].Name != "0002_score" ||
		databases[0].Migrations[0].SQL != "ALTER TABLE leads ADD score int;\n" {
		t.Errorf("the plan's databases = %+v", databases)
	}
	if files := archiveFiles(t, f.changeLists.proposals[0].archive); files["migrations/0002_score.sql"] != "ALTER TABLE leads ADD score int;\n" {
		t.Errorf("the copy holds %v, want the migrations folder", files)
	}
}

// TestProposeDatabaseFlag: --database names the database of the one
// migrations folder under the directory proposed, bound or not, over its
// binding, and writes no binding; zero or several folders are refused by name
// with nothing uploaded.
func TestProposeDatabaseFlag(t *testing.T) {
	for _, c := range []struct{ name, boundTo, flag string }{
		{"an unbound folder", "", "mdb-x"},
		{"over the folder's binding", "mdb-dev", "mdb-prod"},
	} {
		t.Run(c.name, func(t *testing.T) {
			f := newFakeImportInstance(t)
			signInTo(t, f.URL())
			dir := migrationsOnlyFolder(t, f, c.boundTo)
			before, _ := os.ReadFile(dbdir.Path(dir))

			out, err := runPropose_(t, dir, "--database", c.flag, "--json")
			if err != nil {
				t.Fatalf("propose --database %s: %v\n%s", c.flag, err, out)
			}
			databases, _ := proposedDatabases(t, f)
			if len(databases) != 1 || databases[0].DatabaseID != c.flag || len(databases[0].Migrations) != 1 ||
				databases[0].Migrations[0].Name != "0002_score" {
				t.Errorf("the plan's databases = %+v, want 0002_score for %s", databases, c.flag)
			}
			if !slices.Contains(f.writes(), "POST /api/v2/database/"+c.flag+"/migrations") {
				t.Errorf("the status was not asked of %s: %v", c.flag, f.writes())
			}
			if after, _ := os.ReadFile(dbdir.Path(dir)); !bytes.Equal(before, after) {
				t.Errorf("propose changed the binding: %q -> %q", before, after)
			}
		})
	}

	// An id that is not one path segment reaches the migrations route escaped,
	// never another route through the planning guard.
	for _, id := range []string{"mdb-x?y", "mdb-a/b"} {
		t.Run("an id holding "+id, func(t *testing.T) {
			f := newFakeImportInstance(t)
			signInTo(t, f.URL())
			dir := migrationsOnlyFolder(t, f, "")

			out, err := runPropose_(t, dir, "--database", id, "--json")
			if err != nil {
				t.Fatalf("propose --database %s: %v\n%s", id, err, out)
			}
			want := "/api/v2/database/" + url.PathEscape(id) + "/migrations"
			if !slices.Equal(f.changeLists.migrationPaths, []string{want}) {
				t.Errorf("the migrations requests went to %v, want %s", f.changeLists.migrationPaths, want)
			}
			if databases, _ := proposedDatabases(t, f); len(databases) != 1 || databases[0].DatabaseID != id {
				t.Errorf("the plan's databases = %+v, want %s", databases, id)
			}
		})
	}

	t.Run("two migrations folders", func(t *testing.T) {
		f := newFakeImportInstance(t)
		signInTo(t, f.URL())
		dir := t.TempDir()
		writeLocal(t, dir, "crm/migrations/0001_leads.sql", "CREATE TABLE leads (id int);\n")
		writeLocal(t, dir, "billing/migrations/0001_invoices.sql", "CREATE TABLE invoices (id int);\n")

		out, err := runPropose_(t, dir, "--database", "mdb-x")
		if err == nil || !strings.Contains(err.Error(), "crm/migrations") || !strings.Contains(err.Error(), "billing/migrations") {
			t.Fatalf("propose --database over two migrations folders: %v\n%s", err, out)
		}
		if len(f.changeLists.proposals) != 0 {
			t.Fatal("a list was uploaded")
		}
	})

	t.Run("beside another tool's nested migrations folder", func(t *testing.T) {
		f := newFakeImportInstance(t)
		signInTo(t, f.URL())
		dir := migrationsOnlyFolder(t, f, "")
		writeLocal(t, dir, "app/migrations/env.py", "# Alembic's environment\n")
		writeLocal(t, dir, "app/migrations/versions/abc_init.py", "# an Alembic revision\n")

		out, err := runPropose_(t, dir, "--database", "mdb-x", "--json")
		if err != nil {
			t.Fatalf("propose --database beside app/migrations: %v\n%s", err, out)
		}
		if databases, _ := proposedDatabases(t, f); len(databases) != 1 || databases[0].DatabaseID != "mdb-x" || databases[0].Dir != "." {
			t.Errorf("the plan's databases = %+v, want the base's folder for mdb-x", databases)
		}
	})

	t.Run("no migrations folder", func(t *testing.T) {
		f := newFakeImportInstance(t)
		signInTo(t, f.URL())
		out, err := runPropose_(t, t.TempDir(), "--database", "mdb-x")
		if err == nil || !strings.Contains(err.Error(), "no migrations/ folder holding .sql files") {
			t.Fatalf("propose --database with no migrations folder: %v\n%s", err, out)
		}
		if len(f.changeLists.proposals) != 0 {
			t.Fatal("a list was uploaded")
		}
	})
}

// TestProposeChecksTheDatabasesSpace: with --stack, propose refuses a
// database outside the Space that stack declares in the repository, by flag
// or by binding, as `ronja db migrate push --stack` does, and uploads nothing;
// a database in the stack's Space is proposed.
func TestProposeChecksTheDatabasesSpace(t *testing.T) {
	// The repository's workflow folder declares stack "dev" in Space A; the
	// migrations folder beside it is the one proposed.
	repoWithDev := func(t *testing.T, f *fakeImportInstance) string {
		t.Helper()
		repo := t.TempDir()
		if err := os.Mkdir(filepath.Join(repo, ".git"), 0o755); err != nil {
			t.Fatal(err)
		}
		writeDevStackFolder(t, filepath.Join(repo, "workflows", "report"), f.URL(), testTenantID, "space-a")
		writeLocal(t, repo, "db/migrations/0001_leads.sql", "CREATE TABLE leads (id int);\n")
		f.changeLists.spaces["mdb-a"], f.changeLists.spaces["mdb-b"] = "space-a", "space-b"
		return filepath.Join(repo, "db")
	}
	refused := func(t *testing.T, f *fakeImportInstance, out string, err error) {
		t.Helper()
		r, ok := asManagedRefusal(err)
		if !ok || r.Reason != syncReasonBindSpaceMismatch || !strings.Contains(r.Detail, "mdb-b") ||
			!strings.HasSuffix(r.Detail, "Pass --database with a database in the stack's Space; nothing was proposed") {
			t.Fatalf("propose of a database in Space B on the dev stack: %v\n%s", err, out)
		}
		if len(f.changeLists.proposals) != 0 {
			t.Fatal("a list was uploaded")
		}
		if slices.Contains(f.writes(), "POST /api/v2/database/mdb-b/migrations") {
			t.Errorf("the migration status was read after the refusal: %v", f.writes())
		}
	}

	t.Run("by --database, outside the Space", func(t *testing.T) {
		f := newFakeImportInstance(t)
		signInTo(t, f.URL())
		out, err := runPropose_(t, repoWithDev(t, f), "--stack", "dev", "--database", "mdb-b", "--json")
		refused(t, f, out, err)
	})

	t.Run("by binding, outside the Space", func(t *testing.T) {
		f := newFakeImportInstance(t)
		signInTo(t, f.URL())
		dir := repoWithDev(t, f)
		if err := (&dbdir.Binding{}).SaveStack(dir, wfdir.InstanceKey{URL: f.URL(), TenantID: testTenantID}, "dev", "mdb-b"); err != nil {
			t.Fatal(err)
		}
		out, err := runPropose_(t, dir, "--stack", "dev", "--json")
		refused(t, f, out, err)
	})

	t.Run("by --database, inside the Space", func(t *testing.T) {
		f := newFakeImportInstance(t)
		signInTo(t, f.URL())
		out, err := runPropose_(t, repoWithDev(t, f), "--stack", "dev", "--database", "mdb-a", "--json")
		if err != nil {
			t.Fatalf("propose of a database in the dev stack's Space: %v\n%s", err, out)
		}
		if databases, _ := proposedDatabases(t, f); len(databases) != 1 || databases[0].DatabaseID != "mdb-a" {
			t.Errorf("the plan's databases = %+v, want mdb-a", databases)
		}
	})
}

// TestProposeRefusesMoreThanOnePushHolds: a database with more migrations
// pending than one push applies is refused, naming the last that fits, and
// nothing is uploaded: the deploy job's push would refuse it after approval.
func TestProposeRefusesMoreThanOnePushHolds(t *testing.T) {
	f := newFakeImportInstance(t)
	signInTo(t, f.URL())
	dir := t.TempDir()
	for i := 1; i <= maxPendingPerPush+1; i++ {
		writeLocal(t, dir, fmt.Sprintf("migrations/%04d_step.sql", i), fmt.Sprintf("SELECT %d;\n", i))
	}

	out, err := runPropose_(t, dir, "--database", "mdb-x")
	if err == nil || !strings.Contains(err.Error(), "41 migrations are not applied yet") ||
		!strings.Contains(err.Error(), "Propose up to and including 0040_step first") {
		t.Fatalf("propose of 41 pending: %v\n%s", err, out)
	}
	if len(f.changeLists.proposals) != 0 {
		t.Fatal("a list was uploaded")
	}
}
