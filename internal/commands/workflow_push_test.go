package commands

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/ronjatech/ronja-cli/internal/api"
	"github.com/ronjatech/ronja-cli/internal/wfdir"
)

// The push tests are about the STATE MACHINE, which is where the interesting
// failures live: which row the files go to, whether a second draft gets checked
// out, and what happens when something refuses half way. The fake models those
// transitions rather than answering 200 to everything, so a command that gets
// the sequence wrong fails here rather than in production.

// --- helpers ---------------------------------------------------------------

// cloneFolder produces a folder in the state `wf clone` leaves it in: files on
// disk, a manifest binding, and a baseline that matches. Every push test that
// is not about the FIRST push starts here, because a hand-written baseline is
// exactly the thing these tests are checking the commands maintain.
func cloneFolder(t *testing.T, f *fakeInstance, workflowID string) string {
	t.Helper()
	out, err := runCLI(t, t.TempDir(), "wf", "clone", workflowID, "--json")
	if err != nil {
		t.Fatalf("clone %s: %v", workflowID, err)
	}
	root := decodeJSON(t, out)["root"].(string)
	f.Requests = nil
	return root
}

// initFolder produces the folder `wf init` leaves: a feature binding, no
// workflow, no baseline.
func initFolder(t *testing.T, f *fakeInstance, files map[string]string) string {
	t.Helper()
	root := t.TempDir()
	manifest := &wfdir.Manifest{
		Kind: wfdir.KindWorkflow, Title: "Monthly report", Entrypoint: "main.py",
		Instances: []wfdir.Instance{{URL: f.URL(), TenantID: testTenantID, Binding: wfdir.Binding{FeatureID: "feat-1"}}},
	}
	if err := wfdir.SaveManifest(root, manifest); err != nil {
		t.Fatalf("save manifest: %v", err)
	}
	if err := wfdir.SaveState(root, &wfdir.State{}); err != nil {
		t.Fatalf("save state: %v", err)
	}
	for path, content := range files {
		writeLocal(t, root, path, content)
	}
	return root
}

func writeLocal(t *testing.T, root, path, content string) {
	t.Helper()
	full := filepath.Join(root, filepath.FromSlash(path))
	if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
		t.Fatalf("mkdir for %s: %v", path, err)
	}
	if err := os.WriteFile(full, []byte(content), 0o644); err != nil {
		t.Fatalf("write %s: %v", path, err)
	}
}

// requestsMatching filters the fake's request log, so an ordering assertion
// reads as the sequence it is about rather than as everything that happened.
func requestsMatching(f *fakeInstance, prefix string) []string {
	var out []string
	for _, req := range f.Requests {
		if strings.HasPrefix(req, prefix) {
			out = append(out, req)
		}
	}
	return out
}

// fileAndMetadataWrites is the sequence a push's ordering is about: the file
// PUTs, the metadata PUT and the DELETEs, with the reads and the checkout that
// happen around them left out.
func fileAndMetadataWrites(f *fakeInstance) []string {
	var out []string
	for _, req := range f.Requests {
		if strings.HasPrefix(req, "PUT ") || strings.HasPrefix(req, "DELETE ") {
			out = append(out, req)
		}
	}
	return out
}

// retitle rewrites the manifest's title, the local half of a metadata change.
func retitle(t *testing.T, root, title string) {
	t.Helper()
	manifest, err := wfdir.LoadManifest(root, wfdir.WorkflowKind)
	if err != nil {
		t.Fatalf("load manifest: %v", err)
	}
	manifest.Title = title
	if err := wfdir.SaveManifest(root, manifest); err != nil {
		t.Fatalf("save manifest: %v", err)
	}
}

// bindFolder produces the folder a colleague gets from git: a committed
// ronja.json with a binding, the source files — and NO .ronja/, because the
// baseline is per-user and correctly never committed.
func bindFolder(t *testing.T, f *fakeInstance, workflowID string, files map[string]string) string {
	t.Helper()
	root := t.TempDir()
	manifest := &wfdir.Manifest{
		Kind: wfdir.KindWorkflow, Title: "Monthly report", Entrypoint: "main.py",
		Instances: []wfdir.Instance{{URL: f.URL(), TenantID: testTenantID, Binding: wfdir.Binding{WorkflowID: workflowID, FeatureID: "feat-1"}}},
	}
	if err := wfdir.SaveManifest(root, manifest); err != nil {
		t.Fatalf("save manifest: %v", err)
	}
	for path, content := range files {
		writeLocal(t, root, path, content)
	}
	return root
}

// --- first push ------------------------------------------------------------

func TestPushFirstPushCreatesWorkflowAndRecordsBinding(t *testing.T) {
	f := newFakeInstance(t)
	signIn(t, f)
	root := initFolder(t, f, map[string]string{
		"main.py":        "print('hi')\n",
		"lib/helpers.py": "X = 1\n",
	})

	out, err := runCLI(t, root, "wf", "push", "--json")
	if err != nil {
		t.Fatalf("push: %v", err)
	}
	payload := decodeJSON(t, out)
	if !payload["created"].(bool) {
		t.Error("created = false on a first push")
	}

	if len(f.created) != 1 {
		t.Fatalf("created %d workflows, want 1", len(f.created))
	}
	if got := f.created[0]; got.FeatureID != "feat-1" || got.Title != "Monthly report" || got.Entrypoint != "main.py" {
		t.Errorf("create body = %+v", got)
	}

	// The binding is what makes the SECOND push find this workflow rather than
	// create another one.
	manifest, err := wfdir.LoadManifest(root, wfdir.WorkflowKind)
	if err != nil {
		t.Fatalf("load manifest: %v", err)
	}
	binding, bound, _ := manifest.Binding(f.Key())
	newID := payload["workflowID"].(string)
	if !bound || binding.WorkflowID != newID || binding.FeatureID != "feat-1" {
		t.Errorf("binding = %+v (bound %v), want %s/feat-1", binding, bound, newID)
	}

	if got := f.FileContents(newID); got["main.py"] != "print('hi')\n" || got["lib/helpers.py"] != "X = 1\n" {
		t.Errorf("pushed files = %+v", got)
	}
	// A first push creates the row, so there is nothing to check out and
	// nothing that could have drifted.
	for _, req := range f.Requests {
		if strings.Contains(req, "/checkout") {
			t.Errorf("first push called checkout: %v", f.Requests)
		}
	}
	assertBaselineMatchesDisk(t, root, f.Key())

	// And a second push reuses the binding rather than creating a twin.
	writeLocal(t, root, "main.py", "print('bye')\n")
	if _, err := runCLI(t, root, "wf", "push", "--json"); err != nil {
		t.Fatalf("second push: %v", err)
	}
	if len(f.created) != 1 {
		t.Errorf("second push created another workflow: %+v", f.created)
	}
}

// The runtime is stamped at CREATE. Both halves are the test: the create carries
// the manifest's declaration, and a later push of a folder that already AGREES
// with its row does not restamp it. (The upgrade a later push CAN carry, when
// the two disagree, is workflow_runtime_test.go's.)
func TestPushSendsTheDeclaredRuntimeAtCreate(t *testing.T) {
	f := newFakeInstance(t)
	signIn(t, f)
	dir := t.TempDir()
	if _, err := runCLI(t, dir, "wf", "init", "--feature", "feat-1", "--runtime", "2", "--json"); err != nil {
		t.Fatalf("init: %v", err)
	}

	out, err := runCLI(t, dir, "wf", "push", "--json")
	if err != nil {
		t.Fatalf("push: %v", err)
	}
	payload := decodeJSON(t, out)
	if payload["runtimeVersion"] != float64(wfdir.RuntimeDurable) {
		t.Errorf("push reported runtimeVersion = %v, want %d", payload["runtimeVersion"], wfdir.RuntimeDurable)
	}
	if len(f.created) != 1 {
		t.Fatalf("created %d workflows, want 1", len(f.created))
	}
	if got := f.created[0].RuntimeVersion; got != wfdir.RuntimeDurable {
		t.Errorf("create sent runtimeVersion %d, want %d", got, wfdir.RuntimeDurable)
	}
	// The candidate is validated against the runtime it was written for, or a
	// v2-only construct is checked by v1 rules and passes for the wrong reason.
	if len(f.validated) == 0 || f.validated[0].RuntimeVersion != wfdir.RuntimeDurable {
		t.Errorf("validate sent runtimeVersion %+v, want %d", f.validated, wfdir.RuntimeDurable)
	}

	writeLocal(t, dir, "main.py", "print('changed')\n")
	if _, err := runCLI(t, dir, "wf", "push", "--json"); err != nil {
		t.Fatalf("second push: %v", err)
	}
	if len(f.created) != 1 {
		t.Errorf("the second push created another workflow: %+v", f.created)
	}
}

// A folder that declares no runtime sends no runtimeVersion on the CREATE: the
// key is absent because the folder has no opinion, and the instance stamps its
// own default — which is also what keeps the request byte-identical to the one
// an instance predating durable workflows saw.
//
// VALIDATE is the deliberate asymmetry. It is a pre-creation endpoint with no
// row to read the runtime from, and it takes 0 as "check nothing runtime-scoped"
// — so a create rehearsed with 0 would rehearse a different save from the one
// about to happen. See Manifest.RuntimeForValidate.
func TestPushOfADefaultRuntimeFolderSendsNoRuntimeVersionOnTheCreate(t *testing.T) {
	f := newFakeInstance(t)
	signIn(t, f)
	root := initFolder(t, f, map[string]string{"main.py": "print('hi')\n"})

	if _, err := runCLI(t, root, "wf", "push", "--json"); err != nil {
		t.Fatalf("push: %v", err)
	}
	if len(f.created) != 1 {
		t.Fatalf("created %d workflows, want 1", len(f.created))
	}
	if got := f.created[0].RuntimeVersion; got != 0 {
		t.Errorf("create sent runtimeVersion %d, want it absent", got)
	}
	if got := f.validated[0].RuntimeVersion; got != wfdir.RuntimeCreateDefault {
		t.Errorf("validate sent runtimeVersion %d, want %d — the runtime the create will produce", got, wfdir.RuntimeCreateDefault)
	}
}

// --- checkout ---------------------------------------------------------------

func TestPushChecksOutOnceThenReusesTheDraft(t *testing.T) {
	f := newFakeInstance(t)
	f.AddWorkflow(
		&api.Workflow{ID: "wf-1", Lifecycle: api.LifecycleLive},
		api.WorkflowFile{Path: "main.py", Content: "old\n"},
	)
	signIn(t, f)
	root := cloneFolder(t, f, "wf-1")

	writeLocal(t, root, "main.py", "new\n")
	out, err := runCLI(t, root, "wf", "push", "--json")
	if err != nil {
		t.Fatalf("push: %v", err)
	}
	draftID := decodeJSON(t, out)["draftID"].(string)
	if draftID == "wf-1" {
		t.Fatal("push wrote to the live row instead of a draft")
	}
	if got := f.FileContents(draftID)["main.py"]; got != "new\n" {
		t.Errorf("draft main.py = %q", got)
	}
	// The live row is untouched — push never writes to it.
	if got := f.FileContents("wf-1")["main.py"]; got != "old\n" {
		t.Errorf("live main.py = %q, want it untouched", got)
	}

	// The second push must find the existing draft. The fake fails the test if
	// checkout is called for a workflow that already has one, which is the
	// bug this guards: a per-user draft is get-or-create server-side, so a
	// double checkout is silent and only shows up as a wasted round trip.
	f.Requests = nil
	writeLocal(t, root, "main.py", "newer\n")
	if _, err := runCLI(t, root, "wf", "push", "--json"); err != nil {
		t.Fatalf("second push: %v", err)
	}
	for _, req := range f.Requests {
		if strings.Contains(req, "/checkout") {
			t.Errorf("second push checked out again: %v", f.Requests)
		}
	}
}

