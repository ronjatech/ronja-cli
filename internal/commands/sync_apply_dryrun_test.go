package commands

import (
	"fmt"
	"maps"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/ronjatech/ronja-cli/internal/wfdir"
)

// dryRunLine runs `sync apply --dry-run` over the fixture's tree and answers
// the exit code, the folder's line and the report, asserting it sent no write
// besides an adopted folder's gate validate.
func dryRunLine(t *testing.T, fx idempotenceFixture) (int, syncApplyFolderReport, *syncApplyReport) {
	t.Helper()
	fx.resetRequests()
	out, err := runCLI(t, fx.tree, "sync", "apply", "--stack", "prod", "--dry-run", "--json")
	if w := writesBesidesGate(fx.requests()); len(w) != 0 {
		t.Fatalf("dry run sent %v", w)
	}
	report := applyReportOf(t, out)
	return exitCodeOf(err), applyFolderLine(t, report, filepath.Base(fx.root)), report
}

// A row the lock names and the server holds no claim for is claimed by apply
// before anything else (the lazy backfill) — a write to Ronja's records, though
// to no row. A dry run of such a folder said "nothing to push or publish" and
// stopped there; it now says how many claims apply records, on the folder's
// line and in the summary, for every kind.
func TestDryRunSaysWhichClaimsApplyRecords(t *testing.T) {
	for kind, setup := range idempotenceFixtures() {
		t.Run(kind, func(t *testing.T) {
			fx := setup(t)
			adoptFixture(t, fx)
			if code, line, _, out := applyOnce(t, fx); code != syncExitClean {
				t.Fatalf("first apply: exit %d %+v\n%s", code, line, out)
			}
			m := managedFakeOf(fx)
			m.mu.Lock()
			claims := len(m.claims)
			clear(m.claims)
			m.mu.Unlock()

			code, line, report := dryRunLine(t, fx)
			if code != syncExitClean {
				t.Fatalf("dry run: exit %d %+v", code, line)
			}
			if actions := slices.Collect(maps.Values(unitActions(line))); slices.ContainsFunc(actions, func(a string) bool { return a != string(applyActionUnchanged) }) {
				t.Fatalf("units = %v, want every one unchanged", actions)
			}
			if line.Claims != claims || report.Summary.Claims != claims {
				t.Errorf("claims = %d (summary %d), want %d", line.Claims, report.Summary.Claims, claims)
			}
			if !strings.HasPrefix(line.Detail, "nothing to ") || !strings.Contains(line.Detail, "; apply records ") || !strings.Contains(line.Detail, " in Ronja for ") {
				t.Errorf("detail = %q, want nothing to push and the claims apply records", line.Detail)
			}
			for _, u := range line.Units {
				if strings.HasPrefix(u.Path, "tables/") || strings.HasPrefix(u.Path, "checks/") {
					continue // not a row the lock names
				}
				if strings.HasPrefix(u.Detail, "—") || strings.Contains(u.Detail, "— —") || strings.Contains(u.Detail, "file no drift") || !strings.Contains(u.Detail, "the server has no claim for it yet") {
					t.Errorf("unit detail %q does not keep the claim note after its own sentence", u.Detail)
				}
			}

			// The apply records them, and the next dry run has none to record.
			if code, line, _, out := applyOnce(t, fx); code != syncExitClean {
				t.Fatalf("apply: exit %d %+v\n%s", code, line, out)
			}
			if _, line, _ := dryRunLine(t, fx); line.Claims != 0 || strings.Contains(line.Detail, "claim") {
				t.Errorf("after the apply: %+v", line)
			}
		})
	}
}

