package commands

import (
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/ronjatech/ronja-cli/internal/api"
	"github.com/ronjatech/ronja-cli/internal/useragent"
	"github.com/ronjatech/ronja-cli/internal/wfdir"
)

// `wf init` asks the instance (GET /workflow/defaults) for the runtime a new
// workflow is created on and its starter, pins the runtime and writes the
// starter, so the first push creates on the runtime the starter was written for
// whatever the instance's default has become by then.

// assertNoFolder checks a refused init left the directory as it found it.
func assertNoFolder(t *testing.T, dir string) {
	t.Helper()
	for _, name := range []string{wfdir.ManifestName, "main.py"} {
		if _, err := os.Stat(filepath.Join(dir, name)); !os.IsNotExist(err) {
			t.Errorf("%s was written despite the refusal", name)
		}
	}
}

func TestInitPinsTheInstancesDefaultRuntimeAndStarter(t *testing.T) {
	for _, runtime := range []int{wfdir.RuntimeQuery, wfdir.RuntimeMain} {
		t.Run(fmt.Sprint(runtime), func(t *testing.T) {
			f := newFakeInstance(t)
			starter := fmt.Sprintf("# the instance's starter for runtime %d\n", runtime)
			f.defaults = &api.WorkflowDefaults{RuntimeVersion: runtime, EntrypointStarter: starter}
			signIn(t, f)
			dir := t.TempDir()

			out, err := runCLI(t, dir, "wf", "init", "--feature", "feat-1", "--json")
			if err != nil {
				t.Fatalf("init: %v", err)
			}
			if got := decodeJSON(t, out)["runtime"]; got != float64(runtime) {
				t.Errorf("reported runtime = %v, want %d", got, runtime)
			}
			m, err := wfdir.LoadManifest(dir, wfdir.WorkflowKind)
			if err != nil {
				t.Fatalf("the folder init wrote does not load: %v", err)
			}
			if m.Runtime != runtime {
				t.Errorf("manifest runtime = %d, want the instance's %d pinned", m.Runtime, runtime)
			}
			if got := readFile(t, dir, "main.py"); got != starter {
				t.Errorf("main.py = %q, want the instance's starter %q", got, starter)
			}
			// The default is asked for by leaving the parameter OUT: the server
			// refuses runtime=0.
			if len(f.defaultsAsked) != 1 || f.defaultsAsked[0] != "" {
				t.Errorf("defaults asked with %q, want one request with no query", f.defaultsAsked)
			}
			// The request carried the version an instance counts adoption by.
			if want := useragent.For(resolveVersion()); f.defaultsUserAgent != want || !strings.HasPrefix(want, "ronja-cli/") {
				t.Errorf("User-Agent = %q, want %q (ronja-cli/<version>)", f.defaultsUserAgent, want)
			}
		})
	}
}

// An instance that predates the route answers the miss of GET /workflow/:id
// with the id "defaults": a 404, or on older backends a 400 "no rows".
func TestInitRefusesAnInstanceWithoutTheDefaultsRoute(t *testing.T) {
	for _, tc := range []struct {
		name  string
		stage func(f *fakeInstance)
	}{
		{"404", func(f *fakeInstance) { f.defaults = nil }},
		{"400 no rows", func(f *fakeInstance) {
			f.failDefaults = http.StatusBadRequest
			f.failDefaultsMessage = "no rows"
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newFakeInstance(t)
			tc.stage(f)
			signIn(t, f)
			dir := t.TempDir()

			_, err := runCLI(t, dir, "wf", "init", "--feature", "feat-1")
			if err == nil {
				t.Fatal("init succeeded against an instance without GET /workflow/defaults")
			}
			if !strings.Contains(err.Error(), "has no GET /api/v2/workflow/defaults, so it predates this CLI") {
				t.Errorf("the refusal does not say the instance is too old: %v", err)
			}
			assertNoFolder(t, dir)
		})
	}
}

// Any other 400 is the instance's answer, not a sign of its age.
func TestInitDoesNotCallAnotherBadRequestAnOldInstance(t *testing.T) {
	f := newFakeInstance(t)
	f.failDefaults = http.StatusBadRequest
	f.failDefaultsMessage = "no rows for runtime 7"
	signIn(t, f)

	_, err := runCLI(t, t.TempDir(), "wf", "init", "--feature", "feat-1")
	if err == nil || strings.Contains(err.Error(), "predates this CLI") || !strings.Contains(err.Error(), "no rows for runtime 7") {
		t.Fatalf("init = %v, want the instance's 400 as it is", err)
	}
}

