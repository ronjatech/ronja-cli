package commands

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/ronjatech/ronja-cli/internal/api"
	"github.com/ronjatech/ronja-cli/internal/wfdir"
)

// freshCheckout is what CI and a colleague's `git clone` actually have: the
// committed files and nothing else. `.ronja/` is git-ignored, so removing it is
// a faithful model rather than a shortcut — and it is the state that used to
// leave --force as the only way through.
func freshCheckout(t *testing.T, root string) {
	t.Helper()
	if err := os.RemoveAll(filepath.Join(root, wfdir.StateDirName)); err != nil {
		t.Fatal(err)
	}
}

// TestFreshCheckoutPushesWithoutForce is THE acceptance criterion of the head
// anchor, for a workflow: a checkout with no local baseline pushes, safely,
// with no flag — and the file writes still carry their preconditions, which
// --force would have switched off.
func TestFreshCheckoutPushesWithoutForce(t *testing.T) {
	f := newFakeInstance(t)
	f.AddWorkflow(&api.Workflow{ID: "wf-1", Lifecycle: api.LifecycleLive, FeatureScope: "private"},
		api.WorkflowFile{Path: "main.py", Content: "live\n"})
	f.versions["wf-1"] = []api.Workflow{{ID: "wfv-1"}}
	signIn(t, f)

	out, err := runCLI(t, t.TempDir(), "wf", "clone", "wf-1", "--stack", "dev", "--json")
	if err != nil {
		t.Fatalf("clone: %v", err)
	}
	root := decodeJSON(t, out)["root"].(string)
	if lock := readFile(t, wfdir.LockPath(root)); !strings.Contains(lock, `"wfv-1"`) {
		t.Fatalf("clone must record the version it forked from:\n%s", lock)
	}

	writeLocal(t, root, "main.py", "mine\n")
	freshCheckout(t, root)
	f.fileWrites = nil

	out, err = runCLI(t, root, "wf", "push", "--json")
	if err != nil {
		t.Fatalf("a fresh checkout must push without --force: %v", err)
	}
	draftID := decodeJSON(t, out)["draftID"].(string)
	if got := f.FileContents(draftID)["main.py"]; got != "mine\n" {
		t.Errorf("the push did not land: main.py = %q", got)
	}
	// The half that makes this better than --force rather than merely quieter.
	var asserted bool
	for _, w := range f.fileWrites {
		if w.Path == "main.py" && w.BaseSha256 != nil {
			asserted = true
			if *w.BaseSha256 != wfdir.HashString("live\n") {
				t.Errorf("the precondition asserted %q, want the live bytes", *w.BaseSha256)
			}
		}
	}
	if !asserted {
		t.Errorf("an anchored push must still arm the per-file precondition; writes = %+v", f.fileWrites)
	}
}

// TestFreshCheckoutRefusesWhenSomebodyPublished is the other half, and the one
// that makes the first half safe to have: the anchor is not a way past the
// guard, it IS the guard.
func TestFreshCheckoutRefusesWhenSomebodyPublished(t *testing.T) {
	f := newFakeInstance(t)
	f.AddWorkflow(&api.Workflow{ID: "wf-1", Lifecycle: api.LifecycleLive, FeatureScope: "private"},
		api.WorkflowFile{Path: "main.py", Content: "live\n"})
	f.versions["wf-1"] = []api.Workflow{{ID: "wfv-1"}}
	signIn(t, f)

	out, err := runCLI(t, t.TempDir(), "wf", "clone", "wf-1", "--stack", "dev", "--json")
	if err != nil {
		t.Fatalf("clone: %v", err)
	}
	root := decodeJSON(t, out)["root"].(string)
	writeLocal(t, root, "main.py", "mine\n")
	freshCheckout(t, root)

	// A colleague publishes.
	f.versions["wf-1"] = []api.Workflow{{ID: "wfv-2"}, {ID: "wfv-1"}}

	_, err = runCLI(t, root, "wf", "push")
	if err == nil {
		t.Fatal("a push onto a workflow that was published to since the clone must be refused")
	}
	for _, want := range []string{"wfv-1", "wfv-2", "published to"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("the refusal must name what moved (missing %q): %v", want, err)
		}
	}
	if strings.Contains(err.Error(), "--force") {
		t.Errorf("the refusal must not offer --force — it would discard the publish it just found: %v", err)
	}
}

