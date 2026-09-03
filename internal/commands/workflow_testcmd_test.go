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

// --- resume -----------------------------------------------------------------

// durableFolder is draftFolder with the ROW declaring the durable runtime,
// which is where the CLI reads it from: the row is what the run funnel reads,
// and it is right about a workflow this folder did not create.
func durableFolder(t *testing.T, f *fakeInstance, shape func(*api.Workflow)) string {
	t.Helper()
	return draftFolder(t, f, func(draft *api.Workflow) {
		draft.RuntimeVersion = wfdir.RuntimeDurable
		if shape != nil {
			shape(draft)
		}
	})
}

// declareDurableInManifest is the FALLBACK path: an instance that predates
// durable workflows sends no runtimeVersion, so the row decodes as 0 and the
// folder's own declaration is all there is to go on. A 0 means "the instance
// did not say", not "runtime 1".
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

// historicRun is one entry of a workflow's run history.
func historicRun(id, status string, executedAt time.Time) api.WorkflowRun {
	return api.WorkflowRun{ID: id, WorkflowID: "draft-1", Status: status, ExecutedAt: executedAt}
}

// The two halves of a resume's contract, in one test: it names the most recent
// FAILED run, and it sends no parameter values at all — the server refuses a
// resume that carries any, because a changed parameter invalidates every step
// result computed under the old ones.
//
// The history is staged newest-LAST and out of order, because the endpoint has
// no default ordering: a CLI that took the first row would pick a done run here
// and a random one in production.
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
	// A replayed step reads as replayed rather than as "skipped", which is what
	// a continue_on_error step that never ran also says.
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
	// The ordering is REQUESTED, not hoped for. Without it the limit above
	// returns an arbitrary window of the history.
	if len(f.runQueries) != 1 || !strings.Contains(f.runQueries[0], "orderBy=executed_at+desc") {
		t.Errorf("run history query = %v, want an explicit newest-first ordering", f.runQueries)
	}
}

// A resume with nothing to resume must say so. Starting a fresh run instead
// would be the one outcome the flag exists to avoid — a full re-run of work
// somebody asked to continue.
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

// A standard-runtime folder is NOT refused for being standard-runtime. Resume
// works on both runtimes — on the standard one a step is journaled when the
// author writes an explicit key — so the only thing that can be missing is a
// failed run, and that is what the refusal must say. Refusing on the runtime
// would tell an author who did write keys that their resume is impossible.
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

// --param with --resume is refused rather than dropped: the server rejects
// parameter values on a resume, and a CLI that silently discarded them would
// hide exactly the mistake that rule exists to catch.
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

// A failed run of a durable workflow ends with the invocation that continues it
// and the size of what a resume would skip.
//
// The count is the response's journalEntries — the LINEAGE's journal — and NOT
// len(steps): the run below has one observable step and three journaled ones,
// so a report that counted steps would print a confident wrong number in the
// one place somebody is deciding whether to trust the skip.
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

// TestTestSaysNothingRanWhenTheFailureWasSetup is the other half of the resume
// hint: it must NOT be offered when the workflow's code never ran.
//
// "Fix the code and push first if the failure was a bug" is advice about the
// author's files. A run that died fetching S3 credentials failed before the
// container had them — the Python harness stamps processing_started_at
// immediately before it execs the code, and compiles the entrypoint inside that
// exec — so nothing the author wrote was ever evaluated, and telling them to fix
// their code is an outage rendered as bad authorship.
//
// Accepted edge, pinned here rather than left to be rediscovered: a durable run
// from before migration 000389 has no exec_run (steps [], NULL stamp) and no
// journal, so it prints this message too. That is the cost of reading the only
// signal that exists, and it errs toward "re-run it" rather than toward blaming
// files nobody looked at.
func TestTestSaysNothingRanWhenTheFailureWasSetup(t *testing.T) {
	f := newFakeInstance(t)
	failed := finishedRun(api.RunStatusError, api.RunHealthFailed)
	// No stamp, no steps, no journal, no captured output: the container never
	// got as far as producing any of them.
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
	// It reports where the run died, not whose fault that is — see
	// printNothingRanNotice. Some pre-execution failures really are the author's.
	if strings.Contains(out, "not a bug in your files") {
		t.Errorf("the notice claimed the author's files are innocent, which it cannot know:\n%s", out)
	}
	// The first line only, in the notice — the Error: line above already carries
	// the whole message, and repeating it would bury the sentence that matters.
	if !strings.Contains(out, "(get S3 credentials: assume role: AccessDenied).") {
		t.Errorf("expected the first line of the error in the notice:\n%s", out)
	}
	if !strings.Contains(out, "at the presign step") {
		t.Errorf("the Error: line must still carry the full message:\n%s", out)
	}
}

