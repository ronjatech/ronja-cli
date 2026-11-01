package commands

import (
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/ronjatech/ronja-cli/internal/api"
	"github.com/ronjatech/ronja-cli/internal/wfdir"
)

func sameDir(t *testing.T, a, b string) bool {
	t.Helper()
	ai, err := os.Stat(a)
	if err != nil {
		return false
	}
	bi, err := os.Stat(b)
	if err != nil {
		return false
	}
	return os.SameFile(ai, bi)
}

func assertWorkdirUntouched(t *testing.T, dir string, keep ...string) {
	t.Helper()
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	allowed := map[string]bool{}
	for _, k := range keep {
		allowed[k] = true
	}
	for _, e := range entries {
		if !allowed[e.Name()] {
			t.Errorf("init wrote %s into the working directory", e.Name())
		}
	}
}

func TestWorkflowInitIntoADirectory(t *testing.T) {
	f := newFakeInstance(t)
	signIn(t, f)
	dir := t.TempDir()

	out, err := runCLI(t, dir, "wf", "init", "ga4", "--feature", "feat-1", "--json")
	if err != nil {
		t.Fatalf("init: %v", err)
	}
	target := filepath.Join(dir, "ga4")
	for _, name := range []string{wfdir.ManifestName, "main.py", ".ronja"} {
		if _, err := os.Stat(filepath.Join(target, name)); err != nil {
			t.Errorf("%s not written inside the named directory: %v", name, err)
		}
	}
	assertWorkdirUntouched(t, dir, "ga4")
	if root, _ := decodeJSON(t, out)["root"].(string); !sameDir(t, root, target) {
		t.Errorf("reported root = %q, want %s", root, target)
	}
}

func TestWorkflowInitIntoADirectoryRefusesAnExistingManifestAndCreatesNothing(t *testing.T) {
	f := newFakeInstance(t)
	signIn(t, f)
	dir := t.TempDir()
	target := filepath.Join(dir, "ga4")
	if _, err := runCLI(t, dir, "wf", "init", target, "--feature", "feat-1"); err != nil {
		t.Fatalf("first init: %v", err)
	}
	if _, err := runCLI(t, dir, "wf", "init", target, "--feature", "feat-1"); err == nil {
		t.Fatal("a second init into the same directory was accepted")
	}

	missing := filepath.Join(dir, "nested", "ga4")
	if err := os.MkdirAll(filepath.Join(dir, "nested"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "nested", wfdir.ManifestName), []byte("{}"), 0o644); err != nil {
		t.Fatal(err)
	}
	f.featureStatus["feat-foreign"] = http.StatusNotFound
	if _, err := runCLI(t, dir, "wf", "init", missing, "--feature", "feat-foreign"); err == nil {
		t.Fatal("init accepted a feature this credential cannot reach")
	}
	if _, err := os.Stat(missing); err == nil {
		t.Errorf("a refused init created %s", missing)
	}
}

func TestWorkflowInitFromResolvesAgainstTheWorkingDirectory(t *testing.T) {
	f := newFakeInstance(t)
	signIn(t, f)
	dir := t.TempDir()
	if err := os.MkdirAll(filepath.Join(dir, "scripts"), 0o755); err != nil {
		t.Fatal(err)
	}
	src := "print('report')\n"
	if err := os.WriteFile(filepath.Join(dir, "scripts", "report.py"), []byte(src), 0o644); err != nil {
		t.Fatal(err)
	}

	out, err := runCLI(t, dir, "wf", "init", "ga4", "--from", "scripts/report.py", "--feature", "feat-1", "--json")
	if err != nil {
		t.Fatalf("init: %v", err)
	}
	if got := readFile(t, filepath.Join(dir, "ga4"), "main.py"); got != src {
		t.Errorf("main.py = %q, want the --from script", got)
	}
	if w, ok := decodeJSON(t, out)["warnings"]; ok && strings.Contains(strings.Join(toStrings(w), " "), "still in this folder") {
		t.Errorf("a source outside the new folder was reported as inside it: %v", w)
	}
}

