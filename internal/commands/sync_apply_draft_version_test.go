package commands

import (
	"fmt"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/ronjatech/ronja-cli/internal/api"
	"github.com/ronjatech/ronja-cli/internal/wfdir"
)

// draftVersionHooks is what the precondition tests need of a draft kind's
// fixture: the versions its commits sent, and a write to the open draft as
// another writer (an agent, the web app) would make it.
type draftVersionHooks struct {
	sent        func() []string
	agentWrites func()
	setHook     func(func(r *http.Request))
}

func draftVersionHooksOf(fx idempotenceFixture) draftVersionHooks {
	switch {
	case fx.wf != nil:
		f := fx.wf
		return draftVersionHooks{
			sent:        func() []string { return f.draftVersions },
			agentWrites: func() { f.putFile(f.draftOf["wf-1"], "main.py", "agent\n") },
			setHook:     func(h func(*http.Request)) { f.beforeServe = h },
		}
	case fx.app != nil:
		f := fx.app
		return draftVersionHooks{
			sent:        func() []string { return f.draftVersions },
			agentWrites: func() { f.putFile(f.draftOf["app-1"], "App.tsx", "agent\n") },
			setHook:     func(h func(*http.Request)) { f.beforeServe = h },
		}
	default:
		f := fx.mod
		return draftVersionHooks{
			sent:        func() []string { return f.mod.draftVersions },
			agentWrites: func() { f.putModuleFile(f.mod.rows[f.mod.draftOf["module-1"]], "tracker.py", "agent\n") },
			setHook:     func(h func(*http.Request)) { f.beforeServe = h },
		}
	}
}

// Every draft kind's commit carries the version of the draft the run checked,
// and a write to the draft after that check, before the commit lands, is
// refused by the server: the folder is not applied and live keeps what it had.
func TestSyncApplyPublishesOnlyTheDraftItChecked(t *testing.T) {
	for name, fixture := range draftKindFixtures() {
		t.Run(name+"/checked", func(t *testing.T) {
			fx := fixture(t)
			hooks := draftVersionHooksOf(fx)
			fx.edit(t)
			code, line, _, out := applyOnce(t, fx)
			if code != syncExitClean || !fx.deployed() {
				t.Fatalf("exit %d, %s: %s\n%s", code, line.Verdict, line.Detail, out)
			}
			sent := hooks.sent()
			if len(sent) != 1 || sent[0] == "" {
				t.Fatalf("the commit sent ifDraftVersion %q, want the checked draft's version", sent)
			}
		})
		t.Run(name+"/written-after-the-check", func(t *testing.T) {
			fx := fixture(t)
			hooks := draftVersionHooksOf(fx)
			fx.edit(t)
			hooks.setHook(func(r *http.Request) {
				if r.Method == http.MethodPost && strings.HasSuffix(r.URL.Path, "/commit") {
					hooks.agentWrites()
				}
			})
			code, line, _, out := applyOnce(t, fx)
			if code == syncExitClean || fx.deployed() || line.Verdict == verdictApplied {
				t.Fatalf("exit %d, %s, deployed %v: a draft written after the check went live\n%s", code, line.Verdict, fx.deployed(), out)
			}
			if fx.draftKind.liveMain() == "agent\n" {
				t.Fatal("live holds the later write")
			}
			if !strings.Contains(line.Detail, "nothing was published") {
				t.Fatalf("detail %q does not say nothing was published", line.Detail)
			}
		})
	}
}

// A write to the draft after the push and before the publish's check: the run
// sees the draft no longer holds the folder and commits nothing.
func TestSyncApplyRefusesAWorkflowDraftWrittenAfterThePush(t *testing.T) {
	fx := workflowIdempotenceFixture(t)
	f := fx.wf
	fx.edit(t)
	tampered := false
	f.beforeServe = func(r *http.Request) {
		draft := f.draftOf["wf-1"]
		if tampered || draft == "" || r.Method != http.MethodGet || r.URL.Path != "/api/v2/workflow/wf-1/draft" {
			return
		}
		if f.FileContents(draft)["main.py"] == "deployed\n" {
			f.putFile(draft, "main.py", "agent\n")
			tampered = true
		}
	}
	code, line, _, out := applyOnce(t, fx)
	if !tampered {
		t.Fatal("the run never read the draft between its push and its publish")
	}
	if code == syncExitClean || len(f.committed) != 0 {
		t.Fatalf("exit %d, committed %v\n%s", code, f.committed, out)
	}
	if !strings.Contains(line.Detail, "no longer holds exactly this folder") {
		t.Fatalf("%s: %s", line.Verdict, line.Detail)
	}
}

