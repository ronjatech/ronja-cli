package commands

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"

	"github.com/ronjatech/ronja-cli/internal/api"
	"github.com/ronjatech/ronja-cli/internal/markers"
	"github.com/ronjatech/ronja-cli/internal/wfdir"
)

// Dependency order, two passes and in-run resolution (sync_apply_order.go), on
// hand-written ADOPTED trees deployed to an empty feature on the import
// fixture's one instance.

// orderTree is an empty repository (under git) on the import fixture's
// instance, with an empty `dev` feature.
func orderTree(t *testing.T) (*fakeImportInstance, string) {
	t.Helper()
	f := importFixture(t)
	f.pipe.privilegeLevel = adminPrivilegeLevel
	f.pipe.AddFeature(devFeature, "Sales dev", "organization")
	dir := t.TempDir()
	if err := os.Mkdir(filepath.Join(dir, ".git"), 0o755); err != nil {
		t.Fatal(err)
	}
	return f, dir
}

// adoptedFolder writes an adopted folder of kind at rel with stack dev on the
// dev feature, its dependencies and binds, and files.
func adoptedFolder(t *testing.T, f *fakeImportInstance, dir, rel string, m *wfdir.Manifest, bind map[string]string, files map[string]string) string {
	t.Helper()
	root := filepath.Join(dir, filepath.FromSlash(rel))
	if err := os.MkdirAll(root, 0o755); err != nil {
		t.Fatal(err)
	}
	m.ID = testFolderID(rel)
	m.FormatVersion = wfdir.ManifestFormatVersion
	m.Stacks = map[string]wfdir.Stack{"dev": {URL: f.URL(), TenantID: testTenantID, FeatureID: devFeature, Bind: bind}}
	if err := wfdir.SaveFolder(root, m, &wfdir.Lock{}); err != nil {
		t.Fatal(err)
	}
	for path, content := range files {
		writeLocal(t, root, path, content)
	}
	return root
}

func workflowsIn(f *fakeImportInstance, feature string) []*api.Workflow {
	var out []*api.Workflow
	for _, wf := range f.wf.workflows {
		if wf.FeatureID == feature {
			out = append(out, wf)
		}
	}
	return out
}

// cycleTree is two workflows that run each other by address — A→B→A.
func cycleTree(t *testing.T) (*fakeImportInstance, string) {
	f, dir := orderTree(t)
	for _, pair := range [][2]string{{"a", "b"}, {"b", "a"}} {
		adoptedFolder(t, f, dir, "workflows/"+pair[0], &wfdir.Manifest{Kind: wfdir.KindWorkflow, Title: "Flow " + pair[0], Entrypoint: "main.py",
			Dependencies: map[string]wfdir.Dependency{"next": {Kind: markers.KindWorkflow, From: "../" + pair[1]}}},
			nil, map[string]string{"main.py": "tools.runWorkflow({{ workflow('next') }})\n"})
	}
	return f, dir
}

// TestFreshStackWorkflowCycleNeedsNoOrder: a `{{ workflow }}` cycle stands up
// in one apply — pass one creates both rows, so each one's code names the
// other's new id — and a second apply writes nothing.
func TestFreshStackWorkflowCycleNeedsNoOrder(t *testing.T) {
	f, dir := cycleTree(t)
	report, err := applyStack(t, dir, "dev", "--allow-create")
	if err != nil || report.Verdict != verdictApplied {
		t.Fatalf("the cycle did not go up in one apply: %v\n%s", err, describeApply(report))
	}
	flows := workflowsIn(f, devFeature)
	if len(flows) != 2 {
		t.Fatalf("%d workflows in the dev feature, want 2", len(flows))
	}
	byTitle := map[string]*api.Workflow{}
	for _, wf := range flows {
		byTitle[wf.Title] = wf
	}
	a, b := byTitle["Flow a"], byTitle["Flow b"]
	if got := f.wf.FileContents(a.ID)["main.py"]; !strings.Contains(got, "{{ workflow('"+b.ID+"') }}") {
		t.Fatalf("a does not run b's new id %s:\n%s", b.ID, got)
	}
	if got := f.wf.FileContents(b.ID)["main.py"]; !strings.Contains(got, "{{ workflow('"+a.ID+"') }}") {
		t.Fatalf("b does not run a's new id %s:\n%s", a.ID, got)
	}
	if report.Summary.Created != 2 {
		t.Fatalf("created = %d, want 2", report.Summary.Created)
	}
	f.resetRequests()
	if again, err := applyStack(t, dir, "dev", "--allow-create"); err != nil || again.Verdict != verdictApplied {
		t.Fatalf("second apply: %v\n%s", err, describeApply(again))
	}
	if writes := f.writes(); len(writes) != 0 {
		t.Fatalf("the second apply sent %v", writes)
	}
}

