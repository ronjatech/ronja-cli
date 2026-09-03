package commands

// The server-side half of `ronja app push`'s concurrency story: the per-file
// compare-and-swap preconditions.
//
// The drift guard is a check at ONE INSTANT — the moment the file listing was
// read — and everything between that listing and each write is a window it
// cannot see. These cover what closes it: the hashes the push attaches to every
// write, where it deliberately attaches none, and what happens when one is
// refused.
//
// The fake ENFORCES preconditions (fakeAppInstance.refusePrecondition), so a
// regression that sent the WRONG hash fails the push itself rather than only
// the assertion — which is the only way a test of this kind is worth writing.

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/ronjatech/ronja-cli/internal/api"
	"github.com/ronjatech/ronja-cli/internal/wfdir"
)

// The ordinary push: every write asserts what the baseline records the server
// as holding, and a path the baseline has never seen asserts "" — the file must
// NOT exist yet, because the folder believes it is creating it.
func TestAppPushArmsPreconditionsFromTheBaseline(t *testing.T) {
	f := newFakeAppInstance(t)
	live := f.AddApp(&api.DataApp{ID: "data_app-1"})
	f.AddDraft(live.ID, "data_app-draft-1",
		api.DataAppFile{Path: "App.tsx", Content: "old\n"},
		api.DataAppFile{Path: "gone.tsx", Content: "G\n"})
	signInApp(t, f)

	root := appFolderBoundTo(t, f, live.ID, map[string]string{
		"App.tsx":  "old\n",
		"gone.tsx": "G\n",
	})
	writeLocal(t, root, "App.tsx", "mine\n")
	writeLocal(t, root, "lib/chart.tsx", "export const Chart = () => <div/>;\n")
	// Removed locally, so the push deletes it on the server.
	if err := os.Remove(filepath.Join(root, "gone.tsx")); err != nil {
		t.Fatal(err)
	}

	if _, err := runCLI(t, root, "app", "push"); err != nil {
		t.Fatalf("push: %v", err)
	}

	// A file the baseline knows: asserted at the hash it recorded, NOT at what
	// the folder is about to write. Asserting the new content would pass only
	// on a server that already had it.
	edited := f.writesFor("App.tsx")
	if len(edited) != 1 || edited[0].BaseSha256 == nil {
		t.Fatalf("App.tsx writes = %+v, want one carrying a precondition", edited)
	}
	if want := wfdir.HashString("old\n"); *edited[0].BaseSha256 != want {
		t.Errorf("App.tsx baseSha256 = %q, want the baseline's %q", *edited[0].BaseSha256, want)
	}

	// A file the baseline has never seen: must not already exist.
	added := f.writesFor("lib/chart.tsx")
	if len(added) != 1 || added[0].BaseSha256 == nil {
		t.Fatalf("lib/chart.tsx writes = %+v, want one carrying a precondition", added)
	}
	if *added[0].BaseSha256 != "" {
		t.Errorf("lib/chart.tsx baseSha256 = %q, want \"\"", *added[0].BaseSha256)
	}

	// The DELETE asserts what it is removing.
	removed := f.writesFor("gone.tsx")
	if len(removed) != 1 || removed[0].Method != "DELETE" || removed[0].BaseSha256 == nil {
		t.Fatalf("gone.tsx writes = %+v, want one DELETE carrying a precondition", removed)
	}
	if want := wfdir.HashString("G\n"); *removed[0].BaseSha256 != want {
		t.Errorf("gone.tsx baseSha256 = %q, want the baseline's %q", *removed[0].BaseSha256, want)
	}
}