// TestFreshCheckoutOfALegacyFolderIsUnchanged: "v1 stays v1". A folder nobody
// has named a stack in has nowhere committed to keep an anchor, so it must
// behave exactly as it did — including refusing, and including naming --force.
func TestFreshCheckoutOfALegacyFolderIsUnchanged(t *testing.T) {
	f := newFakeInstance(t)
	f.AddWorkflow(&api.Workflow{ID: "wf-1", Lifecycle: api.LifecycleLive, FeatureScope: "private"},
		api.WorkflowFile{Path: "main.py", Content: "live\n"})
	f.versions["wf-1"] = []api.Workflow{{ID: "wfv-1"}}
	signIn(t, f)

	root := cloneFolder(t, f, "wf-1")
	writeLocal(t, root, "main.py", "mine\n")
	freshCheckout(t, root)
	f.Requests = nil

	_, err := runCLI(t, root, "wf", "push")
	if err == nil || !strings.Contains(err.Error(), "no sync baseline") {
		t.Fatalf("a legacy folder must still refuse a baseline-less push: %v", err)
	}
	for _, req := range f.Requests {
		if strings.HasSuffix(req, "/versions") {
			t.Errorf("a legacy folder must not pay for a version read it can never record:\n%s",
				strings.Join(f.Requests, "\n"))
		}
	}
}

// TestCloneFromADraftAnchorsOnItsBase: the anchor names the version this
// folder's CONTENT descends from, which for a draft clone is the draft's own
// base — never the current head. Recording the head would claim to have seen a
// publish the folder does not contain, and the next baseline-less push would
// then overwrite it in silence.
func TestCloneFromADraftAnchorsOnItsBase(t *testing.T) {
	f := newFakeInstance(t)
	f.AddWorkflow(&api.Workflow{ID: "wf-1", Lifecycle: api.LifecycleLive, FeatureScope: "private"},
		api.WorkflowFile{Path: "main.py", Content: "live\n"})
	// My draft was forked at wfv-1; a colleague has since published wfv-2.
	draft := f.AddDraft("wf-1", "wf-1-draft",
		api.WorkflowFile{Path: "main.py", Content: "my draft\n"})
	draft.BaseVersionID = "wfv-1"
	f.versions["wf-1"] = []api.Workflow{{ID: "wfv-2"}, {ID: "wfv-1"}}
	signIn(t, f)

	out, err := runCLI(t, t.TempDir(), "wf", "clone", "wf-1", "--stack", "dev", "--json")
	if err != nil {
		t.Fatalf("clone: %v", err)
	}
	root := decodeJSON(t, out)["root"].(string)
	lock := readFile(t, wfdir.LockPath(root))
	if !strings.Contains(lock, `"wfv-1"`) {
		t.Errorf("a draft clone must anchor on the draft's base:\n%s", lock)
	}
	if strings.Contains(lock, `"wfv-2"`) {
		t.Errorf("a draft clone must NOT anchor on a version it never saw:\n%s", lock)
	}
}