// A starter is a page of code written to disk as the instance sent it; one over
// the cap is refused before anything is written.
func TestInitRefusesAnOversizedStarter(t *testing.T) {
	f := newFakeInstance(t)
	f.defaults = &api.WorkflowDefaults{
		RuntimeVersion:    wfdir.RuntimeQuery,
		EntrypointStarter: strings.Repeat("# x\n", maxStarterBytes/4+1),
	}
	signIn(t, f)
	dir := t.TempDir()

	_, err := runCLI(t, dir, "wf", "init", "--feature", "feat-1")
	if err == nil || !strings.Contains(err.Error(), "over the 256 KiB this CLI writes") {
		t.Fatalf("init = %v, want a refusal of the oversized starter", err)
	}
	assertNoFolder(t, dir)
}

// A starter at the cap is written.
func TestInitWritesAStarterAtTheCap(t *testing.T) {
	f := newFakeInstance(t)
	starter := strings.Repeat("# x\n", maxStarterBytes/4)
	f.defaults = &api.WorkflowDefaults{RuntimeVersion: wfdir.RuntimeQuery, EntrypointStarter: starter}
	signIn(t, f)
	dir := t.TempDir()

	if _, err := runCLI(t, dir, "wf", "init", "--feature", "feat-1"); err != nil {
		t.Fatalf("init: %v", err)
	}
	if got := readFile(t, dir, "main.py"); got != starter {
		t.Errorf("main.py is %d bytes, want the %d-byte starter", len(got), len(starter))
	}
}

// Any other failure is init's failure, before a byte is written.
func TestInitFailsLoudlyWhenTheDefaultsReadFails(t *testing.T) {
	for _, status := range []int{http.StatusInternalServerError, http.StatusUnauthorized} {
		t.Run(fmt.Sprint(status), func(t *testing.T) {
			f := newFakeInstance(t)
			f.failDefaults = status
			signIn(t, f)
			dir := t.TempDir()

			_, err := runCLI(t, dir, "wf", "init", "--feature", "feat-1")
			if err == nil {
				t.Fatal("init succeeded over a failed defaults read")
			}
			if !strings.Contains(err.Error(), "which runtime a new workflow starts on") {
				t.Errorf("the failure does not say what was being asked: %v", err)
			}
			assertNoFolder(t, dir)
		})
	}
}

// An instance whose default is a runtime this build does not know is refused,
// not pinned: LoadManifest would refuse that folder on every later command.
func TestInitRefusesADefaultRuntimeThisBuildDoesNotKnow(t *testing.T) {
	f := newFakeInstance(t)
	f.defaults = &api.WorkflowDefaults{RuntimeVersion: 5, EntrypointStarter: "def main():\n    pass\n"}
	signIn(t, f)
	dir := t.TempDir()

	_, err := runCLI(t, dir, "wf", "init", "--feature", "feat-1")
	if err == nil {
		t.Fatal("init pinned a runtime this build cannot open")
	}
	if !strings.Contains(err.Error(), "run `ronja update`") {
		t.Errorf("the refusal does not say how to get a CLI that knows it: %v", err)
	}
	if strings.Contains(err.Error(), "3 or 4") {
		t.Errorf("the refusal offers runtimes by number: %v", err)
	}
	assertNoFolder(t, dir)
}

// A --runtime this build cannot create on is refused locally, without offering
// runtimes by number: which ones a new workflow can be created on is the
// instance's to say.
func TestInitRuntimeFlagThisBuildCannotCreateOnIsRefused(t *testing.T) {
	f := newFakeInstance(t)
	signIn(t, f)
	dir := t.TempDir()

	_, err := runCLI(t, dir, "wf", "init", "--feature", "feat-1", "--runtime", "5")
	if err == nil {
		t.Fatal("init accepted --runtime 5")
	}
	if want := "--runtime must name a runtime the instance creates new workflows on (got 5); omit it for the instance's default"; err.Error() != want {
		t.Errorf("refusal = %q, want %q", err, want)
	}
	if len(f.defaultsAsked) != 0 {
		t.Errorf("asked the instance %q before refusing locally", f.defaultsAsked)
	}
	assertNoFolder(t, dir)
}

