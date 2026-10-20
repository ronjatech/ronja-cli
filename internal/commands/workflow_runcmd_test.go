package commands

import (
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/ronjatech/ronja-cli/internal/api"
)

// Every refusal here asserts zero run requests: `wf run` runs the live
// workflow, and a refusal after the POST comes after production writes.

// --- helpers ----------------------------------------------------------------

// liveFolder is a live workflow with no draft and a folder cloned from live,
// the state publish leaves behind.
func liveFolder(t *testing.T, f *fakeInstance, shape func(*api.Workflow)) string {
	t.Helper()
	live := f.AddWorkflow(&api.Workflow{ID: "wf-1", Lifecycle: api.LifecycleLive},
		api.WorkflowFile{Path: "main.py", Content: "print(1)\n"})
	if shape != nil {
		shape(live)
	}
	return cloneFolder(t, f, "wf-1")
}

// --- which row --------------------------------------------------------------

func TestRunRefusesAnUnboundFolder(t *testing.T) {
	f := newFakeInstance(t)
	signIn(t, f)
	root := initFolder(t, f, map[string]string{"main.py": "x\n"})

	_, err := runCLI(t, root, "wf", "run")
	if err == nil || !strings.Contains(err.Error(), "nothing to run") {
		t.Fatalf("err = %v, want a refusal naming the missing workflow", err)
	}
	if len(f.runRequests) != 0 {
		t.Errorf("started a run anyway: %+v", f.runRequests)
	}
}

// `wf run` is a sync-loop verb because it runs this folder's workflow; taking an
// id would make it a wrapper around one endpoint.
func TestRunTakesNoWorkflowID(t *testing.T) {
	f := newFakeInstance(t)
	signIn(t, f)
	root := liveFolder(t, f, nil)

	_, err := runCLI(t, root, "wf", "run", "wf-2")
	if err == nil {
		t.Fatal("accepted a workflow id")
	}
	for _, want := range []string{"takes no workflow id", "ronja api"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("refusal missing %q: %v", want, err)
		}
	}
	if len(f.runRequests) != 0 {
		t.Errorf("started a run anyway: %+v", f.runRequests)
	}
}

func TestRunRefusesAWorkflowThatWasNeverPublished(t *testing.T) {
	f := newFakeInstance(t)
	f.AddWorkflow(&api.Workflow{ID: "wf-1", Lifecycle: api.LifecycleDraft},
		api.WorkflowFile{Path: "main.py", Content: "print(1)\n"})
	signIn(t, f)
	root := cloneFolder(t, f, "wf-1")

	_, err := runCLI(t, root, "wf", "run")
	if err == nil {
		t.Fatal("ran a workflow that was never published")
	}
	for _, want := range []string{"never been published", "wf publish"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("refusal missing %q: %v", want, err)
		}
	}
	if len(f.runRequests) != 0 {
		t.Errorf("started a run anyway: %+v", f.runRequests)
	}
}

// --- a draft in the way -------------------------------------------------------

// The staleness check cannot catch this: a folder is clean against its open
// draft, so the live version may be code the author stopped looking at.
func TestRunRefusesWhenADraftIsOpen(t *testing.T) {
	f := newFakeInstance(t)
	signIn(t, f)
	root := draftFolder(t, f, nil)

	_, err := runCLI(t, root, "wf", "run")
	if err == nil {
		t.Fatal("ran live while a draft was open")
	}
	for _, want := range []string{"draft-1", "wf test", "wf publish", "wf discard"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("refusal missing %q: %v", want, err)
		}
	}
	if len(f.runRequests) != 0 {
		t.Errorf("started a run anyway: %+v", f.runRequests)
	}
}

// --- the folder is what is live ---------------------------------------------

func TestRunRefusesAFolderWithChangesThatAreNotLive(t *testing.T) {
	f := newFakeInstance(t)
	signIn(t, f)
	root := liveFolder(t, f, nil)
	writeLocal(t, root, "main.py", "print(2)\n")

	_, err := runCLI(t, root, "wf", "run")
	if err == nil {
		t.Fatal("ran live from a folder holding unpublished changes")
	}
	for _, want := range []string{"main.py", "wf push", "wf publish"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("refusal missing %q: %v", want, err)
		}
	}
	if len(f.runRequests) != 0 {
		t.Errorf("started a run anyway: %+v", f.runRequests)
	}
}

