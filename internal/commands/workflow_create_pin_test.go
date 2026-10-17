package commands

import (
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/ronjatech/ronja-cli/internal/api"
	"github.com/ronjatech/ronja-cli/internal/wfdir"
)

// ── A folder pinned to 4 loads, validates and creates on 4 ──────────────────

// LoadManifest refuses a runtime this build does not know, so 4 must be one it
// knows for a folder an instance defaulting to 4 pinned to open at all.
func TestAFolderPinnedToRuntimeFourCreatesOnFour(t *testing.T) {
	f := newFakeInstance(t)
	f.defaults = &api.WorkflowDefaults{RuntimeVersion: wfdir.RuntimeMain, EntrypointStarter: "def main():\n    return None\n"}
	signIn(t, f)
	dir := t.TempDir()

	if _, err := runCLI(t, dir, "wf", "init", "--feature", "feat-1"); err != nil {
		t.Fatalf("init: %v", err)
	}
	if _, err := runCLI(t, dir, "wf", "validate"); err != nil {
		t.Fatalf("validate a folder pinned to 4: %v", err)
	}
	if _, err := runCLI(t, dir, "wf", "push"); err != nil {
		t.Fatalf("push a folder pinned to 4: %v", err)
	}
	if len(f.created) != 1 || f.created[0].RuntimeVersion != wfdir.RuntimeMain {
		t.Fatalf("create sent %+v, want runtimeVersion 4 — the pinned runtime, never omitted", f.created)
	}
	for _, v := range f.validated {
		if v.RuntimeVersion != wfdir.RuntimeMain {
			t.Errorf("a validate rehearsed runtime %d, want the pinned 4", v.RuntimeVersion)
		}
	}
	if m, err := wfdir.LoadManifest(dir, wfdir.WorkflowKind); err != nil || m.Runtime != wfdir.RuntimeMain {
		t.Errorf("after the push the manifest runtime is %v (err %v), want 4", m, err)
	}
}

// ── An unpinned folder's create names the instance's runtime ────────────────
//
// A folder with no "runtime" key (hand-written, or from an older `wf init`) is
// validated and created on the runtime the instance names, and pinned once the
// create lands.

func TestAnUnpinnedCreatePinsTheInstancesDefault(t *testing.T) {
	f := newFakeInstance(t)
	signIn(t, f)
	dir := t.TempDir()
	initUnpinnedFolder(t, dir)
	f.defaults = &api.WorkflowDefaults{RuntimeVersion: wfdir.RuntimeMain, EntrypointStarter: "x\n"}
	f.defaultsAsked = nil

	out, err := runCLI(t, dir, "wf", "push")
	if err != nil {
		t.Fatalf("push: %v\n%s", err, out)
	}
	if len(f.created) != 1 || f.created[0].RuntimeVersion != wfdir.RuntimeMain {
		t.Fatalf("create sent %+v, want the instance's runtime 4 named", f.created)
	}
	if len(f.validated) != 1 || f.validated[0].RuntimeVersion != wfdir.RuntimeMain {
		t.Errorf("validate sent %+v, want the runtime the create is made on", f.validated)
	}
	if m, err := wfdir.LoadManifest(dir, wfdir.WorkflowKind); err != nil || m.Runtime != wfdir.RuntimeMain {
		t.Errorf("manifest after the create: %v (err %v), want runtime 4 pinned", m, err)
	}
	if want := "this folder named no runtime; pinned to the instance's default 4"; !strings.Contains(out, want) {
		t.Errorf("the push report does not say it pinned the folder (%q):\n%s", want, out)
	}
	if len(f.defaultsAsked) != 1 || f.defaultsAsked[0] != "" {
		t.Errorf("defaults asked %q, want once for the default", f.defaultsAsked)
	}
}

