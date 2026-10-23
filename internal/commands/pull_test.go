package commands

import (
	"context"
	"encoding/json"
	"errors"
	"maps"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/ronjatech/ronja-cli/internal/api"
	"github.com/ronjatech/ronja-cli/internal/wfdir"
)

// `ronja <kind> pull`: a classic edit of a managed row is
// absorbed into the repository with one command, and the next apply writes
// nothing. Every assertion here is on what the FAKE SERVER holds and was sent
// (fakeManaged for the 2a state, the kind fakes for the rows, both serving the
// 5a `managedBy`), and on the folder on disk.

// inCleanGit stands every folder in a clean git work tree, so the dirty-folder
// refusal is decided by the test, not by the temp directory being outside git.
func inCleanGit(t *testing.T) {
	t.Helper()
	inGit(t, nil, true)
}

func inGit(t *testing.T, changed []string, inside bool) {
	t.Helper()
	previous, previousLost := folderUncommitted, folderUnrecoverable
	folderUncommitted = func(string) ([]string, bool, error) { return changed, inside, nil }
	folderUnrecoverable = func(string, []string) ([]string, error) { return nil, nil }
	t.Cleanup(func() { folderUncommitted, folderUnrecoverable = previous, previousLost })
}

// inRealGit makes root a git repository with `ignore` as its .gitignore and
// everything else committed, and has the pull ask git itself — no stub.
func inRealGit(t *testing.T, root, ignore string) {
	t.Helper()
	git, err := exec.LookPath("git")
	if err != nil {
		t.Skip("no git binary")
	}
	previous, previousLost := folderUncommitted, folderUnrecoverable
	folderUncommitted, folderUnrecoverable = gitFolderUncommitted, gitFolderUnrecoverable
	t.Cleanup(func() { folderUncommitted, folderUnrecoverable = previous, previousLost })
	writeLocal(t, root, ".gitignore", ignore)
	for _, args := range [][]string{{"init", "-q"}, {"add", "."}, {"commit", "-q", "-m", "c"}} {
		cmd := exec.Command(git, append([]string{"-C", root}, args...)...)
		cmd.Env = append(os.Environ(), "GIT_AUTHOR_NAME=t", "GIT_AUTHOR_EMAIL=t@t", "GIT_COMMITTER_NAME=t", "GIT_COMMITTER_EMAIL=t@t")
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v\n%s", args, err, out)
		}
	}
}

// webPublishWorkflow is a colleague's publish of wf-1 in the web app: its
// files become what mutate makes of them, and the head moves to a new version.
func webPublishWorkflow(f *fakeInstance, mutate func([]api.WorkflowFile) []api.WorkflowFile) {
	f.snapshotHead("wf-1")
	f.files["wf-1"] = mutate(slices.Clone(f.files["wf-1"]))
	f.versions["wf-1"] = append([]api.Workflow{{ID: "wfv-web-" + strconv.Itoa(len(f.versions["wf-1"]))}}, f.versions["wf-1"]...)
}

// webEditOf is a classic edit of the fixture's one row, made in the web app,
// and the bytes the folder's file holds after a pull absorbs it.
func webEditOf(t *testing.T, kind string, fx idempotenceFixture) (path, want string) {
	t.Helper()
	switch kind {
	case "workflow", "data app", "module":
		fx.draftKind.webPublish("fixed in the web app\n")
		return map[string]string{"workflow": "main.py", "data app": "App.tsx", "module": "tracker.py"}[kind], "fixed in the web app\n"
	case "pipeline":
		f := fx.pipeline
		f.mu.Lock()
		f.tables["table-orders"].Code = "SELECT * FROM raw WHERE fixed_in_the_web_app"
		f.mu.Unlock()
		return "orders.sql", "SELECT * FROM raw WHERE fixed_in_the_web_app"
	case "automation":
		f := fx.automation
		f.mu.Lock()
		f.rows["job-1"].CronExpr = "0 4 * * *"
		f.mu.Unlock()
		f.Touch("job-1", baseTime.Add(48*time.Hour))
		return "nightly.json", ""
	}
	t.Fatalf("no web edit for %q", kind)
	return "", ""
}

// pullArgsOf is `ronja <kind> pull`.
func pullArgsOf(kind string) []string {
	return []string{map[string]string{"workflow": "wf", "data app": "app", "module": "module", "pipeline": "pipeline", "automation": "automation"}[kind], "pull"}
}

func decodePullReport(t *testing.T, out string) pullReport {
	t.Helper()
	var r pullReport
	if err := json.Unmarshal([]byte(out), &r); err != nil {
		t.Fatalf("decode pull --json: %v\n%s", err, out)
	}
	return r
}

// onlyAnchorWrites reports whether every write is an anchor PUT.
func onlyAnchorWrites(writes []string) bool {
	for _, w := range writes {
		method, path, _ := strings.Cut(w, " ")
		if !anchorWriteOnly(method, strings.TrimPrefix(path, "/api/v2/")) {
			return false
		}
	}
	return true
}

// TestPullAbsorbsAWebEdit is the pull round trip end to end, per kind: a deployed adopted
// folder; a colleague's edit in the web app; the next apply refuses it as drift
// and names the pull; `pull --dry-run` writes nothing and prints the diff;
// `pull` rewrites the folder, prints the diff and sends nothing but anchors;
// and the committed result applies with NO write at all — from the same
// checkout and from a fresh one with the lock and baseline gone (two runs,
// I7/I8).
func TestPullAbsorbsAWebEdit(t *testing.T) {
	for kind, setup := range idempotenceFixtures() {
		t.Run(kind, func(t *testing.T) {
			inCleanGit(t)
			fx := setup(t)
			adoptFixture(t, fx)
			if code, line, _, out := applyOnce(t, fx); code != syncExitClean {
				t.Fatalf("first apply: exit %d %+v\n%s", code, line, out)
			}

			path, want := webEditOf(t, kind, fx)
			// From a fresh checkout's view (no local baseline), as CI runs it:
			// a pipeline folder whose file did not change since the last push
			// from THIS checkout is not even compared.
			if err := os.RemoveAll(filepath.Join(fx.root, wfdir.StateDirName)); err != nil {
				t.Fatal(err)
			}
			code, line, _, out := applyOnce(t, fx)
			if code != syncExitDrifted || line.Verdict != verdictRefused {
				t.Fatalf("a web edit did not make apply refuse drift: exit %d %+v\n%s", code, line, out)
			}
			if !strings.Contains(line.Detail, pullCommandOf(kind)) {
				t.Errorf("the drift refusal does not name %s as the remedy:\n%s", pullCommandOf(kind), line.Detail)
			}

			// --dry-run: the diff, and nothing written anywhere.
			before := snapshotFolder(t, fx.root, false)
			fx.resetRequests()
			out, err := runCLI(t, fx.root, append(pullArgsOf(kind), "--dry-run", "--json")...)
			if err != nil {
				t.Fatalf("pull --dry-run: %v\n%s", err, out)
			}
			dry := decodePullReport(t, out)
			if !dry.DryRun || dry.Anchored || len(dry.Files) == 0 || !strings.Contains(dry.Diff, "+++ b/"+path) {
				t.Fatalf("pull --dry-run reported %+v", dry)
			}
			if w := writesBesidesGate(fx.requests()); len(w) != 0 {
				t.Fatalf("pull --dry-run sent %v", w)
			}
			if after := snapshotFolder(t, fx.root, false); !maps.Equal(before, after) {
				t.Fatalf("pull --dry-run changed the folder")
			}

			// The pull.
			fx.resetRequests()
			out, err = runCLI(t, fx.root, append(pullArgsOf(kind), "--json")...)
			if err != nil {
				t.Fatalf("pull: %v\n%s", err, out)
			}
			report := decodePullReport(t, out)
			if !report.Anchored || report.Diff != dry.Diff {
				t.Fatalf("pull reported %+v\n(dry run's diff: %s)", report, dry.Diff)
			}
			if w := writesBesidesGate(fx.requests()); !onlyAnchorWrites(w) {
				t.Fatalf("pull wrote more than anchors: %v", w)
			}
			got := readFile(t, fx.root, path)
			if want != "" && got != want {
				t.Fatalf("%s after pull = %q, want %q", path, got, want)
			}
			if kind == "automation" && !strings.Contains(got, `"0 4 * * *"`) {
				t.Fatalf("%s after pull = %s", path, got)
			}
			if len(report.Pending) != 0 {
				t.Fatalf("a plain web edit left something pending: %v", report.Pending)
			}

			// Commit it, and apply: nothing is written. Then a fresh checkout
			// of the commit, lock and baseline gone, applies with no write too.
			committed := snapshotFolder(t, fx.root, true)
			code, line, writes, out := applyOnce(t, fx)
			if code != syncExitClean || len(writes) != 0 {
				t.Fatalf("apply after the pull: exit %d %+v, writes %v\n%s", code, line, writes, out)
			}
			restoreCheckout(t, fx.root, committed)
			removeLock(t, fx.root)
			code, line, writes, out = applyOnce(t, fx)
			if code != syncExitClean || len(writes) != 0 {
				t.Fatalf("apply from a fresh checkout after the pull: exit %d %+v, writes %v\n%s", code, line, writes, out)
			}
			if fx.draftKind != nil && fx.draftKind.liveMain() != want {
				t.Fatalf("the web edit was overwritten: %q", fx.draftKind.liveMain())
			}
		})
	}
}