// The pre-execution failure that IS the author's, and the reason the notice no
// longer says "that is not a bug in your files".
//
// A package list the folder's ronja.json declares is validated before ExecV2 is
// ever called (manalysis fails the run with "invalid pip packages: …"), so the
// run arrives here with nothing executed and every signal userCodeRan reads
// absent — indistinguishable, to this report, from a container that could not
// get its credentials. The generic "re-run it" is useless advice for a list that
// will be refused identically every time, so this one failure gets a line naming
// the file that holds it.
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

// The three signals that outvote a missing stamp. The check-in is best-effort —
// a background worker that gives up on a network failure — so a run that really
// did execute can arrive unstamped, and concluding "nothing ran" there would
// hide a genuine bug from its author.
//
// A journal count is NOT among them — see the test below this one.
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

// A journal count alone does NOT prove that THIS run executed the author's
// code, and must not suppress the notice.
//
// journalEntries counts what the resume LINEAGE has journaled and is derived on
// read, so a `wf test --resume` inherits a non-zero count from the runs before
// it. The failure this notice exists for — the container dying before it
// starts, on an S3 credential or a launch error — then arrives with no stamp,
// no steps and no logs, and treating the inherited count as evidence handed the
// author a bare infrastructure error on the one run that needed the notice
// most. Every other signal here is a fact about the run in hand; this one is a
// fact about its ancestors.
func TestTestSaysNothingRanWhenOnlyTheLineageHasAJournal(t *testing.T) {
	f := newFakeInstance(t)
	failed := finishedRun(api.RunStatusError, api.RunHealthFailed)
	failed.Logs = ""
	failed.ProcessingStartedAt = nil
	failed.Steps = nil
	// Inherited from earlier runs of the same lineage, not produced by this one.
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

// The same hint on a folder whose INSTANCE never sends a runtimeVersion: the
// row decodes as 0, and the folder's own declaration is the fallback.
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
	// No count: the instance sent none, and "0 steps journaled" would read as a
	// fact about the journal rather than as an absent field.
	if strings.Contains(out, "journaled") {
		t.Errorf("a count was printed with no journalEntries on the response:\n%s", out)
	}
}

// The same failure on a v1 folder offers nothing, because there is nothing to
// offer: a v1 run journals no steps, and a resume of it would re-run all of them.
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

// A v1 run that never reached the interpreter says so too. The notice is about
// the RUN, not about what a resume could skip, so the runtime it ran on has
// nothing to do with whether it is true — and a v1 author reading a bare
// "AccessDenied" has exactly the same reason to go looking through their own
// files for a bug that is not there.
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
	// Still no resume: v1 journals nothing, so there is nothing to skip.
	if strings.Contains(out, "--resume") {
		t.Errorf("a v1 failure offered a resume:\n%s", out)
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

// A durable run that PARKS is not a failure: the burst finished cleanly, its
// outputs are persisted, and it resumes on its own. So the poll ends there (a
// park lasts until an agent answers or a timer fires — days, possibly) and the
// command exits ZERO, because a non-zero exit would stop a nightly pipeline
// over a workflow doing exactly what it was written to do.
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
	// The status is named as a park rather than left as a bare "waiting", which
	// reads as stuck...
	for _, want := range []string{"waiting — nothing failed, and it resumes on its own", "It resumes on its own", "run-1"} {
		if !strings.Contains(out, want) {
			t.Errorf("report missing %q:\n%s", want, out)
		}
	}
	// ...and what DID run is still reported: a park is a partial answer, not no
	// answer.
	if !strings.Contains(out, "Ask the agent") {
		t.Errorf("report dropped the steps that ran before the park:\n%s", out)
	}
	// Neither hint applies. Publish would take code live on a run that has not
	// finished; resume would offer to continue a run nothing has stopped.
	for _, forbidden := range []string{"wf publish", "--resume"} {
		if strings.Contains(out, forbidden) {
			t.Errorf("a parked run was offered %q:\n%s", forbidden, out)
		}
	}
	// The poll ENDED on it rather than sitting the park out.
	if f.runPolls != 1 {
		t.Errorf("polls = %d, want exactly one — the park was waited out", f.runPolls)
	}
}

