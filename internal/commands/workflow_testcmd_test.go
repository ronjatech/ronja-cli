package commands

import (
	"encoding/json"
	"io"
	"net/http"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/ronjatech/ronja-cli/internal/api"
)

// `wf test` tests, in the order the command refuses things: which row, is the
// folder pushed, is there a gate, are the parameters right, would this write to
// live tables — and only then the run and its poll loop.
//
// The refusal tests all assert ZERO run requests, not just a non-zero exit.
// That is the property that matters: a refusal that happens after the POST has
// already replaced a production table is not a refusal.

// --- helpers ----------------------------------------------------------------

// withFastPolling collapses the poll cadence so a test can drive the whole loop
// without sleeping through it.
//
// The backoff CEILING is collapsed as well, not just the starting interval: a
// test that polls its way past pollsAtStartInterval would otherwise start
// growing its waits towards the real 10s cap and take the suite with it.
func withFastPolling(t *testing.T) {
	t.Helper()
	savedInterval, savedMax := runPollInterval, maxPollInterval
	runPollInterval = time.Millisecond
	maxPollInterval = 5 * time.Millisecond
	t.Cleanup(func() {
		runPollInterval, maxPollInterval = savedInterval, savedMax
	})
}

// draftFolder is the state every test here starts from: a live workflow, the
// caller's own draft of it, and a folder cloned from that draft (so it is
// clean, which is what `wf test` requires). shape adjusts the draft row —
// output tables, an approval gate, declared parameters — before the clone.
func draftFolder(t *testing.T, f *fakeInstance, shape func(*api.Workflow)) string {
	t.Helper()
	f.AddWorkflow(&api.Workflow{ID: "wf-1", Lifecycle: api.LifecycleLive},
		api.WorkflowFile{Path: "main.py", Content: "print(1)\n"})
	draft := f.AddDraft("wf-1", "draft-1", api.WorkflowFile{Path: "main.py", Content: "print(1)\n"})
	if shape != nil {
		shape(draft)
	}
	return cloneFolder(t, f, "wf-1")
}

// finishedRun is a terminal poll answer. The fake stamps the run id.
func finishedRun(status, health string) api.RunResponse {
	return api.RunResponse{
		WorkflowRun: api.WorkflowRun{Status: status, Logs: "line one\nline two\n"},
		Steps:       []api.StepDTO{},
		Health:      health,
	}
}

func runningRun() api.RunResponse {
	return api.RunResponse{
		WorkflowRun: api.WorkflowRun{Status: api.RunStatusRunning},
		Steps:       []api.StepDTO{},
		Health:      api.RunHealthDone,
	}
}

func ptr[T any](v T) *T { return &v }

// --- which row --------------------------------------------------------------

// `wf test` runs the DRAFT. A folder bound to a live workflow the caller has no
// draft of has nothing to test, and running live instead would be the opposite
// of what was asked.
func TestTestRefusesWhenThereIsNoDraft(t *testing.T) {
	f := newFakeInstance(t)
	f.AddWorkflow(&api.Workflow{ID: "wf-1", Lifecycle: api.LifecycleLive},
		api.WorkflowFile{Path: "main.py", Content: "print(1)\n"})
	signIn(t, f)
	root := cloneFolder(t, f, "wf-1")

	_, err := runCLI(t, root, "wf", "test")
	if err == nil {
		t.Fatal("test ran with no draft")
	}
	if !strings.Contains(err.Error(), "wf push") {
		t.Errorf("refusal does not point at push: %v", err)
	}
	if len(f.runRequests) != 0 {
		t.Errorf("started a run anyway: %+v", f.runRequests)
	}
}

func TestTestRefusesAnUnboundFolder(t *testing.T) {
	f := newFakeInstance(t)
	signIn(t, f)
	root := initFolder(t, f, map[string]string{"main.py": "x\n"})

	_, err := runCLI(t, root, "wf", "test")
	if err == nil || !strings.Contains(err.Error(), "nothing to test") {
		t.Fatalf("err = %v, want a refusal naming the missing workflow", err)
	}
	if len(f.runRequests) != 0 {
		t.Errorf("started a run anyway: %+v", f.runRequests)
	}
}

// --- staleness --------------------------------------------------------------

func TestTestRefusesAFolderWithUnpushedChanges(t *testing.T) {
	f := newFakeInstance(t)
	signIn(t, f)
	root := draftFolder(t, f, nil)
	writeLocal(t, root, "main.py", "print(2)\n")

	_, err := runCLI(t, root, "wf", "test")
	if err == nil {
		t.Fatal("test ran against a stale draft")
	}
	for _, want := range []string{"main.py", "wf push", "--stale-ok"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("refusal missing %q: %v", want, err)
		}
	}
	if len(f.runRequests) != 0 {
		t.Errorf("started a run anyway: %+v", f.runRequests)
	}
}