func TestWorkflowInitWarnsWhenTheSourceIsInsideTheNamedDirectory(t *testing.T) {
	f := newFakeInstance(t)
	signIn(t, f)
	dir := t.TempDir()
	if err := os.MkdirAll(filepath.Join(dir, "ga4"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "ga4", "report.py"), []byte("x = 1\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	out, err := runCLI(t, dir, "wf", "init", "ga4", "--from", "ga4/report.py", "--feature", "feat-1", "--json")
	if err != nil {
		t.Fatalf("init: %v", err)
	}
	w := strings.Join(toStrings(decodeJSON(t, out)["warnings"]), " ")
	if !strings.Contains(w, "report.py is still in this folder") {
		t.Errorf("no warning that the source sits inside the new folder: %q", w)
	}
}

func TestAppInitIntoADirectory(t *testing.T) {
	f := newFakeAppInstance(t)
	signInApp(t, f)
	dir := t.TempDir()

	out, err := runCLI(t, dir, "app", "init", "dash", "--feature", "feat-1", "--json")
	if err != nil {
		t.Fatalf("init: %v", err)
	}
	target := filepath.Join(dir, "dash")
	for _, name := range []string{wfdir.ManifestName, "App.tsx", ".ronja"} {
		if _, err := os.Stat(filepath.Join(target, name)); err != nil {
			t.Errorf("%s not written inside the named directory: %v", name, err)
		}
	}
	assertWorkdirUntouched(t, dir, "dash")
	if root, _ := decodeJSON(t, out)["root"].(string); !sameDir(t, root, target) {
		t.Errorf("reported root = %q, want %s", root, target)
	}
}

func TestAppInitIntoADirectoryRefusesAndCreatesNothing(t *testing.T) {
	f := newFakeAppInstance(t)
	signInApp(t, f)
	dir := t.TempDir()

	existing := filepath.Join(dir, "dash")
	if _, err := runCLI(t, dir, "app", "init", existing, "--feature", "feat-1"); err != nil {
		t.Fatalf("first init: %v", err)
	}
	if _, err := runCLI(t, dir, "app", "init", existing, "--feature", "feat-1"); err == nil {
		t.Fatal("a second init into the same directory was accepted")
	}

	f.featureStatus["feat-foreign"] = http.StatusBadRequest
	missing := filepath.Join(dir, "other")
	if _, err := runCLI(t, dir, "app", "init", missing, "--feature", "feat-foreign"); err == nil {
		t.Fatal("app init accepted a feature this credential cannot reach")
	}
	if _, err := os.Stat(missing); err == nil {
		t.Errorf("a refused init created %s", missing)
	}
}

func TestAppInitFromResolvesAgainstTheWorkingDirectory(t *testing.T) {
	f := newFakeAppInstance(t)
	signInApp(t, f)
	dir := t.TempDir()
	src := "export default function App() { return null }\n"
	if err := os.WriteFile(filepath.Join(dir, "Revenue.tsx"), []byte(src), 0o644); err != nil {
		t.Fatal(err)
	}

	if _, err := runCLI(t, dir, "app", "init", "dash", "--from", "Revenue.tsx", "--feature", "feat-1"); err != nil {
		t.Fatalf("init: %v", err)
	}
	if got := readFile(t, filepath.Join(dir, "dash"), "App.tsx"); got != src {
		t.Errorf("App.tsx = %q, want the --from component", got)
	}
}

func assertNotCreated(t *testing.T, path string) {
	t.Helper()
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Errorf("a refused init created %s", path)
	}
}

func TestWorkflowInitIntoADirectoryRefusesBeforeCreatingIt(t *testing.T) {
	t.Run("missing --from", func(t *testing.T) {
		f := newFakeInstance(t)
		signIn(t, f)
		dir := t.TempDir()
		if _, err := runCLI(t, dir, "wf", "init", "ga4", "--from", "nope.py", "--feature", "feat-1"); err == nil {
			t.Fatal("init accepted a --from that does not exist")
		}
		assertNotCreated(t, filepath.Join(dir, "ga4"))
	})
	t.Run("bad --stack", func(t *testing.T) {
		f := newFakeInstance(t)
		signIn(t, f)
		dir := t.TempDir()
		if _, err := runCLI(t, dir, "wf", "init", "ga4", "--stack", "bad name", "--feature", "feat-1"); err == nil {
			t.Fatal("init accepted an invalid stack name")
		}
		assertNotCreated(t, filepath.Join(dir, "ga4"))
	})
	t.Run("empty starter", func(t *testing.T) {
		f := newFakeInstance(t)
		f.defaults = &api.WorkflowDefaults{RuntimeVersion: wfdir.RuntimeQuery, EntrypointStarter: "  \n"}
		signIn(t, f)
		dir := t.TempDir()
		_, err := runCLI(t, dir, "wf", "init", "ga4", "--feature", "feat-1")
		if err == nil || !strings.Contains(err.Error(), "sent no starter main.py") {
			t.Fatalf("init wrote an empty main.py, or said nothing about it: %v", err)
		}
		assertNotCreated(t, filepath.Join(dir, "ga4"))
	})
}

func TestAppInitIntoADirectoryRefusesBeforeCreatingIt(t *testing.T) {
	t.Run("missing --from", func(t *testing.T) {
		f := newFakeAppInstance(t)
		signInApp(t, f)
		dir := t.TempDir()
		if _, err := runCLI(t, dir, "app", "init", "dash", "--from", "Nope.tsx", "--feature", "feat-1"); err == nil {
			t.Fatal("app init accepted a --from that does not exist")
		}
		assertNotCreated(t, filepath.Join(dir, "dash"))
	})
	t.Run("bad --stack", func(t *testing.T) {
		f := newFakeAppInstance(t)
		signInApp(t, f)
		dir := t.TempDir()
		if _, err := runCLI(t, dir, "app", "init", "dash", "--stack", "bad name", "--feature", "feat-1"); err == nil {
			t.Fatal("app init accepted an invalid stack name")
		}
		assertNotCreated(t, filepath.Join(dir, "dash"))
	})
}