// TestAnchorIsNotStampedOntoAStaleDraft: the FreshDraft leg of the guard. The
// live row has not moved, but a draft was already open — so the anchor cannot
// speak for the bytes in it, and the push refuses with the remedy that fits
// (discard the draft) rather than the flag that does not.
func TestAnchorIsNotStampedOntoAStaleDraft(t *testing.T) {
	f := newFakeInstance(t)
	f.AddWorkflow(&api.Workflow{ID: "wf-1", Lifecycle: api.LifecycleLive, FeatureScope: "private"},
		api.WorkflowFile{Path: "main.py", Content: "live\n"})
	f.versions["wf-1"] = []api.Workflow{{ID: "wfv-1"}}
	signIn(t, f)

	out, err := runCLI(t, t.TempDir(), "wf", "clone", "wf-1", "--stack", "dev", "--json")
	if err != nil {
		t.Fatalf("clone: %v", err)
	}
	root := decodeJSON(t, out)["root"].(string)
	writeLocal(t, root, "main.py", "mine\n")
	freshCheckout(t, root)

	// Somebody (me, in the web builder) has a draft open that this checkout has
	// never seen.
	stale := f.AddDraft("wf-1", "wf-1-draft",
		api.WorkflowFile{Path: "main.py", Content: "half-finished\n"})
	stale.BaseVersionID = "wfv-1"

	_, err = runCLI(t, root, "wf", "push")
	if err == nil {
		t.Fatal("a baseline-less push onto an already-open draft must be refused")
	}
	if !strings.Contains(err.Error(), "draft") || !strings.Contains(err.Error(), "discard") {
		t.Errorf("the refusal must name the open draft and how to clear it: %v", err)
	}
}

// TestAppFreshCheckoutPushesWithoutForce is the same acceptance criterion for a
// data app, which had no committable anchor of any kind before this.
func TestAppFreshCheckoutPushesWithoutForce(t *testing.T) {
	f := newFakeAppInstance(t)
	f.AddApp(&api.DataApp{ID: "app-1", Lifecycle: api.LifecycleLive},
		api.DataAppFile{Path: "App.tsx", Content: "live\n"})
	f.versions["app-1"] = []api.DataApp{{ID: "dav-1"}}
	signInApp(t, f)

	out, err := runCLI(t, t.TempDir(), "app", "clone", "app-1", "--stack", "dev", "--json")
	if err != nil {
		t.Fatalf("clone: %v", err)
	}
	root := decodeJSON(t, out)["root"].(string)
	if lock := readFile(t, wfdir.LockPath(root)); !strings.Contains(lock, `"dav-1"`) {
		t.Fatalf("clone must record the version it forked from:\n%s", lock)
	}

	writeLocal(t, root, "App.tsx", "mine\n")
	freshCheckout(t, root)

	if _, err := runCLI(t, root, "app", "push", "--no-validate"); err != nil {
		t.Fatalf("a fresh checkout must push without --force: %v", err)
	}
}

// TestAppFreshCheckoutRefusesWhenSomebodyPublished — the refusing half.
func TestAppFreshCheckoutRefusesWhenSomebodyPublished(t *testing.T) {
	f := newFakeAppInstance(t)
	f.AddApp(&api.DataApp{ID: "app-1", Lifecycle: api.LifecycleLive},
		api.DataAppFile{Path: "App.tsx", Content: "live\n"})
	f.versions["app-1"] = []api.DataApp{{ID: "dav-1"}}
	signInApp(t, f)

	out, err := runCLI(t, t.TempDir(), "app", "clone", "app-1", "--stack", "dev", "--json")
	if err != nil {
		t.Fatalf("clone: %v", err)
	}
	root := decodeJSON(t, out)["root"].(string)
	writeLocal(t, root, "App.tsx", "mine\n")
	freshCheckout(t, root)

	f.versions["app-1"] = []api.DataApp{{ID: "dav-2"}, {ID: "dav-1"}}

	_, err = runCLI(t, root, "app", "push", "--no-validate")
	if err == nil {
		t.Fatal("a push onto an app that was published to since the clone must be refused")
	}
	for _, want := range []string{"dav-1", "dav-2", "published to"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("the refusal must name what moved (missing %q): %v", want, err)
		}
	}
}

