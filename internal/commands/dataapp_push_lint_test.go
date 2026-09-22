package commands

import (
	"net/http"
	"slices"
	"strings"
	"testing"

	"github.com/ronjatech/ronja-cli/internal/api"
	"github.com/ronjatech/ronja-cli/internal/wfdir"
)

// dataapp_push_lint_test.go — the write-time lint on `ronja app push`: the
// warnings the last clean file write returned land on the result and the
// report, and every way they must NOT — a set that does not compile, a push
// that stopped part-way, a file the push then deleted.

// lintWarningsFolder is the two-file folder the lint-warning tests push: a
// kit component and the entrypoint that imports it, written in that order
// (the entrypoint goes last — TestAppPushWritesEntrypointLast).
func lintWarningsFolder(t *testing.T, f *fakeAppInstance) string {
	t.Helper()
	root := writeAppFolder(t, t.TempDir(),
		&wfdir.Manifest{Title: "App"},
		map[string]string{"App.tsx": "entry", "lib/chart.tsx": "chart"})
	m := appManifestOf(t, root)
	m.SetBinding(f.Key(), wfdir.Binding{FeatureID: "feat-1"})
	if err := wfdir.SaveManifest(root, m); err != nil {
		t.Fatal(err)
	}
	return root
}

// The two lint answers the fake stages, shaped as the server shapes them: an
// intermediate set-wide finding against the entrypoint (line 0 — the kit file
// landed before the entrypoint that imports the theme), then the final set's
// one located finding.
var (
	lintWarningIntermediate = api.CompileDiagnostic{
		File: "App.tsx", Line: 0, Column: 0,
		Message: "the kit's classes (bg-card, text-muted-foreground, …) have nothing to compile them, so the app renders unstyled",
	}
	lintWarningFinal = api.CompileDiagnostic{
		File: "lib/chart.tsx", Line: 12, Column: 4,
		Message: "layout.title is stripped by the chart theme, so this chart renders with no heading",
	}
)

// TestAppPushCarriesTheLastWritesLintWarnings pins the --json half of the
// write-time lint: the warnings the server returns beside a CLEAN compile land
// on the result, they are the LAST write's and only the last write's, and they
// are not a verdict — Compiles stays true and the exit code stays zero.
//
// "Last" is the rule that matters. Every write lints the whole set as it
// stands, so the kit file's write — made before the entrypoint that imports the
// theme — reports a finding that the entrypoint's write then answers. Merging
// the two would keep alive a warning about a set that no longer exists.
func TestAppPushCarriesTheLastWritesLintWarnings(t *testing.T) {
	f := newFakeAppInstance(t)
	signInApp(t, f)
	f.lintWarnings["lib/chart.tsx"] = []api.CompileDiagnostic{lintWarningIntermediate}
	f.lintWarnings["App.tsx"] = []api.CompileDiagnostic{lintWarningFinal}
	root := lintWarningsFolder(t, f)

	out, err := runCLI(t, root, "app", "push", "--json")
	if err != nil {
		t.Fatalf("a warning is not a verdict, so the push must exit zero: %v", err)
	}
	var result appPushResult
	decodeJSONInto(t, out, &result)
	if result.Compiles == nil || !*result.Compiles {
		t.Errorf("a warning must not change the verdict, got compiles=%+v", result.Compiles)
	}
	if len(result.Pushed) != 2 {
		t.Errorf("both files should have been pushed, got %v", result.Pushed)
	}
	if !slices.Equal(result.Warnings, []api.CompileDiagnostic{lintWarningFinal}) {
		t.Errorf("the LAST write's warnings alone should land, got %+v", result.Warnings)
	}
}

