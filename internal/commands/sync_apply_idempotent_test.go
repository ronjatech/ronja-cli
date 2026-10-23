package commands

import (
	"io/fs"
	"maps"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/ronjatech/ronja-cli/internal/api"
	"github.com/ronjatech/ronja-cli/internal/wfdir"
)

// APPLYING AN UNCHANGED REPOSITORY TWICE CHANGES NOTHING AND LEAVES NOTHING
// BEHIND — asserted on what the SERVER was sent, never on what the CLI reports.
//
// One fixture per kind `sync apply` covers.
//
// Each fixture is a folder in agreement with a fake that models the server's
// own state machine: a checkout makes a draft row, a commit moves the draft
// onto live, drops it and files a version. That matters more than anything else
// in the test — the regression here is a request the CLI sends for nothing, and
// a fake that answered every write with 200 and stored nothing would hide the
// draft it leaves behind and the version it mints.
//
// Four variants of each:
//
//   - edited: run one has a change to deploy, so run two is the apply after a
//     real deploy — the state every CI pipeline is in on its next merge;
//   - ci: before EACH run the folder is put back exactly as committed and
//     .ronja/ is gone, which is what a CI runner that throws its checkout away
//     does. The lock then still carries the anchors from before run one's
//     publish, so run two is the stale-lock case the live-equals-desired rule
//     (live_equals_desired.go) exists for.

type idempotenceFixture struct {
	// tree is what apply is pointed at; root is the one folder in it.
	tree, root string
	// requests answers every request the fake has served, as "METHOD /path".
	requests func() []string
	// resetRequests forgets them, so a run is asserted on its own requests.
	resetRequests func()
	// drafts names every draft open on the fake.
	drafts func() []string
	// edit stages a change for run one to deploy.
	edit func(t *testing.T)
	// deployed reports that run one's change reached the LIVE row.
	deployed func() bool

	// The draft kinds only (workflow, data app, module); nil elsewhere.
	draftKind *draftKindHooks
	// Each fixture's own fake; nil on the others. The module fixture's fake is
	// the workflow one (its module arm), held in mod.
	pipeline   *fakePipelineInstance
	wf         *fakeInstance
	mod        *fakeInstance
	app        *fakeAppInstance
	automation *fakeAutomationInstance
}

// draftKindHooks is what the tests below need of the two kinds that stage a
// draft and publish it: the folder's own single-folder push, its title, the
// live row's, the commit, and a publish made in the web app.
type draftKindHooks struct {
	// push is the single-folder push command, run in the folder.
	push []string
	// retitle sets the manifest's "title".
	retitle func(t *testing.T, title string)
	// liveTitle is what the LIVE row is called.
	liveTitle func() string
	// failCommit makes every commit answer this status (0 is success).
	failCommit func(status int)
	// failPut makes every PUT of the folder's main file answer this status (0
	// is success).
	failPut func(status int)
	// openDraftAtCheckout opens the user's own draft, holding content in its
	// main file, at the instant the push checks one out — after its
	// inspection found none.
	openDraftAtCheckout func(content string)
	// commits counts the commits the fake has accepted.
	commits func() int
	// webPublish is a colleague's publish in the web app: the live row's main
	// file becomes content, and the head moves to a new version.
	webPublish func(content string)
	// liveMain is the live row's main file.
	liveMain func() string
}

// webPublishedAt is when a fixture's web publish committed its version: later
// than any baseline a fixture's clone or apply reads.
func webPublishedAt() *time.Time {
	at := time.Date(2026, 9, 1, 12, 0, 0, 0, time.UTC)
	return &at
}

func idempotenceFixtures() map[string]func(t *testing.T) idempotenceFixture {
	return map[string]func(t *testing.T) idempotenceFixture{
		"workflow":   workflowIdempotenceFixture,
		"data app":   appIdempotenceFixture,
		"pipeline":   pipelineIdempotenceFixture,
		"automation": automationIdempotenceFixture,
		"module":     moduleIdempotenceFixture,
	}
}

