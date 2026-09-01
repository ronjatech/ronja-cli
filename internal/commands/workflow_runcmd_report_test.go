package commands

import (
	"strings"
	"testing"
	"time"

	"github.com/ronjatech/ronja-cli/internal/api"
	"github.com/ronjatech/ronja-cli/internal/wfdir"
)

// The other half of `wf run`: what it says once a run exists. The drift note it
// makes instead of refusing, the run itself, the POST whose outcome is unknown,
// the terminal statuses and what each is worth as an exit code, and the two
// hints that make the loop findable.
//
// Split from workflow_runcmd_test.go, which holds the refusals — the two halves
// ask different questions, and one file asking both was over the size a person
// reads top to bottom.

// --- drift on the live row ----------------------------------------------------

// The drift the check above is structurally blind to: it compares disk against
// the baseline, both local, so a colleague's republish over an untouched folder
// passes it clean. A note, not a refusal — the live row is what this command
// runs — but a clean folder otherwise reads as "you are running exactly what
// you are looking at".
func TestRunNotesLiveChangesThatDidNotComeFromThisFolder(t *testing.T) {
	f := newFakeInstance(t)
	f.runScript = []api.RunResponse{finishedRun(api.RunStatusDone, api.RunHealthDone)}
	signIn(t, f)
	withFastPolling(t)
	root := liveFolder(t, f, nil)
	// A colleague publishes over the live workflow.
	f.putFile("wf-1", "main.py", "print(99)\n")

	var err error
	stderr := captureStderr(t, func() { _, err = runCLI(t, root, "wf", "run") })
	if err != nil {
		t.Fatalf("the note blocked the run: %v", err)
	}
	if !strings.Contains(stderr, "did not come from this folder") {
		t.Errorf("no drift note for a workflow republished elsewhere:\n%s", stderr)
	}
	if len(f.runRequests) != 1 {
		t.Errorf("runs = %+v, want the run to have gone ahead", f.runRequests)
	}
}

// ...and silence when there is nothing to say. A note on every run is a note
// nobody reads.
func TestRunSaysNothingWhenTheLiveRowMatchesTheFolder(t *testing.T) {
	f := newFakeInstance(t)
	f.runScript = []api.RunResponse{finishedRun(api.RunStatusDone, api.RunHealthDone)}
	signIn(t, f)
	withFastPolling(t)
	root := liveFolder(t, f, nil)

	var err error
	stderr := captureStderr(t, func() { _, err = runCLI(t, root, "wf", "run") })
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	if strings.Contains(stderr, "did not come from this folder") {
		t.Errorf("drift note on an untouched live workflow:\n%s", stderr)
	}
}

// --- the run itself -----------------------------------------------------------

func TestRunRunsTheLiveRowAndReportsIt(t *testing.T) {
	f := newFakeInstance(t)
	f.runScript = []api.RunResponse{finishedRun(api.RunStatusDone, api.RunHealthDone)}
	signIn(t, f)
	withFastPolling(t)
	root := liveFolder(t, f, nil)

	out, err := runCLI(t, root, "wf", "run")
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	if len(f.runRequests) != 1 {
		t.Fatalf("runs = %+v, want exactly one", f.runRequests)
	}
	// The LIVE row, never a draft.
	if f.runRequests[0].WorkflowID != "wf-1" {
		t.Errorf("ran %s, want the live workflow", f.runRequests[0].WorkflowID)
	}
	for _, want := range []string{"run-1", "done"} {
		if !strings.Contains(out, want) {
			t.Errorf("report missing %q:\n%s", want, out)
		}
	}
	// The publish hint belongs to a draft run: this code is already live.
	if strings.Contains(out, "wf publish") {
		t.Errorf("a live run offered publish:\n%s", out)
	}
}

// $RONJA_URL and $RONJA_TOKEN outrank the credential file, so the instance the
// side effects land on is worth one line — BEFORE the POST, because after it the
// line is a record of where they already went.
func TestRunNamesTheInstanceBeforeStarting(t *testing.T) {
	f := newFakeInstance(t)
	f.runScript = []api.RunResponse{finishedRun(api.RunStatusDone, api.RunHealthDone)}
	signIn(t, f)
	withFastPolling(t)
	root := liveFolder(t, f, nil)

	var err error
	stderr := captureStderr(t, func() { _, err = runCLI(t, root, "wf", "run") })
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	target := strings.Index(stderr, f.URL())
	started := strings.Index(stderr, "Started run")
	if target < 0 {
		t.Fatalf("the instance was never named:\n%s", stderr)
	}
	if started < 0 || target > started {
		t.Errorf("the instance was not named before the run started:\n%s", stderr)
	}
}