// --runtime N asks the instance for N (?runtime=N) and writes N's starter — the
// instance's, never one this build carries.
func TestInitRuntimeFlagReadsThatRuntimesStarterFromTheInstance(t *testing.T) {
	for _, tc := range []struct{ flag, serverDefault int }{
		{wfdir.RuntimeQuery, wfdir.RuntimeMain},
		{wfdir.RuntimeMain, wfdir.RuntimeQuery},
	} {
		t.Run(fmt.Sprint(tc.flag), func(t *testing.T) {
			f := newFakeInstance(t)
			starter := fmt.Sprintf("# runtime %d, from the instance\n", tc.flag)
			f.defaults = &api.WorkflowDefaults{RuntimeVersion: tc.serverDefault, EntrypointStarter: "# the default's\n"}
			f.otherStarters = map[int]string{tc.flag: starter}
			signIn(t, f)
			dir := t.TempDir()

			if _, err := runCLI(t, dir, "wf", "init", "--feature", "feat-1", "--runtime", fmt.Sprint(tc.flag)); err != nil {
				t.Fatalf("init --runtime %d: %v", tc.flag, err)
			}
			m, err := wfdir.LoadManifest(dir, wfdir.WorkflowKind)
			if err != nil {
				t.Fatalf("load manifest: %v", err)
			}
			if m.Runtime != tc.flag {
				t.Errorf("manifest runtime = %d, want --runtime's %d", m.Runtime, tc.flag)
			}
			if got := readFile(t, dir, "main.py"); got != starter {
				t.Errorf("main.py = %q, want the instance's starter for runtime %d", got, tc.flag)
			}
			if want := fmt.Sprintf("runtime=%d", tc.flag); len(f.defaultsAsked) != 1 || f.defaultsAsked[0] != want {
				t.Errorf("defaults asked with %q, want [%q]", f.defaultsAsked, want)
			}
		})
	}
}

// A runtime the instance does not create on is its 400, shown as it is — the
// instance's sentence names the runtimes it does create on.
func TestInitRuntimeFlagTheInstanceDoesNotOfferIsItsRefusal(t *testing.T) {
	f := newFakeInstance(t)
	signIn(t, f)
	dir := t.TempDir()

	_, err := runCLI(t, dir, "wf", "init", "--feature", "feat-1", "--runtime", "4")
	if err == nil {
		t.Fatal("init pinned a runtime the instance does not create on")
	}
	if want := "a new workflow cannot be created on runtime 4; it can be created on runtime 3"; !strings.Contains(err.Error(), want) {
		t.Errorf("the instance's refusal was not surfaced verbatim (%q): %v", want, err)
	}
	assertNoFolder(t, dir)
}

// An instance that ignored ?runtime answers its default; pinning that would pin
// a runtime the author did not ask for.
func TestInitRefusesAnAnswerForAnotherRuntime(t *testing.T) {
	f := newFakeInstance(t)
	f.defaults = &api.WorkflowDefaults{RuntimeVersion: wfdir.RuntimeQuery, EntrypointStarter: fakeStarter}
	// The fake answers ?runtime=4 with runtime 3's row, as an instance ignoring
	// the parameter would.
	f.otherStarters = map[int]string{wfdir.RuntimeMain: fakeStarter}
	f.ignoreRuntimeParam = true
	signIn(t, f)
	dir := t.TempDir()

	_, err := runCLI(t, dir, "wf", "init", "--feature", "feat-1", "--runtime", "4")
	if err == nil || !strings.Contains(err.Error(), "asked") || !strings.Contains(err.Error(), "answered runtime 3") {
		t.Fatalf("init pinned an answer for a runtime it did not ask for: %v", err)
	}
	assertNoFolder(t, dir)
}

