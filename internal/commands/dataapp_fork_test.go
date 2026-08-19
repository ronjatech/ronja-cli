package commands

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/ronjatech/ronja-cli/internal/wfdir"
)

// The component-kit fork.
//
// Two properties carry the whole command, and both are invisible in the happy
// path's output: the file lands at EXACTLY the kit path (anywhere else shadows
// nothing, and a data app whose "customised" Button is never imported compiles
// perfectly and looks unchanged), and NOTHING is written on the server.

// kitButtonSource is a stand-in for a vendored kit component. Byte-exactness is
// the assertion, so it deliberately carries the things a careless copy loses: a
// trailing newline, a tab, and a non-ASCII character.
const kitButtonSource = "// Vendored from shadcn/ui — MIT.\nexport function Button() {\n\treturn null\n}\n"

// forkFolder lays out a bound data-app folder for the fork tests.
func forkFolder(t *testing.T, f *fakeAppInstance, files map[string]string) string {
	t.Helper()
	if files == nil {
		files = map[string]string{"App.tsx": "export default function App() { return null }"}
	}
	root := writeAppFolder(t, t.TempDir(), &wfdir.Manifest{Title: "Revenue explorer"}, files)
	m := appManifestOf(t, root)
	m.SetBinding(f.Key(), wfdir.Binding{DataAppID: "data_app-1", FeatureID: "feat-1"})
	if err := wfdir.SaveManifest(root, m); err != nil {
		t.Fatal(err)
	}
	return root
}

// TestAppForkWritesTheKitFileVerbatim is the happy path, and the byte-exactness
// is the point: the file has to be what the compiler currently resolves, or the
// fork silently starts from something else.
func TestAppForkWritesTheKitFileVerbatim(t *testing.T) {
	f := newFakeAppInstance(t)
	f.AddKitFile("components/ui/button.tsx", kitButtonSource)
	signInApp(t, f)
	root := forkFolder(t, f, nil)

	if _, err := runCLI(t, root, "app", "fork", "components/ui/button.tsx"); err != nil {
		t.Fatalf("fork: %v", err)
	}

	if got := readFile(t, root, "components", "ui", "button.tsx"); got != kitButtonSource {
		t.Errorf("forked file is not the kit source verbatim:\n%q\nwant\n%q", got, kitButtonSource)
	}
	// LOCAL-FIRST: the only request a fork may make is the docs read. Anything
	// else means a draft was opened or a row was touched by a command whose
	// entire promise is that it changes nothing.
	for _, req := range f.Requests {
		if req != "GET /docs/api/kit/components/ui/button.tsx" {
			t.Errorf("fork made a request it had no business making: %q (all: %v)", req, f.Requests)
		}
	}
	if f.checkouts != 0 {
		t.Errorf("fork checked out a draft (%d times) — it must change nothing on the server", f.checkouts)
	}
}

// The import alias is what is on screen when someone decides to fork a
// component, so it is accepted and stripped rather than refused.
func TestAppForkAcceptsTheImportAliasForm(t *testing.T) {
	f := newFakeAppInstance(t)
	f.AddKitFile("components/ui/button.tsx", kitButtonSource)
	signInApp(t, f)
	root := forkFolder(t, f, nil)

	if _, err := runCLI(t, root, "app", "fork", "@/components/ui/button.tsx"); err != nil {
		t.Fatalf("fork: %v", err)
	}
	if got := readFile(t, root, "components", "ui", "button.tsx"); got != kitButtonSource {
		t.Errorf("the alias form should land at the same path, got %q", got)
	}
}

// TestAppForkRefusesAnExistingFile: overwriting is the one failure with no
// undo. A fork already made is the author's own code, and a file that merely
// happens to sit at a kit path is being shadowed on purpose.
func TestAppForkRefusesAnExistingFile(t *testing.T) {
	f := newFakeAppInstance(t)
	f.AddKitFile("components/ui/button.tsx", kitButtonSource)
	signInApp(t, f)
	mine := "// mine, edited\nexport function Button() { return <b/> }\n"
	root := forkFolder(t, f, map[string]string{
		"App.tsx":                     "export default function App() { return null }",
		"components/ui/button.tsx":    mine,
		"components/ui/keep-me.tsx":   "export const x = 1\n",
		"components/operator/note.ts": "export const y = 2\n",
	})

	_, err := runCLI(t, root, "app", "fork", "components/ui/button.tsx")
	if err == nil {
		t.Fatal("fork over an existing file must fail")
	}
	if !strings.Contains(err.Error(), "already exists") {
		t.Errorf("the refusal should say the file is already there, got: %v", err)
	}
	if got := readFile(t, root, "components", "ui", "button.tsx"); got != mine {
		t.Errorf("the existing file was modified:\n%q", got)
	}
	// Refusals before requests: this one is knowable locally, and CI should not
	// pay a round trip to be told so.
	for _, req := range f.Requests {
		if strings.HasPrefix(req, "GET /docs/api/kit/") {
			t.Errorf("fork fetched the kit before checking the local file: %v", f.Requests)
		}
	}
}

