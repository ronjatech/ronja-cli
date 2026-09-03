package commands

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/ronjatech/ronja-cli/internal/api"
	"github.com/ronjatech/ronja-cli/internal/wfdir"
)

// exitCodeOf maps a command's error onto the process exit code, the way `run`
// does. Used by the tests that assert the three-valued contract without
// spawning a subprocess; TestSyncStatusExitCodeReachesTheProcess pins that
// `run` itself really is wired to it.
func exitCodeOf(err error) int {
	if err == nil {
		return 0
	}
	var coded *exitCodeError
	if errors.As(err, &coded) {
		return coded.code
	}
	return 1
}

// clonedTree is clonedFolder plus the directory ABOVE it, which is what a tree
// command is pointed at.
func clonedTree(t *testing.T) (*fakeInstance, string, string) {
	t.Helper()
	f, root := clonedFolder(t)
	return f, filepath.Dir(root), root
}

func TestSyncStatusCleanTree(t *testing.T) {
	_, tree, _ := clonedTree(t)

	out, err := runCLI(t, tree, "sync", "status", "--json")
	if code := exitCodeOf(err); code != syncExitClean {
		t.Fatalf("exit = %d (%v), want %d", code, err, syncExitClean)
	}
	report := decodeJSON(t, out)
	if report["verdict"] != verdictClean {
		t.Fatalf("verdict = %v, want %q\n%s", report["verdict"], verdictClean, out)
	}
	folders := report["folders"].([]any)
	if len(folders) != 1 {
		t.Fatalf("folders = %v, want one", folders)
	}
	folder := folders[0].(map[string]any)
	if folder["path"] != "dest" || folder["kind"] != wfdir.KindWorkflow {
		t.Errorf("folder = %v", folder)
	}
	if folder["verdict"] != verdictClean {
		t.Errorf("folder verdict = %v, want %q", folder["verdict"], verdictClean)
	}
}

// Drift is the actionable case, and it must be distinguishable from "could not
// tell" by the exit code alone — a CI job pushes on one and stops on the other.
func TestSyncStatusReportsDriftAsOne(t *testing.T) {
	f, tree, _ := clonedTree(t)
	f.putFile("wf-1", "main.py", "changed on the server\n")

	out, err := runCLI(t, tree, "sync", "status", "--json")
	if code := exitCodeOf(err); code != syncExitDrifted {
		t.Fatalf("exit = %d (%v), want %d\n%s", code, err, syncExitDrifted, out)
	}
	report := decodeJSON(t, out)
	if report["verdict"] != verdictDrifted {
		t.Fatalf("verdict = %v, want %q\n%s", report["verdict"], verdictDrifted, out)
	}
	summary := report["summary"].(map[string]any)
	if summary["drifted"].(float64) != 1 {
		t.Errorf("summary = %v", summary)
	}
}

// The case this whole command exists for: a folder cloned from git carries no
// .ronja/, so there is nothing to compare against. The per-folder `wf status`
// reports every remote file as ADDED and exits zero; the tree must report that
// it could not tell, and exit 2.
func TestSyncStatusFreshCheckoutIsUnknownNotDrift(t *testing.T) {
	_, tree, root := clonedTree(t)
	// Exactly what `git clone` leaves behind: the committed files and manifest,
	// and no local baseline at all.
	if err := os.RemoveAll(filepath.Join(root, wfdir.StateDirName)); err != nil {
		t.Fatal(err)
	}

	out, err := runCLI(t, tree, "sync", "status", "--json")
	if code := exitCodeOf(err); code != syncExitUnknown {
		t.Fatalf("exit = %d (%v), want %d\n%s", code, err, syncExitUnknown, out)
	}
	report := decodeJSON(t, out)
	if report["verdict"] != verdictUnknown {
		t.Fatalf("verdict = %v, want %q\n%s", report["verdict"], verdictUnknown, out)
	}
	// And specifically NOT drifted, which is the wrong answer a naive
	// implementation gives: every remote file reads as new.
	if report["summary"].(map[string]any)["drifted"].(float64) != 0 {
		t.Errorf("a fresh checkout was counted as drift: %v", report["summary"])
	}
}