// An empty starter is refused only when it would be WRITTEN: --from brings the
// code, and an existing entrypoint is adopted, so neither needs one.
func TestInitEmptyStarterIsRefusedOnlyWhenItWouldBeWritten(t *testing.T) {
	empty := func(t *testing.T) *fakeInstance {
		f := newFakeInstance(t)
		f.defaults = &api.WorkflowDefaults{RuntimeVersion: wfdir.RuntimeQuery, EntrypointStarter: "  \n"}
		signIn(t, f)
		return f
	}

	t.Run("written", func(t *testing.T) {
		empty(t)
		dir := t.TempDir()
		_, err := runCLI(t, dir, "wf", "init", "--feature", "feat-1")
		if err == nil || !strings.Contains(err.Error(), "sent no starter main.py") {
			t.Fatalf("init wrote an empty main.py, or said nothing about it: %v", err)
		}
		assertNoFolder(t, dir)
	})
	t.Run("--from", func(t *testing.T) {
		empty(t)
		dir := t.TempDir()
		src := filepath.Join(t.TempDir(), "report.py")
		if err := os.WriteFile(src, []byte("print('mine')\n"), 0o644); err != nil {
			t.Fatal(err)
		}
		if _, err := runCLI(t, dir, "wf", "init", "--feature", "feat-1", "--from", src); err != nil {
			t.Fatalf("init --from refused over a starter it never writes: %v", err)
		}
		if got := readFile(t, dir, "main.py"); got != "print('mine')\n" {
			t.Errorf("main.py = %q, want the --from source", got)
		}
	})
	t.Run("adopting an existing main.py", func(t *testing.T) {
		empty(t)
		dir := t.TempDir()
		writeLocal(t, dir, "main.py", "print('already here')\n")
		if _, err := runCLI(t, dir, "wf", "init", "--feature", "feat-1"); err != nil {
			t.Fatalf("init refused to adopt an entrypoint over a starter it never writes: %v", err)
		}
	})
}

// Code init did not write from the starter (--from, or an adopted entrypoint)
// is still pinned to the instance's default, and the report says so: the
// author did not choose that runtime and the code was not written for it.
func TestInitSaysWhenThePinnedRuntimeIsNotWhatTheCodeWasWrittenFrom(t *testing.T) {
	const note = "this folder is pinned to runtime 3, the one"
	const notFromStarter = "main.py was not written from its starter"
	from := func(t *testing.T) []string {
		src := filepath.Join(t.TempDir(), "report.py")
		if err := os.WriteFile(src, []byte("print('mine')\n"), 0o644); err != nil {
			t.Fatal(err)
		}
		return []string{"--from", src}
	}
	for _, tc := range []struct {
		name              string
		args              func(t *testing.T, dir string) []string
		scaffolded, noted bool
	}{
		{"--from", func(t *testing.T, _ string) []string { return from(t) }, false, true},
		{"adopting an existing main.py", func(t *testing.T, dir string) []string {
			writeLocal(t, dir, "main.py", "print('already here')\n")
			return nil
		}, false, true},
		{"--from with --runtime named", func(t *testing.T, _ string) []string {
			return append(from(t), "--runtime", "3")
		}, false, false},
		{"the starter written", func(*testing.T, string) []string { return nil }, true, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newFakeInstance(t)
			signIn(t, f)

			dir := t.TempDir()
			args := append([]string{"wf", "init", "--feature", "feat-1", "--json"}, tc.args(t, dir)...)
			out, err := runCLI(t, dir, args...)
			if err != nil {
				t.Fatalf("init: %v", err)
			}
			payload := decodeJSON(t, out)
			if payload["runtime"] != float64(wfdir.RuntimeQuery) {
				t.Errorf("runtime = %v, want 3", payload["runtime"])
			}
			if payload["scaffolded"] != tc.scaffolded {
				t.Errorf("scaffolded = %v, want %v", payload["scaffolded"], tc.scaffolded)
			}
			warnings, _ := payload["warnings"].([]any)
			var joined []string
			for _, w := range warnings {
				joined = append(joined, fmt.Sprint(w))
			}
			got := strings.Join(joined, "\n")
			if noted := strings.Contains(got, note) && strings.Contains(got, notFromStarter); noted != tc.noted {
				t.Errorf("--json warnings carry the note = %v, want %v: %q", noted, tc.noted, got)
			}

			// The human report carries the same note.
			human := t.TempDir()
			humanArgs := append([]string{"wf", "init", "--feature", "feat-1"}, tc.args(t, human)...)
			var runErr error
			stderr := captureStderr(t, func() { _, runErr = runCLI(t, human, humanArgs...) })
			if runErr != nil {
				t.Fatalf("init (human): %v", runErr)
			}
			if noted := strings.Contains(stderr, note) && strings.Contains(stderr, notFromStarter); noted != tc.noted {
				t.Errorf("the human report carries the note = %v, want %v:\n%s", noted, tc.noted, stderr)
			}
		})
	}
}