// moduleIdempotenceFixture is a live module, cloned onto stack prod. Its main
// file is tracker.py; __init__.py rides along, as the package needs it.
func moduleIdempotenceFixture(t *testing.T) idempotenceFixture {
	f := newFakeInstance(t)
	f.AddModule(&api.Module{ID: "module-1", Lifecycle: api.LifecycleLive, Name: "emab_tracker", Title: "EMAB tracker"},
		api.ModuleFile{Path: moduleInitFile, Content: fakeModuleInitSeed},
		api.ModuleFile{Path: "tracker.py", Content: "live\n"})
	f.mod.versions["module-1"] = []api.Module{{ID: "modv-1"}}
	signIn(t, f)
	out, err := runCLI(t, t.TempDir(), "module", "clone", "module-1", "--stack", "prod", "--json")
	if err != nil {
		t.Fatalf("clone: %v\n%s", err, out)
	}
	root := decodeJSON(t, out)["root"].(string)
	liveMain := func() string { return f.ModuleFileContents("module-1")["tracker.py"] }
	return idempotenceFixture{
		tree: filepath.Dir(root), root: root, mod: f,
		requests:      func() []string { f.mu.Lock(); defer f.mu.Unlock(); return slices.Clone(f.Requests) },
		resetRequests: func() { f.mu.Lock(); defer f.mu.Unlock(); f.Requests = nil },
		drafts:        func() []string { return slices.Sorted(maps.Values(f.mod.draftOf)) },
		edit:          func(t *testing.T) { writeLocal(t, root, "tracker.py", "deployed\n") },
		deployed:      func() bool { return liveMain() == "deployed\n" },
		draftKind: &draftKindHooks{
			push:       []string{"module", "push"},
			retitle:    func(t *testing.T, title string) { setModuleManifestTitle(t, root, title) },
			liveTitle:  func() string { return f.mod.rows["module-1"].Title },
			failCommit: func(status int) { f.mod.failCommit = status },
			failPut:    func(status int) { f.mod.failPut["tracker.py"] = status },
			openDraftAtCheckout: func(content string) {
				f.mod.beforeCheckout = func(liveID string) {
					if _, open := f.mod.draftOf[liveID]; open {
						return
					}
					live := f.mod.rows[liveID]
					draft := f.AddModule(&api.Module{
						ID: "module-draft-the-authors", ParentModuleID: liveID, Lifecycle: api.LifecycleDraft,
						FeatureID: live.FeatureID, Name: live.Name, Title: live.Title, DrafterUserID: fakeCallerUserID,
						BaseVersionID: f.moduleHeadVersionID(liveID), UpdatedAt: live.UpdatedAt.Add(time.Hour),
					}, api.ModuleFile{Path: moduleInitFile, Content: fakeModuleInitSeed}, api.ModuleFile{Path: "tracker.py", Content: content})
					f.mod.draftOf[liveID] = draft.ID
				}
			},
			commits: func() int { return len(f.mod.committed) },
			webPublish: func(content string) {
				f.snapshotModuleHead("module-1")
				f.putModuleFile(f.mod.rows["module-1"], "tracker.py", content)
				f.mod.versions["module-1"] = append([]api.Module{{ID: "modv-web", CommittedAt: webPublishedAt()}}, f.mod.versions["module-1"]...)
			},
			liveMain: liveMain,
		},
	}
}

func workflowIdempotenceFixture(t *testing.T) idempotenceFixture {
	f := newFakeInstance(t)
	// A second file, so an entrypoint change can be the ONLY difference.
	f.AddWorkflow(&api.Workflow{ID: "wf-1", Lifecycle: api.LifecycleLive, FeatureScope: "private", Title: "Monthly report",
		RuntimeVersion: wfdir.RuntimeDefault},
		api.WorkflowFile{Path: "main.py", Content: "live\n"}, api.WorkflowFile{Path: "helper.py", Content: "def main():\n    pass\n"})
	f.versions["wf-1"] = []api.Workflow{{ID: "wfv-1"}}
	signIn(t, f)
	out, err := runCLI(t, t.TempDir(), "wf", "clone", "wf-1", "--stack", "prod", "--json")
	if err != nil {
		t.Fatalf("clone: %v", err)
	}
	root := decodeJSON(t, out)["root"].(string)
	return idempotenceFixture{
		tree: filepath.Dir(root), root: root, wf: f,
		requests:      func() []string { f.mu.Lock(); defer f.mu.Unlock(); return slices.Clone(f.Requests) },
		resetRequests: func() { f.mu.Lock(); defer f.mu.Unlock(); f.Requests = nil },
		drafts:        func() []string { return slices.Sorted(maps.Values(f.draftOf)) },
		edit:          func(t *testing.T) { writeLocal(t, root, "main.py", "deployed\n") },
		deployed:      func() bool { return f.FileContents("wf-1")["main.py"] == "deployed\n" },
		draftKind: &draftKindHooks{
			push: []string{"wf", "push"},
			retitle: func(t *testing.T, title string) {
				m, err := wfdir.LoadManifest(root, wfdir.WorkflowKind)
				if err != nil {
					t.Fatal(err)
				}
				m.Title = title
				if err := wfdir.SaveManifest(root, m); err != nil {
					t.Fatal(err)
				}
			},
			liveTitle:  func() string { return f.workflows["wf-1"].Title },
			failCommit: func(status int) { f.failCommit = status },
			failPut:    func(status int) { f.failPut["main.py"] = status },
			openDraftAtCheckout: func(content string) {
				f.beforeCheckout = func(liveID string) {
					if _, open := f.draftOf[liveID]; !open {
						f.AddDraft(liveID, "draft-the-authors", api.WorkflowFile{Path: "main.py", Content: content})
					}
				}
			},
			commits: func() int { return len(f.committed) },
			webPublish: func(content string) {
				f.snapshotHead("wf-1")
				f.putFile("wf-1", "main.py", content)
				f.versions["wf-1"] = append([]api.Workflow{{ID: "wfv-web", CommittedAt: webPublishedAt()}}, f.versions["wf-1"]...)
			},
			liveMain: func() string { return f.FileContents("wf-1")["main.py"] },
		},
	}
}