// TestPullRefusesAFolderThatIsNotAdopted: a classic folder (no folder id) is
// refused, naming both ways forward.
func TestPullRefusesAFolderThatIsNotAdopted(t *testing.T) {
	for kind, setup := range idempotenceFixtures() {
		t.Run(kind, func(t *testing.T) {
			inCleanGit(t)
			fx := setup(t)
			before := snapshotFolder(t, fx.root, false)
			fx.resetRequests()
			_, err := runCLI(t, fx.root, pullArgsOf(kind)...)
			if err == nil || !strings.Contains(err.Error(), "no folder id") ||
				!strings.Contains(err.Error(), "ronja sync adopt") || !strings.Contains(err.Error(), "clone") {
				t.Fatalf("pull of a classic folder: %v", err)
			}
			if w := writesBesidesGate(fx.requests()); len(w) != 0 {
				t.Fatalf("a refused pull sent %v", w)
			}
			if after := snapshotFolder(t, fx.root, false); !maps.Equal(before, after) {
				t.Fatalf("a refused pull changed the folder")
			}
		})
	}
}

// TestPullRefusesUncommittedWork: a folder with uncommitted changes, and one
// outside git, are refused before anything is read or written; --force pulls
// over them; --dry-run is not refused, since it writes nothing.
func TestPullRefusesUncommittedWork(t *testing.T) {
	for _, c := range []struct {
		name    string
		changed []string
		inside  bool
		reason  string
	}{
		{"dirty", []string{"main.py"}, true, "uncommitted changes (main.py)"},
		{"outside git", nil, false, "not in a git work tree"},
	} {
		t.Run(c.name, func(t *testing.T) {
			fx := workflowIdempotenceFixture(t)
			adoptFixture(t, fx)
			fx.draftKind.webPublish("fixed in the web app\n")
			writeLocal(t, fx.root, "main.py", "work in progress\n")
			inGit(t, c.changed, c.inside)

			fx.resetRequests()
			_, err := runCLI(t, fx.root, "wf", "pull")
			if err == nil || !strings.Contains(err.Error(), c.reason) || !strings.Contains(err.Error(), "--force") {
				t.Fatalf("pull: %v", err)
			}
			if got := readFile(t, fx.root, "main.py"); got != "work in progress\n" {
				t.Fatalf("a refused pull overwrote work: %q", got)
			}
			if w := writesBesidesGate(fx.requests()); len(w) != 0 {
				t.Fatalf("a refused pull sent %v", w)
			}
			if out, err := runCLI(t, fx.root, "wf", "pull", "--dry-run"); err != nil || !strings.Contains(out, "-work in progress") {
				t.Fatalf("pull --dry-run on uncommitted work: %v\n%s", err, out)
			}
			if out, err := runCLI(t, fx.root, "wf", "pull", "--force"); err != nil {
				t.Fatalf("pull --force: %v\n%s", err, out)
			}
			if got := readFile(t, fx.root, "main.py"); got != "fixed in the web app\n" {
				t.Fatalf("pull --force did not pull: %q", got)
			}
		})
	}
}

// TestPullRefusesAClaimThatIsNotOK: the stack's claim in a refusing state is
// refused with the 2b outcome a deploy names, before anything is written.
func TestPullRefusesAClaimThatIsNotOK(t *testing.T) {
	for _, c := range []struct {
		state string
		stage func(m *fakeManaged, id string)
	}{
		{api.ManagedStateTrashed, func(m *fakeManaged, id string) { m.trashed[id] = true }},
		{api.ManagedStateLost, func(m *fakeManaged, id string) { m.gone[id] = true }},
		{api.ManagedStateNotReadable, func(m *fakeManaged, id string) { m.unreadable[id] = true }},
		{api.ManagedStateMovedOut, func(m *fakeManaged, id string) { m.moveRow("workflow", id, "feat-elsewhere") }},
	} {
		t.Run(c.state, func(t *testing.T) {
			inCleanGit(t)
			fx := workflowIdempotenceFixture(t)
			adoptFixture(t, fx)
			m := managedFakeOf(fx)
			c.stage(m, "wf-1")
			before := snapshotFolder(t, fx.root, false)
			fx.resetRequests()
			_, err := runCLI(t, fx.root, "wf", "pull")
			r, ok := asManagedRefusal(err)
			if !ok || r.Reason != c.state {
				t.Fatalf("pull of a %s claim: %v", c.state, err)
			}
			if w := writesBesidesGate(fx.requests()); len(w) != 0 {
				t.Fatalf("a refused pull sent %v", w)
			}
			if after := snapshotFolder(t, fx.root, false); !maps.Equal(before, after) {
				t.Fatalf("a refused pull changed the folder")
			}
		})
	}
}

// TestPullWritesAnUnaliasedReferenceAsALiteral: a live reference no alias of
// the folder names is written as the literal id and reported by line — and
// ronja.json's dependencies and binds are not touched, so nothing is silently
// re-bound. An id an alias names is written as the alias.
func TestPullWritesAnUnaliasedReferenceAsALiteral(t *testing.T) {
	inCleanGit(t)
	fx := workflowIdempotenceFixture(t)
	m := loadAnyManifest(t, fx.root)
	m.Dependencies = map[string]wfdir.Dependency{"orders": {Kind: "table"}}
	st := m.Stacks["prod"]
	st.Bind = map[string]string{"orders": "table-orders"}
	m.Stacks["prod"] = st
	if err := wfdir.SaveManifest(fx.root, m); err != nil {
		t.Fatal(err)
	}
	adoptFixture(t, fx)
	manifestBefore := readFile(t, fx.root, wfdir.ManifestName)
	fx.draftKind.webPublish("import x\n" +
		"a = {{ ref('table-orders') }}\n" +
		"b = {{ ref('table-unbound') }}\n")
	out, err := runCLI(t, fx.root, "wf", "pull", "--json")
	if err != nil {
		t.Fatalf("pull: %v\n%s", err, out)
	}
	r := decodePullReport(t, out)
	if got := readFile(t, fx.root, "main.py"); !strings.Contains(got, "{{ ref('orders') }}") || !strings.Contains(got, "{{ ref('table-unbound') }}") {
		t.Fatalf("main.py after pull:\n%s", got)
	}
	if len(r.Literals) != 1 || r.Literals[0] != (pullLiteral{Path: "main.py", Line: 3, Kind: "table", ID: "table-unbound"}) {
		t.Fatalf("literals = %+v", r.Literals)
	}
	if after := readFile(t, fx.root, wfdir.ManifestName); after != manifestBefore {
		t.Fatalf("pull rewrote ronja.json:\nbefore %s\nafter  %s", manifestBefore, after)
	}
	human, err := runCLI(t, fx.root, "wf", "pull", "--dry-run")
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(human, "literal") {
		t.Fatalf("a second pull, with nothing new, reported literals again:\n%s", human)
	}
}

// TestPullLeavesTheOpenDraftAlone: pull reads LIVE, never the author's draft,
// and leaves the draft — its files and the drift guard over it — as it is.
func TestPullLeavesTheOpenDraftAlone(t *testing.T) {
	inCleanGit(t)
	fx := workflowIdempotenceFixture(t)
	adoptFixture(t, fx)
	f := fx.wf
	f.AddDraft("wf-1", "wf-1-draft", api.WorkflowFile{Path: "main.py", Content: "my unpublished draft\n"},
		api.WorkflowFile{Path: "helper.py", Content: "def main():\n    pass\n"})
	fx.draftKind.webPublish("fixed in the web app\n")
	out, err := runCLI(t, fx.root, "wf", "pull", "--json")
	if err != nil {
		t.Fatalf("pull: %v\n%s", err, out)
	}
	r := decodePullReport(t, out)
	if got := readFile(t, fx.root, "main.py"); got != "fixed in the web app\n" {
		t.Fatalf("pull did not read live: %q", got)
	}
	if got := f.FileContents("wf-1-draft")["main.py"]; got != "my unpublished draft\n" {
		t.Fatalf("pull touched the draft: %q", got)
	}
	if !slices.ContainsFunc(r.Notes, func(n string) bool { return strings.Contains(n, "open draft") }) {
		t.Fatalf("pull said nothing of the open draft: %+v", r.Notes)
	}
}

// TestPullRefusesPendingAddressDeclarations: a `moved` the server has not
// applied is refused — the folder and the server do not yet agree which file
// holds which row.
func TestPullRefusesPendingAddressDeclarations(t *testing.T) {
	inCleanGit(t)
	fx := pipelineIdempotenceFixture(t)
	adoptFixture(t, fx)
	for _, p := range []string{"orders.sql", "checks/orders.json"} {
		to := strings.Replace(p, "orders", "orders_v2", 1)
		if err := os.Rename(filepath.Join(fx.root, p), filepath.Join(fx.root, to)); err != nil {
			t.Fatal(err)
		}
	}
	m := loadAnyManifest(t, fx.root)
	m.Moved = []wfdir.MovedEntry{{From: "orders.sql", To: "orders_v2.sql"}}
	if m.FormatVersion < 5 {
		m.FormatVersion = 5
	}
	if err := wfdir.SaveManifest(fx.root, m); err != nil {
		t.Fatal(err)
	}
	_, err := runCLI(t, fx.root, "pipeline", "pull")
	if r, ok := asManagedRefusal(err); !ok || r.Reason != pullReasonPending || !strings.Contains(r.Detail, "orders.sql moved to orders_v2.sql") {
		t.Fatalf("pull with a pending move: %v", err)
	}
}