// The dry run asks an automation file the questions its push asks first, of
// the push's own listing of the stack's feature: a file that declares a field
// the server cannot patch, and a row that is no longer in the feature, are
// refusals in the apply — and were `unchanged` in the dry run, which read the
// row by id and compared only the patchable fields. (An adopted folder's moved
// row is refused earlier still, by Ronja's record of it.)
func TestDryRunShowsAnAutomationApplyRefuses(t *testing.T) {
	cases := map[string]struct {
		file   string
		change func(fx idempotenceFixture)
		want   string
		// classic: a folder with no folder id, whose row moving is not
		// refused earlier by Ronja's records of it.
		classic bool
	}{
		"unpatchable trigger kind": {
			file:   `{"cronExpr": "0 2 * * *", "triggerKind": "cron"}`,
			change: func(fx idempotenceFixture) { fx.automation.RowOf("job-1").TriggerKind = "email" },
			want:   `"triggerKind" declares "cron"`,
		},
		"moved to another feature": {
			file:    `{"cronExpr": "0 2 * * *"}`,
			change:  func(fx idempotenceFixture) { fx.automation.RowOf("job-1").FeatureID = "collection-other" },
			want:    "binding broken: job-1 is not in feature collection-1",
			classic: true,
		},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			fx := automationIdempotenceFixture(t)
			if !tc.classic {
				adoptFixture(t, fx)
			}
			writeLocal(t, fx.root, "nightly.json", tc.file)
			if code, line, _, out := applyOnce(t, fx); code != syncExitClean {
				t.Fatalf("first apply: exit %d %+v\n%s", code, line, out)
			}
			tc.change(fx)

			code, line, _ := dryRunLine(t, fx)
			if code != syncExitDrifted || line.Verdict != verdictRefused || line.Reason != syncReasonNotDeployed {
				t.Fatalf("dry run: exit %d %+v — want the refusal the apply makes", code, line)
			}
			if len(line.Units) != 1 || line.Units[0].Action != string(applyActionRefuse) || !strings.Contains(line.Units[0].Detail, tc.want) {
				t.Errorf("units = %+v, want refuse saying %q", line.Units, tc.want)
			}
			if code, line, _, out := applyOnce(t, fx); code != syncExitDrifted || line.Verdict != verdictRefused {
				t.Fatalf("the apply after it: exit %d %+v\n%s", code, line, out)
			}
		})
	}
}

// A CI runner's checkout has no .ronja/ baseline: the dry run compares every
// pipeline file with what is live rather than skipping the ones this checkout
// pushed, and still finds each unchanged — and the apply that follows sends no
// write.
func TestDryRunOfAFreshCheckoutOfAnUnchangedPipeline(t *testing.T) {
	fx := pipelineIdempotenceFixture(t)
	adoptFixture(t, fx)
	if code, line, _, out := applyOnce(t, fx); code != syncExitClean {
		t.Fatalf("first apply: exit %d %+v\n%s", code, line, out)
	}
	committed := snapshotFolder(t, fx.root, true)
	restoreCheckout(t, fx.root, committed)
	code, line, _ := dryRunLine(t, fx)
	if code != syncExitClean {
		t.Fatalf("dry run: exit %d %+v", code, line)
	}
	want := map[string]string{
		"orders.sql":                   string(applyActionUnchanged),
		"metrics/aov.json":             string(applyActionUnchanged),
		"tables/fortnox_invoices.json": string(applyActionUnchanged),
		"checks/orders.json":           string(applyActionUnchanged),
	}
	if got := unitActions(line); !maps.Equal(got, want) {
		t.Fatalf("units = %v, want %v\n%+v", got, want, line)
	}
	restoreCheckout(t, fx.root, committed)
	if code, line, writes, out := applyOnce(t, fx); code != syncExitClean || len(writes) != 0 {
		t.Fatalf("apply: exit %d, writes %v %+v\n%s", code, writes, line, out)
	}
}

// A declared move is not a file that differs from what is live, and the dry
// run's line says what it is.
func TestDescribeComparedNamesMovesAndRemovals(t *testing.T) {
	units := []syncApplyUnitReport{
		{Path: "a.sql", Action: string(applyActionUnchanged)},
		{Path: "old.sql", To: "new.sql", Action: string(applyActionMove)},
		{Path: "gone.sql", Action: string(applyActionRemove)},
	}
	kind, _ := wfdir.KindByName(wfdir.KindPipeline)
	if got, want := describeCompared(kind, units, 0), "nothing to push or publish: the live rows already hold this folder; apply records 1 declared move and stops managing 1 declared removal, leaving its row as it is"; got != want {
		t.Errorf("got  %q\nwant %q", got, want)
	}
	units[0].Action = string(applyActionUpdate)
	if got, want := describeCompared(kind, units, 0), "would push and publish 1 of its 1 file, which differ from what is live; it also records 1 declared move and stops managing 1 declared removal, leaving its row as it is"; got != want {
		t.Errorf("got  %q\nwant %q", got, want)
	}
}