// A renamed or moved directory must not sail through the gate reporting
// success. Zero folders is exit 2, never 0.
func TestSyncStatusEmptyWalkIsUnknown(t *testing.T) {
	f := newFakeInstance(t)
	signIn(t, f)
	empty := t.TempDir()

	out, err := runCLI(t, empty, "sync", "status", "--json")
	if code := exitCodeOf(err); code != syncExitUnknown {
		t.Fatalf("exit = %d (%v), want %d", code, err, syncExitUnknown)
	}
	report := decodeJSON(t, out)
	if report["verdict"] != verdictUnknown {
		t.Fatalf("verdict = %v, want %q", report["verdict"], verdictUnknown)
	}
	if len(report["folders"].([]any)) != 0 {
		t.Errorf("folders = %v, want none", report["folders"])
	}
}

// A folder that does not declare the stack asked for is skipped rather than
// aborting the run — and is never counted as clean, which is what stops a
// --stack typo reporting a whole repository healthy.
func TestSyncStatusUnknownStackIsNeverGreen(t *testing.T) {
	_, tree, _ := clonedTree(t)

	out, err := runCLI(t, tree, "sync", "status", "--stack", "nope", "--json")
	if code := exitCodeOf(err); code != syncExitUnknown {
		t.Fatalf("exit = %d (%v), want %d\n%s", code, err, syncExitUnknown, out)
	}
	folder := decodeJSON(t, out)["folders"].([]any)[0].(map[string]any)
	if folder["verdict"] != verdictUnknown {
		t.Errorf("verdict = %v, want %q", folder["verdict"], verdictUnknown)
	}
	if folder["reason"] != syncReasonUnnamedStack && folder["reason"] != syncReasonStackAbsent {
		t.Errorf("reason = %v, want a stack reason", folder["reason"])
	}
}

// A --stack that cannot BE a stack name is a wrong argument, not something to
// degrade around: reporting every folder in the repository as "does not declare
// that stack" would describe a problem the reader does not have.
func TestSyncStatusRefusesAnInvalidStackName(t *testing.T) {
	_, tree, _ := clonedTree(t)

	_, err := runCLI(t, tree, "sync", "status", "--stack", "not a name", "--json")
	if err == nil {
		t.Fatal("an invalid stack name was accepted")
	}
	var coded *exitCodeError
	if errors.As(err, &coded) {
		t.Errorf("an invalid argument earned a tree verdict exit code (%d) rather than an ordinary failure", coded.code)
	}
}

// --dir must be honoured from anywhere, which is the whole point of threading a
// root through the status paths instead of relying on the working directory.
func TestSyncStatusHonoursDirFromElsewhere(t *testing.T) {
	_, tree, _ := clonedTree(t)
	elsewhere := t.TempDir()

	out, err := runCLI(t, elsewhere, "sync", "status", "--dir", tree, "--json")
	if code := exitCodeOf(err); code != syncExitClean {
		t.Fatalf("exit = %d (%v), want %d\n%s", code, err, syncExitClean, out)
	}
	if len(decodeJSON(t, out)["folders"].([]any)) != 1 {
		t.Errorf("folders = %v, want one", decodeJSON(t, out)["folders"])
	}
}

// The exit code has to survive `run`, not only the RunE error, because that is
// the only thing a CI job actually sees. Asserted through run itself so the
// wiring cannot rot behind exitCodeOf's copy of the rule.
func TestSyncStatusExitCodeReachesTheProcess(t *testing.T) {
	f := newFakeInstance(t)
	signIn(t, f)
	t.Chdir(t.TempDir())

	if code := run([]string{"sync", "status"}); code != syncExitUnknown {
		t.Fatalf("run exit = %d, want %d", code, syncExitUnknown)
	}
}

