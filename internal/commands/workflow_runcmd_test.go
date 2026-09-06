package commands

import (
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/ronjatech/ronja-cli/internal/api"
)

// `wf run`'s REFUSALS, in the order the command makes them: which row, is there
// a draft in the way, is the folder what is live, is there a gate, are the
// parameters right — and then the three the server makes, which this command
// renders rather than passing through as a status code.
//
// Every test here asserts ZERO run requests, for the reason the `wf test` ones
// do: this command runs the LIVE workflow, so a refusal that arrives after the
// POST has already written to production tables is not a refusal. What happens
// once a run DOES start — the notes, the report, the terminal statuses — is
// workflow_runcmd_report_test.go.

// --- helpers ----------------------------------------------------------------

// liveFolder is the state `wf run` is FOR: a live workflow, no draft of it, and
// a folder cloned from live (so the baseline describes the live row, which is
// exactly what publish leaves behind).
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

// The doctrine, made testable. `wf run` is a sync-loop verb because it runs THIS
// FOLDER's workflow; the moment it accepts an id it is a wrapper around one
// endpoint, and the refusal is what keeps that argument honest.
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

// A workflow that has never been published has no live row at all, and the
// remedy is publish rather than anything about the folder.
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

// The refusal the staleness check below CANNOT make. A folder with an open draft
// is clean against that draft, so nothing about its files says the live version
// is somebody else's code — which is precisely the publish → edit → push state
// where a live run would run something the author stopped looking at.
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

// A copy cloned from git has no baseline (.ronja/ is local-only), so there is
// nothing to compare live against — and "I cannot tell" has to refuse, because
// the whole claim this command makes is that it ran the code in front of you.
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

// The other half of that refusal, and a DIFFERENT message: the baseline exists
// and names some other row. Reachable on the ordinary non-admin path — `wf
// publish` on a shared workflow submits the draft for review and returns
// without re-anchoring the baseline, so once an admin commits it the folder
// names a draft that is gone. The files are usually exactly what went live, so
// a message about a copy taken from git would send somebody to re-clone a
// perfectly good folder.
func TestRunTellsAMismatchedBaselineApartFromNoBaselineAtAll(t *testing.T) {
	f := newFakeInstance(t)
	signIn(t, f)
	root := liveFolder(t, f, nil)

	// push opens a draft and anchors the baseline on it.
	if _, err := runCLI(t, root, "wf", "push"); err != nil {
		t.Fatalf("push: %v", err)
	}
	draftID := f.draftOf["wf-1"]
	if draftID == "" {
		t.Fatal("push opened no draft")
	}
	// ...and an admin commits it, which is the server-side half the author's
	// folder never hears about.
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
	// The two states share a check and must not share a sentence.
	if strings.Contains(err.Error(), "has no sync baseline") {
		t.Errorf("a mismatched baseline was reported as a missing one: %v", err)
	}
	if len(f.runRequests) != 0 {
		t.Errorf("started a run anyway: %+v", f.runRequests)
	}
}

// --- the approval gate ------------------------------------------------------

// Same locked decision as `wf test`: refused before any POST, and the message
// never names the off-switch — turning the gate off is a governance change, and
// nobody should learn it from a CLI hint.
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

// The full matrix is pinned against `wf test` (TestTestParameterValidation);
// what matters here is that `wf run` goes through the same chokepoint, against
// the LIVE row's declaration.
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
	// Coerced, not passed through as the text a shell handed over.
	if got["month"] != "2026-07" || got["limit"] != float64(12) {
		t.Errorf("parameterValues = %#v, want the coerced values", got)
	}
}

// --- the two refusals only the server can make --------------------------------

// Running a workflow needs only READ access to it, so a 403 is the role (a
// read-only member) or a token's scopes — never the feature's scope. A bare
// 403 tells somebody nothing; naming both causes and the way forward does,
// and the message must NOT send them hunting for admin access or a Feature
// owner, which were the rules while the run route borrowed the write gate.
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

// A 404 on the run of a workflow the command has just read is the gate's
// oracle-safe answer for a row the credential can no longer SEE — a race with
// an access change, not absence and not a rule about who may run. Bare, it
// reads "not found" straight after a successful push.
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

// The concurrency-skip 409. Two properties: NOTHING RAN — this is not a failed
// run — and the blocking run is named from the response's own `blockingRunId`
// rather than scraped out of the message.
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

// The same 409 with nobody to name. The server omits the details entirely when
// the run that lost the race is anonymous — the partial unique index refuses
// without a read — so the clause about the run holding the slot has to go with
// it rather than print an empty id.
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
	// No half-written sentence about a run nobody named.
	for _, forbidden := range []string{"holds the slot", "ronja api /api/v2/workflow/run/"} {
		if strings.Contains(err.Error(), forbidden) {
			t.Errorf("an anonymous 409 still rendered %q: %v", forbidden, err)
		}
	}
	if f.runPolls != 0 {
		t.Errorf("polled %d times for a run that never started", f.runPolls)
	}
}

// Everything else arrives in the server's own words — the credit kill-stop above
// all, whose one sentence says what to do about it.
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