// TestPublishReanchorsTheLock: a commit makes a NEW version, so the folder that
// produced it is the folder that agrees with it. Without this the anchor is one
// version behind from the first publish onwards, and every CI push after that
// refuses — the guard crying wolf about the deploy's own work.
func TestPublishReanchorsTheLock(t *testing.T) {
	f := newFakeInstance(t)
	f.AddWorkflow(&api.Workflow{ID: "wf-1", Lifecycle: api.LifecycleLive, FeatureScope: "private"},
		api.WorkflowFile{Path: "main.py", Content: "live\n"})
	f.versions["wf-1"] = []api.Workflow{{ID: "wfv-1"}}
	signIn(t, f)

	out, err := runCLI(t, t.TempDir(), "wf", "clone", "wf-1", "--stack", "dev", "--json")
	if err != nil {
		t.Fatalf("clone: %v", err)
	}
	root := decodeJSON(t, out)["root"].(string)
	writeLocal(t, root, "main.py", "mine\n")
	if _, err := runCLI(t, root, "wf", "push"); err != nil {
		t.Fatalf("push: %v", err)
	}
	// The commit lands, and the server's history moves with it.
	f.versions["wf-1"] = []api.Workflow{{ID: "wfv-2"}, {ID: "wfv-1"}}
	if _, err := runCLI(t, root, "wf", "publish"); err != nil {
		t.Fatalf("publish: %v", err)
	}
	lock := readFile(t, wfdir.LockPath(root))
	if !strings.Contains(lock, `"wfv-2"`) {
		t.Errorf("publish must re-anchor onto the version it just committed:\n%s", lock)
	}
}

// TestFreshCheckoutOfANeverPublishedWorkflowPushes is the acceptance criterion
// on the shape it failed hardest on: `wf init` → `push` → commit → CI. Publish
// is a separate manual step, so a folder whose workflow exists only as a
// parentless draft is entirely ordinary — and it could not push from a fresh
// checkout at all. Two files, because the one-file case slips through
// isUntouchedFirstPush and hides this.
func TestFreshCheckoutOfANeverPublishedWorkflowPushes(t *testing.T) {
	f := newFakeInstance(t)
	signIn(t, f)
	root := t.TempDir()
	if _, err := runCLI(t, root, "wf", "init", "--feature", "feat-1", "--stack", "dev"); err != nil {
		t.Fatalf("init --stack dev: %v", err)
	}
	writeLocal(t, root, "main.py", "body\n")
	writeLocal(t, root, "helper.py", "helper\n")
	if _, err := runCLI(t, root, "wf", "push"); err != nil {
		t.Fatalf("first push: %v", err)
	}
	// The push that CREATED the row must leave an anchor behind: a row with no
	// versions is its own head, and without recording it the committed lock says
	// nothing at all and the checkout below has nothing to be held to.
	lock := readFile(t, wfdir.LockPath(root))
	if !strings.Contains(lock, `"headVersionID"`) {
		t.Fatalf("a first push must anchor the folder on the row it created:\n%s", lock)
	}

	writeLocal(t, root, "main.py", "changed in CI\n")
	freshCheckout(t, root)

	out, err := runCLI(t, root, "wf", "push", "--json")
	if err != nil {
		t.Fatalf("a fresh checkout of a never-published workflow must push without --force: %v", err)
	}
	draftID := decodeJSON(t, out)["draftID"].(string)
	if got := f.FileContents(draftID)["main.py"]; got != "changed in CI\n" {
		t.Errorf("the push did not land: main.py = %q", got)
	}
}