func appIdempotenceFixture(t *testing.T) idempotenceFixture {
	f := newFakeAppInstance(t)
	f.AddApp(&api.DataApp{ID: "app-1", Lifecycle: api.LifecycleLive},
		api.DataAppFile{Path: "App.tsx", Content: "live\n"})
	f.versions["app-1"] = []api.DataApp{{ID: "dav-1"}}
	f.featureScope["feat-1"] = "private"
	signInApp(t, f)
	out, err := runCLI(t, t.TempDir(), "app", "clone", "app-1", "--stack", "prod", "--json")
	if err != nil {
		t.Fatalf("clone: %v", err)
	}
	root := decodeJSON(t, out)["root"].(string)
	liveMain := func() string {
		for _, file := range f.files["app-1"] {
			if file.Path == "App.tsx" {
				return file.Content
			}
		}
		return ""
	}
	return idempotenceFixture{
		tree: filepath.Dir(root), root: root, app: f,
		requests:      func() []string { return slices.Clone(f.Requests) },
		resetRequests: func() { f.Requests = nil },
		drafts:        func() []string { return slices.Sorted(maps.Values(f.draftOf)) },
		edit:          func(t *testing.T) { writeLocal(t, root, "App.tsx", "deployed\n") },
		deployed:      func() bool { return liveMain() == "deployed\n" },
		draftKind: &draftKindHooks{
			push:       []string{"app", "push"},
			retitle:    func(t *testing.T, title string) { setAppTitle(t, root, title) },
			liveTitle:  func() string { return f.apps["app-1"].Name },
			failCommit: func(status int) { f.failCommit = status },
			failPut:    func(status int) { f.failPut["App.tsx"] = status },
			openDraftAtCheckout: func(content string) {
				f.beforeCheckout = func(liveID string) {
					if _, open := f.draftOf[liveID]; !open {
						f.AddDraft(liveID, "data_app-draft-the-authors", api.DataAppFile{Path: "App.tsx", Content: content})
					}
				}
			},
			commits: func() int { return len(f.committed) },
			webPublish: func(content string) {
				f.snapshotHead("app-1")
				f.files["app-1"] = []api.DataAppFile{{DataAppID: "app-1", Path: "App.tsx", Content: content}}
				f.versions["app-1"] = append([]api.DataApp{{ID: "dav-web", CommittedAt: webPublishedAt()}}, f.versions["app-1"]...)
			},
			liveMain: liveMain,
		},
	}
}

// The pipeline fixture carries every unit a pipeline folder deploys: a table's
// SQL, a metric that declares its calendar, the table's health checks, and a
// docs sidecar for a table the folder documents but does not build.
//
// The calendar and the sidecar are here because both kept writing on every
// fresh-checkout run after the rest had stopped: each was skipped only on a
// recording the CI checkout's lock carries from before the last apply.
func pipelineIdempotenceFixture(t *testing.T) idempotenceFixture {
	f := newFakePipelineInstance(t)
	signInPipeline(t, f)
	f.AddFeature("collection-1", "Sales pipeline", "private")
	f.AddTable(&api.Table{ID: "table-orders", Name: "orders", FeatureID: "collection-1", Code: "SELECT * FROM raw"})
	// The source a table id: aovRecipe's "orders" is the authoring form, which
	// no stored row holds. AddMetric stores it as the server would (canonical).
	f.AddMetric("metric-aov", "aov", strings.Replace(aovRecipe, `"source":"orders"`, `"source":"table-orders"`, 1)).
		ReportingTimezone = "Europe/Stockholm"
	f.AddCheck("table-orders", api.TableCheck{Name: "Unik rad per id", Expression: "count(*) = count(distinct id)", Enabled: true})
	// The sidecar's table, already saying what the sidecar will.
	f.AddTable(&api.Table{ID: "table-raw", Name: "Fortnox invoices", FeatureID: "collection-1", Kind: "integration",
		Description: "One row per invoice line.", DescriptionSource: api.DescriptionSourceUser})
	f.fields["table-raw"] = []string{"invoice_no", "amount"}
	f.columnDocs["table-raw"] = map[string]string{"amount": "line total"}
	dir := t.TempDir()
	if _, stderr, err := runPipelineCLI(t, dir, "pipeline", "clone", "collection-1", "out", "--stack", "prod", "--json"); err != nil {
		t.Fatalf("clone: %v\n%s", err, stderr)
	}
	root := filepath.Join(dir, "out")
	// A clone writes no calendar into a metric file and no sidecar at all, so
	// both are authored here, as an author would — and each already agrees with
	// its live row, which is what an unchanged repository is.
	writeLocal(t, root, "metrics/aov.json", metricFileWithZone(f.RecipeOf("metric-aov"), "Europe/Stockholm"))
	manifest := pipelineManifestOf(t, root)
	manifest.Dependencies = map[string]wfdir.Dependency{"fortnox_invoices": {Kind: "table"}}
	stack := manifest.Stacks["prod"]
	stack.Bind = map[string]string{"fortnox_invoices": "table-raw"}
	manifest.Stacks["prod"] = stack
	if err := wfdir.SaveManifest(root, manifest); err != nil {
		t.Fatal(err)
	}
	writeSidecar(t, root, "fortnox_invoices", `{"description": "One row per invoice line.", "columns": {"amount": "line total"}}`)
	return idempotenceFixture{
		tree: dir, root: root, pipeline: f,
		requests:      func() []string { f.mu.Lock(); defer f.mu.Unlock(); return slices.Clone(f.Requests) },
		resetRequests: func() { f.mu.Lock(); defer f.mu.Unlock(); f.Requests = nil },
		drafts:        func() []string { f.mu.Lock(); defer f.mu.Unlock(); return slices.Sorted(maps.Values(f.draftOf)) },
		edit: func(t *testing.T) {
			writeLocal(t, root, "orders.sql", "SELECT * FROM raw WHERE shipped")
			// Hand-written, as the guide's example is: no "version", no "time",
			// no "dimensions". The server stores all three filled in, so the
			// second run's live-first skip has to compare the file in that form
			// (sameRecipe) or it re-publishes the metric every run.
			writeLocal(t, root, "metrics/aov.json", metricFileWithZone(
				`{"value":"revenue / orders","source":"table-orders","base_measures":[{"name":"revenue","agg":"sum","column":"net_amount"}]}`,
				"Europe/Stockholm"))
			writeLocal(t, root, "checks/orders.json",
				`{"checks":[{"name":"Unik rad per id","expression":"count(*) = count(distinct order_id)"}]}`)
			writeSidecar(t, root, "fortnox_invoices", `{"description": "One row per Fortnox invoice line.", "columns": {"amount": "line total"}}`)
		},
		deployed: func() bool {
			if f.CodeOf("table-orders") != "SELECT * FROM raw WHERE shipped" {
				return false
			}
			if !strings.Contains(f.RecipeOf("metric-aov"), "net_amount") {
				return false
			}
			f.mu.Lock()
			defer f.mu.Unlock()
			if f.tables["table-raw"].Description != "One row per Fortnox invoice line." {
				return false
			}
			for _, check := range f.checkState.rows["table-orders"] {
				if check.Expression == "count(*) = count(distinct order_id)" {
					return true
				}
			}
			return false
		},
	}
}