// --- ordering ---------------------------------------------------------------

func TestPushWritesTheEntrypointFirst(t *testing.T) {
	f := newFakeInstance(t)
	signIn(t, f)
	root := initFolder(t, f, map[string]string{
		"aaa.py":         "A\n",
		"main.py":        "M\n",
		"lib/helpers.py": "H\n",
	})

	if _, err := runCLI(t, root, "wf", "push", "--json"); err != nil {
		t.Fatalf("push: %v", err)
	}
	puts := requestsMatching(f, "PUT ")
	if len(puts) != 3 {
		t.Fatalf("PUTs = %v, want 3", puts)
	}
	if !strings.HasSuffix(puts[0], "/main.py") {
		t.Errorf("first PUT = %s, want the entrypoint", puts[0])
	}
	// The rest are sorted, so a push is reproducible request-for-request.
	if !strings.HasSuffix(puts[1], "/aaa.py") || !strings.HasSuffix(puts[2], "/lib/helpers.py") {
		t.Errorf("PUT order = %v", puts)
	}
}

// The whole-push order, which the server's two rules pin between them: an
// entrypoint patch is refused unless the row already holds that file, and a
// deletion is refused while the row still names the file as its entrypoint. So
// writes must come before the metadata patch, and the patch before the
// deletions — anything else makes a rename impossible.
func TestPushOrdersWritesThenMetadataThenDeletions(t *testing.T) {
	f := newFakeInstance(t)
	f.AddWorkflow(
		&api.Workflow{ID: "wf-1", Lifecycle: api.LifecycleLive, Title: "Old name"},
		api.WorkflowFile{Path: "main.py", Content: "M\n"},
		api.WorkflowFile{Path: "aaa.py", Content: "A\n"},
		api.WorkflowFile{Path: "lib/old.py", Content: "O\n"},
	)
	signIn(t, f)
	root := cloneFolder(t, f, "wf-1")

	writeLocal(t, root, "main.py", "M2\n")
	writeLocal(t, root, "aaa.py", "A2\n")
	if err := os.Remove(filepath.Join(root, "lib", "old.py")); err != nil {
		t.Fatalf("remove: %v", err)
	}
	retitle(t, root, "New name")

	if _, err := runCLI(t, root, "wf", "push", "--json"); err != nil {
		t.Fatalf("push: %v", err)
	}
	writes := fileAndMetadataWrites(f)
	if len(writes) != 4 {
		t.Fatalf("writes = %v, want 4", writes)
	}
	if !strings.HasSuffix(writes[0], "/files/main.py") || !strings.HasSuffix(writes[1], "/files/aaa.py") {
		t.Errorf("file writes = %v, want the entrypoint first then path order", writes)
	}
	if strings.Contains(writes[2], "/files/") || !strings.HasPrefix(writes[2], "PUT ") {
		t.Errorf("third write = %s, want the metadata patch", writes[2])
	}
	if !strings.HasPrefix(writes[3], "DELETE ") {
		t.Errorf("fourth write = %s, want the deletion", writes[3])
	}
}

// The rename the old ordering made impossible: the row goes on naming the old
// entrypoint, so the DELETE of it is refused and the folder can never converge.
func TestPushRenamesTheEntrypoint(t *testing.T) {
	f := newFakeInstance(t)
	f.AddWorkflow(
		&api.Workflow{ID: "wf-1", Lifecycle: api.LifecycleLive, Entrypoint: "main.py"},
		api.WorkflowFile{Path: "main.py", Content: "M\n"},
	)
	signIn(t, f)
	root := cloneFolder(t, f, "wf-1")

	writeLocal(t, root, "run.py", "M\n")
	if err := os.Remove(filepath.Join(root, "main.py")); err != nil {
		t.Fatalf("remove: %v", err)
	}
	manifest, err := wfdir.LoadManifest(root, wfdir.WorkflowKind)
	if err != nil {
		t.Fatalf("load manifest: %v", err)
	}
	manifest.Entrypoint = "run.py"
	if err := wfdir.SaveManifest(root, manifest); err != nil {
		t.Fatalf("save manifest: %v", err)
	}

	out, err := runCLI(t, root, "wf", "push", "--json")
	if err != nil {
		t.Fatalf("push: %v", err)
	}
	draftID := decodeJSON(t, out)["draftID"].(string)
	if got := f.entrypointPatches[draftID]; got != "run.py" {
		t.Errorf("entrypoint patches = %+v, want run.py on %s", f.entrypointPatches, draftID)
	}
	if got := f.workflows[draftID].Entrypoint; got != "run.py" {
		t.Errorf("draft entrypoint = %q", got)
	}
	files := f.FileContents(draftID)
	if _, still := files["main.py"]; still {
		t.Errorf("the old entrypoint survived the rename: %+v", files)
	}
	if files["run.py"] != "M\n" {
		t.Errorf("draft files = %+v", files)
	}
	assertBaselineMatchesDisk(t, root, f.Key())
}

// A refused deletion is reported as-is, and what already landed is recorded —
// the same partial-push contract the file writes have.
func TestPushPassesThroughADeletionRefusal(t *testing.T) {
	f := newFakeInstance(t)
	f.AddWorkflow(
		&api.Workflow{ID: "wf-1", Lifecycle: api.LifecycleLive},
		api.WorkflowFile{Path: "main.py", Content: "M\n"},
		api.WorkflowFile{Path: "lib/old.py", Content: "O\n"},
	)
	f.failDelete["lib/old.py"] = 400
	signIn(t, f)
	root := cloneFolder(t, f, "wf-1")

	writeLocal(t, root, "main.py", "M2\n")
	if err := os.Remove(filepath.Join(root, "lib", "old.py")); err != nil {
		t.Fatalf("remove: %v", err)
	}
	out, err := runCLI(t, root, "wf", "push", "--json")
	if err == nil {
		t.Fatal("push claimed to delete a file the server refused")
	}
	if !strings.Contains(err.Error(), "lib/old.py") {
		t.Errorf("error = %v, want it to name the file", err)
	}
	payload := decodeJSON(t, out)
	if pushed := payload["pushed"].([]any); len(pushed) != 1 {
		t.Errorf("pushed = %v, want the file that did land", pushed)
	}
	// The baseline holds what the server holds: main.py as pushed, lib/old.py
	// still there. Without that, the retry reads as somebody else's drift.
	state, err := wfdir.LoadState(root)
	if err != nil {
		t.Fatalf("load state: %v", err)
	}
	baseline := state.For(f.Key())
	if baseline == nil || baseline.Files["main.py"].SHA256 != wfdir.HashString("M2\n") {
		t.Errorf("baseline = %+v, want the pushed content", baseline)
	}
	if _, ok := baseline.Files["lib/old.py"]; !ok {
		t.Error("the baseline dropped a file the server still holds")
	}
}

// --- drift ------------------------------------------------------------------

func TestPushRefusesDriftUnlessForced(t *testing.T) {
	f := newFakeInstance(t)
	f.AddWorkflow(
		&api.Workflow{ID: "wf-1", Lifecycle: api.LifecycleLive},
		api.WorkflowFile{Path: "main.py", Content: "old\n"},
	)
	f.AddDraft("wf-1", "draft-1", api.WorkflowFile{Path: "main.py", Content: "old\n"})
	signIn(t, f)
	root := cloneFolder(t, f, "wf-1")

	// Someone edits the same per-user draft in the web builder.
	f.putFile("draft-1", "main.py", "theirs\n")
	writeLocal(t, root, "main.py", "mine\n")

	_, err := runCLI(t, root, "wf", "push", "--json")
	if err == nil {
		t.Fatal("push overwrote a drifted draft")
	}
	if !strings.Contains(err.Error(), "changed on the server") {
		t.Errorf("error = %v, want it to name the drift", err)
	}
	if got := f.FileContents("draft-1")["main.py"]; got != "theirs\n" {
		t.Errorf("draft main.py = %q — the refused push wrote anyway", got)
	}

	if _, err := runCLI(t, root, "wf", "push", "--force", "--json"); err != nil {
		t.Fatalf("forced push: %v", err)
	}
	if got := f.FileContents("draft-1")["main.py"]; got != "mine\n" {
		t.Errorf("after --force main.py = %q", got)
	}
}

// The title and the entrypoint are pushed as blindly as the code is, so they
// need the same guard: without it a colleague renaming the workflow in the web
// builder is silently reverted by the next sync.
func TestPushRefusesARemoteTitleChangeUnlessForced(t *testing.T) {
	f := newFakeInstance(t)
	f.AddWorkflow(&api.Workflow{ID: "wf-1", Lifecycle: api.LifecycleLive, Title: "Monthly report"},
		api.WorkflowFile{Path: "main.py", Content: "M\n"})
	f.AddDraft("wf-1", "draft-1", api.WorkflowFile{Path: "main.py", Content: "M\n"})
	signIn(t, f)
	root := cloneFolder(t, f, "wf-1")

	// Two renames of the same row: theirs in the builder, mine in the manifest.
	// Not one file differs, so the refusal can only be about the metadata.
	f.workflows["draft-1"].Title = "Their name"
	retitle(t, root, "My name")

	_, err := runCLI(t, root, "wf", "push", "--json")
	if err == nil {
		t.Fatal("push reverted a rename made on the server")
	}
	for _, want := range []string{"title", "Their name", "Monthly report", "--force"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error = %v, want it to mention %q", err, want)
		}
	}
	if f.titlePatches["draft-1"] != "" {
		t.Errorf("the refused push patched anyway: %+v", f.titlePatches)
	}

	if _, err := runCLI(t, root, "wf", "push", "--force", "--json"); err != nil {
		t.Fatalf("forced push: %v", err)
	}
	if f.titlePatches["draft-1"] != "My name" {
		t.Errorf("title patches = %+v, want My name", f.titlePatches)
	}
}

// Nothing to refuse when the remote already holds what the manifest wants: the
// push would not change the title, so there is nothing of theirs to overwrite.
// It still RECORDS the new title, so the guard keeps measuring against the row
// as it now is.
func TestPushAcceptsARemoteTitleChangeItAgreesWith(t *testing.T) {
	f := newFakeInstance(t)
	f.AddWorkflow(&api.Workflow{ID: "wf-1", Lifecycle: api.LifecycleLive, Title: "Monthly report"},
		api.WorkflowFile{Path: "main.py", Content: "M\n"})
	f.AddDraft("wf-1", "draft-1", api.WorkflowFile{Path: "main.py", Content: "M\n"})
	signIn(t, f)
	root := cloneFolder(t, f, "wf-1")

	f.workflows["draft-1"].Title = "Same idea"
	retitle(t, root, "Same idea")

	if _, err := runCLI(t, root, "wf", "push", "--json"); err != nil {
		t.Fatalf("push refused a rename it agreed with: %v", err)
	}
	if len(f.titlePatches) != 0 {
		t.Errorf("patched a title that already matched: %+v", f.titlePatches)
	}
	state, err := wfdir.LoadState(root)
	if err != nil {
		t.Fatalf("load state: %v", err)
	}
	if got := state.For(f.Key()).Title; got != "Same idea" {
		t.Errorf("baseline title = %q, want the row's current one", got)
	}
}

