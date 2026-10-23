package commands

import (
	"encoding/json"
	"fmt"
	"maps"
	"net/http"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/ronjatech/ronja-cli/internal/api"
	"github.com/ronjatech/ronja-cli/internal/wfdir"
)

// The CLI half of server-side state: an ADOPTED folder —
// one whose ronja.json carries an "id" — reads its state from the server, the
// server wins, and the lock is a cache. Every test here asserts on what the
// FAKE SERVER holds and was sent (fakeManaged, which models api/v2/managed),
// never on what the CLI reports alone.

// managedFakeOf is a fixture's managed-state fake.
func managedFakeOf(fx idempotenceFixture) *fakeManaged {
	switch {
	case fx.pipeline != nil:
		return fx.pipeline.managed
	case fx.wf != nil:
		return fx.wf.managed
	case fx.mod != nil:
		return fx.mod.managed
	case fx.app != nil:
		return fx.app.managed
	default:
		return fx.automation.managed
	}
}

// featureOf is the stack's feature a fixture deploys to.
func featureOf(t *testing.T, root string) string {
	t.Helper()
	m := loadAnyManifest(t, root)
	return m.Stacks["prod"].FeatureID
}

// loadAnyManifest reads a folder's ronja.json whatever its kind.
func loadAnyManifest(t *testing.T, root string) *wfdir.Manifest {
	t.Helper()
	raw, err := os.ReadFile(wfdir.ManifestPath(root))
	if err != nil {
		t.Fatal(err)
	}
	var probe struct {
		Kind string `json:"kind"`
	}
	if err := json.Unmarshal(raw, &probe); err != nil {
		t.Fatal(err)
	}
	kind, ok := wfdir.KindByName(probe.Kind)
	if !ok {
		t.Fatalf("unknown kind %q", probe.Kind)
	}
	m, err := wfdir.LoadManifest(root, kind)
	if err != nil {
		t.Fatal(err)
	}
	return m
}

// adoptFixture runs `ronja sync adopt --stack prod` over a fixture's tree.
func adoptFixture(t *testing.T, fx idempotenceFixture) string {
	t.Helper()
	out, err := runCLI(t, fx.tree, "sync", "adopt", "--stack", "prod", "--json")
	if err != nil {
		t.Fatalf("adopt: %v\n%s", err, out)
	}
	return out
}

// removeLock deletes a folder's ronja.lock.json and its local baseline — a CI
// runner whose checkout never committed the lock.
func removeLock(t *testing.T, root string) {
	t.Helper()
	for _, p := range []string{wfdir.LockPath(root), filepath.Join(root, wfdir.StateDirName)} {
		if err := os.RemoveAll(p); err != nil {
			t.Fatal(err)
		}
	}
}

// managedRequestsIn answers the requests to /api/v2/managed.
func managedRequestsIn(requests []string) []string {
	var out []string
	for _, r := range requests {
		if strings.Contains(r, "/api/v2/managed") {
			out = append(out, r)
		}
	}
	return out
}

// TestSyncAdoptClaimsEveryLockBoundRowAndRecordsItsAnchor: adopt mints the
// folder id into ronja.json, claims every row the lock records in the stack's
// feature, and records the lock's anchor on each claim — and a second adopt
// sends no write at all.
func TestSyncAdoptClaimsEveryLockBoundRowAndRecordsItsAnchor(t *testing.T) {
	for kind, setup := range idempotenceFixtures() {
		t.Run(kind, func(t *testing.T) {
			fx := setup(t)
			m := managedFakeOf(fx)
			lockBefore := readFile(t, wfdir.LockPath(fx.root))
			out := adoptFixture(t, fx)
			var err error

			manifest := loadAnyManifest(t, fx.root)
			if !manifest.Adopted() || manifest.FormatVersion != 6 {
				t.Fatalf("adopt wrote no folder id, or did not stamp format version 6 (%d)\n%s", manifest.FormatVersion, out)
			}
			lock, err := wfdir.LoadLock(fx.root)
			if err != nil {
				t.Fatal(err)
			}
			want := lock.ManagedAddresses("prod", mustKind(t, manifest.Kind))
			claims := m.Claims(featureOf(t, fx.root))
			if len(want) == 0 || len(claims) != len(want) {
				t.Fatalf("claims = %d, lock rows = %d\n%s", len(claims), len(want), out)
			}
			for _, a := range want {
				c := m.ClaimAt(featureOf(t, fx.root), manifest.ID, a.Address)
				if c == nil || c.ResourceID != a.ID || c.Kind != a.Kind {
					t.Fatalf("no claim of %s %s at %q: %+v", a.Kind, a.ID, a.Address, c)
				}
				anchor := lock.Anchor("prod", mustKind(t, manifest.Kind), a.Address)
				if len(anchor) > 0 && canonicalJSON(c.Applied) != canonicalJSON(anchor) {
					t.Fatalf("applied at %q = %v, want the lock's anchor %v", a.Address, c.Applied, anchor)
				}
			}
			if after := readFile(t, wfdir.LockPath(fx.root)); after != lockBefore {
				t.Errorf("adopt rewrote a lock that agreed with the server:\nbefore %s\nafter  %s", lockBefore, after)
			}

			fx.resetRequests()
			// The human report this time: the same answer, and a second run claims
			// nothing and writes nothing.
			out, err = runCLI(t, fx.tree, "sync", "adopt", "--stack", "prod")
			if err != nil {
				t.Fatalf("second adopt: %v\n%s", err, out)
			}
			if writes := writesBesidesGate(fx.requests()); len(writes) != 0 {
				t.Fatalf("a second adopt sent %v\n%s", writes, out)
			}
			if !strings.Contains(out, api.ManagedOutcomeAlreadyClaimed) {
				t.Errorf("a second adopt did not report already_claimed:\n%s", out)
			}
		})
	}
}

func mustKind(t *testing.T, name string) wfdir.Kind {
	t.Helper()
	k, ok := wfdir.KindByName(name)
	if !ok {
		t.Fatalf("unknown kind %q", name)
	}
	return k
}

// TestManagedLostLock is the managed/lost-lock scenario as a CLI test:
// adopt, apply, lose the lock, apply again. The second apply creates no row,
// rewrites the lock from the server, sends no write at all on an unchanged
// tree (1c) — and its drift guard is ARMED from `applied`: a change made on the
// server since is refused, not overwritten.
//
// Two ways to lose it: deleted outright (a repository that never committed
// one), and STALE — the lock committed at adopt time restored by every fresh
// checkout, while the server's anchor moved on with each deploy.
func TestManagedLostLock(t *testing.T) {
	for kind, setup := range idempotenceFixtures() {
		for _, edited := range []bool{false, true} {
			for _, stale := range []bool{false, true} {
				name := kind + "/unchanged"
				if edited {
					name = kind + "/edited"
				}
				if stale {
					name += "/stale-lock"
				} else {
					name += "/no-lock"
				}
				t.Run(name, func(t *testing.T) { lostLock(t, setup(t), edited, stale) })
			}
		}
	}
}

func lostLock(t *testing.T, fx idempotenceFixture, edited, stale bool) {
	m := managedFakeOf(fx)
	adoptFixture(t, fx)
	feature := featureOf(t, fx.root)
	claimsBefore := len(m.Claims(feature))
	if edited {
		// Run one DEPLOYS, so the anchor run two needs is the one run one
		// recorded on the server — not the one adopt copied from the lock.
		fx.edit(t)
	}
	// What the repository holds: the change, and the lock as adopt left it.
	committed := snapshotFolder(t, fx.root, true)
	loseLock := func() {
		t.Helper()
		if stale {
			restoreCheckout(t, fx.root, committed)
		} else {
			removeLock(t, fx.root)
		}
	}

	code, line, _, out := applyOnce(t, fx)
	if code != syncExitClean {
		t.Fatalf("first apply: exit %d %+v\n%s", code, line, out)
	}
	if edited && !fx.deployed() {
		t.Fatalf("run one did not deploy\n%s", out)
	}
	loseLock()
	code, line, writes, out := applyOnce(t, fx)
	if code != syncExitClean || line.Verdict != verdictApplied {
		t.Fatalf("apply with the lock lost: exit %d %+v\n%s", code, line, out)
	}
	if len(writes) != 0 {
		t.Fatalf("an unchanged tree with the lock lost sent %v\n%s", writes, out)
	}
	if got := len(m.Claims(feature)); got != claimsBefore {
		t.Fatalf("claims went from %d to %d — a row was created\n%s", claimsBefore, got, out)
	}
	if _, err := os.Stat(wfdir.LockPath(fx.root)); err != nil {
		t.Fatalf("the lock was not rewritten from the server: %v", err)
	}
	if !stale && !slices.ContainsFunc(line.Units, func(u syncApplyUnitReport) bool { return u.Action == string(applyActionAdopt) }) {
		t.Errorf("no unit reported adopt for a lock the server rewrote: %+v", line.Units)
	}

	// ARMED: a change made on the server, then a fresh checkout. The anchor
	// came from `applied`, so the change is drift.
	if fx.draftKind == nil {
		return
	}
	fx.draftKind.webPublish("published in the web app\n")
	loseLock()
	code, line, _, out = applyOnce(t, fx)
	if code != syncExitDrifted || line.Verdict != verdictRefused {
		t.Fatalf("a web publish with the lock lost: exit %d %+v — the drift guard was not armed\n%s", code, line, out)
	}
	if got := fx.draftKind.liveMain(); got != "published in the web app\n" {
		t.Fatalf("the colleague's publish was overwritten: %q", got)
	}
}

// folderIDOf is a folder's id, failing the test when it has none.
func folderIDOf(t *testing.T, root string) string {
	t.Helper()
	m := loadAnyManifest(t, root)
	if !m.Adopted() {
		t.Fatalf("%s has no folder id", root)
	}
	return m.ID
}

// editLock rewrites a folder's lock through a function.
func editLock(t *testing.T, root string, edit func(l *wfdir.Lock)) {
	t.Helper()
	l, err := wfdir.LoadLock(root)
	if err != nil {
		t.Fatal(err)
	}
	edit(l)
	if err := wfdir.SaveLock(root, l); err != nil {
		t.Fatal(err)
	}
}

// TestManagedServerWinsOverADisagreeingLock is the refresh's first outcome: the server's
// claim names the row, the lock says otherwise — the run proceeds on the
// server's row, rewrites the lock (an acting run only), and reports the unit
// as `adopt`. A dry-run reports the same and writes nothing, on either side.
func TestManagedServerWinsOverADisagreeingLock(t *testing.T) {
	fx := workflowIdempotenceFixture(t)
	adoptFixture(t, fx)
	editLock(t, fx.root, func(l *wfdir.Lock) {
		if err := l.SetManaged("prod", wfdir.WorkflowKind, "", "wf-stale", map[string]any{}); err != nil {
			t.Fatal(err)
		}
	})
	lockBefore := readFile(t, wfdir.LockPath(fx.root))

	fx.resetRequests()
	out, err := runCLI(t, fx.tree, "sync", "apply", "--stack", "prod", "--dry-run", "--json")
	if err != nil {
		t.Fatalf("dry-run: %v\n%s", err, out)
	}
	line := applyFolderLine(t, applyReportOf(t, out), filepath.Base(fx.root))
	if len(line.Units) != 1 || line.Units[0].Action != string(applyActionAdopt) || line.Units[0].ID != "wf-1" {
		t.Fatalf("dry-run units = %+v, want one adopt of wf-1", line.Units)
	}
	if writes := writesBesidesGate(fx.requests()); len(writes) != 0 {
		t.Fatalf("a dry-run sent %v", writes)
	}
	if got := readFile(t, wfdir.LockPath(fx.root)); got != lockBefore {
		t.Fatalf("a dry-run rewrote the lock:\n%s", got)
	}

	code, line, _, out := applyOnce(t, fx)
	if code != syncExitClean || line.Verdict != verdictApplied {
		t.Fatalf("apply: exit %d %+v\n%s", code, line, out)
	}
	lock, err := wfdir.LoadLock(fx.root)
	if err != nil {
		t.Fatal(err)
	}
	if id := lock.Binding("prod").WorkflowID; id != "wf-1" {
		t.Fatalf("lock still records %q, want the server's wf-1", id)
	}
	if lock.HeadVersion("prod") == "" {
		t.Fatalf("the rewritten lock has no anchor: the drift guard is disarmed")
	}
}

// TestManagedRefusesAClaimItCannotUse is the refresh's four refusing outcomes: the
// server's claim names a row this credential may not read, one in the trash,
// one that no longer exists, and one that now lives in another feature. Each
// refuses the folder under its own reason, names what the reader needs, and
// sends nothing.
func TestManagedRefusesAClaimItCannotUse(t *testing.T) {
	rows := map[string]struct {
		stage func(m *fakeManaged)
		want  []string
	}{
		syncReasonNotReadable: {func(m *fakeManaged) { m.unreadable["wf-1"] = true }, []string{fakePrincipal, "sync unmanage"}},
		syncReasonTrashed:     {func(m *fakeManaged) { m.trashed["wf-1"] = true }, []string{"wf-1", "Restore"}},
		syncReasonLost:        {func(m *fakeManaged) { m.gone["wf-1"] = true }, []string{"wf-1", "sync unmanage", "--allow-create"}},
		syncReasonMovedOut:    {func(m *fakeManaged) { m.moveRow("workflow", "wf-1", "feat-elsewhere") }, []string{"feat-elsewhere", "feat-1", "sync unmanage", "--allow-create", "back"}},
	}
	for reason, row := range rows {
		t.Run(reason, func(t *testing.T) {
			fx := workflowIdempotenceFixture(t)
			adoptFixture(t, fx)
			row.stage(managedFakeOf(fx))
			code, line, writes, out := applyOnce(t, fx)
			if code != syncExitDrifted || line.Verdict != verdictRefused || line.Reason != reason {
				t.Fatalf("exit %d, folder %+v, want refused %s\n%s", code, line, reason, out)
			}
			for _, w := range row.want {
				if !strings.Contains(line.Detail, w) {
					t.Errorf("detail does not name %q:\n%s", w, line.Detail)
				}
			}
			if len(writes) != 0 {
				t.Fatalf("a refused folder sent %v", writes)
			}
		})
	}
}

// TestManagedClaimsALockBoundRowWithNoClaim is the lazy backfill: an adopted
// folder whose server has no claim yet (the id was written by hand, or adopt
// never ran against this stack) claims the row its lock records, then deploys.
func TestManagedClaimsALockBoundRowWithNoClaim(t *testing.T) {
	for kind, setup := range idempotenceFixtures() {
		t.Run(kind, func(t *testing.T) { claimsALockBoundRowWithNoClaim(t, setup(t)) })
	}
}

