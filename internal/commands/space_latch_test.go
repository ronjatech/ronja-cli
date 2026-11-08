package commands

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/ronjatech/ronja-cli/internal/markers"
	"github.com/ronjatech/ronja-cli/internal/wfdir"
)

// devSpaceTree is the gated tree with its pipeline and workflow folders'
// stack "main" declaring Space Dev, the Space their feature is in, and a tag
// added to the pipeline's metric aov, whose PATCH the instance answers without
// the Space echo.
func devSpaceTree(t *testing.T) (*fakeImportInstance, string) {
	t.Helper()
	f, dir := spacedTree(t)
	for _, folder := range []string{"pipelines/sales", "workflows/sales-report"} {
		root := filepath.Join(dir, folder)
		m := loadAnyManifest(t, root)
		st := m.Stacks["main"]
		st.SpaceID, st.Space = "space-dev", "Dev"
		m.Stacks["main"] = st
		if err := wfdir.SaveManifest(root, m); err != nil {
			t.Fatal(err)
		}
	}
	metric := filepath.Join(dir, "pipelines/sales/metrics/aov.json")
	body := strings.Replace(readFile(t, metric), "{", "{\n  \"tags\": [\"kpi\"],", 1)
	if err := os.WriteFile(metric, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
	f.spaceEchoMissing = map[string]bool{"PATCH /api/v2/tag/of/table/table-aov": true}
	return f, dir
}

// A Space refusal some step swallowed, here a metric's tag PATCH whose answer
// did not confirm the Space, still ends the command at exit 2 and is printed.
func TestASwallowedSpaceRefusalExitsTwo(t *testing.T) {
	_, dir := devSpaceTree(t)
	t.Chdir(filepath.Join(dir, "pipelines/sales"))
	var code int
	stderr := captureStderr(t, func() {
		stdout, restore := captureStdout(t)
		code = run([]string{"pipeline", "push"})
		restore(stdout)
	})
	if code != syncExitUnknown {
		t.Errorf("exit %d, want %d\n%s", code, syncExitUnknown, stderr)
	}
	if n := spaceErrorLines(stderr); n != 1 {
		t.Errorf("the Space refusal is printed as an error %d times, want once:\n%s", n, stderr)
	}
}

// spaceErrorLines counts the error lines of stderr that carry the refusal of
// Space Dev.
func spaceErrorLines(stderr string) int {
	n := 0
	for _, line := range strings.Split(stderr, "\n") {
		if strings.HasPrefix(line, "error: ") && strings.Contains(line, "did not confirm it acted in Space Dev (space-dev)") {
			n++
		}
	}
	return n
}

// `sync apply` reports a swallowed Space refusal on the folder that met it,
// and stops only the folders sharing that Space: a later one on it is not
// attempted, under the same reason, while folders on a stack with no Space
// deploy. The run exits 2.
func TestSyncApplyStopsOnlyTheFoldersOfAHeldSpace(t *testing.T) {
	f, dir := devSpaceTree(t)
	f.resetRequests()
	report, err := applyStack(t, dir, "main")
	if exitCodeOf(err) != syncExitUnknown {
		t.Errorf("exit %d (%v), want %d", exitCodeOf(err), err, syncExitUnknown)
	}
	met := applyFolderLine(t, &report, "pipelines/sales")
	if met.Verdict != verdictUnknown || met.Reason != syncReasonSpaceHeaderUnsupported ||
		!strings.Contains(met.Detail, "PATCH /api/v2/tag/of/table/table-aov answered HTTP 200 with no X-Ronja-Space") {
		t.Errorf("the folder that met the refusal: %s / %s — %s", met.Verdict, met.Reason, met.Detail)
	}
	stopped := applyFolderLine(t, &report, "workflows/sales-report")
	if stopped.Verdict != verdictUnknown || stopped.Reason != syncReasonSpaceHeaderUnsupported ||
		stopped.Detail != "not attempted: pipelines/sales got no confirmation of the Space, and nothing further was sent" {
		t.Errorf("the later folder in the Space: %s / %s — %s", stopped.Verdict, stopped.Reason, stopped.Detail)
	}
	for _, path := range []string{"apps/sales-dashboard", "automations/sales", "modules/sales_helpers"} {
		if line := applyFolderLine(t, &report, path); line.Verdict != verdictApplied {
			t.Errorf("%s, on a stack with no Space: %s / %s — %s", path, line.Verdict, line.Reason, line.Detail)
		}
	}
	for _, req := range f.requests {
		if strings.Contains(req, "workflow-report") {
			t.Errorf("sent for the stopped folder: %s", req)
		}
	}
}

// `sync check` reports a Space refusal its write-gate placement met as the
// folder's Space refusal, not as a lookup that failed.
func TestSyncCheckReportsAGateSpaceRefusalByItsReason(t *testing.T) {
	f, dir := devSpaceTree(t)
	f.spaceEchoMissing = map[string]bool{"POST /api/v2/workflow/validate": true}
	out, err := runCLI(t, dir, "sync", "check", "--stack", "main", "--json")
	var report syncCheckReport
	if jerr := json.Unmarshal([]byte(out), &report); jerr != nil {
		t.Fatalf("decode %q: %v (err %v)", out, jerr, err)
	}
	if exitCodeOf(err) != syncExitUnknown {
		t.Errorf("exit %d (%v), want %d", exitCodeOf(err), err, syncExitUnknown)
	}
	for _, line := range report.Folders {
		if line.Path != "workflows/sales-report" {
			continue
		}
		if line.Verdict != verdictUnknown || line.Reason != syncReasonSpaceHeaderUnsupported || !strings.Contains(line.Detail, "POST /api/v2/workflow/validate") {
			t.Errorf("sync check: %s / %s — %s", line.Verdict, line.Reason, line.Detail)
		}
		return
	}
	t.Fatalf("no workflows/sales-report line:\n%s", out)
}

// devOrderTree is orderTree with its dev feature in Space Dev: workflows/b,
// and workflows/a naming it by address, so pass one creates b's row before
// any code is written, and an app that runs after every workflow. Each
// folder's stack "dev" declares Space Dev.
func devOrderTree(t *testing.T) (*fakeImportInstance, string) {
	t.Helper()
	f, dir := orderTree(t)
	f.app.privilegeLevel = adminPrivilegeLevel
	f.app.featureScope[devFeature] = "organization"
	f.featureSpace = map[string]string{devFeature: "space-dev"}
	f.wf.spaceNames = map[string]string{"space-dev": "Dev", "space-main": "Main"}
	f.wf.defaultSpace = "space-main"
	roots := []string{
		adoptedFolder(t, f, dir, "workflows/a", &wfdir.Manifest{Kind: wfdir.KindWorkflow, Title: "Flow a", Entrypoint: "main.py",
			Dependencies: map[string]wfdir.Dependency{"next": {Kind: markers.KindWorkflow, From: "../b"}}},
			nil, map[string]string{"main.py": "tools.runWorkflow({{ workflow('next') }})\n"}),
		adoptedFolder(t, f, dir, "workflows/b", &wfdir.Manifest{Kind: wfdir.KindWorkflow, Title: "Flow b", Entrypoint: "main.py"},
			nil, map[string]string{"main.py": "print('b')\n"}),
		adoptedFolder(t, f, dir, "apps/board", &wfdir.Manifest{Kind: wfdir.KindDataApp, Title: "Board", Entrypoint: wfdir.DataAppKind.DefaultEntrypoint},
			nil, map[string]string{wfdir.DataAppKind.DefaultEntrypoint: "export default function App() { return null }\n"}),
	}
	for _, root := range roots {
		m := loadAnyManifest(t, root)
		st := m.Stacks["dev"]
		st.SpaceID, st.Space = "space-dev", "Dev"
		m.Stacks["dev"] = st
		if err := wfdir.SaveManifest(root, m); err != nil {
			t.Fatal(err)
		}
	}
	return f, dir
}

// A Space refusal met by pass one's create, whose failure clears the folder
// from the run, is still reported on that folder, and the later folders on
// the Space name it.
func TestSyncApplyReportsAPassOneSpaceRefusalOnItsFolder(t *testing.T) {
	f, dir := devOrderTree(t)
	f.spaceEchoMissing = map[string]bool{"POST /api/v2/workflow": true}
	report, err := applyStack(t, dir, "dev", "--allow-create")
	if exitCodeOf(err) != syncExitUnknown || report.Verdict != verdictUnknown {
		t.Errorf("exit %d (%v), verdict %s, want %d and unknown", exitCodeOf(err), err, report.Verdict, syncExitUnknown)
	}
	created, wrote := -1, -1
	for i, req := range f.requests {
		if req == "POST /api/v2/workflow" && created < 0 {
			created = i
		}
		if strings.HasPrefix(req, "PUT ") && wrote < 0 {
			wrote = i
		}
	}
	if created < 0 || (wrote >= 0 && wrote < created) {
		t.Fatalf("pass one's create was not the first write (create %d, first file write %d): %v", created, wrote, f.requests)
	}
	met := applyFolderLine(t, &report, "workflows/b")
	if met.Verdict != verdictUnknown || met.Reason != syncReasonSpaceHeaderUnsupported ||
		!strings.Contains(met.Detail, "POST /api/v2/workflow answered HTTP") {
		t.Errorf("the folder whose create met the refusal: %s / %s — %s", met.Verdict, met.Reason, met.Detail)
	}
	for _, path := range []string{"workflows/a", "apps/board"} {
		line := applyFolderLine(t, &report, path)
		if line.Verdict != verdictUnknown || line.Reason != syncReasonSpaceHeaderUnsupported ||
			line.Detail != "not attempted: workflows/b got no confirmation of the Space, and nothing further was sent" {
			t.Errorf("%s, later in the Space: %s / %s — %s", path, line.Verdict, line.Reason, line.Detail)
		}
	}
}

// A Space refusal a folder's line already carries is printed once, in that
// line, and not again as the run's error.
func TestSyncApplyPrintsAFoldersSpaceRefusalOnce(t *testing.T) {
	f, dir := devOrderTree(t)
	f.spaceEchoMissing = map[string]bool{"POST /api/v2/workflow": true}
	t.Chdir(dir)
	var code int
	var out string
	stderr := captureStderr(t, func() {
		stdout, restore := captureStdout(t)
		code = run([]string{"sync", "apply", "--stack", "dev", "--allow-create"})
		out = restore(stdout)
	})
	if code != syncExitUnknown {
		t.Errorf("exit %d, want %d", code, syncExitUnknown)
	}
	if n := strings.Count(out+stderr, "did not confirm it acted in Space Dev (space-dev)"); n != 1 {
		t.Errorf("the Space refusal is printed %d times, want once:\n%s\n%s", n, out, stderr)
	}
}

// `sync check` reports a Space refusal on the folder whose gate met it, and a
// later folder on the Space as not attempted, naming that folder, rather than
// as a second copy of the refusal.
func TestSyncCheckReportsALaterFolderOfAHeldSpaceAsNotAttempted(t *testing.T) {
	f, dir := devOrderTree(t)
	f.spaceEchoMissing = map[string]bool{"POST /api/v2/workflow/validate": true}
	out, err := runCLI(t, dir, "sync", "check", "--stack", "dev", "--json")
	var report syncCheckReport
	if jerr := json.Unmarshal([]byte(out), &report); jerr != nil {
		t.Fatalf("decode %q: %v (err %v)", out, jerr, err)
	}
	if exitCodeOf(err) != syncExitUnknown {
		t.Errorf("exit %d (%v), want %d", exitCodeOf(err), err, syncExitUnknown)
	}
	lines := map[string]syncCheckFolderReport{}
	for _, line := range report.Folders {
		lines[line.Path] = line
	}
	if met := lines["workflows/a"]; met.Verdict != verdictUnknown || met.Reason != syncReasonSpaceHeaderUnsupported ||
		!strings.Contains(met.Detail, "POST /api/v2/workflow/validate") {
		t.Errorf("workflows/a: %s / %s — %s", met.Verdict, met.Reason, met.Detail)
	}
	if later := lines["workflows/b"]; later.Verdict != verdictUnknown || later.Reason != syncReasonSpaceHeaderUnsupported ||
		later.Detail != "not attempted: workflows/a got no confirmation of the Space, and nothing further was sent" {
		t.Errorf("workflows/b: %s / %s — %s", later.Verdict, later.Reason, later.Detail)
	}
}