// A state file written before the metadata was recorded must not start refusing
// pushes: no recorded title means no guard, and the first push to write a
// baseline arms it.
func TestPushWithALegacyBaselineHasNoMetadataGuard(t *testing.T) {
	f := newFakeInstance(t)
	f.AddWorkflow(&api.Workflow{ID: "wf-1", Lifecycle: api.LifecycleLive, Title: "Monthly report"},
		api.WorkflowFile{Path: "main.py", Content: "M\n"})
	f.AddDraft("wf-1", "draft-1", api.WorkflowFile{Path: "main.py", Content: "M\n"})
	signIn(t, f)
	root := cloneFolder(t, f, "wf-1")

	// The shape a folder synced by an older CLI has on disk.
	state, err := wfdir.LoadState(root)
	if err != nil {
		t.Fatalf("load state: %v", err)
	}
	legacy := state.For(f.Key())
	legacy.Title, legacy.Entrypoint = "", ""
	state.Set(f.Key(), legacy)
	if err := wfdir.SaveState(root, state); err != nil {
		t.Fatalf("save state: %v", err)
	}

	f.workflows["draft-1"].Title = "Their name"
	retitle(t, root, "My name")
	if _, err := runCLI(t, root, "wf", "push", "--json"); err != nil {
		t.Fatalf("a folder with no recorded metadata was refused: %v", err)
	}
	if f.titlePatches["draft-1"] != "My name" {
		t.Errorf("title patches = %+v", f.titlePatches)
	}
	// And the baseline it just wrote arms the guard for the next one.
	after, err := wfdir.LoadState(root)
	if err != nil {
		t.Fatalf("load state: %v", err)
	}
	if got := after.For(f.Key()); got.Title != "My name" || got.Entrypoint != "main.py" {
		t.Errorf("baseline = %+v, want the metadata recorded", got)
	}
}

// A --force push that STOPS must not disarm the guard for the next one. The
// forced content is not ours until it lands, so the baseline written on the way
// out records only what the folder may honestly claim — never the colleague's
// bytes, which would turn the retry into a silent overwrite.
func TestPushForcedThatFailsDoesNotBaselineTheDriftedContent(t *testing.T) {
	f := newFakeInstance(t)
	f.AddWorkflow(&api.Workflow{ID: "wf-1", Lifecycle: api.LifecycleLive},
		api.WorkflowFile{Path: "main.py", Content: "M\n"},
		api.WorkflowFile{Path: "lib/helpers.py", Content: "H\n"})
	f.AddDraft("wf-1", "draft-1",
		api.WorkflowFile{Path: "main.py", Content: "M\n"},
		api.WorkflowFile{Path: "lib/helpers.py", Content: "H\n"})
	signIn(t, f)
	root := cloneFolder(t, f, "wf-1")

	// They edit one file in the builder; I edit another and force past it.
	f.putFile("draft-1", "lib/helpers.py", "theirs\n")
	writeLocal(t, root, "main.py", "mine\n")
	f.failPut["main.py"] = 500

	if _, err := runCLI(t, root, "wf", "push", "--force", "--json"); err == nil {
		t.Fatal("push reported success through a rejected file")
	}
	state, err := wfdir.LoadState(root)
	if err != nil {
		t.Fatalf("load state: %v", err)
	}
	if got := state.For(f.Key()).Files["lib/helpers.py"].SHA256; got == wfdir.HashString("theirs\n") {
		t.Error("the failed --force recorded the colleague's content as the baseline")
	}

	// Which is what keeps the retry honest: nothing landed, so their change is
	// still theirs and still has to be forced past deliberately.
	delete(f.failPut, "main.py")
	_, err = runCLI(t, root, "wf", "push", "--json")
	if err == nil {
		t.Fatal("the retry overwrote the drift the --force never got to")
	}
	if !strings.Contains(err.Error(), "changed on the server") {
		t.Errorf("error = %v, want the drift refusal", err)
	}
}

// A push with nothing to WRITE can still have something to RECORD: it checked
// out a fresh draft on the way here, and a baseline that goes on naming the row
// the files came from describes a row the next push is not comparing against.
func TestPushUpToDateAfterACheckoutRecordsTheNewDraft(t *testing.T) {
	f := newFakeInstance(t)
	f.AddWorkflow(&api.Workflow{ID: "wf-1", Lifecycle: api.LifecycleLive},
		api.WorkflowFile{Path: "main.py", Content: "M\n"})
	signIn(t, f)
	// No draft yet, so the baseline this leaves names the LIVE row.
	root := cloneFolder(t, f, "wf-1")

	out, err := runCLI(t, root, "wf", "push", "--json")
	if err != nil {
		t.Fatalf("push: %v", err)
	}
	payload := decodeJSON(t, out)
	if !payload["upToDate"].(bool) {
		t.Errorf("upToDate = false on a push with nothing to write: %s", out)
	}
	draftID := payload["draftID"].(string)
	state, err := wfdir.LoadState(root)
	if err != nil {
		t.Fatalf("load state: %v", err)
	}
	if got := state.For(f.Key()).SourceID; got != draftID {
		t.Errorf("baseline sourceID = %s, want the draft %s it just checked out", got, draftID)
	}
	assertBaselineMatchesDisk(t, root, f.Key())
}

// The crash window in a first push: the binding is recorded in ronja.json the
// moment the workflow exists, BEFORE any file is written, so a process that dies
// there leaves a folder that is bound, has no baseline, and faces a row already
// holding the entrypoint file the create seeded. That is the CLI's own
// half-finished work, and refusing it with a message about somebody else's
// changes sends the author looking for a colleague who does not exist.
func TestPushResumesAFirstPushThatDiedBeforeTheBaseline(t *testing.T) {
	f := newFakeInstance(t)
	f.AddWorkflow(&api.Workflow{ID: "wf-new-1", Lifecycle: api.LifecycleDraft, Hidden: true},
		api.WorkflowFile{Path: "main.py", Content: fakeEntrypointStarter})
	signIn(t, f)
	root := bindFolder(t, f, "wf-new-1", map[string]string{"main.py": "M\n"})

	if _, err := runCLI(t, root, "wf", "push", "--json"); err != nil {
		t.Fatalf("push refused its own half-finished first push: %v", err)
	}
	if got := f.FileContents("wf-new-1")["main.py"]; got != "M\n" {
		t.Errorf("main.py = %q", got)
	}
	assertBaselineMatchesDisk(t, root, f.Key())

	// And it stays narrow. A second file on the row is a workflow somebody
	// built, so the missing baseline is a real question again.
	f2 := newFakeInstance(t)
	f2.AddWorkflow(&api.Workflow{ID: "wf-new-1", Lifecycle: api.LifecycleDraft, Hidden: true},
		api.WorkflowFile{Path: "main.py", Content: fakeEntrypointStarter},
		api.WorkflowFile{Path: "lib/helpers.py", Content: "H\n"})
	signIn(t, f2)
	root2 := bindFolder(t, f2, "wf-new-1", map[string]string{"main.py": "M\n"})
	if _, err := runCLI(t, root2, "wf", "push", "--json"); err == nil {
		t.Fatal("push overwrote a draft it had no baseline for")
	}
}

// A PUT that timed out may still have committed — the deadline was ours, the
// transaction was the server's — so the push asks once. What it may RECORD is
// only what it was trying to write: the read answers with the server's current
// content, which is a different question, and the difference is the whole
// safety property (see TestPushNeverAcknowledgesContentItDidNotWrite).
func TestReconcileTimedOutPutRecordsOnlyWhatItMeantToWrite(t *testing.T) {
	f := newFakeInstance(t)
	f.AddWorkflow(&api.Workflow{ID: "wf-1", Lifecycle: api.LifecycleDraft},
		api.WorkflowFile{Path: "main.py", Content: "landed\n"},
		api.WorkflowFile{Path: "theirs.py", Content: "COLLEAGUE\n"})
	client := api.New(f.URL(), "test-token")

	landed := map[string]string{}
	if !reconcileTimedOutPut(context.Background(), client, aliasCodec{}, "wf-1", "main.py", "landed\n", landed) {
		t.Error("the server holds exactly what the write was trying to leave behind — that is a write that landed")
	}
	if landed["main.py"] != "landed\n" {
		t.Errorf("landed = %+v, want the content the write meant to leave behind", landed)
	}

	// The server holds something else, so the write did NOT land — and that
	// something else is nobody's business of ours to acknowledge.
	other := map[string]string{}
	if reconcileTimedOutPut(context.Background(), client, aliasCodec{}, "wf-1", "theirs.py", "mine\n", other) {
		t.Error("a file holding somebody else's content was reported as a write that landed")
	}
	if len(other) != 0 {
		t.Errorf("landed = %+v — the baseline acknowledged content this push never wrote", other)
	}

	// An answer we cannot get changes nothing: the entry keeps whatever the
	// baseline already acknowledged, rather than claiming a write that may not
	// have happened.
	unknown := map[string]string{"gone.py": "acknowledged\n"}
	reconcileTimedOutPut(context.Background(), client, aliasCodec{}, "wf-1", "gone.py", "mine\n", unknown)
	if unknown["gone.py"] != "acknowledged\n" {
		t.Errorf("landed = %+v — a failed re-read rewrote the baseline", unknown)
	}
}

// TestPushNeverAcknowledgesContentItDidNotWrite is the workflow twin of
// TestAppPushNeverAcknowledgesContentItDidNotWrite, and the same silent data
// loss it pins.
//
// A --force push runs past a colleague's change. One file's PUT then times out
// WITHOUT landing, so the reconciling read comes back holding their content —
// not ours. Recording that acknowledges a change nobody here made and disarms
// the drift guard, so the next plain push overwrites their work without a word.
// The end-to-end consequence is what is asserted: the retry must still be
// refused, and their file must still be theirs.
func TestPushNeverAcknowledgesContentItDidNotWrite(t *testing.T) {
	f := newFakeInstance(t)
	f.AddWorkflow(&api.Workflow{ID: "wf-1", Lifecycle: api.LifecycleLive},
		api.WorkflowFile{Path: "main.py", Content: "M\n"},
		api.WorkflowFile{Path: "lib/helpers.py", Content: "H\n"})
	f.AddDraft("wf-1", "draft-1",
		api.WorkflowFile{Path: "main.py", Content: "M\n"},
		api.WorkflowFile{Path: "lib/helpers.py", Content: "H\n"})
	signIn(t, f)
	root := cloneFolder(t, f, "wf-1")

	// They edit lib/helpers.py in the builder; I edit the same file here and
	// force past them.
	f.putFile("draft-1", "lib/helpers.py", "COLLEAGUE\n")
	writeLocal(t, root, "lib/helpers.py", "mine\n")
	// And that write dies on OUR deadline before the server ever sees it, which
	// is the one failure a workflow push goes and asks about.
	failOnceWithATimeout(t, "PUT", "/api/v2/workflow/draft-1/files/lib/helpers.py")

	if _, err := runCLI(t, root, "wf", "push", "--force", "--json"); err == nil {
		t.Fatal("push reported success through a timed-out file write")
	}
	state, err := wfdir.LoadState(root)
	if err != nil {
		t.Fatalf("load state: %v", err)
	}
	if got := state.For(f.Key()).Files["lib/helpers.py"].SHA256; got == wfdir.HashString("COLLEAGUE\n") {
		t.Error("the baseline acknowledged the colleague's content for a file this push never wrote — the next push would overwrite it in silence")
	}

	// The consequence, pinned end to end: with the timeout spent, the retry
	// WITHOUT --force must still see their change and refuse it.
	_, err = runCLI(t, root, "wf", "push", "--json")
	if err == nil {
		t.Fatal("the retry must still be refused as drift — the colleague's file was never acknowledged")
	}
	if !strings.Contains(err.Error(), "changed on the server") {
		t.Errorf("error = %v, want the drift refusal", err)
	}
	if got := f.FileContents("draft-1")["lib/helpers.py"]; got != "COLLEAGUE\n" {
		t.Errorf("the colleague's file was silently overwritten, got %q", got)
	}
}