func TestTestStaleOKRunsTheDraftAsItStands(t *testing.T) {
	f := newFakeInstance(t)
	f.runScript = []api.RunResponse{finishedRun(api.RunStatusDone, api.RunHealthDone)}
	signIn(t, f)
	withFastPolling(t)
	root := draftFolder(t, f, nil)
	writeLocal(t, root, "main.py", "print(2)\n")

	if _, err := runCLI(t, root, "wf", "test", "--stale-ok"); err != nil {
		t.Fatalf("--stale-ok did not run: %v", err)
	}
	if len(f.runRequests) != 1 {
		t.Fatalf("runs = %+v, want exactly one", f.runRequests)
	}
	// The DRAFT, not the live row.
	if f.runRequests[0].WorkflowID != "draft-1" {
		t.Errorf("ran %s, want the draft", f.runRequests[0].WorkflowID)
	}
}

// --- drift on the draft -----------------------------------------------------

// The web builder edits the SAME per-user draft, so the code `wf test` is about
// to run can carry an edit that never came from this folder. That is a note,
// not a refusal — the draft is what this command runs by definition — but it
// must be said, because a clean folder otherwise reads as "you are testing
// exactly what you are looking at".
func TestTestNotesDraftChangesThatDidNotComeFromThisFolder(t *testing.T) {
	f := newFakeInstance(t)
	f.runScript = []api.RunResponse{finishedRun(api.RunStatusDone, api.RunHealthDone)}
	signIn(t, f)
	withFastPolling(t)
	root := draftFolder(t, f, nil)
	// Somebody edits the draft in a browser tab.
	f.putFile("draft-1", "main.py", "print(99)\n")

	var err error
	stderr := captureStderr(t, func() { _, err = runCLI(t, root, "wf", "test") })
	if err != nil {
		t.Fatalf("the note blocked the run: %v", err)
	}
	if !strings.Contains(stderr, "did not come from this folder") {
		t.Errorf("no drift note for a draft edited elsewhere:\n%s", stderr)
	}
	if len(f.runRequests) != 1 {
		t.Errorf("runs = %+v, want the run to have gone ahead", f.runRequests)
	}
}

// ...and silence when there is nothing to say. A note on every run is a note
// nobody reads.
func TestTestSaysNothingWhenTheDraftMatchesTheFolder(t *testing.T) {
	f := newFakeInstance(t)
	f.runScript = []api.RunResponse{finishedRun(api.RunStatusDone, api.RunHealthDone)}
	signIn(t, f)
	withFastPolling(t)
	root := draftFolder(t, f, nil)

	var err error
	stderr := captureStderr(t, func() { _, err = runCLI(t, root, "wf", "test") })
	if err != nil {
		t.Fatalf("test: %v", err)
	}
	if strings.Contains(stderr, "did not come from this folder") {
		t.Errorf("drift note on an untouched draft:\n%s", stderr)
	}
}

// --- the approval gate ------------------------------------------------------

// The locked decision: a gated workflow fail-fasts before any run POST, and the
// message never suggests turning the gate off — that edit would ride the draft
// onto the live workflow at publish.
func TestTestRefusesAnApprovalGatedDraftBeforeRunning(t *testing.T) {
	f := newFakeInstance(t)
	signIn(t, f)
	root := draftFolder(t, f, func(draft *api.Workflow) {
		draft.ApprovalGate = &api.WorkflowApprovalGate{Enabled: true}
	})

	_, err := runCLI(t, root, "wf", "test")
	if err == nil {
		t.Fatal("test ran a gated workflow")
	}
	if len(f.runRequests) != 0 {
		t.Fatalf("gate preflight let a run start: %+v", f.runRequests)
	}
	for _, want := range []string{"approval gate", "agent session"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("refusal missing %q: %v", want, err)
		}
	}
	// The workaround must not be advertised, in any spelling.
	for _, forbidden := range []string{"disable", "turn off", "approvalGate"} {
		if strings.Contains(strings.ToLower(err.Error()), strings.ToLower(forbidden)) {
			t.Errorf("refusal suggests the gate-off workaround (%q): %v", forbidden, err)
		}
	}
}

