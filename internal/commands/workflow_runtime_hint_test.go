package commands

import (
	"fmt"
	"net/http"
	"strings"
	"testing"

	"github.com/ronjatech/ronja-cli/internal/api"
	"github.com/ronjatech/ronja-cli/internal/wfdir"
)

// `sync apply` reports a folder the instance refused for its runtime with the
// same way out `wf push` prints — in Notes, so Detail stays one line.
func TestSyncApplyShowsTheRuntimeHintInNotes(t *testing.T) {
	f := newFakeInstance(t)
	signIn(t, f)
	tree := unpinnedTree(t, f, 1)
	f.failCreate = http.StatusBadRequest
	f.failCreateMessage = "invalid runtimeVersion 3 (expected 4)"

	out, err := runCLI(t, tree, "sync", "apply", "--allow-create", "--json")
	if exitCodeOf(err) == syncExitClean {
		t.Fatalf("apply succeeded over a refused create\n%s", out)
	}
	line := applyFolderLine(t, applyReportOf(t, out), "wf0")
	if !strings.Contains(line.Detail, f.failCreateMessage) {
		t.Errorf("detail %q does not carry the refusal", line.Detail)
	}
	if strings.Contains(line.Detail, "\n") {
		t.Errorf("detail is not one line: %q", line.Detail)
	}
	if notes := strings.Join(line.Notes, "\n"); !strings.Contains(notes, "run `ronja update`") {
		t.Errorf("notes %q do not carry the runtime hint", line.Notes)
	}
}

// ── An instance's runtime refusal says what to do about it ──────────────────

func TestRuntimeRefusalHint(t *testing.T) {
	frozenBody := `{"error":"runtime 3 takes no more code changes","code":"runtime_frozen"}`
	for _, tc := range []struct {
		name          string
		status        int
		message, body string
		drift, frozen bool
	}{
		{"drift: the instance does not take the runtime sent", 400,
			"invalid runtimeVersion 4 (expected 3; runtime 1 and 2 are retired)", "", true, false},
		{"drift: the row moved past the folder", 400,
			"runtimeVersion is a one-way upgrade: a runtime-4 workflow cannot be moved back to runtime 3", "", true, false},
		{"a data app's specVersion refusal is not a runtime refusal", 400,
			"specVersion is a one-way upgrade: a spec-2 app cannot be moved back to spec 1", "", false, false},
		{"frozen, by wire code", 400, "runtime 3 takes no more code changes", frozenBody, false, true},
		{"frozen in prose only: no code, no hint", 400, "runtime 3 is frozen", "", false, false},
		{"not a runtime refusal", 400, "title is required", "", false, false},
		{"not a refusal at all", 500, "invalid runtimeVersion 4", "", false, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			body := tc.body
			if body == "" {
				body = fmt.Sprintf(`{"error":%q}`, tc.message)
			}
			base := fmt.Errorf("create workflow in feature feat-1: %w",
				&api.Error{Status: tc.status, Code: tc.message, Body: body})
			got := runtimeRefusalHint(base).Error()
			if !strings.HasPrefix(got, base.Error()) {
				t.Errorf("the refusal itself was not kept first: %q", got)
			}
			drift := strings.Contains(got, "if it names an older one, set \"runtime\" in ronja.json to one it lists")
			if drift != tc.drift {
				t.Errorf("drift hint present = %v, want %v: %q", drift, tc.drift, got)
			}
			frozen := strings.Contains(got, `to raise this folder, set "runtime" in ronja.json`)
			if frozen != tc.frozen {
				t.Errorf("frozen hint present = %v, want %v: %q", frozen, tc.frozen, got)
			}
			if (tc.drift || tc.frozen) && !strings.Contains(got, "1, 2, 3 or 4") {
				t.Errorf("the hint does not list the runtimes this CLI knows: %q", got)
			}
		})
	}
}

// Through the real entry point, where the hint is applied: a create the
// instance refuses for its runtime reaches stderr with the way out under it.
func TestARuntimeRefusedCreateEndsWithTheUpdateHint(t *testing.T) {
	f := newFakeInstance(t)
	f.otherStarters = map[int]string{wfdir.RuntimeMain: "x\n"}
	signIn(t, f)
	dir := t.TempDir()
	if _, err := runCLI(t, dir, "wf", "init", "--feature", "feat-1", "--runtime", "4"); err != nil {
		t.Fatalf("init: %v", err)
	}
	f.failCreate = http.StatusBadRequest
	f.failCreateMessage = "invalid runtimeVersion 4 (expected 3; runtime 1 and 2 are retired)"
	t.Chdir(dir)

	var code int
	stderr := captureStderr(t, func() {
		code = run([]string{"wf", "push", "--no-validate"})
	})
	if code == 0 {
		t.Fatal("push succeeded over a refused create")
	}
	if !strings.Contains(stderr, f.failCreateMessage) || !strings.Contains(stderr, "run `ronja update`") {
		t.Errorf("stderr does not carry the refusal and the hint:\n%s", stderr)
	}
}