// seedLockAdoptedFolder builds the ONE fixture that actually reaches
// adoptStack, which is the write both read-only tests exist to catch.
//
// ⚠️ Getting here is fiddlier than it looks, and both tests previously staged
// something that could not reach it at all. adoptStack returns immediately on
// `f.Stack == ""`, so a run with no --stack can never trip it — and with
// --stack, a LEGACY instances[] folder never even gets opened, because
// resolveStackForFolder refuses it as `unnamed_stack` first. The one shape that
// survives both gates is a manifest that declares NO stack, a committed lock
// that DOES record one, and `--stack <that name>`: resolveStackForFolder adopts
// a lock-recorded stack by name (it is a push whose lock write landed and whose
// manifest write did not), selectNamed then returns Bound with the CREDENTIAL's
// own key — which is Known when signed in — and adoptStack writes ronja.json to
// record the stack the lock already knows about.
//
// Mutation-proved: inserting `_ = f.adoptStack()` into the tree path makes both
// WritesNothing tests fail on this fixture, and neither of their previous ones.
func seedLockAdoptedFolder(t *testing.T, f *fakeInstance, dir string) string {
	t.Helper()
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatalf("mkdir %s: %v", dir, err)
	}
	manifest := &wfdir.Manifest{
		Kind: wfdir.KindWorkflow, Title: "Monthly report", Entrypoint: "main.py",
	}
	lock := &wfdir.Lock{Stacks: map[string]wfdir.LockStack{
		"prod": {WorkflowID: "wf-1"},
	}}
	if err := wfdir.SaveFolder(dir, manifest, lock); err != nil {
		t.Fatalf("save %s: %v", dir, err)
	}
	writeLocal(t, dir, "main.py", "print('hi')\n")
	return dir
}

// assertTreeUntouched is the read-only assertion both tree commands owe the
// customer's repository, run around one invocation.
//
// Snapshotting paths, sizes and mtimes rather than content: a write that
// happened to reproduce the same bytes is still a write, and the mtime is what
// makes a git working tree look dirty to the tools people actually use.
func assertTreeUntouched(t *testing.T, tree string, run func()) {
	t.Helper()
	before := snapshotTree(t, tree)
	run()
	after := snapshotTree(t, tree)
	for path, want := range before {
		got, present := after[path]
		if !present {
			t.Errorf("REMOVED %s", path)
			continue
		}
		if got != want {
			t.Errorf("MODIFIED %s: %s -> %s — see adoptStack, which rewrites %s to record a stack the lock already names",
				path, want, got, wfdir.ManifestName)
		}
	}
	for path := range after {
		if _, present := before[path]; !present {
			t.Errorf("CREATED %s — see wfdir.SaveState, which also writes a .gitignore", path)
		}
	}
}

// `ronja sync status` must not touch the customer's repository. AT ALL.
//
// The claim is true today and is one refactor from not being true, which is why
// this is an assertion and not a comment. wfdir.SaveState writes a .gitignore
// beside the state file, and openFolder's adoptStack REWRITES ronja.json — so a
// tree path that reached either would create or change files in a repository as
// a side effect of a command called `status`, in a git workflow where a clean
// tree is the whole point.
//
// ⚠️ This test used to stage a fixture that could not reach adoptStack at all
// (see seedLockAdoptedFolder), so it was green whether or not the command
// wrote. It now runs on the fixture that does, alongside a folder that makes the
// run do real work.
func TestSyncStatusWritesNothing(t *testing.T) {
	f := newFakeInstance(t)
	f.AddWorkflow(
		&api.Workflow{ID: "wf-1", Lifecycle: api.LifecycleLive, Title: "Monthly report"},
		api.WorkflowFile{Path: "main.py", Content: "print('hi')\n"},
	)
	signIn(t, f)
	tree := t.TempDir()
	seedLockAdoptedFolder(t, f, filepath.Join(tree, "adopts"))
	// A second folder of another kind, deliberately unbound, so the pipeline
	// status path is exercised too — its early "not bound here" return is exactly
	// the sort of path a later refactor might hang a save off.
	seedManifest(t, filepath.Join(tree, "pipe"), wfdir.PipelineKind, nil)
	writeLocal(t, filepath.Join(tree, "pipe"), "orders.sql", "SELECT 1\n")

	assertTreeUntouched(t, tree, func() {
		if _, err := runCLI(t, tree, "sync", "status", "--stack", "prod", "--json"); exitCodeOf(err) == 0 {
			t.Fatalf("expected a non-zero verdict for this fixture, got clean")
		}
	})
}