// The race the drift guard cannot see: the file listing was clean, and somebody
// wrote to the draft in the window before this push's own PUT. The write is
// refused server-side, the file keeps THEIR content, and the push stops.
//
// The conflicting file is deliberately not the entrypoint: an app push writes
// the entrypoint LAST, so a conflict on a non-entrypoint file is also the case
// where "did it carry on afterwards?" has an answer.
func TestAppPushStopsWhenAFileMovesUnderItMidPush(t *testing.T) {
	f := newFakeAppInstance(t)
	live := f.AddApp(&api.DataApp{ID: "data_app-1"})
	draft := f.AddDraft(live.ID, "data_app-draft-1",
		api.DataAppFile{Path: "App.tsx", Content: "old\n"},
		api.DataAppFile{Path: "lib/chart.tsx", Content: "C\n"})
	signInApp(t, f)

	root := appFolderBoundTo(t, f, live.ID, map[string]string{
		"App.tsx":       "old\n",
		"lib/chart.tsx": "C\n",
	})
	writeLocal(t, root, "App.tsx", "mine\n")
	writeLocal(t, root, "lib/chart.tsx", "also mine\n")

	// A colleague's web-builder save, landing between the listing this push
	// compared against and its own first write.
	f.beforeFileWrite = func(method, path string) {
		if method == "PUT" && path == "lib/chart.tsx" {
			f.beforeFileWrite = nil
			f.putFile(draft.ID, "lib/chart.tsx", "theirs\n")
		}
	}

	var out string
	var err error
	narration := captureStderr(t, func() {
		out, err = runCLI(t, root, "app", "push", "--json")
	})
	if err == nil {
		t.Fatal("push overwrote a file that changed under it")
	}
	if !strings.Contains(err.Error(), "lib/chart.tsx") {
		t.Errorf("error = %v, want it to name the file", err)
	}
	// The narration is the actionable half, and it is on stderr so a --json
	// caller's stdout stays one parseable object.
	for _, want := range []string{"lib/chart.tsx", "app status", "--force"} {
		if !strings.Contains(narration, want) {
			t.Errorf("stderr did not mention %q:\n%s", want, narration)
		}
	}
	if got := f.FileContents(draft.ID)["lib/chart.tsx"]; got != "theirs\n" {
		t.Errorf("lib/chart.tsx = %q — the refused write landed anyway", got)
	}
	// Stopped rather than carried on: the entrypoint is written last, so it is
	// still the server's old copy.
	if got := f.FileContents(draft.ID)["App.tsx"]; got != "old\n" {
		t.Errorf("App.tsx = %q — the push continued past the conflict", got)
	}
	// One attempt. A retry (or an automatic --force) would be a second write.
	if writes := f.writesFor("lib/chart.tsx"); len(writes) != 1 {
		t.Errorf("lib/chart.tsx was written %d times, want exactly one attempt: %+v", len(writes), writes)
	}

	// A structured signal, not prose. A caller that has to tell "somebody got
	// there first" from "this file was rejected" by substring-matching the
	// error breaks the day the server rewords it.
	payload := decodeJSON(t, out)
	if payload["conflict"] != true {
		t.Errorf("conflict = %v, want true", payload["conflict"])
	}
	if payload["error"] == nil {
		t.Error("payload carries no error field")
	}

	// The baseline written beside that report must not claim the colleague's
	// content: acknowledging it would disarm the drift guard against the very
	// change the server just refused to let us overwrite, and the next plain
	// push would destroy it without a word.
	state, loadErr := wfdir.LoadState(root)
	if loadErr != nil {
		t.Fatalf("load state: %v", loadErr)
	}
	if got := state.For(f.Key()).Files["lib/chart.tsx"].SHA256; got == wfdir.HashString("theirs\n") {
		t.Error("the baseline acknowledged content this push never wrote")
	}
}

