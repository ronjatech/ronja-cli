package commands

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/ronjatech/ronja-cli/internal/api"
	"github.com/ronjatech/ronja-cli/internal/wfdir"
)

const (
	fakeJSStarter     = "export async function main(params, env) {\n  return null\n}\n"
	fakeJSPackageJSON = "{\n  \"private\": true,\n  \"dependencies\": {}\n}\n"
)

// stageJSDefaults makes the fake an instance that creates JavaScript workflows.
func stageJSDefaults(f *fakeInstance) {
	f.jsDefaults = &api.WorkflowDefaults{
		Language:          wfdir.LanguageJS,
		RuntimeVersion:    wfdir.RuntimeMain,
		Entrypoint:        "main.js",
		EntrypointStarter: fakeJSStarter,
		PackageJSON:       fakeJSPackageJSON,
	}
}

// initJSFolder runs `wf init --language js` in a fresh directory.
func initJSFolder(t *testing.T, f *fakeInstance) string {
	t.Helper()
	dir := t.TempDir()
	if _, err := runCLI(t, dir, "wf", "init", "--feature", "feat-1", "--language", "js", "--json"); err != nil {
		t.Fatalf("init --language js: %v", err)
	}
	return dir
}

func TestInitLanguageJSWritesMainJSAndPackageJSON(t *testing.T) {
	f := newFakeInstance(t)
	stageJSDefaults(f)
	signIn(t, f)
	dir := t.TempDir()

	out, err := runCLI(t, dir, "wf", "init", "--feature", "feat-1", "--language", "js", "--json")
	if err != nil {
		t.Fatalf("init: %v", err)
	}
	payload := decodeJSON(t, out)
	if payload["language"] != "js" || payload["entrypoint"] != "main.js" || payload["runtime"] != float64(wfdir.RuntimeMain) {
		t.Errorf("reported %v / %v / %v, want js / main.js / 4", payload["language"], payload["entrypoint"], payload["runtime"])
	}
	if got := readFile(t, dir, "main.js"); got != fakeJSStarter {
		t.Errorf("main.js = %q, want the instance's starter", got)
	}
	if got := readFile(t, dir, "package.json"); got != fakeJSPackageJSON {
		t.Errorf("package.json = %q, want the instance's", got)
	}
	if _, err := os.Stat(filepath.Join(dir, "main.py")); !os.IsNotExist(err) {
		t.Error("a JavaScript folder was given a main.py")
	}
	if len(f.defaultsAsked) != 1 || f.defaultsAsked[0] != "language=js" {
		t.Errorf("defaults asked with %q, want one request for language=js", f.defaultsAsked)
	}

	m, err := wfdir.LoadManifest(dir, wfdir.WorkflowKind)
	if err != nil {
		t.Fatalf("the folder init wrote does not load: %v", err)
	}
	if m.Language != wfdir.LanguageJS || m.Entrypoint != "main.js" || m.Runtime != wfdir.RuntimeMain {
		t.Errorf("manifest language/entrypoint/runtime = %q/%q/%d, want js/main.js/4", m.Language, m.Entrypoint, m.Runtime)
	}
	raw := readFile(t, dir, wfdir.ManifestName)
	if !strings.HasPrefix(raw, "{\n  \"formatVersion\": 4,") || !strings.Contains(raw, `"language": "js"`) {
		t.Errorf("a js manifest must lead with formatVersion 4 and carry its language:\n%s", raw)
	}
}