// A git clone has no baseline (.ronja/ is local-only), and "cannot tell" must
// refuse: the command's claim is that it ran the code in front of you.
func TestRunRefusesAFolderWithNoBaseline(t *testing.T) {
	f := newFakeInstance(t)
	signIn(t, f)
	root := liveFolder(t, f, nil)
	if err := os.RemoveAll(filepath.Join(root, ".ronja")); err != nil {
		t.Fatalf("remove baseline: %v", err)
	}

	_, err := runCLI(t, root, "wf", "run")
	if err == nil {
		t.Fatal("ran live from a folder with no baseline")
	}
	if !strings.Contains(err.Error(), "no sync baseline") {
		t.Errorf("refusal does not say what is missing: %v", err)
	}
	if len(f.runRequests) != 0 {
		t.Errorf("started a run anyway: %+v", f.runRequests)
	}
}

// Reachable on the non-admin path: publishing a shared workflow submits the
// draft for review without re-anchoring the baseline, so after an admin commits
// it the folder names a gone draft; a "copy from git" message would send
// somebody to re-clone a good folder.
func TestRunTellsAMismatchedBaselineApartFromNoBaselineAtAll(t *testing.T) {
	f := newFakeInstance(t)
	signIn(t, f)
	root := liveFolder(t, f, nil)

	if _, err := runCLI(t, root, "wf", "push"); err != nil {
		t.Fatalf("push: %v", err)
	}
	draftID := f.draftOf["wf-1"]
	if draftID == "" {
		t.Fatal("push opened no draft")
	}
	// An admin commits it server-side.
	delete(f.draftOf, "wf-1")
	delete(f.workflows, draftID)

	_, err := runCLI(t, root, "wf", "run")
	if err == nil {
		t.Fatal("ran live from a folder whose baseline names another row")
	}
	for _, want := range []string{draftID, "wf-1", "wf status"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("refusal missing %q: %v", want, err)
		}
	}
	if strings.Contains(err.Error(), "has no sync baseline") {
		t.Errorf("a mismatched baseline was reported as a missing one: %v", err)
	}
	if len(f.runRequests) != 0 {
		t.Errorf("started a run anyway: %+v", f.runRequests)
	}
}

// --- the approval gate ------------------------------------------------------

// The message never names the off-switch: turning the gate off is a governance
// change nobody should learn from a CLI hint.
func TestRunRefusesAnApprovalGatedWorkflow(t *testing.T) {
	f := newFakeInstance(t)
	signIn(t, f)
	root := liveFolder(t, f, func(live *api.Workflow) {
		live.ApprovalGate = &api.WorkflowApprovalGate{Enabled: true}
	})

	_, err := runCLI(t, root, "wf", "run")
	if err == nil {
		t.Fatal("ran a gated workflow")
	}
	if len(f.runRequests) != 0 {
		t.Fatalf("the gate preflight let a run start: %+v", f.runRequests)
	}
	for _, want := range []string{"approval gate", "agent session", "mid-run approval"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("refusal missing %q: %v", want, err)
		}
	}
	for _, forbidden := range []string{"disable", "turn off", "approvalGate"} {
		if strings.Contains(strings.ToLower(err.Error()), strings.ToLower(forbidden)) {
			t.Errorf("refusal suggests the gate-off workaround (%q): %v", forbidden, err)
		}
	}
}

// --- parameters -------------------------------------------------------------

// The full matrix is TestTestParameterValidation; this pins that `wf run` uses
// the same chokepoint against the live row's declaration.
func TestRunChecksParametersAgainstTheLiveDeclaration(t *testing.T) {
	f := newFakeInstance(t)
	f.runScript = []api.RunResponse{finishedRun(api.RunStatusDone, api.RunHealthDone)}
	signIn(t, f)
	withFastPolling(t)
	root := liveFolder(t, f, func(live *api.Workflow) {
		live.Parameters = []api.WorkflowParameter{{Name: "month", Type: "string"}, {Name: "limit", Type: "number"}}
	})

	_, err := runCLI(t, root, "wf", "run", "--param", "moth=2026-07")
	if err == nil || !strings.Contains(err.Error(), "not declared") {
		t.Fatalf("err = %v, want an undeclared-parameter refusal", err)
	}
	if len(f.runRequests) != 0 {
		t.Fatalf("a bad parameter still started a run: %+v", f.runRequests)
	}

	if _, err := runCLI(t, root, "wf", "run", "--param", "month=2026-07", "--param", "limit=12"); err != nil {
		t.Fatalf("run: %v", err)
	}
	if len(f.runRequests) != 1 {
		t.Fatalf("runs = %+v, want one", f.runRequests)
	}
	got := f.runRequests[0].ParameterValues
	if got["month"] != "2026-07" || got["limit"] != float64(12) {
		t.Errorf("parameterValues = %#v, want the coerced values", got)
	}
}

// --- the two refusals only the server can make --------------------------------

