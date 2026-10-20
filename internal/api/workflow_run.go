package api

import (
	"context"
	"encoding/json"
	"errors"
	"net/url"
	"strconv"
	"time"
)

// The RUN half of the workflow surface: start one, and poll it to completion.
// Every tag is copied from the server struct named in its comment, and
// `wf test --json` emits the decoded run verbatim, so a field not mirrored here
// silently disappears from the CLI's output.

// Workflow run statuses, mirrored from rdb.WorkflowRunStatus
// (backend/ronja/rdb/table_workflow.go). The last two appear only on a durable
// (runtime-2) run that suspended, and must be named so a parked run is not
// reported as a failure:
//
//   - waiting is PARKED: nothing failed and nothing is executing, and it can
//     last indefinitely.
//   - resuming is the wake race's single-winner latch, terminal for THIS row:
//     the parked run never executes again, and its successor carries the lineage.
const (
	RunStatusRunning  = "running"
	RunStatusDone     = "done"
	RunStatusError    = "error"
	RunStatusWaiting  = "waiting"
	RunStatusResuming = "resuming"
)

// Derived run health, mirrored from workflow.RunHealth
// (backend/api/v2/workflow/run_steps.go), computed on read. Degraded is a run
// that finished with a best-effort step failed, which a bare "done" would hide.
const (
	RunHealthDone     = "done"
	RunHealthDegraded = "degraded"
	RunHealthFailed   = "failed"
	RunHealthWaiting  = "waiting"
)

// WorkflowRun mirrors rdb.WorkflowRun — one execution of one workflow row. It
// is mirrored in full (all but TenantID), unlike Workflow, because `--json`
// prints it.
type WorkflowRun struct {
	ID              string                   `json:"id"`
	WorkflowID      string                   `json:"workflowID"`
	ParameterValues map[string]any           `json:"parameterValues"`
	Status          string                   `json:"status"`
	Outputs         []WorkflowRunOutput      `json:"outputs"`
	TableOutputs    []WorkflowRunTableOutput `json:"tableOutputs"`
	// TableOutputCount is the full count; the run list carries only the first
	// 20 TableOutputs. Read it through RunTableOutputCount, which also covers an
	// instance that predates the field.
	TableOutputCount int `json:"tableOutputCount"`
	// Logs is the run's full captured output; there is no streaming endpoint.
	Logs        string     `json:"logs"`
	Error       *string    `json:"error"`
	ExecutedBy  string     `json:"executedBy"`
	ExecutedAt  time.Time  `json:"executedAt"`
	CompletedAt *time.Time `json:"completedAt"`

	ExecRunID         *string `json:"execRunID"`
	TraceID           *string `json:"traceID"`
	TriggeringRunKind *string `json:"triggeringRunKind"`
	TriggeringRunID   *string `json:"triggeringRunID"`
	// ProcessingStartedAt is stamped when user code begins, so the gap from
	// ExecutedAt is queue + container start; absent when the container never
	// reported in.
	ProcessingStartedAt *time.Time `json:"processingStartedAt"`

	// ResumeOfRunID marks a RESUME of an earlier failed run and always names
	// the LINEAGE ROOT (the server normalizes it), so R1 → R2 → R3 has both R2
	// and R3 pointing at R1.
	ResumeOfRunID *string `json:"resumeOfRunID"`

	// Kind is the workflow's output channel, denormalized at run-create time:
	// unspecified|function|report|pipeline.
	Kind string `json:"kind"`
	// RuntimeVersion is denormalized at run-create time: every resume of the
	// lineage runs under it, not under the workflow row's current value.
	RuntimeVersion int `json:"runtimeVersion"`
	// Body is the inline return value of a function-kind run.
	Body *string `json:"body"`

	// Telemetry counters, hydrated from the joined exec_run.
	TokensUsed  int64 `json:"tokensUsed"`
	AICallCount int64 `json:"aiCallCount"`
	QueryCount  int64 `json:"queryCount"`
}