// A gate that exists but is switched off is not a gate.
func TestTestRunsWithADisabledGate(t *testing.T) {
	f := newFakeInstance(t)
	f.runScript = []api.RunResponse{finishedRun(api.RunStatusDone, api.RunHealthDone)}
	signIn(t, f)
	withFastPolling(t)
	root := draftFolder(t, f, func(draft *api.Workflow) {
		draft.ApprovalGate = &api.WorkflowApprovalGate{Enabled: false}
	})

	if _, err := runCLI(t, root, "wf", "test"); err != nil {
		t.Fatalf("a disabled gate blocked the run: %v", err)
	}
}

// --- parameters -------------------------------------------------------------

func TestTestParameterValidation(t *testing.T) {
	declared := []api.WorkflowParameter{
		{Name: "month", Type: "string", Required: true},
		{Name: "limit", Type: "number"},
		{Name: "region", Type: "select", Options: []string{"EU", "US"}},
		{Name: "anywhere", Type: "select"},
		{Name: "note", Type: "string"},
		{Name: "since", Type: "date"},
		{Name: "defaulted", Type: "string", Required: true, DefaultValue: "x"},
	}
	cases := []struct {
		name    string
		args    []string
		wantErr string
		want    map[string]any
	}{{
		name:    "unknown key names what is declared",
		args:    []string{"--param", "month=2026-07", "--param", "moth=1"},
		wantErr: "not declared",
	}, {
		name:    "required without a default must be supplied",
		args:    []string{"--param", "limit=1"},
		wantErr: `requires parameter "month"`,
	}, {
		name: "a required parameter WITH a default may be omitted",
		args: []string{"--param", "month=2026-07"},
		want: map[string]any{"month": "2026-07"},
	}, {
		name: "a number is coerced, not passed as text",
		args: []string{"--param", "month=2026-07", "--param", "limit=12.5"},
		want: map[string]any{"month": "2026-07", "limit": 12.5},
	}, {
		name:    "a non-numeric number is refused",
		args:    []string{"--param", "month=2026-07", "--param", "limit=lots"},
		wantErr: "expects a number",
	}, {
		name:    "a select value outside its options is refused",
		args:    []string{"--param", "month=2026-07", "--param", "region=APAC"},
		wantErr: "not one of its options",
	}, {
		name: "a select with no declared options accepts anything",
		args: []string{"--param", "month=2026-07", "--param", "anywhere=whatever"},
		want: map[string]any{"month": "2026-07", "anywhere": "whatever"},
	}, {
		// The value may perfectly well contain an '='; only the first splits.
		name: "only the first equals splits",
		args: []string{"--param", "month=2026-07", "--param", "note=a=b=c"},
		want: map[string]any{"month": "2026-07", "note": "a=b=c"},
	}, {
		name: "a date crosses the wire as the string it was written as",
		args: []string{"--param", "month=2026-07", "--param", "since=2026-01-01"},
		want: map[string]any{"month": "2026-07", "since": "2026-01-01"},
	}, {
		name:    "a value with no equals at all is a usage error",
		args:    []string{"--param", "month"},
		wantErr: "key=value",
	}, {
		name:    "the same parameter twice is refused rather than last-wins",
		args:    []string{"--param", "month=a", "--param", "month=b"},
		wantErr: "more than once",
	}}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			f := newFakeInstance(t)
			f.runScript = []api.RunResponse{finishedRun(api.RunStatusDone, api.RunHealthDone)}
			signIn(t, f)
			withFastPolling(t)
			root := draftFolder(t, f, func(draft *api.Workflow) { draft.Parameters = declared })

			_, err := runCLI(t, root, append([]string{"wf", "test"}, tc.args...)...)
			if tc.wantErr != "" {
				if err == nil || !strings.Contains(err.Error(), tc.wantErr) {
					t.Fatalf("err = %v, want one containing %q", err, tc.wantErr)
				}
				if len(f.runRequests) != 0 {
					t.Errorf("bad parameters still started a run: %+v", f.runRequests)
				}
				return
			}
			if err != nil {
				t.Fatalf("test: %v", err)
			}
			if len(f.runRequests) != 1 {
				t.Fatalf("runs = %+v, want one", f.runRequests)
			}
			got := f.runRequests[0].ParameterValues
			if len(got) != len(tc.want) {
				t.Fatalf("parameterValues = %+v, want %+v", got, tc.want)
			}
			for name, want := range tc.want {
				if got[name] != want {
					t.Errorf("parameterValues[%q] = %#v, want %#v", name, got[name], want)
				}
			}
		})
	}
}