// TestGitFolderUncommittedReadsTheFolderOnly: the dirty check is git's own
// porcelain, scoped to the folder — a change elsewhere in the repository and
// the folder's lock file are not uncommitted work in it; a modified or an
// untracked file in it is.
func TestGitFolderUncommittedReadsTheFolderOnly(t *testing.T) {
	git, err := exec.LookPath("git")
	if err != nil {
		t.Skip("no git binary")
	}
	repo := t.TempDir()
	run := func(args ...string) {
		t.Helper()
		cmd := exec.Command(git, append([]string{"-C", repo}, args...)...)
		cmd.Env = append(os.Environ(), "GIT_AUTHOR_NAME=t", "GIT_AUTHOR_EMAIL=t@t", "GIT_COMMITTER_NAME=t", "GIT_COMMITTER_EMAIL=t@t")
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v\n%s", args, err, out)
		}
	}
	run("init", "-q")
	folder := filepath.Join(repo, "wf", "monthly")
	writeLocal(t, folder, "main.py", "a\n")
	writeLocal(t, folder, wfdir.LockName, "{}\n")
	writeLocal(t, repo, "elsewhere.txt", "x\n")
	run("add", ".")
	run("commit", "-q", "-m", "c")

	if changed, inside, err := gitFolderUncommitted(folder); err != nil || !inside || len(changed) != 0 {
		t.Fatalf("a clean folder: %v %v %v", changed, inside, err)
	}
	writeLocal(t, repo, "elsewhere.txt", "y\n")
	writeLocal(t, folder, wfdir.LockName, "{\"stacks\":{}}\n")
	writeLocal(t, folder, ".ronja/state.json", "{}\n")
	if changed, _, _ := gitFolderUncommitted(folder); len(changed) != 0 {
		t.Fatalf("a change outside the folder, the lock or .ronja/ read as uncommitted work: %v", changed)
	}
	writeLocal(t, folder, "main.py", "b\n")
	writeLocal(t, folder, "new.py", "c\n")
	if changed, _, _ := gitFolderUncommitted(folder); !slices.Equal(changed, []string{"main.py", "new.py"}) {
		t.Fatalf("changed = %v, want main.py and new.py", changed)
	}
	if _, inside, _ := gitFolderUncommitted(t.TempDir()); inside {
		t.Fatalf("a directory outside git read as inside one")
	}
}

// pullCycle is the G3 loop on one fixture after `prepare` and a clean first
// apply: `edit` changes the server, the next apply (fresh checkout) refuses
// it, a pull sends nothing but anchors, and then — after the deploys the pull
// reported as pending, if any — an apply sends no write, from the same
// checkout and from a fresh one.
func pullCycle(t *testing.T, fx idempotenceFixture, pull []string, prepare func(t *testing.T), edit func(t *testing.T), wantPending bool) pullReport {
	t.Helper()
	inCleanGit(t)
	if prepare != nil {
		prepare(t)
	}
	adoptFixture(t, fx)
	if code, line, _, out := applyOnce(t, fx); code != syncExitClean {
		t.Fatalf("first apply: exit %d %+v\n%s", code, line, out)
	}
	if code, line, writes, out := applyOnce(t, fx); code != syncExitClean || len(writes) != 0 {
		t.Fatalf("the fixture is not settled before the edit: exit %d %+v %v\n%s", code, line, writes, out)
	}
	edit(t)
	if err := os.RemoveAll(filepath.Join(fx.root, wfdir.StateDirName)); err != nil {
		t.Fatal(err)
	}
	if code, line, _, out := applyOnce(t, fx); code != syncExitDrifted {
		t.Fatalf("the edit did not make apply refuse drift: exit %d %+v\n%s", code, line, out)
	}
	fx.resetRequests()
	out, err := runCLI(t, fx.root, append(pull, "--json")...)
	if err != nil {
		t.Fatalf("pull: %v\n%s", err, out)
	}
	report := decodePullReport(t, out)
	if !report.Anchored || len(report.Files) == 0 {
		t.Fatalf("pull reported %+v", report)
	}
	if w := writesBesidesGate(fx.requests()); !onlyAnchorWrites(w) {
		t.Fatalf("pull wrote more than anchors: %v", w)
	}
	if (len(report.Pending) > 0) != wantPending {
		t.Fatalf("pending = %v, want pending: %v", report.Pending, wantPending)
	}
	if wantPending {
		if code, line, _, out := applyOnce(t, fx); code != syncExitClean {
			t.Fatalf("the apply that deploys what the pull left pending: exit %d %+v\n%s", code, line, out)
		}
	}
	committed := snapshotFolder(t, fx.root, true)
	if code, line, writes, out := applyOnce(t, fx); code != syncExitClean || len(writes) != 0 {
		t.Fatalf("apply after the pull: exit %d %+v, writes %v\n%s", code, line, writes, out)
	}
	restoreCheckout(t, fx.root, committed)
	removeLock(t, fx.root)
	if code, line, writes, out := applyOnce(t, fx); code != syncExitClean || len(writes) != 0 {
		t.Fatalf("apply from a fresh checkout after the pull: exit %d %+v, writes %v\n%s", code, line, writes, out)
	}
	return report
}

// TestPipelinePullAbsorbsEachLeg: every attribute a pipeline folder manages —
// a metric's definition and declared name, a table's checks, a sidecar's
// prose, and a .sql header's documentation and name — edited in the web app is
// pulled back into the file that declares it, and the folder settles.
func TestPipelinePullAbsorbsEachLeg(t *testing.T) {
	t.Run("metric definition", func(t *testing.T) {
		fx := pipelineIdempotenceFixture(t)
		f := fx.pipeline
		pullCycle(t, fx, []string{"pipeline", "pull"}, nil, func(t *testing.T) {
			f.mu.Lock()
			f.tables["metric-aov"].MetricRecipe = json.RawMessage(strings.Replace(string(f.tables["metric-aov"].MetricRecipe), `"revenue / orders"`, `"revenue / nullif(orders, 0)"`, 1))
			f.mu.Unlock()
		}, false)
		if got := readFile(t, fx.root, "metrics/aov.json"); !strings.Contains(got, "nullif(orders, 0)") || !strings.Contains(got, `"reportingTimezone": "Europe/Stockholm"`) {
			t.Fatalf("metrics/aov.json after pull:\n%s", got)
		}
	})
	t.Run("metric name", func(t *testing.T) {
		fx := pipelineIdempotenceFixture(t)
		f := fx.pipeline
		pullCycle(t, fx, []string{"pipeline", "pull"}, func(t *testing.T) {
			writeLocal(t, fx.root, "metrics/aov.json", `{"recipe":`+f.RecipeOf("metric-aov")+`,"reportingTimezone":"Europe/Stockholm","name":"aov"}`)
		}, func(t *testing.T) {
			f.mu.Lock()
			f.tables["metric-aov"].Name = "Average order value"
			f.mu.Unlock()
		}, false)
		if got := readFile(t, fx.root, "metrics/aov.json"); !strings.Contains(got, `"name": "Average order value"`) {
			t.Fatalf("metrics/aov.json after pull:\n%s", got)
		}
	})
	t.Run("checks", func(t *testing.T) {
		fx := pipelineIdempotenceFixture(t)
		f := fx.pipeline
		pullCycle(t, fx, []string{"pipeline", "pull"}, nil, func(t *testing.T) {
			f.mu.Lock()
			f.checkState.rows["table-orders"][0].Expression = "count(*) > 0"
			f.mu.Unlock()
		}, false)
		if got := readFile(t, fx.root, "checks/orders.json"); !strings.Contains(got, `"count(*) > 0"`) {
			t.Fatalf("checks/orders.json after pull:\n%s", got)
		}
	})
	t.Run("docs sidecar", func(t *testing.T) {
		fx := pipelineIdempotenceFixture(t)
		f := fx.pipeline
		pullCycle(t, fx, []string{"pipeline", "pull"}, nil, func(t *testing.T) {
			f.mu.Lock()
			f.tables["table-raw"].Description = "One row per invoice line, credit notes included."
			f.mu.Unlock()
		}, false)
		if got := readFile(t, fx.root, "tables/fortnox_invoices.json"); !strings.Contains(got, "credit notes included") || !strings.Contains(got, `"amount": "line total"`) {
			t.Fatalf("the sidecar after pull:\n%s", got)
		}
	})
	t.Run("header documentation and name", func(t *testing.T) {
		fx := pipelineIdempotenceFixture(t)
		f := fx.pipeline
		report := pullCycle(t, fx, []string{"pipeline", "pull"}, func(t *testing.T) {
			writeLocal(t, fx.root, "orders.sql", "-- @table One row per order.\n-- @name orders\nSELECT * FROM raw")
		}, func(t *testing.T) {
			f.mu.Lock()
			f.tables["table-orders"].Description = "One row per shipped order."
			f.tables["table-orders"].DescriptionSource = api.DescriptionSourceUser
			f.tables["table-orders"].Name = "shipped_orders"
			f.mu.Unlock()
		}, true)
		if got := readFile(t, fx.root, "orders.sql"); got != "-- @table One row per shipped order.\n-- @name shipped_orders\nSELECT * FROM raw" {
			t.Fatalf("orders.sql after pull:\n%s", got)
		}
		if !strings.Contains(strings.Join(report.Pending, "\n"), "orders.sql") {
			t.Fatalf("the header rewrite was not reported pending: %v", report.Pending)
		}
		if got := f.CodeOf("table-orders"); !strings.HasPrefix(got, "-- @table One row per shipped order.") {
			t.Fatalf("the apply after the pull did not publish the header: %q", got)
		}
	})
}