// TestPassOnePinsAnUnpinnedWorkflowsRuntime: pass one creates an adopted
// workflow without going through the folder's push, and still names the
// instance's runtime on the create, pins it in ronja.json and says so — as a
// `wf push` create does.
func TestPassOnePinsAnUnpinnedWorkflowsRuntime(t *testing.T) {
	f, dir := cycleTree(t)
	f.wf.defaults = &api.WorkflowDefaults{RuntimeVersion: wfdir.RuntimeMain, EntrypointStarter: "x\n"}
	report, err := applyStack(t, dir, "dev", "--allow-create")
	if err != nil || report.Verdict != verdictApplied {
		t.Fatalf("apply: %v\n%s", err, describeApply(report))
	}
	if len(f.wf.created) != 2 {
		t.Fatalf("created %+v, want both workflows", f.wf.created)
	}
	for _, c := range f.wf.created {
		if c.RuntimeVersion != wfdir.RuntimeMain {
			t.Errorf("create of %q sent runtime %d, want the instance's 4", c.Title, c.RuntimeVersion)
		}
	}
	for _, line := range report.Folders {
		root := filepath.Join(dir, filepath.FromSlash(line.Path))
		if m, err := wfdir.LoadManifest(root, wfdir.WorkflowKind); err != nil || m.Runtime != wfdir.RuntimeMain {
			t.Errorf("%s: manifest after the create %v (err %v), want runtime 4 pinned", line.Path, m, err)
		}
		if want := runtimePinnedNote(wfdir.RuntimeMain); !strings.Contains(strings.Join(line.Notes, "\n"), want) {
			t.Errorf("%s: notes %q do not say %q", line.Path, line.Notes, want)
		}
	}
}

// TestFreshStackCrashBetweenPassesThenRerun: pass one creates both workflows,
// and pass two fails on every file write — the run stops with the rows created
// and claimed but holding no code. A rerun from a checkout that lost every
// lock (CI) finds both rows through their claims and finishes them: no second
// copy of either.
func TestFreshStackCrashBetweenPassesThenRerun(t *testing.T) {
	f, dir := cycleTree(t)
	f.wf.failPut["main.py"] = 500
	if report, err := applyStack(t, dir, "dev", "--allow-create"); err == nil {
		t.Fatalf("an apply whose every file write failed went through:\n%s", describeApply(report))
	}
	if n := len(workflowsIn(f, devFeature)); n != 2 {
		t.Fatalf("pass one made %d workflows, want 2", n)
	}
	for _, rel := range []string{"workflows/a", "workflows/b"} {
		if err := os.Remove(wfdir.LockPath(filepath.Join(dir, rel))); err != nil {
			t.Fatal(err)
		}
	}
	dropLocalState(t, dir)
	delete(f.wf.failPut, "main.py")

	report, err := applyStack(t, dir, "dev", "--allow-create")
	if err != nil || report.Verdict != verdictApplied {
		t.Fatalf("the rerun: %v\n%s", err, describeApply(report))
	}
	if n := len(workflowsIn(f, devFeature)); n != 2 {
		t.Fatalf("the rerun left %d workflows in the dev feature, want the same 2", n)
	}
	if report.Summary.Created != 0 {
		t.Fatalf("the rerun created %d rows", report.Summary.Created)
	}
}