// A workflow with no parameters sends no parameterValues at all.
func TestTestSendsNoParameterValuesWhenThereAreNone(t *testing.T) {
	f := newFakeInstance(t)
	f.runScript = []api.RunResponse{finishedRun(api.RunStatusDone, api.RunHealthDone)}
	signIn(t, f)
	withFastPolling(t)
	root := draftFolder(t, f, nil)

	if _, err := runCLI(t, root, "wf", "test"); err != nil {
		t.Fatalf("test: %v", err)
	}
	if got := f.runRequests[0].ParameterValues; len(got) != 0 {
		t.Errorf("parameterValues = %+v, want none", got)
	}
}

// --- output safety ----------------------------------------------------------

// The bootstrap case, and the common one: no bound output tables, no friction.
func TestTestRunsFreelyWithNoOutputTables(t *testing.T) {
	f := newFakeInstance(t)
	f.runScript = []api.RunResponse{finishedRun(api.RunStatusDone, api.RunHealthDone)}
	signIn(t, f)
	withFastPolling(t)
	root := draftFolder(t, f, nil)

	out, err := runCLI(t, root, "wf", "test")
	if err != nil {
		t.Fatalf("test: %v", err)
	}
	if strings.Contains(out, "--write-live") {
		t.Errorf("mentioned --write-live for a draft with no output tables:\n%s", out)
	}
	if len(f.runRequests) != 1 {
		t.Errorf("runs = %+v, want one", f.runRequests)
	}
}

func TestTestRefusesBoundOutputTablesWithoutWriteLive(t *testing.T) {
	f := newFakeInstance(t)
	f.tableNames["tbl-1"] = "Monthly revenue"
	f.tableNames["tbl-2"] = "Customer ledger"
	signIn(t, f)
	root := draftFolder(t, f, func(draft *api.Workflow) {
		draft.OutputTableIDs = []string{"tbl-1", "tbl-2"}
	})

	_, err := runCLI(t, root, "wf", "test")
	if err == nil {
		t.Fatal("wrote to live tables without --write-live")
	}
	if len(f.runRequests) != 0 {
		t.Fatalf("refusal came after the run started: %+v", f.runRequests)
	}
	for _, want := range []string{"tbl-1", "Monthly revenue", "tbl-2", "Customer ledger", "--write-live", "2 live tables"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("refusal missing %q:\n%v", want, err)
		}
	}
}

func TestTestRunsBoundOutputTablesWithWriteLive(t *testing.T) {
	f := newFakeInstance(t)
	f.runScript = []api.RunResponse{finishedRun(api.RunStatusDone, api.RunHealthDone)}
	signIn(t, f)
	withFastPolling(t)
	root := draftFolder(t, f, func(draft *api.Workflow) {
		draft.OutputTableIDs = []string{"tbl-1"}
	})

	if _, err := runCLI(t, root, "wf", "test", "--write-live"); err != nil {
		t.Fatalf("--write-live did not run: %v", err)
	}
	if len(f.runRequests) != 1 {
		t.Errorf("runs = %+v, want one", f.runRequests)
	}
}

// The names are decoration; the ids are the warning. A token that cannot reach
// the table surface must still get told which tables would be overwritten.
func TestTestWriteLiveRefusalSurvivesNameLookupFailure(t *testing.T) {
	f := newFakeInstance(t)
	f.failTableLookup = 403
	signIn(t, f)
	root := draftFolder(t, f, func(draft *api.Workflow) {
		draft.OutputTableIDs = []string{"tbl-1"}
	})

	_, err := runCLI(t, root, "wf", "test")
	if err == nil {
		t.Fatal("a failed name lookup dropped the refusal")
	}
	if !strings.Contains(err.Error(), "tbl-1") || !strings.Contains(err.Error(), "--write-live") {
		t.Errorf("refusal lost its content: %v", err)
	}
}

// --- a refused run POST -----------------------------------------------------

// Some refusals only the server can make: the credit kill-stop is the one that
// matters, and it arrives as a 400 on the run POST. Its words are the server's
// and must reach the caller unparaphrased — a CLI that answered "run failed"
// would hide the one sentence that says what to do about it.
func TestTestPassesThroughTheServersRefusalOfTheRunPost(t *testing.T) {
	f := newFakeInstance(t)
	f.failRun = http.StatusBadRequest
	f.failRunMessage = "this organisation is out of credits — runs are paused until the balance is topped up"
	signIn(t, f)
	withFastPolling(t)
	root := draftFolder(t, f, nil)

	_, err := runCLI(t, root, "wf", "test")
	if err == nil {
		t.Fatal("a refused run POST exited zero")
	}
	if !strings.Contains(err.Error(), f.failRunMessage) {
		t.Errorf("the server's own words did not survive:\n  got:  %v\n  want: %s", err, f.failRunMessage)
	}
	// The POST was made and refused; nothing was polled, because no run exists.
	if len(f.runRequests) != 1 {
		t.Errorf("runRequests = %+v, want the single refused POST", f.runRequests)
	}
	if f.runPolls != 0 {
		t.Errorf("polled %d times for a run that never started", f.runPolls)
	}
}