// TestAppPushPrintsLintWarningsUnderTheVerdict pins the human half: a
// `Warnings:` block between `Compiles: yes` and `Next:`, one `file:line:
// message` per finding with `:line` omitted for a set-wide finding (line 0 —
// `App.tsx:0:` would send the reader to a line that does not exist), and the
// sentence that says these are advisory.
func TestAppPushPrintsLintWarningsUnderTheVerdict(t *testing.T) {
	f := newFakeAppInstance(t)
	signInApp(t, f)
	// Both findings on the final write, so the report has a located and a
	// set-wide line to render.
	f.lintWarnings["App.tsx"] = []api.CompileDiagnostic{lintWarningIntermediate, lintWarningFinal}
	root := lintWarningsFolder(t, f)

	out, err := runCLI(t, root, "app", "push")
	if err != nil {
		t.Fatalf("a warning is not a verdict, so the push must exit zero: %v", err)
	}
	verdict := strings.Index(out, "Compiles: yes")
	block := strings.Index(out, "  Warnings:\n")
	next := strings.Index(out, "Next: ronja app publish")
	if verdict < 0 || block < 0 || next < 0 || !(verdict < block && block < next) {
		t.Fatalf("expected Compiles: yes, then Warnings:, then Next:, got:\n%s", out)
	}
	for _, want := range []string{
		"    App.tsx: the kit's classes (bg-card, text-muted-foreground, …) have nothing to compile them, so the app renders unstyled\n",
		"    lib/chart.tsx:12: layout.title is stripped by the chart theme, so this chart renders with no heading\n",
		"fix them before publishing",
		"do not change the verdict",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("expected %q in the report, got:\n%s", want, out)
		}
	}
	if strings.Contains(out, "App.tsx:0:") {
		t.Errorf("a set-wide finding must not be printed at line 0:\n%s", out)
	}
}

// TestAppPushOmitsWarningsWhenTheLintIsClean pins the absent half: a clean
// answer leaves `warnings` out of --json entirely rather than emitting `[]`,
// and a push whose only change is a DELETE prints none — a DELETE is not
// linted server-side, so there is no answer to carry.
func TestAppPushOmitsWarningsWhenTheLintIsClean(t *testing.T) {
	t.Run("a clean write", func(t *testing.T) {
		f := newFakeAppInstance(t)
		signInApp(t, f)
		root := lintWarningsFolder(t, f)

		out, err := runCLI(t, root, "app", "push", "--json")
		if err != nil {
			t.Fatalf("push: %v", err)
		}
		var raw map[string]any
		decodeJSONInto(t, out, &raw)
		if _, present := raw["warnings"]; present {
			t.Errorf("a clean lint should omit the key, got %v", raw["warnings"])
		}
	})

	t.Run("a delete-only push", func(t *testing.T) {
		f := newFakeAppInstance(t)
		live := f.AddApp(&api.DataApp{ID: "data_app-1"})
		draft := f.AddDraft(live.ID, "data_app-draft-1",
			api.DataAppFile{Path: "App.tsx", Content: "entry"},
			api.DataAppFile{Path: "lib/old.tsx", Content: "old"})
		signInApp(t, f)
		// Staged against the file the push does NOT write, to show that a
		// warning the draft was carrying has no route out through a delete.
		f.lintWarnings["App.tsx"] = []api.CompileDiagnostic{lintWarningFinal}

		root := writeAppFolder(t, t.TempDir(), &wfdir.Manifest{Title: live.Name},
			map[string]string{"App.tsx": "entry"})
		m := appManifestOf(t, root)
		m.SetBinding(f.Key(), wfdir.Binding{DataAppID: live.ID, FeatureID: "feat-1"})
		if err := wfdir.SaveManifest(root, m); err != nil {
			t.Fatal(err)
		}
		state := &wfdir.State{}
		state.Set(f.Key(), baselineFromApp(aliasCodec{}, draft, []api.DataAppFile{
			{Path: "App.tsx", Content: "entry"},
			{Path: "lib/old.tsx", Content: "old"},
		}))
		if err := wfdir.SaveState(root, state); err != nil {
			t.Fatal(err)
		}

		out, err := runCLI(t, root, "app", "push")
		if err != nil {
			t.Fatalf("push: %v", err)
		}
		if !strings.Contains(out, "deleted  lib/old.tsx") {
			t.Fatalf("expected the delete to be the push's one change, got:\n%s", out)
		}
		if strings.Contains(out, "Warnings:") {
			t.Errorf("a delete-only push has no lint answer to print:\n%s", out)
		}
	})
}