// snapshotTree records every path beneath root with its size and mtime.
// Dot-directories are INCLUDED: .ronja/ is precisely where an accidental save
// would land.
func snapshotTree(t *testing.T, root string) map[string]string {
	t.Helper()
	out := map[string]string{}
	err := filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		rel, relErr := filepath.Rel(root, path)
		if relErr != nil {
			return relErr
		}
		if d.IsDir() {
			out[rel+"/"] = "dir"
			return nil
		}
		info, infoErr := d.Info()
		if infoErr != nil {
			return infoErr
		}
		out[rel] = fmt.Sprintf("%d bytes, mtime %s", info.Size(), info.ModTime().UTC().Format(time.RFC3339Nano))
		return nil
	})
	if err != nil {
		t.Fatalf("snapshot %s: %v", root, err)
	}
	return out
}

// THE FALSE GREEN, end to end and on all three kinds.
//
// A folder bound to THIS credential's own organization, naming a feature, whose
// content has never been pushed, reported `clean` and exited ZERO. An entire
// table's SQL or an entire workflow could sit undeployed in a repository and
// `ronja sync status` called it healthy — which is exactly the failure the
// command exists to prevent, and a CI gate built on it passed.
//
// The mechanism was one line per loop: pipelineRemoteStatus sets
// reasonNothingBound once the folder is bound with a featureID and zero bound
// tables, reasonNoWorkflowYet / reasonNoAppYet are the same shape, and the tree
// verdict mapped all three to clean unconditionally. They are honest only when
// there is nothing to deploy, so both sides of that boundary are pinned here —
// per kind, because the three loops reach it through three different functions
// and a fix to one says nothing about the others.
//
// Asserted through the COMMAND rather than through the verdict helpers: the unit
// tests in sync_verdict_test.go pinned the helper and passed throughout, because
// what was wrong was the answer the helper gave, not the wiring into it.
func TestSyncStatusNeverDeployedIsNotClean(t *testing.T) {
	tests := []struct {
		name string
		kind wfdir.Kind
		// content is what the non-empty folder holds. The empty case writes none
		// of it, which is the whole comparison.
		content map[string]string
		// noise is a file the folder's own loop would NOT push. It rides along in
		// the empty case, where it must not count as something to deploy: a
		// pipeline syncs .sql and ignores everything else, so a README beside the
		// tables is not content anybody failed to deploy.
		noise map[string]string
	}{
		{
			name:    "pipeline",
			kind:    wfdir.PipelineKind,
			content: map[string]string{"revenue.sql": "SELECT 1\n"},
			noise:   map[string]string{"README.md": "# notes\n"},
		},
		{
			name:    "workflow",
			kind:    wfdir.WorkflowKind,
			content: map[string]string{"main.py": "print('hi')\n"},
		},
		{
			name:    "data app",
			kind:    wfdir.DataAppKind,
			content: map[string]string{"App.tsx": "export default () => null;\n"},
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			for _, deployed := range []struct {
				name     string
				files    map[string]string
				wantExit int
				wantWord string
			}{
				// An empty folder genuinely has nothing to deploy — which is what
				// keeps `init` followed by `sync status` sane, and is the reason
				// the exception exists at all.
				{"empty folder", nil, syncExitClean, verdictClean},
				{"content on disk", test.content, syncExitDrifted, verdictDrifted},
			} {
				t.Run(deployed.name, func(t *testing.T) {
					f := newFakeInstance(t)
					signIn(t, f)
					tree := t.TempDir()
					dir := filepath.Join(tree, "folder")
					// Bound to THIS organization, naming a feature, with nothing
					// in the lock: exactly the shape that produced the false
					// green.
					seedManifest(t, dir, test.kind, &wfdir.Manifest{
						Title: "Ops",
						Stacks: map[string]wfdir.Stack{
							"prod": {URL: f.URL(), TenantID: testTenantID, FeatureID: "feat-1"},
						},
					})
					for path, body := range deployed.files {
						writeLocal(t, dir, path, body)
					}
					for path, body := range test.noise {
						writeLocal(t, dir, path, body)
					}

					out, err := runCLI(t, tree, "sync", "status", "--stack", "prod", "--json")
					if code := exitCodeOf(err); code != deployed.wantExit {
						t.Fatalf("exit = %d (%v), want %d\n%s", code, err, deployed.wantExit, out)
					}
					report := decodeJSON(t, out)
					if report["verdict"] != deployed.wantWord {
						t.Fatalf("verdict = %v, want %q\n%s", report["verdict"], deployed.wantWord, out)
					}
					// And specifically NOT unknown: nothing here is ambiguous. We
					// know the files are on disk and we know nothing is on the
					// server, so a gate must get the actionable answer rather
					// than "could not tell".
					folder := report["folders"].([]any)[0].(map[string]any)
					if folder["verdict"] != deployed.wantWord {
						t.Fatalf("folder verdict = %v, want %q\n%s", folder["verdict"], deployed.wantWord, out)
					}
					if deployed.wantWord == verdictDrifted {
						detail, _ := folder["detail"].(string)
						if !strings.Contains(detail, "never been deployed") {
							t.Errorf("the detail reuses the drift wording, which claims a comparison that never happened: %s", detail)
						}
					}
				})
			}
		})
	}
}