// pipelineTree: pipelines/a-report reads pipelines/b-base's table by address,
// so PATH order would build the report before the table it reads.
func pipelineTree(t *testing.T) (*fakeImportInstance, string) {
	f, dir := orderTree(t)
	adoptedFolder(t, f, dir, "pipelines/b-base", &wfdir.Manifest{Kind: wfdir.KindPipeline, Title: "Base",
		Dependencies: map[string]wfdir.Dependency{"raw": {Kind: markers.KindTable}}},
		map[string]string{"raw": "table-raw-orders"}, map[string]string{"base.sql": "SELECT * FROM {{ ref('raw') }}\n"})
	adoptedFolder(t, f, dir, "pipelines/a-report", &wfdir.Manifest{Kind: wfdir.KindPipeline, Title: "Report",
		Dependencies: map[string]wfdir.Dependency{"base": {Kind: markers.KindTable, From: "../b-base", Address: "base.sql"}}},
		nil, map[string]string{"report.sql": "SELECT * FROM {{ ref('base') }}\n"})
	return f, dir
}

// TestApplyBuildsPipelinesInTheOrderTheirTablesAreRead: a table read by address
// is a build edge — the base folder runs first, and the report's table reads
// the base table this run created.
func TestApplyBuildsPipelinesInTheOrderTheirTablesAreRead(t *testing.T) {
	f, dir := pipelineTree(t)
	report, err := applyStack(t, dir, "dev", "--allow-create")
	if err != nil || report.Verdict != verdictApplied {
		t.Fatalf("apply: %v\n%s", err, describeApply(report))
	}
	if got := strings.Join(report.Sequence, ","); got != "pipelines/b-base,pipelines/a-report" {
		t.Fatalf("sequence = %s, want the base first", got)
	}
	rows := devRows(f, devFeature)
	base, rep := rows.tables["base"], rows.tables["report"]
	if base == nil || rep == nil || !strings.Contains(rep.Code, "{{ ref('"+base.ID+"') }}") {
		t.Fatalf("the report does not read the base table this run made: %+v / %+v", base, rep)
	}
}

// TestApplyRefusesAPipelineReadCycle: two pipeline folders reading each
// other's tables have no build order, so both are refused cyclic_dependency
// and nothing is created.
func TestApplyRefusesAPipelineReadCycle(t *testing.T) {
	f, dir := pipelineTree(t)
	root := filepath.Join(dir, "pipelines/b-base")
	m := loadAnyManifest(t, root)
	m.Dependencies["report"] = wfdir.Dependency{Kind: markers.KindTable, From: "../a-report", Address: "report.sql"}
	if err := wfdir.SaveManifest(root, m); err != nil {
		t.Fatal(err)
	}
	writeLocal(t, root, "loop.sql", "SELECT * FROM {{ ref('report') }}\n")
	f.resetRequests()
	report, err := applyStack(t, dir, "dev", "--allow-create")
	if err == nil {
		t.Fatalf("a read cycle applied:\n%s", describeApply(report))
	}
	for _, line := range report.Folders {
		if line.Reason != syncReasonCyclicDependency || !strings.Contains(line.Detail, "pipelines/a-report, pipelines/b-base") {
			t.Fatalf("%s: %s / %s, want cyclic_dependency naming both", line.Path, line.Reason, line.Detail)
		}
	}
	if writes := f.writes(); len(writes) != 0 {
		t.Fatalf("a refused cycle sent %v", writes)
	}
}

// TestAddressResolvesInMemoryToARowThisRunMade: a row the run recorded answers
// an address with no claim read at all — the resolution needs no credential.
func TestAddressResolvesInMemoryToARowThisRunMade(t *testing.T) {
	f, dir := pipelineTree(t)
	_ = f
	report := filepath.Join(dir, "pipelines/a-report")
	m := loadAnyManifest(t, report)
	overlay := newAddressOverlay()
	overlay.record(devFeature, testFolderID("pipelines/b-base"), "base.sql", wfdir.ManagedTable, "table-made-this-run")
	ctx := withAddressOverlay(withAddressWalkRoot(t.Context(), dir), overlay)
	bind, errs, edges, pending := resolveAddressDeps(ctx, nil, report, m, "dev")
	if bind["base"] != "table-made-this-run" || len(errs) != 0 || len(pending) != 0 {
		t.Fatalf("bind %v errs %v pending %v", bind, errs, pending)
	}
	if len(edges) != 1 || edges[0].ID != "table-made-this-run" {
		t.Fatalf("edges = %+v", edges)
	}
	// A kind the declaration does not hold is refused, as a claim's is.
	overlay.record(devFeature, testFolderID("pipelines/b-base"), "base.sql", wfdir.ManagedWorkflow, "workflow-x")
	if _, errs, _, _ := resolveAddressDeps(ctx, nil, report, m, "dev"); errs["base"].Reason != syncReasonAddressUnresolved {
		t.Fatalf("a row of the wrong kind resolved: %v", errs)
	}
}