// A pipeline's table and metric commits carry their drafts' versions; a write
// to a table's draft after the check is refused by the server and the table
// stays as it was.
func TestSyncApplyPublishesOnlyThePipelineDraftsItChecked(t *testing.T) {
	t.Run("checked", func(t *testing.T) {
		fx := pipelineIdempotenceFixture(t)
		fx.edit(t)
		code, line, _, out := applyOnce(t, fx)
		if code != syncExitClean || !fx.deployed() {
			t.Fatalf("exit %d, %s: %s\n%s", code, line.Verdict, line.Detail, out)
		}
		sent := fx.pipeline.draftVersions
		if len(sent) != 2 || sent[0] == "" || sent[1] == "" {
			t.Fatalf("the table and metric commits sent %q, want both drafts' versions", sent)
		}
	})
	t.Run("written-after-the-check", func(t *testing.T) {
		fx := pipelineIdempotenceFixture(t)
		f := fx.pipeline
		fx.edit(t)
		f.beforeServe = func(r *http.Request) {
			id, ok := strings.CutSuffix(strings.TrimPrefix(r.URL.Path, "/api/v2/feature/model/"), "/commit")
			if !ok || r.Method != http.MethodPost {
				return
			}
			f.mu.Lock()
			defer f.mu.Unlock()
			if row := f.tables[id]; row != nil && row.ParentModelID == "table-orders" {
				row.Code = "SELECT 'agent'"
				row.UpdatedAt = row.UpdatedAt.Add(time.Minute)
			}
		}
		code, line, _, out := applyOnce(t, fx)
		if code == syncExitClean {
			t.Fatalf("exit %d, want the table refused\n%s", code, out)
		}
		if got := f.CodeOf("table-orders"); got != "SELECT * FROM raw" {
			t.Fatalf("live table-orders holds %q", got)
		}
		if u := unitByPath(t, line, "orders.sql"); u.Outcome != unitOutcomeRefused || !strings.Contains(u.OutcomeDetail, `table "orders": its draft changed`) {
			t.Fatalf("orders.sql: %s %q, want refused naming the table\n%s", u.Outcome, u.OutcomeDetail, out)
		}
	})
}

// A metric draft holding another definition (an agent's push after this
// folder's) while the metric file is unchanged since the last apply and another
// file of the folder changed: the apply writes the file's definition into the
// draft before publishing, and never publishes the other one.
func TestSyncApplyNeverPublishesAMetricDraftTheFileDoesNotHold(t *testing.T) {
	fx := pipelineIdempotenceFixture(t)
	f := fx.pipeline
	if code, line, _, out := applyOnce(t, fx); code != syncExitClean {
		t.Fatalf("first apply: exit %d, %s: %s\n%s", code, line.Verdict, line.Detail, out)
	}
	before := f.RecipeOf("metric-aov")
	f.mu.Lock()
	agent := f.AddDraft("metric-aov", "metric-draft-agent", "")
	agent.Kind = api.TableKindMetric
	agent.MetricRecipe = []byte(strings.Replace(before, `"amount"`, `"agent_amount"`, 1))
	agent.ReportingTimezone = f.tables["metric-aov"].ReportingTimezone
	f.mu.Unlock()
	// Another change in the folder, so it is pushed and published at all.
	writeLocal(t, fx.root, "orders.sql", "SELECT * FROM raw WHERE shipped")

	code, line, _, out := applyOnce(t, fx)
	if got := f.RecipeOf("metric-aov"); strings.Contains(got, "agent_amount") {
		t.Fatalf("the agent's definition went live: %s\n%s", got, out)
	}
	if code == syncExitClean && f.RecipeOf("metric-aov") != before {
		t.Fatalf("applied, but live holds %s, want the file's %s", f.RecipeOf("metric-aov"), before)
	}
	if code != syncExitClean {
		t.Fatalf("exit %d, %s: %s — the file's definition should have been staged and published\n%s", code, line.Verdict, line.Detail, out)
	}
}