// THE OTHER FALSE GREEN, and the wider of the two.
//
// The tree verdict read exactly ONE leg — the server's copy against the local
// baseline — so a folder whose files had been EDITED and never pushed came back
// `clean`, exit 0. The report already computed that (`Local`); nothing read it.
//
// Every per-folder `status` is right to leave it out of its own drift verdict:
// "a file the author edited locally is exactly what a push is for". That is the
// semantic of an interactive one-folder command. A repository gate asks a
// different question — would a push from this tree change the organization? —
// and a local edit is a yes.
//
// The metadata cases matter for a reason of their own: they are the ones a push
// would be REFUSED for. A runtime-1 folder against a runtime-2 row cannot be
// pushed at all, and reporting that repository as healthy is worse than
// reporting drift.
func TestSyncStatusLocalChangesAreNotClean(t *testing.T) {
	tests := []struct {
		name string
		// change mutates a cloned, in-sync workflow folder so that a push would
		// change the organization.
		change func(t *testing.T, f *fakeInstance, root string)
		// want is a fragment the folder's detail must carry, so the two halves of
		// the sentence stay distinguishable: a reader acts differently on "you
		// hold work that was never deployed" and "the server moved under you".
		want string
	}{
		{
			name: "a file edited locally and never pushed",
			change: func(t *testing.T, _ *fakeInstance, root string) {
				writeLocal(t, root, "main.py", "print('edited but never pushed')\n")
			},
			want: "never been deployed",
		},
		{
			name: "a file added locally and never pushed",
			change: func(t *testing.T, _ *fakeInstance, root string) {
				writeLocal(t, root, "lib/extra.py", "X = 2\n")
			},
			want: "never been deployed",
		},
		{
			name: "a file deleted locally and never pushed",
			change: func(t *testing.T, _ *fakeInstance, root string) {
				if err := os.Remove(filepath.Join(root, "lib", "helpers.py")); err != nil {
					t.Fatal(err)
				}
			},
			want: "never been deployed",
		},
		{
			// A push here is not merely unmade, it is IMPOSSIBLE: the runtime moves
			// one way, so a folder DECLARING 1 against a row on 2 is refused. The
			// declaration has to be made here: the fixture's row reports no runtime,
			// so the clone recorded none, and a folder declaring nothing has no
			// opinion for the row to contradict.
			name: "a runtime generation a push would be refused for",
			change: func(t *testing.T, f *fakeInstance, root string) {
				setManifestRuntime(t, root, wfdir.RuntimeDefault)
				f.workflows["wf-1"].RuntimeVersion = 2
			},
			want: "REFUSES",
		},
		{
			name: "a reporting timezone the folder declares and the row does not have",
			change: func(t *testing.T, f *fakeInstance, root string) {
				zone := "Europe/Stockholm"
				manifest, err := wfdir.LoadManifest(root, wfdir.WorkflowKind)
				if err != nil {
					t.Fatal(err)
				}
				manifest.ReportingTimezone = &zone
				if err := wfdir.SaveManifest(root, manifest); err != nil {
					t.Fatal(err)
				}
			},
			want: "reporting timezone",
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			f, tree, root := clonedTree(t)
			// Sanity: the fixture is in step before the change, so the assertion
			// below is about the change and not about the fixture.
			if _, err := runCLI(t, tree, "sync", "status", "--json"); exitCodeOf(err) != syncExitClean {
				t.Fatalf("the fixture was not clean to begin with: %v", err)
			}
			test.change(t, f, root)

			out, err := runCLI(t, tree, "sync", "status", "--json")
			if code := exitCodeOf(err); code != syncExitDrifted {
				t.Fatalf("exit = %d (%v), want %d\n%s", code, err, syncExitDrifted, out)
			}
			report := decodeJSON(t, out)
			if report["verdict"] != verdictDrifted {
				t.Fatalf("verdict = %v, want %q\n%s", report["verdict"], verdictDrifted, out)
			}
			detail, _ := report["folders"].([]any)[0].(map[string]any)["detail"].(string)
			if !strings.Contains(detail, test.want) {
				t.Errorf("detail = %q, want it to mention %q", detail, test.want)
			}
		})
	}
}

