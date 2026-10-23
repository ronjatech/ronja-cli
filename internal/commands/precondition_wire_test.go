package commands

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/ronjatech/ronja-cli/internal/api"
	"github.com/ronjatech/ronja-cli/internal/wfdir"
)

// A file precondition (baseSha256) is compared by the server against the hash
// of the bytes it STORES — the wire form, ids in place of aliases
// (rworkflow/rdataapp store_concurrency.go). The drift guard vouches in DISK
// form, so a folder that declares dependencies used to assert a hash the server
// never held, and every armed push after the first was refused 409 as a change
// nobody made. These push a folder that uses an alias, edit the file keeping
// the alias, and push again: the second push must land, asserting the stored
// bytes' hash.

// TestAnAliasedWorkflowEditAssertsTheStoredBytes is the workflow loop.
func TestAnAliasedWorkflowEditAssertsTheStoredBytes(t *testing.T) {
	f := newFakeInstance(t)
	signIn(t, f)
	source := "read(\"{{ ref('orders') }}\")\n"
	root := aliasWorkflowFolder(t, f, source)
	if out, err := runCLI(t, root, "wf", "push", "--json"); err != nil {
		t.Fatalf("first push: %v\n%s", err, out)
	}
	id := workflowIDOf(t, root)

	for i, line := range []string{"# second\n", "# third\n"} {
		stored := f.FileContents(f.draftOrLive(id))["main.py"]
		source += line
		writeLocal(t, root, "main.py", source)
		f.fileWrites = nil
		if out, err := runCLI(t, root, "wf", "push", "--json"); err != nil {
			t.Fatalf("push %d after an edit that kept the alias: %v\n%s", i+2, err, out)
		}
		assertStoredPrecondition(t, f.writesFor("main.py"), stored, i+2)
	}
}

// TestAnAliasedAppEditAssertsTheStoredBytes is the data-app loop.
func TestAnAliasedAppEditAssertsTheStoredBytes(t *testing.T) {
	f := newFakeAppInstance(t)
	signInApp(t, f)
	source := "const q = \"{{ ref('orders') }}\";\n"
	root := aliasAppFolder(t, f, tableDep(), map[string]string{"orders": "table-orders"},
		api.DataAppAccess{}, map[string]string{"App.tsx": source})
	if out, err := runCLI(t, root, "app", "push", "--json"); err != nil {
		t.Fatalf("first push: %v\n%s", err, out)
	}

	for i, line := range []string{"// second\n", "// third\n"} {
		stored := f.FileContents(appDraftIDOf(t, root, f))["App.tsx"]
		source += line
		writeLocal(t, root, "App.tsx", source)
		f.fileWrites = nil
		if out, err := runCLI(t, root, "app", "push", "--json"); err != nil {
			t.Fatalf("push %d after an edit that kept the alias: %v\n%s", i+2, err, out)
		}
		assertStoredPrecondition(t, f.writesFor("App.tsx"), stored, i+2)
	}
}

// TestAnAliasedFolderAssertsTheStoredBytes is the same rule on an IMPORTED
// tree: a head-vouched fresh checkout first, then a baseline, each write
// asserting the stored bytes' hash.
func TestAnAliasedFolderAssertsTheStoredBytes(t *testing.T) {
	f := importFixture(t)
	dir, _ := runImport(t, f)
	if err := os.Mkdir(filepath.Join(dir, ".git"), 0o755); err != nil {
		t.Fatal(err)
	}
	root := filepath.Join(dir, "workflows/sales-report")
	for i, line := range []string{"# first", "# second"} {
		stored := f.wf.FileContents(f.wf.draftOrLive("workflow-report"))["main.py"]
		main := filepath.Join(root, "main.py")
		if err := os.WriteFile(main, []byte(readFile(t, main)+line+"\n"), 0o644); err != nil {
			t.Fatal(err)
		}
		f.wf.fileWrites = nil
		if out, err := runCLI(t, root, "wf", "push"); err != nil {
			t.Fatalf("push %d: %v\n%s", i+1, err, out)
		}
		assertStoredPrecondition(t, f.wf.writesFor("main.py"), stored, i+1)
	}
}

func assertStoredPrecondition(t *testing.T, writes []recordedFileWrite, stored string, push int) {
	t.Helper()
	if len(writes) != 1 || writes[0].BaseSha256 == nil {
		t.Fatalf("push %d wrote the file as %+v, want one write with a precondition", push, writes)
	}
	if got, want := *writes[0].BaseSha256, wfdir.HashString(stored); got != want {
		t.Fatalf("push %d asserted %s, want the stored bytes' hash %s", push, got, want)
	}
}

// draftOrLive is the row a push of a workflow writes to: the caller's open
// draft, else the row itself.
func (f *fakeInstance) draftOrLive(id string) string {
	if draft := f.draftOf[id]; draft != "" {
		return draft
	}
	return id
}
