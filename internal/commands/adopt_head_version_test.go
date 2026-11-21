package commands

import (
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/ronjatech/ronja-cli/internal/api"
	"github.com/ronjatech/ronja-cli/internal/wfdir"
)

// ADOPT RECORDS THE LIVE VERSION IT CAN PROVE. A folder adopted onto its stack
// rather than cloned records which live version it holds only when live holds
// it exactly, or when this checkout's local baseline was taken from live and
// live has not moved since; otherwise nothing, and the folder's line says why.

// forgetHeadVersion makes a folder's stack record no live version, in its lock
// and on its claim: a folder committed by a CLI that never recorded one.
func forgetHeadVersion(t *testing.T, m *fakeManaged, root, stack string) {
	t.Helper()
	editLock(t, root, func(l *wfdir.Lock) { l.SetHeadVersion(stack, "") })
	manifest := loadAnyManifest(t, root)
	if !manifest.Adopted() {
		return
	}
	if c := m.ClaimAt(manifest.Stacks[stack].FeatureID, manifest.ID, ""); c != nil {
		m.mu.Lock()
		delete(c.Applied, "headVersionID")
		m.mu.Unlock()
	}
}

// recordedHeadVersion answers the live version a folder's lock and its claim
// record for a stack.
func recordedHeadVersion(t *testing.T, m *fakeManaged, root, stack string) (lock, claim string) {
	t.Helper()
	manifest := loadAnyManifest(t, root)
	c := m.ClaimAt(manifest.Stacks[stack].FeatureID, manifest.ID, "")
	if c == nil {
		t.Fatalf("%s has no claim on stack %s", root, stack)
	}
	m.mu.Lock()
	claim, _ = c.Applied["headVersionID"].(string)
	m.mu.Unlock()
	return mustLoadLock(t, root).HeadVersion(stack), claim
}

// adoptLine runs `sync adopt` over a tree and answers the line of the folder
// at path.
func adoptLine(t *testing.T, tree, stack, path string) syncManagedFolderReport {
	t.Helper()
	out, err := runCLI(t, tree, "sync", "adopt", "--stack", stack, "--json")
	if err != nil {
		t.Fatalf("adopt: %v\n%s", err, out)
	}
	var report syncManagedReport
	decodeJSONInto(t, out, &report)
	for _, line := range report.Folders {
		if line.Path == path {
			return line
		}
	}
	t.Fatalf("adopt reported no folder %s:\n%s", path, out)
	return syncManagedFolderReport{}
}

// Every folder equals live and is adopted with no live version recorded and
// no local baseline. Adopt records live's version, so
// the first change list proposed from it deploys with no push in between.
func TestAdoptRecordsTheLiveVersionTheFolderHolds(t *testing.T) {
	f, dir, _ := freshStackFixture(t)
	if report, err := applyStack(t, dir, "dev", "--allow-create"); err != nil || report.Verdict != verdictApplied {
		t.Fatalf("the first deploy: %v\n%s", err, describeApply(report))
	}
	want := map[string]string{}
	for _, o := range ownFolders {
		root := filepath.Join(dir, o.path)
		if want[o.path] = mustLoadLock(t, root).HeadVersion("dev"); want[o.path] == "" {
			t.Fatalf("%s: the deploy recorded no live version", o.path)
		}
		forgetHeadVersion(t, f.managed, root, "dev")
	}
	dropLocalState(t, dir)

	if out, err := runCLI(t, dir, "sync", "adopt", "--stack", "dev", "--json"); err != nil {
		t.Fatalf("adopt: %v\n%s", err, out)
	}
	for _, o := range ownFolders {
		lock, claim := recordedHeadVersion(t, f.managed, filepath.Join(dir, o.path), "dev")
		if lock != want[o.path] || claim != want[o.path] {
			t.Errorf("%s: the lock records %q and the claim %q, want live's %s", o.path, lock, claim, want[o.path])
		}
	}

	editOwnFolders(t, dir, "proposed")
	if out, err := runPropose_(t, dir, "--stack", "dev", "--json"); err != nil {
		t.Fatalf("propose: %v\n%s", err, out)
	}
	deployCopy(t, f.changeLists.proposals[0].archive)
}