// TestApplyDefersAnAddressOnlyToARowTheRunDeploys: a folder reading by address
// a row nothing in this run deploys is refused before any write, under
// --allow-create too.
func TestApplyDefersAnAddressOnlyToARowTheRunDeploys(t *testing.T) {
	f, dir := pipelineTree(t)
	f.resetRequests()
	report, err := applyStack(t, filepath.Join(dir, "pipelines/a-report"), "dev", "--allow-create")
	if err == nil || report.Folders[0].Reason != syncReasonAddressUnresolved || !strings.Contains(report.Folders[0].Detail, "does not deploy") {
		t.Fatalf("err %v\n%s", err, describeApply(report))
	}
	if writes := f.writes(); len(writes) != 0 {
		t.Fatalf("sent %v", writes)
	}
}

// testFolderID is a stable folder id (a UUID's shape) for a folder path.
func testFolderID(rel string) string {
	h := hex.EncodeToString(func() []byte { s := sha256.Sum256([]byte(rel)); return s[:] }())
	return h[0:8] + "-" + h[8:12] + "-4" + h[13:16] + "-8" + h[17:20] + "-" + h[20:32]
}

// TestAnUnboundOutputReadByRefResolvesToTheRunsOutput: an `output` this stack
// does not bind, read with `{{ ref() }}`, goes out as the table the workflow's
// runs made under that name — so the server stores the id it would have
// resolved the name to anyway, and the folder reads it back as the name: no
// permanent drift once the output exists.
func TestAnUnboundOutputReadByRefResolvesToTheRunsOutput(t *testing.T) {
	f := importFixture(t)
	dir, _ := runImport(t, f)
	if err := os.Mkdir(filepath.Join(dir, ".git"), 0o755); err != nil {
		t.Fatal(err)
	}
	// table-daily is the workflow's own output, as its runs made it.
	f.pipe.outputOf["table-daily"] = [2]string{"workflow-report", "orders_daily"}
	root := filepath.Join(dir, "workflows/sales-report")
	m := loadAnyManifest(t, root)
	delete(m.Stacks["main"].Bind, "orders_daily")
	if err := wfdir.SaveManifest(root, m); err != nil {
		t.Fatal(err)
	}
	writeLocal(t, root, "main.py", readFile(t, filepath.Join(root, "main.py"))+
		"prev = tools.query(\"SELECT * FROM {{ ref('orders_daily') }}\")\n")
	if out, err := runCLI(t, root, "wf", "push"); err != nil {
		t.Fatalf("push: %v\n%s", err, out)
	}
	sent := f.wf.FileContents(f.wf.draftOf["workflow-report"])["main.py"]
	if !strings.Contains(sent, "{{ ref('table-daily') }}") || strings.Contains(sent, "{{ ref('orders_daily') }}") {
		t.Fatalf("the read of the unbound output did not go out as the output's id:\n%s", sent)
	}
	status, err := runCLI(t, root, "wf", "status", "--json")
	var st struct {
		Local  struct{ Modified []string } `json:"local"`
		Remote struct {
			Drift struct{ Modified []string } `json:"drift"`
		} `json:"remote"`
	}
	if jerr := json.Unmarshal([]byte(status), &st); err != nil || jerr != nil || len(st.Local.Modified) != 0 || len(st.Remote.Drift.Modified) != 0 {
		t.Fatalf("the folder reads as drifted after the push: %v %v\n%s", err, jerr, status)
	}
}