// RunTableOutputCount is how many tables the run wrote, whichever read it came
// from: a run from the list carries only the first 20 entries plus the count.
func (r *WorkflowRun) RunTableOutputCount() int {
	return max(r.TableOutputCount, len(r.TableOutputs))
}

// WorkflowRunOutput mirrors rdb.WorkflowRunOutput — one file a run produced.
type WorkflowRunOutput struct {
	FileKey string `json:"fileKey"`
	// Format is html|csv|xlsx|json|pdf|docx|xml.
	Format string `json:"format"`
	FileID string `json:"fileID,omitempty"`
	// Name is the author-supplied filename, empty for unnamed outputs.
	Name string `json:"name,omitempty"`
}

// WorkflowRunTableOutput mirrors rdb.WorkflowRunTableOutput — one TABLE a run
// wrote to.
type WorkflowRunTableOutput struct {
	ModelID     string `json:"modelID"`
	LogicalName string `json:"logicalName"`
	DisplayName string `json:"displayName"`
	// WriteMode is replace|append.
	WriteMode string `json:"writeMode"`
	FileCount int    `json:"fileCount"`
}

// RunResponse mirrors workflow.RunResponse — the GET /workflow/run/:runID wire
// shape. The server embeds the run, so its fields sit at the JSON root: never
// give the embedded WorkflowRun a json tag, or every run field stops decoding.
type RunResponse struct {
	WorkflowRun
	// Steps is never null: a legacy or step-less run hydrates as [].
	Steps  []StepDTO `json:"steps"`
	Health string    `json:"health"`
	// JournalEntries is how many steps a resume of this run's lineage would
	// replay, populated only for a FAILED run. It is NOT len(Steps): steps are
	// the run-span timeline, a different set from the journal.
	JournalEntries int `json:"journalEntries"`
}

// StepDTO mirrors workflow.StepDTO — one observable step of a run, projected
// from the container-step run spans. ParentStepID keeps the server's
// parentStepId spelling.
type StepDTO struct {
	ID           string  `json:"id"`
	ParentStepID *string `json:"parentStepId"`
	Seq          int     `json:"seq"`
	Name         string  `json:"name"`
	Status       string  `json:"status"`
	StartedAt    string  `json:"startedAt"`
	EndedAt      *string `json:"endedAt"`
	DurationMs   *int64  `json:"durationMs"`
	AICallCount  int64   `json:"aiCallCount"`
	QueryCount   int64   `json:"queryCount"`
	TokensUsed   int64   `json:"tokensUsed"`
	Error        *string `json:"error"`
	Logs         *string `json:"logs"`
	// Replayed marks a step whose result came from the resume lineage's
	// journal; it is what separates a durability skip from a continue_on_error
	// skip, which share status "skipped".
	Replayed bool `json:"replayed"`
}

// Running reports a run still in flight — the poll loop's continue condition.
// Deliberately "== running" rather than "not done and not error": a parked run
// can wait for days, and a status added server-side later should end the poll
// and be reported rather than hang it.
func (r *RunResponse) Running() bool {
	return r != nil && r.Status == RunStatusRunning
}

// RunWorkflow starts a run and returns immediately with the run row at status
// "running"; GetWorkflowRun polls it. parameterValues is always sent, as {} when
// there are none, because the server decodes a missing key to a nil map.
func (c *Client) RunWorkflow(ctx context.Context, workflowID string, parameterValues map[string]any) (*WorkflowRun, error) {
	if parameterValues == nil {
		parameterValues = map[string]any{}
	}
	body := struct {
		ParameterValues map[string]any `json:"parameterValues"`
	}{ParameterValues: parameterValues}
	var out WorkflowRun
	if err := c.Do(ctx, "POST", "workflow/"+url.PathEscape(workflowID)+"/run", body, &out); err != nil {
		return nil, err
	}
	return &out, nil
}

// RunInFlightCode is the wire discriminator on the 409 every run surface
// answers when a resource's `skip` concurrency policy refuses a start, mirrored
// from gt.RunInFlightCode (backend/lib/api/gt/error.go).
const RunInFlightCode = "run_in_flight"