// --- a run POST that timed out --------------------------------------------------

// The expensive one. The server commits the workflow_runs row and only then
// dispatches, so a deadline of OURS says nothing about whether the run started
// — and reported as a refusal, the natural retry fires a second live run into
// replace-mode output tables. So the history is read back and the run that did
// start is adopted, and the command carries on as if the POST had answered.
func TestRunAdoptsTheRunThatStartedWhenThePostTimedOut(t *testing.T) {
	f := newFakeInstance(t)
	f.runScript = []api.RunResponse{finishedRun(api.RunStatusDone, api.RunHealthDone)}
	signIn(t, f)
	withFastPolling(t)
	root := liveFolder(t, f, nil)
	// The row the POST committed before our deadline expired — stamped by the
	// SERVER's clock, which by the time a request has been slow enough to time
	// out is past the moment we asked. An older run sits beside it, because the
	// rule is "the newest run since we asked" and not "the newest run".
	f.runHistory["wf-1"] = []api.WorkflowRun{
		{ID: "run-earlier", WorkflowID: "wf-1", Status: api.RunStatusDone, ExecutedAt: time.Now().UTC().Add(-time.Hour)},
		{ID: "run-9", WorkflowID: "wf-1", Status: api.RunStatusRunning, ExecutedAt: time.Now().UTC().Add(time.Minute)},
	}
	landOnceThenTimeOut(t, "POST", "/api/v2/workflow/wf-1/run")

	var (
		out string
		err error
	)
	stderr := captureStderr(t, func() { out, err = runCLI(t, root, "wf", "run") })
	if err != nil {
		t.Fatalf("a run that had already started was reported as a failure: %v", err)
	}
	// The whole point: exactly one POST. A second one is a second live run.
	if len(f.runRequests) != 1 {
		t.Fatalf("run POSTs = %d, want exactly one — the timeout started a second run", len(f.runRequests))
	}
	if !strings.Contains(stderr, "run-9") || !strings.Contains(stderr, "timed out") {
		t.Errorf("the adopted run was not announced:\n%s", stderr)
	}
	if !strings.Contains(out, "run-9") {
		t.Errorf("the report followed a run other than the one that started:\n%s", out)
	}
}

// ...and when it cannot find one, it says the run MAY have started. Never that
// it did not: that sentence is what makes somebody run it again.
func TestRunNeverClaimsATimedOutRunDidNotStart(t *testing.T) {
	f := newFakeInstance(t)
	signIn(t, f)
	withFastPolling(t)
	root := liveFolder(t, f, nil)
	landOnceThenTimeOut(t, "POST", "/api/v2/workflow/wf-1/run")

	_, err := runCLI(t, root, "wf", "run")
	if err == nil {
		t.Fatal("an unconfirmed run exited zero")
	}
	for _, want := range []string{"timed out", "MAY STILL BE RUNNING", "/runs"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("message missing %q: %v", want, err)
		}
	}
	// The 409's sentence, which means the opposite and must never leak here.
	if strings.Contains(err.Error(), "Nothing ran") {
		t.Errorf("an unconfirmed run was reported as one that never started: %v", err)
	}
	if len(f.runRequests) != 1 {
		t.Errorf("run POSTs = %d, want exactly one", len(f.runRequests))
	}
}

// --- terminal statuses --------------------------------------------------------

// A park exits ZERO, exactly as it does for `wf test` — nothing failed, and the
// run resumes by itself — so the loud line is the only thing standing between a
// parked run and somebody recording a green tick against it.
func TestRunParkedRunIsLoudAndStillExitsZero(t *testing.T) {
	f := newFakeInstance(t)
	parked := finishedRun(api.RunStatusWaiting, api.RunHealthWaiting)
	f.runScript = []api.RunResponse{parked}
	signIn(t, f)
	withFastPolling(t)
	root := liveFolder(t, f, func(live *api.Workflow) { live.RuntimeVersion = wfdir.RuntimeDurable })

	out, err := runCLI(t, root, "wf", "run")
	if err != nil {
		t.Fatalf("a parked run exited non-zero: %v", err)
	}
	for _, want := range []string{"WAITING:", "NOT a completed verification", "resumes on its own", "run-1"} {
		if !strings.Contains(out, want) {
			t.Errorf("report missing %q:\n%s", want, out)
		}
	}
	// The poll ENDED on it rather than sitting the park out.
	if f.runPolls != 1 {
		t.Errorf("polls = %d, want exactly one — the park was waited out", f.runPolls)
	}
}