// A push refused before the create leaves ronja.json byte-identical: the pin is
// in memory until the workflow exists.
func TestARefusedValidateLeavesAnUnpinnedManifestByteIdentical(t *testing.T) {
	f := newFakeInstance(t)
	signIn(t, f)
	dir := t.TempDir()
	initUnpinnedFolder(t, dir)
	before := readFile(t, dir, wfdir.ManifestName)
	f.validate = &api.ValidateResult{Findings: []api.ValidateFinding{
		{Severity: "error", Code: "bad", Message: "no", Path: "main.py"},
	}}

	if _, err := runCLI(t, dir, "wf", "push"); err == nil {
		t.Fatal("push succeeded over a refused validate")
	}
	if len(f.validated) != 1 || f.validated[0].RuntimeVersion != wfdir.RuntimeQuery {
		t.Errorf("validate sent %+v, want the instance's runtime 3", f.validated)
	}
	if len(f.created) != 0 {
		t.Errorf("a refused push created %+v", f.created)
	}
	if after := readFile(t, dir, wfdir.ManifestName); after != before {
		t.Errorf("a refused push rewrote ronja.json:\nbefore %s\nafter  %s", before, after)
	}
}

// Every failure of the defaults read stops the push before anything is sent.
func TestAnUnpinnedCreateIsLoudWhenTheDefaultsCannotBeRead(t *testing.T) {
	for _, tc := range []struct {
		name  string
		stage func(f *fakeInstance)
		want  string
	}{
		{"5xx", func(f *fakeInstance) { f.failDefaults = http.StatusInternalServerError }, "which runtime a new workflow starts on"},
		{"no route", func(f *fakeInstance) { f.defaults = nil }, "predates this CLI"},
		{"unknown default", func(f *fakeInstance) {
			f.defaults = &api.WorkflowDefaults{RuntimeVersion: 5, EntrypointStarter: "x\n"}
		}, "run `ronja update`"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newFakeInstance(t)
			signIn(t, f)
			dir := t.TempDir()
			initUnpinnedFolder(t, dir)
			before := readFile(t, dir, wfdir.ManifestName)
			tc.stage(f)

			_, err := runCLI(t, dir, "wf", "push")
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("push = %v, want a refusal saying %q", err, tc.want)
			}
			if len(f.validated) != 0 || len(f.created) != 0 {
				t.Errorf("sent validate %+v / create %+v over an unread default", f.validated, f.created)
			}
			if after := readFile(t, dir, wfdir.ManifestName); after != before {
				t.Errorf("ronja.json changed:\n%s", after)
			}
		})
	}
}

// `wf validate` rehearses the runtime the create will be made on, and writes
// nothing.
func TestValidateOfAnUnpinnedUncreatedFolderUsesTheServedRuntime(t *testing.T) {
	f := newFakeInstance(t)
	signIn(t, f)
	dir := t.TempDir()
	initUnpinnedFolder(t, dir)
	before := readFile(t, dir, wfdir.ManifestName)
	f.defaults = &api.WorkflowDefaults{RuntimeVersion: wfdir.RuntimeMain, EntrypointStarter: "x\n"}

	if _, err := runCLI(t, dir, "wf", "validate"); err != nil {
		t.Fatalf("validate: %v", err)
	}
	if len(f.validated) != 1 || f.validated[0].RuntimeVersion != wfdir.RuntimeMain {
		t.Errorf("validate sent %+v, want the served runtime 4", f.validated)
	}
	if after := readFile(t, dir, wfdir.ManifestName); after != before {
		t.Errorf("validate rewrote ronja.json:\n%s", after)
	}
}

// unpinnedTree is a tree of n uncreated workflow folders that name no runtime.
func unpinnedTree(t *testing.T, f *fakeInstance, n int) string {
	t.Helper()
	tree := t.TempDir()
	for i := range n {
		dir := filepath.Join(tree, fmt.Sprintf("wf%d", i))
		if err := os.MkdirAll(dir, 0o755); err != nil {
			t.Fatal(err)
		}
		initUnpinnedFolder(t, dir)
	}
	f.defaultsAsked = nil
	return tree
}

// `sync check` validates an unpinned, uncreated folder against the runtime the
// instance serves, asks for it once per tree, and writes nothing.
func TestSyncCheckValidatesAnUnpinnedFolderAgainstTheServedRuntime(t *testing.T) {
	f := newFakeInstance(t)
	signIn(t, f)
	tree := unpinnedTree(t, f, 2)
	before := readFile(t, tree, "wf0", wfdir.ManifestName)
	f.defaults = &api.WorkflowDefaults{RuntimeVersion: wfdir.RuntimeMain, EntrypointStarter: "x\n"}

	out, err := runCLI(t, tree, "sync", "check", "--json")
	if code := exitCodeOf(err); code != syncExitClean {
		t.Fatalf("exit = %d (%v)\n%s", code, err, out)
	}
	if len(f.validated) != 2 {
		t.Fatalf("validated %d folders, want 2", len(f.validated))
	}
	for _, v := range f.validated {
		if v.RuntimeVersion != wfdir.RuntimeMain {
			t.Errorf("sync check validated against runtime %d, want the served 4", v.RuntimeVersion)
		}
	}
	if len(f.defaultsAsked) != 1 {
		t.Errorf("defaults asked %d times for one tree, want 1", len(f.defaultsAsked))
	}
	if after := readFile(t, tree, "wf0", wfdir.ManifestName); after != before {
		t.Errorf("sync check rewrote ronja.json:\n%s", after)
	}
}