// TestAnUnboundOutputReadByRefResolvesWithNoLock is the same promise for a
// checkout with no ronja.lock.json — a CI runner, or a stack a CI run stood up
// — where only the server's claims name the workflow: the output still goes
// out as its id, by a single-folder push and by `sync apply`, and the folder
// does not read as drifted afterwards.
func TestAnUnboundOutputReadByRefResolvesWithNoLock(t *testing.T) {
	f := importFixture(t)
	dir, _ := runImport(t, f)
	if err := os.Mkdir(filepath.Join(dir, ".git"), 0o755); err != nil {
		t.Fatal(err)
	}
	f.pipe.outputOf["table-daily"] = [2]string{"workflow-report", "orders_daily"}
	root := filepath.Join(dir, "workflows/sales-report")
	m := loadAnyManifest(t, root)
	delete(m.Stacks["main"].Bind, "orders_daily")
	if err := wfdir.SaveManifest(root, m); err != nil {
		t.Fatal(err)
	}
	writeLocal(t, root, "main.py", readFile(t, filepath.Join(root, "main.py"))+
		"prev = tools.query(\"SELECT * FROM {{ ref('orders_daily') }}\")\n")
	dropLock := func() {
		t.Helper()
		if err := os.Remove(filepath.Join(root, wfdir.LockName)); err != nil && !os.IsNotExist(err) {
			t.Fatal(err)
		}
		if err := os.RemoveAll(filepath.Join(root, ".ronja")); err != nil {
			t.Fatal(err)
		}
	}
	dropLock()
	if out, err := runCLI(t, root, "wf", "push"); err != nil {
		t.Fatalf("push: %v\n%s", err, out)
	}
	sent := f.wf.FileContents(f.wf.draftOf["workflow-report"])["main.py"]
	if !strings.Contains(sent, "{{ ref('table-daily') }}") || strings.Contains(sent, "{{ ref('orders_daily') }}") {
		t.Fatalf("with no lock, the read of the unbound output did not go out as the output's id:\n%s", sent)
	}
	if out, err := runCLI(t, root, "wf", "discard", "--yes"); err != nil {
		t.Fatalf("discard: %v\n%s", err, out)
	}

	dropLock()
	if report, err := applyStack(t, dir, "main"); err != nil {
		t.Fatalf("apply with no lock: %v\n%s", err, describeApply(report))
	}
	live := f.wf.FileContents("workflow-report")["main.py"]
	if !strings.Contains(live, "{{ ref('table-daily') }}") {
		t.Fatalf("with no lock, apply published the output's name rather than its id:\n%s", live)
	}
	dropLock()
	f.resetRequests()
	if report, err := applyStack(t, dir, "main"); err != nil {
		t.Fatalf("second apply with no lock: %v\n%s", err, describeApply(report))
	}
	if writes := workflowWrites(f); len(writes) != 0 {
		t.Fatalf("the second lockless apply of an unchanged tree sent %v — the folder reads as drifted", writes)
	}
}

// TestAnUnboundOutputWhoseLookupFailsIsRefused: when listing the feature's
// tables fails, the unbound output the code reads by ref is NOT sent as its
// name — the server would store an id, and the folder would read as drifted
// from then on — but refused lookup_failed before anything is written, by a
// single-folder push and by `sync apply`.
func TestAnUnboundOutputWhoseLookupFailsIsRefused(t *testing.T) {
	f := importFixture(t)
	dir, _ := runImport(t, f)
	if err := os.Mkdir(filepath.Join(dir, ".git"), 0o755); err != nil {
		t.Fatal(err)
	}
	f.pipe.outputOf["table-daily"] = [2]string{"workflow-report", "orders_daily"}
	root := filepath.Join(dir, "workflows/sales-report")
	m := loadAnyManifest(t, root)
	delete(m.Stacks["main"].Bind, "orders_daily")
	if err := wfdir.SaveManifest(root, m); err != nil {
		t.Fatal(err)
	}
	writeLocal(t, root, "main.py", readFile(t, filepath.Join(root, "main.py"))+
		"prev = tools.query(\"SELECT * FROM {{ ref('orders_daily') }}\")\n")
	f.fail["GET /api/v2/feature/model"] = 500
	f.resetRequests()
	out, err := runCLI(t, root, "wf", "push")
	if err == nil || !strings.Contains(err.Error(), `output "orders_daily" is read with {{ ref() }}`) || !strings.Contains(err.Error(), "nothing was written") {
		t.Fatalf("a failed output lookup did not refuse the push: %v\n%s", err, out)
	}
	if writes := workflowWrites(f); len(writes) != 0 {
		t.Fatalf("the refused push sent %v", writes)
	}
	report, err := applyStack(t, dir, "main")
	if err == nil {
		t.Fatalf("apply deployed it:\n%s", describeApply(report))
	}
	for _, line := range report.Folders {
		if line.Path == "workflows/sales-report" && line.Reason != syncReasonLookupFailed {
			t.Fatalf("apply: %s / %s — %s", line.Verdict, line.Reason, line.Detail)
		}
	}
	if writes := workflowWrites(f); len(writes) != 0 {
		t.Fatalf("apply sent %v", writes)
	}
}

