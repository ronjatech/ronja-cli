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
	"github.com/ronjatech/ronja-cli/internal/wfdir"
)

// The refusal tests assert zero run requests, not just a non-zero exit: a
// refusal after the POST may already have replaced a production table.

// --- helpers ----------------------------------------------------------------

// withFastPolling collapses the poll cadence, including the backoff ceiling: a
// test polling past pollsAtStartInterval would otherwise grow towards the real
// cap.
func withFastPolling(t *testing.T) {
	t.Helper()
	savedInterval, savedMax := runPollInterval, maxPollInterval
	runPollInterval = time.Millisecond
	maxPollInterval = 5 * time.Millisecond
	t.Cleanup(func() {
		runPollInterval, maxPollInterval = savedInterval, savedMax
	})
}

// draftFolder is a live workflow, the caller's draft of it, and a clean folder
// cloned from that draft; shape adjusts the draft row before the clone.
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

// finishedRun is a terminal poll answer; the fake stamps the run id.
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
	if f.runRequests[0].WorkflowID != "draft-1" {
		t.Errorf("ran %s, want the draft", f.runRequests[0].WorkflowID)
	}
}

// --- drift on the draft -----------------------------------------------------

// The web builder edits the same per-user draft, so the code about to run can
// carry an edit this folder never made. A note, not a refusal.
func TestTestNotesDraftChangesThatDidNotComeFromThisFolder(t *testing.T) {
	f := newFakeInstance(t)
	f.runScript = []api.RunResponse{finishedRun(api.RunStatusDone, api.RunHealthDone)}
	signIn(t, f)
	withFastPolling(t)
	root := draftFolder(t, f, nil)
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

// The message never suggests turning the gate off: that edit would ride the
// draft onto the live workflow at publish.
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
	for _, forbidden := range []string{"disable", "turn off", "approvalGate"} {
		if strings.Contains(strings.ToLower(err.Error()), strings.ToLower(forbidden)) {
			t.Errorf("refusal suggests the gate-off workaround (%q): %v", forbidden, err)
		}
	}
}

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

// A token that cannot read the table names must still be told which ids would
// be overwritten.
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

// --- resume -----------------------------------------------------------------

// durableFolder is draftFolder with the row declaring the durable runtime; the
// CLI reads the runtime from the row, as the run funnel does.
func durableFolder(t *testing.T, f *fakeInstance, shape func(*api.Workflow)) string {
	t.Helper()
	return draftFolder(t, f, func(draft *api.Workflow) {
		draft.RuntimeVersion = wfdir.RuntimeDurable
		if shape != nil {
			shape(draft)
		}
	})
}

// declareDurableInManifest is the fallback for an instance that sends no
// runtimeVersion: a 0 means "not said", not runtime 1.
func declareDurableInManifest(t *testing.T, root string) {
	t.Helper()
	manifest, err := wfdir.LoadManifest(root, wfdir.WorkflowKind)
	if err != nil {
		t.Fatalf("load manifest: %v", err)
	}
	manifest.Runtime = wfdir.RuntimeDurable
	if err := wfdir.SaveManifest(root, manifest); err != nil {
		t.Fatalf("save manifest: %v", err)
	}
}

func historicRun(id, status string, executedAt time.Time) api.WorkflowRun {
	return api.WorkflowRun{ID: id, WorkflowID: "draft-1", Status: status, ExecutedAt: executedAt}
}

// The server refuses a resume carrying parameter values, since a changed
// parameter invalidates every journaled step. The history is staged out of
// order because the endpoint has no default ordering.
func TestTestResumeSendsTheLastFailedRunAndNoParameters(t *testing.T) {
	f := newFakeInstance(t)
	resumed := finishedRun(api.RunStatusDone, api.RunHealthDone)
	resumed.Steps = []api.StepDTO{
		{ID: "span-1", Name: "Load orders", Status: "skipped", Replayed: true},
		{ID: "span-2", Name: "Publish", Status: "done"},
	}
	f.runScript = []api.RunResponse{resumed}
	base := time.Date(2026, 8, 13, 9, 0, 0, 0, time.UTC)
	f.runHistory["draft-1"] = []api.WorkflowRun{
		historicRun("run-old-failure", api.RunStatusError, base),
		historicRun("run-latest-failure", api.RunStatusError, base.Add(2*time.Hour)),
		historicRun("run-done", api.RunStatusDone, base.Add(time.Hour)),
	}
	signIn(t, f)
	withFastPolling(t)
	root := durableFolder(t, f, nil)

	out, err := runCLI(t, root, "wf", "test", "--resume")
	if err != nil {
		t.Fatalf("test --resume: %v", err)
	}
	// A never-run continue_on_error step also says "skipped".
	if !strings.Contains(out, "replayed  Load orders") {
		t.Errorf("the report does not distinguish a journal hit from a skip:\n%s", out)
	}
	if len(f.runRequests) != 1 {
		t.Fatalf("runRequests = %+v, want one", f.runRequests)
	}
	got := f.runRequests[0]
	if got.ResumeOfRunID != "run-latest-failure" {
		t.Errorf("resumed %q, want the most recent failed run", got.ResumeOfRunID)
	}
	if len(got.ParameterValues) != 0 {
		t.Errorf("a resume sent parameterValues %+v", got.ParameterValues)
	}
	// Without a requested order the limit returns an arbitrary window.
	if len(f.runQueries) != 1 || !strings.Contains(f.runQueries[0], "orderBy=executed_at+desc") {
		t.Errorf("run history query = %v, want an explicit newest-first ordering", f.runQueries)
	}
}