// RunInFlight is the machine-readable half of that 409: the run already holding
// the workflow's concurrency slot, which the message does not reliably name.
// BlockingRunID is empty when the server could not name the run (the partial
// unique index refuses without a read), and callers must handle that.
type RunInFlight struct {
	Code              string `json:"code"`
	BlockingRunID     string `json:"blockingRunId"`
	BlockingRunStatus string `json:"blockingRunStatus"`
	BlockingSince     string `json:"blockingSince"`
}

// AsRunInFlight reports whether an error is the concurrency-skip refusal, and
// what it said. Gated on the CODE, not the status: a 409 here can also be a file
// precondition refusing (see workflow_write.go).
func AsRunInFlight(err error) (*RunInFlight, bool) {
	var apiErr *Error
	if !errors.As(err, &apiErr) || apiErr.Status != StatusConflict {
		return nil, false
	}
	var out RunInFlight
	if json.Unmarshal([]byte(apiErr.Body), &out) != nil || out.Code != RunInFlightCode {
		return nil, false
	}
	return &out, true
}

// ResumeWorkflowRun starts a run that RESUMES a failed one, replaying the
// lineage's journaled step results; like RunWorkflow it returns immediately.
// It sends resumeOfRunID and nothing else, deliberately not sharing RunWorkflow's
// body: a resume inherits the target's parameter values and the server refuses
// any the caller supplies, since changed parameters would invalidate the journal.
func (c *Client) ResumeWorkflowRun(ctx context.Context, workflowID, resumeOfRunID string) (*WorkflowRun, error) {
	body := struct {
		ResumeOfRunID string `json:"resumeOfRunID"`
	}{ResumeOfRunID: resumeOfRunID}
	var out WorkflowRun
	if err := c.Do(ctx, "POST", "workflow/"+url.PathEscape(workflowID)+"/run", body, &out); err != nil {
		return nil, err
	}
	return &out, nil
}

// ListWorkflowRuns reads a workflow row's most recent runs, newest first;
// limit <= 0 takes the server's default page. The order is requested explicitly
// because the server applies no default ORDER BY, and a limited query without one
// would return arbitrary rows.
func (c *Client) ListWorkflowRuns(ctx context.Context, workflowID string, limit int) ([]WorkflowRun, error) {
	query := url.Values{}
	query.Set("orderBy", "executed_at desc")
	if limit > 0 {
		query.Set("limit", strconv.Itoa(limit))
	}
	var out struct {
		Result []WorkflowRun `json:"result"`
	}
	path := "workflow/" + url.PathEscape(workflowID) + "/runs?" + query.Encode()
	if err := c.Do(ctx, "GET", path, nil, &out); err != nil {
		return nil, err
	}
	return out.Result, nil
}

// GetWorkflowRun reads one run: the row, its steps and its derived health.
func (c *Client) GetWorkflowRun(ctx context.Context, runID string) (*RunResponse, error) {
	var out RunResponse
	if err := c.Do(ctx, "GET", "workflow/run/"+url.PathEscape(runID), nil, &out); err != nil {
		return nil, err
	}
	return &out, nil
}

// GetWorkflowRunLineageHead reads the run currently carrying this run's resume
// lineage: the named run, or the successor that took over after it parked. A
// follow polls it with the ORIGINAL run id every time, because a resumed run
// sits at `resuming` forever and the server re-resolves the head on each call; a
// returned `id` different from runID is the signal that it parked and resumed.
//
// A 404 is the route missing on an older instance (a plain-text body, so key on
// the status, never the body), or a run the caller cannot read or that does not
// exist; GetWorkflowRun answering while this 404s means the route is missing.
func (c *Client) GetWorkflowRunLineageHead(ctx context.Context, runID string) (*RunResponse, error) {
	var out RunResponse
	if err := c.Do(ctx, "GET", "workflow/run/"+url.PathEscape(runID)+"/head", nil, &out); err != nil {
		return nil, err
	}
	return &out, nil
}