// TestFreshCheckoutOfAClonedNeverPublishedWorkflowPushes is the same failure
// arrived at by cloning rather than by creating — and the one whose advice was
// destructive. The clone anchors on the row's own id (an unversioned row's
// head), the checkout then found a "draft" open, and the refusal told the reader
// to run `wf discard`, which REFUSES a parentless draft and routes to
// --delete-workflow. The remedy named either failed or deleted the workflow.
func TestFreshCheckoutOfAClonedNeverPublishedWorkflowPushes(t *testing.T) {
	f := newFakeInstance(t)
	f.AddWorkflow(&api.Workflow{ID: "wf-1", Lifecycle: api.LifecycleDraft, FeatureScope: "private"},
		api.WorkflowFile{Path: "main.py", Content: "live\n"},
		api.WorkflowFile{Path: "helper.py", Content: "helper\n"})
	signIn(t, f)

	out, err := runCLI(t, t.TempDir(), "wf", "clone", "wf-1", "--stack", "dev", "--json")
	if err != nil {
		t.Fatalf("clone: %v", err)
	}
	root := decodeJSON(t, out)["root"].(string)
	if lock := readFile(t, wfdir.LockPath(root)); !strings.Contains(lock, `"wf-1"`) {
		t.Fatalf("an unversioned row's head is its own id:\n%s", lock)
	}

	writeLocal(t, root, "main.py", "mine\n")
	freshCheckout(t, root)

	if _, err := runCLI(t, root, "wf", "push"); err != nil {
		t.Fatalf("a fresh checkout of a cloned never-published workflow must push: %v", err)
	}
	if got := f.FileContents("wf-1")["main.py"]; got != "mine\n" {
		t.Errorf("the push did not land on the workflow itself: main.py = %q", got)
	}
}

// TestFreshCheckoutOfANeverPublishedAppPushes — the same criterion for a data
// app, which has no isUntouchedFirstPush escape at all.
func TestFreshCheckoutOfANeverPublishedAppPushes(t *testing.T) {
	f := newFakeAppInstance(t)
	signInApp(t, f)
	root := t.TempDir()
	if _, err := runCLI(t, root, "app", "init", "--feature", "feat-1", "--stack", "dev"); err != nil {
		t.Fatalf("app init --stack dev: %v", err)
	}
	if _, err := runCLI(t, root, "app", "push", "--no-validate"); err != nil {
		t.Fatalf("first app push: %v", err)
	}
	if lock := readFile(t, wfdir.LockPath(root)); !strings.Contains(lock, `"headVersionID"`) {
		t.Fatalf("a first push must anchor the folder on the app it created:\n%s", lock)
	}

	writeLocal(t, root, "App.tsx", "export default function App() { return null }\n")
	freshCheckout(t, root)

	if _, err := runCLI(t, root, "app", "push", "--no-validate"); err != nil {
		t.Fatalf("a fresh checkout of a never-published app must push without --force: %v", err)
	}
}

// TestForcedPushPastAMovedHeadClearsTheAnchor is the silent-overwrite
// regression. --force writes a DRAFT and never touches live, so a forced push
// past a version somebody else published leaves that version's content nowhere
// in the repository. Advancing the anchor onto it made the NEXT fresh checkout
// vouch for it and overwrite it with preconditions that pass — a refusal turned
// into the exact thing the anchor exists to prevent.
func TestForcedPushPastAMovedHeadClearsTheAnchor(t *testing.T) {
	f := newFakeInstance(t)
	f.AddWorkflow(&api.Workflow{ID: "wf-1", Lifecycle: api.LifecycleLive, FeatureScope: "private"},
		api.WorkflowFile{Path: "main.py", Content: "live\n"})
	f.versions["wf-1"] = []api.Workflow{{ID: "wfv-1"}}
	signIn(t, f)

	out, err := runCLI(t, t.TempDir(), "wf", "clone", "wf-1", "--stack", "dev", "--json")
	if err != nil {
		t.Fatalf("clone: %v", err)
	}
	root := decodeJSON(t, out)["root"].(string)
	writeLocal(t, root, "main.py", "mine\n")

	// A colleague publishes wfv-2. This folder's content descends from wfv-1 and
	// has never contained a byte of theirs.
	f.versions["wf-1"] = []api.Workflow{{ID: "wfv-2"}, {ID: "wfv-1"}}

	if _, err := runCLI(t, root, "wf", "push", "--force"); err != nil {
		t.Fatalf("forced push: %v", err)
	}
	lock := readFile(t, wfdir.LockPath(root))
	if strings.Contains(lock, `"wfv-2"`) {
		t.Fatalf("a forced push must not claim a version this folder never saw:\n%s", lock)
	}

	// And the consequence, which is what makes it a blocker rather than a
	// cosmetic wrong value: the next fresh checkout must not VOUCH.
	freshCheckout(t, root)
	writeLocal(t, root, "main.py", "second CI push\n")
	if _, err := runCLI(t, root, "wf", "push"); err == nil {
		t.Fatal("a fresh checkout after a forced push must not vouch on a version the folder never saw")
	}
}

