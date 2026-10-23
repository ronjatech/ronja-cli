package commands

import (
	"context"
	"errors"
	"net/http"
	"slices"
	"strings"
	"testing"
)

// The draft kinds' live-first legs of `sync apply` (workflow, data app): what a
// publish an earlier run left behind, a classic push, a web publish and a refused
// apply do to the draft. The fixtures are sync_apply_idempotent_test.go's.

// A STAGED TITLE WHOSE PUBLISH FAILED IS STILL DEPLOYED BY THE NEXT RUN. Run one
// stages the rename in a draft and the commit is refused; run two has nothing
// to push and a draft that pre-existed it, and publishes it. The gate this
// replaced compared FILES, so a draft whose only difference from live was its
// title read as nothing staged and the rename sat in a draft for ever.
func TestSyncApplyPublishesAStagedTitleAnEarlierRunLeftBehind(t *testing.T) {
	for kind, setup := range draftKindFixtures() {
		t.Run(kind, func(t *testing.T) {
			fx := setup(t)
			hooks := fx.draftKind
			hooks.retitle(t, "Renamed in the repository")
			hooks.failCommit(403)
			if code, line, _, out := applyOnce(t, fx); code != syncExitDrifted {
				t.Fatalf("run 1: exit = %d, folder = %+v, want the refused publish\n%s", code, line, out)
			}
			hooks.failCommit(0)

			code, line, _, out := applyOnce(t, fx)
			if code != syncExitClean || line.Verdict != verdictApplied {
				t.Fatalf("run 2: exit = %d, folder = %+v\n%s", code, line, out)
			}
			if got := hooks.liveTitle(); got != "Renamed in the repository" {
				t.Fatalf("the live row is titled %q — run 2 reported the folder deployed while its rename sat in a draft\n%s", got, out)
			}
		})
	}
}

// THE CLASSIC CHECK-OUT STILL OPENS A DRAFT, AND APPLY LEAVES IT ALONE.
//
// A single-folder push with nothing to write still checks a draft out — it is
// the gesture `wf test` runs against — and that draft is a copy of live. The
// apply after it must neither publish it (an empty version) nor write to it.
// This is what makes "did a draft pre-exist" a sound publish gate: the live
// comparison is asked whether or not a draft is open, so a copy of live never
// reaches the gate. Asked only when no draft was open, this run publishes.
func TestSyncApplyAfterAClassicPushThatWroteNothingMintsNoVersion(t *testing.T) {
	for kind, setup := range draftKindFixtures() {
		t.Run(kind, func(t *testing.T) {
			fx := setup(t)
			hooks := fx.draftKind
			if out, err := runCLI(t, fx.root, hooks.push...); err != nil {
				t.Fatalf("%v: %v\n%s", hooks.push, err, out)
			}
			drafts := fx.drafts()
			if len(drafts) != 1 {
				t.Fatalf("the single-folder push left drafts %v — it must still check one out", drafts)
			}

			code, line, writes, out := applyOnce(t, fx)
			if code != syncExitClean || line.Verdict != verdictApplied {
				t.Fatalf("exit = %d, folder = %+v\n%s", code, line, out)
			}
			if hooks.commits() != 0 || len(writes) != 0 {
				t.Fatalf("apply after a push that wrote nothing sent %v and committed %d — an empty version\n%s",
					writes, hooks.commits(), out)
			}
			if got := fx.drafts(); !slices.Equal(got, drafts) {
				t.Errorf("drafts = %v, want the author's own %v left as it was", got, drafts)
			}
		})
	}
}

// DRIFT IS STILL REFUSED, AND LIVE EQUAL TO DESIRED STILL IS NOT DRIFT.
//
// A fresh checkout (no local baseline, the committed anchor at the version the
// repository forked from) meets a live row a colleague published to in the web
// app. When that publish left live DIFFERENT from the folder, apply refuses and
// writes nothing to live; when it left live holding exactly the folder (live
// equals desired is never drift), the
// folder is up to date and apply sends no write at all.
func TestSyncApplyStillRefusesAWebPublishTheRepositoryDoesNotHold(t *testing.T) {
	for kind, setup := range draftKindFixtures() {
		t.Run(kind, func(t *testing.T) {
			fx := setup(t)
			hooks := fx.draftKind
			committed := snapshotFolder(t, fx.root, true)
			mine := hooks.liveMain()

			hooks.webPublish("published in the web app\n")
			restoreCheckout(t, fx.root, committed)
			code, line, _, out := applyOnce(t, fx)
			if code != syncExitDrifted || line.Verdict != verdictRefused {
				t.Fatalf("exit = %d, folder = %+v — a web publish the repository does not hold was not refused\n%s", code, line, out)
			}
			if got := hooks.liveMain(); got != "published in the web app\n" || hooks.commits() != 0 {
				t.Fatalf("live = %q after %d commit(s) — the colleague's publish was overwritten\n%s", got, hooks.commits(), out)
			}

			// Their publish put back exactly what the repository holds. The
			// refused run above checked a draft out before its drift guard fired
			// (the push resolves its target first) and discarded it on the way
			// out, so this run starts with no draft at all.
			hooks.webPublish(mine)
			restoreCheckout(t, fx.root, committed)
			code, line, writes, out := applyOnce(t, fx)
			if code != syncExitClean || line.Verdict != verdictApplied {
				t.Fatalf("live equal to desired: exit = %d, folder = %+v\n%s", code, line, out)
			}
			if len(writes) != 0 {
				t.Fatalf("live equal to desired sent %v", writes)
			}
		})
	}
}