// metricFileWithZone is a metric file declaring its calendar.
func metricFileWithZone(recipe, zone string) string {
	return `{"recipe":` + recipe + `,"reportingTimezone":` + strconv.Quote(zone) + `}`
}

func automationIdempotenceFixture(t *testing.T) idempotenceFixture {
	f := newFakeAutomationInstance(t)
	signInAutomation(t, f)
	f.AddAutomation(&api.Automation{ID: "job-1", Name: "nightly", CronExpr: "0 2 * * *", Enabled: true})
	tree := t.TempDir()
	root := filepath.Join(tree, "orders")
	if err := os.MkdirAll(root, 0o755); err != nil {
		t.Fatal(err)
	}
	manifest, lock := automationStack(f, "collection-1",
		map[string]string{"nightly.json": "job-1"},
		map[string]string{"nightly.json": anchoredAt(baseTime)})
	writeAutomationFolder(t, root, manifest, lock, map[string]string{"nightly.json": `{"cronExpr": "0 2 * * *"}`})
	return idempotenceFixture{
		tree: tree, root: root, automation: f,
		requests:      f.requestOrder,
		resetRequests: func() { f.mu.Lock(); defer f.mu.Unlock(); f.Requests = nil },
		// An automation has no draft: the loop writes the row directly.
		drafts:   func() []string { return nil },
		edit:     func(t *testing.T) { writeLocal(t, root, "nightly.json", `{"cronExpr": "0 3 * * *"}`) },
		deployed: func() bool { return f.RowOf("job-1").CronExpr == "0 3 * * *" },
	}
}

// writesIn is every request that is not a read. ANY non-GET counts — a draft
// checkout, a validate, a no-op patch — because each is a request an unchanged
// repository had no reason to send.
func writesIn(requests []string) []string {
	var out []string
	for _, r := range requests {
		if !strings.HasPrefix(r, "GET ") {
			out = append(out, r)
		}
	}
	return out
}

// writesBesidesGate is writesIn without the write gate's own request: POST
// /workflow/validate persists nothing, and an ADOPTED workflow folder's gate
// asks it on every deploy and every dry run, changed or not. A classic folder
// sends none when nothing changed, which writesIn still holds it to.
func writesBesidesGate(requests []string) []string {
	var out []string
	for _, w := range writesIn(requests) {
		if w != "POST /api/v2/workflow/validate" {
			out = append(out, w)
		}
	}
	return out
}