// TestAppCloneFromADraftDoesNotAnchorOnTheAppID is the app half of
// TestCloneFromADraftAnchorsOnItsBase, and it is the test whose absence let a
// data app record an anchor that can never match: rdataapp stamps the LIVE APP'S
// OWN ID in base_version_id, where rworkflow stamps the head VERSION's, so the
// shared resolver wrote "app-1" into a lock whose head reader answers "dav-1"
// forever. Every later fresh-checkout push was then refused as "it was at
// version app-1", an app id where a version id belongs, and re-cloning
// reproduced it.
func TestAppCloneFromADraftDoesNotAnchorOnTheAppID(t *testing.T) {
	f := newFakeAppInstance(t)
	f.AddApp(&api.DataApp{ID: "app-1", Lifecycle: api.LifecycleLive},
		api.DataAppFile{Path: "App.tsx", Content: "live\n"})
	draft := f.AddDraft("app-1", "app-1-draft", api.DataAppFile{Path: "App.tsx", Content: "my draft\n"})
	// Exactly what buildDraftFromParent writes — the parent's own id, not a
	// version, on an app that HAS versions.
	draft.BaseVersionID = "app-1"
	f.versions["app-1"] = []api.DataApp{{ID: "dav-1"}}
	signInApp(t, f)

	out, err := runCLI(t, t.TempDir(), "app", "clone", "app-1", "--stack", "dev", "--json")
	if err != nil {
		t.Fatalf("clone: %v", err)
	}
	root := decodeJSON(t, out)["root"].(string)
	lock := readFile(t, wfdir.LockPath(root))
	if strings.Contains(lock, `"headVersionID"`) {
		t.Fatalf("which version a data-app draft forked from is not recorded anywhere, so the clone must record NO anchor rather than one that cannot match:\n%s", lock)
	}

	// The consequence, stated as the user meets it: whatever the push does with
	// this folder, it must not refuse by naming an app id as a version.
	writeLocal(t, root, "App.tsx", "mine\n")
	freshCheckout(t, root)
	_, err = runCLI(t, root, "app", "push", "--no-validate")
	if err != nil && strings.Contains(err.Error(), "at version app-1") {
		t.Errorf("the refusal names an app id where a version belongs: %v", err)
	}
}

// TestAppCloneFromARestoreDraftAnchorsOnThatVersion: the one rdataapp path that
// DOES stamp a real version id in base_version_id (RestoreFromVersion) means
// what the workflow field means, and must be used rather than thrown away with
// the sentinel.
func TestAppCloneFromARestoreDraftAnchorsOnThatVersion(t *testing.T) {
	f := newFakeAppInstance(t)
	f.AddApp(&api.DataApp{ID: "app-1", Lifecycle: api.LifecycleLive},
		api.DataAppFile{Path: "App.tsx", Content: "live\n"})
	draft := f.AddDraft("app-1", "app-1-draft", api.DataAppFile{Path: "App.tsx", Content: "restored\n"})
	draft.BaseVersionID = "dav-1"
	f.versions["app-1"] = []api.DataApp{{ID: "dav-2"}, {ID: "dav-1"}}
	signInApp(t, f)

	out, err := runCLI(t, t.TempDir(), "app", "clone", "app-1", "--stack", "dev", "--json")
	if err != nil {
		t.Fatalf("clone: %v", err)
	}
	lock := readFile(t, wfdir.LockPath(decodeJSON(t, out)["root"].(string)))
	if !strings.Contains(lock, `"dav-1"`) {
		t.Errorf("a restore-forked draft anchors on the version it restored:\n%s", lock)
	}
	if strings.Contains(lock, `"dav-2"`) {
		t.Errorf("and never on a version it never saw:\n%s", lock)
	}
}