// A docs file documenting a table outside the stack's feature, through no
// write source of the stack, is refused by the apply's write gate — and the dry
// run makes the same check (planWriteGate), so it says so rather than listing
// the file `unchanged`.
func TestDryRunRefusesADocsFileOutsideTheStacksFeature(t *testing.T) {
	fx := pipelineIdempotenceFixture(t)
	adoptFixture(t, fx)
	if code, line, _, out := applyOnce(t, fx); code != syncExitClean {
		t.Fatalf("first apply: exit %d %+v\n%s", code, line, out)
	}
	fx.pipeline.mu.Lock()
	fx.pipeline.tables["table-raw"].FeatureID = "collection-other"
	fx.pipeline.mu.Unlock()
	code, line, _ := dryRunLine(t, fx)
	if code != syncExitDrifted || line.Reason != syncReasonWritesOutsideTarget || !strings.Contains(line.Detail, "tables/fortnox_invoices.json") {
		t.Fatalf("dry run: exit %d %+v — want writes_outside_target naming the docs file", code, line)
	}
	if code, line, _, out := applyOnce(t, fx); code != syncExitDrifted || line.Reason != syncReasonWritesOutsideTarget {
		t.Fatalf("apply: exit %d %+v\n%s", code, line, out)
	}
}

// statusFolderVerdict runs `sync status --stack prod` over the fixture's tree
// and answers the exit code and the folder's verdict and detail.
func statusFolderVerdict(t *testing.T, fx idempotenceFixture) (int, string, string) {
	t.Helper()
	fx.resetRequests()
	out, err := runCLI(t, fx.tree, "sync", "status", "--stack", "prod", "--json")
	if w := writesBesidesGate(fx.requests()); len(w) != 0 {
		t.Fatalf("status sent %v", w)
	}
	for _, raw := range decodeJSON(t, out)["folders"].([]any) {
		folder := raw.(map[string]any)
		if folder["path"] == filepath.Base(fx.root) {
			return exitCodeOf(err), folder["verdict"].(string), fmt.Sprint(folder["detail"])
		}
	}
	t.Fatalf("no folder line in\n%s", out)
	return 0, "", ""
}

// A fresh clone has no local baseline, and `sync status` answered every
// adopted workflow, data-app and module folder `unknown` — "nothing could be
// compared" — and exited 2, while `sync apply --dry-run` compared the folder
// with the live row and said `unchanged`. For an adopted folder status now
// makes that same comparison, reading only: clean when the live row holds the
// folder, drifted when it does not. A classic folder keeps its answer.
func TestSyncStatusOfAFreshCloneComparesWithLive(t *testing.T) {
	for kind, setup := range draftKindFixtures() {
		t.Run(kind, func(t *testing.T) {
			fx := setup(t)
			committed := snapshotFolder(t, fx.root, true)
			restoreCheckout(t, fx.root, committed)
			if code, verdict, detail := statusFolderVerdict(t, fx); code != syncExitUnknown || verdict != verdictUnknown {
				t.Fatalf("classic fresh clone: exit %d %s (%s), want unknown as before", code, verdict, detail)
			}

			adoptFixture(t, fx)
			if code, line, _, out := applyOnce(t, fx); code != syncExitClean {
				t.Fatalf("first apply: exit %d %+v\n%s", code, line, out)
			}
			committed = snapshotFolder(t, fx.root, true)
			restoreCheckout(t, fx.root, committed)
			code, verdict, detail := statusFolderVerdict(t, fx)
			if code != syncExitClean || verdict != verdictClean || !strings.Contains(detail, "changes nothing") {
				t.Fatalf("adopted fresh clone: exit %d %s (%s), want clean", code, verdict, detail)
			}
			// The live comparison asks more of the folder status opened; it
			// does not open (and resolve) it a second time.
			if n := countRequests(fx.requests(), "GET /api/v2/managed"); n != 1 {
				t.Errorf("status read the folder's records %d times, want once:\n%s", n, strings.Join(fx.requests(), "\n"))
			}
			if code, line, _ := dryRunLine(t, fx); code != syncExitClean || line.Units[0].Action != string(applyActionUnchanged) {
				t.Fatalf("the dry run disagrees: %+v", line)
			}

			fx.edit(t)
			if code, verdict, detail := statusFolderVerdict(t, fx); code != syncExitDrifted || verdict != verdictDrifted || strings.Contains(detail, "ronja ronja") {
				t.Fatalf("an edited fresh clone: exit %d %s (%s), want drifted", code, verdict, detail)
			}
		})
	}
}