// A Ronja that answers a publish without saying it checked the draft version
// ignored the field: the deploy still goes through, and says it went through
// unchecked. One that checked says nothing.
func TestSyncApplyWarnsWhenTheDraftVersionWasNotChecked(t *testing.T) {
	const warning = "does not check draft versions yet"
	for name, ignores := range map[string]bool{"checked": false, "older Ronja": true} {
		t.Run(name, func(t *testing.T) {
			fx := workflowIdempotenceFixture(t)
			fx.wf.ignoresDraftVersion = ignores
			fx.edit(t)
			var code int
			stderr := captureStderr(t, func() { code, _, _, _ = applyOnce(t, fx) })
			if code != syncExitClean || !fx.deployed() {
				t.Fatalf("exit %d, deployed %v\n%s", code, fx.deployed(), stderr)
			}
			if got := strings.Contains(stderr, warning); got != ignores {
				t.Fatalf("warned %v, want %v\n%s", got, ignores, stderr)
			}
		})
	}
}

// draftRetitle is a draft kind's hooks for the title tests below: the GET of
// the open draft row that the publish check makes, and a retitle of that draft
// as another writer would make it.
type draftRetitle struct {
	draftPath func() string
	pushed    func() bool
	retitle   func(title string)
	liveTitle func() string
	commits   func() int
}

func workflowDraftRetitle(fx idempotenceFixture) draftRetitle {
	f := fx.wf
	return draftRetitle{
		draftPath: func() string { return "/api/v2/workflow/" + f.draftOf["wf-1"] },
		pushed:    func() bool { return f.FileContents(f.draftOf["wf-1"])["main.py"] == "deployed\n" },
		retitle: func(title string) {
			d := f.workflows[f.draftOf["wf-1"]]
			d.Title, d.UpdatedAt = title, d.UpdatedAt.Add(time.Minute)
		},
		liveTitle: func() string { return f.workflows["wf-1"].Title },
		commits:   func() int { return len(f.committed) },
	}
}

func moduleDraftRetitle(fx idempotenceFixture) draftRetitle {
	f := fx.mod
	draft := func() *api.Module { return f.mod.rows[f.mod.draftOf["module-1"]] }
	return draftRetitle{
		draftPath: func() string { return "/api/v2/module/" + f.mod.draftOf["module-1"] },
		pushed: func() bool {
			d := draft()
			return d != nil && f.ModuleFileContents(d.ID)["tracker.py"] == "deployed\n"
		},
		retitle: func(title string) {
			d := draft()
			d.Title, d.UpdatedAt = title, d.UpdatedAt.Add(time.Minute)
		},
		liveTitle: func() string { return f.mod.rows["module-1"].Title },
		commits:   func() int { return len(f.mod.committed) },
	}
}

