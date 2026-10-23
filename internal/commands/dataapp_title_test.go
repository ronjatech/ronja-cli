package commands

import (
	"strings"
	"testing"

	"github.com/ronjatech/ronja-cli/internal/api"
	"github.com/ronjatech/ronja-cli/internal/wfdir"
)

// A data app's TITLE: patched only through the name rule. It used to be patched
// onto the draft whenever it differed from ronja.json, with no drift check —
// so a rename made in the web app was reverted by the next push, silently.

func titlePatches(f *fakeAppInstance) []string {
	var out []string
	for _, p := range f.patches {
		if p.Name != nil {
			out = append(out, *p.Name)
		}
	}
	return out
}

func cloneTitledApp(t *testing.T, f *fakeAppInstance) string {
	t.Helper()
	f.AddApp(&api.DataApp{ID: "app-1", Lifecycle: api.LifecycleLive},
		api.DataAppFile{Path: "App.tsx", Content: "live\n"})
	f.versions["app-1"] = []api.DataApp{{ID: "dav-1"}}
	signInApp(t, f)
	out, err := runCLI(t, t.TempDir(), "app", "clone", "app-1", "--stack", "dev", "--json")
	if err != nil {
		t.Fatalf("clone: %v", err)
	}
	return decodeJSON(t, out)["root"].(string)
}

func setAppTitle(t *testing.T, root, title string) {
	t.Helper()
	m := appManifestOf(t, root)
	m.Title = title
	if err := wfdir.SaveManifest(root, m); err != nil {
		t.Fatal(err)
	}
}

func appliedTitle(t *testing.T, root string) string {
	t.Helper()
	lock, err := wfdir.LoadLock(root)
	if err != nil {
		t.Fatal(err)
	}
	return lock.AppliedTitle("dev")
}

// A WEB-APP RENAME IS REFUSED, NOT REVERTED — and --force is the app loop's
// existing override.
func TestAppPushRefusesATitleRenamedElsewhere(t *testing.T) {
	f := newFakeAppInstance(t)
	root := cloneTitledApp(t, f)
	f.apps["app-1"].Name = "Renamed in the web app"
	writeLocal(t, root, "App.tsx", "mine\n")

	_, err := runCLI(t, root, "app", "push", "--no-validate")
	if err == nil {
		t.Fatal("a push over a title renamed elsewhere went ahead")
	}
	if !strings.Contains(err.Error(), "Renamed in the web app") {
		t.Errorf("the refusal does not name the live title: %v", err)
	}
	if got := titlePatches(f); len(got) != 0 {
		t.Fatalf("the title was patched anyway: %v", got)
	}
	if got := f.FileContents("app-1")["App.tsx"]; got != "live\n" {
		t.Errorf("a refused push wrote files: %q", got)
	}

	if _, err := runCLI(t, root, "app", "push", "--no-validate", "--force"); err != nil {
		t.Fatalf("push --force: %v", err)
	}
	if got := titlePatches(f); len(got) != 1 || got[0] != "Revenue explorer" {
		t.Errorf("--force patched %v, want the declared title once", got)
	}
}

// THE FOLDER'S OWN RENAME: the title in ronja.json changes, the baseline still
// matches the live app, so the draft is renamed and publish lands it — and the
// applied title is recorded in the lock.
func TestAppPushRenamesFromTheManifestTitle(t *testing.T) {
	f := newFakeAppInstance(t)
	root := cloneTitledApp(t, f)
	setAppTitle(t, root, "Revenue cockpit")

	out, err := runCLI(t, root, "app", "push", "--no-validate", "--json")
	if err != nil {
		t.Fatalf("push: %v\n%s", err, out)
	}
	var result appPushResult
	decodeJSONInto(t, out, &result)
	if result.Rename == nil || result.Rename.To != "Revenue cockpit" {
		t.Fatalf("rename = %+v", result.Rename)
	}
	if result.UpToDate {
		t.Error("a push that renamed the draft reported up to date")
	}
	if _, err := runCLI(t, root, "app", "publish"); err != nil {
		t.Fatalf("publish: %v", err)
	}
	if got := f.apps["app-1"].Name; got != "Revenue cockpit" {
		t.Errorf("live title = %q", got)
	}
	if got := appliedTitle(t, root); got != "Revenue cockpit" {
		t.Errorf("applied title = %q", got)
	}
}

// NO RECORDED TITLE, BUT THE HEAD VOUCHES — a fresh checkout of a lock with no
// applied title, whose anchor is still the live app's head. A data app's live
// title moves only through a commit, which moves the head, so the live title is
// this folder's own and the declared one is a rename to send.
func TestAppPushRenamesInAFreshCheckoutWhenTheHeadVouches(t *testing.T) {
	f := newFakeAppInstance(t)
	root := cloneTitledApp(t, f)
	setAppTitle(t, root, "Revenue cockpit")
	freshCheckout(t, root)

	if out, err := runCLI(t, root, "app", "push", "--no-validate"); err != nil {
		t.Fatalf("push: %v\n%s", err, out)
	}
	if got := titlePatches(f); len(got) != 1 || got[0] != "Revenue cockpit" {
		t.Fatalf("patched %v, want the declared title once", got)
	}
	if got := appliedTitle(t, root); got != "Revenue explorer" {
		t.Errorf("applied title = %q, want the vouched live title recorded", got)
	}
}