// snapshotFolder reads every file under root, keyed by its slash path. With
// committedOnly it leaves out .ronja/, which is git-ignored — what a fresh
// checkout of the folder holds.
func snapshotFolder(t *testing.T, root string, committedOnly bool) map[string]string {
	t.Helper()
	out := map[string]string{}
	err := filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		rel, _ := filepath.Rel(root, path)
		if d.IsDir() {
			if committedOnly && rel == wfdir.StateDirName {
				return filepath.SkipDir
			}
			return nil
		}
		body, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		out[filepath.ToSlash(rel)] = string(body)
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	return out
}

// restoreCheckout puts a folder back to exactly a committed snapshot, the way a
// CI runner's fresh checkout does: everything else, .ronja/ included, is gone.
func restoreCheckout(t *testing.T, root string, committed map[string]string) {
	t.Helper()
	if err := os.RemoveAll(root); err != nil {
		t.Fatal(err)
	}
	for path, body := range committed {
		writeLocal(t, root, path, body)
	}
}

func TestSyncApplyTwiceSendsNoWriteTheSecondTime(t *testing.T) {
	for kind, setup := range idempotenceFixtures() {
		for _, edited := range []bool{false, true} {
			for _, ci := range []bool{false, true} {
				name := kind
				if edited {
					name += "/edited"
				} else {
					name += "/unchanged"
				}
				if ci {
					name += "/fresh-checkout"
				} else {
					name += "/same-checkout"
				}
				t.Run(name, func(t *testing.T) {
					fx := setup(t)
					if edited {
						fx.edit(t)
					}
					committed := snapshotFolder(t, fx.root, true)

					var afterRunOne map[string]string
					for run := 1; run <= 2; run++ {
						if ci {
							restoreCheckout(t, fx.root, committed)
						}
						fx.resetRequests()
						out, err := runCLI(t, fx.tree, "sync", "apply", "--stack", "prod", "--json")
						if code := exitCodeOf(err); code != syncExitClean {
							t.Fatalf("run %d: exit = %d (%v), want %d\n%s", run, code, err, syncExitClean, out)
						}
						writes := writesIn(fx.requests())
						switch {
						case run == 1 && edited:
							// The fixture deploys: without this, "run two wrote nothing"
							// could be a run one that never did anything either.
							if !fx.deployed() {
								t.Fatalf("run 1 did not deploy the change\n%s", out)
							}
						case len(writes) != 0:
							t.Fatalf("run %d of an unchanged repository sent %d write(s): %v\n%s", run, len(writes), writes, out)
						}
						// Nothing left behind on the server after any run.
						if drafts := fx.drafts(); len(drafts) != 0 {
							t.Fatalf("run %d left draft(s) open: %v\n%s", run, drafts, out)
						}
						if run == 1 {
							afterRunOne = snapshotFolder(t, fx.root, false)
							continue
						}
						// And nothing left behind locally: a second run that rewrote
						// the lock or the baseline describing the same state is a
						// diff in somebody's working tree for no reason.
						if !ci {
							if after := snapshotFolder(t, fx.root, false); !maps.Equal(after, afterRunOne) {
								t.Errorf("run 2 rewrote the folder:\nbefore %v\nafter  %v", afterRunOne, after)
							}
						}
						if line := applyFolderLine(t, applyReportOf(t, out), filepath.Base(fx.root)); line.Verdict != verdictApplied {
							t.Errorf("run 2 folder = %+v, want %s", line, verdictApplied)
						}
					}
				})
			}
		}
	}
}

// draftKindFixtures is the workflow, data-app and module fixtures, the kinds
// whose push stages a draft and whose apply publishes it.
func draftKindFixtures() map[string]func(t *testing.T) idempotenceFixture {
	return map[string]func(t *testing.T) idempotenceFixture{
		"workflow": workflowIdempotenceFixture,
		"data app": appIdempotenceFixture,
		"module":   moduleIdempotenceFixture,
	}
}

// applyOnce runs `sync apply` over the fixture's tree with its requests reset,
// and answers the exit code, the folder's line and the writes it sent — besides
// an adopted folder's gate validate (writesBesidesGate); a classic fixture's
// strict "nothing at all" is TestSyncApplyTwiceSendsNoWriteTheSecondTime's.
func applyOnce(t *testing.T, fx idempotenceFixture) (int, syncApplyFolderReport, []string, string) {
	t.Helper()
	fx.resetRequests()
	out, err := runCLI(t, fx.tree, "sync", "apply", "--stack", "prod", "--json")
	line := applyFolderLine(t, applyReportOf(t, out), filepath.Base(fx.root))
	return exitCodeOf(err), line, writesBesidesGate(fx.requests()), out
}