// A retitle of the draft after the push and before the publish's check is a
// change nobody approved: the check refuses it and nothing is committed. A
// folder that declares no title does not manage it, so the same retitle is not
// compared and the publish goes through.
func TestSyncApplyRefusesADraftRetitledAfterThePush(t *testing.T) {
	kinds := map[string]struct {
		fixture func(t *testing.T) idempotenceFixture
		hooks   func(idempotenceFixture) draftRetitle
		setHook func(idempotenceFixture, func(*http.Request))
	}{
		"workflow": {workflowIdempotenceFixture, workflowDraftRetitle,
			func(fx idempotenceFixture, h func(*http.Request)) { fx.wf.beforeServe = h }},
		"module": {moduleIdempotenceFixture, moduleDraftRetitle,
			func(fx idempotenceFixture, h func(*http.Request)) { fx.mod.beforeServe = h }},
	}
	for name, k := range kinds {
		for _, managed := range []bool{true, false} {
			t.Run(fmt.Sprintf("%s/managed=%v", name, managed), func(t *testing.T) {
				fx := k.fixture(t)
				h := k.hooks(fx)
				if !managed {
					fx.draftKind.retitle(t, "")
				}
				fx.edit(t)
				tampered := false
				k.setHook(fx, func(r *http.Request) {
					if tampered || r.Method != http.MethodGet || !h.pushed() || r.URL.Path != h.draftPath() {
						return
					}
					h.retitle("Retitled elsewhere")
					tampered = true
				})
				code, line, _, out := applyOnce(t, fx)
				if !tampered {
					t.Fatalf("the run never read the pushed draft\n%s", out)
				}
				if !managed {
					if code != syncExitClean || !fx.deployed() {
						t.Fatalf("exit %d, %s: %s — an unmanaged title must not be compared\n%s", code, line.Verdict, line.Detail, out)
					}
					return
				}
				if code == syncExitClean || h.commits() != 0 {
					t.Fatalf("exit %d, %d commits: the retitled draft went live\n%s", code, h.commits(), out)
				}
				if got := h.liveTitle(); got == "Retitled elsewhere" {
					t.Fatalf("live is titled %q", got)
				}
				if !strings.Contains(line.Detail, "no longer holds exactly this folder") {
					t.Fatalf("%s: %s", line.Verdict, line.Detail)
				}
			})
		}
	}
}

// staleModuleDraft opens a draft of the fixture's module titled title while
// the live module keeps the declared one, as a draft opened before a rename
// was.
func staleModuleDraft(fx idempotenceFixture, title string) *api.Module {
	f := fx.mod
	f.mu.Lock()
	defer f.mu.Unlock()
	live := f.mod.rows["module-1"]
	draft := f.AddModule(&api.Module{
		ID: "module-draft-stale", ParentModuleID: "module-1", Lifecycle: api.LifecycleDraft,
		FeatureID: live.FeatureID, Name: live.Name, Title: title, DrafterUserID: fakeCallerUserID,
		BaseVersionID: f.moduleHeadVersionID("module-1"), UpdatedAt: live.UpdatedAt.Add(time.Hour),
	}, api.ModuleFile{Path: moduleInitFile, Content: fakeModuleInitSeed}, api.ModuleFile{Path: "tracker.py", Content: "live\n"})
	f.mod.draftOf["module-1"] = draft.ID
	return draft
}

// A module's title leg judges the draft as well as the live row. A draft still
// holding this folder's own earlier title (the one the lock last recorded)
// while live already holds the declared one gets the declared title from the
// push, and the publish lands it rather than the stale one.
func TestSyncApplyPublishesTheDeclaredTitleOverAStaleModuleDraft(t *testing.T) {
	fx := moduleIdempotenceFixture(t)
	lock, err := wfdir.LoadLock(fx.root)
	if err != nil {
		t.Fatal(err)
	}
	lock.SetAppliedTitle("prod", "Earlier title")
	if err := wfdir.SaveLock(fx.root, lock); err != nil {
		t.Fatal(err)
	}
	staleModuleDraft(fx, "Earlier title")
	fx.edit(t)
	code, line, _, out := applyOnce(t, fx)
	if code != syncExitClean || !fx.deployed() {
		t.Fatalf("exit %d, %s: %s\n%s", code, line.Verdict, line.Detail, out)
	}
	if got := fx.mod.mod.rows["module-1"].Title; got != "EMAB tracker" {
		t.Fatalf("live module is titled %q, want the declared %q", got, "EMAB tracker")
	}
}