func TestTestResumeWithNoFailedRunRefusesRatherThanStartingOne(t *testing.T) {
	f := newFakeInstance(t)
	base := time.Date(2026, 8, 13, 9, 0, 0, 0, time.UTC)
	f.runHistory["draft-1"] = []api.WorkflowRun{historicRun("run-done", api.RunStatusDone, base)}
	signIn(t, f)
	withFastPolling(t)
	root := durableFolder(t, f, nil)

	_, err := runCLI(t, root, "wf", "test", "--resume")
	if err == nil {
		t.Fatal("--resume with no failed run exited zero")
	}
	if !strings.Contains(err.Error(), "no failed run") {
		t.Errorf("error does not say what is missing: %v", err)
	}
	if len(f.runRequests) != 0 {
		t.Errorf("a refused resume started a run anyway: %+v", f.runRequests)
	}
}

// Resume works on both runtimes (the standard one journals explicitly keyed
// steps), so the only thing missing can be a failed run.
func TestTestResumeOnADefaultRuntimeFolderRefusesOnlyForWantOfAFailedRun(t *testing.T) {
	f := newFakeInstance(t)
	signIn(t, f)
	withFastPolling(t)
	root := draftFolder(t, f, nil)

	_, err := runCLI(t, root, "wf", "test", "--resume")
	if err == nil {
		t.Fatal("--resume with no failed run exited zero")
	}
	if !strings.Contains(err.Error(), "no failed run") {
		t.Errorf("error does not say what is missing: %v", err)
	}
	if strings.Contains(err.Error(), "--runtime 2") {
		t.Errorf("the standard runtime was treated as the reason a resume is impossible: %v", err)
	}
	if len(f.runRequests) != 0 {
		t.Errorf("a refused resume started a run anyway: %+v", f.runRequests)
	}
}

// Refused rather than dropped: discarding them would hide the mistake the
// server's rule exists to catch.
func TestTestResumeRefusesParameters(t *testing.T) {
	f := newFakeInstance(t)
	base := time.Date(2026, 8, 13, 9, 0, 0, 0, time.UTC)
	f.runHistory["draft-1"] = []api.WorkflowRun{historicRun("run-failed", api.RunStatusError, base)}
	signIn(t, f)
	withFastPolling(t)
	root := durableFolder(t, f, func(draft *api.Workflow) {
		draft.Parameters = []api.WorkflowParameter{{Name: "month", Type: "string"}}
	})

	_, err := runCLI(t, root, "wf", "test", "--resume", "--param", "month=2026-07")
	if err == nil {
		t.Fatal("--resume --param exited zero")
	}
	if !strings.Contains(err.Error(), "--param") {
		t.Errorf("error does not name the flag: %v", err)
	}
	if len(f.runRequests) != 0 {
		t.Errorf("a refused resume started a run anyway: %+v", f.runRequests)
	}
}

// The count is the lineage's journalEntries, not len(steps): the run below has
// one observable step and three journaled ones.
func TestTestFailedDurableRunPrintsTheResumeInvocationAndTheJournalSize(t *testing.T) {
	f := newFakeInstance(t)
	failed := finishedRun(api.RunStatusError, api.RunHealthFailed)
	failed.JournalEntries = 3
	failed.Steps = []api.StepDTO{{ID: "span-1", Name: "Load orders", Status: "done"}}
	f.runScript = []api.RunResponse{failed}
	signIn(t, f)
	withFastPolling(t)
	root := durableFolder(t, f, nil)

	out, err := runCLI(t, root, "wf", "test")
	if err == nil {
		t.Fatal("a failed run exited zero")
	}
	if !strings.Contains(out, "ronja wf test --resume") {
		t.Errorf("a failed durable run does not offer the resume:\n%s", out)
	}
	if !strings.Contains(out, "3 steps journaled — a resume skips them") {
		t.Errorf("the journal size is missing or is not journalEntries:\n%s", out)
	}
}