// Running needs only read access, so a 403 is the role or the token's scopes,
// never the feature's scope; the message must not send anyone hunting for admin
// access or a Feature owner.
func TestRunExplainsRoleOrScopeOn403(t *testing.T) {
	f := newFakeInstance(t)
	f.failRun = http.StatusForbidden
	f.failRunMessage = "forbidden"
	signIn(t, f)
	withFastPolling(t)
	root := liveFolder(t, f, nil)

	_, err := runCLI(t, root, "wf", "run")
	if err == nil {
		t.Fatal("a refused run exited zero")
	}
	for _, want := range []string{"User role", "automation:write", "read access to the workflow itself is enough", "forbidden"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("403 message missing %q: %v", want, err)
		}
	}
	for _, stale := range []string{"admin access", "shared feature", "Feature's owner"} {
		if strings.Contains(err.Error(), stale) {
			t.Errorf("403 message still names the retired write-gate rule %q: %v", stale, err)
		}
	}
	if f.runPolls != 0 {
		t.Errorf("polled %d times for a run that never started", f.runPolls)
	}
}

// A 404 on a workflow just read is the gate's oracle-safe answer for a row the
// credential can no longer see: a race with an access change, not absence.
func TestRunExplainsLostVisibilityOn404(t *testing.T) {
	f := newFakeInstance(t)
	f.failRun = http.StatusNotFound
	f.failRunMessage = "not found"
	signIn(t, f)
	withFastPolling(t)
	root := liveFolder(t, f, nil)

	_, err := runCLI(t, root, "wf", "run")
	if err == nil {
		t.Fatal("a refused run exited zero")
	}
	for _, want := range []string{"can no longer see it", "only read access", "wf status"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("404 message missing %q: %v", want, err)
		}
	}
	if f.runPolls != 0 {
		t.Errorf("polled %d times for a run that never started", f.runPolls)
	}
}

// The concurrency-skip 409: nothing ran, and the blocking run comes from the
// response's `blockingRunId`, not scraped from the message.
func TestRunReportsTheBlockingRunOn409(t *testing.T) {
	f := newFakeInstance(t)
	f.failRun = http.StatusConflict
	f.failRunBody = map[string]any{
		"error":             "run workflowrun-7 is running since 2026-08-31T09:00:00Z; this workflow is set to skip overlapping runs",
		"code":              api.RunInFlightCode,
		"blockingRunId":     "workflowrun-7",
		"blockingRunStatus": "running",
		"blockingSince":     "2026-08-31T09:00:00Z",
	}
	signIn(t, f)
	withFastPolling(t)
	root := liveFolder(t, f, nil)

	_, err := runCLI(t, root, "wf", "run")
	if err == nil {
		t.Fatal("a skipped run exited zero")
	}
	for _, want := range []string{"did not start", "Nothing ran", "skip overlapping runs", "workflowrun-7"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("409 message missing %q: %v", want, err)
		}
	}
	if f.runPolls != 0 {
		t.Errorf("polled %d times for a run that never started", f.runPolls)
	}
}

// The server omits the details when the partial unique index refused without a
// read, so the slot-holder clause must go rather than print an empty id.
func TestRunReportsAnAnonymousBlockingRunOn409(t *testing.T) {
	f := newFakeInstance(t)
	f.failRun = http.StatusConflict
	f.failRunBody = map[string]any{
		"error": "a run of this workflow is already in progress",
		"code":  api.RunInFlightCode,
	}
	signIn(t, f)
	withFastPolling(t)
	root := liveFolder(t, f, nil)

	_, err := runCLI(t, root, "wf", "run")
	if err == nil {
		t.Fatal("a skipped run exited zero")
	}
	for _, want := range []string{"did not start", "Nothing ran", "skip overlapping runs"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("409 message missing %q: %v", want, err)
		}
	}
	for _, forbidden := range []string{"holds the slot", "ronja api /api/v2/workflow/run/"} {
		if strings.Contains(err.Error(), forbidden) {
			t.Errorf("an anonymous 409 still rendered %q: %v", forbidden, err)
		}
	}
	if f.runPolls != 0 {
		t.Errorf("polled %d times for a run that never started", f.runPolls)
	}
}

func TestRunPassesThroughTheServersOtherRefusals(t *testing.T) {
	f := newFakeInstance(t)
	f.failRun = http.StatusBadRequest
	f.failRunMessage = "this organisation is out of credits — runs are paused until the balance is topped up"
	signIn(t, f)
	withFastPolling(t)
	root := liveFolder(t, f, nil)

	_, err := runCLI(t, root, "wf", "run")
	if err == nil {
		t.Fatal("a refused run POST exited zero")
	}
	if !strings.Contains(err.Error(), f.failRunMessage) {
		t.Errorf("the server's own words did not survive:\n  got:  %v\n  want: %s", err, f.failRunMessage)
	}
}