// A refused DELETE stops the push the same way, and says something different:
// its 409 has two causes the CLI cannot tell apart (the content moved, or
// somebody deleted it first), and only the first leaves anything behind.
func TestAppPushStopsWhenAFileToDeleteMovesUnderIt(t *testing.T) {
	f := newFakeAppInstance(t)
	live := f.AddApp(&api.DataApp{ID: "data_app-1"})
	draft := f.AddDraft(live.ID, "data_app-draft-1",
		api.DataAppFile{Path: "App.tsx", Content: "A\n"},
		api.DataAppFile{Path: "gone.tsx", Content: "G\n"})
	signInApp(t, f)

	root := appFolderBoundTo(t, f, live.ID, map[string]string{
		"App.tsx":  "A\n",
		"gone.tsx": "G\n",
	})
	if err := os.Remove(filepath.Join(root, "gone.tsx")); err != nil {
		t.Fatal(err)
	}

	f.beforeFileWrite = func(method, path string) {
		if method == "DELETE" && path == "gone.tsx" {
			f.beforeFileWrite = nil
			f.putFile(draft.ID, "gone.tsx", "they kept editing it\n")
		}
	}

	var out string
	var err error
	narration := captureStderr(t, func() {
		out, err = runCLI(t, root, "app", "push", "--json")
	})
	if err == nil {
		t.Fatal("push deleted a file that changed under it")
	}
	if got := f.FileContents(draft.ID)["gone.tsx"]; got != "they kept editing it\n" {
		t.Errorf("gone.tsx = %q — the refused delete happened anyway", got)
	}
	if !strings.Contains(narration, "this push did not delete it") {
		t.Errorf("a refused DELETE needs its own wording, got:\n%s", narration)
	}
	if payload := decodeJSON(t, out); payload["conflict"] != true {
		t.Errorf("conflict = %v, want true", payload["conflict"])
	}
}

// The suppression rule. Each of these is a path where the drift guard was
// deliberately bypassed, and in each the baseline demonstrably does not
// describe the row — so a precondition built from it could only ever produce a
// 409 for a difference the push was already told to proceed past.
//
// Not merely assertions about the wire: the fake ENFORCES preconditions, so a
// regression that stopped suppressing them fails the push itself.
func TestAppPushSuppressesPreconditionsExactlyWhereTheDriftGuardIsBypassed(t *testing.T) {
	assertNoPreconditions := func(t *testing.T, f *fakeAppInstance) {
		t.Helper()
		if len(f.fileWrites) == 0 {
			t.Fatal("nothing was written, so the assertion proves nothing")
		}
		for _, w := range f.fileWrites {
			if w.BaseSha256 != nil {
				t.Errorf("%s %s carried baseSha256=%q, want none", w.Method, w.Path, *w.BaseSha256)
			}
		}
	}

	// --force means "overwrite the remote with what I have". Sending the
	// baseline's hashes would 409 in precisely the case the flag exists to
	// override.
	t.Run("--force over real drift", func(t *testing.T) {
		f := newFakeAppInstance(t)
		live := f.AddApp(&api.DataApp{ID: "data_app-1"})
		draft := f.AddDraft(live.ID, "data_app-draft-1",
			api.DataAppFile{Path: "App.tsx", Content: "changed in the web builder\n"})
		signInApp(t, f)

		root := appFolderBoundTo(t, f, live.ID, map[string]string{"App.tsx": "what I last synced\n"})
		writeLocal(t, root, "App.tsx", "mine\n")

		captureStderr(t, func() {
			if _, err := runCLI(t, root, "app", "push", "--force"); err != nil {
				t.Fatalf("--force push: %v", err)
			}
		})
		assertNoPreconditions(t, f)
		if got := f.FileContents(draft.ID)["App.tsx"]; got != "mine\n" {
			t.Errorf("--force should have overwritten the draft, got %q", got)
		}
	})

	// --force with a CLEAN baseline, which is the subtler half: the only
	// difference a precondition could still catch is one that lands AFTER the
	// drift check read the listing — the same race, arriving a little later,
	// and answering it with a 409 whose advice is "run --force" is a flag that
	// no longer does anything.
	t.Run("--force over a clean baseline", func(t *testing.T) {
		f := newFakeAppInstance(t)
		live := f.AddApp(&api.DataApp{ID: "data_app-1"})
		f.AddDraft(live.ID, "data_app-draft-1",
			api.DataAppFile{Path: "App.tsx", Content: "old\n"})
		signInApp(t, f)

		root := appFolderBoundTo(t, f, live.ID, map[string]string{"App.tsx": "old\n"})
		writeLocal(t, root, "App.tsx", "mine\n")

		captureStderr(t, func() {
			if _, err := runCLI(t, root, "app", "push", "--force"); err != nil {
				t.Fatalf("--force push: %v", err)
			}
		})
		assertNoPreconditions(t, f)
	})

	// A push that CREATED the app. There is no baseline at all and nothing on
	// the server is older than this push, so there is nothing anyone could have
	// changed underneath it — the drift guard is skipped entirely and so are
	// the preconditions.
	//
	// Unlike the workflow's equivalent this is a WIRE assertion rather than a
	// safety one: rdataapp.Create returns an EMPTY row (no seeded entrypoint —
	// see the fake's serveCreate), so a "" precondition here would happen to
	// pass rather than 409 on the CLI's own seconds-old app. It is pinned
	// anyway, because a first push has nothing to assert and should look
	// exactly like the request it was before preconditions existed.
	t.Run("a first push", func(t *testing.T) {
		f := newFakeAppInstance(t)
		signInApp(t, f)

		root := writeAppFolder(t, t.TempDir(), &wfdir.Manifest{Title: "Revenue explorer"},
			map[string]string{"App.tsx": "export default function App(){ return <div/>; }"})
		m := appManifestOf(t, root)
		m.SetBinding(f.Key(), wfdir.Binding{FeatureID: "feat-1"})
		if err := wfdir.SaveManifest(root, m); err != nil {
			t.Fatal(err)
		}

		if _, err := runCLI(t, root, "app", "push"); err != nil {
			t.Fatalf("first push: %v", err)
		}
		assertNoPreconditions(t, f)
	})
}