// A module draft retitled somewhere other than this folder, while live holds
// the declared title, is refused rather than overwritten: nothing is
// committed, and the draft keeps the title somebody gave it.
func TestSyncApplyRefusesAModuleDraftRetitledElsewhere(t *testing.T) {
	fx := moduleIdempotenceFixture(t)
	draft := staleModuleDraft(fx, "Retitled in the web app")
	fx.edit(t)
	code, line, _, out := applyOnce(t, fx)
	if code == syncExitClean || fx.draftKind.commits() != 0 {
		t.Fatalf("exit %d, %d commits: the foreign retitle was overwritten and published\n%s", code, fx.draftKind.commits(), out)
	}
	if !strings.Contains(line.Detail, "renamed somewhere other than this folder") || !strings.Contains(line.Detail, "Retitled in the web app") {
		t.Fatalf("%s: %s", line.Verdict, line.Detail)
	}
	if draft.Title != "Retitled in the web app" || fx.mod.mod.rows["module-1"].Title != "EMAB tracker" {
		t.Fatalf("draft %q, live %q", draft.Title, fx.mod.mod.rows["module-1"].Title)
	}
}

// Everything a pipeline file writes into its draft is what the publish check
// compares, not the SQL or the recipe alone: a draft renamed or re-described
// after the push, before the check, is refused and live keeps what it had.
func TestSyncApplyRefusesAPipelineDraftWhoseDeclaredMetadataMoved(t *testing.T) {
	const metricBody = `{"recipe":{"value":"revenue / orders","source":"table-orders","base_measures":[{"name":"revenue","agg":"sum","column":"net_amount"}]},"reportingTimezone":"Europe/Stockholm"`
	rows := map[string]struct {
		path, content, liveID string
		holds                 func(row *api.Table) bool
		tamper                func(row *api.Table)
		live                  func(row *api.Table) string
	}{
		"table name": {
			path: "orders.sql", content: "-- @name Shipped orders\nSELECT * FROM raw WHERE shipped", liveID: "table-orders",
			holds:  func(r *api.Table) bool { return r.Name == "Shipped orders" },
			tamper: func(r *api.Table) { r.Name = "Renamed elsewhere" },
			live:   func(r *api.Table) string { return r.Name },
		},
		"table description": {
			path: "orders.sql", content: "-- @table One row per shipped order.\nSELECT * FROM raw WHERE shipped", liveID: "table-orders",
			holds:  func(r *api.Table) bool { return r.Description == "One row per shipped order." },
			tamper: func(r *api.Table) { r.Description = "Described elsewhere" },
			live:   func(r *api.Table) string { return r.Description },
		},
		"metric name": {
			path: "metrics/aov.json", content: metricBody + `,"name":"Average order value"}`, liveID: "metric-aov",
			holds:  func(r *api.Table) bool { return r.Name == "Average order value" },
			tamper: func(r *api.Table) { r.Name = "Renamed elsewhere" },
			live:   func(r *api.Table) string { return r.Name },
		},
	}
	for name, row := range rows {
		t.Run(name, func(t *testing.T) {
			fx := pipelineIdempotenceFixture(t)
			f := fx.pipeline
			fx.edit(t)
			writeLocal(t, fx.root, row.path, row.content)
			tampered := false
			f.beforeServe = func(r *http.Request) {
				id, ok := strings.CutPrefix(r.URL.Path, "/api/v2/feature/model/")
				if tampered || !ok || r.Method != http.MethodGet {
					return
				}
				f.mu.Lock()
				defer f.mu.Unlock()
				if d := f.tables[id]; d != nil && d.ParentModelID == row.liveID && row.holds(d) {
					row.tamper(d)
					d.UpdatedAt = d.UpdatedAt.Add(time.Minute)
					tampered = true
				}
			}
			code, line, _, out := applyOnce(t, fx)
			if !tampered {
				t.Fatalf("the run never read the pushed draft\n%s", out)
			}
			if code == syncExitClean {
				t.Fatalf("exit %d, want %s refused\n%s", code, row.path, out)
			}
			f.mu.Lock()
			got := row.live(f.tables[row.liveID])
			f.mu.Unlock()
			if got == "Renamed elsewhere" || got == "Described elsewhere" {
				t.Fatalf("live %s holds %q", row.liveID, got)
			}
			if u := unitByPath(t, line, row.path); u.Outcome != unitOutcomeRefused || !strings.Contains(u.OutcomeDetail, "no longer holds exactly this folder") {
				t.Fatalf("%s: %s %q\n%s", row.path, u.Outcome, u.OutcomeDetail, out)
			}
		})
	}
}