// The CLI's per-file cap and the server's validate cap are deliberately the same
// number, so a folder the CLI refuses is one the server would refuse anyway and
// one it accepts is never rejected server-side for its size. The server pins the
// literal with its own test; this is the other half of that pair, so neither can
// move alone.
func TestMaxFileBytesMatchesTheServerValidateCap(t *testing.T) {
	if maxFileBytes != 1<<20 {
		t.Errorf("maxFileBytes = %d, want 1<<20 — rworkflow.maxValidateFileBytes is the same literal",
			maxFileBytes)
	}
}

// A push with nothing to do writes nothing at all — not one file, not the
// metadata, and not the baseline. The baseline matters: rewriting it on a no-op
// would move its timestamp for no reason, and the whole point of the state file
// is that it describes a sync that actually happened.
func TestPushWithNothingToDoWritesNothing(t *testing.T) {
	f := newFakeInstance(t)
	f.AddWorkflow(&api.Workflow{ID: "wf-1", Lifecycle: api.LifecycleLive},
		api.WorkflowFile{Path: "main.py", Content: "M\n"})
	f.AddDraft("wf-1", "draft-1", api.WorkflowFile{Path: "main.py", Content: "M\n"})
	signIn(t, f)
	root := cloneFolder(t, f, "wf-1")
	before := readFile(t, root, ".ronja", "state.json")

	out, err := runCLI(t, root, "wf", "push", "--json")
	if err != nil {
		t.Fatalf("push: %v", err)
	}
	payload := decodeJSON(t, out)
	if !payload["upToDate"].(bool) {
		t.Errorf("upToDate = false on a push with nothing to do: %s", out)
	}
	if writes := fileAndMetadataWrites(f); len(writes) != 0 {
		t.Errorf("wrote %v", writes)
	}
	for _, req := range f.Requests {
		if strings.HasPrefix(req, "POST ") && !strings.HasSuffix(req, "/validate") {
			t.Errorf("made a state-changing call: %v", f.Requests)
		}
	}
	if after := readFile(t, root, ".ronja", "state.json"); after != before {
		t.Errorf("the baseline was rewritten by a no-op push:\n%s\n%s", before, after)
	}
}

// The colleague case: a git clone of somebody's workflow folder has ronja.json
// but no .ronja/, so there is no baseline to decide drift against and the CLI
// cannot tell "unchanged" from "about to overwrite their work".
func TestPushRefusesAFolderWithNoBaseline(t *testing.T) {
	f := newFakeInstance(t)
	f.AddWorkflow(&api.Workflow{ID: "wf-1", Lifecycle: api.LifecycleLive},
		api.WorkflowFile{Path: "main.py", Content: "theirs\n"})
	signIn(t, f)
	root := bindFolder(t, f, "wf-1", map[string]string{"main.py": "mine\n"})

	_, err := runCLI(t, root, "wf", "push", "--json")
	if err == nil {
		t.Fatal("push overwrote a workflow it had no baseline for")
	}
	for _, want := range []string{"no sync baseline", "--force"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error = %v, want it to mention %q", err, want)
		}
	}
	// A draft was checked out before the guard ran — that is the point at which
	// the remote file set becomes readable — but nothing was written into it.
	if writes := fileAndMetadataWrites(f); len(writes) != 0 {
		t.Errorf("the refused push wrote %v", writes)
	}
}

// --force through a missing baseline, where the remote happens to hold exactly
// what the folder holds. Pinning the behaviour rather than asserting a
// preference: the file writes are SKIPPED (the content comparison is against
// what the server has, not against the baseline), and the baseline is still
// recorded — a forced push has one to write even when it writes no files.
func TestPushForcedWithIdenticalRemoteSkipsTheWritesAndRecordsTheBaseline(t *testing.T) {
	f := newFakeInstance(t)
	f.AddWorkflow(&api.Workflow{ID: "wf-1", Lifecycle: api.LifecycleLive},
		api.WorkflowFile{Path: "main.py", Content: "same\n"})
	signIn(t, f)
	root := bindFolder(t, f, "wf-1", map[string]string{"main.py": "same\n"})

	out, err := runCLI(t, root, "wf", "push", "--force", "--json")
	if err != nil {
		t.Fatalf("forced push: %v", err)
	}
	payload := decodeJSON(t, out)
	if pushed := payload["pushed"].([]any); len(pushed) != 0 {
		t.Errorf("pushed = %v, want nothing (the content already matched)", pushed)
	}
	if payload["upToDate"].(bool) {
		t.Error("upToDate = true on a push that had no baseline to be up to date against")
	}
	if writes := fileAndMetadataWrites(f); len(writes) != 0 {
		t.Errorf("wrote %v", writes)
	}
	assertBaselineMatchesDisk(t, root, f.Key())
}

// Pushing into a draft an admin is already reviewing changes what they are
// looking at. Allowed — it is the drafter's own draft — but never silent.
func TestPushWarnsWhenTheDraftIsUnderReview(t *testing.T) {
	f := newFakeInstance(t)
	f.AddWorkflow(&api.Workflow{ID: "wf-1", Lifecycle: api.LifecycleLive},
		api.WorkflowFile{Path: "main.py", Content: "M\n"})
	draft := f.AddDraft("wf-1", "draft-1", api.WorkflowFile{Path: "main.py", Content: "M\n"})
	submitted := time.Date(2026, 7, 28, 11, 0, 0, 0, time.UTC)
	draft.SubmittedForReviewAt = &submitted
	signIn(t, f)
	root := cloneFolder(t, f, "wf-1")

	writeLocal(t, root, "main.py", "M2\n")
	out, err := runCLI(t, root, "wf", "push", "--json")
	if err != nil {
		t.Fatalf("push: %v", err)
	}
	if !decodeJSON(t, out)["draftUnderReview"].(bool) {
		t.Errorf("draftUnderReview = false pushing into a submitted draft: %s", out)
	}

	// And an ordinary draft does not claim to be under review.
	f2 := newFakeInstance(t)
	f2.AddWorkflow(&api.Workflow{ID: "wf-1", Lifecycle: api.LifecycleLive},
		api.WorkflowFile{Path: "main.py", Content: "M\n"})
	signIn(t, f2)
	root2 := cloneFolder(t, f2, "wf-1")
	writeLocal(t, root2, "main.py", "M2\n")
	out2, err := runCLI(t, root2, "wf", "push", "--json")
	if err != nil {
		t.Fatalf("push: %v", err)
	}
	if decodeJSON(t, out2)["draftUnderReview"].(bool) {
		t.Errorf("draftUnderReview = true on a draft nobody submitted: %s", out2)
	}
}

// The baseline's timestamp describes the row AFTER the push, not the row as it
// was when the target was resolved — the file writes moved it.
func TestPushBaselineTimestampComesFromTheRereadRow(t *testing.T) {
	f := newFakeInstance(t)
	f.AddWorkflow(&api.Workflow{ID: "wf-1", Lifecycle: api.LifecycleLive},
		api.WorkflowFile{Path: "main.py", Content: "M\n"})
	before := f.AddDraft("wf-1", "draft-1", api.WorkflowFile{Path: "main.py", Content: "M\n"}).UpdatedAt
	signIn(t, f)
	root := cloneFolder(t, f, "wf-1")

	writeLocal(t, root, "main.py", "M2\n")
	if _, err := runCLI(t, root, "wf", "push", "--json"); err != nil {
		t.Fatalf("push: %v", err)
	}
	state, err := wfdir.LoadState(root)
	if err != nil {
		t.Fatalf("load state: %v", err)
	}
	stamped := state.For(f.Key()).BaselineUpdatedAt
	if stamped == before.UTC().Format(time.RFC3339) {
		t.Errorf("baselineUpdatedAt = %s — the pre-push row, not the re-read one", stamped)
	}
	if want := f.workflows["draft-1"].UpdatedAt.UTC().Format(time.RFC3339); stamped != want {
		t.Errorf("baselineUpdatedAt = %s, want %s", stamped, want)
	}
}

// --- the validate gate ------------------------------------------------------

func TestPushRefusesOnValidationErrorsAndWritesNothing(t *testing.T) {
	f := newFakeInstance(t)
	f.validate = &api.ValidateResult{Findings: []api.ValidateFinding{{
		Severity: api.SeverityError, Code: "unresolved_ref",
		Message: `"table-gone" isn't a table reachable from any of your workspaces`,
		Path:    "main.py", Marker: "{{ ref('table-gone') }}",
	}}}
	signIn(t, f)
	root := initFolder(t, f, map[string]string{"main.py": "x\n"})

	out, err := runCLI(t, root, "wf", "push", "--json")
	if err == nil {
		t.Fatal("push proceeded through an error finding")
	}
	payload := decodeJSON(t, out)
	if payload["findings"] == nil {
		t.Errorf("payload carries no findings: %s", out)
	}
	// Nothing was created and nothing was written: the gate is the point.
	if len(f.created) != 0 {
		t.Errorf("created a workflow despite the refusal: %+v", f.created)
	}
	if puts := requestsMatching(f, "PUT "); len(puts) != 0 {
		t.Errorf("issued file writes despite the refusal: %v", puts)
	}
}

func TestPushWarningsDoNotBlockAndRideAlongInTheResult(t *testing.T) {
	f := newFakeInstance(t)
	// Deliberately NOT secret_dropped: that one code is a dropped BINDING, and a
	// push that lands with one exits non-zero (see the tests below). Every other
	// warning is advice, and advice does not fail a deploy.
	f.validate = &api.ValidateResult{Findings: []api.ValidateFinding{{
		Severity: "warning", Code: "invalid_params",
		Message: "parameter \"month\" has no label", Path: "main.py",
	}}}
	signIn(t, f)
	root := initFolder(t, f, map[string]string{"main.py": "x\n"})

	out, err := runCLI(t, root, "wf", "push", "--json")
	if err != nil {
		t.Fatalf("a warning blocked the push: %v", err)
	}
	// Under --json the stderr warning is invisible, so the finding has to be in
	// the payload or a script never sees it at all.
	payload := decodeJSON(t, out)
	findings, ok := payload["findings"].([]any)
	if !ok || len(findings) != 1 {
		t.Fatalf("findings = %v, want the warning: %s", payload["findings"], out)
	}
	if payload["error"] != nil {
		t.Errorf("error = %v on a push that succeeded", payload["error"])
	}
	if payload["droppedBindings"] != nil {
		t.Errorf("droppedBindings = %v on a warning that is not a dropped binding", payload["droppedBindings"])
	}
}