// A METADATA-ONLY CHANGE IS A CHANGE. Every live-first skip asks "does the live
// row already hold this folder", and each is only as good as its parity with
// what the push WRITES: an attribute the push writes but the skip does not
// compare reads as "live already holds it", and the change never deploys. One
// row per attribute, each the ONLY difference between the folder and live, each
// applied from the same checkout and from a fresh one — and run two then writes
// nothing.
//
// The two pipeline header rows put the header's SQL on the live row first (a
// colleague published the same bytes in the web app without the prose or the
// rename): a header is part of the SQL, so an ordinary header edit is a code
// change too, and only this state makes the skip's documentation and name
// clauses the deciding ones.
func TestSyncApplyDeploysAMetadataOnlyChange(t *testing.T) {
	type row struct {
		setup  func(t *testing.T) idempotenceFixture
		change func(t *testing.T, fx idempotenceFixture)
		landed func(fx idempotenceFixture) bool
	}
	editManifest := func(t *testing.T, root string, kind wfdir.Kind, edit func(m *wfdir.Manifest)) {
		t.Helper()
		m, err := wfdir.LoadManifest(root, kind)
		if err != nil {
			t.Fatal(err)
		}
		edit(m)
		if err := wfdir.SaveManifest(root, m); err != nil {
			t.Fatal(err)
		}
	}
	liveWorkflow := func(fx idempotenceFixture) api.Workflow {
		fx.wf.mu.Lock()
		defer fx.wf.mu.Unlock()
		return *fx.wf.workflows["wf-1"]
	}
	liveTable := func(fx idempotenceFixture, id string) api.Table {
		fx.pipeline.mu.Lock()
		defer fx.pipeline.mu.Unlock()
		return *fx.pipeline.tables[id]
	}
	// webPublishSQL puts these bytes on the live table as a colleague's web
	// publish would — the SQL only.
	webPublishSQL := func(fx idempotenceFixture, sql string) {
		fx.pipeline.mu.Lock()
		defer fx.pipeline.mu.Unlock()
		fx.pipeline.tables["table-orders"].Code = sql
	}
	rows := map[string]row{
		"workflow/title": {
			setup:  workflowIdempotenceFixture,
			change: func(t *testing.T, fx idempotenceFixture) { fx.draftKind.retitle(t, "Renamed in the repository") },
			landed: func(fx idempotenceFixture) bool { return liveWorkflow(fx).Title == "Renamed in the repository" },
		},
		"workflow/entrypoint": {
			setup: workflowIdempotenceFixture,
			change: func(t *testing.T, fx idempotenceFixture) {
				editManifest(t, fx.root, wfdir.WorkflowKind, func(m *wfdir.Manifest) { m.Entrypoint = "helper.py" })
			},
			landed: func(fx idempotenceFixture) bool { return liveWorkflow(fx).Entrypoint == "helper.py" },
		},
		"workflow/parameters": {
			setup: workflowIdempotenceFixture,
			change: func(t *testing.T, fx idempotenceFixture) {
				editManifest(t, fx.root, wfdir.WorkflowKind, func(m *wfdir.Manifest) {
					m.SetParameters([]api.WorkflowParameter{{Name: "month", Label: "Month", Type: "string"}})
				})
			},
			landed: func(fx idempotenceFixture) bool {
				params := liveWorkflow(fx).Parameters
				return len(params) == 1 && params[0].Name == "month"
			},
		},
		"workflow/calendar": {
			setup: workflowIdempotenceFixture,
			change: func(t *testing.T, fx idempotenceFixture) {
				editManifest(t, fx.root, wfdir.WorkflowKind, func(m *wfdir.Manifest) { m.SetReportingTimezone("Europe/Stockholm") })
			},
			landed: func(fx idempotenceFixture) bool { return liveWorkflow(fx).ReportingTimezone == "Europe/Stockholm" },
		},
		"workflow/runtime upgrade": {
			setup: workflowIdempotenceFixture,
			change: func(t *testing.T, fx idempotenceFixture) {
				editManifest(t, fx.root, wfdir.WorkflowKind, func(m *wfdir.Manifest) { m.Runtime = wfdir.RuntimeQuery })
			},
			landed: func(fx idempotenceFixture) bool { return liveWorkflow(fx).RuntimeVersion == wfdir.RuntimeQuery },
		},
		"data app/title": {
			setup:  appIdempotenceFixture,
			change: func(t *testing.T, fx idempotenceFixture) { fx.draftKind.retitle(t, "Renamed in the repository") },
			landed: func(fx idempotenceFixture) bool { return fx.draftKind.liveTitle() == "Renamed in the repository" },
		},
		"data app/access": {
			setup: appIdempotenceFixture,
			change: func(t *testing.T, fx idempotenceFixture) {
				editManifest(t, fx.root, wfdir.DataAppKind, func(m *wfdir.Manifest) {
					m.SetAccess(api.DataAppAccess{AllowedTableIDs: []string{"table-orders"}})
				})
			},
			landed: func(fx idempotenceFixture) bool {
				return slices.Equal(fx.app.apps["app-1"].DataAppAccess.AllowedTableIDs, []string{"table-orders"})
			},
		},
		"module/title": {
			setup:  moduleIdempotenceFixture,
			change: func(t *testing.T, fx idempotenceFixture) { fx.draftKind.retitle(t, "Renamed in the repository") },
			landed: func(fx idempotenceFixture) bool { return fx.draftKind.liveTitle() == "Renamed in the repository" },
		},
		"module/package name": {
			setup: moduleIdempotenceFixture,
			change: func(t *testing.T, fx idempotenceFixture) {
				editManifest(t, fx.root, wfdir.ModuleKind, func(m *wfdir.Manifest) { m.Name = "emab_tracker_v2" })
			},
			landed: func(fx idempotenceFixture) bool { return fx.mod.mod.rows["module-1"].Name == "emab_tracker_v2" },
		},
		"pipeline/table documentation": {
			setup: pipelineIdempotenceFixture,
			change: func(t *testing.T, fx idempotenceFixture) {
				writeLocal(t, fx.root, "orders.sql", "-- @table One row per order.\nSELECT * FROM raw")
			},
			landed: func(fx idempotenceFixture) bool {
				return liveTable(fx, "table-orders").Description == "One row per order."
			},
		},
		"pipeline/table documentation, SQL already live": {
			setup: pipelineIdempotenceFixture,
			change: func(t *testing.T, fx idempotenceFixture) {
				sql := "-- @table One row per order.\nSELECT * FROM raw"
				writeLocal(t, fx.root, "orders.sql", sql)
				webPublishSQL(fx, sql)
			},
			landed: func(fx idempotenceFixture) bool {
				return liveTable(fx, "table-orders").Description == "One row per order."
			},
		},
		"pipeline/table name, SQL already live": {
			setup: pipelineIdempotenceFixture,
			change: func(t *testing.T, fx idempotenceFixture) {
				sql := "-- @name orders_v2\nSELECT * FROM raw"
				writeLocal(t, fx.root, "orders.sql", sql)
				webPublishSQL(fx, sql)
			},
			landed: func(fx idempotenceFixture) bool { return liveTable(fx, "table-orders").Name == "orders_v2" },
		},
		"pipeline/metric description": {
			setup: pipelineIdempotenceFixture,
			change: func(t *testing.T, fx idempotenceFixture) {
				body := strings.TrimSuffix(metricFileWithZone(fx.pipeline.RecipeOf("metric-aov"), "Europe/Stockholm"), "}") +
					`,"description":"Average order value."}`
				writeLocal(t, fx.root, "metrics/aov.json", body)
			},
			landed: func(fx idempotenceFixture) bool {
				return liveTable(fx, "metric-aov").Description == "Average order value."
			},
		},
		"pipeline/metric calendar": {
			setup: pipelineIdempotenceFixture,
			change: func(t *testing.T, fx idempotenceFixture) {
				writeLocal(t, fx.root, "metrics/aov.json", metricFileWithZone(fx.pipeline.RecipeOf("metric-aov"), "America/New_York"))
			},
			landed: func(fx idempotenceFixture) bool {
				return liveTable(fx, "metric-aov").ReportingTimezone == "America/New_York"
			},
		},
	}
	for name, tc := range rows {
		for _, fresh := range []bool{false, true} {
			variant := name + "/same-checkout"
			if fresh {
				variant = name + "/fresh-checkout"
			}
			t.Run(variant, func(t *testing.T) {
				fx := tc.setup(t)
				if fresh {
					// A repository that has been applied before and committed, as
					// any CI repository has: the first apply records what a clone
					// leaves out (a workflow's applied title, which no head anchor
					// can vouch for), and its lock is what every later checkout
					// starts from.
					if code, line, _, out := applyOnce(t, fx); code != syncExitClean {
						t.Fatalf("the first apply: exit = %d, folder = %+v\n%s", code, line, out)
					}
					restoreCheckout(t, fx.root, snapshotFolder(t, fx.root, true))
				}
				tc.change(t, fx)
				if tc.landed(fx) {
					t.Fatal("the change is already live before apply — the row tests nothing")
				}
				committed := snapshotFolder(t, fx.root, true)
				if fresh {
					restoreCheckout(t, fx.root, committed)
				}
				code, line, _, out := applyOnce(t, fx)
				if code != syncExitClean || line.Verdict != verdictApplied {
					t.Fatalf("exit = %d, folder = %+v\n%s", code, line, out)
				}
				if !tc.landed(fx) {
					t.Fatalf("apply reported the folder deployed, but the change is not live\n%s", out)
				}
				if drafts := fx.drafts(); len(drafts) != 0 {
					t.Fatalf("apply left draft(s) %v\n%s", drafts, out)
				}
				if fresh {
					restoreCheckout(t, fx.root, committed)
				}
				code, _, writes, out := applyOnce(t, fx)
				if code != syncExitClean || len(writes) != 0 {
					t.Fatalf("run 2: exit = %d, writes = %v\n%s", code, writes, out)
				}
			})
		}
	}
}