// TestAppCloneFromADraftOfAnUnversionedAppAnchorsOnTheApp: when the app has no
// versions at all, the parent-id sentinel and the head reader's answer are the
// SAME value, so the anchor is knowable and must be recorded — otherwise every
// clone of a never-published app's draft would give up an anchor it could have
// had.
func TestAppCloneFromADraftOfAnUnversionedAppAnchorsOnTheApp(t *testing.T) {
	f := newFakeAppInstance(t)
	f.AddApp(&api.DataApp{ID: "app-1", Lifecycle: api.LifecycleLive},
		api.DataAppFile{Path: "App.tsx", Content: "live\n"})
	draft := f.AddDraft("app-1", "app-1-draft", api.DataAppFile{Path: "App.tsx", Content: "my draft\n"})
	draft.BaseVersionID = "app-1"
	signInApp(t, f)

	out, err := runCLI(t, t.TempDir(), "app", "clone", "app-1", "--stack", "dev", "--json")
	if err != nil {
		t.Fatalf("clone: %v", err)
	}
	lock := readFile(t, wfdir.LockPath(decodeJSON(t, out)["root"].(string)))
	if !strings.Contains(lock, `"app-1"`) || !strings.Contains(lock, `"headVersionID"`) {
		t.Errorf("an unversioned app's head is its own id, and that anchor is real:\n%s", lock)
	}
}

// anchorFolder is the smallest thing anchorAfterPush can be driven against: a
// stack folder with a lock on disk and nothing else.
func anchorFolder(t *testing.T, stack, recorded string) *folder {
	t.Helper()
	root := t.TempDir()
	manifest := &wfdir.Manifest{Kind: wfdir.KindWorkflow, Title: "T", Entrypoint: "main.py"}
	lock := &wfdir.Lock{}
	if recorded != "" {
		lock.SetHeadVersion(stack, recorded)
	}
	if err := wfdir.SaveFolder(root, manifest, lock); err != nil {
		t.Fatal(err)
	}
	return &folder{Root: root, Kind: wfdir.WorkflowKind, Manifest: manifest, Lock: lock, Stack: stack}
}

// TestAnchorAfterPushDecisionTable covers the four-input decision directly.
// Every other test in this file asserts a REFUSAL, and a refusal short-circuits
// before anchorAfterPush ever runs — so the table it actually implements had no
// coverage at all, which is why the forced leg shipped writing a version the
// folder had never seen.
func TestAnchorAfterPushDecisionTable(t *testing.T) {
	for _, tc := range []struct {
		name          string
		recorded      string
		current       string
		anchoredDraft bool
		forced        bool
		want          string
	}{
		{"a draft forked from the recorded head re-states it", "wfv-1", "wfv-1", true, false, "wfv-1"},
		{"a draft forked from live acquires the first anchor", "", "wfv-1", true, false, "wfv-1"},
		{"a row this push created anchors on itself", "", "wf-new-1", true, false, "wf-new-1"},
		{"a stale draft on an unmoved head re-states it", "wfv-1", "wfv-1", false, false, "wfv-1"},
		{"a stale draft on a MOVED head records nothing", "wfv-1", "wfv-2", false, false, "wfv-1"},
		{"a fresh draft on a moved head follows the row it forked", "wfv-1", "wfv-2", true, false, "wfv-2"},
		{"--force past a moved head CLEARS rather than advances", "wfv-1", "wfv-2", false, true, ""},
		{"--force clears even when this push forked the draft it wrote", "wfv-1", "wfv-2", true, true, ""},
		{"--force with nothing recorded still claims nothing", "", "wfv-2", false, true, ""},
		{"--force on a fresh fork with nothing recorded claims nothing either", "", "wfv-2", true, true, ""},
		{"--force on an unmoved head leaves it alone", "wfv-1", "wfv-1", false, true, "wfv-1"},
		{"a head that could not be read changes nothing", "wfv-1", "", false, false, "wfv-1"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := anchorFolder(t, "dev", tc.recorded)
			h := headAgreement{Recorded: tc.recorded, Current: tc.current, AnchoredDraft: tc.anchoredDraft}
			if err := anchorAfterPush(f, h, tc.forced); err != nil {
				t.Fatal(err)
			}
			if got := f.headVersion(); got != tc.want {
				t.Errorf("in memory: anchor = %q, want %q", got, tc.want)
			}
			// And on DISK, because the lock is a committed file: a pointer left
			// in memory is a pointer the next checkout does not have.
			reloaded, err := wfdir.LoadLock(f.Root)
			if err != nil {
				t.Fatal(err)
			}
			if got := reloaded.HeadVersion("dev"); got != tc.want {
				t.Errorf("on disk: anchor = %q, want %q", got, tc.want)
			}
		})
	}
}