// A workflow whose OWN DRAFT could not be read reports the live row's answer.
// That is a true statement about a row a push would not write to, so it is
// "could not tell" — not clean, which is what it used to report.
func TestSyncStatusUnreadableDraftIsUnknown(t *testing.T) {
	f, tree, _ := clonedTree(t)
	// The draft lookup fails; everything below is measured against the live row,
	// which matches the baseline exactly.
	f.failDraft = 500

	out, err := runCLI(t, tree, "sync", "status", "--json")
	if code := exitCodeOf(err); code != syncExitUnknown {
		t.Fatalf("exit = %d (%v), want %d\n%s", code, err, syncExitUnknown, out)
	}
	detail, _ := decodeJSON(t, out)["folders"].([]any)[0].(map[string]any)["detail"].(string)
	if !strings.Contains(detail, "draft") {
		t.Errorf("the report does not say which half was unread: %q", detail)
	}
}

// The FALSE POSITIVE the local leg had to avoid, which is the mirror of the
// false green it fixes — and it lands on exactly the run that matters most.
//
// wfdir.DiffHashes against an ABSENT baseline reports every local file as
// `Added`, and a folder freshly cloned from git has no .ronja/ at all. A tree
// verdict that read `Added` would therefore report a perfectly in-step
// repository as holding undeployed work, in CI, on every clean build.
//
// Here the folder is in step: the committed lock records the live table's
// fingerprint, the server still matches it, and the .sql file on disk is the one
// that was pushed. There is no local baseline. It must be CLEAN.
// inStepSQL is the one string a folder, its lock and the server all hold in the
// in-step fixtures below.
const inStepSQL = "SELECT 1\n"