// TestDryRunSaysWhatLiveAlreadyHolds is QA's dry-run finding: an unchanged
// tree's dry run listed every bound row as an update and said it would push
// and publish each, while the apply wrote nothing. Every kind's dry run now
// makes the comparison apply makes — does the live row hold the folder, per
// FILE for a pipeline (its tables, metrics, docs and checks files) and an
// automation folder — and says `unchanged` / nothing to push or publish (or
// write), with reads only; a changed one says it would.
func TestDryRunSaysWhatLiveAlreadyHolds(t *testing.T) {
	for kind, setup := range idempotenceFixtures() {
		t.Run(kind, func(t *testing.T) {
			fx := setup(t)
			adoptFixture(t, fx)
			if code, line, _, out := applyOnce(t, fx); code != syncExitClean {
				t.Fatalf("first apply: exit %d %+v\n%s", code, line, out)
			}
			dryRun := func() syncApplyFolderReport {
				t.Helper()
				fx.resetRequests()
				out, err := runCLI(t, fx.tree, "sync", "apply", "--stack", "prod", "--dry-run", "--json")
				if err != nil {
					t.Fatalf("dry run: %v\n%s", err, out)
				}
				if w := writesBesidesGate(fx.requests()); len(w) != 0 {
					t.Fatalf("dry run sent %v", w)
				}
				return applyFolderLine(t, applyReportOf(t, out), filepath.Base(fx.root))
			}
			line := dryRun()
			if fx.draftKind == nil {
				if actions := unitActions(line); len(actions) == 0 || slices.ContainsFunc(slices.Collect(maps.Values(actions)), func(a string) bool { return a != string(applyActionUnchanged) }) {
					t.Fatalf("dry run of an unchanged folder: %v\n%+v", actions, line)
				}
				if !strings.HasPrefix(line.Detail, "nothing to ") {
					t.Fatalf("dry run of an unchanged folder says %q", line.Detail)
				}
				fx.edit(t)
				line = dryRun()
				if !slices.Contains(slices.Collect(maps.Values(unitActions(line))), string(applyActionUpdate)) || !strings.HasPrefix(line.Detail, "would ") {
					t.Fatalf("dry run of a changed folder: %+v", line)
				}
				return
			}
			if len(line.Units) != 1 || line.Units[0].Action != string(applyActionUnchanged) || !strings.Contains(line.Detail, "nothing to push or publish") {
				t.Fatalf("dry run of an unchanged folder: %+v", line)
			}
			fx.edit(t)
			line = dryRun()
			if len(line.Units) != 1 || line.Units[0].Action != string(applyActionUpdate) || !strings.Contains(line.Detail, "would push and publish") {
				t.Fatalf("dry run of a changed folder: %+v", line)
			}
		})
	}
}