// The wake latch: a poll can land on `resuming` between "a wake decided to
// resume this run" and "the resume run exists". Terminal for the row we polled
// and not a failure — the work is continuing — so it exits zero and says where
// the work went, because the run id in the report is no longer the one to
// follow.
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

// Exit zero means the run did not FAIL, and nothing wider. A status this CLI
// does not know — one added server-side after it shipped — ends the poll (see
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
// that moment — and it only gets said because the ROOT installs an interrupt
// handler (signalContext) and hands every command a context that carries it.
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
		// A served poll proves the handler is installed: the root registers it
		// before the command makes any request at all.
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

// --- --follow ---------------------------------------------------------------

// The whole point of the flag: a Durable run parks, a successor run picks the
// work up, and the follow reports the SUCCESSOR's terminal status rather than
// stopping at the park. The named run's own route is never consulted — it would
// sit at `resuming` forever.
//
// The script walks the whole wake path rather than jumping park → successor,
// because two of its states are only reachable there. `resuming` must not be
// terminal — a follow that treated it as one would stop one poll before the
// work resumed — and the server's wake is allowed to OSCILLATE: ReleaseWakeClaim
// rolls a run from `resuming` back to `waiting` under the same id when the wake
// has to be retried, so the second park has to narrate too.
func TestTestFollowCrossesAParkAndReportsTheSuccessor(t *testing.T) {
	f := newFakeInstance(t)
	f.headScript = []api.RunResponse{
		{WorkflowRun: api.WorkflowRun{ID: "run-1", Status: api.RunStatusRunning}, Health: api.RunHealthDone,
			Steps: []api.StepDTO{{ID: "s1", Name: "Ask the agent", Status: "running"}}},
		{WorkflowRun: api.WorkflowRun{ID: "run-1", Status: api.RunStatusWaiting}, Health: api.RunHealthWaiting,
			Steps: []api.StepDTO{{ID: "s1", Name: "Ask the agent", Status: "done", DurationMs: ptr(int64(900))}}},
		{WorkflowRun: api.WorkflowRun{ID: "run-1", Status: api.RunStatusResuming}, Health: api.RunHealthWaiting,
			Steps: []api.StepDTO{{ID: "s1", Name: "Ask the agent", Status: "done", DurationMs: ptr(int64(900))}}},
		// The rollback: the wake claim was released, and the run is parked again
		// under the same id waiting for the next attempt.
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
	// The park was narrated with the two things a reader needs at that moment:
	// what it is waiting on, and that Ctrl-C does not stop the run. The wake and
	// the SECOND park are narrated too: sticky per-run flags would have printed
	// each of those once and then gone silent through the retry.
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
	// The long advice is once per FOLLOW; the park line itself is once per park.
	if got := strings.Count(stderr, "Ctrl-C stops watching, not the run"); got != 1 {
		t.Errorf("how-to-wait advice printed %d times, want once per follow:\n%s", got, stderr)
	}
	// The other half of the seeded head: the run we STARTED is not a hop, so the
	// first poll — which names it — announces nothing.
	if strings.Contains(stderr, "Resumed as run run-1") {
		t.Errorf("the run we started was announced as a resume of itself:\n%s", stderr)
	}
	// A replayed step reads as replayed rather than as "skipped" — the same
	// word the report uses — so a resume's re-narration does not look like a
	// run that did nothing.
	if !strings.Contains(stderr, "replayed  Ask the agent") {
		t.Errorf("a journaled step was narrated as a bare skip:\n%s", stderr)
	}
	// The REPORT is the head run, not the run that was started.
	if !strings.Contains(out, "run-2") {
		t.Errorf("report named the parked run rather than the head:\n%s", out)
	}
	// And the plain run route was never polled: the follow asks only the head.
	if f.runPolls != 0 {
		t.Errorf("polled the named run %d times under --follow", f.runPolls)
	}
	if f.headPolls < 7 {
		t.Errorf("head polls = %d, want at least 7 (it stopped before the successor finished)", f.headPolls)
	}
}

// A park and its resume can both happen inside one poll interval, and then the
// FIRST head response already names the successor. The hop is still news, so it
// is still narrated — which is why the follow seeds its head with the run it
// started rather than learning it from the first response.
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

// The negative control: a lineage that ends in failure is still a failure.
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

// A fleet where NOTHING has the route falls back, and never refuses. The run
// has already started by the time a follow gets here, so an error whose remedy
// is "run it again" would invite a second live run — which is the failure
// adoptTimedOutRun exists to prevent. What the fallback reports is today's
// documented non-follow answer: the first park, reported as a park.
//
// Three polls rather than one, so "the note is printed once per follow" is a
// claim this script can falsify — the fallback is decided per poll now, and a
// note attached to the decision would print on every one of them.
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
	// The answer came from the plain route, and the follow stopped where a plain
	// poll stops: the park.
	if f.runPolls != 3 {
		t.Errorf("plain polls = %d, want 3 (one per poll, stopping at the park)", f.runPolls)
	}
	// Every poll still TRIED the head route first, which is what makes a
	// rollout that completes mid-follow pick itself up.
	if f.headRequests != 3 {
		t.Errorf("head requests = %d, want 3 — the fallback latched instead of retrying the route", f.headRequests)
	}
	if !strings.Contains(out, "waiting") {
		t.Errorf("report did not carry the plain poll's answer:\n%s", out)
	}
}