// A DRIFT REFUSAL LEAVES NO DRAFT BEHIND. A colleague's web publish moves live
// away from the folder and apply refuses — correctly. The refused push had
// already checked a draft out (it resolves its target before the drift guard
// runs), and a draft left standing is one every LATER fresh-checkout apply
// finds pre-existing and refuses over ("you already have a draft … run
// discard"), so the folder stayed blocked after the author had absorbed the
// change and moved on, until somebody discarded it by hand.
//
// The whole sequence: refused with no draft left, absorbed (live already holds
// it, nothing sent), then a real edit that deploys from a fresh checkout.
func TestSyncApplyDriftRefusalLeavesNoDraftBehind(t *testing.T) {
	for kind, setup := range draftKindFixtures() {
		t.Run(kind, func(t *testing.T) {
			fx := setup(t)
			hooks := fx.draftKind
			committed := snapshotFolder(t, fx.root, true)

			hooks.webPublish("published in the web app\n")
			restoreCheckout(t, fx.root, committed)
			code, line, _, out := applyOnce(t, fx)
			if code != syncExitDrifted || line.Verdict != verdictRefused {
				t.Fatalf("exit = %d, folder = %+v — the web publish was not refused\n%s", code, line, out)
			}
			if drafts := fx.drafts(); len(drafts) != 0 {
				t.Fatalf("the refused apply left draft(s) %v behind\n%s", drafts, out)
			}

			// The author absorbs the colleague's change and commits the folder.
			writeLocal(t, fx.root, mainFileOf(fx), "published in the web app\n")
			code, line, writes, out := applyOnce(t, fx)
			if code != syncExitClean || line.Verdict != verdictApplied || len(writes) != 0 {
				t.Fatalf("absorbed: exit = %d, folder = %+v, writes = %v\n%s", code, line, writes, out)
			}
			committed = snapshotFolder(t, fx.root, true)

			// And then a real edit, deployed from a fresh checkout.
			restoreCheckout(t, fx.root, committed)
			writeLocal(t, fx.root, mainFileOf(fx), "edited after absorbing\n")
			committed = snapshotFolder(t, fx.root, true)
			restoreCheckout(t, fx.root, committed)
			code, line, _, out = applyOnce(t, fx)
			if code != syncExitClean || line.Verdict != verdictApplied {
				t.Fatalf("the edit after absorbing: exit = %d, folder = %+v\n%s", code, line, out)
			}
			if got := hooks.liveMain(); got != "edited after absorbing\n" {
				t.Fatalf("live = %q — the edit did not deploy\n%s", got, out)
			}
		})
	}
}

// A REFUSED FIRST WRITE LEAVES NO DRAFT BEHIND EITHER. The server refusing the
// push's very first write (here the one file it has to send) means nothing
// landed: the draft this apply checked out still holds only a copy of live, and
// left standing it blocks every later fresh-checkout apply exactly as a drift
// refusal's did. Once the server accepts the write, the same apply deploys.
func TestSyncApplyRefusedFirstWriteLeavesNoDraftBehind(t *testing.T) {
	for kind, setup := range draftKindFixtures() {
		t.Run(kind, func(t *testing.T) {
			fx := setup(t)
			hooks := fx.draftKind
			writeLocal(t, fx.root, mainFileOf(fx), "edited\n")
			committed := snapshotFolder(t, fx.root, true)
			hooks.failPut(http.StatusUnprocessableEntity)
			code, line, writes, out := applyOnce(t, fx)
			if code == syncExitClean || line.Verdict == verdictApplied {
				t.Fatalf("exit = %d, folder = %+v — the refused write was not reported\n%s", code, line, out)
			}
			if drafts := fx.drafts(); len(drafts) != 0 {
				t.Fatalf("the apply whose first write was refused left draft(s) %v behind (writes %v)\n%s", drafts, writes, out)
			}

			hooks.failPut(0)
			restoreCheckout(t, fx.root, committed)
			code, line, _, out = applyOnce(t, fx)
			if code != syncExitClean || line.Verdict != verdictApplied {
				t.Fatalf("once the write is accepted: exit = %d, folder = %+v\n%s", code, line, out)
			}
			if got := hooks.liveMain(); got != "edited\n" {
				t.Fatalf("live = %q — the edit did not deploy\n%s", got, out)
			}
		})
	}
}