func claimsALockBoundRowWithNoClaim(t *testing.T, fx idempotenceFixture) {
	m := managedFakeOf(fx)
	manifest := loadAnyManifest(t, fx.root)
	manifest.ID = "0b6f7c2e-3a41-4f0e-9d4c-7a5e2b9c1d08"
	if err := wfdir.SaveManifest(fx.root, manifest); err != nil {
		t.Fatal(err)
	}
	code, line, writes, out := applyOnce(t, fx)
	if code != syncExitClean {
		t.Fatalf("exit %d %+v\n%s", code, line, out)
	}
	if !slices.Contains(writes, "POST /api/v2/managed/claim") {
		t.Fatalf("no POST /managed/claim in %v", writes)
	}
	lock := mustLoadLock(t, fx.root)
	kind := mustKind(t, manifest.Kind)
	rows := lock.ManagedAddresses("prod", kind)
	if len(rows) == 0 {
		t.Fatal("the fixture's lock records no row")
	}
	for _, a := range rows {
		c := m.ClaimAt(featureOf(t, fx.root), manifest.ID, a.Address)
		if c == nil || c.ResourceID != a.ID {
			t.Fatalf("no claim of %s at %q after the backfill: %+v\n%s", a.ID, a.Address, c, out)
		}
		// The lock's anchor goes with it, even on a run that writes nothing
		// else — or the next runner without the lock has no drift baseline.
		if anchor := lock.Anchor("prod", kind, a.Address); len(anchor) > 0 && canonicalJSON(c.Applied) != canonicalJSON(anchor) {
			t.Fatalf("applied at %q = %v, want the lock's %v", a.Address, c.Applied, anchor)
		}
	}
}

// TestManagedRefusesABackfillOfARowManagedElsewhere: the lazy backfill's
// refusal — the row the lock records is already claimed at another address —
// names that address and claims nothing.
func TestManagedRefusesABackfillOfARowManagedElsewhere(t *testing.T) {
	fx := workflowIdempotenceFixture(t)
	m := managedFakeOf(fx)
	other := "11111111-2222-4333-8444-555555555555"
	m.stage("feat-1", other, "", "workflow", "wf-1", "github.com/acme/other//wf")
	manifest := loadAnyManifest(t, fx.root)
	manifest.ID = "0b6f7c2e-3a41-4f0e-9d4c-7a5e2b9c1d08"
	if err := wfdir.SaveManifest(fx.root, manifest); err != nil {
		t.Fatal(err)
	}
	// A change to deploy, so a refusal that came AFTER the push would show.
	fx.edit(t)
	code, line, writes, out := applyOnce(t, fx)
	if code != syncExitDrifted || line.Reason != syncReasonClaimedElsewhere {
		t.Fatalf("exit %d %+v, want refused %s\n%s", code, line, syncReasonClaimedElsewhere, out)
	}
	if !slices.Equal(writes, []string{"POST /api/v2/managed/claim"}) || fx.deployed() {
		t.Fatalf("the refusal came after a write: %v", writes)
	}
	if !strings.Contains(line.Detail, other) {
		t.Errorf("the refusal does not name the address holding the row:\n%s", line.Detail)
	}
	if c := m.ClaimAt("feat-1", manifest.ID, ""); c != nil {
		t.Fatalf("a claim was made anyway: %+v", c)
	}
}

// TestInitOnAStackMintsAFolderIDAndItsCreateClaims: `wf init --stack` is one of
// the two places an id is minted, and the first push's create carries
// `managed` — so a second push from a checkout with no lock finds the row by
// its address rather than creating another. Without --stack init mints
// nothing: the classic pattern is untouched.
func TestInitOnAStackMintsAFolderIDAndItsCreateClaims(t *testing.T) {
	f := newFakeInstance(t)
	signIn(t, f)
	root := t.TempDir()
	if _, err := runCLI(t, root, "wf", "init", "--feature", "feat-1", "--stack", "dev"); err != nil {
		t.Fatalf("init: %v", err)
	}
	id := folderIDOf(t, root)
	if v := loadAnyManifest(t, root).FormatVersion; v != 6 {
		t.Fatalf("init --stack wrote an id at format version %d, want 6", v)
	}
	writeLocal(t, root, "main.py", "body\n")
	if _, err := runCLI(t, root, "wf", "push"); err != nil {
		t.Fatalf("push: %v", err)
	}
	if len(f.created) != 1 || f.created[0].Managed == nil {
		t.Fatalf("the create carried no managed claim: %+v", f.created)
	}
	got := *f.created[0].Managed
	if got.FolderID != id || got.Address != "" || got.StackLabel != "dev" {
		t.Fatalf("managed = %+v, want folder %s, address \"\", stack dev", got, id)
	}
	c := f.managed.ClaimAt("feat-1", id, "")
	if c == nil || c.ResourceID != f.created[0].FeatureID && c.ResourceID == "" {
		t.Fatalf("no claim after the create: %+v", c)
	}
	createdID := c.ResourceID
	if len(c.Applied) == 0 {
		t.Fatalf("the create's anchor was not recorded on its claim")
	}

	removeLock(t, root)
	writeLocal(t, root, "main.py", "changed\n")
	if _, err := runCLI(t, root, "wf", "push"); err != nil {
		t.Fatalf("push with no lock: %v", err)
	}
	if len(f.created) != 1 {
		t.Fatalf("a push with no lock created a second workflow: %+v", f.created)
	}
	if got := f.FileContents(createdID)["main.py"]; got != "changed\n" {
		t.Fatalf("the push did not reach %s: main.py = %q", createdID, got)
	}

	plain := t.TempDir()
	if _, err := runCLI(t, plain, "wf", "init", "--feature", "feat-1"); err != nil {
		t.Fatalf("init without a stack: %v", err)
	}
	if strings.Contains(readFile(t, wfdir.ManifestPath(plain)), `"id"`) {
		t.Fatalf("init without --stack minted a folder id:\n%s", readFile(t, wfdir.ManifestPath(plain)))
	}
}

// TestManagedCreatesCarryTheirAddress: each per-file kind claims the address
// shape the server validates — x.sql, metrics/x.json, x.json — and the claim
// lands in the created row's own feature.
func TestManagedCreatesCarryTheirAddress(t *testing.T) {
	t.Run("pipeline", func(t *testing.T) {
		fx := pipelineIdempotenceFixture(t)
		adoptFixture(t, fx)
		writeLocal(t, fx.root, "shipped.sql", "SELECT * FROM raw WHERE shipped")
		writeLocal(t, fx.root, "metrics/margin.json", `{"recipe":`+strings.Replace(aovRecipe, `"source":"orders"`, `"source":"table-orders"`, 1)+`}`)
		out, err := runCLI(t, fx.tree, "sync", "apply", "--stack", "prod", "--allow-create", "--json")
		if err != nil {
			t.Fatalf("apply: %v\n%s", err, out)
		}
		id := folderIDOf(t, fx.root)
		var tables, metrics []string
		for _, in := range fx.pipeline.created {
			if in.Managed == nil || in.Managed.FolderID != id {
				t.Fatalf("a table create carried no claim: %+v", in)
			}
			tables = append(tables, in.Managed.Address)
		}
		for _, in := range fx.pipeline.metricsCreated {
			if in.Managed == nil || in.Managed.FolderID != id {
				t.Fatalf("a metric create carried no claim: %+v", in)
			}
			metrics = append(metrics, in.Managed.Address)
		}
		if !slices.Equal(tables, []string{"shipped.sql"}) || !slices.Equal(metrics, []string{"metrics/margin.json"}) {
			t.Fatalf("addresses = tables %v, metrics %v", tables, metrics)
		}
		for _, addr := range []string{"shipped.sql", "metrics/margin.json"} {
			if c := fx.pipeline.managed.ClaimAt("collection-1", id, addr); c == nil {
				t.Fatalf("no claim at %s", addr)
			}
		}
	})
	t.Run("automation", func(t *testing.T) {
		fx := automationIdempotenceFixture(t)
		adoptFixture(t, fx)
		writeLocal(t, fx.root, "weekly.json", `{"cronExpr": "0 4 * * 1"}`)
		out, err := runCLI(t, fx.tree, "sync", "apply", "--stack", "prod", "--allow-create", "--json")
		if err != nil {
			t.Fatalf("apply: %v\n%s", err, out)
		}
		created := fx.automation.created
		if len(created) != 1 || created[0].Managed == nil || created[0].Managed.Address != "weekly.json" {
			t.Fatalf("created = %+v", created)
		}
		if c := fx.automation.managed.ClaimAt("collection-1", folderIDOf(t, fx.root), "weekly.json"); c == nil {
			t.Fatal("no claim at weekly.json")
		}
	})
}

// TestManagedCreateAdoptsTheWinnerOfItsAddress: two applies create at one
// address at the same moment. The loser's create answers 409 address_claimed
// and is rolled back; it adopts the winner BY ID and continues as an update —
// one row, one claim.
func TestManagedCreateAdoptsTheWinnerOfItsAddress(t *testing.T) {
	t.Run("workflow", func(t *testing.T) {
		f := newFakeInstance(t)
		signIn(t, f)
		root := t.TempDir()
		if _, err := runCLI(t, root, "wf", "init", "--feature", "feat-1", "--stack", "dev"); err != nil {
			t.Fatalf("init: %v", err)
		}
		id := folderIDOf(t, root)
		writeLocal(t, root, "main.py", "body\n")
		title := loadAnyManifest(t, root).Title
		f.managed.race = func(m *fakeManaged) {
			// Another apply under the SAME credential (one CI token): its
			// parentless draft is the caller's own, so it reads ok.
			f.AddWorkflow(&api.Workflow{ID: "wf-winner", Lifecycle: api.LifecycleDraft, DrafterUserID: fakePrincipal, FeatureID: "feat-1", Title: title},
				api.WorkflowFile{Path: "main.py", Content: fakeEntrypointStarter})
			m.insert("feat-1", "workflow", "wf-winner", api.ManagedClaimInput{FolderID: id, StackLabel: "dev"})
		}
		out, err := runCLI(t, root, "wf", "push")
		if err != nil {
			t.Fatalf("push: %v\n%s", err, out)
		}
		lock, err := wfdir.LoadLock(root)
		if err != nil {
			t.Fatal(err)
		}
		if got := lock.Binding("dev").WorkflowID; got != "wf-winner" {
			t.Fatalf("bound to %q, want the winner wf-winner", got)
		}
		if len(f.managed.Claims("feat-1")) != 1 {
			t.Fatalf("claims = %+v", f.managed.Claims("feat-1"))
		}
		for wid := range f.workflows {
			if strings.HasPrefix(wid, "wf-new-") {
				t.Fatalf("the losing create was not rolled back: %s exists", wid)
			}
		}
		if got := f.FileContents("wf-winner")["main.py"]; got != "body\n" {
			t.Fatalf("the push did not continue onto the winner: main.py = %q", got)
		}
	})
	t.Run("table", func(t *testing.T) {
		fx := pipelineIdempotenceFixture(t)
		adoptFixture(t, fx)
		id := folderIDOf(t, fx.root)
		writeLocal(t, fx.root, "shipped.sql", "SELECT * FROM raw WHERE shipped")
		fx.pipeline.managed.race = func(m *fakeManaged) {
			fx.pipeline.tables["table-winner"] = &api.Table{ID: "table-winner", Name: "shipped", Kind: api.TableKindDerived,
				FeatureID: "collection-1", Code: "SELECT * FROM raw WHERE shipped", Status: api.TableStatusReady}
			fx.pipeline.synced["table-winner"] = true
			m.insert("collection-1", "table", "table-winner", api.ManagedClaimInput{FolderID: id, Address: "shipped.sql", StackLabel: "prod"})
		}
		out, err := runCLI(t, fx.tree, "sync", "apply", "--stack", "prod", "--allow-create", "--json")
		if err != nil {
			t.Fatalf("apply: %v\n%s", err, out)
		}
		if b := stackBindingOf(t, fx.root); b.Tables["shipped.sql"] != "table-winner" {
			t.Fatalf("shipped.sql bound to %q, want table-winner", b.Tables["shipped.sql"])
		}
		for tid := range fx.pipeline.tables {
			if strings.HasPrefix(tid, "table-new-") {
				t.Fatalf("the losing create was not rolled back: %s exists", tid)
			}
		}
	})
}

// TestManagedNameTakenRereadsState: a table's or metric's name
// gate runs before its claim, so a lost race answers name_taken. An adopted
// folder re-reads its state — the address now claimed means the winner is
// adopted by id; no claim means the name belongs to a row this folder does not
// manage, refused with the adoption remedy and never taken over by name.
func TestManagedNameTakenRereadsState(t *testing.T) {
	t.Run("claimed: adopted", func(t *testing.T) {
		fx := pipelineIdempotenceFixture(t)
		adoptFixture(t, fx)
		id := folderIDOf(t, fx.root)
		writeLocal(t, fx.root, "shipped.sql", "SELECT * FROM raw WHERE shipped")
		fx.pipeline.nameTakenOnCreate = `a table named "shipped" already exists in this feature`
		fx.pipeline.AddTable(&api.Table{ID: "table-winner", Name: "shipped", FeatureID: "collection-1", Code: "SELECT * FROM raw WHERE shipped"})
		fx.pipeline.managed.afterList = func(m *fakeManaged) {
			m.insert("collection-1", "table", "table-winner", api.ManagedClaimInput{FolderID: id, Address: "shipped.sql", StackLabel: "prod"})
		}
		out, err := runCLI(t, fx.tree, "sync", "apply", "--stack", "prod", "--allow-create", "--json")
		if err != nil {
			t.Fatalf("apply: %v\n%s", err, out)
		}
		if b := stackBindingOf(t, fx.root); b.Tables["shipped.sql"] != "table-winner" {
			t.Fatalf("shipped.sql bound to %q, want table-winner", b.Tables["shipped.sql"])
		}
	})
	t.Run("unclaimed: refused", func(t *testing.T) {
		fx := pipelineIdempotenceFixture(t)
		adoptFixture(t, fx)
		writeLocal(t, fx.root, "shipped.sql", "SELECT * FROM raw WHERE shipped")
		fx.pipeline.nameTakenOnCreate = `a table named "shipped" already exists in this feature`
		fx.pipeline.AddTable(&api.Table{ID: "table-colleague", Name: "shipped", FeatureID: "collection-1", Code: "SELECT * FROM raw WHERE shipped"})
		out, stderr, err := runPipelineCLI(t, fx.tree, "sync", "apply", "--stack", "prod", "--allow-create", "--json")
		if exitCodeOf(err) != syncExitDrifted {
			t.Fatalf("exit %d, want refused\n%s", exitCodeOf(err), out)
		}
		if !strings.Contains(stderr, "ronja sync adopt") {
			t.Errorf("the refusal does not name the adoption remedy:\n%s", stderr)
		}
		if b := stackBindingOf(t, fx.root); b.Tables["shipped.sql"] != "" {
			t.Fatalf("shipped.sql was bound to %q by its name", b.Tables["shipped.sql"])
		}
	})
}