// A WEB RENAME WITH NOTHING RECORDED: the rename was committed, so the head
// moved and cannot vouch — refused, on every push rather than recorded and
// reverted one push later; --force renames.
func TestAppPushRefusesAnUnrecordedWebRenameOnEveryPush(t *testing.T) {
	f := newFakeAppInstance(t)
	root := cloneTitledApp(t, f)
	// What a web rename of a live app IS on the server: an auto-forked draft,
	// committed — a new version on top.
	f.apps["app-1"].Name = "Renamed in the web app"
	f.versions["app-1"] = append([]api.DataApp{{ID: "dav-2"}}, f.versions["app-1"]...)
	freshCheckout(t, root)

	for push := 1; push <= 2; push++ {
		if _, err := runCLI(t, root, "app", "push", "--no-validate"); err == nil {
			t.Fatalf("push %d over an unrecorded web rename went ahead", push)
		}
		if got := titlePatches(f); len(got) != 0 {
			t.Fatalf("push %d patched %v", push, got)
		}
	}
	if got := appliedTitle(t, root); got != "" {
		t.Errorf("a refused push recorded %q as this folder's own", got)
	}
	if _, err := runCLI(t, root, "app", "push", "--no-validate", "--force"); err != nil {
		t.Fatalf("push --force: %v", err)
	}
	if got := titlePatches(f); len(got) != 1 || got[0] != "Revenue explorer" {
		t.Errorf("--force patched %v, want the declared title once", got)
	}
}

// THE TITLE LEG ALONE, with the files guard out of the way (the local baseline
// is kept): a web rename and NO recorded title on a laptop is refused, on every
// push.
func TestAppPushOnALaptopRefusesAnUnrecordedWebRenameOnEveryPush(t *testing.T) {
	f := newFakeAppInstance(t)
	root := cloneTitledApp(t, f)
	// An older lock: no applied title. The local baseline is the one a clone
	// writes.
	lock, _ := wfdir.LoadLock(root)
	lock.SetAppliedTitle("dev", "")
	if err := wfdir.SaveLock(root, lock); err != nil {
		t.Fatal(err)
	}
	f.apps["app-1"].Name = "Renamed in the web app"
	f.versions["app-1"] = append([]api.DataApp{{ID: "dav-2"}}, f.versions["app-1"]...)
	writeLocal(t, root, "App.tsx", "mine\n")
	for push := 1; push <= 2; push++ {
		_, err := runCLI(t, root, "app", "push", "--no-validate")
		if err == nil || !strings.Contains(err.Error(), "Renamed in the web app") {
			t.Fatalf("push %d: want a refusal naming the live title, got %v", push, err)
		}
		if got := titlePatches(f); len(got) != 0 {
			t.Fatalf("push %d patched %v", push, got)
		}
	}
}

// LIVE EQUALS DESIRED IS NEVER DRIFT: a stale applied title beside an app that
// already holds the manifest's title is re-anchored, not refused.
func TestAppPushReanchorsATitleTheAppAlreadyHolds(t *testing.T) {
	f := newFakeAppInstance(t)
	root := cloneTitledApp(t, f)
	lock, _ := wfdir.LoadLock(root)
	lock.SetAppliedTitle("dev", "A stale title")
	if err := wfdir.SaveLock(root, lock); err != nil {
		t.Fatal(err)
	}
	freshCheckout(t, root)
	writeLocal(t, root, "App.tsx", "mine\n")
	if _, err := runCLI(t, root, "app", "push", "--no-validate"); err != nil {
		t.Fatalf("push refused the app's own title: %v", err)
	}
	if got := appliedTitle(t, root); got != "Revenue explorer" {
		t.Errorf("applied title = %q", got)
	}
	if got := titlePatches(f); len(got) != 0 {
		t.Errorf("patched %v", got)
	}
}

// A ROW NEVER PUBLISHED is retitled IN PLACE — a parentless draft is edited
// directly, and its head is its own id — so the head anchor cannot vouch for
// its title even though it "agrees": with nothing recorded, a mismatch is
// refused.
func TestAppPushNeverVouchesForAnUnpublishedAppsTitle(t *testing.T) {
	f := newFakeAppInstance(t)
	signInApp(t, f)
	root := t.TempDir()
	if _, err := runCLI(t, root, "app", "init", "--feature", "feat-1", "--title", "Revenue explorer", "--stack", "dev"); err != nil {
		t.Fatalf("init: %v", err)
	}
	if out, err := runCLI(t, root, "app", "push", "--no-validate", "--json"); err != nil {
		t.Fatalf("first push: %v\n%s", err, out)
	}
	// An older lock, a fresh checkout, and the draft retitled in place.
	lock, _ := wfdir.LoadLock(root)
	lock.SetAppliedTitle("dev", "")
	if err := wfdir.SaveLock(root, lock); err != nil {
		t.Fatal(err)
	}
	freshCheckout(t, root)
	for _, app := range f.apps {
		app.Name = "Renamed in the web app"
	}
	before := len(titlePatches(f))
	if _, err := runCLI(t, root, "app", "push", "--no-validate"); err == nil {
		t.Fatal("a mismatched title on an unpublished app was vouched for by its head")
	}
	if got := titlePatches(f); len(got) != before {
		t.Errorf("patched %v", got[before:])
	}
}