// A folder adopted with no recorded title and no local baseline proposes a
// rename: adopt recorded live's title with its version, so the copy's deploy
// tells the rename from one made in the web app, and applies it.
func TestAdoptedFolderRenamesThroughAChangeList(t *testing.T) {
	f, dir, _ := freshStackFixture(t)
	if report, err := applyStack(t, dir, "dev", "--allow-create"); err != nil || report.Verdict != verdictApplied {
		t.Fatalf("the first deploy: %v\n%s", err, describeApply(report))
	}
	for _, o := range ownFolders {
		root := filepath.Join(dir, o.path)
		forgetHeadVersion(t, f.managed, root, "dev")
		editLock(t, root, func(l *wfdir.Lock) { l.SetAppliedTitle("dev", "") })
		manifest := loadAnyManifest(t, root)
		if c := f.managed.ClaimAt(manifest.Stacks["dev"].FeatureID, manifest.ID, ""); c != nil {
			f.managed.mu.Lock()
			delete(c.Applied, "title")
			f.managed.mu.Unlock()
		}
	}
	dropLocalState(t, dir)

	if out, err := runCLI(t, dir, "sync", "adopt", "--stack", "dev", "--json"); err != nil {
		t.Fatalf("adopt: %v\n%s", err, out)
	}
	for _, o := range ownFolders {
		root := filepath.Join(dir, o.path)
		m := loadAnyManifest(t, root)
		m.Title = "Renamed by the list"
		if err := wfdir.SaveManifest(root, m); err != nil {
			t.Fatal(err)
		}
	}
	if out, err := runPropose_(t, dir, "--stack", "dev", "--json"); err != nil {
		t.Fatalf("propose: %v\n%s", err, out)
	}
	deployCopy(t, f.changeLists.proposals[0].archive)

	for _, o := range ownFolders {
		stack := mustLoadLock(t, filepath.Join(dir, o.path)).Stacks["dev"]
		var title string
		switch {
		case stack.WorkflowID != "":
			title = f.wf.workflows[stack.WorkflowID].Title
		case stack.DataAppID != "":
			title = f.app.apps[stack.DataAppID].Name
		default:
			title = f.wf.mod.rows[stack.ModuleID].Title
		}
		if title != "Renamed by the list" {
			t.Errorf("%s: live is titled %q after the copy deployed, want the list's rename", o.path, title)
		}
	}
}

// onRequest runs hook before the fixture's fake answers each request.
func onRequest(fx idempotenceFixture, hook func(r *http.Request)) {
	switch {
	case fx.wf != nil:
		fx.wf.beforeServe = hook
	case fx.mod != nil:
		fx.mod.beforeServe = hook
	default:
		fx.app.beforeServe = hook
	}
}

// renameLive is a colleague's rename in the web app, published: the live row's
// title changes and its head moves to a new version, its files unchanged.
func renameLive(fx idempotenceFixture, title string) {
	fx.draftKind.webPublish(fx.draftKind.liveMain())
	switch {
	case fx.wf != nil:
		fx.wf.workflows["wf-1"].Title = title
	case fx.mod != nil:
		fx.mod.mod.rows["module-1"].Title = title
	default:
		fx.app.apps["app-1"].Name = title
	}
}

func TestAdoptRecordsNothingItCannotProve(t *testing.T) {
	cases := map[string]struct {
		stage func(t *testing.T, fx idempotenceFixture)
		why   string
	}{
		"live differs and there is no local baseline": {
			stage: func(t *testing.T, fx idempotenceFixture) {
				fx.edit(t)
				dropLocalState(t, fx.root)
			},
			why: "nothing here shows the folder descends from it",
		},
		"the local baseline shows live moved": {
			stage: func(t *testing.T, fx idempotenceFixture) {
				fx.draftKind.webPublish("colleague\n")
			},
			why: "changed since this checkout last synced with it",
		},
		"live renamed since the local baseline": {
			stage: func(t *testing.T, fx idempotenceFixture) {
				renameLive(fx, "Renamed in the web app")
			},
			why: "changed since this checkout last synced with it",
		},
		"the local baseline was taken from a draft": {
			stage: func(t *testing.T, fx idempotenceFixture) {
				fx.edit(t)
				state, err := wfdir.LoadState(fx.root)
				if err != nil {
					t.Fatal(err)
				}
				for _, inst := range state.Instances {
					inst.State.SourceID, inst.State.SourceLifecycle = "draft-elsewhere", api.LifecycleDraft
				}
				if err := wfdir.SaveState(fx.root, state); err != nil {
					t.Fatal(err)
				}
			},
			why: "nothing here shows the folder descends from it",
		},
		"live is published between the two version reads": {
			stage: func(t *testing.T, fx idempotenceFixture) {
				dropLocalState(t, fx.root)
				published := false
				onRequest(fx, func(r *http.Request) {
					if r.Method == http.MethodGet && strings.HasSuffix(r.URL.Path, "/files") && !published {
						published = true
						fx.draftKind.webPublish(fx.draftKind.liveMain())
					}
				})
			},
			why: "was published while adopt read it",
		},
	}
	for kind, setup := range draftKindFixtures() {
		for name, c := range cases {
			t.Run(kind+"/"+name, func(t *testing.T) {
				fx := setup(t)
				forgetHeadVersion(t, managedFakeOf(fx), fx.root, "prod")
				c.stage(t, fx)
				line := adoptLine(t, fx.tree, "prod", filepath.Base(fx.root))
				onRequest(fx, nil)
				lock, claim := recordedHeadVersion(t, managedFakeOf(fx), fx.root, "prod")
				if lock != "" || claim != "" {
					t.Errorf("recorded %q in the lock and %q on the claim, want nothing", lock, claim)
				}
				if notes := strings.Join(line.Notes, "\n"); !strings.Contains(notes, c.why) {
					t.Errorf("the line does not say why (%q): %s %s\n%s", c.why, line.Verdict, line.Detail, notes)
				}
			})
		}
	}
}