// lintWarningsDraftFolder is the folder the lint-warning tests that need a
// DELETE push: a live app whose draft holds App.tsx and lib/old.tsx, a folder
// holding only an EDITED App.tsx, and a baseline that already describes the
// draft — so the push writes the entrypoint, deletes lib/old.tsx, and nothing
// else.
func lintWarningsDraftFolder(t *testing.T, f *fakeAppInstance) string {
	t.Helper()
	live := f.AddApp(&api.DataApp{ID: "data_app-1"})
	draft := f.AddDraft(live.ID, "data_app-draft-1",
		api.DataAppFile{Path: "App.tsx", Content: "entry"},
		api.DataAppFile{Path: "lib/old.tsx", Content: "old"})
	root := writeAppFolder(t, t.TempDir(), &wfdir.Manifest{Title: live.Name},
		map[string]string{"App.tsx": "entry, edited"})
	m := appManifestOf(t, root)
	m.SetBinding(f.Key(), wfdir.Binding{DataAppID: live.ID, FeatureID: "feat-1"})
	if err := wfdir.SaveManifest(root, m); err != nil {
		t.Fatal(err)
	}
	state := &wfdir.State{}
	state.Set(f.Key(), baselineFromApp(aliasCodec{}, draft, []api.DataAppFile{
		{Path: "App.tsx", Content: "entry"},
		{Path: "lib/old.tsx", Content: "old"},
	}))
	if err := wfdir.SaveState(root, state); err != nil {
		t.Fatal(err)
	}
	return root
}

// assertNoWarnings pins the absent half of --json: the key is left out, not
// emitted as `[]` or `null`.
func assertNoWarnings(t *testing.T, out string) {
	t.Helper()
	var raw map[string]any
	decodeJSONInto(t, out, &raw)
	if got, present := raw["warnings"]; present {
		t.Errorf("warnings should be absent, got %v\n%s", got, out)
	}
}

// TestAppPushDropsWarningsWhenTheSetDoesNotCompile pins the invariant that
// --json and the human report cannot describe different things: a lint about
// a set that does not compile is dropped from BOTH, whichever way the "does not
// compile" arrives. Three arrivals, and the first two reach the verdict switch
// through different arms — a 400 refusal, and a 200 whose validated_at is nil
// — which is why the nil-out is one predicate after the switch rather than a
// line in each arm.
func TestAppPushDropsWarningsWhenTheSetDoesNotCompile(t *testing.T) {
	t.Run("the validate refuses", func(t *testing.T) {
		f := newFakeAppInstance(t)
		signInApp(t, f)
		f.lintWarnings["App.tsx"] = []api.CompileDiagnostic{lintWarningFinal}
		f.validateFails = "Unexpected end of file"
		root := lintWarningsFolder(t, f)

		out, err := runCLI(t, root, "app", "push", "--json")
		if err == nil {
			t.Fatal("a push whose result does not compile must exit non-zero")
		}
		var result appPushResult
		decodeJSONInto(t, out, &result)
		if result.Compiles == nil || *result.Compiles {
			t.Fatalf("expected compiles=false, got %+v", result.Compiles)
		}
		assertNoWarnings(t, out)
	})

	t.Run("the validate answers 200 without a stamp", func(t *testing.T) {
		f := newFakeAppInstance(t)
		signInApp(t, f)
		f.lintWarnings["App.tsx"] = []api.CompileDiagnostic{lintWarningFinal}
		f.validateUnstamped = true
		root := lintWarningsFolder(t, f)

		out, err := runCLI(t, root, "app", "push", "--json")
		if err == nil {
			t.Fatal("a push whose result does not compile must exit non-zero")
		}
		var result appPushResult
		decodeJSONInto(t, out, &result)
		if result.Compiles == nil || *result.Compiles {
			t.Fatalf("expected compiles=false, got %+v", result.Compiles)
		}
		assertNoWarnings(t, out)
	})

	// The last write's own failure: the server lints on the success path only,
	// so a compile-failing LAST write overwrites the earlier write's warnings
	// with nothing rather than leaving them standing.
	t.Run("the last write does not compile", func(t *testing.T) {
		f := newFakeAppInstance(t)
		signInApp(t, f)
		f.lintWarnings["lib/chart.tsx"] = []api.CompileDiagnostic{lintWarningIntermediate}
		f.compileFails["App.tsx"] = "Unexpected end of file"
		root := lintWarningsFolder(t, f)

		out, _ := runCLI(t, root, "app", "push", "--json")
		assertNoWarnings(t, out)
	})

	t.Run("the human report prints none under Compiles: NO", func(t *testing.T) {
		f := newFakeAppInstance(t)
		signInApp(t, f)
		f.lintWarnings["App.tsx"] = []api.CompileDiagnostic{lintWarningFinal}
		f.validateFails = "Unexpected end of file"
		root := lintWarningsFolder(t, f)

		out, _ := runCLI(t, root, "app", "push")
		if !strings.Contains(out, "Compiles: NO") {
			t.Fatalf("expected Compiles: NO, got:\n%s", out)
		}
		if strings.Contains(out, "Warnings:") {
			t.Errorf("a set that does not compile has no lint worth printing:\n%s", out)
		}
	})
}