// Python is the absent key: an explicit --language python writes exactly the
// manifest no flag writes.
func TestInitLanguagePythonWritesTheManifestOfNoFlag(t *testing.T) {
	f := newFakeInstance(t)
	signIn(t, f)
	plain, explicit := t.TempDir(), t.TempDir()
	if _, err := runCLI(t, plain, "wf", "init", "--feature", "feat-1", "--title", "x"); err != nil {
		t.Fatalf("init: %v", err)
	}
	if _, err := runCLI(t, explicit, "wf", "init", "--feature", "feat-1", "--title", "x", "--language", "python"); err != nil {
		t.Fatalf("init --language python: %v", err)
	}
	a, b := readFile(t, plain, wfdir.ManifestName), readFile(t, explicit, wfdir.ManifestName)
	if a != b {
		t.Errorf("--language python changed the manifest:\n%s\nvs\n%s", a, b)
	}
	if strings.Contains(a, "language") || strings.Contains(a, "formatVersion") {
		t.Errorf("a python manifest must carry neither language nor formatVersion:\n%s", a)
	}
}

// An instance that predates JavaScript workflows ignores ?language and answers
// its Python defaults. Scaffolding those as main.js would be a Python starter
// in a JavaScript folder, so init refuses before writing anything.
func TestInitLanguageJSRefusesAnInstanceWithoutJavaScript(t *testing.T) {
	f := newFakeInstance(t)
	signIn(t, f)
	dir := t.TempDir()

	_, err := runCLI(t, dir, "wf", "init", "--feature", "feat-1", "--language", "js")
	if err == nil || !strings.Contains(err.Error(), "does not create JavaScript workflows") {
		t.Fatalf("init = %v, want a refusal naming the instance's missing JavaScript", err)
	}
	for _, name := range []string{wfdir.ManifestName, "main.js", "main.py", "package.json"} {
		if _, err := os.Stat(filepath.Join(dir, name)); !os.IsNotExist(err) {
			t.Errorf("%s was written despite the refusal", name)
		}
	}
}

func TestInitRefusesABadLanguageOrRuntime(t *testing.T) {
	for _, tc := range []struct {
		name string
		args []string
		want string
	}{
		{"unknown language", []string{"--language", "ts"}, `--language must be "python" or "js"`},
		{"js on runtime 3", []string{"--language", "js", "--runtime", "3"}, "runs on runtime 4 only"},
		{"js from a python file", []string{"--language", "js", "--from", "script.py"}, "is not a .js or .mjs file"},
		{"python from a js file", []string{"--from", "script.js"}, "pass --language js"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newFakeInstance(t)
			stageJSDefaults(f)
			signIn(t, f)
			dir := t.TempDir()
			args := append([]string{"wf", "init", "--feature", "feat-1"}, tc.args...)
			_, err := runCLI(t, dir, args...)
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("init %v = %v, want %q", tc.args, err, tc.want)
			}
			if len(f.defaultsAsked) != 0 {
				t.Errorf("asked the instance %q before refusing a flag", f.defaultsAsked)
			}
		})
	}
}

// The first push of a js folder creates a js workflow and validates it as one;
// a python push still sends no language, so its bodies are what every instance
// has always read.
func TestPushSendsLanguageOnlyForJS(t *testing.T) {
	f := newFakeInstance(t)
	stageJSDefaults(f)
	signIn(t, f)
	dir := initJSFolder(t, f)

	out, err := runCLI(t, dir, "wf", "push", "--json")
	if err != nil {
		t.Fatalf("push: %v", err)
	}
	if len(f.created) != 1 {
		t.Fatalf("created %d workflows, want 1", len(f.created))
	}
	if got := f.created[0]; got.Language != wfdir.LanguageJS || got.Entrypoint != "main.js" || got.RuntimeVersion != wfdir.RuntimeMain {
		t.Errorf("create body = %+v, want language js on main.js, runtime 4", got)
	}
	if len(f.validated) == 0 || f.validated[0].Language != wfdir.LanguageJS {
		t.Errorf("validate bodies = %+v, want language js", f.validated)
	}
	id := decodeJSON(t, out)["workflowID"].(string)
	if got := f.FileContents(id); got["main.js"] != fakeJSStarter || got["package.json"] != fakeJSPackageJSON {
		t.Errorf("pushed files = %+v", got)
	}

	py := newFakeInstance(t)
	signIn(t, py)
	root := initFolder(t, py, map[string]string{"main.py": "print('hi')\n"})
	if _, err := runCLI(t, root, "wf", "push", "--json"); err != nil {
		t.Fatalf("python push: %v", err)
	}
	if len(py.created) != 1 || py.created[0].Language != "" || py.validated[0].Language != "" {
		t.Errorf("a python push sent a language: create %+v, validate %+v", py.created, py.validated)
	}
}