// Ronja runs many instances behind one load balancer, so a follow that starts
// mid-rollout can have its FIRST poll answered by an old pod and every later
// one by a new pod. A one-shot "this fleet is old" decision taken on that first
// 404 would report the first park of a fleet that follows perfectly well — and
// the park is exactly where the plain route's answer stops.
func TestTestFollowUpgradesWhenTheHeadRouteAnswersAfterAMiss(t *testing.T) {
	f := newFakeInstance(t)
	f.headScript = []api.RunResponse{
		{WorkflowRun: api.WorkflowRun{Status: headRouteMiss}},
		{WorkflowRun: api.WorkflowRun{ID: "run-1", Status: api.RunStatusWaiting}, Health: api.RunHealthWaiting},
		{WorkflowRun: api.WorkflowRun{ID: "run-2", Status: api.RunStatusDone, ResumeOfRunID: ptr("run-1")}, Health: api.RunHealthDone},
	}
	// What the old pod answers on the fallback poll, and then the park a latched
	// follow would have called the end of the run.
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
	// Only the missed poll fell back; the rest were answered by the route.
	if f.runPolls != 1 {
		t.Errorf("plain polls = %d, want 1 (only the poll that missed the route)", f.runPolls)
	}
	if got := strings.Count(stderr, "answered 404 for the run-lineage route"); got != 1 {
		t.Errorf("fallback note printed %d times, want once per follow:\n%s", got, stderr)
	}
}

// The other half of the same fleet fact: a 404 mid-follow is an OLDER POD, not
// a failed poll. A follow that never concluded anything from a 404 would spend
// its failure budget on pods that merely predate the route and abort with "lost
// contact" on a run that is perfectly healthy — so the misses staged here
// outnumber the budget on purpose, and consecutively.
//
// Meanwhile the plain route reports the run we started, parked: proof that
// FOLLOW semantics survive the fallback on a fleet that has answered the head
// route once, since a park read off the plain route must not end the wait.
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

// The other 404-shaped thing that is not one: a run id matching nothing is a
// 400 from a new instance, and must fall through the ordinary failure budget
// rather than be read as an old instance and degrade.
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

// --follow riding the DEFAULT timeout says so up front, because by the deadline
// the advice costs another whole run — and says nothing when the caller has
// already chosen a timeout.
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

// Without the flag, nothing changed: the head route is not touched at all.
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

// `wf run` carries the same flag, and it is the verb --follow was asked for: a
// live run that parks is the one somebody is about to call verified.
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