// A `{{ secret }}` marker naming a secret the author cannot reach is the
// server's SOFT tier: the workflow saves, the binding is filtered out, and every
// run that touches the marker fails. The push used to exit ZERO through that —
// so a CI job passed and shipped a workflow that cannot run.
//
// The severity stays a warning (it is the right tier for a person about to
// create the secret); the EXIT CODE is what was wrong, because it is the only
// thing CI reads.
func TestPushExitsNonZeroWhenTheSaveDropsABinding(t *testing.T) {
	f := newFakeInstance(t)
	f.validate = &api.ValidateResult{Findings: []api.ValidateFinding{{
		Severity: "warning", Code: api.FindingSecretDropped,
		Message: `secret "secret-other-org" isn't reachable to you`, Path: "main.py",
	}}}
	signIn(t, f)
	root := initFolder(t, f, map[string]string{"main.py": "x\n"})

	out, err := runCLI(t, root, "wf", "push", "--json")
	if err == nil {
		t.Fatal("push exited zero with a dropped binding — CI would ship a workflow that fails at run time")
	}
	if !strings.Contains(err.Error(), "--allow-dropped-bindings") {
		t.Errorf("error = %v, want the opt-out named", err)
	}
	payload := decodeJSON(t, out)
	// The push LANDED. This is an exit code, not a refusal: the workflow was
	// created and the files written, and a report that said otherwise would send
	// the author looking for work that is on the server.
	if payload["created"] != true {
		t.Errorf("created = %v — the push should still have landed", payload["created"])
	}
	if len(f.created) != 1 {
		t.Errorf("created workflows = %+v, want the push to have landed", f.created)
	}
	dropped, ok := payload["droppedBindings"].([]any)
	if !ok || len(dropped) != 1 {
		t.Fatalf("droppedBindings = %v, want the one drop: %s", payload["droppedBindings"], out)
	}
	if !strings.Contains(dropped[0].(string), "secret-other-org") {
		t.Errorf("droppedBindings = %v, want the id named", dropped)
	}
}

// The opt-out, for the "I will bind it later" flow. A flag, never a TTY test or
// a --json test: the exit code has to mean the same thing wherever it is read.
func TestPushAllowsDroppedBindingsWhenAsked(t *testing.T) {
	f := newFakeInstance(t)
	f.validate = &api.ValidateResult{Findings: []api.ValidateFinding{{
		Severity: "warning", Code: api.FindingSecretDropped,
		Message: `secret "secret-later" isn't reachable to you`, Path: "main.py",
	}}}
	signIn(t, f)
	root := initFolder(t, f, map[string]string{"main.py": "x\n"})

	out, err := runCLI(t, root, "wf", "push", "--json", "--allow-dropped-bindings")
	if err != nil {
		t.Fatalf("--allow-dropped-bindings did not accept the drop: %v", err)
	}
	payload := decodeJSON(t, out)
	// Still REPORTED. The flag accepts the drop; it does not hide it.
	if got, ok := payload["droppedBindings"].([]any); !ok || len(got) != 1 {
		t.Errorf("droppedBindings = %v, want the drop still reported: %s", payload["droppedBindings"], out)
	}
	if payload["droppedBindingsAllowed"] != true {
		t.Errorf("droppedBindingsAllowed = %v, want true", payload["droppedBindingsAllowed"])
	}
}

// A second push of an unchanged folder is the shape CI actually runs: the
// re-run of the deploy that just failed. It has nothing to write, and the
// workflow is still broken, so it must not go green.
func TestPushUpToDateStillRefusesADroppedBinding(t *testing.T) {
	f := newFakeInstance(t)
	f.AddWorkflow(&api.Workflow{ID: "wf-1", Lifecycle: api.LifecycleDraft},
		api.WorkflowFile{Path: "main.py", Content: "x\n"})
	signIn(t, f)
	root := cloneFolder(t, f, "wf-1")
	f.validate = &api.ValidateResult{Findings: []api.ValidateFinding{{
		Severity: "warning", Code: api.FindingSecretDropped,
		Message: `secret "secret-other-org" isn't reachable to you`, Path: "main.py",
	}}}

	if _, err := runCLI(t, root, "wf", "push", "--json"); err == nil {
		t.Fatal("the first push exited zero with a dropped binding")
	}
	out, err := runCLI(t, root, "wf", "push", "--json")
	if err == nil {
		t.Fatal("the up-to-date re-push went green on a workflow that still carries the drop")
	}
	if payload := decodeJSON(t, out); payload["upToDate"] != true {
		t.Errorf("upToDate = %v, want the second push to have written nothing", payload["upToDate"])
	}
}

// The stale-parameter case validate-first exists to prevent: the row's SAVED
// parameters are checked alongside the files by a real save, so a validate that
// left them out passes and the first file write then fails — with a message
// about a parameter, attributed to whichever file happened to be in flight.
func TestPushValidatesAgainstTheTargetsSavedParameters(t *testing.T) {
	f := newFakeInstance(t)
	f.AddWorkflow(&api.Workflow{ID: "wf-1", Lifecycle: api.LifecycleLive,
		Parameters: []api.WorkflowParameter{{Name: "region", Type: "select", OptionsQuery: "SELECT r FROM {{ ref('tbl-live') }}"}}},
		api.WorkflowFile{Path: "main.py", Content: "M\n"})
	draft := f.AddDraft("wf-1", "draft-1", api.WorkflowFile{Path: "main.py", Content: "M\n"})
	// The draft's are the ones a push writes against, so they are the ones that
	// have to be sent — not the live row's.
	draft.Parameters = []api.WorkflowParameter{{Name: "region", Type: "select", OptionsQuery: "SELECT r FROM {{ ref('tbl-draft') }}"}}
	signIn(t, f)
	root := cloneFolder(t, f, "wf-1")

	writeLocal(t, root, "main.py", "M2\n")
	if _, err := runCLI(t, root, "wf", "push", "--json"); err != nil {
		t.Fatalf("push: %v", err)
	}
	if len(f.validated) != 1 {
		t.Fatalf("validate calls = %d, want 1", len(f.validated))
	}
	params := f.validated[0].Parameters
	if len(params) != 1 || params[0].OptionsQuery != "SELECT r FROM {{ ref('tbl-draft') }}" {
		t.Errorf("validated parameters = %+v, want the draft's", params)
	}
}

// setManifestParameters marks a folder as MANAGING parameters and records the
// declaration, the way a hand-edited ronja.json does.
func setManifestParameters(t *testing.T, root string, params ...api.WorkflowParameter) {
	t.Helper()
	m, err := wfdir.LoadManifest(root, wfdir.WorkflowKind)
	if err != nil {
		t.Fatalf("load manifest: %v", err)
	}
	m.SetParameters(params)
	if err := wfdir.SaveManifest(root, m); err != nil {
		t.Fatalf("save manifest: %v", err)
	}
}

// The point of the whole feature: the manifest declares the parameters and a
// push makes the row match, on the create as well as on an update.
func TestPushSyncsTheManifestsDeclaredParameters(t *testing.T) {
	f := newFakeInstance(t)
	signIn(t, f)
	root := initFolder(t, f, map[string]string{"main.py": "x\n"})
	setManifestParameters(t, root, api.WorkflowParameter{Name: "upto", Label: "Up to", Type: "number", Required: true})

	if _, err := runCLI(t, root, "wf", "push", "--json"); err != nil {
		t.Fatalf("push: %v", err)
	}
	if len(f.created) != 1 {
		t.Fatalf("created = %d, want 1", len(f.created))
	}
	if got := f.created[0].Parameters; len(got) != 1 || got[0].Name != "upto" || got[0].Type != "number" {
		t.Fatalf("created with parameters %+v, want the declared one", got)
	}

	// And a later change is patched onto the existing row.
	setManifestParameters(t, root, api.WorkflowParameter{Name: "upto", Label: "Up to", Type: "number", Required: true},
		api.WorkflowParameter{Name: "region", Label: "Region", Type: "string"})
	writeLocal(t, root, "main.py", "x2\n")
	if _, err := runCLI(t, root, "wf", "push", "--json"); err != nil {
		t.Fatalf("second push: %v", err)
	}
	patched := f.parameterPatches["wf-new-1"]
	if patched == nil {
		t.Fatalf("second push patched no parameters; patches = %+v", f.parameterPatches)
	}
	if len(*patched) != 2 || (*patched)[1].Name != "region" {
		t.Errorf("patched parameters = %+v, want both declared", *patched)
	}
}

// The back-compat invariant, and the reason Manifest.Parameters is a pointer: a
// folder created before parameters were part of the manifest does not manage
// them, so a push must leave the row's declaration completely alone rather than
// read "no key" as "declares none" and wipe it.
func TestPushLeavesParametersAloneForAFolderThatDoesNotManageThem(t *testing.T) {
	f := newFakeInstance(t)
	f.AddWorkflow(&api.Workflow{ID: "wf-1", Lifecycle: api.LifecycleLive,
		Parameters: []api.WorkflowParameter{{Name: "region", Type: "string"}}},
		api.WorkflowFile{Path: "main.py", Content: "M\n"})
	signIn(t, f)
	root := cloneFolder(t, f, "wf-1")

	// Drop the key the clone wrote — this is a pre-parameters folder.
	m, err := wfdir.LoadManifest(root, wfdir.WorkflowKind)
	if err != nil {
		t.Fatalf("load manifest: %v", err)
	}
	m.Parameters = nil
	if err := wfdir.SaveManifest(root, m); err != nil {
		t.Fatalf("save manifest: %v", err)
	}

	writeLocal(t, root, "main.py", "M2\n")
	if _, err := runCLI(t, root, "wf", "push", "--json"); err != nil {
		t.Fatalf("push: %v", err)
	}
	for id, p := range f.parameterPatches {
		t.Fatalf("push patched parameters on %s (%+v) for an unmanaged folder", id, p)
	}
	if got := f.workflows["wf-1"].Parameters; len(got) != 1 || got[0].Name != "region" {
		t.Errorf("row parameters = %+v, want the original left intact", got)
	}
}

// Removing the last parameter has to be expressible, which is what separates an
// explicit [] from an absent key: the patch must carry a non-nil empty list, or
// the server reads it as "unchanged" and the parameter can never be deleted.
func TestPushClearsParametersWhenTheManifestDeclaresNone(t *testing.T) {
	f := newFakeInstance(t)
	f.AddWorkflow(&api.Workflow{ID: "wf-1", Lifecycle: api.LifecycleLive,
		Parameters: []api.WorkflowParameter{{Name: "region", Type: "string"}}},
		api.WorkflowFile{Path: "main.py", Content: "M\n"})
	signIn(t, f)
	root := cloneFolder(t, f, "wf-1")
	setManifestParameters(t, root) // managed, declaring none

	writeLocal(t, root, "main.py", "M2\n")
	if _, err := runCLI(t, root, "wf", "push", "--json"); err != nil {
		t.Fatalf("push: %v", err)
	}
	// Onto the DRAFT: a push never writes to the live row, and the declaration
	// reaches the parent when the draft is committed.
	draftID := f.draftOf["wf-1"]
	patched, ok := f.parameterPatches[draftID]
	if !ok {
		t.Fatalf("no parameter patch sent to draft %q; patches = %+v", draftID, f.parameterPatches)
	}
	if patched == nil {
		t.Fatalf("parameter patch was nil — the server reads that as unchanged")
	}
	if len(*patched) != 0 {
		t.Errorf("patched parameters = %+v, want an explicit empty list", *patched)
	}
}