// An adopted folder whose stack records no live version is adopted again
// from a checkout whose local baseline is clean against live: the version is
// recorded, though the folder holds an edit live does not.
func TestReadoptFillsAMissingVersion(t *testing.T) {
	for kind, setup := range draftKindFixtures() {
		t.Run(kind, func(t *testing.T) {
			fx := setup(t)
			want := mustLoadLock(t, fx.root).HeadVersion("prod")
			if want == "" {
				t.Fatal("the clone recorded no live version")
			}
			forgetHeadVersion(t, managedFakeOf(fx), fx.root, "prod")
			fx.edit(t)
			state := filepath.Join(fx.root, wfdir.StateDirName)
			aside := filepath.Join(t.TempDir(), "state")
			if err := os.Rename(state, aside); err != nil {
				t.Fatal(err)
			}
			adoptLine(t, fx.tree, "prod", filepath.Base(fx.root))
			if lock, claim := recordedHeadVersion(t, managedFakeOf(fx), fx.root, "prod"); lock != "" || claim != "" {
				t.Fatalf("the checkout with no baseline recorded %q / %q", lock, claim)
			}

			if err := os.RemoveAll(state); err != nil {
				t.Fatal(err)
			}
			if err := os.Rename(aside, state); err != nil {
				t.Fatal(err)
			}
			adoptLine(t, fx.tree, "prod", filepath.Base(fx.root))
			if lock, claim := recordedHeadVersion(t, managedFakeOf(fx), fx.root, "prod"); lock != want || claim != want {
				t.Errorf("adopting again recorded %q in the lock and %q on the claim, want live's %s", lock, claim, want)
			}
		})
	}
}

// A row never published has no live version to record: adopt records nothing
// and says to publish it once first.
func TestAdoptNamesTheFirstPublishForANeverPublishedRow(t *testing.T) {
	f := newFakeInstance(t)
	signIn(t, f)
	tree := t.TempDir()
	root := filepath.Join(tree, "report")
	if err := os.Mkdir(root, 0o755); err != nil {
		t.Fatal(err)
	}
	if _, err := runCLI(t, root, "wf", "init", "--feature", "feat-1", "--stack", "dev"); err != nil {
		t.Fatalf("init: %v", err)
	}
	writeLocal(t, root, "main.py", "body\n")
	if out, err := runCLI(t, root, "wf", "push"); err != nil {
		t.Fatalf("push: %v\n%s", err, out)
	}
	forgetHeadVersion(t, f.managed, root, "dev")

	line := adoptLine(t, tree, "dev", "report")
	if lock, claim := recordedHeadVersion(t, f.managed, root, "dev"); lock != "" || claim != "" {
		t.Errorf("recorded %q in the lock and %q on the claim, want nothing", lock, claim)
	}
	if notes := strings.Join(line.Notes, "\n"); !strings.Contains(notes, "has never been published") || !strings.Contains(notes, "`ronja wf publish`") {
		t.Errorf("the line does not name the first publish: %s %s\n%s", line.Verdict, line.Detail, notes)
	}
}