// --- the poll loop ----------------------------------------------------------

func TestTestPollsToDoneAndReportsTheTimeline(t *testing.T) {
	f := newFakeInstance(t)
	f.runScript = []api.RunResponse{
		{WorkflowRun: api.WorkflowRun{Status: api.RunStatusRunning}, Health: api.RunHealthDone,
			Steps: []api.StepDTO{{ID: "s1", Name: "Fetch data", Status: "running"}}},
		{WorkflowRun: api.WorkflowRun{Status: api.RunStatusRunning}, Health: api.RunHealthDone,
			Steps: []api.StepDTO{
				{ID: "s1", Name: "Fetch data", Status: "done", DurationMs: ptr(int64(1200))},
				{ID: "s2", Name: "Write report", Status: "running"},
			}},
		{WorkflowRun: api.WorkflowRun{Status: api.RunStatusDone, Logs: "all good\n"}, Health: api.RunHealthDone,
			Steps: []api.StepDTO{
				{ID: "s1", Name: "Fetch data", Status: "done", DurationMs: ptr(int64(1200))},
				{ID: "s2", Name: "Write report", Status: "done", DurationMs: ptr(int64(300))},
			}},
	}
	signIn(t, f)
	withFastPolling(t)
	root := draftFolder(t, f, nil)

	out, err := runCLI(t, root, "wf", "test")
	if err != nil {
		t.Fatalf("test: %v", err)
	}
	if f.runPolls < 3 {
		t.Errorf("polls = %d, want at least 3 (it stopped before the run finished)", f.runPolls)
	}
	for _, want := range []string{"run-1", "done", "Fetch data", "Write report", "all good"} {
		if !strings.Contains(out, want) {
			t.Errorf("report missing %q:\n%s", want, out)
		}
	}
}

func TestTestExitsNonZeroOnAFailedRun(t *testing.T) {
	f := newFakeInstance(t)
	failed := finishedRun(api.RunStatusError, api.RunHealthFailed)
	failed.Error = ptr("ZeroDivisionError: division by zero")
	f.runScript = []api.RunResponse{failed}
	signIn(t, f)
	withFastPolling(t)
	root := draftFolder(t, f, nil)

	out, err := runCLI(t, root, "wf", "test")
	if err == nil {
		t.Fatal("a failed run exited zero")
	}
	// The report is still printed: a failed run IS the answer.
	if !strings.Contains(out, "ZeroDivisionError") {
		t.Errorf("report did not carry the run's error:\n%s", out)
	}
}

// Exit zero means the run SUCCEEDED and nothing else. A status this CLI does
// not know — one added server-side after it shipped — ends the poll (see
// api.RunResponse.Running) and must NOT be reported as a pass: a green exit for
// "cancelled" is the kind of thing a nightly pipeline believes.
func TestTestExitsNonZeroOnAStatusItDoesNotRecognise(t *testing.T) {
	f := newFakeInstance(t)
	f.runScript = []api.RunResponse{finishedRun("cancelled", api.RunHealthDone)}
	signIn(t, f)
	withFastPolling(t)
	root := draftFolder(t, f, nil)

	out, err := runCLI(t, root, "wf", "test")
	if err == nil {
		t.Fatal("an unrecognised terminal status exited zero")
	}
	// The status is NAMED — the whole value of not hanging on it is that the
	// caller learns something the CLI does not know.
	if !strings.Contains(err.Error(), "cancelled") {
		t.Errorf("error does not name the status: %v", err)
	}
	if !strings.Contains(out, "cancelled") {
		t.Errorf("report did not carry the status:\n%s", out)
	}
	// Ended the poll rather than waiting the status out.
	if f.runPolls != 1 {
		t.Errorf("polls = %d, want exactly one", f.runPolls)
	}
}

