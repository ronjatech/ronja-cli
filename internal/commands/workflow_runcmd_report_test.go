package commands

import (
	"strings"
	"testing"
	"time"

	"github.com/ronjatech/ronja-cli/internal/api"
	"github.com/ronjatech/ronja-cli/internal/wfdir"
)

// --- drift on the live row ----------------------------------------------------

// The local disk-vs-baseline check cannot see a colleague's republish over an
// untouched folder. A note, not a refusal: the live row is what this runs.
func TestRunNotesLiveChangesThatDidNotComeFromThisFolder(t *testing.T) {
	f := newFakeInstance(t)
	f.runScript = []api.RunResponse{finishedRun(api.RunStatusDone, api.RunHealthDone)}
	signIn(t, f)
	withFastPolling(t)
	root := liveFolder(t, f, nil)
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
	if f.runRequests[0].WorkflowID != "wf-1" {
		t.Errorf("ran %s, want the live workflow", f.runRequests[0].WorkflowID)
	}
	for _, want := range []string{"run-1", "done"} {
		if !strings.Contains(out, want) {
			t.Errorf("report missing %q:\n%s", want, out)
		}
	}
	if strings.Contains(out, "wf publish") {
		t.Errorf("a live run offered publish:\n%s", out)
	}
}

// The environment can outrank the credential file, so the instance is named
// before the POST, while it can still stop anything.
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

// The server commits the run row before dispatching, so a client deadline says
// nothing about whether it started; reported as a refusal, the natural retry
// would fire a second live run into replace-mode output tables.
func TestRunAdoptsTheRunThatStartedWhenThePostTimedOut(t *testing.T) {
	f := newFakeInstance(t)
	f.runScript = []api.RunResponse{finishedRun(api.RunStatusDone, api.RunHealthDone)}
	signIn(t, f)
	withFastPolling(t)
	root := liveFolder(t, f, nil)
	// The older run is there because the rule is "the newest run since we
	// asked", not "the newest run".
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

// "It did not start" is the sentence that makes somebody run it again.
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
	// The 409's sentence, which means the opposite.
	if strings.Contains(err.Error(), "Nothing ran") {
		t.Errorf("an unconfirmed run was reported as one that never started: %v", err)
	}
	if len(f.runRequests) != 1 {
		t.Errorf("run POSTs = %d, want exactly one", len(f.runRequests))
	}
}

// --- terminal statuses --------------------------------------------------------

// A waiting run exits zero, so the loud line is all that stands between it and
// a green tick.
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
	if f.runPolls != 1 {
		t.Errorf("polls = %d, want exactly one — the park was waited out", f.runPolls)
	}
}

// The poll ends on "resuming" when the work moved to a newer run between two
// polls; zero exit again, so the same loud line applies.
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
	// This row never executes again.
	if f.runPolls != 1 {
		t.Errorf("polls = %d, want exactly one — the wake latch was waited out", f.runPolls)
	}
}

// `wf test --resume` continues a draft run, and there is no draft here.
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
	if !strings.Contains(out, "ZeroDivisionError") {
		t.Errorf("report did not carry the run's error:\n%s", out)
	}
	if strings.Contains(out, "--resume") {
		t.Errorf("a live failure offered a draft resume:\n%s", out)
	}
}

// The notice is a fact about the run, so a live run gets it too; the resume
// hint is still withheld, since there is no draft.
func TestRunSaysNothingRanWhenTheLiveFailureWasSetup(t *testing.T) {
	f := newFakeInstance(t)
	failed := finishedRun(api.RunStatusError, api.RunHealthFailed)
	failed.Logs = ""
	failed.Error = ptr("get S3 credentials: assume role: AccessDenied")
	f.runScript = []api.RunResponse{failed}
	signIn(t, f)
	withFastPolling(t)
	root := liveFolder(t, f, nil)

	out, err := runCLI(t, root, "wf", "run")
	if err == nil {
		t.Fatal("a failed run exited zero")
	}
	if !strings.Contains(out, "Nothing in your code ran") {
		t.Errorf("a live setup failure did not say the code never ran:\n%s", out)
	}
	// See printNothingRanNotice: where, never whose fault.
	if strings.Contains(out, "not a bug in your files") {
		t.Errorf("the notice claimed the author's files are innocent, which it cannot know:\n%s", out)
	}
	if !strings.Contains(out, "Run `ronja wf run` again") {
		t.Errorf("the notice named a command that did not produce this run:\n%s", out)
	}
	if strings.Contains(out, "--resume") {
		t.Errorf("a live failure offered a draft resume:\n%s", out)
	}
}

// --- output shape -------------------------------------------------------------

// --json is the same single object `wf test` emits.
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

	// The commit consumed the draft.
	_, err = runCLI(t, root, "wf", "test")
	if err == nil {
		t.Fatal("test ran with no draft")
	}
	if !strings.Contains(err.Error(), "ronja wf run") {
		t.Errorf("the no-draft refusal does not point at the live run: %v", err)
	}
}