// The resume hint's "fix the code" advice must not follow a run whose code
// never ran. Accepted edge: a durable run from before migration 000389 has no
// exec_run and no journal, so it prints this notice too.
func TestTestSaysNothingRanWhenTheFailureWasSetup(t *testing.T) {
	f := newFakeInstance(t)
	failed := finishedRun(api.RunStatusError, api.RunHealthFailed)
	failed.Logs = ""
	failed.Error = ptr("get S3 credentials: assume role: AccessDenied\n  at the presign step")
	f.runScript = []api.RunResponse{failed}
	signIn(t, f)
	withFastPolling(t)
	root := durableFolder(t, f, nil)

	out, err := runCLI(t, root, "wf", "test")
	if err == nil {
		t.Fatal("a failed run exited zero")
	}
	if strings.Contains(out, "--resume") {
		t.Errorf("a setup failure offered a resume, which is advice about files nothing read:\n%s", out)
	}
	if !strings.Contains(out, "Nothing in your code ran") {
		t.Errorf("expected the report to say the code never ran:\n%s", out)
	}
	// Where, never whose fault: see printNothingRanNotice.
	if strings.Contains(out, "not a bug in your files") {
		t.Errorf("the notice claimed the author's files are innocent, which it cannot know:\n%s", out)
	}
	// The first line only; the Error: line carries the whole message.
	if !strings.Contains(out, "(get S3 credentials: assume role: AccessDenied).") {
		t.Errorf("expected the first line of the error in the notice:\n%s", out)
	}
	if !strings.Contains(out, "at the presign step") {
		t.Errorf("the Error: line must still carry the full message:\n%s", out)
	}
}

// The pre-execution failure that is the author's: a refused package list looks
// like any setup failure to userCodeRan, and a re-run fails identically.
func TestTestNamesPipPackagesWhenTheSetupFailureWasTheFolder(t *testing.T) {
	f := newFakeInstance(t)
	failed := finishedRun(api.RunStatusError, api.RunHealthFailed)
	failed.Logs = ""
	failed.Error = ptr("invalid pip packages: pandas==NOPE is not a valid requirement")
	f.runScript = []api.RunResponse{failed}
	signIn(t, f)
	withFastPolling(t)
	root := durableFolder(t, f, nil)

	out, err := runCLI(t, root, "wf", "test")
	if err == nil {
		t.Fatal("a failed run exited zero")
	}
	if !strings.Contains(out, "Nothing in your code ran") {
		t.Errorf("expected the report to say the code never ran:\n%s", out)
	}
	if !strings.Contains(out, "This one is about the folder's pipPackages — check them in ronja.json.") {
		t.Errorf("an author-caused setup failure did not name the file that caused it:\n%s", out)
	}
	if strings.Contains(out, "not a bug in your files") {
		t.Errorf("the notice absolved the very file that failed the run:\n%s", out)
	}
}

// The check-in is best-effort, so a run that executed can arrive unstamped.
func TestTestStillOffersTheResumeWhenSomethingProvesTheCodeRan(t *testing.T) {
	for _, tc := range []struct {
		name string
		with func(*api.RunResponse)
	}{
		{"a processing stamp", func(r *api.RunResponse) { r.ProcessingStartedAt = ptr(time.Now()) }},
		{"captured logs", func(r *api.RunResponse) { r.Logs = "starting\n" }},
		{"a Python traceback", func(r *api.RunResponse) {
			r.Error = ptr("Traceback (most recent call last):\n  File \"main.py\", line 3\nKeyError: 'x'")
		}},
		{"a step", func(r *api.RunResponse) {
			r.Steps = []api.StepDTO{{ID: "span-1", Name: "Load orders", Status: "failed"}}
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newFakeInstance(t)
			failed := finishedRun(api.RunStatusError, api.RunHealthFailed)
			failed.Logs = ""
			failed.Error = ptr("boom")
			tc.with(&failed)
			f.runScript = []api.RunResponse{failed}
			signIn(t, f)
			withFastPolling(t)
			root := durableFolder(t, f, nil)

			out, err := runCLI(t, root, "wf", "test")
			if err == nil {
				t.Fatal("a failed run exited zero")
			}
			if !strings.Contains(out, "ronja wf test --resume") {
				t.Errorf("%s proves the code ran, so the resume must still be offered:\n%s", tc.name, out)
			}
			if strings.Contains(out, "Nothing in your code ran") {
				t.Errorf("%s proves the code ran:\n%s", tc.name, out)
			}
		})
	}
}

// journalEntries counts the resume lineage, so a resumed run inherits it; it
// says nothing about whether this run executed.
func TestTestSaysNothingRanWhenOnlyTheLineageHasAJournal(t *testing.T) {
	f := newFakeInstance(t)
	failed := finishedRun(api.RunStatusError, api.RunHealthFailed)
	failed.Logs = ""
	failed.ProcessingStartedAt = nil
	failed.Steps = nil
	failed.JournalEntries = 2
	failed.Error = ptr("failed to assume role for object storage")
	f.runScript = []api.RunResponse{failed}
	signIn(t, f)
	withFastPolling(t)
	root := durableFolder(t, f, nil)

	out, err := runCLI(t, root, "wf", "test")
	if err == nil {
		t.Fatal("a failed run exited zero")
	}
	if !strings.Contains(out, "Nothing in your code ran") {
		t.Errorf("an inherited journal count suppressed the notice on a run that never started:\n%s", out)
	}
}