// Ctrl-C stops the WAITING. The run is somebody else's process on somebody
// else's machine and keeps going, which is the one thing a person needs told at
// that moment — and it only gets said because the command installs its own
// interrupt handler (cobra hands it a background context).
func TestTestCtrlCStopsWaitingAndSaysTheRunSurvives(t *testing.T) {
	f := newFakeInstance(t)
	// Never terminates: the only way out of the poll loop is the interrupt.
	f.runScript = []api.RunResponse{runningRun()}
	var once sync.Once
	polled := make(chan struct{})
	f.onPoll = func() { once.Do(func() { close(polled) }) }
	signIn(t, f)
	withFastPolling(t)
	root := draftFolder(t, f, nil)

	go func() {
		// A served poll proves the handler is installed: it is registered before
		// the command makes any request at all.
		<-polled
		self, err := os.FindProcess(os.Getpid())
		if err != nil {
			return
		}
		_ = self.Signal(os.Interrupt)
	}()

	_, err := runCLI(t, root, "wf", "test")
	if err == nil {
		t.Fatal("Ctrl-C during the poll loop exited zero")
	}
	for _, want := range []string{"run-1", "continues server-side"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("interrupt message missing %q: %v", want, err)
		}
	}
}

// A run that outlives --timeout is reported as still going, with its id, and a
// non-zero exit. The run itself is untouched.
func TestTestTimesOutWithoutKillingTheRun(t *testing.T) {
	f := newFakeInstance(t)
	f.runScript = []api.RunResponse{runningRun()}
	signIn(t, f)
	withFastPolling(t)
	root := draftFolder(t, f, nil)

	out, err := runCLI(t, root, "wf", "test", "--timeout", "30ms", "--json")
	if err == nil {
		t.Fatal("waited past --timeout")
	}
	for _, want := range []string{"run-1", "still going"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("timeout error missing %q: %v", want, err)
		}
	}
	payload := decodeJSON(t, out)
	if payload["id"] != "run-1" || payload["status"] != api.RunStatusRunning {
		t.Errorf("timeout payload = %+v, want the run as last seen", payload)
	}
}

// 0 means "wait for as long as it takes" — a documented value a negative
// duration has no claim to. Refused rather than treated as another spelling of
// forever, which is what it used to be.
func TestTestRejectsANegativeTimeout(t *testing.T) {
	f := newFakeInstance(t)
	signIn(t, f)
	root := draftFolder(t, f, nil)

	_, err := runCLI(t, root, "wf", "test", "--timeout", "-5m")
	if err == nil {
		t.Fatal("accepted a negative --timeout")
	}
	if !strings.Contains(err.Error(), "negative") {
		t.Errorf("refusal does not say what is wrong: %v", err)
	}
	if len(f.runRequests) != 0 {
		t.Errorf("started a run anyway: %+v", f.runRequests)
	}
}

// A pod restarting mid-deploy is not a reason to abandon a run.
func TestTestToleratesTransientPollFailures(t *testing.T) {
	f := newFakeInstance(t)
	f.failRunGets = maxConsecutivePollFailures - 1
	f.runScript = []api.RunResponse{finishedRun(api.RunStatusDone, api.RunHealthDone)}
	signIn(t, f)
	withFastPolling(t)
	root := draftFolder(t, f, nil)

	if _, err := runCLI(t, root, "wf", "test"); err != nil {
		t.Fatalf("gave up on %d transient failures: %v", maxConsecutivePollFailures-1, err)
	}
}

func TestTestGivesUpAfterAnUnbrokenRunOfPollFailures(t *testing.T) {
	f := newFakeInstance(t)
	f.failRunGets = maxConsecutivePollFailures + 5
	f.runScript = []api.RunResponse{finishedRun(api.RunStatusDone, api.RunHealthDone)}
	signIn(t, f)
	withFastPolling(t)
	root := draftFolder(t, f, nil)

	_, err := runCLI(t, root, "wf", "test")
	if err == nil {
		t.Fatal("polled a dead instance forever")
	}
	if !strings.Contains(err.Error(), "continues server-side") {
		t.Errorf("give-up message does not say the run survives: %v", err)
	}
}

// --- output shape -----------------------------------------------------------