// TestLegacyFolderIsNeverAnchored: "v1 stays v1" applied to the write half. A
// folder with no stack has nowhere committed to keep a pointer, so every leg of
// the table above must be a no-op for it.
func TestLegacyFolderIsNeverAnchored(t *testing.T) {
	f := anchorFolder(t, "dev", "")
	f.Stack = ""
	if err := anchorAfterPush(f, headAgreement{Current: "wfv-1", AnchoredDraft: true}, false); err != nil {
		t.Fatal(err)
	}
	if got := f.headVersion(); got != "" {
		t.Errorf("a legacy folder recorded an anchor: %q", got)
	}
}

// TestDiscardDoesNotReanchorPastAPublish is anchorAfterPush's forced-leg bug
// arriving through a quieter door. `wf discard` throws the DRAFT away and leaves
// the folder's files exactly as they are, so if a colleague published while that
// draft was open, re-anchoring on live stamps a version whose content the
// committed files do not contain — and the next fresh checkout vouches on it.
func TestDiscardDoesNotReanchorPastAPublish(t *testing.T) {
	f := newFakeInstance(t)
	f.AddWorkflow(&api.Workflow{ID: "wf-1", Lifecycle: api.LifecycleLive, FeatureScope: "private"},
		api.WorkflowFile{Path: "main.py", Content: "live\n"})
	f.versions["wf-1"] = []api.Workflow{{ID: "wfv-1"}}
	signIn(t, f)

	out, err := runCLI(t, t.TempDir(), "wf", "clone", "wf-1", "--stack", "dev", "--json")
	if err != nil {
		t.Fatalf("clone: %v", err)
	}
	root := decodeJSON(t, out)["root"].(string)
	writeLocal(t, root, "main.py", "mine\n")
	if _, err := runCLI(t, root, "wf", "push"); err != nil {
		t.Fatalf("push: %v", err)
	}

	// A colleague publishes while my draft is open, and I throw my draft away.
	f.versions["wf-1"] = []api.Workflow{{ID: "wfv-2"}, {ID: "wfv-1"}}
	if _, err := runCLI(t, root, "wf", "discard", "--yes"); err != nil {
		t.Fatalf("discard: %v", err)
	}

	lock := readFile(t, wfdir.LockPath(root))
	if strings.Contains(lock, `"wfv-2"`) {
		t.Fatalf("a discard leaves the local files untouched, so it may not claim the version published since:\n%s", lock)
	}
	if !strings.Contains(lock, `"wfv-1"`) {
		t.Fatalf("and it must leave the anchor it had, which is what makes the next push refuse honestly:\n%s", lock)
	}
}