// One changed file of an otherwise unchanged pipeline folder is the one
// `update`: the metric, the docs file and the checks file beside it stay
// `unchanged`, because each is asked its own push's question.
func TestDryRunComparesAPipelineFolderFileByFile(t *testing.T) {
	fx := pipelineIdempotenceFixture(t)
	adoptFixture(t, fx)
	if code, line, _, out := applyOnce(t, fx); code != syncExitClean {
		t.Fatalf("first apply: exit %d %+v\n%s", code, line, out)
	}
	writeLocal(t, fx.root, "orders.sql", "SELECT * FROM raw WHERE shipped")
	fx.resetRequests()
	out, err := runCLI(t, fx.tree, "sync", "apply", "--stack", "prod", "--dry-run", "--json")
	if err != nil {
		t.Fatalf("dry run: %v\n%s", err, out)
	}
	if w := writesIn(fx.requests()); len(w) != 0 {
		t.Fatalf("dry run sent %v", w)
	}
	line := applyFolderLine(t, applyReportOf(t, out), filepath.Base(fx.root))
	want := map[string]string{
		"orders.sql":                   string(applyActionUpdate),
		"metrics/aov.json":             string(applyActionUnchanged),
		"tables/fortnox_invoices.json": string(applyActionUnchanged),
		"checks/orders.json":           string(applyActionUnchanged),
	}
	if got := unitActions(line); !maps.Equal(got, want) {
		t.Fatalf("units = %v, want %v\n%+v", got, want, line)
	}
	if line.Detail != "would push and publish 1 of its 4 files, which differ from what is live" {
		t.Errorf("detail = %q", line.Detail)
	}
}

// The comparison keeps what the plan already said about a unit: a row the
// server holds no claim for is one apply claims before anything else, and a
// dry run that called it `unchanged` without saying so would hide that write.
func TestDryRunKeepsTheClaimNoteOnAComparedUnit(t *testing.T) {
	fx := automationIdempotenceFixture(t)
	adoptFixture(t, fx)
	if code, line, _, out := applyOnce(t, fx); code != syncExitClean {
		t.Fatalf("first apply: exit %d %+v\n%s", code, line, out)
	}
	fx.automation.managed.mu.Lock()
	clear(fx.automation.managed.claims)
	fx.automation.managed.mu.Unlock()
	out, err := runCLI(t, fx.tree, "sync", "apply", "--stack", "prod", "--dry-run", "--json")
	if err != nil {
		t.Fatalf("dry run: %v\n%s", err, out)
	}
	line := applyFolderLine(t, applyReportOf(t, out), filepath.Base(fx.root))
	if len(line.Units) != 1 || line.Units[0].Action != string(applyActionUnchanged) ||
		!strings.Contains(line.Units[0].Detail, "the server has no claim for it yet") {
		t.Fatalf("units = %+v", line.Units)
	}
}