// --json is ONE object: the run row FLATTENED at the root (mirroring the
// server's embedded *rdb.WorkflowRun) plus steps and health.
func TestTestJSONIsTheFlattenedRunResponse(t *testing.T) {
	f := newFakeInstance(t)
	done := finishedRun(api.RunStatusDone, api.RunHealthDegraded)
	done.TableOutputs = []api.WorkflowRunTableOutput{
		{ModelID: "tbl-1", DisplayName: "Monthly revenue", WriteMode: "replace", FileCount: 2},
	}
	done.Steps = []api.StepDTO{{ID: "s1", Name: "Fetch", Status: "failed", Error: ptr("nope")}}
	f.runScript = []api.RunResponse{done}
	signIn(t, f)
	withFastPolling(t)
	root := draftFolder(t, f, nil)

	out, err := runCLI(t, root, "wf", "test", "--json")
	if err != nil {
		t.Fatalf("test: %v", err)
	}
	payload := decodeJSON(t, out)
	if payload["id"] != "run-1" || payload["status"] != api.RunStatusDone {
		t.Errorf("run fields are not at the root: %+v", payload)
	}
	if payload["health"] != api.RunHealthDegraded {
		t.Errorf("health = %v, want the derived value passed through", payload["health"])
	}
	if steps, ok := payload["steps"].([]any); !ok || len(steps) != 1 {
		t.Errorf("steps = %v, want one", payload["steps"])
	}
	if tables, ok := payload["tableOutputs"].([]any); !ok || len(tables) != 1 {
		t.Errorf("tableOutputs = %v, want one", payload["tableOutputs"])
	}
}

func TestTestLogsModes(t *testing.T) {
	long := strings.Repeat("noise\n", 200) + "the interesting bit\n"
	for _, tc := range []struct {
		mode     string
		wantTail bool
		wantAny  bool
	}{
		{mode: "tail", wantTail: true, wantAny: true},
		{mode: "full", wantTail: false, wantAny: true},
		{mode: "none", wantTail: false, wantAny: false},
	} {
		t.Run(tc.mode, func(t *testing.T) {
			f := newFakeInstance(t)
			done := finishedRun(api.RunStatusDone, api.RunHealthDone)
			done.Logs = long
			f.runScript = []api.RunResponse{done}
			signIn(t, f)
			withFastPolling(t)
			root := draftFolder(t, f, nil)

			out, err := runCLI(t, root, "wf", "test", "--logs", tc.mode)
			if err != nil {
				t.Fatalf("test: %v", err)
			}
			if got := strings.Contains(out, "the interesting bit"); got != tc.wantAny {
				t.Errorf("logs present = %v, want %v", got, tc.wantAny)
			}
			if got := strings.Contains(out, "last 40 of 201 lines"); got != tc.wantTail {
				t.Errorf("tail header present = %v, want %v", got, tc.wantTail)
			}
		})
	}
}

func TestTestRejectsAnUnknownLogsMode(t *testing.T) {
	f := newFakeInstance(t)
	signIn(t, f)
	root := draftFolder(t, f, nil)

	if _, err := runCLI(t, root, "wf", "test", "--logs", "everything"); err == nil {
		t.Fatal("accepted an unknown --logs mode")
	}
}

// --- no prompts anywhere ----------------------------------------------------

// The locked decision: --write-live is a flag, never a y/N fallback. These
// tests run without a terminal, so a prompt would either hang or be answered by
// accident — pinning its ABSENCE is what keeps a later "just ask on a TTY"
// change honest.
func TestTestNeverPrompts(t *testing.T) {
	f := newFakeInstance(t)
	f.runScript = []api.RunResponse{finishedRun(api.RunStatusDone, api.RunHealthDone)}
	f.tableNames["tbl-1"] = "Monthly revenue"
	signIn(t, f)
	withFastPolling(t)
	root := draftFolder(t, f, func(draft *api.Workflow) {
		draft.OutputTableIDs = []string{"tbl-1"}
	})

	// Refused without the flag...
	out, err := runCLI(t, root, "wf", "test")
	if err == nil {
		t.Fatal("ran without --write-live")
	}
	for _, text := range []string{out, err.Error()} {
		if strings.Contains(strings.ToLower(text), "[y/n]") || strings.Contains(text, "Continue?") {
			t.Errorf("a prompt appeared:\n%s", text)
		}
	}
	// ...and runs with it, still without asking anything.
	out, err = runCLI(t, root, "wf", "test", "--write-live")
	if err != nil {
		t.Fatalf("test: %v", err)
	}
	if strings.Contains(strings.ToLower(out), "[y/n]") {
		t.Errorf("a prompt appeared:\n%s", out)
	}
}

// --- unit: number coercion --------------------------------------------------