func TestSyncStatusFreshCloneWithALockIsClean(t *testing.T) {
	f := newFakeInstance(t)
	f.tableNames["table-1"] = "Revenue"
	f.tableCode["table-1"] = inStepSQL
	f.featureTables["feat-1"] = []*api.TableListItem{{ID: "table-1", Name: "Revenue", Status: "ready"}}
	signIn(t, f)
	tree := t.TempDir()
	dir := filepath.Join(tree, "pipe")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	manifest := &wfdir.Manifest{
		Kind: wfdir.KindPipeline, Title: "Ops",
		Stacks: map[string]wfdir.Stack{
			"prod": {URL: f.URL(), TenantID: testTenantID, FeatureID: "feat-1"},
		},
	}
	lock := &wfdir.Lock{}
	// The live fingerprint the folder last agreed with, which is what a fresh
	// checkout has instead of a baseline — and it is the hash of the DISK form,
	// which for a table with no aliases and no positional refs is the bytes the
	// server holds. In step, all three ways round.
	lock.SetTableLive("prod", "revenue.sql", "table-1", wfdir.HashString(inStepSQL))
	if err := wfdir.SaveFolder(dir, manifest, lock); err != nil {
		t.Fatal(err)
	}
	// On disk, and deliberately NOT empty: this is the file that would read as
	// `Added` against a baseline that is not there.
	writeLocal(t, dir, "revenue.sql", inStepSQL)
	// The binding, so the file is bound to a table rather than one a push would
	// create — WillCreate is the other half of this leg and has its own test.
	manifest.Record(lock, wfdir.Selection{
		Name: "prod", Key: f.Key(), Bound: true,
		Binding: wfdir.Binding{FeatureID: "feat-1", Tables: map[string]string{"revenue.sql": "table-1"}},
	}, wfdir.Binding{FeatureID: "feat-1", Tables: map[string]string{"revenue.sql": "table-1"}})
	if err := wfdir.SaveFolder(dir, manifest, lock); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(dir, wfdir.StateDirName)); !os.IsNotExist(err) {
		t.Fatalf("this fixture must have no local baseline, which is the whole point")
	}

	out, err := runCLI(t, tree, "sync", "status", "--stack", "prod", "--json")
	if code := exitCodeOf(err); code != syncExitClean {
		t.Fatalf("exit = %d (%v), want %d — a fresh clone of an in-step folder must not read as undeployed work\n%s",
			code, err, syncExitClean, out)
	}
}