// A workflow or app whose LIVE row changed in Ronja since this checkout last
// deployed it is refused by `sync apply` — and the refusal leads with what
// changed and the command that takes it into the folder, names nothing `sync
// apply` lacks (it has no --force), and is decided BEFORE a checkout: no draft
// is opened only to be withdrawn.
func TestApplyRefusesALiveChangeBeforeCheckingOut(t *testing.T) {
	for _, kind := range []string{"workflow", "data app"} {
		t.Run(kind, func(t *testing.T) {
			fx := draftKindFixtures()[kind](t)
			adoptFixture(t, fx)
			if code, line, _, out := applyOnce(t, fx); code != syncExitClean {
				t.Fatalf("first apply: exit %d %+v\n%s", code, line, out)
			}
			fx.draftKind.webPublish("changed in the web app\n")
			fx.edit(t)

			// The dry run takes the decision the apply is about to take, in
			// its words.
			code, preview, _ := dryRunLine(t, fx)
			code, line, writes, out := applyOnce(t, fx)
			if preview.Verdict != line.Verdict || preview.Reason != line.Reason || preview.Detail != line.Detail {
				t.Errorf("the dry run says %s/%s %q\nthe apply says %s/%s %q", preview.Verdict, preview.Reason, preview.Detail, line.Verdict, line.Reason, line.Detail)
			}
			if code != syncExitDrifted || line.Verdict != verdictRefused {
				t.Fatalf("apply: exit %d %+v\n%s", code, line, out)
			}
			label := map[string]string{"workflow": "wf", "data app": "app"}[kind]
			wantHead := `the live ` + kind + ` changed in Ronja since stack "prod" last applied it — run ` + "`ronja " + label + " pull`"
			if !strings.HasPrefix(line.Detail, wantHead) {
				t.Errorf("detail = %q\nwant it to lead with %q", line.Detail, wantHead)
			}
			if strings.Contains(line.Detail, "or push --force") || !strings.Contains(line.Detail, "push --force` in this folder, then apply again") {
				t.Errorf("detail names a remedy sync apply does not have: %q", line.Detail)
			}
			if len(writes) != 0 {
				t.Errorf("the refused apply sent %v — a draft opened and withdrawn for nothing", writes)
			}
			if drafts := fx.drafts(); len(drafts) != 0 {
				t.Errorf("drafts left open: %v", drafts)
			}
		})
	}
}

// A deploy that rewrites an ADOPTED folder's ronja.lock.json says so once:
// the folder keeps its records in Ronja, so a deploy never needs the lock and a
// merge conflict in it is settled by taking either side — but a fresh
// checkout's dry run and status compare against it (BL-e7b3), so it is still
// committed. An unchanged run rewrites no lock and says nothing; a classic
// folder, whose lock is its record, is never listed.
func TestApplySaysAnAdoptedLockIsACache(t *testing.T) {
	for _, adopted := range []bool{true, false} {
		fx := workflowIdempotenceFixture(t)
		if adopted {
			adoptFixture(t, fx)
		}
		fx.edit(t)
		out, err := runCLI(t, fx.tree, "sync", "apply", "--stack", "prod", "--json")
		if err != nil {
			t.Fatalf("apply: %v\n%s", err, out)
		}
		got := applyReportOf(t, out).LocksRewritten
		if want := []string{filepath.Base(fx.root)}; adopted && !slices.Equal(got, want) || !adopted && len(got) != 0 {
			t.Fatalf("adopted=%v: locksRewritten = %v", adopted, got)
		}
		if adopted {
			writeLocal(t, fx.root, "main.py", "deployed again\n")
			human, err := runCLI(t, fx.tree, "sync", "apply", "--stack", "prod")
			if err != nil {
				t.Fatalf("apply: %v\n%s", err, human)
			}
			for _, want := range []string{"a deploy never needs it", "commit it", "take either side, then run `ronja sync apply`"} {
				if !strings.Contains(human, want) {
					t.Errorf("the lock note does not say %q:\n%s", want, human)
				}
			}
		}
		human, err := runCLI(t, fx.tree, "sync", "apply", "--stack", "prod")
		if err != nil || strings.Contains(human, wfdir.LockName+" changed") {
			t.Fatalf("an unchanged apply: %v\n%s", err, human)
		}
	}
}