// TestAppPushDropsWarningsWhenThePushStopsPartWay pins the other absence: a
// push that did not complete has no complete set for a lint to have
// described, whether it stopped on a write or — after every write landed
// clean — on a delete, which leaves the server holding a set the last write's
// lint did not look at. Both are definite rejections, so the CLI KNOWS the set
// is not the folder's.
func TestAppPushDropsWarningsWhenThePushStopsPartWay(t *testing.T) {
	t.Run("a rejected write", func(t *testing.T) {
		f := newFakeAppInstance(t)
		signInApp(t, f)
		// The kit file's write lands with a warning; the entrypoint's is refused.
		f.lintWarnings["lib/chart.tsx"] = []api.CompileDiagnostic{lintWarningIntermediate}
		f.failPut["App.tsx"] = http.StatusForbidden
		root := lintWarningsFolder(t, f)

		out, err := runCLI(t, root, "app", "push", "--json")
		if err == nil {
			t.Fatal("a rejected write must fail the push")
		}
		var result appPushResult
		decodeJSONInto(t, out, &result)
		if result.Error == "" {
			t.Fatalf("expected the stop to be reported, got %+v", result)
		}
		assertNoWarnings(t, out)
	})

	t.Run("a rejected delete after a clean write", func(t *testing.T) {
		f := newFakeAppInstance(t)
		signInApp(t, f)
		root := lintWarningsDraftFolder(t, f)
		f.lintWarnings["App.tsx"] = []api.CompileDiagnostic{lintWarningFinal}
		f.failDelete["lib/old.tsx"] = http.StatusForbidden

		out, err := runCLI(t, root, "app", "push", "--json")
		if err == nil {
			t.Fatal("a rejected delete must fail the push")
		}
		var result appPushResult
		decodeJSONInto(t, out, &result)
		if !slices.Contains(result.Pushed, "App.tsx") || result.Error == "" {
			t.Fatalf("expected the write to have landed and the delete to be the stop, got %+v", result)
		}
		assertNoWarnings(t, out)
	})
}

// TestAppPushKeepsWarningsWhenTheCompilerDidNotAnswer pins the one no-verdict
// case where the lint STAYS: every byte landed and nothing later changed the
// set, so the last write's answer still describes the draft — the instance
// merely never said whether it compiles. Compiles is nil, and the warnings are
// printed under the "not known" verdict.
func TestAppPushKeepsWarningsWhenTheCompilerDidNotAnswer(t *testing.T) {
	f := newFakeAppInstance(t)
	signInApp(t, f)
	f.lintWarnings["App.tsx"] = []api.CompileDiagnostic{lintWarningFinal}
	f.validateFails = "Internal server error"
	f.validateStatus = http.StatusInternalServerError
	root := lintWarningsFolder(t, f)

	out, err := runCLI(t, root, "app", "push", "--json")
	if err == nil {
		t.Fatal("a push whose compile check never answered must exit non-zero")
	}
	var result appPushResult
	decodeJSONInto(t, out, &result)
	if result.Compiles != nil || result.CompileCheck == nil {
		t.Fatalf("expected no verdict, got compiles=%+v check=%+v", result.Compiles, result.CompileCheck)
	}
	if !slices.Equal(result.Warnings, []api.CompileDiagnostic{lintWarningFinal}) {
		t.Errorf("the set landed, so its lint should stay, got %+v", result.Warnings)
	}

	f2 := newFakeAppInstance(t)
	signInApp(t, f2)
	f2.lintWarnings["App.tsx"] = []api.CompileDiagnostic{lintWarningFinal}
	f2.validateFails = "Internal server error"
	f2.validateStatus = http.StatusInternalServerError
	root2 := lintWarningsFolder(t, f2)

	out2, _ := runCLI(t, root2, "app", "push")
	verdict := strings.Index(out2, "Compiles: not known")
	block := strings.Index(out2, "  Warnings:\n")
	if verdict < 0 || block < 0 || verdict > block {
		t.Fatalf("expected Compiles: not known, then Warnings:, got:\n%s", out2)
	}
}