func TestTestFailedRunUsesTheManifestWhenTheRowSaysNothing(t *testing.T) {
	f := newFakeInstance(t)
	f.runScript = []api.RunResponse{finishedRun(api.RunStatusError, api.RunHealthFailed)}
	signIn(t, f)
	withFastPolling(t)
	root := draftFolder(t, f, nil)
	declareDurableInManifest(t, root)

	out, err := runCLI(t, root, "wf", "test")
	if err == nil {
		t.Fatal("a failed run exited zero")
	}
	if !strings.Contains(out, "ronja wf test --resume") {
		t.Errorf("the manifest's declaration was ignored:\n%s", out)
	}
	// "0 steps journaled" would read as a fact rather than an absent field.
	if strings.Contains(out, "journaled") {
		t.Errorf("a count was printed with no journalEntries on the response:\n%s", out)
	}
}

// A v1 run journals no steps, so a resume would re-run all of them.
func TestTestFailedDefaultRuntimeRunOffersNoResume(t *testing.T) {
	f := newFakeInstance(t)
	f.runScript = []api.RunResponse{finishedRun(api.RunStatusError, api.RunHealthFailed)}
	signIn(t, f)
	withFastPolling(t)
	root := draftFolder(t, f, nil)

	out, err := runCLI(t, root, "wf", "test")
	if err == nil {
		t.Fatal("a failed run exited zero")
	}
	if strings.Contains(out, "--resume") {
		t.Errorf("a v1 failure offered a resume:\n%s", out)
	}
}

// The notice is about the run, so the runtime does not matter.
func TestTestSaysNothingRanOnADefaultRuntimeRunToo(t *testing.T) {
	f := newFakeInstance(t)
	failed := finishedRun(api.RunStatusError, api.RunHealthFailed)
	failed.Logs = ""
	failed.Error = ptr("start container: image pull failed")
	f.runScript = []api.RunResponse{failed}
	signIn(t, f)
	withFastPolling(t)
	root := draftFolder(t, f, nil)

	out, err := runCLI(t, root, "wf", "test")
	if err == nil {
		t.Fatal("a failed run exited zero")
	}
	if !strings.Contains(out, "Nothing in your code ran") {
		t.Errorf("a v1 setup failure did not say the code never ran:\n%s", out)
	}
	if !strings.Contains(out, "Run `ronja wf test` again") {
		t.Errorf("the notice did not name the command that produced this run:\n%s", out)
	}
	if strings.Contains(out, "--resume") {
		t.Errorf("a v1 failure offered a resume:\n%s", out)
	}
}

// --- a refused run POST -----------------------------------------------------

// The credit kill-stop arrives as a 400 on the run POST, and its sentence must
// reach the caller unparaphrased.
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
	if !strings.Contains(out, "ZeroDivisionError") {
		t.Errorf("report did not carry the run's error:\n%s", out)
	}
}

// A waiting run is not a failure and may wait for days, so the poll ends there
// and exits zero; non-zero would stop a nightly pipeline over a healthy run.
func TestTestParkedRunExitsZeroAndReportsTheWait(t *testing.T) {
	f := newFakeInstance(t)
	parked := finishedRun(api.RunStatusWaiting, api.RunHealthWaiting)
	parked.Steps = []api.StepDTO{{ID: "s1", Name: "Ask the agent", Status: "done", DurationMs: ptr(int64(900))}}
	f.runScript = []api.RunResponse{parked}
	signIn(t, f)
	withFastPolling(t)
	root := durableFolder(t, f, nil)

	out, err := runCLI(t, root, "wf", "test")
	if err != nil {
		t.Fatalf("a parked run exited non-zero: %v", err)
	}
	for _, want := range []string{"waiting — nothing failed, and it resumes on its own", "It resumes on its own", "run-1"} {
		if !strings.Contains(out, want) {
			t.Errorf("report missing %q:\n%s", want, out)
		}
	}
	if !strings.Contains(out, "Ask the agent") {
		t.Errorf("report dropped the steps that ran before the park:\n%s", out)
	}
	for _, forbidden := range []string{"wf publish", "--resume"} {
		if strings.Contains(out, forbidden) {
			t.Errorf("a parked run was offered %q:\n%s", forbidden, out)
		}
	}
	if f.runPolls != 1 {
		t.Errorf("polls = %d, want exactly one — the park was waited out", f.runPolls)
	}
}

// The wake latch: `resuming` is terminal for the polled row but not a failure,
// and the run id in the report is no longer the one to follow.
func TestTestResumingRunExitsZeroAndSaysItContinuedElsewhere(t *testing.T) {
	f := newFakeInstance(t)
	f.runScript = []api.RunResponse{finishedRun(api.RunStatusResuming, api.RunHealthDone)}
	signIn(t, f)
	withFastPolling(t)
	root := durableFolder(t, f, nil)

	out, err := runCLI(t, root, "wf", "test")
	if err != nil {
		t.Fatalf("a run handed to a successor exited non-zero: %v", err)
	}
	for _, want := range []string{"resuming — continued in a newer run", "continued in a NEWER run"} {
		if !strings.Contains(out, want) {
			t.Errorf("report missing %q:\n%s", want, out)
		}
	}
	if strings.Contains(out, "wf publish") {
		t.Errorf("a handed-on run was offered publish:\n%s", out)
	}
}