// A baseline an older CLI wrote records no aliases, so a binding re-pointed in
// ronja.json since reads as the live row having changed: apply refuses it,
// and the dry run said it would push and publish. The dry run now takes the
// apply's decision, and both say what the refusal is — the binding, as the
// live row still holds its old id — and how to get past it.
func TestARebindOnABaselineWithoutAliasesSaysHowToGetPastIt(t *testing.T) {
	for _, tc := range []struct {
		name, file, command string
		kind                wfdir.Kind
		setup               func(t *testing.T) idempotenceFixture
	}{
		{"workflow", "main.py", "wf", wfdir.WorkflowKind, workflowIdempotenceFixture},
		{"data app", "App.tsx", "app", wfdir.DataAppKind, appIdempotenceFixture},
	} {
		t.Run(tc.name, func(t *testing.T) {
			fx := tc.setup(t)
			adoptFixture(t, fx)
			m, err := wfdir.LoadManifest(fx.root, tc.kind)
			if err != nil {
				t.Fatal(err)
			}
			m.Dependencies = tableDep()
			bind := func(id string) {
				t.Helper()
				stack := m.Stacks["prod"]
				stack.Bind = map[string]string{"orders": id}
				m.Stacks["prod"] = stack
				if err := wfdir.SaveManifest(fx.root, m); err != nil {
					t.Fatal(err)
				}
			}
			bind("table-orders")
			writeLocal(t, fx.root, tc.file, "q = \"{{ ref('orders') }}\"\n")
			if code, line, _, out := applyOnce(t, fx); code != syncExitClean {
				t.Fatalf("first apply: exit %d %+v\n%s", code, line, out)
			}
			if got := fx.draftKind.liveMain(); got != "q = \"{{ ref('table-orders') }}\"\n" {
				t.Fatalf("live holds %q", got)
			}
			forgetBaselineAliases(t, fx.root)
			bind("table-orders-2")

			// Status says the same: a re-pointed binding the push refuses, and
			// the way past it — never a change made in Ronja for `pull` to take,
			// which would write the old id into the folder and lose the rebind.
			out, err := runCLI(t, fx.root, tc.command, "status", "--stack", "prod", "--json")
			if err != nil {
				t.Fatalf("status: %v\n%s", err, out)
			}
			report := decodeJSON(t, out)
			rebound, _ := report["rebound"].([]any)
			if len(rebound) != 1 || !strings.Contains(fmt.Sprint(rebound[0].(map[string]any)["refused"]), "put the binding back") {
				t.Errorf("status does not name the rebind the push refuses: %v", rebound)
			}
			if drift := fmt.Sprint(report["remote"].(map[string]any)["drift"]); strings.Contains(drift, tc.file) {
				t.Errorf("status reports %s as changed on the server: %s", tc.file, drift)
			}
			if sc, v, detail := statusFolderVerdict(t, fx); sc != syncExitDrifted || v != verdictDrifted ||
				strings.Contains(detail, "on the server") || strings.Contains(detail, "takes it into this folder") ||
				!strings.Contains(detail, "put the binding back in ronja.json, run `ronja "+tc.command+" pull`") {
				t.Errorf("sync status: exit %d %s — %s", sc, v, detail)
			}

			code, preview, _ := dryRunLine(t, fx)
			code2, line, writes, out := applyOnce(t, fx)
			if code2 != syncExitDrifted || line.Verdict != verdictRefused || len(writes) != 0 {
				t.Fatalf("apply: exit %d %+v %v\n%s", code2, line, writes, out)
			}
			if code != code2 || preview.Verdict != line.Verdict || preview.Detail != line.Detail {
				t.Errorf("the dry run (exit %d) says %s %q\nthe apply says %s %q", code, preview.Verdict, preview.Detail, line.Verdict, line.Detail)
			}
			for _, want := range []string{
				"binding `orders` as table-orders (ronja.json now says table-orders-2)",
				"put the binding back",
				"`ronja " + tc.command + " pull`",
			} {
				if !strings.Contains(line.Detail, want) {
					t.Errorf("the refusal does not say %q:\n%s", want, line.Detail)
				}
			}

			// And the way it names works.
			bind("table-orders")
			if out, err := runCLI(t, fx.root, tc.command, "pull", "--stack", "prod", "--force", "--json"); err != nil {
				t.Fatalf("pull: %v\n%s", err, out)
			}
			bind("table-orders-2")
			if code, line, _, out := applyOnce(t, fx); code != syncExitClean || line.Verdict != verdictApplied {
				t.Fatalf("apply after the way past it: exit %d %+v\n%s", code, line, out)
			}
			if got := fx.draftKind.liveMain(); got != "q = \"{{ ref('table-orders-2') }}\"\n" {
				t.Fatalf("live holds %q", got)
			}
		})
	}
}

// forgetBaselineAliases rewrites a folder's local baseline as an older CLI
// wrote it: with no record of the aliases it was taken under.
func forgetBaselineAliases(t *testing.T, root string) {
	t.Helper()
	state, err := wfdir.LoadState(root)
	if err != nil {
		t.Fatal(err)
	}
	for _, inst := range state.Instances {
		inst.State.Aliases, inst.State.AliasesRecorded = nil, false
	}
	if err := wfdir.SaveState(root, state); err != nil {
		t.Fatal(err)
	}
}

// countRequests counts the requests that are exactly want ("METHOD /path").
func countRequests(requests []string, want string) int {
	n := 0
	for _, r := range requests {
		if r == want {
			n++
		}
	}
	return n
}