// TestAppPushDropsWarningsWhenTheValidateStops is the sibling of the
// no-answer case above for the arm that STOPS: a validate refused outright (a
// 403, say — not a compile refusal, not an outage) ends the push with an
// error and no verdict, the human report prints no `Warnings:` block, and
// --json must not carry one either.
func TestAppPushDropsWarningsWhenTheValidateStops(t *testing.T) {
	f := newFakeAppInstance(t)
	signInApp(t, f)
	f.lintWarnings["App.tsx"] = []api.CompileDiagnostic{lintWarningFinal}
	f.validateFails = "forbidden"
	f.validateStatus = http.StatusForbidden
	root := lintWarningsFolder(t, f)

	out, err := runCLI(t, root, "app", "push", "--json")
	if err == nil {
		t.Fatal("a validate that is refused must fail the push")
	}
	var result appPushResult
	decodeJSONInto(t, out, &result)
	if result.Compiles != nil || result.CompileCheck != nil || result.Error == "" {
		t.Fatalf("expected a stop with no verdict, got %+v", result)
	}
	assertNoWarnings(t, out)

	f2 := newFakeAppInstance(t)
	signInApp(t, f2)
	f2.lintWarnings["App.tsx"] = []api.CompileDiagnostic{lintWarningFinal}
	f2.validateFails = "forbidden"
	f2.validateStatus = http.StatusForbidden
	root2 := lintWarningsFolder(t, f2)

	out2, _ := runCLI(t, root2, "app", "push")
	if strings.Contains(out2, "Warnings:") {
		t.Errorf("a stopped push has no lint worth printing:\n%s", out2)
	}
}

// TestAppPushDropsWarningsAboutFilesItDeleted pins the delete's own stale
// finding: the lint runs over the files map, not the bundle graph, so an
// orphan the last write still reached is linted and THEN deleted — and a
// report that printed its line under `Compiles: yes` would name a file the
// draft no longer holds. Only that file's findings go; the rest describe files
// that are still there, still as linted.
func TestAppPushDropsWarningsAboutFilesItDeleted(t *testing.T) {
	f := newFakeAppInstance(t)
	signInApp(t, f)
	root := lintWarningsDraftFolder(t, f)
	aboutOrphan := api.CompileDiagnostic{
		File: "lib/old.tsx", Line: 3, Column: 1,
		Message: "layout.title is stripped by the chart theme, so this chart renders with no heading",
	}
	f.lintWarnings["App.tsx"] = []api.CompileDiagnostic{aboutOrphan, lintWarningIntermediate}

	out, err := runCLI(t, root, "app", "push", "--json")
	if err != nil {
		t.Fatalf("a warning is not a verdict, so the push must exit zero: %v\n%s", err, out)
	}
	var result appPushResult
	decodeJSONInto(t, out, &result)
	if !slices.Equal(result.Deleted, []string{"lib/old.tsx"}) {
		t.Fatalf("expected the orphan to be deleted, got %+v", result.Deleted)
	}
	if !slices.Equal(result.Warnings, []api.CompileDiagnostic{lintWarningIntermediate}) {
		t.Errorf("the deleted file's finding should go and the other stay, got %+v", result.Warnings)
	}
}

// TestFormatLintWarning pins the three shapes: located, set-wide (line 0, no
// `:0:`), and a finding with no file at all, which is the message alone.
func TestFormatLintWarning(t *testing.T) {
	cases := []struct {
		name string
		in   api.CompileDiagnostic
		want string
	}{
		{"located", api.CompileDiagnostic{File: "lib/chart.tsx", Line: 12, Column: 4, Message: "no heading"}, "lib/chart.tsx:12: no heading"},
		{"set-wide", api.CompileDiagnostic{File: "App.tsx", Line: 0, Message: "unstyled"}, "App.tsx: unstyled"},
		{"no file", api.CompileDiagnostic{Line: 7, Message: "nothing compiles these classes"}, "nothing compiles these classes"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := formatLintWarning(tc.in); got != tc.want {
				t.Errorf("formatLintWarning(%+v) = %q, want %q", tc.in, got, tc.want)
			}
		})
	}
}