const devFeature = "collection-dev"

// applyStack runs `sync apply --stack <stack> --allow-create --json` and
// decodes the report.
func applyStack(t *testing.T, dir, stack string, extra ...string) (syncApplyReport, error) {
	t.Helper()
	out, err := runCLI(t, dir, append([]string{"sync", "apply", "--stack", stack, "--json"}, extra...)...)
	var report syncApplyReport
	if jerr := json.Unmarshal([]byte(out), &report); jerr != nil {
		t.Fatalf("decode the apply report: %v (err %v)\n%s", jerr, err, out)
	}
	return report, err
}

func describeApply(r syncApplyReport) string {
	b, _ := json.MarshalIndent(r, "", "  ")
	return string(b)
}

// devRows collects what the run made in the dev feature.
type devRowsSeen struct {
	tables      map[string]*api.Table // by name
	workflows   []*api.Workflow
	apps        []*api.DataApp
	automations map[string]*api.Automation // by name
}

func devRows(f *fakeImportInstance, feature string) devRowsSeen {
	out := devRowsSeen{tables: map[string]*api.Table{}, automations: map[string]*api.Automation{}}
	for _, row := range f.pipe.tables {
		if row.FeatureID == feature && row.ParentModelID == "" {
			out.tables[row.Name] = row
		}
	}
	for _, wf := range f.wf.workflows {
		if wf.FeatureID == feature {
			out.workflows = append(out.workflows, wf)
		}
	}
	for _, app := range f.app.apps {
		if app.FeatureID == feature {
			out.apps = append(out.apps, app)
		}
	}
	for _, a := range f.auto.rows {
		if a.FeatureID == feature {
			out.automations[a.Name] = a
		}
	}
	return out
}

func keysOf[V any](m map[string]V) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

// dropLocalState removes every folder's .ronja/ — what a fresh checkout of the
// repository holds.
func dropLocalState(t *testing.T, dir string) {
	t.Helper()
	var states []string
	_ = filepath.WalkDir(dir, func(path string, d os.DirEntry, err error) error {
		if err == nil && d.IsDir() && d.Name() == wfdir.StateDirName {
			states = append(states, path)
			return filepath.SkipDir
		}
		return err
	})
	for _, s := range states {
		if err := os.RemoveAll(s); err != nil {
			t.Fatal(err)
		}
	}
}