// The other way a live run ends without finishing: the wait ended and the work
// moved to a newer run between two polls, so the poll terminates on "resuming".
// Zero exit again, same reader about to record a verification — so the same loud
// line has to stand in the way, saying the work is still going elsewhere.
func TestRunResumingRunIsLoudAndStillExitsZero(t *testing.T) {
	f := newFakeInstance(t)
	f.runScript = []api.RunResponse{finishedRun(api.RunStatusResuming, api.RunHealthDone)}
	signIn(t, f)
	withFastPolling(t)
	root := liveFolder(t, f, func(live *api.Workflow) { live.RuntimeVersion = wfdir.RuntimeDurable })

	out, err := runCLI(t, root, "wf", "run")
	if err != nil {
		t.Fatalf("a resuming run exited non-zero: %v", err)
	}
	for _, want := range []string{"CONTINUED:", "NOT a completed verification", "NEWER run"} {
		if !strings.Contains(out, want) {
			t.Errorf("report missing %q:\n%s", want, out)
		}
	}
	// The poll ENDED on it: this row never executes again.
	if f.runPolls != 1 {
		t.Errorf("polls = %d, want exactly one — the wake latch was waited out", f.runPolls)
	}
}

// A failed live run exits non-zero and offers no resume: `wf test --resume`
// continues a run of the DRAFT, and there is no draft here.
func TestRunFailedRunExitsNonZeroAndOffersNoResume(t *testing.T) {
	f := newFakeInstance(t)
	failed := finishedRun(api.RunStatusError, api.RunHealthFailed)
	failed.Error = ptr("ZeroDivisionError: division by zero")
	failed.JournalEntries = 3
	f.runScript = []api.RunResponse{failed}
	signIn(t, f)
	withFastPolling(t)
	root := liveFolder(t, f, func(live *api.Workflow) { live.RuntimeVersion = wfdir.RuntimeDurable })

	out, err := runCLI(t, root, "wf", "run")
	if err == nil {
		t.Fatal("a failed run exited zero")
	}
	// The report is still printed: a failed run IS the answer.
	if !strings.Contains(out, "ZeroDivisionError") {
		t.Errorf("report did not carry the run's error:\n%s", out)
	}
	if strings.Contains(out, "--resume") {
		t.Errorf("a live failure offered a draft resume:\n%s", out)
	}
}

// --- output shape -------------------------------------------------------------

// --json is the same single object `wf test` emits: the run row flattened at the
// root, plus steps and health, on stdout and nowhere else.
func TestRunJSONIsTheFlattenedRunResponse(t *testing.T) {
	f := newFakeInstance(t)
	f.runScript = []api.RunResponse{finishedRun(api.RunStatusDone, api.RunHealthDegraded)}
	signIn(t, f)
	withFastPolling(t)
	root := liveFolder(t, f, nil)

	out, err := runCLI(t, root, "wf", "run", "--json")
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	payload := decodeJSON(t, out)
	if payload["id"] != "run-1" || payload["status"] != api.RunStatusDone {
		t.Errorf("run fields are not at the root: %+v", payload)
	}
	if payload["health"] != api.RunHealthDegraded {
		t.Errorf("health = %v, want the derived value passed through", payload["health"])
	}
}

// --- the loop's own signposts -------------------------------------------------

// The two hints that make the loop findable: publish points at the run it just
// made possible, and `wf test`'s no-draft refusal names the command that runs
// the live version instead.
func TestPublishAndTestPointAtRun(t *testing.T) {
	f := newFakeInstance(t)
	signIn(t, f)
	root := draftFolder(t, f, nil)

	out, err := runCLI(t, root, "wf", "publish")
	if err != nil {
		t.Fatalf("publish: %v", err)
	}
	if !strings.Contains(out, "Next: ronja wf run") {
		t.Errorf("publish does not point at the run:\n%s", out)
	}

	// The draft is gone with the commit, so `wf test` now has nothing to run.
	_, err = runCLI(t, root, "wf", "test")
	if err == nil {
		t.Fatal("test ran with no draft")
	}
	if !strings.Contains(err.Error(), "ronja wf run") {
		t.Errorf("the no-draft refusal does not point at the live run: %v", err)
	}
}