// A path that is not in the kit answers with the plain-text /docs/api
// breadcrumb, which reads as "wrong host" if passed through untranslated.
func TestAppForkUnknownKitPathIsAClearError(t *testing.T) {
	f := newFakeAppInstance(t)
	f.AddKitFile("components/ui/button.tsx", kitButtonSource)
	// Modelling the real 404: the breadcrumb carries a near-miss suggestion
	// computed from the route table, and a one-letter typo is the likeliest way
	// anyone gets here at all.
	f.kitNotFoundBody = "404 page not found\n\nThis is the Ronja API. Start at /llms.txt\n" +
		"Did you mean:\n  GET /docs/api/kit/components/ui/button.tsx\n"
	signInApp(t, f)
	root := forkFolder(t, f, nil)

	_, err := runCLI(t, root, "app", "fork", "components/ui/buton.tsx")
	if err == nil {
		t.Fatal("forking a file that is not in the kit must fail")
	}
	message := err.Error()
	if !strings.Contains(message, "components/ui/buton.tsx") {
		t.Errorf("the error should name the path asked for, got: %v", err)
	}
	if !strings.Contains(message, "component kit") {
		t.Errorf("the error should say what was not found, got: %v", err)
	}
	if !strings.Contains(message, "components/ui/button.tsx") {
		t.Errorf("the near-miss suggestion from the breadcrumb should survive, got: %v", err)
	}
	if !strings.Contains(message, "/docs/api/kit.md") {
		t.Errorf("the error should point at the index, got: %v", err)
	}
	if _, statErr := os.Stat(filepath.Join(root, "components", "ui", "buton.tsx")); statErr == nil {
		t.Error("a failed fork must leave no file behind")
	}
}

// A kit path the folder could not hold is refused before the request, the same
// way clone refuses a server file set it cannot lay out.
func TestAppForkRefusesAPathTheFolderCannotHold(t *testing.T) {
	f := newFakeAppInstance(t)
	signInApp(t, f)
	root := forkFolder(t, f, nil)

	for _, arg := range []string{"../escape.tsx", "components/ui", "dist/button.tsx"} {
		if _, err := runCLI(t, root, "app", "fork", arg); err == nil {
			t.Errorf("fork %q should have been refused", arg)
		}
	}
	for _, req := range f.Requests {
		if strings.HasPrefix(req, "GET /docs/api/kit/") {
			t.Errorf("a locally-refusable path still cost a round trip: %v", f.Requests)
		}
	}
	if _, err := os.Stat(filepath.Join(filepath.Dir(root), "escape.tsx")); err == nil {
		t.Error("a traversal path escaped the folder")
	}
}

// --json is the agent path: the created path and where the bytes came from, on
// stdout, with nothing else in it.
func TestAppForkJSONReportsThePathAndSource(t *testing.T) {
	f := newFakeAppInstance(t)
	f.AddKitFile("lib/utils.ts", "export const cn = () => \"\"\n")
	signInApp(t, f)
	root := forkFolder(t, f, nil)

	out, err := runCLI(t, root, "app", "fork", "lib/utils.ts", "--json")
	if err != nil {
		t.Fatalf("fork: %v", err)
	}
	payload := decodeJSON(t, out)
	if payload["path"] != "lib/utils.ts" {
		t.Errorf("path = %v, want lib/utils.ts", payload["path"])
	}
	if payload["root"] != root {
		t.Errorf("root = %v, want %s", payload["root"], root)
	}
	if got, want := payload["source"], f.URL()+"/docs/api/kit/lib/utils.ts"; got != want {
		t.Errorf("source = %v, want %s", got, want)
	}
	if payload["bytes"] != float64(len("export const cn = () => \"\"\n")) {
		t.Errorf("bytes = %v", payload["bytes"])
	}
}