// A managed folder validates what it DECLARES, not what the row currently holds
// — otherwise a newly declared select parameter's optionsQuery markers are
// pushed without ever having been checked.
func TestPushValidatesTheDeclaredParametersNotTheRows(t *testing.T) {
	f := newFakeInstance(t)
	f.AddWorkflow(&api.Workflow{ID: "wf-1", Lifecycle: api.LifecycleLive,
		Parameters: []api.WorkflowParameter{{Name: "region", Type: "select", OptionsQuery: "SELECT r FROM {{ ref('tbl-old') }}"}}},
		api.WorkflowFile{Path: "main.py", Content: "M\n"})
	signIn(t, f)
	root := cloneFolder(t, f, "wf-1")
	setManifestParameters(t, root, api.WorkflowParameter{
		Name: "region", Type: "select", OptionsQuery: "SELECT r FROM {{ ref('tbl-new') }}"})

	writeLocal(t, root, "main.py", "M2\n")
	if _, err := runCLI(t, root, "wf", "push", "--json"); err != nil {
		t.Fatalf("push: %v", err)
	}
	if len(f.validated) != 1 {
		t.Fatalf("validate calls = %d, want 1", len(f.validated))
	}
	got := f.validated[0].Parameters
	if len(got) != 1 || got[0].OptionsQuery != "SELECT r FROM {{ ref('tbl-new') }}" {
		t.Errorf("validated parameters = %+v, want the manifest's declaration", got)
	}
}

// A clone is a faithful copy, so the folder it produces manages the parameters
// it can plainly see — otherwise the first push would silently not sync them.
func TestCloneRecordsTheWorkflowsParameters(t *testing.T) {
	f := newFakeInstance(t)
	f.AddWorkflow(&api.Workflow{ID: "wf-1", Lifecycle: api.LifecycleLive,
		Parameters: []api.WorkflowParameter{{Name: "month", Type: "date"}}},
		api.WorkflowFile{Path: "main.py", Content: "M\n"})
	signIn(t, f)
	root := cloneFolder(t, f, "wf-1")

	m, err := wfdir.LoadManifest(root, wfdir.WorkflowKind)
	if err != nil {
		t.Fatalf("load manifest: %v", err)
	}
	if !m.ManagesParameters() {
		t.Fatalf("cloned folder does not manage parameters")
	}
	if got := m.DeclaredParameters(); len(got) != 1 || got[0].Name != "month" {
		t.Errorf("manifest parameters = %+v, want the workflow's", got)
	}
}

// An unbound folder has no row to take parameters from, and must not claim one:
// the server reads an absent list as "no parameters", which is the honest
// answer for a workflow that does not exist yet.
func TestPushSendsNoParametersForAnUnboundFolder(t *testing.T) {
	f := newFakeInstance(t)
	signIn(t, f)
	root := initFolder(t, f, map[string]string{"main.py": "x\n"})

	if _, err := runCLI(t, root, "wf", "push", "--json"); err != nil {
		t.Fatalf("push: %v", err)
	}
	if len(f.validated) != 1 || len(f.validated[0].Parameters) != 0 {
		t.Errorf("validated = %+v, want no parameters", f.validated)
	}
}

func TestPushNoValidateSkipsTheCheck(t *testing.T) {
	f := newFakeInstance(t)
	f.validate = &api.ValidateResult{Findings: []api.ValidateFinding{{
		Severity: api.SeverityError, Code: "unresolved_ref", Path: "main.py",
	}}}
	signIn(t, f)
	root := initFolder(t, f, map[string]string{"main.py": "x\n"})

	if _, err := runCLI(t, root, "wf", "push", "--no-validate", "--json"); err != nil {
		t.Fatalf("push: %v", err)
	}
	for _, req := range f.Requests {
		if strings.HasSuffix(req, "/validate") {
			t.Errorf("--no-validate still called validate: %v", f.Requests)
		}
	}
}

// --- failures mid-push ------------------------------------------------------

func TestPushStopsAtARejectedFileAndSaysWhatLanded(t *testing.T) {
	f := newFakeInstance(t)
	f.failPut["lib/helpers.py"] = 400
	signIn(t, f)
	root := initFolder(t, f, map[string]string{
		"main.py":        "M\n",
		"lib/helpers.py": "H\n",
		"zzz.py":         "Z\n",
	})

	out, err := runCLI(t, root, "wf", "push", "--json")
	if err == nil {
		t.Fatal("push reported success through a rejected file")
	}
	if !strings.Contains(err.Error(), "lib/helpers.py") {
		t.Errorf("error = %v, want it to name the file", err)
	}
	payload := decodeJSON(t, out)
	pushed := payload["pushed"].([]any)
	if len(pushed) != 1 || pushed[0].(string) != "main.py" {
		t.Errorf("pushed = %v, want just the entrypoint", pushed)
	}
	if payload["error"] == nil {
		t.Error("payload carries no error field")
	}
	// Stopped rather than carried on: zzz.py sorts after the failure.
	if got := f.FileContents(payload["workflowID"].(string)); len(got) != 1 {
		t.Errorf("server holds %+v — the push continued past the failure", got)
	}

	// And the retry that fixes it is not refused as drift. The half-finished
	// push moved the server away from the baseline; if that were not recorded,
	// the author's own writes would come back at them as somebody else's
	// changes and the only way forward would be --force.
	delete(f.failPut, "lib/helpers.py")
	if _, err := runCLI(t, root, "wf", "push", "--json"); err != nil {
		t.Fatalf("retry after a partial push: %v", err)
	}
	if got := f.FileContents(payload["workflowID"].(string)); len(got) != 3 {
		t.Errorf("after the retry the server holds %+v", got)
	}
}

// A CREATE seeds a starter file at the entrypoint inside the same transaction,
// so a brand-new workflow is not empty. If the very first PUT — the
// entrypoint's own — then fails, the baseline recorded on the way out must
// still describe that seed. Recording zero files instead makes the retry read
// the server's own starter as somebody else's drift and demand --force for a
// workflow the CLI created seconds earlier.
func TestPushRetriesAfterAFailedFirstPushWithoutForce(t *testing.T) {
	f := newFakeInstance(t)
	f.failPut["main.py"] = 500
	signIn(t, f)
	root := initFolder(t, f, map[string]string{"main.py": "M\n"})

	out, err := runCLI(t, root, "wf", "push", "--json")
	if err == nil {
		t.Fatal("push reported success through a rejected entrypoint")
	}
	payload := decodeJSON(t, out)
	id := payload["workflowID"].(string)
	if payload["created"] != true {
		t.Fatalf("the workflow was not created: %s", out)
	}
	if len(payload["pushed"].([]any)) != 0 {
		t.Errorf("pushed = %v, want nothing to have landed", payload["pushed"])
	}
	// The baseline has to hold the SEED, because that is what the server holds.
	state, err := wfdir.LoadState(root)
	if err != nil {
		t.Fatalf("load state: %v", err)
	}
	baseline := state.For(f.Key())
	if baseline == nil || baseline.Files["main.py"].SHA256 != wfdir.HashString(fakeEntrypointStarter) {
		t.Fatalf("baseline = %+v, want it to describe the seeded entrypoint", baseline)
	}

	// The retry heals it, with no --force anywhere.
	delete(f.failPut, "main.py")
	if _, err := runCLI(t, root, "wf", "push", "--json"); err != nil {
		t.Fatalf("retry after a failed first push: %v", err)
	}
	if got := f.FileContents(id)["main.py"]; got != "M\n" {
		t.Errorf("main.py = %q after the retry", got)
	}
}

// Every file landed and then the metadata patch failed. The files are on the
// server whatever happened next, so the baseline has to say so — otherwise the
// author's own writes come back at them as drift on the retry.
func TestPushRecordsWhatLandedWhenTheMetadataPatchFails(t *testing.T) {
	f := newFakeInstance(t)
	f.AddWorkflow(&api.Workflow{ID: "wf-1", Lifecycle: api.LifecycleLive, Title: "Old name"},
		api.WorkflowFile{Path: "main.py", Content: "M\n"})
	f.failPatch = 500
	signIn(t, f)
	root := cloneFolder(t, f, "wf-1")

	writeLocal(t, root, "main.py", "M2\n")
	retitle(t, root, "New name")

	out, err := runCLI(t, root, "wf", "push", "--json")
	if err == nil {
		t.Fatal("push reported success through a failed metadata patch")
	}
	if !strings.Contains(err.Error(), "the title") {
		t.Errorf("error = %v, want it to name what it was changing", err)
	}
	if pushed := decodeJSON(t, out)["pushed"].([]any); len(pushed) != 1 {
		t.Errorf("pushed = %v, want the file that landed", pushed)
	}
	assertBaselineMatchesDisk(t, root, f.Key())

	// So the retry is a retry, not a drift refusal.
	f.failPatch = 0
	if _, err := runCLI(t, root, "wf", "push", "--json"); err != nil {
		t.Fatalf("retry after a failed metadata patch: %v", err)
	}
}

func TestPushRefusesBinaryAndOversizedFiles(t *testing.T) {
	f := newFakeInstance(t)
	signIn(t, f)
	root := initFolder(t, f, map[string]string{
		"main.py":  "x\n",
		"blob.dat": "abc\x00def",
	})

	_, err := runCLI(t, root, "wf", "push", "--json")
	if err == nil {
		t.Fatal("push accepted a file with null bytes")
	}
	if !strings.Contains(err.Error(), "blob.dat") {
		t.Errorf("error = %v, want it to name the file", err)
	}
	// The refusal is still LOCAL — no workflow was read, no draft opened, no file
	// written. The one request is the organization lookup openFolderLocally owes
	// an environment token before it may trust this folder's binding: without it
	// the binding is matched on URL alone and another organization's is adopted.
	// A stored profile makes even this one disappear.
	if got := requestsMatching(f, "GET /api/v2/authentication/me"); len(f.Requests) != len(got) {
		t.Errorf("made a request beyond the organization lookup before refusing: %v", f.Requests)
	}

	if err := os.Remove(filepath.Join(root, "blob.dat")); err != nil {
		t.Fatalf("remove: %v", err)
	}
	writeLocal(t, root, "big.py", strings.Repeat("x", maxFileBytes+1))
	if _, err := runCLI(t, root, "wf", "push", "--json"); err == nil {
		t.Fatal("push accepted an oversized file")
	} else if !strings.Contains(err.Error(), "big.py") {
		t.Errorf("error = %v, want it to name the file", err)
	}
}

// checkPushable's case-collision leg gets a unit test rather than a CLI one:
// the folder it refuses cannot be CREATED on the macOS and Windows machines
// this suite also runs on, which is the entire reason the check exists.
func TestCheckPushableRefusesCaseCollisions(t *testing.T) {
	err := checkPushable(map[string]string{
		"main.py":    "x",
		"Helpers.py": "y",
		"helpers.py": "z",
	}, wfdir.WorkflowKind, maxFileBytes)
	if err == nil {
		t.Fatal("accepted two paths differing only by case")
	}
	if !strings.Contains(err.Error(), "Helpers.py and helpers.py") {
		t.Errorf("error = %v, want it to name both paths", err)
	}
}