// An unknown status ends the poll (api.RunResponse.Running) but must not exit
// zero: a nightly pipeline would believe a green "cancelled".
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
	if !strings.Contains(err.Error(), "cancelled") {
		t.Errorf("error does not name the status: %v", err)
	}
	if !strings.Contains(out, "cancelled") {
		t.Errorf("report did not carry the status:\n%s", out)
	}
	if f.runPolls != 1 {
		t.Errorf("polls = %d, want exactly one", f.runPolls)
	}
}

// Ctrl-C stops the waiting, not the server-side run; the message depends on the
// root's interrupt handler (signalContext).
func TestTestCtrlCStopsWaitingAndSaysTheRunSurvives(t *testing.T) {
	f := newFakeInstance(t)
	f.runScript = []api.RunResponse{runningRun()}
	var once sync.Once
	polled := make(chan struct{})
	f.onPoll = func() { once.Do(func() { close(polled) }) }
	signIn(t, f)
	withFastPolling(t)
	root := draftFolder(t, f, nil)

	go func() {
		// The root registers the handler before any request.
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

// 0 means "wait forever"; a negative duration is refused, not another spelling.
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

// --json mirrors the server's embedded *rdb.WorkflowRun, flattened at the root.
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

// --write-live is a flag, never a y/N fallback; this pins the absence of a
// prompt against a later "just ask on a TTY".
func TestTestNeverPrompts(t *testing.T) {
	f := newFakeInstance(t)
	f.runScript = []api.RunResponse{finishedRun(api.RunStatusDone, api.RunHealthDone)}
	f.tableNames["tbl-1"] = "Monthly revenue"
	signIn(t, f)
	withFastPolling(t)
	root := draftFolder(t, f, func(draft *api.Workflow) {
		draft.OutputTableIDs = []string{"tbl-1"}
	})

	out, err := runCLI(t, root, "wf", "test")
	if err == nil {
		t.Fatal("ran without --write-live")
	}
	for _, text := range []string{out, err.Error()} {
		if strings.Contains(strings.ToLower(text), "[y/n]") || strings.Contains(text, "Continue?") {
			t.Errorf("a prompt appeared:\n%s", text)
		}
	}
	out, err = runCLI(t, root, "wf", "test", "--write-live")
	if err != nil {
		t.Fatalf("test: %v", err)
	}
	if strings.Contains(strings.ToLower(out), "[y/n]") {
		t.Errorf("a prompt appeared:\n%s", out)
	}
}

// --- unit: number coercion --------------------------------------------------

// A whole number crosses the wire as an integer: through a float64 a row id or
// epoch-nanosecond timestamp would arrive silently rounded. The marshalled form
// is asserted too.
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

	if got, err := coerceParam(p, "12.5"); err != nil || got != 12.5 {
		t.Errorf("coerce 12.5 = %#v, %v", got, err)
	}
	if got, err := coerceParam(p, "-7"); err != nil || got != int64(-7) {
		t.Errorf("coerce -7 = %#v, %v", got, err)
	}
	if _, err := coerceParam(p, "lots"); err == nil {
		t.Error("coerced a non-number")
	}
}

// --- unit: the poll schedule --------------------------------------------------

// Each poll re-reads the whole run, log and all, hence the backoff; the first
// polls stay prompt and the growth stops at a cap.
func TestPollIntervalBacksOffButStartsPrompt(t *testing.T) {
	interval := runPollInterval
	var elapsed time.Duration
	for poll := 1; poll <= pollsAtStartInterval; poll++ {
		if interval != runPollInterval {
			t.Fatalf("wait %d = %s, want the starting %s", poll, interval, runPollInterval)
		}
		elapsed += interval
		interval = nextPollInterval(interval, poll)
	}
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
	// Roughly a minute, so a retune of one constant cannot drift it unnoticed.
	if elapsed < 30*time.Second || elapsed > 90*time.Second {
		t.Errorf("reached the cap after %s of polling, want roughly a minute", elapsed)
	}
}

// --- --follow ---------------------------------------------------------------

// The named run's own route is never consulted: it would sit at `resuming`
// forever. The script walks the whole wake path because `resuming` must not be
// terminal here, and ReleaseWakeClaim can roll a run back to `waiting` under
// the same id, so a second park must narrate too.
func TestTestFollowCrossesAParkAndReportsTheSuccessor(t *testing.T) {
	f := newFakeInstance(t)
	f.headScript = []api.RunResponse{
		{WorkflowRun: api.WorkflowRun{ID: "run-1", Status: api.RunStatusRunning}, Health: api.RunHealthDone,
			Steps: []api.StepDTO{{ID: "s1", Name: "Ask the agent", Status: "running"}}},
		{WorkflowRun: api.WorkflowRun{ID: "run-1", Status: api.RunStatusWaiting}, Health: api.RunHealthWaiting,
			Steps: []api.StepDTO{{ID: "s1", Name: "Ask the agent", Status: "done", DurationMs: ptr(int64(900))}}},
		{WorkflowRun: api.WorkflowRun{ID: "run-1", Status: api.RunStatusResuming}, Health: api.RunHealthWaiting,
			Steps: []api.StepDTO{{ID: "s1", Name: "Ask the agent", Status: "done", DurationMs: ptr(int64(900))}}},
		// The wake claim was released: waiting again under the same id.
		{WorkflowRun: api.WorkflowRun{ID: "run-1", Status: api.RunStatusWaiting}, Health: api.RunHealthWaiting,
			Steps: []api.StepDTO{{ID: "s1", Name: "Ask the agent", Status: "done", DurationMs: ptr(int64(900))}}},
		{WorkflowRun: api.WorkflowRun{ID: "run-1", Status: api.RunStatusResuming}, Health: api.RunHealthWaiting,
			Steps: []api.StepDTO{{ID: "s1", Name: "Ask the agent", Status: "done", DurationMs: ptr(int64(900))}}},
		{WorkflowRun: api.WorkflowRun{ID: "run-2", Status: api.RunStatusRunning, ResumeOfRunID: ptr("run-1")}, Health: api.RunHealthDone,
			Steps: []api.StepDTO{
				{ID: "s9", Name: "Ask the agent", Status: "skipped", Replayed: true},
				{ID: "s10", Name: "Write report", Status: "running"},
			}},
		{WorkflowRun: api.WorkflowRun{ID: "run-2", Status: api.RunStatusDone, ResumeOfRunID: ptr("run-1"), Logs: "all good\n"}, Health: api.RunHealthDone,
			Steps: []api.StepDTO{
				{ID: "s9", Name: "Ask the agent", Status: "skipped", Replayed: true},
				{ID: "s10", Name: "Write report", Status: "done", DurationMs: ptr(int64(300))},
			}},
	}
	signIn(t, f)
	withFastPolling(t)
	root := durableFolder(t, f, nil)

	var out string
	var err error
	stderr := captureStderr(t, func() {
		out, err = runCLI(t, root, "wf", "test", "--follow")
	})
	if err != nil {
		t.Fatalf("a followed run that finished exited non-zero: %v", err)
	}
	// Sticky per-run flags would go silent through the retry's second park.
	for _, want := range []string{
		"parked — waiting on an agent",
		"Ctrl-C stops watching, not the run",
		"is waking",
		"parked again — still watching",
		"Resumed as run run-2",
	} {
		if !strings.Contains(stderr, want) {
			t.Errorf("narration missing %q:\n%s", want, stderr)
		}
	}
	if got := strings.Count(stderr, "Ctrl-C stops watching, not the run"); got != 1 {
		t.Errorf("how-to-wait advice printed %d times, want once per follow:\n%s", got, stderr)
	}
	// The run we started is not a hop.
	if strings.Contains(stderr, "Resumed as run run-1") {
		t.Errorf("the run we started was announced as a resume of itself:\n%s", stderr)
	}
	if !strings.Contains(stderr, "replayed  Ask the agent") {
		t.Errorf("a journaled step was narrated as a bare skip:\n%s", stderr)
	}
	if !strings.Contains(out, "run-2") {
		t.Errorf("report named the parked run rather than the head:\n%s", out)
	}
	if f.runPolls != 0 {
		t.Errorf("polled the named run %d times under --follow", f.runPolls)
	}
	if f.headPolls < 7 {
		t.Errorf("head polls = %d, want at least 7 (it stopped before the successor finished)", f.headPolls)
	}
}

// Park and resume can both fall inside one poll interval, which is why the
// follow seeds its head with the run it started.
func TestTestFollowNarratesAResumeItLearnsOnTheFirstPoll(t *testing.T) {
	f := newFakeInstance(t)
	f.headScript = []api.RunResponse{
		{WorkflowRun: api.WorkflowRun{ID: "run-2", Status: api.RunStatusDone, ResumeOfRunID: ptr("run-1")}, Health: api.RunHealthDone},
	}
	signIn(t, f)
	withFastPolling(t)
	root := durableFolder(t, f, nil)

	var err error
	stderr := captureStderr(t, func() {
		_, err = runCLI(t, root, "wf", "test", "--follow")
	})
	if err != nil {
		t.Fatalf("a followed run that finished exited non-zero: %v", err)
	}
	if !strings.Contains(stderr, "Resumed as run run-2") {
		t.Errorf("a resume seen on the first poll was silent:\n%s", stderr)
	}
}

func TestTestFollowExitsNonZeroWhenTheHeadFails(t *testing.T) {
	f := newFakeInstance(t)
	f.headScript = []api.RunResponse{
		{WorkflowRun: api.WorkflowRun{ID: "run-1", Status: api.RunStatusWaiting}, Health: api.RunHealthWaiting},
		{WorkflowRun: api.WorkflowRun{ID: "run-2", Status: api.RunStatusError, ResumeOfRunID: ptr("run-1"),
			Error: ptr("ZeroDivisionError: division by zero")}, Health: api.RunHealthFailed},
	}
	signIn(t, f)
	withFastPolling(t)
	root := durableFolder(t, f, nil)

	var out string
	var err error
	captureStderr(t, func() {
		out, err = runCLI(t, root, "wf", "test", "--follow")
	})
	if err == nil {
		t.Fatal("a followed run that failed exited zero")
	}
	if !strings.Contains(out, "ZeroDivisionError") {
		t.Errorf("report did not carry the head run's error:\n%s", out)
	}
}

// Falls back, never refuses: the run has already started, and "run it again"
// would invite a second live run. Three polls so "the note prints once per
// follow" is falsifiable, since the fallback is decided per poll.
func TestTestFollowFallsBackOnAFleetWithoutTheHeadRoute(t *testing.T) {
	f := newFakeInstance(t)
	f.noHeadRoute = true
	f.runScript = []api.RunResponse{runningRun(), runningRun(), finishedRun(api.RunStatusWaiting, api.RunHealthWaiting)}
	signIn(t, f)
	withFastPolling(t)
	root := durableFolder(t, f, nil)

	var out string
	var err error
	stderr := captureStderr(t, func() {
		out, err = runCLI(t, root, "wf", "test", "--follow")
	})
	if err != nil {
		t.Fatalf("the fallback refused instead of reporting the plain route: %v", err)
	}
	if got := strings.Count(stderr, "answered 404 for the run-lineage route"); got != 1 {
		t.Errorf("fallback note printed %d times, want once per follow:\n%s", got, stderr)
	}
	if f.runPolls != 3 {
		t.Errorf("plain polls = %d, want 3 (one per poll, stopping at the park)", f.runPolls)
	}
	// Trying the head route every poll picks up a rollout that completes
	// mid-follow.
	if f.headRequests != 3 {
		t.Errorf("head requests = %d, want 3 — the fallback latched instead of retrying the route", f.headRequests)
	}
	if !strings.Contains(out, "waiting") {
		t.Errorf("report did not carry the plain poll's answer:\n%s", out)
	}
}

// Mid-rollout, the first poll can hit an old pod and later ones new pods; a
// one-shot "this fleet is old" decision would stop at the first park.
func TestTestFollowUpgradesWhenTheHeadRouteAnswersAfterAMiss(t *testing.T) {
	f := newFakeInstance(t)
	f.headScript = []api.RunResponse{
		{WorkflowRun: api.WorkflowRun{Status: headRouteMiss}},
		{WorkflowRun: api.WorkflowRun{ID: "run-1", Status: api.RunStatusWaiting}, Health: api.RunHealthWaiting},
		{WorkflowRun: api.WorkflowRun{ID: "run-2", Status: api.RunStatusDone, ResumeOfRunID: ptr("run-1")}, Health: api.RunHealthDone},
	}
	f.runScript = []api.RunResponse{runningRun(), finishedRun(api.RunStatusWaiting, api.RunHealthWaiting)}
	signIn(t, f)
	withFastPolling(t)
	root := durableFolder(t, f, nil)

	var out string
	var err error
	stderr := captureStderr(t, func() {
		out, err = runCLI(t, root, "wf", "test", "--follow")
	})
	if err != nil {
		t.Fatalf("a followed run that finished exited non-zero: %v", err)
	}
	if !strings.Contains(out, "run-2") {
		t.Errorf("the follow stopped at the fallback's park instead of following the lineage:\n%s", out)
	}
	if !strings.Contains(stderr, "Resumed as run run-2") {
		t.Errorf("narration missing the resume hop:\n%s", stderr)
	}
	if f.runPolls != 1 {
		t.Errorf("plain polls = %d, want 1 (only the poll that missed the route)", f.runPolls)
	}
	if got := strings.Count(stderr, "answered 404 for the run-lineage route"); got != 1 {
		t.Errorf("fallback note printed %d times, want once per follow:\n%s", got, stderr)
	}
}

// A 404 mid-follow is an older pod, not a failed poll, so the staged misses
// outnumber the failure budget on purpose. A park read off the plain route must
// not end the wait once the fleet has answered the head route.
func TestTestFollowSurvivesHeadRouteMissesMidFollow(t *testing.T) {
	f := newFakeInstance(t)
	misses := maxConsecutivePollFailures + 1
	script := []api.RunResponse{
		{WorkflowRun: api.WorkflowRun{ID: "run-1", Status: api.RunStatusRunning}, Health: api.RunHealthDone},
	}
	for i := 0; i < misses; i++ {
		script = append(script, api.RunResponse{WorkflowRun: api.WorkflowRun{Status: headRouteMiss}})
	}
	f.headScript = append(script, api.RunResponse{
		WorkflowRun: api.WorkflowRun{ID: "run-2", Status: api.RunStatusDone, ResumeOfRunID: ptr("run-1")},
		Health:      api.RunHealthDone,
	})
	f.runScript = []api.RunResponse{finishedRun(api.RunStatusWaiting, api.RunHealthWaiting)}
	signIn(t, f)
	withFastPolling(t)
	root := durableFolder(t, f, nil)

	var out string
	var err error
	stderr := captureStderr(t, func() {
		out, err = runCLI(t, root, "wf", "test", "--follow")
	})
	if err != nil {
		t.Fatalf("%d route misses ended the follow: %v", misses, err)
	}
	if !strings.Contains(out, "run-2") {
		t.Errorf("the follow stopped on a parked plain read rather than following through it:\n%s", out)
	}
	if f.runPolls != misses {
		t.Errorf("plain polls = %d, want %d (one per missed poll, and none after)", f.runPolls, misses)
	}
	if got := strings.Count(stderr, "answered 404 for the run-lineage route"); got != 1 {
		t.Errorf("fallback note printed %d times, want once per follow:\n%s", got, stderr)
	}
}

// A run id matching nothing is a 400 from a new instance, not an old-instance
// signal.
func TestTestFollowTreatsAMissingRunAsAFailedPollNotAnOldInstance(t *testing.T) {
	f := newFakeInstance(t)
	f.failHeadGets = http.StatusBadRequest
	signIn(t, f)
	withFastPolling(t)
	root := durableFolder(t, f, nil)

	var err error
	stderr := captureStderr(t, func() {
		_, err = runCLI(t, root, "wf", "test", "--follow")
	})
	if err == nil {
		t.Fatal("a head route answering 400 forever exited zero")
	}
	if !strings.Contains(err.Error(), "polls in a row failed") {
		t.Errorf("a 400 did not land on the failure budget: %v", err)
	}
	if strings.Contains(stderr, "answered 404 for the run-lineage route") {
		t.Errorf("a missing run was mistaken for an old instance:\n%s", stderr)
	}
	if f.runPolls != 0 {
		t.Errorf("a 400 sent the follow down the old-instance fallback (%d plain polls)", f.runPolls)
	}
}

// Said up front, because by the deadline the advice costs another whole run.
func TestTestFollowNotesTheDefaultTimeoutOnlyWhenItWasNotChosen(t *testing.T) {
	for _, tc := range []struct {
		name string
		args []string
		want bool
	}{
		{"default", []string{"wf", "test", "--follow"}, true},
		{"chosen", []string{"wf", "test", "--follow", "--timeout", "1h"}, false},
		{"forever", []string{"wf", "test", "--follow", "--timeout", "0"}, false},
		{"no follow", []string{"wf", "test"}, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newFakeInstance(t)
			f.headScript = []api.RunResponse{finishedRun(api.RunStatusDone, api.RunHealthDone)}
			f.runScript = []api.RunResponse{finishedRun(api.RunStatusDone, api.RunHealthDone)}
			signIn(t, f)
			withFastPolling(t)
			root := durableFolder(t, f, nil)

			var err error
			stderr := captureStderr(t, func() {
				_, err = runCLI(t, root, tc.args...)
			})
			if err != nil {
				t.Fatalf("%v: %v", tc.args, err)
			}
			if got := strings.Contains(stderr, "A park can outlast it"); got != tc.want {
				t.Errorf("default-timeout note present = %v, want %v:\n%s", got, tc.want, stderr)
			}
		})
	}
}