// TestPullAbsorbsMetadata: a title changed in the web app, on each folder kind
// that manages one, is pulled into ronja.json — and nothing but the title.
func TestPullAbsorbsMetadata(t *testing.T) {
	for kind, setup := range idempotenceFixtures() {
		if kind == "pipeline" || kind == "automation" {
			continue
		}
		t.Run(kind, func(t *testing.T) {
			fx := setup(t)
			var m *wfdir.Manifest
			pullCycle(t, fx, pullArgsOf(kind), func(t *testing.T) {
				fx.draftKind.retitle(t, fx.draftKind.liveTitle())
			}, func(t *testing.T) {
				m = loadAnyManifest(t, fx.root)
				switch kind {
				case "workflow":
					fx.wf.mu.Lock()
					fx.wf.workflows["wf-1"].Title = "Renamed in the web app"
					fx.wf.mu.Unlock()
				case "module":
					fx.mod.mu.Lock()
					fx.mod.mod.rows["module-1"].Title = "Renamed in the web app"
					fx.mod.mu.Unlock()
				case "data app":
					fx.app.apps["app-1"].Name = "Renamed in the web app"
				}
			}, false)
			after := loadAnyManifest(t, fx.root)
			if after.Title != "Renamed in the web app" {
				t.Fatalf("title after pull = %q", after.Title)
			}
			after.Title = m.Title
			a, _ := json.Marshal(after)
			b, _ := json.Marshal(m)
			if string(a) != string(b) {
				t.Fatalf("pull changed more than the title:\nbefore %s\nafter  %s", b, a)
			}
		})
	}
}

// TestAutomationPullKeepsUndeclaredFieldsUndeclared: three-state survives a
// pull — a field the file does not declare stays undeclared, whatever the
// row now says.
func TestAutomationPullKeepsUndeclaredFieldsUndeclared(t *testing.T) {
	fx := automationIdempotenceFixture(t)
	f := fx.automation
	pullCycle(t, fx, []string{"automation", "pull"}, nil, func(t *testing.T) {
		f.mu.Lock()
		f.rows["job-1"].CronExpr = "0 5 * * *"
		f.rows["job-1"].Description = "added in the web app"
		f.mu.Unlock()
		f.Touch("job-1", baseTime.Add(72*time.Hour))
	}, false)
	got := readFile(t, fx.root, "nightly.json")
	if !strings.Contains(got, `"cronExpr": "0 5 * * *"`) || strings.Contains(got, "description") || strings.Contains(got, "enabled") {
		t.Fatalf("nightly.json after pull:\n%s", got)
	}
}

// TestPullPrintsTheDiff: the human report is the unified diff, then what was
// done.
func TestPullPrintsTheDiff(t *testing.T) {
	inCleanGit(t)
	fx := workflowIdempotenceFixture(t)
	adoptFixture(t, fx)
	fx.draftKind.webPublish("fixed in the web app\n")
	out, err := runCLI(t, fx.root, "wf", "pull")
	if err != nil {
		t.Fatalf("pull: %v\n%s", err, out)
	}
	for _, want := range []string{"--- a/main.py\n+++ b/main.py\n@@ -1 +1 @@\n-live\n+fixed in the web app\n", `Recorded on stack "prod"`, "commit it"} {
		if !strings.Contains(out, want) {
			t.Fatalf("the report lacks %q:\n%s", want, out)
		}
	}
	out, err = runCLI(t, fx.root, "wf", "pull")
	if err != nil || !strings.Contains(out, "Nothing to pull") {
		t.Fatalf("a second pull: %v\n%s", err, out)
	}
}

// TestAnchorWriteOnlyAdmitsTheAnchorRoutes: the pull's guard admits the two
// routes an anchor is recorded through and nothing else.
func TestAnchorWriteOnlyAdmitsTheAnchorRoutes(t *testing.T) {
	for _, c := range []struct {
		method, path string
		want         bool
	}{
		{"PUT", "managed/managed-1/applied", true},
		{"PUT", "managed/folder/applied", true},
		{"POST", "managed/claim", false},
		{"POST", "managed/move", false},
		{"DELETE", "managed/managed-1", false},
		{"PUT", "managed/a/b/applied", false},
		{"PUT", "workflow/wf-1", false},
		{"POST", "managed/managed-1/applied", false},
		// The write gate's check, which persists nothing.
		{"POST", "workflow/validate", true},
		{"POST", "workflow/wf-1/checkout", false},
		{"PUT", "workflow/validate", false},
	} {
		if got := anchorWriteOnly(c.method, c.path); got != c.want {
			t.Errorf("%s %s: %v, want %v", c.method, c.path, got, c.want)
		}
	}
}

// TestAutomationPullWritesBackAClaimedFileThatIsGone: a claimed automation
// whose file is gone from the folder is written whole, and the folder settles.
func TestAutomationPullWritesBackAClaimedFileThatIsGone(t *testing.T) {
	inCleanGit(t)
	fx := automationIdempotenceFixture(t)
	adoptFixture(t, fx)
	if code, _, _, out := applyOnce(t, fx); code != syncExitClean {
		t.Fatalf("first apply:\n%s", out)
	}
	if err := os.Remove(filepath.Join(fx.root, "nightly.json")); err != nil {
		t.Fatal(err)
	}
	if out, err := runCLI(t, fx.root, "automation", "pull"); err != nil {
		t.Fatalf("pull: %v\n%s", err, out)
	}
	got := readFile(t, fx.root, "nightly.json")
	if !strings.Contains(got, `"cronExpr": "0 2 * * *"`) || !strings.Contains(got, `"enabled": true`) {
		t.Fatalf("nightly.json after pull:\n%s", got)
	}
	if code, line, writes, out := applyOnce(t, fx); code != syncExitClean || len(writes) != 0 {
		t.Fatalf("apply after the pull: exit %d %+v %v\n%s", code, line, writes, out)
	}
}

// TestDriftRefusalNamesPullOnlyInAnAdoptedFolder: a classic folder's drift
// refusal keeps the remedies it always had — it has nothing to pull into.
func TestDriftRefusalNamesPullOnlyInAnAdoptedFolder(t *testing.T) {
	for kind, setup := range idempotenceFixtures() {
		t.Run(kind, func(t *testing.T) {
			fx := setup(t)
			if code, line, _, out := applyOnce(t, fx); code != syncExitClean {
				t.Fatalf("first apply: exit %d %+v\n%s", code, line, out)
			}
			webEditOf(t, kind, fx)
			if err := os.RemoveAll(filepath.Join(fx.root, wfdir.StateDirName)); err != nil {
				t.Fatal(err)
			}
			var line syncApplyFolderReport
			var code int
			stderr := captureStderr(t, func() { code, line, _, _ = applyOnce(t, fx) })
			if code != syncExitDrifted {
				t.Fatalf("a web edit did not make a classic apply refuse: exit %d %+v", code, line)
			}
			if strings.Contains(line.Detail+stderr, " pull`") {
				t.Fatalf("a classic folder's refusal names pull:\n%s\n%s", line.Detail, stderr)
			}
		})
	}
}