// `sync apply --allow-create` over a tree of unpinned folders asks once, and
// carries the pin note on each folder it pinned.
func TestSyncApplyPinsUnpinnedCreatesWithOneDefaultsRead(t *testing.T) {
	f := newFakeInstance(t)
	signIn(t, f)
	tree := unpinnedTree(t, f, 2)

	out, err := runCLI(t, tree, "sync", "apply", "--allow-create", "--json")
	if code := exitCodeOf(err); code != syncExitClean {
		t.Fatalf("exit = %d (%v)\n%s", code, err, out)
	}
	if len(f.created) != 2 {
		t.Fatalf("created %+v, want 2", f.created)
	}
	if len(f.defaultsAsked) != 1 {
		t.Errorf("defaults asked %d times for one tree, want 1", len(f.defaultsAsked))
	}
	report := applyReportOf(t, out)
	for _, path := range []string{"wf0", "wf1"} {
		line := applyFolderLine(t, report, path)
		if !strings.Contains(strings.Join(line.Notes, "\n"), "pinned to the instance's default 3") {
			t.Errorf("%s: notes %q do not say the folder was pinned", path, line.Notes)
		}
	}
}

// A 500 on the defaults read during apply is an unanswered request: `unknown`,
// never "not deployed".
func TestSyncApplyDefaultsReadFailureIsUnknown(t *testing.T) {
	f := newFakeInstance(t)
	signIn(t, f)
	tree := unpinnedTree(t, f, 1)
	f.failDefaults = http.StatusInternalServerError

	out, err := runCLI(t, tree, "sync", "apply", "--allow-create", "--json")
	if code := exitCodeOf(err); code != syncExitUnknown {
		t.Fatalf("exit = %d (%v), want %d\n%s", code, err, syncExitUnknown, out)
	}
	line := applyFolderLine(t, applyReportOf(t, out), "wf0")
	if line.Verdict != verdictUnknown || line.Reason != syncReasonUnanswered {
		t.Fatalf("folder = %+v, want %s/%s", line, verdictUnknown, syncReasonUnanswered)
	}
	if len(f.created) != 0 {
		t.Errorf("created %+v over an unread default", f.created)
	}
}

// A bound folder that names no runtime is checked against its row's runtime, so
// when the row cannot be read it is checked against none, and the note says so.
func TestValidateOfAnUnpinnedFolderWhoseRowCannotBeReadSaysNoRuntimeRulesRan(t *testing.T) {
	for _, tc := range []struct {
		name    string
		runtime int
		said    bool
	}{
		{"unpinned", 0, true},
		{"pinned", wfdir.RuntimeQuery, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newFakeInstance(t)
			signIn(t, f)
			dir := bindFolder(t, f, "wf-gone", map[string]string{"main.py": "print('hi')\n"})
			setManifestRuntime(t, dir, tc.runtime)

			const want = "without runtime-scoped rules"
			for _, args := range [][]string{{"wf", "validate"}, {"sync", "check"}} {
				f.validated = nil
				stderr := captureStderr(t, func() { _, _ = runCLI(t, dir, args...) })
				if !strings.Contains(stderr, "could not read wf-gone") {
					t.Fatalf("%v: no could-not-read note:\n%s", args, stderr)
				}
				if said := strings.Contains(stderr, want); said != tc.said {
					t.Errorf("%v: the note says no runtime rules ran = %v, want %v:\n%s", args, said, tc.said, stderr)
				}
				if len(f.validated) != 1 || f.validated[0].RuntimeVersion != tc.runtime {
					t.Errorf("%v: validate sent %+v, want runtime %d", args, f.validated, tc.runtime)
				}
			}
		})
	}
}