// TestAdoptedFolderNeverAdoptsAMetricByName: an adopted folder never locates
// a metric by its name. A
// legacy folder adopts a name_taken metric that holds exactly its file's
// definition; an adopted folder never does — the server's state is the only
// way it finds a row.
func TestAdoptedFolderNeverAdoptsAMetricByName(t *testing.T) {
	for _, adopted := range []bool{false, true} {
		name := "legacy: adopted by name"
		if adopted {
			name = "adopted folder: refused"
		}
		t.Run(name, func(t *testing.T) {
			fx := pipelineIdempotenceFixture(t)
			if adopted {
				adoptFixture(t, fx)
			}
			// A metric holding EXACTLY this file's definition — the case the
			// legacy reconcile adopts.
			recipe := string(canonicalRecipe(t, json.RawMessage(strings.Replace(aovRecipe, `"source":"orders"`, `"source":"table-orders"`, 1))))
			writeLocal(t, fx.root, "metrics/margin.json", `{"recipe":`+recipe+`}`)
			fx.pipeline.nameTakenOnMetricCreate = `a metric named "margin" already exists in this feature`
			fx.pipeline.AddMetric("metric-hand-made", "margin", recipe)
			out, err := runCLI(t, fx.tree, "sync", "apply", "--stack", "prod", "--allow-create", "--json")
			id, _, _ := mustLoadLock(t, fx.root).MetricSeen("prod", "metrics/margin.json")
			if !adopted {
				if err != nil || id != "metric-hand-made" {
					t.Fatalf("the legacy control did not adopt by name (id %q, %v)\n%s", id, err, out)
				}
				return
			}
			if exitCodeOf(err) != syncExitDrifted {
				t.Fatalf("exit %d, want refused\n%s", exitCodeOf(err), out)
			}
			if id != "" {
				t.Fatalf("metrics/margin.json was bound to %s by its name", id)
			}
		})
	}
}

// stackBindingOf is the prod stack's binding as the lock records it.
func stackBindingOf(t *testing.T, root string) wfdir.Binding {
	t.Helper()
	lock, err := wfdir.LoadLock(root)
	if err != nil {
		t.Fatal(err)
	}
	return lock.Binding("prod")
}

// withSourceLabel stands every folder in a repository with this remote for the
// rest of the test.
func withSourceLabel(t *testing.T, remote string) {
	t.Helper()
	previous := sourceLabelOf
	sourceLabelOf = func(root string) string { return remote + "//" + filepath.Base(root) }
	t.Cleanup(func() { sourceLabelOf = previous })
}

// TestManagedAnchorWriteIsACompareAndSet: the anchor a deploy records goes out
// as a CAS on the claim's revision. A revision that moved with the anchor
// unchanged (a relabel) is re-read and retried once; an anchor another apply
// recorded meanwhile is surfaced, not overwritten; a claim released meanwhile
// is reported as no longer managed.
func TestManagedAnchorWriteIsACompareAndSet(t *testing.T) {
	deploy := func(t *testing.T, stage func(m *fakeManaged)) (int, syncApplyFolderReport, idempotenceFixture, string) {
		fx := workflowIdempotenceFixture(t)
		adoptFixture(t, fx)
		fx.edit(t)
		stage(managedFakeOf(fx))
		code, line, _, out := applyOnce(t, fx)
		return code, line, fx, out
	}
	t.Run("revision moved, anchor did not: retried", func(t *testing.T) {
		code, line, fx, out := deploy(t, func(m *fakeManaged) { m.staleOnce = true })
		if code != syncExitClean || line.Verdict != verdictApplied {
			t.Fatalf("exit %d %+v\n%s", code, line, out)
		}
		c := managedFakeOf(fx).ClaimAt("feat-1", folderIDOf(t, fx.root), "")
		lock, _ := wfdir.LoadLock(fx.root)
		if c.Applied["headVersionID"] != lock.HeadVersion("prod") {
			t.Fatalf("applied = %v, lock head = %s — the retry did not land", c.Applied, lock.HeadVersion("prod"))
		}
	})
	t.Run("another anchor recorded: surfaced", func(t *testing.T) {
		code, line, fx, out := deploy(t, func(m *fakeManaged) {
			m.raceOnce = map[string]any{"headVersionID": "wfv-theirs", "title": "Monthly report"}
		})
		if code != syncExitDrifted || line.Reason != syncReasonAnchorNotRecorded {
			t.Fatalf("exit %d %+v, want refused %s\n%s", code, line, syncReasonAnchorNotRecorded, out)
		}
		if c := managedFakeOf(fx).ClaimAt("feat-1", folderIDOf(t, fx.root), ""); c.Applied["headVersionID"] != "wfv-theirs" {
			t.Fatalf("the other apply's anchor was overwritten: %v", c.Applied)
		}
		if !fx.deployed() {
			t.Fatalf("the change itself did not land")
		}
	})
	t.Run("claim released meanwhile: lost", func(t *testing.T) {
		code, line, _, out := deploy(t, func(m *fakeManaged) { m.dropOnApplied = true })
		if code != syncExitDrifted || line.Reason != syncReasonLost {
			t.Fatalf("exit %d %+v, want refused %s\n%s", code, line, syncReasonLost, out)
		}
		if !strings.Contains(line.Detail, "ronja sync adopt") {
			t.Errorf("the 404 does not name the remedy:\n%s", line.Detail)
		}
	})
}