// A js folder never pushes what a Node project grows beside its source.
func TestPushOfAJSFolderSkipsNodeModulesDistAndBuild(t *testing.T) {
	f := newFakeInstance(t)
	stageJSDefaults(f)
	signIn(t, f)
	dir := initJSFolder(t, f)
	writeLocal(t, dir, "lib/util.mjs", "export const x = 1\n")
	writeLocal(t, dir, "package-lock.json", "{}\n")
	writeLocal(t, dir, "node_modules/lodash/index.js", "module.exports = {}\n")
	writeLocal(t, dir, "dist/bundle.js", "x\n")
	writeLocal(t, dir, "build/out.js", "x\n")

	out, err := runCLI(t, dir, "wf", "push", "--json")
	if err != nil {
		t.Fatalf("push: %v", err)
	}
	got := f.FileContents(decodeJSON(t, out)["workflowID"].(string))
	for _, want := range []string{"main.js", "package.json", "package-lock.json", "lib/util.mjs"} {
		if _, ok := got[want]; !ok {
			t.Errorf("%s was not pushed: %v", want, got)
		}
	}
	for path := range got {
		for _, skipped := range []string{"node_modules/", "dist/", "build/"} {
			if strings.HasPrefix(path, skipped) {
				t.Errorf("pushed %s", path)
			}
		}
	}
}

// A workflow's language is fixed at create, so a folder bound to a row of the
// other language is refused before anything is written.
func TestPushRefusesAFolderOfTheOtherLanguage(t *testing.T) {
	f := newFakeInstance(t)
	stageJSDefaults(f)
	signIn(t, f)
	f.AddWorkflow(&api.Workflow{ID: "wf-py", Lifecycle: api.LifecycleLive, Title: "Monthly report", Entrypoint: "main.py", FeatureID: "feat-1", RuntimeVersion: wfdir.RuntimeMain},
		api.WorkflowFile{WorkflowID: "wf-py", Path: "main.py", Content: "print(1)\n"})
	dir := initJSFolder(t, f)
	m, err := wfdir.LoadManifest(dir, wfdir.WorkflowKind)
	if err != nil {
		t.Fatal(err)
	}
	m.Instances[0].WorkflowID = "wf-py"
	if err := wfdir.SaveManifest(dir, m); err != nil {
		t.Fatal(err)
	}

	_, err = runCLI(t, dir, "wf", "push", "--json")
	if err == nil || !strings.Contains(err.Error(), "is a python workflow on the server") {
		t.Fatalf("push = %v, want a language refusal", err)
	}
	if got := f.FileContents("wf-py"); got["main.py"] != "print(1)\n" || len(got) != 1 {
		t.Errorf("the python row's files changed: %v", got)
	}

	// status says so before a push is tried.
	out, err := runCLI(t, dir, "wf", "status", "--json")
	if err != nil {
		t.Fatalf("status: %v", err)
	}
	if !strings.Contains(out, "is a python workflow on the server") {
		t.Errorf("status did not report the language mismatch:\n%s", out)
	}
}

// A loose main.js is far more often an ordinary Node project than a workflow,
// so `ronja context` says nothing about workflows there.
func TestContextIsSilentAboutALooseMainJS(t *testing.T) {
	f := newFakeInstance(t)
	signIn(t, f)
	dir := t.TempDir()
	writeLocal(t, dir, "main.js", fakeJSStarter)

	out, err := runCLI(t, dir, "context", "--no-docs")
	if err != nil {
		t.Fatalf("context: %v", err)
	}
	if strings.Contains(out, "wf init") {
		t.Errorf("a loose main.js produced a workflow suggestion:\n%s", out)
	}
}