// THE FRESH-CLONE PIPELINE LEG, and the last false green on this command.
//
// On a checkout with no .ronja/, every LOCAL signal is silent: DiffHashes has no
// baseline to compare against, so `Local` is empty by construction. The remote
// leg still fires — the committed lock carries the live table's fingerprint —
// but it only answers "has the server moved", and the answer is no. So a
// repository holding SQL that was never deployed reported clean, exit 0, on
// exactly the run a CI gate makes.
//
// The lock is what closes it, and closing it is what the lock is FOR: its own
// doc calls the live fingerprint "a fact about the ENVIRONMENT, not about a
// person… the first thing a fresh CI checkout has ever had to compare against".
// The committed .sql against that committed hash answers the question directly.
//
// The comparison is only valid because the recorded fingerprint is of the DISK
// form — stated at the point it is written (pipeline_push.go: "the two are the
// same fingerprint by construction") — so this fixture pins the round trip too:
// the file, the lock and the server's `code` all hold the same bytes until the
// file is edited.
func TestSyncStatusUndeployedSQLOnAFreshCloneIsDrifted(t *testing.T) {
	f := newFakeInstance(t)
	f.tableNames["table-1"] = "Revenue"
	f.tableCode["table-1"] = inStepSQL
	f.featureTables["feat-1"] = []*api.TableListItem{{ID: "table-1", Name: "Revenue", Status: "ready"}}
	signIn(t, f)
	tree := t.TempDir()
	dir := filepath.Join(tree, "pipe")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	manifest := &wfdir.Manifest{
		Kind: wfdir.KindPipeline, Title: "Ops",
		Stacks: map[string]wfdir.Stack{
			"prod": {URL: f.URL(), TenantID: testTenantID, FeatureID: "feat-1"},
		},
	}
	lock := &wfdir.Lock{}
	lock.SetTableLive("prod", "revenue.sql", "table-1", wfdir.HashString(inStepSQL))
	b := wfdir.Binding{FeatureID: "feat-1", Tables: map[string]string{"revenue.sql": "table-1"}}
	manifest.Record(lock, wfdir.Selection{Name: "prod", Key: f.Key(), Bound: true, Binding: b}, b)
	if err := wfdir.SaveFolder(dir, manifest, lock); err != nil {
		t.Fatal(err)
	}
	// The committed SQL — what `git clone` hands you — is NOT what was deployed.
	// Somebody edited it, committed it, and never pushed.
	writeLocal(t, dir, "revenue.sql", "SELECT 2 -- edited, committed, never pushed\n")
	if _, err := os.Stat(filepath.Join(dir, wfdir.StateDirName)); !os.IsNotExist(err) {
		t.Fatalf("this fixture must have no local baseline, which is the whole point")
	}

	out, err := runCLI(t, tree, "sync", "status", "--stack", "prod", "--json")
	if code := exitCodeOf(err); code != syncExitDrifted {
		t.Fatalf("exit = %d (%v), want %d\n%s", code, err, syncExitDrifted, out)
	}
	folder := decodeJSON(t, out)["folders"].([]any)[0].(map[string]any)
	if folder["verdict"] != verdictDrifted {
		t.Fatalf("verdict = %v, want %q\n%s", folder["verdict"], verdictDrifted, out)
	}
	detail, _ := folder["detail"].(string)
	// It must be reported as work that was never deployed, NOT as the server
	// moving — the server did not move, and a reader acts differently on each.
	if !strings.Contains(detail, "never been deployed") || !strings.Contains(detail, "revenue.sql") {
		t.Errorf("detail = %q, want it to name the file as undeployed work", detail)
	}
	if strings.Contains(detail, "changed on the server") {
		t.Errorf("the server did not move, and the report says it did: %q", detail)
	}
}

// A credential that will not resolve at all is the one class of failure where
// NOTHING was verified — no walk, no folder, no comparison — so it is exit 2.
//
// It used to be returned bare, which `run` scores 1: the code this command's own
// help defines as "checked, and something drifted". A job that branches on 1
// pushes. Both tree commands, because they resolve the credential in two
// separate RunE bodies and a fix to one says nothing about the other.
func TestSyncCredentialFailureIsUnknownNotDrift(t *testing.T) {
	for _, command := range []string{"status", "check"} {
		t.Run(command, func(t *testing.T) {
			// A profile name nothing has stored: config.Resolve refuses before
			// anything is read from disk or asked of any instance.
			t.Setenv("RONJA_CONFIG_DIR", t.TempDir())
			t.Setenv("RONJA_URL", "")
			t.Setenv("RONJA_TOKEN", "")
			t.Setenv("RONJA_PROFILE", "")

			_, err := runCLI(t, t.TempDir(), "sync", command, "--profile", "nothing-stored", "--json")
			if err == nil {
				t.Fatal("an unresolvable credential was accepted")
			}
			if code := exitCodeOf(err); code != syncExitUnknown {
				t.Fatalf("exit = %d (%v), want %d — nothing was checked, which is not the same answer as drift",
					code, err, syncExitUnknown)
			}
		})
	}
}