func TestTestWithoutFollowNeverAsksForTheLineageHead(t *testing.T) {
	f := newFakeInstance(t)
	f.runScript = []api.RunResponse{finishedRun(api.RunStatusDone, api.RunHealthDone)}
	signIn(t, f)
	withFastPolling(t)
	root := durableFolder(t, f, nil)

	if _, err := runCLI(t, root, "wf", "test"); err != nil {
		t.Fatalf("test: %v", err)
	}
	if f.headPolls != 0 {
		t.Errorf("a plain `wf test` polled the head route %d times", f.headPolls)
	}
}

func TestRunFollowCrossesAParkAndReportsTheSuccessor(t *testing.T) {
	f := newFakeInstance(t)
	f.headScript = []api.RunResponse{
		{WorkflowRun: api.WorkflowRun{ID: "run-1", Status: api.RunStatusWaiting}, Health: api.RunHealthWaiting},
		{WorkflowRun: api.WorkflowRun{ID: "run-2", Status: api.RunStatusDone, ResumeOfRunID: ptr("run-1")}, Health: api.RunHealthDone},
	}
	signIn(t, f)
	withFastPolling(t)
	root := liveFolder(t, f, nil)

	var out string
	var err error
	stderr := captureStderr(t, func() {
		out, err = runCLI(t, root, "wf", "run", "--follow")
	})
	if err != nil {
		t.Fatalf("a followed live run that finished exited non-zero: %v", err)
	}
	if !strings.Contains(stderr, "Resumed as run run-2") {
		t.Errorf("narration missing the resume hop:\n%s", stderr)
	}
	if !strings.Contains(out, "run-2") {
		t.Errorf("report named the parked run rather than the head:\n%s", out)
	}
}

// --- unit: the step narrator ------------------------------------------------

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

// captureStderr uses a temp file rather than a pipe, as captureStdout does.
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
