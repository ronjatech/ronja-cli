package commands

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/ronjatech/ronja-cli/internal/wfdir"
)

// A table's health-checks file under `moved`: it is named by the table's stem
// and moves with it. The fixtures are addresses_test.go's.

// renameGLLines renames a seeded checks folder's gl_lines.sql to gl.sql and
// declares the move, leaving the checks file wherever the caller wants it.
func renameGLLines(t *testing.T, root string, renameChecks bool) {
	t.Helper()
	renames := [][2]string{{"gl_lines.sql", "gl.sql"}}
	if renameChecks {
		renames = append(renames, [2]string{"checks/gl_lines.json", "checks/gl.json"})
	}
	for _, r := range renames {
		if err := os.Rename(filepath.Join(root, r[0]), filepath.Join(root, r[1])); err != nil {
			t.Fatal(err)
		}
	}
	manifest := pipelineManifestOf(t, root)
	manifest.Moved = []wfdir.MovedEntry{{From: "gl_lines.sql", To: "gl.sql"}}
	if err := wfdir.SaveManifest(root, manifest); err != nil {
		t.Fatal(err)
	}
}

// A TABLE MOVED WITHOUT ITS CHECKS FILE is refused before anything is written.
// Applied, the checks record would be re-keyed to checks/gl.json, which nothing
// holds: the push would read it as a deleted checks file, call every check on
// the table an orphan, and suggest the --prune that silences them — while the
// file still declaring them, checks/gl_lines.json, is refused for naming no
// .sql file.
func TestPipelinePushRefusesATableMovedWithoutItsChecksFile(t *testing.T) {
	f := newFakePipelineInstance(t)
	signInPipeline(t, f)
	root := seedChecksFolder(t, f, twoChecks)
	if _, stderr, err := runPipelineCLI(t, root, "pipeline", "push"); err != nil {
		t.Fatalf("seed push: %v\n%s", err, stderr)
	}
	renameGLLines(t, root, false)
	manifestBefore := readBytes(t, wfdir.ManifestPath(root))
	stateBefore := readBytes(t, wfdir.StatePath(root))
	writesBefore := len(f.checkWrites())

	for _, args := range [][]string{{"pipeline", "push"}, {"pipeline", "push", "--prune"}} {
		_, stderr, err := runPipelineCLI(t, root, args...)
		if err == nil || !strings.Contains(err.Error(), "rename checks/gl_lines.json to checks/gl.json too") {
			t.Fatalf("%v: err = %v, want the checks-left-behind refusal\n%s", args, err, stderr)
		}
		if !bytes.Equal(manifestBefore, readBytes(t, wfdir.ManifestPath(root))) {
			t.Fatalf("%v: a refused move rewrote %s", args, wfdir.ManifestName)
		}
		if !bytes.Equal(stateBefore, readBytes(t, wfdir.StatePath(root))) {
			t.Fatalf("%v: a refused move rewrote the local baseline", args)
		}
	}
	if got := len(f.checkWrites()) - writesBefore; got != 0 {
		t.Errorf("a refused move wrote %d check(s)", got)
	}
	for _, name := range []string{"Unik rad per id", "Bygger om dagligen"} {
		if !f.checkNamed("table-gl", name).Enabled {
			t.Errorf("%q was silenced", name)
		}
	}
}

// A CHAIN names the END of the chain: with gl_lines.sql → gl.sql → ledger.sql
// declared and the checks file left at checks/gl_lines.json, renaming it to the
// chain's middle (checks/gl.json) would only be refused again on the next run.
func TestPipelinePushChecksLeftBehindInAChainNamesTheChainsEnd(t *testing.T) {
	f := newFakePipelineInstance(t)
	signInPipeline(t, f)
	root := seedChecksFolder(t, f, twoChecks)
	if _, stderr, err := runPipelineCLI(t, root, "pipeline", "push"); err != nil {
		t.Fatalf("seed push: %v\n%s", err, stderr)
	}
	if err := os.Rename(filepath.Join(root, "gl_lines.sql"), filepath.Join(root, "ledger.sql")); err != nil {
		t.Fatal(err)
	}
	manifest := pipelineManifestOf(t, root)
	manifest.Moved = []wfdir.MovedEntry{{From: "gl_lines.sql", To: "gl.sql"}, {From: "gl.sql", To: "ledger.sql"}}
	if err := wfdir.SaveManifest(root, manifest); err != nil {
		t.Fatal(err)
	}
	_, stderr, err := runPipelineCLI(t, root, "pipeline", "push")
	if err == nil || !strings.Contains(err.Error(), "rename checks/gl_lines.json to checks/ledger.json too") {
		t.Fatalf("err = %v, want the refusal to name the chain's end\n%s", err, stderr)
	}
}

// The same rename with the checks file renamed too: the checks entry travels
// with the table, a --prune silences nothing, and nothing is re-sent.
func TestPipelinePushMovesATableTogetherWithItsChecksFile(t *testing.T) {
	f := newFakePipelineInstance(t)
	signInPipeline(t, f)
	root := seedChecksFolder(t, f, twoChecks)
	if _, stderr, err := runPipelineCLI(t, root, "pipeline", "push"); err != nil {
		t.Fatalf("seed push: %v\n%s", err, stderr)
	}
	renameGLLines(t, root, true)
	writesBefore := len(f.checkWrites())

	out, stderr, err := runPipelineCLI(t, root, "pipeline", "push", "--prune", "--json")
	if err != nil {
		t.Fatalf("push: %v\n%s\n%s", err, out, stderr)
	}
	if len(f.created) != 0 {
		t.Fatalf("a rename created %+v", f.created)
	}
	if got := len(f.checkWrites()) - writesBefore; got != 0 {
		t.Errorf("checks that only changed address were written %d time(s)", got)
	}
	for _, name := range []string{"Unik rad per id", "Bygger om dagligen"} {
		if !f.checkNamed("table-gl", name).Enabled {
			t.Errorf("%q was silenced by a move", name)
		}
	}
	checks := pipelineStateOf(t, root, f.Key()).Checks
	if _, old := checks["checks/gl_lines.json"]; old || len(checks["checks/gl.json"].Managed) != 2 {
		t.Errorf("checks records = %+v, want both at checks/gl.json", checks)
	}
	if b := pipelineBindingOf(t, root, f.Key()); b.Tables["gl.sql"] != "table-gl" || b.Tables["gl_lines.sql"] != "" {
		t.Errorf("binding = %v", b.Tables)
	}
}