// TestPullRefusesToLoseAFileGitCannotBringBack: a file git IGNORES is never
// uncommitted work to the dirty check, and the folder's enumeration does not
// read .gitignore — so a pull that would delete or overwrite one is refused,
// naming it, and --force pulls anyway. Real git, no stub.
func TestPullRefusesToLoseAFileGitCannotBringBack(t *testing.T) {
	for _, c := range []struct {
		name string
		edit func([]api.WorkflowFile) []api.WorkflowFile
		// after is notes.py once --force pulled; nil is deleted.
		after *string
	}{
		{"delete", func(files []api.WorkflowFile) []api.WorkflowFile {
			return slices.DeleteFunc(files, func(f api.WorkflowFile) bool { return f.Path == "notes.py" })
		}, nil},
		{"overwrite", func(files []api.WorkflowFile) []api.WorkflowFile {
			for i := range files {
				if files[i].Path == "notes.py" {
					files[i].Content = "changed in the web app\n"
				}
			}
			return files
		}, ptr("changed in the web app\n")},
	} {
		t.Run(c.name, func(t *testing.T) {
			inCleanGit(t)
			fx := workflowIdempotenceFixture(t)
			adoptFixture(t, fx)
			// notes.py is deployed, and live holds it: then the author's
			// repository ignores it.
			writeLocal(t, fx.root, "notes.py", "kept out of git\n")
			if code, line, _, out := applyOnce(t, fx); code != syncExitClean {
				t.Fatalf("apply: exit %d %+v\n%s", code, line, out)
			}
			if fx.wf.FileContents("wf-1")["notes.py"] != "kept out of git\n" {
				t.Fatalf("the apply did not deploy notes.py")
			}
			webPublishWorkflow(fx.wf, c.edit)
			inRealGit(t, fx.root, "notes.py\n")

			fx.resetRequests()
			_, err := runCLI(t, fx.root, "wf", "pull")
			r, ok := asManagedRefusal(err)
			if !ok || r.Reason != pullReasonUnrecoverable || !strings.Contains(r.Detail, "notes.py (ignored by git)") || !strings.Contains(r.Detail, "--force") {
				t.Fatalf("pull over an ignored file: %v", err)
			}
			if got := readFile(t, fx.root, "notes.py"); got != "kept out of git\n" {
				t.Fatalf("a refused pull touched notes.py: %q", got)
			}
			if w := writesBesidesGate(fx.requests()); len(w) != 0 {
				t.Fatalf("a refused pull sent %v", w)
			}

			if out, err := runCLI(t, fx.root, "wf", "pull", "--force"); err != nil {
				t.Fatalf("pull --force: %v\n%s", err, out)
			}
			got := readDiskFile(fx.root, "notes.py")
			if (got == nil) != (c.after == nil) || (got != nil && *got != *c.after) {
				t.Fatalf("notes.py after pull --force = %v, want %v", got, c.after)
			}
		})
	}
}

// TestPullRefusesToReplaceAnUndeployedChange: Alice committed a fix to main.py
// and has not deployed it; Bob edited helper.py in the web app. A pull would
// make the folder say what is live — Alice's fix gone — so it is refused,
// naming main.py; --force replaces it, and the report says so.
func TestPullRefusesToReplaceAnUndeployedChange(t *testing.T) {
	inCleanGit(t)
	fx := workflowIdempotenceFixture(t)
	adoptFixture(t, fx)
	if code, line, _, out := applyOnce(t, fx); code != syncExitClean {
		t.Fatalf("first apply: exit %d %+v\n%s", code, line, out)
	}
	writeLocal(t, fx.root, "main.py", "alice's fix\n")
	webPublishWorkflow(fx.wf, func(files []api.WorkflowFile) []api.WorkflowFile {
		for i := range files {
			if files[i].Path == "helper.py" {
				files[i].Content = "def main():\n    return 'bob'\n"
			}
		}
		return files
	})

	fx.resetRequests()
	_, err := runCLI(t, fx.root, "wf", "pull")
	r, ok := asManagedRefusal(err)
	if !ok || r.Reason != pullReasonUndeployed || !strings.Contains(r.Detail, "main.py") ||
		strings.Contains(r.Detail, "helper.py") || !strings.Contains(r.Detail, "--force") {
		t.Fatalf("pull over an undeployed change: %v", err)
	}
	if got := readFile(t, fx.root, "main.py"); got != "alice's fix\n" {
		t.Fatalf("a refused pull replaced Alice's fix: %q", got)
	}
	if w := writesBesidesGate(fx.requests()); len(w) != 0 {
		t.Fatalf("a refused pull sent %v", w)
	}

	out, err := runCLI(t, fx.root, "wf", "pull", "--force", "--json")
	if err != nil {
		t.Fatalf("pull --force: %v\n%s", err, out)
	}
	report := decodePullReport(t, out)
	if !slices.Equal(report.Replaced, []string{"main.py"}) || !report.Anchored {
		t.Fatalf("pull --force reported %+v", report)
	}
	if got := readFile(t, fx.root, "main.py"); got != "live\n" {
		t.Fatalf("pull --force left main.py as %q", got)
	}
	if got := readFile(t, fx.root, "helper.py"); got != "def main():\n    return 'bob'\n" {
		t.Fatalf("pull --force did not take Bob's edit: %q", got)
	}
}

// TestPullReportsForcedReplacement: the human report of a forced pull names
// what it replaced and never says the folder's change was kept.
func TestPullReportsForcedReplacement(t *testing.T) {
	inCleanGit(t)
	fx := workflowIdempotenceFixture(t)
	adoptFixture(t, fx)
	if code, _, _, out := applyOnce(t, fx); code != syncExitClean {
		t.Fatalf("first apply:\n%s", out)
	}
	writeLocal(t, fx.root, "main.py", "alice's fix\n")
	fx.draftKind.webPublish("bob's fix\n")
	out, err := runCLI(t, fx.root, "wf", "pull", "--force")
	if err != nil {
		t.Fatalf("pull --force: %v\n%s", err, out)
	}
	for _, want := range []string{"Replaced with what is live (--force)", "    main.py\n", "not in this folder any more"} {
		if !strings.Contains(out, want) {
			t.Fatalf("the report lacks %q:\n%s", want, out)
		}
	}
	if strings.Contains(out, "Left as they are") {
		t.Fatalf("a forced pull said it left the folder's change as it is:\n%s", out)
	}
}

// TestPullLeavesALocalChangeWhenLiveHasNotMoved: a change of the folder's own,
// on a row nobody changed since the stack applied it, has nothing to absorb —
// the pull leaves it, says so, and sends nothing at all.
func TestPullLeavesALocalChangeWhenLiveHasNotMoved(t *testing.T) {
	for kind, setup := range draftKindFixtures() {
		t.Run(kind, func(t *testing.T) {
			inCleanGit(t)
			fx := setup(t)
			adoptFixture(t, fx)
			if code, line, _, out := applyOnce(t, fx); code != syncExitClean {
				t.Fatalf("first apply: exit %d %+v\n%s", code, line, out)
			}
			fx.edit(t)
			path := map[string]string{"workflow": "main.py", "data app": "App.tsx", "module": "tracker.py"}[kind]
			before := snapshotFolder(t, fx.root, false)
			fx.resetRequests()
			out, err := runCLI(t, fx.root, append(pullArgsOf(kind), "--json")...)
			if err != nil {
				t.Fatalf("pull: %v\n%s", err, out)
			}
			report := decodePullReport(t, out)
			if len(report.Files) != 0 || report.Anchored || !slices.Equal(report.Kept, []string{path}) {
				t.Fatalf("pull reported %+v", report)
			}
			if after := snapshotFolder(t, fx.root, false); !maps.Equal(before, after) {
				t.Fatalf("pull changed the folder")
			}
			if w := writesBesidesGate(fx.requests()); len(w) != 0 {
				t.Fatalf("pull sent %v", w)
			}
			if code, _, _, out := applyOnce(t, fx); code != syncExitClean || !fx.deployed() {
				t.Fatalf("the change left in the folder did not deploy: exit %d\n%s", code, out)
			}
		})
	}
}

// TestPipelinePullRefusesToReplaceAnUndeployedChange: the pipeline's unit is
// the file, against the live SQL the stack recorded. Alice's committed change
// to orders.sql with Bob's web edit of the same table is refused, naming it;
// --force replaces it. With the table unmoved, her change is left as it is
// and reported pending — while a web edit elsewhere is still pulled.
func TestPipelinePullRefusesToReplaceAnUndeployedChange(t *testing.T) {
	setup := func(t *testing.T) idempotenceFixture {
		inCleanGit(t)
		fx := pipelineIdempotenceFixture(t)
		adoptFixture(t, fx)
		if code, line, _, out := applyOnce(t, fx); code != syncExitClean {
			t.Fatalf("first apply: exit %d %+v\n%s", code, line, out)
		}
		writeLocal(t, fx.root, "orders.sql", "SELECT * FROM raw WHERE alice")
		return fx
	}
	t.Run("both moved", func(t *testing.T) {
		fx := setup(t)
		f := fx.pipeline
		f.mu.Lock()
		f.tables["table-orders"].Code = "SELECT * FROM raw WHERE bob"
		f.mu.Unlock()
		fx.resetRequests()
		_, err := runCLI(t, fx.root, "pipeline", "pull")
		r, ok := asManagedRefusal(err)
		if !ok || r.Reason != pullReasonUndeployed || !strings.Contains(r.Detail, "orders.sql") {
			t.Fatalf("pull over an undeployed change: %v", err)
		}
		if got := readFile(t, fx.root, "orders.sql"); got != "SELECT * FROM raw WHERE alice" {
			t.Fatalf("a refused pull replaced orders.sql: %q", got)
		}
		if w := writesBesidesGate(fx.requests()); len(w) != 0 {
			t.Fatalf("a refused pull sent %v", w)
		}
		out, err := runCLI(t, fx.root, "pipeline", "pull", "--force", "--json")
		if err != nil {
			t.Fatalf("pull --force: %v\n%s", err, out)
		}
		if report := decodePullReport(t, out); !slices.Equal(report.Replaced, []string{"orders.sql"}) {
			t.Fatalf("pull --force reported %+v", report)
		}
		if got := readFile(t, fx.root, "orders.sql"); got != "SELECT * FROM raw WHERE bob" {
			t.Fatalf("pull --force left orders.sql as %q", got)
		}
	})
	t.Run("live unmoved", func(t *testing.T) {
		fx := setup(t)
		f := fx.pipeline
		f.mu.Lock()
		f.tables["table-raw"].Description = "Changed in the web app."
		f.mu.Unlock()
		out, err := runCLI(t, fx.root, "pipeline", "pull", "--json")
		if err != nil {
			t.Fatalf("pull: %v\n%s", err, out)
		}
		report := decodePullReport(t, out)
		if got := readFile(t, fx.root, "orders.sql"); got != "SELECT * FROM raw WHERE alice" {
			t.Fatalf("pull replaced a change live has nothing to say about: %q", got)
		}
		if !slices.Contains(report.Kept, "orders.sql") || !strings.Contains(strings.Join(report.Pending, "\n"), "orders.sql") {
			t.Fatalf("pull did not report orders.sql as left and pending: %+v", report)
		}
		if got := readFile(t, fx.root, "tables/fortnox_invoices.json"); !strings.Contains(got, "Changed in the web app.") {
			t.Fatalf("the web edit elsewhere was not pulled:\n%s", got)
		}
		if code, _, _, out := applyOnce(t, fx); code != syncExitClean || f.CodeOf("table-orders") != "SELECT * FROM raw WHERE alice" {
			t.Fatalf("Alice's change did not deploy after the pull: exit %d\n%s", code, out)
		}
	})
}