// The fresh-checkout path — no local baseline, the committed head anchor
// vouching — is the one place the preconditions come from the REMOTE listing
// rather than from a baseline. It is also the one place they are ARMED where
// the pre-anchor CLI required --force, so this is strictly a tightening.
func TestAppPushAnchoredByHeadArmsPreconditionsFromTheRemoteListing(t *testing.T) {
	f := newFakeAppInstance(t)
	f.AddApp(&api.DataApp{ID: "app-1", Lifecycle: api.LifecycleLive},
		api.DataAppFile{Path: "App.tsx", Content: "published\n"})
	f.versions["app-1"] = []api.DataApp{{ID: "dav-1"}}
	signInApp(t, f)

	out, err := runCLI(t, t.TempDir(), "app", "clone", "app-1", "--stack", "dev", "--json")
	if err != nil {
		t.Fatalf("clone: %v", err)
	}
	root := decodeJSON(t, out)["root"].(string)
	writeLocal(t, root, "App.tsx", "mine\n")
	// What `git clone` looks like: the lock's anchor survives, the git-ignored
	// .ronja/ baseline does not.
	freshCheckout(t, root)

	captureStderr(t, func() {
		if _, err := runCLI(t, root, "app", "push", "--no-validate"); err != nil {
			t.Fatalf("anchored push: %v", err)
		}
	})

	writes := f.writesFor("App.tsx")
	if len(writes) != 1 || writes[0].BaseSha256 == nil {
		t.Fatalf("App.tsx writes = %+v, want one carrying a precondition", writes)
	}
	// The REMOTE listing's hash, which is the live row's content the anchor
	// just vouched for — not "" (the folder is not creating this file) and not
	// the local content. Note the listing is read from the freshly-forked
	// DRAFT, which a checkout seeds byte-identical to live.
	if want := wfdir.HashString("published\n"); *writes[0].BaseSha256 != want {
		t.Errorf("App.tsx baseSha256 = %q, want the remote listing's %q", *writes[0].BaseSha256, want)
	}
}