// The refusal names the folder's OWN primitive, and the workflow wording is
// pinned unchanged.
//
// checkPushable runs for all three folder loops, so a data app refused for
// holding a PNG was told "a workflow holds source code" — naming a primitive the
// author is not working on, which reads as a bug in the CLI rather than as
// something to act on. The hint leg is for the folder that already holds
// artefacts a previous CLI wrote into the root: those names are NOT excluded
// from the walk (a user's own report.json must still sync, or refuse loudly),
// so saying what they look like after the refusal is the only help available.
func TestCheckPushableNamesTheKindAndSpotsAppTestArtefacts(t *testing.T) {
	binary := map[string]string{"App.tsx": "x", "blob.dat": "abc\x00def"}
	artefacts := map[string]string{"App.tsx": "x", "screenshot-1.png": "\x89PNG\x00"}

	for _, tc := range []struct {
		name     string
		files    map[string]string
		kind     wfdir.Kind
		want     string
		wantHint bool
	}{
		{"workflow", binary, wfdir.WorkflowKind, "a workflow holds source code, not data or binaries", false},
		{"data app", binary, wfdir.DataAppKind, "a data app holds source code, not data or binaries", false},
		{"pipeline", binary, wfdir.PipelineKind, "a pipeline holds source code, not data or binaries", false},
		{"app test artefacts", artefacts, wfdir.DataAppKind, "a data app holds source code, not data or binaries", true},
		// The hint is the data-app loop's, and only that loop's. A workflow
		// folder holding a screenshot-1.png is holding something else's output:
		// `ronja wf` has no `test --out-dir` to move it with, so pointing the
		// author at one would be the same wrong-primitive answer the refusal
		// above it exists to stop giving.
		{"artefact-shaped file in a workflow folder", artefacts, wfdir.WorkflowKind, "a workflow holds source code, not data or binaries", false},
		{"artefact-shaped file in a pipeline folder", artefacts, wfdir.PipelineKind, "a pipeline holds source code, not data or binaries", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			err := checkPushable(tc.files, tc.kind, maxFileBytes)
			if err == nil {
				t.Fatal("accepted a file holding null bytes")
			}
			if !strings.Contains(err.Error(), tc.want) {
				t.Errorf("the refusal should name this folder's primitive (%q), got: %v", tc.want, err)
			}
			hinted := strings.Contains(err.Error(), "ronja app test")
			if hinted != tc.wantHint {
				t.Errorf("hint present = %v, want %v; got: %v", hinted, tc.wantHint, err)
			}
			// The remedy is the last line either way — the hint goes above it, not
			// in place of it.
			if !strings.HasSuffix(err.Error(), "Remove them from the folder (or keep them out of it) and try again") {
				t.Errorf("the refusal must still end on what to do about it, got: %v", err)
			}
		})
	}
}

func TestPushRefusesAMissingEntrypoint(t *testing.T) {
	f := newFakeInstance(t)
	signIn(t, f)
	root := initFolder(t, f, map[string]string{"other.py": "x\n"})

	_, err := runCLI(t, root, "wf", "push", "--json")
	if err == nil {
		t.Fatal("push accepted a folder with no entrypoint")
	}
	if !strings.Contains(err.Error(), "main.py") {
		t.Errorf("error = %v, want it to name the entrypoint", err)
	}
}

// --- deletions --------------------------------------------------------------

func TestPushDeletesFilesThatAreGoneLocally(t *testing.T) {
	f := newFakeInstance(t)
	f.AddWorkflow(
		&api.Workflow{ID: "wf-1", Lifecycle: api.LifecycleLive},
		api.WorkflowFile{Path: "main.py", Content: "M\n"},
		api.WorkflowFile{Path: "lib/old.py", Content: "O\n"},
	)
	signIn(t, f)
	root := cloneFolder(t, f, "wf-1")

	if err := os.Remove(filepath.Join(root, "lib", "old.py")); err != nil {
		t.Fatalf("remove: %v", err)
	}
	out, err := runCLI(t, root, "wf", "push", "--json")
	if err != nil {
		t.Fatalf("push: %v", err)
	}
	payload := decodeJSON(t, out)
	deleted := payload["deleted"].([]any)
	if len(deleted) != 1 || deleted[0].(string) != "lib/old.py" {
		t.Fatalf("deleted = %v", deleted)
	}
	draftID := payload["draftID"].(string)
	if _, still := f.FileContents(draftID)["lib/old.py"]; still {
		t.Error("the file is still on the draft")
	}
	// Unchanged files are not re-sent.
	if pushed := payload["pushed"].([]any); len(pushed) != 0 {
		t.Errorf("pushed = %v, want nothing (only a deletion happened)", pushed)
	}
	assertBaselineMatchesDisk(t, root, f.Key())
}

// --- target states ----------------------------------------------------------

func TestPushRefusesABrokenBinding(t *testing.T) {
	f := newFakeInstance(t)
	f.AddWorkflow(&api.Workflow{ID: "wf-1", Lifecycle: api.LifecycleLive},
		api.WorkflowFile{Path: "main.py", Content: "M\n"})
	signIn(t, f)
	root := cloneFolder(t, f, "wf-1")
	delete(f.workflows, "wf-1")

	_, err := runCLI(t, root, "wf", "push", "--json")
	if err == nil {
		t.Fatal("push accepted a binding whose workflow is gone")
	}
	if !strings.Contains(err.Error(), "no longer exists") {
		t.Errorf("error = %v", err)
	}
	// Never a silent replacement.
	if len(f.created) != 0 {
		t.Errorf("push created a replacement workflow: %+v", f.created)
	}
}

func TestPushRefusesAProposedTarget(t *testing.T) {
	f := newFakeInstance(t)
	f.AddWorkflow(&api.Workflow{ID: "wf-1", Lifecycle: api.LifecycleLive},
		api.WorkflowFile{Path: "main.py", Content: "M\n"})
	signIn(t, f)
	root := cloneFolder(t, f, "wf-1")
	f.workflows["wf-1"].Lifecycle = api.LifecycleProposed

	_, err := runCLI(t, root, "wf", "push", "--json")
	if err == nil {
		t.Fatal("push accepted a proposed row")
	}
	if !strings.Contains(err.Error(), "proposal") {
		t.Errorf("error = %v, want it to name the state", err)
	}
}

// --- metadata ---------------------------------------------------------------

func TestPushUpdatesTheTitleWhenTheManifestChanged(t *testing.T) {
	f := newFakeInstance(t)
	f.AddWorkflow(&api.Workflow{ID: "wf-1", Lifecycle: api.LifecycleLive, Title: "Old name"},
		api.WorkflowFile{Path: "main.py", Content: "M\n"})
	signIn(t, f)
	root := cloneFolder(t, f, "wf-1")

	manifest, err := wfdir.LoadManifest(root, wfdir.WorkflowKind)
	if err != nil {
		t.Fatalf("load manifest: %v", err)
	}
	manifest.Title = "New name"
	if err := wfdir.SaveManifest(root, manifest); err != nil {
		t.Fatalf("save manifest: %v", err)
	}
	out, err := runCLI(t, root, "wf", "push", "--json")
	if err != nil {
		t.Fatalf("push: %v", err)
	}
	draftID := decodeJSON(t, out)["draftID"].(string)
	if f.titlePatches[draftID] != "New name" {
		t.Errorf("title patches = %+v, want New name on %s", f.titlePatches, draftID)
	}
}

// --- optimistic concurrency --------------------------------------------------

// What the preconditions actually are, asserted on the wire. A file the baseline
// knows carries its LAST-SYNCED hash — not the content being written and not the
// content the server happens to hold — and a file the baseline has never seen
// carries "", which asserts it does not exist yet.
//
// The distinction between an absent field and an empty one is the whole point,
// so nil is checked explicitly rather than through a string comparison that
// would read the two as the same thing.
func TestPushSendsTheBaselineHashAsAPrecondition(t *testing.T) {
	f := newFakeInstance(t)
	f.AddWorkflow(&api.Workflow{ID: "wf-1", Lifecycle: api.LifecycleDraft},
		api.WorkflowFile{Path: "main.py", Content: "old\n"})
	signIn(t, f)
	root := cloneFolder(t, f, "wf-1")

	writeLocal(t, root, "main.py", "new\n")
	writeLocal(t, root, "lib/helpers.py", "H\n")
	if _, err := runCLI(t, root, "wf", "push", "--json"); err != nil {
		t.Fatalf("push: %v", err)
	}

	edited := f.writesFor("main.py")
	if len(edited) != 1 || edited[0].BaseSha256 == nil {
		t.Fatalf("main.py writes = %+v, want one carrying a precondition", edited)
	}
	// The BASELINE's hash — what the last sync recorded the server as holding —
	// which for an edited file is NOT the bytes being written.
	if got, want := *edited[0].BaseSha256, wfdir.HashString("old\n"); got != want {
		t.Errorf("main.py baseSha256 = %s, want the last-synced hash %s", got, want)
	}
	if *edited[0].BaseSha256 == wfdir.HashString("new\n") {
		t.Error("main.py asserted the content it was about to write, which is always true and guards nothing")
	}

	added := f.writesFor("lib/helpers.py")
	if len(added) != 1 || added[0].BaseSha256 == nil {
		t.Fatalf("lib/helpers.py writes = %+v, want one carrying a precondition", added)
	}
	if *added[0].BaseSha256 != "" {
		t.Errorf("lib/helpers.py baseSha256 = %q, want \"\" — a file the baseline has never seen must not already exist",
			*added[0].BaseSha256)
	}
}

// The race the drift guard cannot see: the file listing was clean, and somebody
// wrote to the draft in the window before this push's own PUT. The write is
// refused server-side, the file keeps THEIR content, and the push stops.
func TestPushStopsWhenAFileMovesUnderItMidPush(t *testing.T) {
	f := newFakeInstance(t)
	f.AddWorkflow(&api.Workflow{ID: "wf-1", Lifecycle: api.LifecycleDraft},
		api.WorkflowFile{Path: "main.py", Content: "old\n"},
		api.WorkflowFile{Path: "zzz.py", Content: "Z\n"})
	signIn(t, f)
	root := cloneFolder(t, f, "wf-1")
	writeLocal(t, root, "main.py", "mine\n")
	writeLocal(t, root, "zzz.py", "Z2\n")

	// A colleague's web-builder save, landing between the listing this push
	// compared against and its own first write.
	f.beforeFileWrite = func(method, path string) {
		if method == "PUT" && path == "main.py" {
			f.beforeFileWrite = nil
			f.putFile("wf-1", "main.py", "theirs\n")
		}
	}

	var out string
	var err error
	narration := captureStderr(t, func() {
		out, err = runCLI(t, root, "wf", "push", "--json")
	})
	if err == nil {
		t.Fatal("push overwrote a file that changed under it")
	}
	if !strings.Contains(err.Error(), "main.py") {
		t.Errorf("error = %v, want it to name the file", err)
	}
	// The narration is the actionable half, and it is on stderr so a --json
	// caller's stdout stays one parseable object.
	for _, want := range []string{"main.py", "wf status", "--force"} {
		if !strings.Contains(narration, want) {
			t.Errorf("stderr did not mention %q:\n%s", want, narration)
		}
	}
	if got := f.FileContents("wf-1")["main.py"]; got != "theirs\n" {
		t.Errorf("main.py = %q — the refused write landed anyway", got)
	}
	// Stopped rather than carried on: zzz.py sorts after the entrypoint.
	if got := f.FileContents("wf-1")["zzz.py"]; got != "Z\n" {
		t.Errorf("zzz.py = %q — the push continued past the conflict", got)
	}
	// One attempt. A retry (or an automatic --force) would be a second write.
	if writes := f.writesFor("main.py"); len(writes) != 1 {
		t.Errorf("main.py was written %d times, want exactly one attempt: %+v", len(writes), writes)
	}
	// The partial-push report still describes what did happen, and the baseline
	// beside it must not claim the colleague's content.
	if decodeJSON(t, out)["error"] == nil {
		t.Error("payload carries no error field")
	}
	state, err := wfdir.LoadState(root)
	if err != nil {
		t.Fatalf("load state: %v", err)
	}
	if got := state.For(f.Key()).Files["main.py"].SHA256; got == wfdir.HashString("theirs\n") {
		t.Error("the baseline acknowledged content this push never wrote — the next push would overwrite it in silence")
	}
}