// TestPassOneCreatesNothingAFolderWouldBeRefusedFor: pass one creates a
// workflow early only when another workflow names it, and only when what it
// waits on exists by then. Here `a` reads the base pipeline's table and runs
// `b`, `b` runs `a`; the base pipeline's create fails, so `a` — waiting on a
// table that is not there — is not created in pass one, its own push refuses
// it before any write, and `b`, whose run target never appeared, is refused
// too, as is the chain x → y → b. No empty, claimed row is left behind.
func TestPassOneCreatesNothingAFolderWouldBeRefusedFor(t *testing.T) {
	f, dir := pipelineTree(t)
	adoptedFolder(t, f, dir, "workflows/a", &wfdir.Manifest{Kind: wfdir.KindWorkflow, Title: "Flow a", Entrypoint: "main.py",
		Dependencies: map[string]wfdir.Dependency{
			"next": {Kind: markers.KindWorkflow, From: "../b"},
			"base": {Kind: markers.KindTable, From: "../../pipelines/b-base", Address: "base.sql"},
		}},
		nil, map[string]string{"main.py": "tools.query(\"SELECT * FROM {{ ref('base') }}\")\ntools.runWorkflow({{ workflow('next') }})\n"})
	adoptedFolder(t, f, dir, "workflows/b", &wfdir.Manifest{Kind: wfdir.KindWorkflow, Title: "Flow b", Entrypoint: "main.py",
		Dependencies: map[string]wfdir.Dependency{"next": {Kind: markers.KindWorkflow, From: "../a"}}},
		nil, map[string]string{"main.py": "tools.runWorkflow({{ workflow('next') }})\n"})
	// And a chain onto b: x runs y, y runs b — neither may be made early for
	// a b that is itself waiting on a.
	for _, pair := range [][2]string{{"x", "y"}, {"y", "b"}} {
		adoptedFolder(t, f, dir, "workflows/"+pair[0], &wfdir.Manifest{Kind: wfdir.KindWorkflow, Title: "Flow " + pair[0], Entrypoint: "main.py",
			Dependencies: map[string]wfdir.Dependency{"next": {Kind: markers.KindWorkflow, From: "../" + pair[1]}}},
			nil, map[string]string{"main.py": "tools.runWorkflow({{ workflow('next') }})\n"})
	}
	f.pipe.failCreate = 500
	report, err := applyStack(t, dir, "dev", "--allow-create")
	if err == nil {
		t.Fatalf("applied with a failed pipeline:\n%s", describeApply(report))
	}
	if wfs := workflowsIn(f, devFeature); len(wfs) != 0 {
		t.Fatalf("pass one left %d workflow(s) behind for folders that were then refused:\n%s", len(wfs), describeApply(report))
	}
	for _, line := range report.Folders {
		if strings.HasPrefix(line.Path, "workflows/") && line.Verdict == verdictApplied {
			t.Fatalf("%s applied", line.Path)
		}
	}
}

// TestPlanGateRefusesAWorkflowPassOneWouldCreate: `b` is named by `a` by
// address, so pass one would create b's row before any code is written — and
// b's code writes a Finance table. planWriteGate refuses b at plan time, so a,
// waiting on b, is refused too, and no row is created in the dev feature.
// Without the plan-time gate pass one creates b's empty, claimed row, and only
// b's own push refuses it — the row stays behind.
func TestPlanGateRefusesAWorkflowPassOneWouldCreate(t *testing.T) {
	f, dir := orderTree(t)
	adoptedFolder(t, f, dir, "workflows/a", &wfdir.Manifest{Kind: wfdir.KindWorkflow, Title: "Flow a", Entrypoint: "main.py",
		Dependencies: map[string]wfdir.Dependency{"next": {Kind: markers.KindWorkflow, From: "../b"}}},
		nil, map[string]string{"main.py": "tools.runWorkflow({{ workflow('next') }})\n"})
	adoptedFolder(t, f, dir, "workflows/b", &wfdir.Manifest{Kind: wfdir.KindWorkflow, Title: "Flow b", Entrypoint: "main.py"},
		nil, map[string]string{"main.py": "df = tools.query(\"SELECT 1\")\ntools.appendToTable(df, \"table-ledger\")\n"})
	report, err := applyStack(t, dir, "dev", "--allow-create")
	if err == nil {
		t.Fatalf("applied a workflow writing Finance:\n%s", describeApply(report))
	}
	if wfs := workflowsIn(f, devFeature); len(wfs) != 0 {
		t.Fatalf("%d workflow row(s) left in dev for folders that were refused:\n%s", len(wfs), describeApply(report))
	}
	for _, line := range report.Folders {
		switch line.Path {
		case "workflows/b":
			if line.Verdict != verdictRefused || line.Reason != syncReasonWritesOutsideTarget || !strings.Contains(line.Detail, "table-ledger") {
				t.Fatalf("b: %s / %s — %s", line.Verdict, line.Reason, line.Detail)
			}
		case "workflows/a":
			if line.Verdict != verdictRefused || line.Reason != syncReasonAddressUnresolved {
				t.Fatalf("a: %s / %s — %s", line.Verdict, line.Reason, line.Detail)
			}
		}
	}
}