// A "number" parameter that is a whole number crosses the wire as an INTEGER.
//
// float64 carries 53 bits of mantissa, so a row id or an epoch-nanosecond
// timestamp routed through one arrives at the Python script silently rounded —
// and a value that is wrong by one is worse than a value that was refused. The
// marshalled form is asserted too, because the integer only survives if it is
// still an integer when it is encoded.
func TestCoerceParamKeepsWholeNumbersExact(t *testing.T) {
	p := api.WorkflowParameter{Name: "id", Type: "number"}

	big := "9007199254740993" // 2^53 + 1: the smallest integer a float64 loses
	got, err := coerceParam(p, big)
	if err != nil {
		t.Fatalf("coerce %s: %v", big, err)
	}
	if got != int64(9007199254740993) {
		t.Errorf("coerced %s to %#v, want an exact int64", big, got)
	}
	encoded, err := json.Marshal(map[string]any{"id": got})
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	if string(encoded) != `{"id":9007199254740993}` {
		t.Errorf("wire form = %s, want the integer unrounded", encoded)
	}

	// Fractions still work, and are still float64.
	if got, err := coerceParam(p, "12.5"); err != nil || got != 12.5 {
		t.Errorf("coerce 12.5 = %#v, %v", got, err)
	}
	// As do negatives, which ParseInt handles and a naive digit check would not.
	if got, err := coerceParam(p, "-7"); err != nil || got != int64(-7) {
		t.Errorf("coerce -7 = %#v, %v", got, err)
	}
	// And a non-number is still refused rather than passed through as text.
	if _, err := coerceParam(p, "lots"); err == nil {
		t.Error("coerced a non-number")
	}
}

// --- unit: the poll schedule --------------------------------------------------

// Each poll re-reads the whole run, log and all, so a long run's polling gets
// progressively more expensive at a fixed cadence. The backoff is the cheap
// mitigation, and both halves of it matter: the FIRST polls stay at the
// starting interval (a run that finishes in four seconds must still be
// reported in four seconds) and the growth STOPS (nobody should wait minutes
// to be told a run ended).
func TestPollIntervalBacksOffButStartsPrompt(t *testing.T) {
	interval := runPollInterval
	var elapsed time.Duration
	// The first waits are unchanged — this is the whole reason for the
	// pollsAtStartInterval hold.
	for poll := 1; poll <= pollsAtStartInterval; poll++ {
		if interval != runPollInterval {
			t.Fatalf("wait %d = %s, want the starting %s", poll, interval, runPollInterval)
		}
		elapsed += interval
		interval = nextPollInterval(interval, poll)
	}
	// ...and then it grows to the cap, and stops there.
	polls := pollsAtStartInterval
	for interval < maxPollInterval && polls < 1000 {
		polls++
		elapsed += interval
		interval = nextPollInterval(interval, polls)
	}
	if interval != maxPollInterval {
		t.Fatalf("interval settled at %s, want the %s cap", interval, maxPollInterval)
	}
	if nextPollInterval(interval, polls+1) != maxPollInterval {
		t.Errorf("the cap is not a cap: %s", nextPollInterval(interval, polls+1))
	}
	// "Roughly a minute of polling" — the number nobody should have to
	// re-derive from the constants when one of them is retuned.
	if elapsed < 30*time.Second || elapsed > 90*time.Second {
		t.Errorf("reached the cap after %s of polling, want roughly a minute", elapsed)
	}
}

// --- unit: the step narrator ------------------------------------------------

// The live timeline goes to stderr, so it is exercised directly: the property
// is that a step prints on each TRANSITION and not on every poll.
func TestNarrateStepsPrintsEachTransitionOnce(t *testing.T) {
	seen := map[string]string{}
	first := []api.StepDTO{{ID: "s1", Name: "Fetch data", Status: "running"}}
	second := []api.StepDTO{{ID: "s1", Name: "Fetch data", Status: "done", DurationMs: ptr(int64(1500))}}

	out := captureStderr(t, func() {
		narrateSteps(first, seen)
		narrateSteps(first, seen) // an unchanged poll must add nothing
		narrateSteps(second, seen)
	})

	if got := strings.Count(out, "Fetch data"); got != 2 {
		t.Errorf("printed %d lines for one step's two states:\n%s", got, out)
	}
	if !strings.Contains(out, "1.5s") {
		t.Errorf("duration not rendered:\n%s", out)
	}
}

// captureStderr collects what fn writes to os.Stderr. A temp file rather than a
// pipe, for the same reason captureStdout uses one.
func captureStderr(t *testing.T, fn func()) string {
	t.Helper()
	tmp, err := os.CreateTemp(t.TempDir(), "stderr-*")
	if err != nil {
		t.Fatalf("create capture file: %v", err)
	}
	saved := os.Stderr
	os.Stderr = tmp
	fn()
	os.Stderr = saved

	if _, err := tmp.Seek(0, io.SeekStart); err != nil {
		t.Fatalf("rewind capture file: %v", err)
	}
	body, err := io.ReadAll(tmp)
	if err != nil {
		t.Fatalf("read capture file: %v", err)
	}
	tmp.Close()
	return string(body)
}