// The same race one file LATER — after an earlier PUT has already committed.
//
// TestPushStopsWhenAFileMovesUnderItMidPush conflicts on the very first write,
// so the push has nothing to have landed and the interesting question never
// comes up: when a push stops part-way, the baseline has to record what DID
// land and nothing else. Getting either half wrong is silent. Forget file 1 and
// the author's own completed write reads as somebody else's drift on the next
// push, which then refuses the retry that would fix it. Record the colleague's
// file 2 and the drift guard is disarmed against the very change it just
// refused to overwrite, so the next plain push destroys it without a word.
func TestPushBaselineAfterAMidPushConflictRecordsOnlyWhatLanded(t *testing.T) {
	f := newFakeInstance(t)
	f.AddWorkflow(&api.Workflow{ID: "wf-1", Lifecycle: api.LifecycleDraft},
		api.WorkflowFile{Path: "main.py", Content: "M\n"},
		api.WorkflowFile{Path: "zzz.py", Content: "Z\n"})
	signIn(t, f)
	root := cloneFolder(t, f, "wf-1")
	writeLocal(t, root, "main.py", "mine\n")
	writeLocal(t, root, "zzz.py", "also mine\n")

	// The colleague lands on zzz.py only — so main.py's precondition holds and
	// its write commits, and the conflict happens on the SECOND file.
	f.beforeFileWrite = func(method, path string) {
		if method == "PUT" && path == "zzz.py" {
			f.beforeFileWrite = nil
			f.putFile("wf-1", "zzz.py", "theirs\n")
		}
	}

	var out string
	var err error
	captureStderr(t, func() {
		out, err = runCLI(t, root, "wf", "push", "--json")
	})
	if err == nil {
		t.Fatal("push overwrote a file that changed under it")
	}
	// The premise: file 1 really did land. Without this the assertions below
	// would pass on a push that never got past the entrypoint.
	if got := f.FileContents("wf-1")["main.py"]; got != "mine\n" {
		t.Fatalf("main.py = %q, want the first write to have landed before the conflict", got)
	}
	if got := f.FileContents("wf-1")["zzz.py"]; got != "theirs\n" {
		t.Errorf("zzz.py = %q — the refused write landed anyway", got)
	}

	payload := decodeJSON(t, out)
	if pushed, _ := payload["pushed"].([]any); len(pushed) != 1 || pushed[0] != "main.py" {
		t.Errorf("pushed = %v, want exactly main.py", payload["pushed"])
	}
	if payload["conflict"] != true {
		t.Errorf("conflict = %v, want true — the caller cannot tell this from a rejection otherwise", payload["conflict"])
	}

	state, err2 := wfdir.LoadState(root)
	if err2 != nil {
		t.Fatalf("load state: %v", err2)
	}
	baseline := state.For(f.Key())
	if baseline == nil {
		t.Fatal("the stopped push recorded no baseline at all")
	}
	// File 1: recorded at what this push WROTE. Anything else — the old
	// content, or nothing — makes the author's own write look like drift.
	if got, want := baseline.Files["main.py"].SHA256, wfdir.HashString("mine\n"); got != want {
		t.Errorf("baseline main.py = %s, want the content this push landed (%s)", got, want)
	}
	// File 2: NOT the colleague's. It may honestly hold the last-synced hash —
	// that is what the folder still knows — but never the bytes this push was
	// just refused permission to overwrite.
	if got := baseline.Files["zzz.py"].SHA256; got == wfdir.HashString("theirs\n") {
		t.Error("the baseline acknowledged the colleague's file — the next push would overwrite it in silence")
	}
	if got, want := baseline.Files["zzz.py"].SHA256, wfdir.HashString("Z\n"); got != want {
		t.Errorf("baseline zzz.py = %s, want the last-synced hash %s", got, want)
	}
}

// The suppression rule, stated three ways. Each of these is a path where the
// drift guard was deliberately bypassed, and in each the baseline demonstrably
// does not describe the row — so a precondition built from it could only ever
// produce a 409 for a difference the push was already told to proceed past.
//
// These are not merely assertions about the wire: the fake ENFORCES
// preconditions, so a regression that stopped suppressing them fails the push
// itself, not just the check below.
func TestPushSuppressesPreconditionsExactlyWhereTheDriftGuardIsBypassed(t *testing.T) {
	assertNoPreconditions := func(t *testing.T, f *fakeInstance) {
		t.Helper()
		if len(f.fileWrites) == 0 {
			t.Fatal("nothing was written, so the assertion proves nothing")
		}
		for _, w := range f.fileWrites {
			if w.BaseSha256 != nil {
				t.Errorf("%s %s carried baseSha256 %q, want none", w.Method, w.Path, *w.BaseSha256)
			}
		}
	}

	// --force with a CLEAN baseline: nothing had drifted, so the guard had
	// nothing to bypass — and the preconditions must still be suppressed.
	//
	// This is the subtest the "force" case below cannot stand in for. It stages
	// real drift first, so it only ever exercises the branch that already
	// suppressed; a clean baseline takes the early return above it, and an
	// early return that reported Bypassed=false would ARM preconditions under
	// --force. That is not a harmless extra check: it arms them in exactly the
	// window this whole feature is about — a remote that moves AFTER the drift
	// check read the listing — so the push 409s and noteFileConflict advises
	// running --force, which is what the caller just did.
	t.Run("force with a clean baseline", func(t *testing.T) {
		f := newFakeInstance(t)
		f.AddWorkflow(&api.Workflow{ID: "wf-1", Lifecycle: api.LifecycleDraft},
			api.WorkflowFile{Path: "main.py", Content: "M\n"})
		signIn(t, f)
		// Cloned and NOT disturbed: the baseline describes the row exactly, so
		// checkDrift finds no drift and no metadata to report.
		root := cloneFolder(t, f, "wf-1")
		writeLocal(t, root, "main.py", "mine\n")

		if _, err := runCLI(t, root, "wf", "push", "--force", "--json"); err != nil {
			t.Fatalf("forced push: %v", err)
		}
		assertNoPreconditions(t, f)
		if got := f.FileContents("wf-1")["main.py"]; got != "mine\n" {
			t.Errorf("main.py = %q — the forced push did not land", got)
		}
	})

	// --force means "overwrite the remote with what I have". Sending the
	// baseline's hashes would refuse exactly the case the flag exists for.
	t.Run("force", func(t *testing.T) {
		f := newFakeInstance(t)
		f.AddWorkflow(&api.Workflow{ID: "wf-1", Lifecycle: api.LifecycleDraft},
			api.WorkflowFile{Path: "main.py", Content: "M\n"})
		signIn(t, f)
		root := cloneFolder(t, f, "wf-1")
		f.putFile("wf-1", "main.py", "theirs\n")
		writeLocal(t, root, "main.py", "mine\n")

		if _, err := runCLI(t, root, "wf", "push", "--force", "--json"); err != nil {
			t.Fatalf("forced push: %v", err)
		}
		assertNoPreconditions(t, f)
		if got := f.FileContents("wf-1")["main.py"]; got != "mine\n" {
			t.Errorf("main.py = %q — --force did not overwrite", got)
		}
	})

	// A workflow this push CREATED has no baseline, and the create SEEDS a
	// starter file at the entrypoint inside its own transaction — so "" for
	// that path would refuse a workflow the CLI made seconds earlier.
	t.Run("freshly created", func(t *testing.T) {
		f := newFakeInstance(t)
		signIn(t, f)
		root := initFolder(t, f, map[string]string{"main.py": "M\n"})

		if _, err := runCLI(t, root, "wf", "push", "--json"); err != nil {
			t.Fatalf("first push: %v", err)
		}
		assertNoPreconditions(t, f)
	})

	// The same seed seen a command later: a first push that died between
	// recording the binding and writing the baseline. The guard recognises it
	// (isUntouchedFirstPush) and so must the preconditions.
	t.Run("resuming a crashed first push", func(t *testing.T) {
		f := newFakeInstance(t)
		f.AddWorkflow(&api.Workflow{ID: "wf-new-1", Lifecycle: api.LifecycleDraft, Hidden: true},
			api.WorkflowFile{Path: "main.py", Content: fakeEntrypointStarter})
		signIn(t, f)
		root := bindFolder(t, f, "wf-new-1", map[string]string{"main.py": "M\n"})

		if _, err := runCLI(t, root, "wf", "push", "--json"); err != nil {
			t.Fatalf("resumed first push: %v", err)
		}
		assertNoPreconditions(t, f)
		if got := f.FileContents("wf-new-1")["main.py"]; got != "M\n" {
			t.Errorf("main.py = %q", got)
		}
	})
}

// A deletion asserts what it is removing — but never "" for a path the baseline
// does not know, which would be a precondition that can only ever fail on the
// one thing it is applied to.
func TestPushDeletionCarriesTheBaselineHash(t *testing.T) {
	f := newFakeInstance(t)
	f.AddWorkflow(&api.Workflow{ID: "wf-1", Lifecycle: api.LifecycleDraft},
		api.WorkflowFile{Path: "main.py", Content: "M\n"},
		api.WorkflowFile{Path: "lib/old.py", Content: "O\n"})
	signIn(t, f)
	root := cloneFolder(t, f, "wf-1")

	if err := os.Remove(filepath.Join(root, "lib", "old.py")); err != nil {
		t.Fatalf("remove: %v", err)
	}
	if _, err := runCLI(t, root, "wf", "push", "--json"); err != nil {
		t.Fatalf("push: %v", err)
	}
	deletes := f.writesFor("lib/old.py")
	if len(deletes) != 1 || deletes[0].Method != "DELETE" || deletes[0].BaseSha256 == nil {
		t.Fatalf("lib/old.py writes = %+v, want one DELETE carrying a precondition", deletes)
	}
	if got, want := *deletes[0].BaseSha256, wfdir.HashString("O\n"); got != want {
		t.Errorf("delete baseSha256 = %s, want %s", got, want)
	}
}

// And the delete's own race: the file changed after the listing, so what would
// have been deleted is not what the folder last saw.
func TestPushStopsWhenAFileToDeleteMovesUnderIt(t *testing.T) {
	f := newFakeInstance(t)
	f.AddWorkflow(&api.Workflow{ID: "wf-1", Lifecycle: api.LifecycleDraft},
		api.WorkflowFile{Path: "main.py", Content: "M\n"},
		api.WorkflowFile{Path: "lib/old.py", Content: "O\n"})
	signIn(t, f)
	root := cloneFolder(t, f, "wf-1")

	if err := os.Remove(filepath.Join(root, "lib", "old.py")); err != nil {
		t.Fatalf("remove: %v", err)
	}
	f.beforeFileWrite = func(method, path string) {
		if method == "DELETE" && path == "lib/old.py" {
			f.beforeFileWrite = nil
			f.putFile("wf-1", "lib/old.py", "they kept working on it\n")
		}
	}

	_, err := runCLI(t, root, "wf", "push", "--json")
	if err == nil {
		t.Fatal("push deleted a file that changed under it")
	}
	if !strings.Contains(err.Error(), "lib/old.py") {
		t.Errorf("error = %v, want it to name the file", err)
	}
	if got := f.FileContents("wf-1")["lib/old.py"]; got != "they kept working on it\n" {
		t.Errorf("lib/old.py = %q — the refused delete happened anyway", got)
	}
}
