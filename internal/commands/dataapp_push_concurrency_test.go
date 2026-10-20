package commands

// `ronja app push`'s per-file compare-and-swap preconditions, which close the
// window between the drift guard's listing and each write. The fake enforces
// them (fakeAppInstance.refusePrecondition), so a wrong hash fails the push
// itself.

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/ronjatech/ronja-cli/internal/api"
	"github.com/ronjatech/ronja-cli/internal/wfdir"
)

// Every write asserts the baseline's hash; a path the baseline has never seen
// asserts an empty hash, meaning it must not exist yet.
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
	if err := os.Remove(filepath.Join(root, "gone.tsx")); err != nil {
		t.Fatal(err)
	}

	if _, err := runCLI(t, root, "app", "push"); err != nil {
		t.Fatalf("push: %v", err)
	}

	// Asserted at the recorded hash, not the content about to be written.
	edited := f.writesFor("App.tsx")
	if len(edited) != 1 || edited[0].BaseSha256 == nil {
		t.Fatalf("App.tsx writes = %+v, want one carrying a precondition", edited)
	}
	if want := wfdir.HashString("old\n"); *edited[0].BaseSha256 != want {
		t.Errorf("App.tsx baseSha256 = %q, want the baseline's %q", *edited[0].BaseSha256, want)
	}

	added := f.writesFor("lib/chart.tsx")
	if len(added) != 1 || added[0].BaseSha256 == nil {
		t.Fatalf("lib/chart.tsx writes = %+v, want one carrying a precondition", added)
	}
	if *added[0].BaseSha256 != "" {
		t.Errorf("lib/chart.tsx baseSha256 = %q, want \"\"", *added[0].BaseSha256)
	}

	removed := f.writesFor("gone.tsx")
	if len(removed) != 1 || removed[0].Method != "DELETE" || removed[0].BaseSha256 == nil {
		t.Fatalf("gone.tsx writes = %+v, want one DELETE carrying a precondition", removed)
	}
	if want := wfdir.HashString("G\n"); *removed[0].BaseSha256 != want {
		t.Errorf("gone.tsx baseSha256 = %q, want the baseline's %q", *removed[0].BaseSha256, want)
	}
}

// The conflicting file is deliberately not the entrypoint: the entrypoint is
// written last, so its content shows whether the push carried on.
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

	// A colleague's save landing after the drift check's listing.
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
	// Narration on stderr, so --json stdout stays one object.
	for _, want := range []string{"lib/chart.tsx", "app status", "--force"} {
		if !strings.Contains(narration, want) {
			t.Errorf("stderr did not mention %q:\n%s", want, narration)
		}
	}
	if got := f.FileContents(draft.ID)["lib/chart.tsx"]; got != "theirs\n" {
		t.Errorf("lib/chart.tsx = %q — the refused write landed anyway", got)
	}
	if got := f.FileContents(draft.ID)["App.tsx"]; got != "old\n" {
		t.Errorf("App.tsx = %q — the push continued past the conflict", got)
	}
	if writes := f.writesFor("lib/chart.tsx"); len(writes) != 1 {
		t.Errorf("lib/chart.tsx was written %d times, want exactly one attempt: %+v", len(writes), writes)
	}

	// A structured signal, so callers need not substring-match the error.
	payload := decodeJSON(t, out)
	if payload["conflict"] != true {
		t.Errorf("conflict = %v, want true", payload["conflict"])
	}
	if payload["error"] == nil {
		t.Error("payload carries no error field")
	}

	// Acknowledging the colleague's content in the baseline would disarm the
	// drift guard, and the next plain push would destroy it.
	state, loadErr := wfdir.LoadState(root)
	if loadErr != nil {
		t.Fatalf("load state: %v", loadErr)
	}
	if got := state.For(f.Key()).Files["lib/chart.tsx"].SHA256; got == wfdir.HashString("theirs\n") {
		t.Error("the baseline acknowledged content this push never wrote")
	}
}

// A refused DELETE needs its own wording: its 409 means the content moved or
// someone deleted it first, and the CLI cannot tell which.
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

// Where the drift guard is deliberately bypassed the baseline does not describe
// the row, so a precondition from it could only 409 on a difference the push
// was told to proceed past.
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

	// The subtler half: a 409 here would advise "--force" to a push already
	// forcing.
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

	// A wire assertion, not a safety one: a created app has no files (see the
	// fake's serveCreate), so an empty-hash precondition would pass anyway.
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

// On a fresh checkout (no local baseline, the head anchor vouching) the
// preconditions come from the remote listing instead.
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
	// As after `git clone`: the lock's anchor survives, the git-ignored .ronja/
	// baseline does not.
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
	// The listing comes from the freshly forked draft, seeded byte-identical
	// to live.
	if want := wfdir.HashString("published\n"); *writes[0].BaseSha256 != want {
		t.Errorf("App.tsx baseSha256 = %q, want the remote listing's %q", *writes[0].BaseSha256, want)
	}
}