// A DRAFT THAT WAS ALREADY THERE IS NEVER DISCARDED. The discard above is for
// the copy of live a refused apply checked out itself; a draft the author's own
// push left open holds their work, and a refusal must leave it exactly as it was.
func TestSyncApplyDriftRefusalKeepsADraftThatWasAlreadyOpen(t *testing.T) {
	for kind, setup := range draftKindFixtures() {
		t.Run(kind, func(t *testing.T) {
			fx := setup(t)
			hooks := fx.draftKind
			writeLocal(t, fx.root, mainFileOf(fx), "staged by the author\n")
			if out, err := runCLI(t, fx.root, hooks.push...); err != nil {
				t.Fatalf("%v: %v\n%s", hooks.push, err, out)
			}
			drafts := fx.drafts()
			if len(drafts) != 1 {
				t.Fatalf("the author's push left drafts %v", drafts)
			}
			// The repository then moves on without that draft being published: a
			// fresh checkout has no baseline to say what the draft holds, so the
			// apply refuses rather than write over it.
			writeLocal(t, fx.root, mainFileOf(fx), "edited again\n")
			committed := snapshotFolder(t, fx.root, true)
			restoreCheckout(t, fx.root, committed)
			code, line, _, out := applyOnce(t, fx)
			if code != syncExitDrifted || line.Verdict != verdictRefused {
				t.Fatalf("exit = %d, folder = %+v — want the drift refusal\n%s", code, line, out)
			}
			if got := fx.drafts(); !slices.Equal(got, drafts) {
				t.Fatalf("drafts = %v, want the author's own %v left as it was\n%s", got, drafts, out)
			}
		})
	}
}

// …NOR ONE OPENED WHILE THE APPLY RAN. The apply decides a draft is its own
// from its inspection ("no draft open") and the checkout that follows is
// get-or-create, so a draft the same user opened in between — in the web app,
// or from a second job on the same token — came back from the checkout looking
// like this run's. Only a draft that still holds what live held is discarded.
func TestSyncApplyRefusalKeepsADraftOpenedDuringTheApply(t *testing.T) {
	for kind, setup := range draftKindFixtures() {
		t.Run(kind, func(t *testing.T) {
			fx := setup(t)
			hooks := fx.draftKind
			committed := snapshotFolder(t, fx.root, true)
			hooks.webPublish("published in the web app\n")
			restoreCheckout(t, fx.root, committed)
			hooks.openDraftAtCheckout("the author's work in progress\n")
			code, line, _, out := applyOnce(t, fx)
			if code != syncExitDrifted || line.Verdict != verdictRefused {
				t.Fatalf("exit = %d, folder = %+v — want the drift refusal\n%s", code, line, out)
			}
			if drafts := fx.drafts(); len(drafts) != 1 {
				t.Fatalf("drafts = %v — the author's draft, opened while the apply ran, was discarded\n%s", drafts, out)
			}
		})
	}
}

// mainFileOf is the fixture's one file, by kind.
func mainFileOf(fx idempotenceFixture) string {
	switch {
	case slices.Equal(fx.draftKind.push, []string{"app", "push"}):
		return "App.tsx"
	case fx.mod != nil:
		return "tracker.py"
	}
	return "main.py"
}

// discardUnwrittenCheckout's two promises that a fake server cannot reach: a
// draft that is not this run's own is never sent to the discard, and a discard
// that fails leaves the REFUSAL as the error — errors.As still finds it, so the
// verdict and exit code are the refusal's — with the leftover draft named.
func TestDiscardUnwrittenCheckoutKeepsTheRefusal(t *testing.T) {
	refusal := &driftRefusalForTest{}
	called := 0
	discard := func(context.Context, string) error { called++; return errors.New("HTTP 503") }

	if err := discardUnwrittenCheckout(context.Background(), false, "draft-1", "wf", discard, refusal); err != refusal || called != 0 {
		t.Fatalf("not own: err = %v, discards = %d — want the refusal untouched and no discard", err, called)
	}
	err := discardUnwrittenCheckout(context.Background(), true, "draft-1", "wf", discard, refusal)
	var got *driftRefusalForTest
	if called != 1 || !errors.As(err, &got) {
		t.Fatalf("own, discard failed: err = %v, discards = %d — want the refusal still findable", err, called)
	}
	for _, want := range []string{"refused for the test", "draft-1", "ronja wf discard"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("message %q does not mention %q", err, want)
		}
	}
	if !strings.HasPrefix(err.Error(), "refused for the test") {
		t.Errorf("the refusal is not the primary error: %q", err)
	}
}

type driftRefusalForTest struct{}

func (*driftRefusalForTest) Error() string { return "refused for the test" }