// TestManagedMovedAndRemovedReKeyTheServer is `moved` / `removed` against server state: in an
// adopted folder a `moved` entry re-keys the CLAIM (POST /managed/move) and a
// `removed` one unmanages it (DELETE), and neither touches a row — the moved
// table is updated in place, the removed metric is still there.
func TestManagedMovedAndRemovedReKeyTheServer(t *testing.T) {
	fx := pipelineIdempotenceFixture(t)
	adoptFixture(t, fx)
	id := folderIDOf(t, fx.root)
	m := fx.pipeline.managed
	for _, rename := range [][2]string{{"orders.sql", "sales_orders.sql"}, {"checks/orders.json", "checks/sales_orders.json"}} {
		if err := os.Rename(filepath.Join(fx.root, rename[0]), filepath.Join(fx.root, rename[1])); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.Remove(filepath.Join(fx.root, "metrics", "aov.json")); err != nil {
		t.Fatal(err)
	}
	manifest := loadAnyManifest(t, fx.root)
	manifest.FormatVersion = 5
	manifest.Moved = []wfdir.MovedEntry{{From: "orders.sql", To: "sales_orders.sql"}}
	manifest.Removed = []string{"metrics/aov.json"}
	if err := wfdir.SaveManifest(fx.root, manifest); err != nil {
		t.Fatal(err)
	}

	// The CI shape: no lock at all — the declarations are evaluated against
	// the server's state alone.
	removeLock(t, fx.root)
	code, line, writes, out := applyOnce(t, fx)
	if code != syncExitClean {
		t.Fatalf("exit %d %+v\n%s", code, line, out)
	}
	if !slices.Contains(writes, "POST /api/v2/managed/move") {
		t.Fatalf("no move was sent: %v", writes)
	}
	if c := m.ClaimAt("collection-1", id, "sales_orders.sql"); c == nil || c.ResourceID != "table-orders" {
		t.Fatalf("the claim was not re-keyed: %+v", c)
	}
	if c := m.ClaimAt("collection-1", id, "orders.sql"); c != nil {
		t.Fatalf("a claim stayed at the old address: %+v", c)
	}
	if c := m.ClaimAt("collection-1", id, "metrics/aov.json"); c != nil {
		t.Fatalf("the removed metric is still claimed: %+v", c)
	}
	if fx.pipeline.tables["metric-aov"] == nil {
		t.Fatalf("the removed metric's ROW was touched")
	}
	if len(fx.pipeline.created) != 0 {
		t.Fatalf("a move created a table: %+v", fx.pipeline.created)
	}

	// Safe to keep: the next run finds the move applied and sends nothing.
	removeLock(t, fx.root)
	if _, _, writes, out := applyOnce(t, fx); len(writes) != 0 {
		t.Fatalf("a second run re-sent %v\n%s", writes, out)
	}
}

// TestManagedMovedFromAStaleCommittedLock is the CI shape that keeps the lock
// it committed at adopt time and never commits another: run one applies a
// `moved` and a `removed` and throws its lock away, and run two restores the
// committed lock, which still records orders.sql → table-orders and the removed
// metric. The server wins row → address as well as address → row: the stale
// entries are rewritten from the server's state, so run two is the move already
// applied — no refusal, no write, no second claim.
func TestManagedMovedFromAStaleCommittedLock(t *testing.T) {
	fx := pipelineIdempotenceFixture(t)
	adoptFixture(t, fx)
	id := folderIDOf(t, fx.root)
	m := fx.pipeline.managed
	for _, rename := range [][2]string{{"orders.sql", "sales_orders.sql"}, {"checks/orders.json", "checks/sales_orders.json"}} {
		if err := os.Rename(filepath.Join(fx.root, rename[0]), filepath.Join(fx.root, rename[1])); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.Remove(filepath.Join(fx.root, "metrics", "aov.json")); err != nil {
		t.Fatal(err)
	}
	manifest := loadAnyManifest(t, fx.root)
	manifest.FormatVersion = 5
	manifest.Moved = []wfdir.MovedEntry{{From: "orders.sql", To: "sales_orders.sql"}}
	manifest.Removed = []string{"metrics/aov.json"}
	if err := wfdir.SaveManifest(fx.root, manifest); err != nil {
		t.Fatal(err)
	}
	// What the repository holds: the lock as adopt left it.
	committed := snapshotFolder(t, fx.root, true)

	for run := 1; run <= 2; run++ {
		restoreCheckout(t, fx.root, committed)
		code, line, writes, out := applyOnce(t, fx)
		if code != syncExitClean || line.Verdict != verdictApplied {
			t.Fatalf("run %d: exit %d %+v\n%s", run, code, line, out)
		}
		if run == 2 && len(writes) != 0 {
			t.Fatalf("run 2 re-sent %v\n%s", writes, out)
		}
		if c := m.ClaimAt("collection-1", id, "sales_orders.sql"); c == nil || c.ResourceID != "table-orders" {
			t.Fatalf("run %d: the claim is not at sales_orders.sql: %+v", run, c)
		}
		for _, gone := range []string{"orders.sql", "metrics/aov.json"} {
			if c := m.ClaimAt("collection-1", id, gone); c != nil {
				t.Fatalf("run %d: a claim is still at %s: %+v", run, gone, c)
			}
		}
		if len(fx.pipeline.created) != 0 || len(fx.pipeline.metricsCreated) != 0 {
			t.Fatalf("run %d created rows: tables %+v, metrics %+v", run, fx.pipeline.created, fx.pipeline.metricsCreated)
		}
	}
	lock := mustLoadLock(t, fx.root)
	if b := lock.Binding("prod"); b.Tables["orders.sql"] != "" || b.Tables["sales_orders.sql"] != "table-orders" {
		t.Fatalf("the lock was not rewritten from the server: %+v", b.Tables)
	}
}

// TestManagedLocklessApplyKeepsTheChecksDriftGuard: a table's checks record is
// part of the folder's anchor, so a runner with no lock still knows what the folder last
// verified on each check — a check re-severitied in the app since is drift, not
// something a lock-less apply silently writes back over.
func TestManagedLocklessApplyKeepsTheChecksDriftGuard(t *testing.T) {
	fx := pipelineIdempotenceFixture(t)
	writeLocal(t, fx.root, "checks/orders.json",
		`{"checks":[{"name":"Unik rad per id","expression":"count(*) = count(distinct id)","severity":"warn"}]}`)
	adoptFixture(t, fx)
	if code, line, _, out := applyOnce(t, fx); code != syncExitClean {
		t.Fatalf("first apply: exit %d %+v\n%s", code, line, out)
	}
	if a := fx.pipeline.managed.FolderAnchor("collection-1", folderIDOf(t, fx.root)); a == nil || a.Applied["checks"] == nil {
		t.Fatalf("the folder's anchor carries no checks record: %+v", a)
	}
	fx.pipeline.mu.Lock()
	for _, row := range fx.pipeline.checkState.rows["table-orders"] {
		if row.Name == "Unik rad per id" {
			row.Severity = "fail"
		}
	}
	fx.pipeline.mu.Unlock()
	removeLock(t, fx.root)
	code, line, _, out := applyOnce(t, fx)
	if code != syncExitDrifted || line.Reason != syncReasonChecksNotApplied || !strings.Contains(line.Detail, "changed in the app") {
		t.Fatalf("a re-severitied check on a lock-less apply: exit %d %+v — want refused as drift\n%s", code, line, out)
	}
	if got := fx.pipeline.checkNamed("table-orders", "Unik rad per id").Severity; got != "fail" {
		t.Fatalf("the app's severity was overwritten: %q", got)
	}
}

// TestManagedTableWithManyChecksRecordsItsAnchor: a table's checks records
// (one per check, ~160 bytes each) live on the FOLDER's anchor (256 KiB), not
// on the table's claim anchor, which the server caps at 16 KiB — on the claim,
// a table with ~100 checks (a not_null per column of a wide table) failed every
// apply with anchor_not_recorded and left a lockless runner with no checks
// baseline.
func TestManagedTableWithManyChecksRecordsItsAnchor(t *testing.T) {
	fx := pipelineIdempotenceFixture(t)
	checks := []string{`{"name":"Unik rad per id","expression":"count(*) = count(distinct id)"}`}
	for i := 0; i < 150; i++ {
		checks = append(checks, fmt.Sprintf(`{"name":"column_%03d is never null","expression":"count(*) = count(column_%03d)","severity":"warn"}`, i, i))
	}
	writeLocal(t, fx.root, "checks/orders.json", `{"checks":[`+strings.Join(checks, ",")+`]}`)
	adoptFixture(t, fx)
	if code, line, _, out := applyOnce(t, fx); code != syncExitClean {
		t.Fatalf("apply of a table with 150 checks: exit %d %s / %s — %s\n%s", code, line.Verdict, line.Reason, line.Detail, out)
	}
	folder := folderIDOf(t, fx.root)
	a := fx.pipeline.managed.FolderAnchor("collection-1", folder)
	if a == nil || a.Applied["checks"] == nil {
		t.Fatalf("the folder anchor carries no checks records: %+v", a)
	}
	if c := fx.pipeline.managed.ClaimAt("collection-1", folder, "orders.sql"); c == nil || c.Applied["checks"] != nil {
		t.Fatalf("the table's claim anchor still carries the checks record: %+v", c)
	}
	// And a runner with no lock reads them back: nothing to write.
	removeLock(t, fx.root)
	if code, line, writes, out := applyOnce(t, fx); code != syncExitClean || len(writes) != 0 {
		t.Fatalf("lockless apply: exit %d %+v, writes %v\n%s", code, line, writes, out)
	}
}

// TestManagedLocklessApplyKeepsTheDocsDriftGuard is the checks test's sibling
// for a docs sidecar, which has no claim of its own: its record is on the
// FOLDER's anchor and on no claim's, so a lock-less runner still refuses prose
// rewritten in the app rather than writing the file's back over it.
func TestManagedLocklessApplyKeepsTheDocsDriftGuard(t *testing.T) {
	fx := pipelineIdempotenceFixture(t)
	adoptFixture(t, fx)
	if code, line, _, out := applyOnce(t, fx); code != syncExitClean {
		t.Fatalf("first apply: exit %d %+v\n%s", code, line, out)
	}
	folder := folderIDOf(t, fx.root)
	if docs := folderDocsOf(t, fx.pipeline.managed, "collection-1", folder); docs["tables/fortnox_invoices.json"].TableID != "table-raw" {
		t.Fatalf("the folder anchor carries no sidecar record: %+v", docs)
	}
	for _, c := range fx.pipeline.managed.Claims("collection-1") {
		if c.Applied[wfdir.LegacyDocsAnchorKey] != nil {
			t.Fatalf("a claim's anchor carries the sidecar records: %s %v", c.Address, c.Applied)
		}
	}
	fx.pipeline.mu.Lock()
	fx.pipeline.tables["table-raw"].Description = "Rewritten in the app."
	fx.pipeline.mu.Unlock()
	writeSidecar(t, fx.root, "fortnox_invoices", `{"description": "One row per Fortnox invoice line.", "columns": {"amount": "line total"}}`)
	removeLock(t, fx.root)
	code, line, _, out := applyOnce(t, fx)
	if code == syncExitClean {
		t.Fatalf("prose rewritten in the app, applied over on a lock-less run: %+v\n%s", line, out)
	}
	fx.pipeline.mu.Lock()
	got := fx.pipeline.tables["table-raw"].Description
	fx.pipeline.mu.Unlock()
	if got != "Rewritten in the app." {
		t.Fatalf("the app's prose was overwritten: %q", got)
	}
}

// TestManagedTombstone: a managed row an admin moves to another feature leaves
// its claim behind as a TOMBSTONE (rmanaged.DepartOnMoveTx). A lock-less apply
// with --allow-create refuses moved_out naming both features and creates
// nothing; moving the row back revives the claim; and unmanaging the tombstone
// is what lets the file's row be created here anew.
func TestManagedTombstone(t *testing.T) {
	t.Run("workflow: refused, then revived by the move back", func(t *testing.T) {
		fx := workflowIdempotenceFixture(t)
		adoptFixture(t, fx)
		// One clean apply first, so the anchor is complete and the run after
		// the move back has nothing at all to record.
		if code, line, _, out := applyOnce(t, fx); code != syncExitClean {
			t.Fatalf("first apply: exit %d %+v\n%s", code, line, out)
		}
		m := managedFakeOf(fx)
		id := folderIDOf(t, fx.root)
		before := *m.ClaimAt("feat-1", id, "")
		m.moveRow("workflow", "wf-1", "feat-elsewhere")
		removeLock(t, fx.root)
		out, err := runCLI(t, fx.tree, "sync", "apply", "--stack", "prod", "--allow-create", "--json")
		line := applyFolderLine(t, applyReportOf(t, out), filepath.Base(fx.root))
		if exitCodeOf(err) != syncExitDrifted || line.Reason != syncReasonMovedOut {
			t.Fatalf("exit %d %+v, want refused moved_out\n%s", exitCodeOf(err), line, out)
		}
		for _, w := range []string{"feat-elsewhere", "feat-1", "tombstone", "sync unmanage", "--allow-create"} {
			if !strings.Contains(line.Detail, w) {
				t.Errorf("the refusal does not name %q:\n%s", w, line.Detail)
			}
		}
		if len(fx.wf.created) != 0 {
			t.Fatalf("a tombstoned address was created over: %+v", fx.wf.created)
		}

		m.moveRow("workflow", "wf-1", "feat-1")
		code, line, writes, out := applyOnce(t, fx)
		if code != syncExitClean || len(writes) != 0 {
			t.Fatalf("after the move back: exit %d %+v, writes %v\n%s", code, line, writes, out)
		}
		c := m.ClaimAt("feat-1", id, "")
		if c == nil || c.departed() || c.Revision <= before.Revision || canonicalJSON(c.Applied) != canonicalJSON(before.Applied) {
			t.Fatalf("the claim was not revived with its anchor: before %+v, after %+v", before, c)
		}
	})
	t.Run("pipeline table: a lock-less --allow-create creates nothing", func(t *testing.T) {
		fx := pipelineIdempotenceFixture(t)
		adoptFixture(t, fx)
		fx.pipeline.managed.moveRow("table", "table-orders", "collection-2")
		removeLock(t, fx.root)
		out, err := runCLI(t, fx.tree, "sync", "apply", "--stack", "prod", "--allow-create", "--json")
		line := applyFolderLine(t, applyReportOf(t, out), filepath.Base(fx.root))
		if exitCodeOf(err) != syncExitDrifted || line.Reason != syncReasonMovedOut {
			t.Fatalf("exit %d %+v, want refused moved_out\n%s", exitCodeOf(err), line, out)
		}
		if len(fx.pipeline.created) != 0 {
			t.Fatalf("a tombstoned address was created over: %+v", fx.pipeline.created)
		}
	})
	t.Run("unmanage frees the address for --allow-create", func(t *testing.T) {
		fx := automationIdempotenceFixture(t)
		adoptFixture(t, fx)
		m := fx.automation.managed
		m.moveRow("automation", "job-1", "collection-2")
		if out, err := runCLI(t, fx.tree, "sync", "unmanage", "--stack", "prod", "--json"); err != nil {
			t.Fatalf("unmanage: %v\n%s", err, out)
		}
		if id, _ := mustLoadLock(t, fx.root).AutomationSeen("prod", "nightly.json"); id != "" {
			t.Fatalf("the moved row %s is still in the lock — a classic push would write into another feature", id)
		}
		adoptFixture(t, fx)
		// The fake numbers its creates from job-1, which the fixture's row
		// already is.
		fx.automation.nextID = 100
		out, err := runCLI(t, fx.tree, "sync", "apply", "--stack", "prod", "--allow-create", "--json")
		if err != nil {
			t.Fatalf("apply --allow-create: %v\n%s", err, out)
		}
		if len(fx.automation.created) != 1 {
			t.Fatalf("created = %d, want the file's row created here anew", len(fx.automation.created))
		}
		if row := fx.automation.RowOf("job-1"); row == nil || row.FeatureID != "collection-2" || row.CronExpr != "0 2 * * *" {
			t.Fatalf("the moved row was touched: %+v", row)
		}
	})
	t.Run("a create that meets a tombstone refuses and never adopts", func(t *testing.T) {
		fx := pipelineIdempotenceFixture(t)
		adoptFixture(t, fx)
		id := folderIDOf(t, fx.root)
		writeLocal(t, fx.root, "shipped.sql", "SELECT * FROM raw WHERE shipped")
		fx.pipeline.managed.afterList = func(m *fakeManaged) {
			m.stageTombstoneLocked("collection-1", id, "shipped.sql", "table", "table-gone", "collection-2")
		}
		out, err := runCLI(t, fx.tree, "sync", "apply", "--stack", "prod", "--allow-create", "--json")
		line := applyFolderLine(t, applyReportOf(t, out), filepath.Base(fx.root))
		if exitCodeOf(err) != syncExitDrifted || line.Reason != syncReasonMovedOut {
			t.Fatalf("exit %d %+v, want refused moved_out\n%s", exitCodeOf(err), line, out)
		}
		for tid := range fx.pipeline.tables {
			if strings.HasPrefix(tid, "table-new-") {
				t.Fatalf("the create was not rolled back: %s exists", tid)
			}
		}
		if b := stackBindingOf(t, fx.root); b.Tables["shipped.sql"] != "" {
			t.Fatalf("shipped.sql was bound to %q", b.Tables["shipped.sql"])
		}
	})
	t.Run("the lazy claim meets a tombstone at its address", func(t *testing.T) {
		fx := workflowIdempotenceFixture(t)
		manifest := loadAnyManifest(t, fx.root)
		manifest.ID = "0b6f7c2e-3a41-4f0e-9d4c-7a5e2b9c1d08"
		if err := wfdir.SaveManifest(fx.root, manifest); err != nil {
			t.Fatal(err)
		}
		fx.wf.managed.afterList = func(m *fakeManaged) {
			m.stageTombstoneLocked("feat-1", manifest.ID, "", "workflow", "wf-other", "feat-elsewhere")
		}
		code, line, _, out := applyOnce(t, fx)
		if code != syncExitDrifted || line.Reason != syncReasonMovedOut || !strings.Contains(line.Detail, "tombstone") {
			t.Fatalf("exit %d %+v, want refused moved_out\n%s", code, line, out)
		}
	})
	t.Run("an anchor write meets a move", func(t *testing.T) {
		fx := workflowIdempotenceFixture(t)
		adoptFixture(t, fx)
		fx.edit(t)
		m := managedFakeOf(fx)
		m.beforeApplied = func(m *fakeManaged, c *fakeClaim) { m.moveRowLocked(c.Kind, c.ResourceID, "feat-elsewhere") }
		code, line, _, out := applyOnce(t, fx)
		if code != syncExitDrifted || line.Reason != syncReasonMovedOut {
			t.Fatalf("exit %d %+v, want refused moved_out\n%s", code, line, out)
		}
	})
	t.Run("a moved declaration meets a move", func(t *testing.T) {
		fx := pipelineIdempotenceFixture(t)
		adoptFixture(t, fx)
		for _, rename := range [][2]string{{"orders.sql", "sales_orders.sql"}, {"checks/orders.json", "checks/sales_orders.json"}} {
			if err := os.Rename(filepath.Join(fx.root, rename[0]), filepath.Join(fx.root, rename[1])); err != nil {
				t.Fatal(err)
			}
		}
		manifest := loadAnyManifest(t, fx.root)
		manifest.FormatVersion = 5
		manifest.Moved = []wfdir.MovedEntry{{From: "orders.sql", To: "sales_orders.sql"}}
		if err := wfdir.SaveManifest(fx.root, manifest); err != nil {
			t.Fatal(err)
		}
		fx.pipeline.managed.afterList = func(m *fakeManaged) { m.moveRowLocked("table", "table-orders", "collection-2") }
		code, line, _, out := applyOnce(t, fx)
		if code != syncExitDrifted || line.Reason != syncReasonMovedOut {
			t.Fatalf("exit %d %+v, want refused moved_out\n%s", code, line, out)
		}
	})
}

// TestManagedAnchorWriteBackfillsAnEmptySourceLabel: a claim made from outside
// git carries a local label (an older CLI recorded none); the next anchor
// write from a folder with a remote replaces it — the quiet relabel
// managed_source.go promises. Only the same repository is relabelled this way
// (a foreign one needs --adopt-foreign).
func TestManagedAnchorWriteBackfillsAnEmptySourceLabel(t *testing.T) {
	fx := workflowIdempotenceFixture(t)
	adoptFixture(t, fx)
	m := managedFakeOf(fx)
	id := folderIDOf(t, fx.root)
	if c := m.ClaimAt("feat-1", id, ""); c == nil || !isLocalSource(c.SourceLabel) {
		t.Fatalf("the fixture's claim already has a label: %+v", c)
	}
	withSourceLabel(t, "github.com/acme/data")
	fx.edit(t)
	if code, line, _, out := applyOnce(t, fx); code != syncExitClean {
		t.Fatalf("exit %d %+v\n%s", code, line, out)
	}
	if c := m.ClaimAt("feat-1", id, ""); !strings.HasPrefix(c.SourceLabel, "github.com/acme/data//") {
		t.Fatalf("the empty label was not backfilled: %q", c.SourceLabel)
	}
}

// TestManagedAnchor404NamesTheClaimsState: the server answers 404 to an anchor
// write both for a released claim and for a row this credential may no longer
// write. The CLI re-reads and names which — here, a row it may not read.
func TestManagedAnchor404NamesTheClaimsState(t *testing.T) {
	fx := workflowIdempotenceFixture(t)
	adoptFixture(t, fx)
	fx.edit(t)
	m := managedFakeOf(fx)
	m.beforeApplied = func(m *fakeManaged, c *fakeClaim) { m.unreadable[c.ResourceID] = true }
	code, line, _, out := applyOnce(t, fx)
	if code != syncExitDrifted || line.Reason != syncReasonNotReadable || strings.Contains(line.Detail, "released while this ran") {
		t.Fatalf("exit %d %+v, want refused not_readable, not \"released\"\n%s", code, line, out)
	}
}

// TestManagedConcurrentMovedIsAlreadyApplied: two runs apply one `moved` at
// once. The loser's POST /managed/move answers 404 not_claimed — its `from` is
// already gone — and that is the declaration applied, not a failure: the claim
// sits at `to` holding the same row.
func TestManagedConcurrentMovedIsAlreadyApplied(t *testing.T) {
	fx := pipelineIdempotenceFixture(t)
	adoptFixture(t, fx)
	id := folderIDOf(t, fx.root)
	for _, rename := range [][2]string{{"orders.sql", "sales_orders.sql"}, {"checks/orders.json", "checks/sales_orders.json"}} {
		if err := os.Rename(filepath.Join(fx.root, rename[0]), filepath.Join(fx.root, rename[1])); err != nil {
			t.Fatal(err)
		}
	}
	manifest := loadAnyManifest(t, fx.root)
	manifest.Moved = []wfdir.MovedEntry{{From: "orders.sql", To: "sales_orders.sql"}}
	if err := wfdir.SaveManifest(fx.root, manifest); err != nil {
		t.Fatal(err)
	}
	// The other run's move commits between this run's read and its own move.
	fx.pipeline.managed.afterList = func(m *fakeManaged) {
		m.byAddress("collection-1", id, "orders.sql").Address = "sales_orders.sql"
	}
	code, line, _, out := applyOnce(t, fx)
	if code != syncExitClean || line.Verdict != verdictApplied {
		t.Fatalf("exit %d %+v — a move another run applied first is not a failure\n%s", code, line, out)
	}
	if c := fx.pipeline.managed.ClaimAt("collection-1", id, "sales_orders.sql"); c == nil || c.ResourceID != "table-orders" {
		t.Fatalf("claim = %+v", c)
	}
	if len(fx.pipeline.created) != 0 {
		t.Fatalf("created %+v", fx.pipeline.created)
	}
}

// TestManagedNonDeployCommandsWriteNoServerState: `wf run` and `bind` act on a
// folder without deploying it, so an adopted folder whose state has work
// pending — a row its lock records with no claim yet — reads that state and
// writes none of it: no claim, no move, no anchor.
func TestManagedNonDeployCommandsWriteNoServerState(t *testing.T) {
	fx := workflowIdempotenceFixture(t)
	m := managedFakeOf(fx)
	manifest := loadAnyManifest(t, fx.root)
	manifest.ID = "0b6f7c2e-3a41-4f0e-9d4c-7a5e2b9c1d08"
	if err := wfdir.SaveManifest(fx.root, manifest); err != nil {
		t.Fatal(err)
	}
	for _, args := range [][]string{{"wf", "run"}, {"bind", "--stack", "prod", "--yes"}} {
		m.mu.Lock()
		m.requests = nil
		m.mu.Unlock()
		_, _ = runCLI(t, fx.root, args...)
		if writes := m.writesSeen(); len(writes) != 0 {
			t.Fatalf("%v wrote server state: %v", args, writes)
		}
		if len(m.Claims("feat-1")) != 0 {
			t.Fatalf("%v claimed a row: %+v", args, m.Claims("feat-1"))
		}
	}
	// The control: a deploy settles it.
	if out, err := runCLI(t, fx.root, "wf", "push"); err != nil {
		t.Fatalf("push: %v\n%s", err, out)
	}
	if len(m.Claims("feat-1")) != 1 {
		t.Fatalf("a push did not claim the row: %+v", m.Claims("feat-1"))
	}
}

// TestManagedAnchorlessAutomationAtDesiredIsReanchored is "live equal to
// desired is never drift" for the CI wedge:
// a claim whose anchor never landed (a crash or a failed PUT after the create)
// on a runner with no lock used to refuse no_drift_anchor for ever. When the
// live row already holds the file, apply records the anchor instead.
func TestManagedAnchorlessAutomationAtDesiredIsReanchored(t *testing.T) {
	fx := automationIdempotenceFixture(t)
	adoptFixture(t, fx)
	m := fx.automation.managed
	c := m.ClaimAt("collection-1", folderIDOf(t, fx.root), "nightly.json")
	m.mu.Lock()
	c.Applied = map[string]any{}
	m.mu.Unlock()
	removeLock(t, fx.root)
	code, line, writes, out := applyOnce(t, fx)
	if code != syncExitClean || line.Verdict != verdictApplied {
		t.Fatalf("exit %d %+v — live equals desired, want re-anchored\n%s", code, line, out)
	}
	for _, w := range writes {
		if !strings.Contains(w, "/api/v2/managed") {
			t.Fatalf("a re-anchor wrote the row: %v", writes)
		}
	}
	if got := m.ClaimAt("collection-1", folderIDOf(t, fx.root), "nightly.json").Applied["updatedAt"]; got == nil || got == "" {
		t.Fatalf("no anchor was recorded: %v", m.ClaimAt("collection-1", folderIDOf(t, fx.root), "nightly.json").Applied)
	}
	// Differing, it still refuses: there is something to overwrite.
	c = m.ClaimAt("collection-1", folderIDOf(t, fx.root), "nightly.json")
	m.mu.Lock()
	c.Applied = map[string]any{}
	m.mu.Unlock()
	removeLock(t, fx.root)
	fx.edit(t)
	if code, line, _, out := applyOnce(t, fx); code != syncExitDrifted || line.Reason != syncReasonNoDriftAnchor {
		t.Fatalf("an anchorless row that differs: exit %d %+v\n%s", code, line, out)
	}
}

// TestManagedAnchorlessAutomationReDecidedAtWriteTime: the plan proves live
// equals desired for an anchorless automation and lets it through as an
// update, but the WRITE comes later — after every earlier folder has run. An
// operator who pauses the automation in that window must not have the push
// turn it back on: with no anchor both of the push's guards are disarmed, so
// the push itself re-decides from the row it reads, records the anchor only
// when live still equals desired, and otherwise refuses no_drift_anchor.
func TestManagedAnchorlessAutomationReDecidedAtWriteTime(t *testing.T) {
	fx := automationIdempotenceFixture(t)
	writeLocal(t, fx.root, "nightly.json", `{"cronExpr": "0 2 * * *", "enabled": true}`)
	adoptFixture(t, fx)
	m := fx.automation.managed
	c := m.ClaimAt("collection-1", folderIDOf(t, fx.root), "nightly.json")
	m.mu.Lock()
	c.Applied = map[string]any{}
	m.mu.Unlock()
	removeLock(t, fx.root)
	// The plan's listing sees the row at desired; the operator pauses it before
	// the push reads it again.
	fx.automation.mu.Lock()
	fx.automation.afterList = func(f *fakeAutomationInstance) {
		row := f.rows["job-1"]
		row.Enabled, row.DisabledReason = false, "user"
		row.UpdatedAt = row.UpdatedAt.Add(time.Hour)
	}
	fx.automation.mu.Unlock()
	code, line, writes, out := applyOnce(t, fx)
	if code != syncExitDrifted || line.Reason != syncReasonNoDriftAnchor {
		t.Fatalf("exit %d %+v — want refused no_drift_anchor\n%s", code, line, out)
	}
	for _, w := range writes {
		if strings.HasPrefix(w, "PUT /api/v2/automations/") {
			t.Fatalf("the paused automation was written: %v", writes)
		}
	}
	if fx.automation.RowOf("job-1").Enabled {
		t.Fatal("the push turned a paused automation back on")
	}
}

// TestSyncUnmanageReleasesTheClaimsAndLeavesTheRows: unmanage releases every
// claim of the stack's feature, the rows untouched, removes the folder id — and
// the folder then runs legacy, sending nothing to /managed.
func TestSyncUnmanageReleasesTheClaimsAndLeavesTheRows(t *testing.T) {
	fx := pipelineIdempotenceFixture(t)
	adoptFixture(t, fx)
	m := fx.pipeline.managed
	rowsBefore := len(fx.pipeline.tables)
	out, err := runCLI(t, fx.tree, "sync", "unmanage", "--stack", "prod", "--json")
	if err != nil {
		t.Fatalf("unmanage: %v\n%s", err, out)
	}
	if got := m.Claims("collection-1"); len(got) != 0 {
		t.Fatalf("claims left: %+v", got)
	}
	if loadAnyManifest(t, fx.root).Adopted() {
		t.Fatal("the folder id was not removed")
	}
	if len(fx.pipeline.tables) != rowsBefore {
		t.Fatalf("rows went from %d to %d", rowsBefore, len(fx.pipeline.tables))
	}
	fx.resetRequests()
	if code, line, _, out := applyOnce(t, fx); code != syncExitClean {
		t.Fatalf("apply after unmanage: exit %d %+v\n%s", code, line, out)
	}
	if got := managedRequestsIn(fx.requests()); len(got) != 0 {
		t.Fatalf("an unmanaged folder still talked to /managed: %v", got)
	}
}

// TestSyncUnmanageKeepsTheRowsCIMade: a row created by a run whose lock was
// thrown away is known only to the server. Unmanage writes every ok claim's row
// (and its anchor) into the lock before releasing it — the lock is the classic
// pattern's only state — so the next classic push updates that row rather than
// creating a second one.
func TestSyncUnmanageKeepsTheRowsCIMade(t *testing.T) {
	f := newFakeInstance(t)
	signIn(t, f)
	tree := t.TempDir()
	root := filepath.Join(tree, "report")
	if err := os.MkdirAll(root, 0o755); err != nil {
		t.Fatal(err)
	}
	if _, err := runCLI(t, root, "wf", "init", "--feature", "feat-1", "--stack", "dev"); err != nil {
		t.Fatalf("init: %v", err)
	}
	writeLocal(t, root, "main.py", "body\n")
	if out, err := runCLI(t, root, "wf", "push"); err != nil {
		t.Fatalf("push: %v\n%s", err, out)
	}
	claim := f.managed.ClaimAt("feat-1", folderIDOf(t, root), "")
	if claim == nil {
		t.Fatal("the push's create claimed nothing")
	}
	// The CI runner's checkout: the lock the push wrote is gone.
	removeLock(t, root)
	if out, err := runCLI(t, tree, "sync", "unmanage", "--stack", "dev", "--json"); err != nil {
		t.Fatalf("unmanage: %v\n%s", err, out)
	}
	if loadAnyManifest(t, root).Adopted() {
		t.Fatal("the folder id was not removed")
	}
	if got := mustLoadLock(t, root).Binding("dev").WorkflowID; got != claim.ResourceID {
		t.Fatalf("the lock records %q after unmanage, want the row the server's claim named", got)
	}
	writeLocal(t, root, "main.py", "changed\n")
	if out, err := runCLI(t, root, "wf", "push"); err != nil {
		t.Fatalf("classic push after unmanage: %v\n%s", err, out)
	}
	if len(f.created) != 1 {
		t.Fatalf("created = %d — the classic push after unmanage made a second workflow", len(f.created))
	}
}

// TestSyncUnmanageOnAnInstanceWithoutManagedState: an adopted folder pointed at
// an instance that predates /managed is refused, naming `sync unmanage` — and
// that remedy has to work there: the route's 404 means no server state exists
// to strand, so unmanage removes the id and the folder runs from the lock.
func TestSyncUnmanageOnAnInstanceWithoutManagedState(t *testing.T) {
	fx := workflowIdempotenceFixture(t)
	adoptFixture(t, fx)
	m := managedFakeOf(fx)
	m.noRoute = true
	code, line, writes, out := applyOnce(t, fx)
	if code == syncExitClean || !strings.Contains(line.Detail+out, "sync unmanage") {
		t.Fatalf("an old instance: exit %d %+v\n%s", code, line, out)
	}
	if len(writes) != 0 {
		t.Fatalf("a refused folder sent %v", writes)
	}
	out, err := runCLI(t, fx.tree, "sync", "unmanage", "--stack", "prod", "--json")
	if err != nil {
		t.Fatalf("unmanage on an old instance: %v\n%s", err, out)
	}
	if loadAnyManifest(t, fx.root).Adopted() {
		t.Fatalf("the id stayed, so the refusal's remedy loops:\n%s", out)
	}
	fx.edit(t)
	if code, line, _, out := applyOnce(t, fx); code != syncExitClean || !fx.deployed() {
		t.Fatalf("apply from the lock after unmanage: exit %d %+v\n%s", code, line, out)
	}
}

// TestSyncUnmanageReleasesALostClaim: a claim whose row no longer exists is the
// one apply refuses (lost); unmanage releases it, drops the dead id from the
// lock, and the next adopt + apply --allow-create recreates the row.
func TestSyncUnmanageReleasesALostClaim(t *testing.T) {
	fx := automationIdempotenceFixture(t)
	adoptFixture(t, fx)
	m := fx.automation.managed
	delete(fx.automation.rows, "job-1")
	if code, line, _, out := applyOnce(t, fx); line.Reason != syncReasonLost {
		t.Fatalf("exit %d %+v, want refused lost\n%s", code, line, out)
	}
	if out, err := runCLI(t, fx.tree, "sync", "unmanage", "--stack", "prod", "--json"); err != nil {
		t.Fatalf("unmanage: %v\n%s", err, out)
	}
	if id, _ := mustLoadLock(t, fx.root).AutomationSeen("prod", "nightly.json"); id != "" {
		t.Fatalf("the lost row's id %s is still in the lock", id)
	}
	adoptFixture(t, fx)
	out, err := runCLI(t, fx.tree, "sync", "apply", "--stack", "prod", "--allow-create", "--json")
	if err != nil {
		t.Fatalf("apply --allow-create: %v\n%s", err, out)
	}
	if len(fx.automation.created) != 1 || len(m.Claims("collection-1")) != 1 {
		t.Fatalf("created = %d, claims = %+v", len(fx.automation.created), m.Claims("collection-1"))
	}
}

// TestSyncUnmanageMarksTheStackBeforeReleasing (M-a): a release that dies part
// way must not leave a folder whose next apply claims everything again. The
// stack's "unmanaged" mark is saved before the first claim is released, so the
// folder runs from the lock written before it; a second unmanage finishes.
func TestSyncUnmanageMarksTheStackBeforeReleasing(t *testing.T) {
	fx := automationIdempotenceFixture(t)
	adoptFixture(t, fx)
	m := fx.automation.managed
	m.mu.Lock()
	m.failUnmanageOnce = http.StatusServiceUnavailable
	m.mu.Unlock()
	if out, err := runCLI(t, fx.tree, "sync", "unmanage", "--stack", "prod", "--json"); err == nil {
		t.Fatalf("a release that failed reported success:\n%s", out)
	}
	if len(m.Claims("collection-1")) == 0 {
		t.Fatal("the injected failure released nothing to fail on")
	}
	manifest := loadAnyManifest(t, fx.root)
	if !manifest.Adopted() || !manifest.Stacks["prod"].Unmanaged {
		t.Fatalf("the release died before the stack was marked unmanaged: id %q, stack %+v", manifest.ID, manifest.Stacks["prod"])
	}
	// The interrupted stack runs from the lock and claims nothing back.
	fx.edit(t)
	before := len(m.writesSeen())
	code, line, _, out := applyOnce(t, fx)
	if code != syncExitClean || !fx.deployed() {
		t.Fatalf("apply after an interrupted unmanage: exit %d %+v\n%s", code, line, out)
	}
	if writes := m.writesSeen(); len(writes) != before {
		t.Fatalf("a stack marked unmanaged wrote server state: %v", writes[before:])
	}
	// A second unmanage finishes the release and removes the id and the mark.
	if out, err := runCLI(t, fx.tree, "sync", "unmanage", "--stack", "prod", "--json"); err != nil {
		t.Fatalf("second unmanage: %v\n%s", err, out)
	}
	if manifest := loadAnyManifest(t, fx.root); manifest.Adopted() || manifest.Stacks["prod"].Unmanaged {
		t.Fatalf("after the second unmanage: id %q, stack %+v", manifest.ID, manifest.Stacks["prod"])
	}
	if len(m.Claims("collection-1")) != 0 {
		t.Fatalf("claims left: %+v", m.Claims("collection-1"))
	}
}

// TestSyncUnmanageRefusesARowItCannotRecord (M-d): a claim on a row this
// credential may not read names no id, so unmanage cannot hand the row to the
// lock, and a classic push afterwards would create a second one. It refuses,
// naming the address and its claimant and releasing nothing; --force releases
// it and says what that costs.
func TestSyncUnmanageRefusesARowItCannotRecord(t *testing.T) {
	fx := workflowIdempotenceFixture(t)
	adoptFixture(t, fx)
	m := fx.wf.managed
	m.mu.Lock()
	m.unreadable["wf-1"] = true
	m.mu.Unlock()
	removeLock(t, fx.root)
	before := len(m.writesSeen())
	out, err := runCLI(t, fx.tree, "sync", "unmanage", "--stack", "prod", "--json")
	if err == nil {
		t.Fatalf("unmanage released a row it cannot record:\n%s", out)
	}
	var report syncManagedReport
	if jerr := json.Unmarshal([]byte(out), &report); jerr != nil || len(report.Folders) != 1 {
		t.Fatalf("report: %v\n%s", jerr, out)
	}
	line := report.Folders[0]
	if line.Reason != syncReasonNotReadable || !strings.Contains(line.Detail, fakePrincipal) || !strings.Contains(line.Detail, "second row") || !strings.Contains(line.Detail, "--force") {
		t.Fatalf("refusal = %+v", line)
	}
	if got := m.writesSeen(); len(got) != before {
		t.Fatalf("a refused unmanage wrote: %v", got[before:])
	}
	if manifest := loadAnyManifest(t, fx.root); !manifest.Adopted() || manifest.Stacks["prod"].Unmanaged {
		t.Fatalf("a refused unmanage changed ronja.json: %+v", manifest)
	}
	out, err = runCLI(t, fx.tree, "sync", "unmanage", "--stack", "prod", "--force", "--json")
	if err != nil {
		t.Fatalf("unmanage --force: %v\n%s", err, out)
	}
	if len(m.Claims("feat-1")) != 0 || loadAnyManifest(t, fx.root).Adopted() {
		t.Fatalf("--force did not release: claims %+v", m.Claims("feat-1"))
	}
	if !strings.Contains(out, "second row") {
		t.Fatalf("--force did not say what it costs:\n%s", out)
	}
}

// TestManagedTombstoneWhoseRowCameBack (M-e): a tombstone whose row is back in
// the claim's feature (the move back raced the move out, so the claim was not
// revived) cannot be fixed by moving the row back — it is already there. The
// refusal says to unmanage the address, and unmanage keeps the row in the lock
// so adopt claims it again.
func TestManagedTombstoneWhoseRowCameBack(t *testing.T) {
	fx := workflowIdempotenceFixture(t)
	adoptFixture(t, fx)
	m := fx.wf.managed
	c := m.ClaimAt("feat-1", folderIDOf(t, fx.root), "")
	m.mu.Lock()
	at := time.Date(2026, 10, 2, 8, 0, 0, 0, time.UTC)
	c.DepartedAt, c.DepartedTo = &at, "feat-elsewhere"
	m.mu.Unlock()
	removeLock(t, fx.root)
	code, line, _, out := applyOnce(t, fx)
	if code != syncExitDrifted || line.Reason != syncReasonMovedOut {
		t.Fatalf("exit %d %+v, want refused moved_out\n%s", code, line, out)
	}
	if !strings.Contains(line.Detail, "Unmanage this address") || strings.Contains(line.Detail, "Move the row back") {
		t.Fatalf("the refusal sends the reader to move back a row that is already here:\n%s", line.Detail)
	}
	if out, err := runCLI(t, fx.tree, "sync", "unmanage", "--stack", "prod", "--json"); err != nil {
		t.Fatalf("unmanage: %v\n%s", err, out)
	}
	if got := mustLoadLock(t, fx.root).Binding("prod").WorkflowID; got != "wf-1" {
		t.Fatalf("unmanage dropped the row that is back in the feature: lock records %q", got)
	}
	adoptFixture(t, fx)
	if c := m.ClaimAt("feat-1", folderIDOf(t, fx.root), ""); c == nil || c.ResourceID != "wf-1" || c.departed() {
		t.Fatalf("adopt did not claim the row again: %+v", c)
	}
	if code, line, _, out := applyOnce(t, fx); code != syncExitClean || len(fx.wf.created) != 0 {
		t.Fatalf("apply after re-adopting: exit %d %+v, created %d\n%s", code, line, len(fx.wf.created), out)
	}
}

// TestManagedTombstoneNamesWhereItsRowWent (M-f): a tombstone whose row is gone
// reads like a lost claim (unreadable, id kept), so the refusal says the row or
// its feature no longer exists rather than "a feature you may not read", and
// offers no move back; a row merely unreadable says it cannot tell the two
// apart.
func TestManagedTombstoneNamesWhereItsRowWent(t *testing.T) {
	for _, tc := range []struct {
		name, want, not string
		hide            func(m *fakeManaged)
	}{
		{"its row is gone", "no longer exists there", "Move the row back", func(m *fakeManaged) { m.gone["job-1"] = true }},
		{"its row is unreadable", "one you may not read, or one that no longer exists", "", func(m *fakeManaged) { m.unreadable["job-1"] = true }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			fx := automationIdempotenceFixture(t)
			adoptFixture(t, fx)
			m := fx.automation.managed
			m.moveRow("automation", "job-1", "collection-2")
			m.mu.Lock()
			tc.hide(m)
			m.mu.Unlock()
			removeLock(t, fx.root)
			code, line, _, out := applyOnce(t, fx)
			if code != syncExitDrifted || line.Reason != syncReasonMovedOut {
				t.Fatalf("exit %d %+v, want refused moved_out\n%s", code, line, out)
			}
			if !strings.Contains(line.Detail, tc.want) || (tc.not != "" && strings.Contains(line.Detail, tc.not)) {
				t.Fatalf("detail = %s", line.Detail)
			}
		})
	}
}

func mustLoadLock(t *testing.T, root string) *wfdir.Lock {
	t.Helper()
	l, err := wfdir.LoadLock(root)
	if err != nil {
		t.Fatal(err)
	}
	return l
}

// TestManagedRefusesAForeignRepositorysClaim: a fork or template copy
// that kept the folder ids and targets the SAME feature finds the claims made
// by the original repository — refused, unless --adopt-foreign, which takes
// the addresses over (relabels them) and deploys.
func TestManagedRefusesAForeignRepositorysClaim(t *testing.T) {
	fx := workflowIdempotenceFixture(t)
	withSourceLabel(t, "github.com/acme/data")
	adoptFixture(t, fx)
	withSourceLabel(t, "github.com/fork/data")
	code, line, writes, out := applyOnce(t, fx)
	if code != syncExitDrifted || line.Reason != syncReasonForeignClaim {
		t.Fatalf("exit %d %+v, want refused %s\n%s", code, line, syncReasonForeignClaim, out)
	}
	if !strings.Contains(line.Detail, "github.com/acme/data") || len(writes) != 0 {
		t.Fatalf("detail %q, writes %v", line.Detail, writes)
	}

	fx.resetRequests()
	out, err := runCLI(t, fx.tree, "sync", "apply", "--stack", "prod", "--adopt-foreign", "--json")
	if err != nil {
		t.Fatalf("apply --adopt-foreign: %v\n%s", err, out)
	}
	c := managedFakeOf(fx).ClaimAt("feat-1", folderIDOf(t, fx.root), "")
	if !strings.HasPrefix(c.SourceLabel, "github.com/fork/data") {
		t.Fatalf("the claim was not relabelled: %q", c.SourceLabel)
	}

	// A folder MOVED inside the same repository is not foreign: only the remote
	// half of the label decides.
	previous := sourceLabelOf
	sourceLabelOf = func(string) string { return "github.com/fork/data//somewhere/else" }
	t.Cleanup(func() { sourceLabelOf = previous })
	if code, line, _, out := applyOnce(t, fx); code != syncExitClean {
		t.Fatalf("a moved folder was refused: exit %d %+v\n%s", code, line, out)
	}
}

// TestSyncWalkRefusesTwoFoldersSharingAnID: a copied folder keeps its
// id, and one id is one set of addresses — both folders are refused, each
// naming the other.
func TestSyncWalkRefusesTwoFoldersSharingAnID(t *testing.T) {
	fx := workflowIdempotenceFixture(t)
	adoptFixture(t, fx)
	copyRoot := filepath.Join(fx.tree, "copy")
	for path, body := range snapshotFolder(t, fx.root, true) {
		writeLocal(t, copyRoot, path, body)
	}
	out, err := runCLI(t, fx.tree, "sync", "status", "--stack", "prod", "--json")
	if exitCodeOf(err) != syncExitUnknown {
		t.Fatalf("exit %d, want unknown\n%s", exitCodeOf(err), out)
	}
	var report syncStatusReport
	if err := json.Unmarshal([]byte(out), &report); err != nil {
		t.Fatal(err)
	}
	for _, folder := range report.Folders {
		if folder.Reason != syncReasonDuplicateFolderID {
			t.Fatalf("%s: reason %q, want %s", folder.Path, folder.Reason, syncReasonDuplicateFolderID)
		}
		if !strings.Contains(folder.Detail, "copy") || !strings.Contains(folder.Detail, filepath.Base(fx.root)) {
			t.Errorf("%s does not name both folders: %s", folder.Path, folder.Detail)
		}
	}
	if out, err := runCLI(t, fx.tree, "sync", "apply", "--stack", "prod", "--json"); exitCodeOf(err) != syncExitUnknown {
		t.Fatalf("apply over a shared id: exit %d\n%s", exitCodeOf(err), out)
	}
}

// TestLegacyFolderNeverTouchesManagedState: a folder with no id sends no
// request to /managed and no `managed` key on a create, and its ronja.json is
// byte-identical after an apply that creates.
func TestLegacyFolderNeverTouchesManagedState(t *testing.T) {
	fx := pipelineIdempotenceFixture(t)
	manifestBefore := readFile(t, wfdir.ManifestPath(fx.root))
	writeLocal(t, fx.root, "shipped.sql", "SELECT * FROM raw WHERE shipped")
	out, err := runCLI(t, fx.tree, "sync", "apply", "--stack", "prod", "--allow-create", "--json")
	if err != nil {
		t.Fatalf("apply: %v\n%s", err, out)
	}
	if got := managedRequestsIn(fx.requests()); len(got) != 0 {
		t.Fatalf("a legacy folder talked to /managed: %v", got)
	}
	if len(fx.pipeline.created) != 1 || fx.pipeline.created[0].Managed != nil {
		t.Fatalf("a legacy create carried a claim: %+v", fx.pipeline.created)
	}
	raw, _ := json.Marshal(fx.pipeline.created[0])
	if strings.Contains(string(raw), `"managed"`) {
		t.Fatalf("a legacy create body has a managed key: %s", raw)
	}
	if after := readFile(t, wfdir.ManifestPath(fx.root)); after != manifestBefore {
		t.Fatalf("ronja.json changed:\n%s\n->\n%s", manifestBefore, after)
	}
}

// TestManagedSecondRenameFromALockDiscardingCI is the classic folder's lock
// limit (BL-8519), closed for an adopted one: the applied NAME now lives in `applied`, so a CI that never commits
// its lock renames a table twice with no --force — the second rename's
// baseline is the first rename's name, read from the server.
func TestManagedSecondRenameFromALockDiscardingCI(t *testing.T) {
	fx := pipelineIdempotenceFixture(t)
	adoptFixture(t, fx)
	ci := func(t *testing.T, header string) {
		t.Helper()
		writeLocal(t, fx.root, "orders.sql", "-- @name "+header+"\nSELECT * FROM raw")
		committed := snapshotFolder(t, fx.root, true)
		// What the repository holds: the lock as adopt left it, never a lock a
		// CI run wrote.
		committed[wfdir.LockName] = adoptedLock
		restoreCheckout(t, fx.root, committed)
		if code, line, _, out := applyOnce(t, fx); code != syncExitClean {
			t.Fatalf("rename to %q: exit %d %+v\n%s", header, code, line, out)
		}
		if got := fx.pipeline.tables["table-orders"].Name; got != header {
			t.Fatalf("live name = %q, want %q", got, header)
		}
	}
	adoptedLock = readFile(t, wfdir.LockPath(fx.root))
	ci(t, "Sales orders")
	ci(t, "Revenue orders")
}

// adoptedLock is the lock TestManagedSecondRenameFromALockDiscardingCI's
// repository commits.
var adoptedLock string

// TestManagedReadOnlyPathsWriteNothing: status, sync status, sync check and a
// dry-run of an adopted folder whose lock disagrees with the server AND has a
// row the server has no claim for refresh in memory and write nothing — no
// request but GETs, and no file.
func TestManagedReadOnlyPathsWriteNothing(t *testing.T) {
	fx := pipelineIdempotenceFixture(t)
	adoptFixture(t, fx)
	m := fx.pipeline.managed
	id := folderIDOf(t, fx.root)
	// The backfill pending: the server forgets the metric's claim.
	if c := m.ClaimAt("collection-1", id, "metrics/aov.json"); c != nil {
		delete(m.claims, c.ID)
	}
	// The lock disagreeing: it records nothing for the table's anchor.
	editLock(t, fx.root, func(l *wfdir.Lock) {
		if err := l.SetManaged("prod", wfdir.PipelineKind, "orders.sql", "table-orders", map[string]any{}); err != nil {
			t.Fatal(err)
		}
		entry := l.Stacks["prod"]
		table := entry.Tables["orders.sql"]
		table.LiveSHA256 = ""
		entry.Tables["orders.sql"] = table
		l.Stacks["prod"] = entry
	})
	before := snapshotFolder(t, fx.root, false)
	m.mu.Lock()
	m.requests = nil
	m.mu.Unlock()
	runs := [][]string{
		{"sync", "status", "--stack", "prod", "--json"},
		{"sync", "check", "--stack", "prod", "--json"},
		{"sync", "apply", "--stack", "prod", "--dry-run", "--json"},
	}
	for _, args := range runs {
		_, _ = runCLI(t, fx.tree, args...)
		if writes := m.writesSeen(); len(writes) != 0 {
			t.Fatalf("%v wrote server state: %v", args, writes)
		}
	}
	_, _ = runCLI(t, fx.root, "pipeline", "status", "--stack", "prod", "--json")
	if writes := m.writesSeen(); len(writes) != 0 {
		t.Fatalf("pipeline status wrote server state: %v", writes)
	}
	if after := snapshotFolder(t, fx.root, false); !maps.Equal(before, after) {
		t.Fatalf("a read-only path rewrote the folder")
	}
}

// TestManagedTwoStacksKeepTheirOwnClaims: one folder deployed to two stacks —
// two features — holds two independent sets of claims under one folder id.
// Unmanaging one stack releases only its feature's, and the id stays while the
// other stack still holds claims under it.
func TestManagedTwoStacksKeepTheirOwnClaims(t *testing.T) {
	fx := workflowIdempotenceFixture(t)
	f := fx.wf
	f.AddWorkflow(&api.Workflow{ID: "wf-dev", Lifecycle: api.LifecycleLive, FeatureID: "feat-dev", Title: "Monthly report",
		RuntimeVersion: wfdir.RuntimeDefault},
		api.WorkflowFile{Path: "main.py", Content: "live\n"}, api.WorkflowFile{Path: "helper.py", Content: "def main():\n    pass\n"})
	manifest := loadAnyManifest(t, fx.root)
	prod := manifest.Stacks["prod"]
	manifest.Stacks["dev"] = wfdir.Stack{URL: prod.URL, TenantID: prod.TenantID, FeatureID: "feat-dev"}
	if err := wfdir.SaveManifest(fx.root, manifest); err != nil {
		t.Fatal(err)
	}
	editLock(t, fx.root, func(l *wfdir.Lock) {
		if err := l.SetManaged("dev", wfdir.WorkflowKind, "", "wf-dev", map[string]any{"headVersionID": "wf-dev"}); err != nil {
			t.Fatal(err)
		}
	})
	for _, stack := range []string{"prod", "dev"} {
		if out, err := runCLI(t, fx.tree, "sync", "adopt", "--stack", stack, "--json"); err != nil {
			t.Fatalf("adopt %s: %v\n%s", stack, err, out)
		}
	}
	id := folderIDOf(t, fx.root)
	if c := f.managed.ClaimAt("feat-1", id, ""); c == nil || c.ResourceID != "wf-1" {
		t.Fatalf("prod's claim: %+v", c)
	}
	if c := f.managed.ClaimAt("feat-dev", id, ""); c == nil || c.ResourceID != "wf-dev" {
		t.Fatalf("dev's claim: %+v", c)
	}

	// With no lock, each stack still finds its own row.
	removeLock(t, fx.root)
	for _, stack := range []string{"prod", "dev"} {
		out, err := runCLI(t, fx.tree, "sync", "apply", "--stack", stack, "--json")
		if err != nil {
			t.Fatalf("apply %s with no lock: %v\n%s", stack, err, out)
		}
	}
	if lock := mustLoadLock(t, fx.root); lock.Binding("prod").WorkflowID != "wf-1" || lock.Binding("dev").WorkflowID != "wf-dev" {
		t.Fatalf("lock bindings = prod %q, dev %q", lock.Binding("prod").WorkflowID, lock.Binding("dev").WorkflowID)
	}

	out, err := runCLI(t, fx.tree, "sync", "unmanage", "--stack", "dev", "--json")
	if err != nil {
		t.Fatalf("unmanage dev: %v\n%s", err, out)
	}
	if c := f.managed.ClaimAt("feat-dev", id, ""); c != nil {
		t.Fatalf("dev's claim survived: %+v", c)
	}
	if c := f.managed.ClaimAt("feat-1", id, ""); c == nil {
		t.Fatal("unmanaging dev released prod's claim")
	}
	if m := loadAnyManifest(t, fx.root); !m.Adopted() || !m.Stacks["dev"].Unmanaged {
		t.Fatalf("the id must stay while prod holds a claim, and dev must be marked unmanaged: %+v", m.Stacks["dev"])
	}
	// The release STICKS: dev's next apply runs from the lock and claims
	// nothing back.
	fx.resetRequests()
	if out, err := runCLI(t, fx.tree, "sync", "apply", "--stack", "dev", "--json"); err != nil {
		t.Fatalf("apply dev after unmanage: %v\n%s", err, out)
	}
	if got := managedRequestsIn(fx.requests()); len(got) != 0 {
		t.Fatalf("an unmanaged stack still talked to /managed: %v", got)
	}
	if c := f.managed.ClaimAt("feat-dev", id, ""); c != nil {
		t.Fatalf("dev's claim came back: %+v", c)
	}
	// Adopting dev again clears the mark and claims its row.
	if out, err := runCLI(t, fx.tree, "sync", "adopt", "--stack", "dev", "--json"); err != nil {
		t.Fatalf("re-adopt dev: %v\n%s", err, out)
	}
	if loadAnyManifest(t, fx.root).Stacks["dev"].Unmanaged || f.managed.ClaimAt("feat-dev", id, "") == nil {
		t.Fatal("re-adopting dev did not clear its mark and claim its row")
	}
	if out, err := runCLI(t, fx.tree, "sync", "unmanage", "--stack", "dev", "--json"); err != nil {
		t.Fatalf("unmanage dev again: %v\n%s", err, out)
	}
	if out, err := runCLI(t, fx.tree, "sync", "unmanage", "--stack", "prod", "--json"); err != nil {
		t.Fatalf("unmanage prod: %v\n%s", err, out)
	}
	if m := loadAnyManifest(t, fx.root); m.Adopted() || m.Stacks["dev"].Unmanaged {
		t.Fatalf("the id (or dev's mark) stayed with no claims left under it: %+v", m.Stacks["dev"])
	}
}

// TestSyncUnmanageKeepsTheIDForAnotherStacksFolderAnchor: the folder id stays
// while another stack still has state under it, and a folder anchor alone — a
// docs-only folder's baselines — is such state: removing the id would strand
// it, and that stack's next lock-less run would lose its drift guard.
func TestSyncUnmanageKeepsTheIDForAnotherStacksFolderAnchor(t *testing.T) {
	fx := workflowIdempotenceFixture(t)
	f := fx.wf
	f.AddWorkflow(&api.Workflow{ID: "wf-dev", Lifecycle: api.LifecycleLive, FeatureID: "feat-dev", Title: "Monthly report",
		RuntimeVersion: wfdir.RuntimeDefault},
		api.WorkflowFile{Path: "main.py", Content: "live\n"}, api.WorkflowFile{Path: "helper.py", Content: "def main():\n    pass\n"})
	manifest := loadAnyManifest(t, fx.root)
	prod := manifest.Stacks["prod"]
	manifest.Stacks["dev"] = wfdir.Stack{URL: prod.URL, TenantID: prod.TenantID, FeatureID: "feat-dev"}
	if err := wfdir.SaveManifest(fx.root, manifest); err != nil {
		t.Fatal(err)
	}
	editLock(t, fx.root, func(l *wfdir.Lock) {
		if err := l.SetManaged("dev", wfdir.WorkflowKind, "", "wf-dev", map[string]any{"headVersionID": "wf-dev"}); err != nil {
			t.Fatal(err)
		}
	})
	for _, stack := range []string{"prod", "dev"} {
		if out, err := runCLI(t, fx.tree, "sync", "adopt", "--stack", stack, "--json"); err != nil {
			t.Fatalf("adopt %s: %v\n%s", stack, err, out)
		}
	}
	id := folderIDOf(t, fx.root)
	// prod holds no claim any more — only its folder anchor.
	f.managed.mu.Lock()
	delete(f.managed.claims, f.managed.byAddress("feat-1", id, "").ID)
	f.managed.folders[fakeFolderKey("feat-1", id)] = &fakeFolderAnchor{FeatureID: "feat-1", FolderID: id,
		Applied: map[string]any{"tableDocs": map[string]any{}}, Revision: 2}
	f.managed.mu.Unlock()
	if out, err := runCLI(t, fx.tree, "sync", "unmanage", "--stack", "dev", "--json"); err != nil {
		t.Fatalf("unmanage dev: %v\n%s", err, out)
	}
	if m := loadAnyManifest(t, fx.root); !m.Adopted() || !m.Stacks["dev"].Unmanaged {
		t.Fatalf("the id went while prod still has a folder anchor under it: id %q, dev %+v", m.ID, m.Stacks["dev"])
	}
}

func TestNormalizeGitRemoteFoldsEverySpellingOfOneRepository(t *testing.T) {
	for _, in := range []string{
		"https://github.com/acme/data.git",
		"https://user:token@GitHub.com/acme/data",
		"git@github.com:acme/data.git",
		"ssh://git@github.com:22/acme/data.git",
		"github.com/acme/data/",
	} {
		if got := normalizeGitRemote(in); got != "github.com/acme/data" {
			t.Errorf("%s → %q", in, got)
		}
	}
	// The repository path is case-insensitive on the hosts that matter (CI
	// checks out `Acme/Data`, a laptop cloned `acme/data`), and a host's
	// aliases name the same repository.
	for in, want := range map[string]string{
		"https://github.com/Acme/Data.git":                       "github.com/acme/data",
		"git@ssh.github.com:acme/data.git":                       "github.com/acme/data",
		"ssh://git@ssh.github.com:443/Acme/data.git":             "github.com/acme/data",
		"git@altssh.gitlab.com:acme/data.git":                    "gitlab.com/acme/data",
		"https://acme@dev.azure.com/acme/Sales/_git/data":        "dev.azure.com/acme/sales/data",
		"git@ssh.dev.azure.com:v3/acme/Sales/data":               "dev.azure.com/acme/sales/data",
		"https://acme.visualstudio.com/Sales/_git/data":          "dev.azure.com/acme/sales/data",
		"acme@vs-ssh.visualstudio.com:v3/acme/Sales/data":        "dev.azure.com/acme/sales/data",
		"https://u:secret/with/slashes@github.com/acme/data.git": "github.com/acme/data",
		"https://git.example.com:8443/team/repo.git":             "git.example.com/team/repo",
	} {
		if got := normalizeGitRemote(in); got != want {
			t.Errorf("%s → %q, want %q", in, got, want)
		}
	}
	if foreignClaim("github.com/Acme/Data//pipelines/a", "github.com/acme/data//pipelines/a") {
		t.Error("a label an earlier build recorded with the path's case read as foreign")
	}
	if foreignClaim("github.com/acme/data//pipelines/a", "github.com/acme/data//moved/elsewhere") {
		t.Error("a folder moved inside its repository read as foreign")
	}
	if !foreignClaim("github.com/acme/data//a", "github.com/fork/data//a") {
		t.Error("a fork did not read as foreign")
	}
	if foreignClaim("", "github.com/fork/data//a") || foreignClaim("github.com/acme/data//a", "") {
		t.Error("an unknown source read as foreign")
	}
}

// mergeAnchor writes this build's anchor keys over the claim's and keeps every
// key a NEWER CLI recorded there. The retired docs key is carried until the
// folder anchor holds its content, and cleared after.
func TestMergeAnchorKeepsKeysItDoesNotOwn(t *testing.T) {
	prev := map[string]any{"liveSHA256": "old", "fromTheFuture": "keep", "name": "Old name", "tableDocs": map[string]any{"tables/x.json": map[string]any{"tableID": "t"}}}
	got := mergeAnchor(prev, wfdir.ManagedTable, map[string]any{"liveSHA256": "new"}, false)
	if got["liveSHA256"] != "new" || got["fromTheFuture"] != "keep" {
		t.Fatalf("merged = %v", got)
	}
	if _, kept := got["name"]; kept {
		t.Fatalf("a known key the lock left empty was not cleared: %v", got)
	}
	if got["tableDocs"] == nil {
		t.Fatalf("the retired docs key was dropped before its new home held it: %v", got)
	}
	if got := mergeAnchor(prev, wfdir.ManagedTable, map[string]any{"liveSHA256": "new"}, true); got["tableDocs"] != nil || got["fromTheFuture"] != "keep" {
		t.Fatalf("with the folder anchor recorded, merged = %v", got)
	}
}

// TestManagedRefusesAClaimItsKindCannotHold: a claim under this folder's id
// that this folder's kind cannot hold at its address means two folders share
// the id — refused, never adopted.
func TestManagedRefusesAClaimItsKindCannotHold(t *testing.T) {
	fx := workflowIdempotenceFixture(t)
	adoptFixture(t, fx)
	m := managedFakeOf(fx)
	m.stage("feat-1", folderIDOf(t, fx.root), "orders.sql", "table", "table-x", "")
	code, line, writes, out := applyOnce(t, fx)
	if code != syncExitDrifted || line.Reason != syncReasonClaimMismatch || len(writes) != 0 {
		t.Fatalf("exit %d %+v %v\n%s", code, line, writes, out)
	}
}

// TestAdoptedPipelinePushRefusesRenameDetection: in an adopted folder a
// single-folder push REFUSES a bound file that is gone with no declaration —
// a classic folder's push only notes it.
func TestAdoptedPipelinePushRefusesRenameDetection(t *testing.T) {
	fx := pipelineIdempotenceFixture(t)
	adoptFixture(t, fx)
	if err := os.Remove(filepath.Join(fx.root, "metrics", "aov.json")); err != nil {
		t.Fatal(err)
	}
	_, err := runCLI(t, fx.root, "pipeline", "push", "--stack", "prod")
	if err == nil || !strings.Contains(err.Error(), "metrics/aov.json is bound but gone") {
		t.Fatalf("push = %v, want rename detection refused", err)
	}
}

// TestManagedCreateAdoptsAnOkWinnerOnly: a create's 409 address_claimed adopts
// the holder only when its claim is ok. A holder this credential may not read
// (another person's unpublished draft) is refused not_readable — never bound
// by an id the server withheld.
func TestManagedCreateAdoptsAnOkWinnerOnly(t *testing.T) {
	f := newFakeInstance(t)
	signIn(t, f)
	root := t.TempDir()
	if _, err := runCLI(t, root, "wf", "init", "--feature", "feat-1", "--stack", "dev"); err != nil {
		t.Fatalf("init: %v", err)
	}
	id := folderIDOf(t, root)
	writeLocal(t, root, "main.py", "body\n")
	f.managed.race = func(m *fakeManaged) {
		// A colleague's parentless draft: only its drafter reads it.
		f.AddWorkflow(&api.Workflow{ID: "wf-theirs", Lifecycle: api.LifecycleDraft, DrafterUserID: "usr-2", FeatureID: "feat-1"})
		m.insert("feat-1", "workflow", "wf-theirs", api.ManagedClaimInput{FolderID: id, StackLabel: "dev"})
	}
	_, err := runCLI(t, root, "wf", "push")
	if err == nil || !strings.Contains(err.Error(), "may not read") {
		t.Fatalf("push = %v, want a not_readable refusal", err)
	}
	if got := mustLoadLock(t, root).Binding("dev").WorkflowID; got != "" {
		t.Fatalf("bound to %q", got)
	}
}

// TestManagedFakeModelsTheCallersReach pins the fake's model of WHO is asking
// (backend/api/v2/managed probe.go, handler.go, claims.go) by the outcome
// the CLI reports for each: the draft rule, the table/metric role floor, the
// token scopes, and a reader's module proposal.
func TestManagedFakeModelsTheCallersReach(t *testing.T) {
	t.Run("a colleague's draft is not_readable; a proposal is readable by an admin", func(t *testing.T) {
		fx := workflowIdempotenceFixture(t)
		adoptFixture(t, fx)
		m := managedFakeOf(fx)
		wf := fx.wf.workflows["wf-1"]
		wf.Lifecycle, wf.DrafterUserID = api.LifecycleDraft, "usr-2"
		if code, line, _, out := applyOnce(t, fx); code != syncExitDrifted || line.Reason != syncReasonNotReadable {
			t.Fatalf("a colleague's draft: exit %d %+v\n%s", code, line, out)
		}
		wf.Lifecycle = api.LifecycleProposed
		view := m.view(m.ClaimAt("feat-1", folderIDOf(t, fx.root), ""))
		if view.State != api.ManagedStateOK {
			t.Fatalf("an admin reads a proposal: state %s", view.State)
		}
		m.admin = false
		if view := m.view(m.ClaimAt("feat-1", folderIDOf(t, fx.root), "")); view.State != api.ManagedStateNotReadable {
			t.Fatalf("a non-admin reads a colleague's proposal: state %s", view.State)
		}
	})
	t.Run("a table claim needs an admin", func(t *testing.T) {
		fx := pipelineIdempotenceFixture(t)
		fx.pipeline.managed.admin = false
		out, err := runCLI(t, fx.tree, "sync", "adopt", "--stack", "prod", "--json")
		if exitCodeOf(err) != syncExitDrifted || !strings.Contains(out, `"code": "forbidden"`) {
			t.Fatalf("a non-admin adopt of tables: exit %d\n%s", exitCodeOf(err), out)
		}
		if got := fx.pipeline.managed.Claims("collection-1"); len(got) != 0 {
			t.Fatalf("a non-admin claimed %d rows", len(got))
		}
	})
	t.Run("structure:read is needed to read state", func(t *testing.T) {
		fx := workflowIdempotenceFixture(t)
		adoptFixture(t, fx)
		managedFakeOf(fx).denied["structure:read"] = true
		code, line, writes, out := applyOnce(t, fx)
		if code != syncExitUnknown || !strings.Contains(line.Detail, "structure:read") || len(writes) != 0 {
			t.Fatalf("no structure:read: exit %d %+v %v\n%s", code, line, writes, out)
		}
	})
	t.Run("a row kind's read scope gates the row half", func(t *testing.T) {
		fx := pipelineIdempotenceFixture(t)
		adoptFixture(t, fx)
		fx.pipeline.managed.denied["data:read"] = true
		if code, line, _, out := applyOnce(t, fx); code != syncExitDrifted || line.Reason != syncReasonNotReadable {
			t.Fatalf("no data:read: exit %d %+v\n%s", code, line, out)
		}
	})
	t.Run("a managed create needs structure:write", func(t *testing.T) {
		f := newFakeInstance(t)
		signIn(t, f)
		root := t.TempDir()
		if _, err := runCLI(t, root, "wf", "init", "--feature", "feat-1", "--stack", "dev"); err != nil {
			t.Fatalf("init: %v", err)
		}
		writeLocal(t, root, "main.py", "body\n")
		f.managed.denied["structure:write"] = true
		if _, err := runCLI(t, root, "wf", "push"); err == nil || !strings.Contains(err.Error(), "structure") {
			t.Fatalf("push = %v, want the structure scope named", err)
		}
		for id := range f.workflows {
			if strings.HasPrefix(id, "wf-new-") {
				t.Fatalf("the refused create was not rolled back: %s", id)
			}
		}
	})
	t.Run("a move that loses to a concurrent move refuses", func(t *testing.T) {
		fx := pipelineIdempotenceFixture(t)
		adoptFixture(t, fx)
		id := folderIDOf(t, fx.root)
		for _, rename := range [][2]string{{"orders.sql", "sales_orders.sql"}, {"checks/orders.json", "checks/sales_orders.json"}} {
			if err := os.Rename(filepath.Join(fx.root, rename[0]), filepath.Join(fx.root, rename[1])); err != nil {
				t.Fatal(err)
			}
		}
		manifest := loadAnyManifest(t, fx.root)
		manifest.Moved = []wfdir.MovedEntry{{From: "orders.sql", To: "sales_orders.sql"}}
		if err := wfdir.SaveManifest(fx.root, manifest); err != nil {
			t.Fatal(err)
		}
		fx.pipeline.managed.moveConflictOnce = true
		code, line, _, out := applyOnce(t, fx)
		if code == syncExitClean || !strings.Contains(line.Detail, "moved while this request ran") {
			t.Fatalf("a lost move race: exit %d %+v\n%s", code, line, out)
		}
		if c := fx.pipeline.managed.ClaimAt("collection-1", id, "orders.sql"); c == nil {
			t.Fatal("the claim left its address on a refused move")
		}
		if fx.pipeline.CodeOf("table-orders") != "SELECT * FROM raw" {
			t.Fatal("a refused move deployed the table")
		}
	})
}

// adoptedModuleFolder is a module folder on stack prod with a folder id,
// written by hand rather than by `module init --stack` (which mints one too —
// TestModuleInitOnAStackMintsAFolderID), so the stack can be shaped here.
func adoptedModuleFolder(t *testing.T, f *fakeInstance) string {
	t.Helper()
	root := t.TempDir()
	if _, err := runCLI(t, root, "module", "init", "emab_tracker", "--feature", "collection-1", "--title", "EMAB tracker"); err != nil {
		t.Fatalf("module init: %v", err)
	}
	m, err := wfdir.LoadManifest(root, wfdir.ModuleKind)
	if err != nil {
		t.Fatal(err)
	}
	m.Instances = nil
	m.Stacks = map[string]wfdir.Stack{"prod": {URL: f.URL(), TenantID: testTenantID, FeatureID: "collection-1"}}
	m.ID = "0b6f7c2e-3a41-4f0e-9d4c-7a5e2b9c1d08"
	if err := wfdir.SaveManifest(root, m); err != nil {
		t.Fatal(err)
	}
	if err := wfdir.WriteFile(root, "tracker.py", "x = 1\n"); err != nil {
		t.Fatal(err)
	}
	return root
}

// TestManagedModuleCreateClaims: a module's create carries `managed` like
// every other kind's, and the claim binds the created row — a proposal, in a
// shared feature, since the drafter reads their own.
func TestManagedModuleCreateClaims(t *testing.T) {
	f := newFakeInstance(t)
	signInAsModuleAdmin(t, f)
	f.mod.sharedFeature = true
	root := adoptedModuleFolder(t, f)
	if out, err := runCLI(t, root, "module", "push", "--stack", "prod", "--json"); err != nil {
		t.Fatalf("push: %v\n%s", err, out)
	}
	if len(f.mod.created) != 1 || f.mod.created[0].Managed == nil || f.mod.created[0].Managed.Address != "" {
		t.Fatalf("created = %+v", f.mod.created)
	}
	c := f.managed.ClaimAt("collection-1", "0b6f7c2e-3a41-4f0e-9d4c-7a5e2b9c1d08", "")
	if c == nil || c.Kind != "module" || f.mod.rows[c.ResourceID] == nil {
		t.Fatalf("claim = %+v", c)
	}
	if v := f.managed.view(c); v.State != api.ManagedStateOK {
		t.Fatalf("the drafter's own proposal reads %s", v.State)
	}
}

// TestManagedReadersModuleProposalIsRefused is ClaimCreatedTx's FeatureWrite
// (claim_tx.go:62): a mere reader of an organization feature may file a module
// proposal, but not one that claims an address there — the whole create rolls
// back.
func TestManagedReadersModuleProposalIsRefused(t *testing.T) {
	f := newFakeInstance(t)
	signInAsModuleUser(t, f)
	f.mod.sharedFeature = true
	f.managed.readOnlyFeatures["collection-1"] = true
	root := adoptedModuleFolder(t, f)
	if out, err := runCLI(t, root, "module", "push", "--stack", "prod", "--json"); err == nil {
		t.Fatalf("a reader's managed module proposal was accepted\n%s", out)
	}
	if len(f.mod.rows) != 0 || len(f.managed.Claims("")) != 0 {
		t.Fatalf("rows %d, claims %+v — the refused create was not rolled back", len(f.mod.rows), f.managed.Claims(""))
	}
}

// TestSyncAdoptWritesNothingTheServerCannotHold: adopt reads the server before
// it writes the folder id. Against an instance without /managed, with a token
// that may not read it, or for a stack with no featureID, it used to mint the
// id and stamp format 6 first — leaving a working classic folder adopted and
// refusing every later deploy until `sync unmanage`. Now ronja.json is left
// byte for byte as it was.
func TestSyncAdoptWritesNothingTheServerCannotHold(t *testing.T) {
	cases := map[string]func(t *testing.T, fx idempotenceFixture){
		"an instance without /managed": func(t *testing.T, fx idempotenceFixture) { managedFakeOf(fx).noRoute = true },
		"a stack with no featureID": func(t *testing.T, fx idempotenceFixture) {
			m := loadAnyManifest(t, fx.root)
			st := m.Stacks["prod"]
			st.FeatureID = ""
			m.Stacks["prod"] = st
			if err := wfdir.SaveManifest(fx.root, m); err != nil {
				t.Fatal(err)
			}
		},
	}
	for name, stage := range cases {
		t.Run(name, func(t *testing.T) {
			fx := workflowIdempotenceFixture(t)
			stage(t, fx)
			before := readFile(t, wfdir.ManifestPath(fx.root))
			out, err := runCLI(t, fx.tree, "sync", "adopt", "--stack", "prod", "--json")
			if err == nil {
				t.Fatalf("adopt reported success:\n%s", out)
			}
			if got := readFile(t, wfdir.ManifestPath(fx.root)); got != before {
				t.Fatalf("a refused adopt rewrote ronja.json:\n%s", got)
			}
		})
	}
}

// TestSyncUnmanageLowersTheFormatTheIDRaised: format 6 exists only for the
// folder id, so removing the id returns the file to the version its content
// needs — a folder with no dependencies to its stacks' 2 — and a folder whose
// dependencies need 7 keeps 7.
func TestSyncUnmanageLowersTheFormatTheIDRaised(t *testing.T) {
	fx := workflowIdempotenceFixture(t)
	adoptFixture(t, fx)
	if v := loadAnyManifest(t, fx.root).FormatVersion; v != 6 {
		t.Fatalf("adopt stamped %d", v)
	}
	if out, err := runCLI(t, fx.tree, "sync", "unmanage", "--stack", "prod", "--json"); err != nil {
		t.Fatalf("unmanage: %v\n%s", err, out)
	}
	if v := loadAnyManifest(t, fx.root).FormatVersion; v != 2 {
		t.Fatalf("formatVersion after unmanage = %d, want 2 — the id was its only cause", v)
	}

	m := &wfdir.Manifest{ID: "0b6a8f1e-0000-4000-8000-000000000001", FormatVersion: 7,
		Dependencies: map[string]wfdir.Dependency{"ledger": {Kind: "table", Write: true}}}
	m.RemoveFolderID()
	if m.ID != "" || m.StampedFormatVersion() != 7 {
		t.Fatalf("a v7 folder's dependencies lost their version: id %q, version %d", m.ID, m.StampedFormatVersion())
	}

	// Lowered from the id's version to what the content still needs, never
	// below it.
	for want, m := range map[int]*wfdir.Manifest{
		5: {ID: "0b6a8f1e-0000-4000-8000-000000000001", FormatVersion: 6, Kind: wfdir.KindPipeline,
			Moved: []wfdir.MovedEntry{{From: "a.sql", To: "b.sql"}}},
		4: {ID: "0b6a8f1e-0000-4000-8000-000000000001", FormatVersion: 6, Kind: wfdir.KindWorkflow,
			Language: wfdir.LanguageJS},
	} {
		m.RemoveFolderID()
		if got := m.StampedFormatVersion(); got != want {
			t.Errorf("unmanaged %+v stamps %d, want %d", m, got, want)
		}
	}
}

// TestAdoptForeignTakesOverAClaimMovedInTheSameRun: --adopt-foreign relabels
// every foreign claim, and one a `moved` re-keys in the same run is no
// exception — relabelled by the claim itself, before the move, rather than
// looked up at the address the move just left (which found nothing, so the
// claim kept the other repository's label and the next run refused again).
func TestAdoptForeignTakesOverAClaimMovedInTheSameRun(t *testing.T) {
	fx := pipelineIdempotenceFixture(t)
	withSourceLabel(t, "github.com/acme/data")
	adoptFixture(t, fx)
	withSourceLabel(t, "github.com/fork/data")
	for _, rename := range [][2]string{{"orders.sql", "sales_orders.sql"}, {"checks/orders.json", "checks/sales_orders.json"}} {
		if err := os.Rename(filepath.Join(fx.root, rename[0]), filepath.Join(fx.root, rename[1])); err != nil {
			t.Fatal(err)
		}
	}
	manifest := loadAnyManifest(t, fx.root)
	manifest.Moved = []wfdir.MovedEntry{{From: "orders.sql", To: "sales_orders.sql"}}
	if err := wfdir.SaveManifest(fx.root, manifest); err != nil {
		t.Fatal(err)
	}
	if out, err := runCLI(t, fx.tree, "sync", "apply", "--stack", "prod", "--adopt-foreign", "--json"); err != nil {
		t.Fatalf("apply --adopt-foreign: %v\n%s", err, out)
	}
	c := fx.pipeline.managed.ClaimAt("collection-1", folderIDOf(t, fx.root), "sales_orders.sql")
	if c == nil || !strings.HasPrefix(c.SourceLabel, "github.com/fork/data") {
		t.Fatalf("the moved claim kept the other repository's label: %+v", c)
	}
	if code, line, _, out := applyOnce(t, fx); code != syncExitClean {
		t.Fatalf("the next apply without the flag: exit %d %+v\n%s", code, line, out)
	}
}

// TestAStaleLockEntryForAGoneFileIsNotClaimedAgain: a `removed` released the
// metric's claim; the entry was then taken out while a committed lock — the one
// a CI checkout restores — still records the metric. The lazy backfill used to
// claim it again on the server before rename detection refused the folder: a
// server write for a deploy that does not happen. A lock entry whose file is
// gone is not backfilled.
func TestAStaleLockEntryForAGoneFileIsNotClaimedAgain(t *testing.T) {
	fx := pipelineIdempotenceFixture(t)
	adoptFixture(t, fx)
	if code, line, _, out := applyOnce(t, fx); code != syncExitClean {
		t.Fatalf("first apply: exit %d %+v\n%s", code, line, out)
	}
	staleLock := readFile(t, wfdir.LockPath(fx.root))
	if err := os.Remove(filepath.Join(fx.root, "metrics", "aov.json")); err != nil {
		t.Fatal(err)
	}
	m := loadAnyManifest(t, fx.root)
	m.Removed = []string{"metrics/aov.json"}
	if err := wfdir.SaveManifest(fx.root, m); err != nil {
		t.Fatal(err)
	}
	if code, line, _, out := applyOnce(t, fx); code != syncExitClean {
		t.Fatalf("apply with removed: exit %d %+v\n%s", code, line, out)
	}
	id := folderIDOf(t, fx.root)
	if c := fx.pipeline.managed.ClaimAt("collection-1", id, "metrics/aov.json"); c != nil {
		t.Fatalf("removed did not release the claim: %+v", c)
	}
	m = loadAnyManifest(t, fx.root)
	m.Removed = nil
	if err := wfdir.SaveManifest(fx.root, m); err != nil {
		t.Fatal(err)
	}
	for _, args := range [][]string{{"sync", "apply", "--stack", "prod"}, {"pipeline", "push"}} {
		writeLocal(t, fx.root, wfdir.LockName, staleLock)
		dir := fx.tree
		if args[0] == "pipeline" {
			dir = fx.root
		}
		_, _, _ = runPipelineCLI(t, dir, args...)
		if c := fx.pipeline.managed.ClaimAt("collection-1", id, "metrics/aov.json"); c != nil {
			t.Fatalf("%v: a stale lock entry for a gone file was claimed again: %+v", args, c)
		}
	}
}
