package commands

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// The folder note on `ronja context`. What matters is that it fires when there
// IS something to say and stays silent otherwise — a command that editorialises
// about every directory is one people stop reading, which would defeat the
// whole purpose of putting the pointer there.

func TestContextPointsAtWFInsideAWorkflowFolder(t *testing.T) {
	f := newFakeInstance(t)
	signIn(t, f)

	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "ronja.json"),
		[]byte(`{"kind":"workflow","title":"Monthly report","entrypoint":"main.py","instances":[]}`), 0o600); err != nil {
		t.Fatalf("seed manifest: %v", err)
	}

	out, err := runCLI(t, dir, "context", "--no-docs")
	if err != nil {
		t.Fatalf("context: %v", err)
	}
	if !strings.Contains(out, "ronja wf push") {
		t.Errorf("context inside a workflow folder does not mention wf:\n%s", out)
	}
	// Naming the workflow is what makes it read as "this folder", not as a
	// generic advertisement.
	if !strings.Contains(out, "Monthly report") {
		t.Errorf("the note does not name the workflow:\n%s", out)
	}
	// An unbound manifest has never been pushed, and saying so is the
	// difference between "keep editing" and "nothing is on the server yet".
	if !strings.Contains(out, "Not pushed to this instance yet") {
		t.Errorf("an unbound folder was not reported as such:\n%s", out)
	}
}

func TestContextSuggestsWFInitForLoosePython(t *testing.T) {
	f := newFakeInstance(t)
	signIn(t, f)

	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "main.py"), []byte("print(1)\n"), 0o600); err != nil {
		t.Fatalf("seed script: %v", err)
	}

	out, err := runCLI(t, dir, "context", "--no-docs")
	if err != nil {
		t.Fatalf("context: %v", err)
	}
	if !strings.Contains(out, "ronja wf init") {
		t.Errorf("loose Python did not produce an init suggestion:\n%s", out)
	}
	if !strings.Contains(out, "main.py") {
		t.Errorf("the note does not name the file it found:\n%s", out)
	}
}

func TestContextSaysNothingInAnUnrelatedDirectory(t *testing.T) {
	f := newFakeInstance(t)
	signIn(t, f)

	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "notes.md"), []byte("# hi\n"), 0o600); err != nil {
		t.Fatalf("seed: %v", err)
	}

	out, err := runCLI(t, dir, "context", "--no-docs")
	if err != nil {
		t.Fatalf("context: %v", err)
	}
	if strings.Contains(out, "In this directory") {
		t.Errorf("context editorialised about an unrelated directory:\n%s", out)
	}
}

func TestContextJSONCarriesTheLocalWorkflow(t *testing.T) {
	f := newFakeInstance(t)
	signIn(t, f)

	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "ronja.json"),
		[]byte(`{"kind":"workflow","title":"Monthly report","entrypoint":"main.py","instances":[]}`), 0o600); err != nil {
		t.Fatalf("seed manifest: %v", err)
	}

	out, err := runCLI(t, dir, "context", "--no-docs", "--json")
	if err != nil {
		t.Fatalf("context: %v", err)
	}
	payload := decodeJSON(t, out)
	local, ok := payload["localWorkflow"].(map[string]any)
	if !ok {
		t.Fatalf("localWorkflow missing from --json payload: %v", payload)
	}
	if local["title"] != "Monthly report" {
		t.Errorf("localWorkflow.title = %v", local["title"])
	}
	if local["bound"] != false {
		t.Errorf("localWorkflow.bound = %v, want false", local["bound"])
	}
}

// TestContextPointsAtPipelineInsideAPipelineFolder: the third folder kind. The
// note has to name the commands that WORK here — pointing a pipeline folder at
// `wf` would name commands LoadManifest's kind check then refuses, which is the
// exact failure Kind.Command exists to prevent.
func TestContextPointsAtPipelineInsideAPipelineFolder(t *testing.T) {
	f := newFakeInstance(t)
	signIn(t, f)

	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "ronja.json"),
		[]byte(`{"kind":"pipeline","title":"Sales pipeline","instances":[]}`), 0o600); err != nil {
		t.Fatalf("seed manifest: %v", err)
	}

	out, err := runCLI(t, dir, "context", "--no-docs")
	if err != nil {
		t.Fatalf("context: %v", err)
	}
	if !strings.Contains(out, "ronja pipeline push") {
		t.Errorf("context inside a pipeline folder does not mention pipeline:\n%s", out)
	}
	if strings.Contains(out, "ronja wf push") || strings.Contains(out, "ronja app push") {
		t.Errorf("the note names another kind's commands:\n%s", out)
	}
	if !strings.Contains(out, "pipeline folder") {
		t.Errorf("the folder is not named as a pipeline folder:\n%s", out)
	}
	if !strings.Contains(out, "Sales pipeline") {
		t.Errorf("the note does not name the folder:\n%s", out)
	}
	if !strings.Contains(out, "Nothing pushed to this instance yet") {
		t.Errorf("an unbound folder was not reported as such:\n%s", out)
	}
}

// TestContextPayloadCarriesTheFolderKind: the --json payload used to omit `kind`
// entirely, so a program could see that there was a folder but not which
// commands drive it — the one thing the note exists to say.
func TestContextPayloadCarriesTheFolderKind(t *testing.T) {
	f := newFakeInstance(t)
	signIn(t, f)

	for _, tc := range []struct{ manifest, want string }{
		{`{"kind":"workflow","title":"W","entrypoint":"main.py","instances":[]}`, "workflow"},
		{`{"kind":"dataapp","title":"A","entrypoint":"App.tsx","instances":[]}`, "dataapp"},
		{`{"kind":"pipeline","title":"P","instances":[]}`, "pipeline"},
	} {
		t.Run(tc.want, func(t *testing.T) {
			dir := t.TempDir()
			if err := os.WriteFile(filepath.Join(dir, "ronja.json"), []byte(tc.manifest), 0o600); err != nil {
				t.Fatalf("seed manifest: %v", err)
			}
			out, err := runCLI(t, dir, "context", "--no-docs", "--json")
			if err != nil {
				t.Fatalf("context: %v", err)
			}
			payload := decodeJSON(t, out)
			local, ok := payload["localWorkflow"].(map[string]any)
			if !ok {
				t.Fatalf("no localWork in the payload:\n%s", out)
			}
			if local["kind"] != tc.want {
				t.Errorf("kind = %v, want %q", local["kind"], tc.want)
			}
		})
	}
}