// settledPipeline is the pipeline fixture adopted and applied once, in a clean
// git work tree.
func settledPipeline(t *testing.T) idempotenceFixture {
	t.Helper()
	inCleanGit(t)
	fx := pipelineIdempotenceFixture(t)
	adoptFixture(t, fx)
	if code, line, _, out := applyOnce(t, fx); code != syncExitClean {
		t.Fatalf("first apply: exit %d %+v\n%s", code, line, out)
	}
	return fx
}

// onNthGet runs edit before the nth GET of path is answered.
func onNthGet(f *fakePipelineInstance, path string, n int, edit func()) {
	seen := 0
	f.beforeServe = func(r *http.Request) {
		if r.Method != http.MethodGet || r.URL.Path != path {
			return
		}
		if seen++; seen == n {
			edit()
		}
	}
}

// TestPipelinePullFailsOnAnUnreadableSidecarTable: a transient 5xx on the read
// that plans a docs sidecar fails the pull before anything is written — it is
// never skipped, since the anchor step after it would read the row fine and
// acknowledge prose the folder never took in. The next apply then refuses the
// web edit as drift instead of overwriting it.
func TestPipelinePullFailsOnAnUnreadableSidecarTable(t *testing.T) {
	fx := settledPipeline(t)
	f := fx.pipeline
	f.mu.Lock()
	f.tables["table-raw"].Description = "Changed in the web app."
	f.failGetOnce["table-raw"] = http.StatusServiceUnavailable
	f.mu.Unlock()
	before := snapshotFolder(t, fx.root, false)
	fx.resetRequests()
	if out, err := runCLI(t, fx.root, "pipeline", "pull"); err == nil || !strings.Contains(err.Error(), "table-raw") {
		t.Fatalf("pull over an unreadable sidecar table: %v\n%s", err, out)
	}
	if w := writesBesidesGate(fx.requests()); len(w) != 0 {
		t.Fatalf("a failed pull sent %v", w)
	}
	if after := snapshotFolder(t, fx.root, false); !maps.Equal(before, after) {
		t.Fatalf("a failed pull changed the folder")
	}
	if code, line, _, out := applyOnce(t, fx); code != syncExitDrifted {
		t.Fatalf("the apply after a failed pull did not refuse the web edit: exit %d %+v\n%s", code, line, out)
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	if got := f.tables["table-raw"].Description; got != "Changed in the web app." {
		t.Fatalf("the web edit was overwritten: %q", got)
	}
}

// TestPipelinePullDoesNotAcknowledgeProseItDidNotTakeIn: a web edit of a
// sidecar's table landing between the pull's read and its anchor step is not
// in the folder — so the anchor records nothing for it, and the next apply
// refuses it as drift rather than overwriting it.
func TestPipelinePullDoesNotAcknowledgeProseItDidNotTakeIn(t *testing.T) {
	fx := settledPipeline(t)
	f := fx.pipeline
	f.mu.Lock()
	f.tables["table-orders"].Code = "SELECT * FROM raw WHERE fixed_in_the_web_app"
	f.mu.Unlock()
	onNthGet(f, "/api/v2/feature/model/table-raw", 2, func() {
		f.mu.Lock()
		f.tables["table-raw"].Description = "Changed in the web app, mid-pull."
		f.mu.Unlock()
	})
	out, err := runCLI(t, fx.root, "pipeline", "pull", "--json")
	if err != nil {
		t.Fatalf("pull: %v\n%s", err, out)
	}
	if report := decodePullReport(t, out); !strings.Contains(strings.Join(report.Pending, "\n"), "fortnox_invoices.json") {
		t.Fatalf("the sidecar the pull could not hold was not reported pending: %+v", report)
	}
	f.beforeServe = nil
	if code, line, _, out := applyOnce(t, fx); code != syncExitDrifted {
		t.Fatalf("the apply after the pull did not refuse the web edit: exit %d %+v\n%s", code, line, out)
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	if got := f.tables["table-raw"].Description; got != "Changed in the web app, mid-pull." {
		t.Fatalf("the web edit was overwritten: %q", got)
	}
}

// TestPipelinePullRefusesAMetricItCannotHold: a metric whose live definition
// the pulled file does not say — here, a web edit landing between the pull's
// read and its anchor step — is refused as inexpressible, and nothing is
// recorded for it: the next apply refuses the web edit as drift.
func TestPipelinePullRefusesAMetricItCannotHold(t *testing.T) {
	fx := settledPipeline(t)
	f := fx.pipeline
	f.mu.Lock()
	f.tables["table-orders"].Code = "SELECT * FROM raw WHERE fixed_in_the_web_app"
	f.mu.Unlock()
	edited := ""
	onNthGet(f, "/api/v2/feature/model/metric-aov", 2, func() {
		f.mu.Lock()
		f.tables["metric-aov"].MetricRecipe = json.RawMessage(strings.Replace(string(f.tables["metric-aov"].MetricRecipe), `"revenue / orders"`, `"revenue / nullif(orders, 0)"`, 1))
		edited = string(f.tables["metric-aov"].MetricRecipe)
		f.mu.Unlock()
	})
	_, err := runCLI(t, fx.root, "pipeline", "pull")
	if r, ok := asManagedRefusal(err); !ok || r.Reason != pullReasonInexpressible || !strings.Contains(r.Detail, "metrics/aov.json") {
		t.Fatalf("pull of a metric it cannot hold: %v", err)
	}
	f.beforeServe = nil
	if code, line, _, out := applyOnce(t, fx); code != syncExitDrifted {
		t.Fatalf("the apply after the refused pull did not refuse the web edit: exit %d %+v\n%s", code, line, out)
	}
	if got := f.RecipeOf("metric-aov"); got != edited {
		t.Fatalf("the web edit was overwritten: %s", got)
	}
}

// TestPullRefusesToWriteThroughASymlink: a planned file whose path passes
// through a symbolic link inside the folder would be written wherever the link
// points, so the pull is refused — and --force does not override it.
func TestPullRefusesToWriteThroughASymlink(t *testing.T) {
	inCleanGit(t)
	fx := workflowIdempotenceFixture(t)
	adoptFixture(t, fx)
	if code, _, _, out := applyOnce(t, fx); code != syncExitClean {
		t.Fatalf("first apply:\n%s", out)
	}
	elsewhere := t.TempDir()
	if err := os.Symlink(elsewhere, filepath.Join(fx.root, "lib")); err != nil {
		t.Skipf("no symlinks here: %v", err)
	}
	webPublishWorkflow(fx.wf, func(files []api.WorkflowFile) []api.WorkflowFile {
		return append(files, api.WorkflowFile{WorkflowID: "wf-1", Path: "lib/util.py", Content: "def util():\n    pass\n"})
	})
	for _, args := range [][]string{{"wf", "pull"}, {"wf", "pull", "--force"}} {
		fx.resetRequests()
		_, err := runCLI(t, fx.root, args...)
		if r, ok := asManagedRefusal(err); !ok || r.Reason != pullReasonSymlinked || !strings.Contains(r.Detail, "lib/util.py (through the link lib)") {
			t.Fatalf("%v through a symlink: %v", args, err)
		}
		if _, err := os.Stat(filepath.Join(elsewhere, "util.py")); !errors.Is(err, os.ErrNotExist) {
			t.Fatalf("%v wrote through the link: %v", args, err)
		}
		if w := writesBesidesGate(fx.requests()); len(w) != 0 {
			t.Fatalf("%v sent %v", args, w)
		}
	}
}

// TestAnUnmanagedStackIsNotTheOwner: a stack marked "unmanaged" deploys the
// classic way, so a deploy from it is told who manages the row — even with the
// claim's own folder id and feature.
func TestAnUnmanagedStackIsNotTheOwner(t *testing.T) {
	mb := &api.ManagedBy{FolderID: otherFolderID, FeatureID: "feat-1", SourceLabel: otherSource, StackLabel: "prod"}
	f := &folder{Kind: wfdir.WorkflowKind, Stack: "prod", Binding: wfdir.Binding{FeatureID: "feat-1"},
		Manifest: &wfdir.Manifest{ID: otherFolderID, Stacks: map[string]wfdir.Stack{"prod": {}}}}
	if n := noticeManagedBy(f, f.Kind, wfdir.ManagedWorkflow, "wf-1", mb, noticeForChange); n != nil {
		t.Fatalf("the managing folder was told: %+v", n)
	}
	st := f.Manifest.Stacks["prod"]
	st.Unmanaged = true
	f.Manifest.Stacks["prod"] = st
	if n := noticeManagedBy(f, f.Kind, wfdir.ManagedWorkflow, "wf-1", mb, noticeForChange); n == nil || !strings.Contains(n.Notice, "managed in git") {
		t.Fatalf("an unmanaged stack was not told: %+v", n)
	}
}

// TestPullThatWouldWriteSendsNothing: a pull whose anchor step would write a
// row — here, a live edit landing between the pull's read and its anchor, so
// the folder no longer holds live — sends no write: the guard refuses it before
// it leaves the process, the pull refuses as would_write naming the route, the
// files stay written and --json says nothing was recorded.
func TestPullThatWouldWriteSendsNothing(t *testing.T) {
	inCleanGit(t)
	fx := workflowIdempotenceFixture(t)
	adoptFixture(t, fx)
	if code, _, _, out := applyOnce(t, fx); code != syncExitClean {
		t.Fatalf("first apply:\n%s", out)
	}
	f := fx.wf
	edited := false
	f.beforeServe = func(r *http.Request) {
		if !edited && r.Method == http.MethodGet && r.URL.Path == "/api/v2/workflow/wf-1/versions" {
			edited = true
			f.putFile("wf-1", "main.py", "changed while the pull ran\n")
		}
	}
	fx.resetRequests()
	out, err := runCLI(t, fx.root, "wf", "pull", "--json")
	f.beforeServe = nil
	r, ok := asManagedRefusal(err)
	if !ok || r.Reason != pullReasonWouldWrite || !strings.Contains(r.Detail, " /api/v2/workflow/") ||
		!strings.Contains(r.Detail, "commit them") {
		t.Fatalf("a pull whose anchor would write: %v\n%s", err, out)
	}
	if w := writesBesidesGate(fx.requests()); !onlyAnchorWrites(w) {
		t.Fatalf("a pull whose anchor would write sent %v", w)
	}
	if report := decodePullReport(t, out); report.Anchored {
		t.Fatalf("--json says the refused anchor was recorded: %+v", report)
	}
	if got := f.FileContents("wf-1")["main.py"]; got != "changed while the pull ran\n" {
		t.Fatalf("the live edit was overwritten: %q", got)
	}
}

// unversioned makes the fixture's row one with NO committed version, and its
// lock's anchor the row's own id — what an import of a row made before
// versions existed records, and what any row a `sync apply` created and never
// updated since carries. The clone's baseline already describes the live row.
func unversioned(t *testing.T, kind string, fx idempotenceFixture) string {
	t.Helper()
	id := map[string]string{"workflow": "wf-1", "data app": "app-1", "module": "module-1"}[kind]
	switch kind {
	case "workflow":
		fx.wf.versions[id] = nil
	case "data app":
		fx.app.versions[id] = nil
	case "module":
		fx.mod.mod.versions[id] = nil
	}
	lock, err := wfdir.LoadLock(fx.root)
	if err != nil {
		t.Fatal(err)
	}
	lock.SetHeadVersion("prod", id)
	if err := wfdir.SaveLock(fx.root, lock); err != nil {
		t.Fatal(err)
	}
	return id
}

// TestPullAbsorbsAWebEditOfARowWithNoVersionHistory is QA's F1: a row that had
// no committed version when the stack applied it records its own id as the
// anchor, and the first web publish moves the head off it. The live row's
// files are then the publish's, not what the stack applied, and the unchanged
// folder was refused as holding a change never deployed. The comparison now
// uses the checkout's baseline of the row as applied: the pull absorbs the
// edit, and the next apply writes nothing.
func TestPullAbsorbsAWebEditOfARowWithNoVersionHistory(t *testing.T) {
	for kind, setup := range draftKindFixtures() {
		t.Run(kind, func(t *testing.T) {
			inCleanGit(t)
			fx := setup(t)
			unversioned(t, kind, fx)
			adoptFixture(t, fx)
			if code, line, _, out := applyOnce(t, fx); code != syncExitClean {
				t.Fatalf("first apply: exit %d %+v\n%s", code, line, out)
			}
			path := map[string]string{"workflow": "main.py", "data app": "App.tsx", "module": "tracker.py"}[kind]
			fx.draftKind.webPublish("fixed in the web app\n")

			fx.resetRequests()
			out, err := runCLI(t, fx.root, append(pullArgsOf(kind), "--json")...)
			if err != nil {
				t.Fatalf("pull of an unchanged folder: %v\n%s", err, out)
			}
			report := decodePullReport(t, out)
			if !report.Anchored || len(report.Replaced) != 0 || len(report.Kept) != 0 {
				t.Fatalf("pull reported %+v", report)
			}
			if got := readFile(t, fx.root, path); got != "fixed in the web app\n" {
				t.Fatalf("%s after pull = %q", path, got)
			}
			if w := writesBesidesGate(fx.requests()); !onlyAnchorWrites(w) {
				t.Fatalf("pull wrote more than anchors: %v", w)
			}
			if code, line, writes, out := applyOnce(t, fx); code != syncExitClean || len(writes) != 0 {
				t.Fatalf("apply after the pull: exit %d %+v, writes %v\n%s", code, line, writes, out)
			}
			if fx.draftKind.liveMain() != "fixed in the web app\n" {
				t.Fatalf("the web edit was overwritten: %q", fx.draftKind.liveMain())
			}
		})
	}
}

// TestPullStillRefusesAnUndeployedChangeOfARowWithNoVersionHistory: the same
// row, with a change of the folder's own that never deployed — still refused,
// naming the file, and --force still replaces it.
func TestPullStillRefusesAnUndeployedChangeOfARowWithNoVersionHistory(t *testing.T) {
	for kind, setup := range draftKindFixtures() {
		t.Run(kind, func(t *testing.T) {
			inCleanGit(t)
			fx := setup(t)
			unversioned(t, kind, fx)
			adoptFixture(t, fx)
			if code, line, _, out := applyOnce(t, fx); code != syncExitClean {
				t.Fatalf("first apply: exit %d %+v\n%s", code, line, out)
			}
			path := map[string]string{"workflow": "main.py", "data app": "App.tsx", "module": "tracker.py"}[kind]
			writeLocal(t, fx.root, path, "alice's fix\n")
			fx.draftKind.webPublish("bob's fix\n")

			fx.resetRequests()
			_, err := runCLI(t, fx.root, pullArgsOf(kind)...)
			r, ok := asManagedRefusal(err)
			if !ok || r.Reason != pullReasonUndeployed || !strings.Contains(r.Detail, path) {
				t.Fatalf("pull over an undeployed change: %v", err)
			}
			if got := readFile(t, fx.root, path); got != "alice's fix\n" {
				t.Fatalf("a refused pull replaced Alice's fix: %q", got)
			}
			if w := writesBesidesGate(fx.requests()); len(w) != 0 {
				t.Fatalf("a refused pull sent %v", w)
			}
		})
	}
}

// TestPullSaysWhenItCannotCompareARowWithNoVersionHistory: with no baseline
// that describes the row as the stack applied it — a fresh checkout has none;
// one re-read after the publish (a discard does that) describes the publish —
// what was applied is nowhere to be read. The pull refuses, saying so and why,
// never claiming the folder changed; --force still takes what is live.
func TestPullSaysWhenItCannotCompareARowWithNoVersionHistory(t *testing.T) {
	for name, tc := range map[string]struct {
		baseline func(t *testing.T, root string)
		why      string
	}{
		"no baseline": {
			baseline: func(t *testing.T, root string) {
				if err := os.RemoveAll(filepath.Join(root, wfdir.StateDirName)); err != nil {
					t.Fatal(err)
				}
			},
			why: "no local sync baseline",
		},
		"a baseline read after the publish": {
			baseline: func(t *testing.T, root string) {
				state, err := wfdir.LoadState(root)
				if err != nil {
					t.Fatal(err)
				}
				for _, inst := range state.Instances {
					inst.State.BaselineUpdatedAt = webPublishedAt().Add(time.Second).Format(time.RFC3339)
				}
				if err := wfdir.SaveState(root, state); err != nil {
					t.Fatal(err)
				}
			},
			why: "read after that publish",
		},
		// Live QA round 2: a first publish a second after the import. The
		// baseline WAS read before it, so the refusal must not say "after".
		"a baseline read just before the publish": {
			baseline: func(t *testing.T, root string) {
				state, err := wfdir.LoadState(root)
				if err != nil {
					t.Fatal(err)
				}
				for _, inst := range state.Instances {
					inst.State.BaselineUpdatedAt = webPublishedAt().Add(-time.Second).Format(time.RFC3339)
				}
				if err := wfdir.SaveState(root, state); err != nil {
					t.Fatal(err)
				}
			},
			why: "read 1s before that publish, too close to it to tell which came first",
		},
	} {
		t.Run(name, func(t *testing.T) {
			inCleanGit(t)
			fx := workflowIdempotenceFixture(t)
			unversioned(t, "workflow", fx)
			adoptFixture(t, fx)
			if code, line, _, out := applyOnce(t, fx); code != syncExitClean {
				t.Fatalf("first apply: exit %d %+v\n%s", code, line, out)
			}
			fx.draftKind.webPublish("fixed in the web app\n")
			tc.baseline(t, fx.root)

			_, err := runCLI(t, fx.root, "wf", "pull")
			if err == nil {
				t.Fatal("pull went ahead with nothing to compare the folder against")
			}
			if r, ok := asManagedRefusal(err); ok && r.Reason == pullReasonUndeployed {
				t.Fatalf("pull claimed an undeployed change it cannot see: %v", err)
			}
			for _, want := range []string{"cannot tell", tc.why, "--force"} {
				if !strings.Contains(err.Error(), want) {
					t.Fatalf("the refusal lacks %q: %v", want, err)
				}
			}
			if got := readFile(t, fx.root, "main.py"); got != "live\n" {
				t.Fatalf("a refused pull wrote main.py: %q", got)
			}
			// A dry run previews the same refusal rather than a clean diff,
			// and --force says it could not check what it replaced.
			out, err := runCLI(t, fx.root, "wf", "pull", "--dry-run", "--json")
			if err != nil {
				t.Fatalf("pull --dry-run: %v\n%s", err, out)
			}
			if notes := strings.Join(decodePullReport(t, out).Notes, " | "); !strings.Contains(notes, "cannot tell") || !strings.Contains(notes, "without --force") {
				t.Fatalf("the dry run hid the comparison it could not make: %q", notes)
			}
			out, err = runCLI(t, fx.root, "wf", "pull", "--force", "--json")
			if err != nil {
				t.Fatalf("pull --force: %v\n%s", err, out)
			}
			if notes := strings.Join(decodePullReport(t, out).Notes, " | "); !strings.Contains(notes, "cannot tell") || !strings.Contains(notes, "--force") {
				t.Fatalf("--force did not say it could not check what it replaced: %q", notes)
			}
			if got := readFile(t, fx.root, "main.py"); got != "fixed in the web app\n" {
				t.Fatalf("pull --force left main.py as %q", got)
			}
		})
	}
}

// TestStatusAfterAPullWithADraftOpenIsClean is QA's status finding: a pull
// with the author's own unrelated draft open leaves the local baseline (it
// describes that draft) alone, so the tree status compared the pulled files
// with the draft and reported them as never deployed — exit 1 — while the live
// row held the folder and an apply sent nothing. The tree status answers what
// an apply would do: clean, exit 0, with the open draft named as a note.
func TestStatusAfterAPullWithADraftOpenIsClean(t *testing.T) {
	for kind, setup := range draftKindFixtures() {
		t.Run(kind, func(t *testing.T) {
			inCleanGit(t)
			fx := setup(t)
			adoptFixture(t, fx)
			if code, line, _, out := applyOnce(t, fx); code != syncExitClean {
				t.Fatalf("first apply: exit %d %+v\n%s", code, line, out)
			}
			// The author's own draft, pushed and left open; the folder back as
			// committed.
			path := map[string]string{"workflow": "main.py", "data app": "App.tsx", "module": "tracker.py"}[kind]
			committed := readFile(t, fx.root, path)
			writeLocal(t, fx.root, path, "work in progress\n")
			if out, err := runCLI(t, fx.root, fx.draftKind.push...); err != nil {
				t.Fatalf("push: %v\n%s", err, out)
			}
			writeLocal(t, fx.root, path, committed)
			fx.draftKind.webPublish("fixed in the web app\n")
			if out, err := runCLI(t, fx.root, pullArgsOf(kind)...); err != nil {
				t.Fatalf("pull: %v\n%s", err, out)
			}
			if code, line, writes, out := applyOnce(t, fx); code != syncExitClean || len(writes) != 0 {
				t.Fatalf("apply after the pull: exit %d %+v, writes %v\n%s", code, line, writes, out)
			}

			out, err := runCLI(t, fx.tree, "sync", "status", "--stack", "prod", "--json")
			if err != nil {
				t.Fatalf("sync status after the pull: exit %d: %v\n%s", exitCodeOf(err), err, out)
			}
			var report syncStatusReport
			if err := json.Unmarshal([]byte(out), &report); err != nil {
				t.Fatalf("decode: %v\n%s", err, out)
			}
			if report.Verdict != verdictClean || len(report.Folders) != 1 || report.Folders[0].Verdict != verdictClean {
				t.Fatalf("sync status reported %+v", report)
			}
			if detail := report.Folders[0].Detail; !strings.Contains(detail, "open draft") || strings.Contains(detail, "never been deployed") {
				t.Fatalf("the clean line does not name the open draft as a note: %q", detail)
			}

			// A real difference from live is still drift, draft or no draft.
			writeLocal(t, fx.root, path, "not deployed yet\n")
			if out, err := runCLI(t, fx.tree, "sync", "status", "--stack", "prod", "--json"); exitCodeOf(err) != syncExitDrifted {
				t.Fatalf("sync status over an undeployed change: exit %d\n%s", exitCodeOf(err), out)
			}
		})
	}
}

// TestPullReportEscapesControlCharacters: the human diff is what an author
// reviews before committing a pull, and it prints the live row's bytes — so an
// escape sequence in them (ESC[1A ESC[2K erases the line above, a bare CR
// overwrites one) could hide a line the pull writes to disk. Every C0 and C1
// control character but tab and newline is printed escaped; --json is left to
// encoding/json, which escapes them already.
func TestPullReportEscapesControlCharacters(t *testing.T) {
	stdout, restore := captureStdout(t)
	printPullReport(wfdir.WorkflowKind, &pullReport{
		Stack: "prod", Files: []pullFileReport{{Path: "main.py", Change: "modified"}},
		Diff:  "+++ b/main.py\n+\tsafe()\n+evil()\x1b[1A\x1b[2K\r\u009b2Kok\n+x = 1 \u202e# reordered\n",
		Notes: []string{"note \x1b]8;;http://x\x07link"},
	})
	got := restore(stdout)
	for _, raw := range []string{"\x1b", "\r", "\u009b", "\x07", "\u202e"} {
		if strings.Contains(got, raw) {
			t.Fatalf("the report printed a raw control character %q:\n%q", raw, got)
		}
	}
	for _, want := range []string{`evil()\x1b[1A\x1b[2K\r\x9b2Kok`, "\tsafe()", `\x1b]8;;http://x\x07link`, `x = 1 \u202e# reordered`} {
		if !strings.Contains(got, want) {
			t.Fatalf("the report does not show %q:\n%s", want, got)
		}
	}
}

// TestFirstCommittedKnowsAFullPageIsNotTheWholeHistory: the server answers a
// row's versions newest first and at most 100 of them, so the last element of a
// FULL page is not the oldest version — a baseline read after the real first
// commit would pass for one read before it. A full page answers that the first
// version cannot be found.
func TestFirstCommittedKnowsAFullPageIsNotTheWholeHistory(t *testing.T) {
	at := func(i int) *time.Time { v := time.Date(2026, 1, 1, 0, 0, i, 0, time.UTC); return &v }
	list := func(n int) func(context.Context, string) ([]api.Workflow, error) {
		return func(context.Context, string) ([]api.Workflow, error) {
			out := make([]api.Workflow, n)
			for i := range out {
				out[i].CommittedAt = at(n - i)
			}
			return out, nil
		}
	}
	first := func(n int) (*time.Time, error) {
		return firstCommitted(list(n), func(v api.Workflow) *time.Time { return v.CommittedAt })(context.Background(), "wf-1")
	}
	if got, err := first(3); err != nil || !got.Equal(*at(1)) {
		t.Fatalf("three versions: %v, %v", got, err)
	}
	if got, err := first(versionListCap); err == nil {
		t.Fatalf("a full page answered %v as the first version", got)
	}
}

// TestPullRefusesAClaimAddressAFolderCannotHold: pipeline and automation pulls
// write each claim's file at the claim's address, which the server only
// checks for shape — so anyone who can write an automation row could claim it
// under this folder's (committed, public) id at `ronja.json` or `.github/x.json`
// and have the next pull write there. Such an address is refused before a file
// is written, as the draft kinds refuse a live file path a folder cannot hold.
func TestPullRefusesAClaimAddressAFolderCannotHold(t *testing.T) {
	for _, address := range []string{"ronja.json", ".github/workflows/x.json"} {
		t.Run(address, func(t *testing.T) {
			f, dir := gatedTree(t)
			root := filepath.Join(dir, "automations/sales")
			f.managed.insert(importFeature, "automation", "sjob-nightly", api.ManagedClaimInput{FolderID: folderIDOf(t, root), Address: address, StackLabel: "main"})
			before := snapshotFolder(t, root, false)
			out, err := runCLI(t, root, "automation", "pull", "--force")
			if r, ok := asManagedRefusal(err); !ok || r.Reason != pullReasonInexpressible || !strings.Contains(r.Detail, address) {
				t.Fatalf("pull with a claim at %s: %v\n%s", address, err, out)
			}
			if after := snapshotFolder(t, root, false); !maps.Equal(before, after) {
				t.Fatal("the refused pull wrote the folder")
			}
		})
	}
}
