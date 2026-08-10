package api

import (
	"context"
	"net/url"
	"time"
)

// The RUN half of the workflow surface: start one, and poll it to completion.
//
// Split from workflow.go (rows the CLI reads) and workflow_write.go (calls that
// change the workflow itself) because a run is neither: it changes nothing
// about the workflow and everything about the world the workflow writes to.
// That asymmetry is why `wf test` gates bound output tables behind a flag.
//
// Same hand-mirror rule as the rest of the package: every tag below is copied
// from the server struct named in its comment. It matters more here than
// anywhere else, because `wf test --json` emits the decoded run VERBATIM — a
// field this file does not mirror is a field that silently disappears from the
// CLI's output rather than merely going unread.

// Workflow run statuses, mirrored from rdb.WorkflowRunStatus
// (backend/ronja/rdb/table_workflow.go:78). The enum is exactly these three,
// and the poll loop's terminal condition is "not running", so a status added
// server-side would end the poll rather than hang it.
const (
	RunStatusRunning = "running"
	RunStatusDone    = "done"
	RunStatusError   = "error"
)

// Derived run health, mirrored from workflow.RunHealth
// (backend/api/v2/workflow/run_steps.go). Never stored: the server computes it
// on read from the run status plus the step rows. Degraded is the one worth
// knowing about — a run that finished but had a best-effort step fail, which a
// bare "done" would hide.
const (
	RunHealthDone     = "done"
	RunHealthDegraded = "degraded"
	RunHealthFailed   = "failed"
)

// WorkflowRun mirrors rdb.WorkflowRun — one execution of one workflow row.
//
// Mirrored in FULL rather than as a subset (unlike Workflow), because this
// struct is what `--json` prints: the CLI reads maybe six of these fields, but
// dropping the rest would quietly narrow the machine-readable output of a
// command whose whole purpose is to report what a run did.
//
// The optional.V columns are pointers wherever "absent" is a real answer
// (a run still going has no completedAt; a successful one has no error).
// TenantID is deliberately not mirrored: it is the server's bookkeeping, and
// nothing at the command line has a use for it.
type WorkflowRun struct {
	ID              string         `json:"id"`
	WorkflowID      string         `json:"workflowID"`
	ParameterValues map[string]any `json:"parameterValues"`
	// Status is running|done|error — see the constants above.
	Status       string                   `json:"status"`
	Outputs      []WorkflowRunOutput      `json:"outputs"`
	TableOutputs []WorkflowRunTableOutput `json:"tableOutputs"`
	// Logs is the run's FULL captured output, on the row itself. There is no
	// streaming endpoint, which is why the CLI tails this at the end rather
	// than following it live.
	Logs        string     `json:"logs"`
	Error       *string    `json:"error"`
	ExecutedBy  string     `json:"executedBy"`
	ExecutedAt  time.Time  `json:"executedAt"`
	CompletedAt *time.Time `json:"completedAt"`

	// ExecRunID / TraceID link the run to the generic execution telemetry and
	// to the cross-primitive trace. Both nullable; the CLI carries them so a
	// caller can follow either one into the web UI.
	ExecRunID *string `json:"execRunID"`
	TraceID   *string `json:"traceID"`
	// TriggeringRunKind / TriggeringRunID name the run that triggered this one.
	// Always absent for a CLI-started run — it is a top-level execution — and
	// mirrored anyway so the shape is the server's, not a CLI-shaped subset.
	TriggeringRunKind *string `json:"triggeringRunKind"`
	TriggeringRunID   *string `json:"triggeringRunID"`
	// ProcessingStartedAt is stamped by the Python harness the moment user code
	// begins, so the gap from ExecutedAt is queue + container start. Absent when
	// the container never reported in.
	ProcessingStartedAt *time.Time `json:"processingStartedAt"`

	// Kind is the workflow's output channel, denormalized at run-create time:
	// unspecified|function|report|pipeline.
	Kind string `json:"kind"`
	// Body is the inline return value of a function-kind run, capped server-side
	// at 8 KB.
	Body *string `json:"body"`

	// Telemetry counters, hydrated from the joined exec_run. Zero for legacy
	// runs and for runs that made no AI call or query.
	TokensUsed  int64 `json:"tokensUsed"`
	AICallCount int64 `json:"aiCallCount"`
	QueryCount  int64 `json:"queryCount"`
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
// wrote to. This is the thing `--write-live` exists for: WriteMode "replace"
// means the run replaced the table's contents.
type WorkflowRunTableOutput struct {
	ModelID     string `json:"modelID"`
	LogicalName string `json:"logicalName"`
	DisplayName string `json:"displayName"`
	// WriteMode is replace|append.
	WriteMode string `json:"writeMode"`
	FileCount int    `json:"fileCount"`
}

// RunResponse mirrors workflow.RunResponse — the GET /workflow/run/:runID wire
// shape.
//
// The server embeds *rdb.WorkflowRun, so every run field sits at the JSON ROOT
// alongside steps and health rather than under a "run" key. The embedded
// (untagged) WorkflowRun below reproduces exactly that flattening — do not give
// it a json tag, or the CLI stops decoding every field of the run it polls.
type RunResponse struct {
	WorkflowRun
	// Steps is never null: a legacy or step-less run hydrates as [].
	Steps []StepDTO `json:"steps"`
	// Health is done|degraded|failed — see the constants above.
	Health string `json:"health"`
}

// StepDTO mirrors workflow.StepDTO — one observable step of a run, projected
// from the container-step run spans.
//
// ParentStepID keeps the server's parentStepId spelling (a frontend back-compat
// name for what is now parent_span_id). TokensUsed is 0 in production today.
type StepDTO struct {
	ID           string  `json:"id"`
	ParentStepID *string `json:"parentStepId"`
	Seq          int     `json:"seq"`
	Name         string  `json:"name"`
	// Status is a run-span status; "failed" is the one the health derivation
	// keys on.
	Status      string  `json:"status"`
	StartedAt   string  `json:"startedAt"`
	EndedAt     *string `json:"endedAt"`
	DurationMs  *int64  `json:"durationMs"`
	AICallCount int64   `json:"aiCallCount"`
	QueryCount  int64   `json:"queryCount"`
	TokensUsed  int64   `json:"tokensUsed"`
	Error       *string `json:"error"`
	Logs        *string `json:"logs"`
}

// Running reports a run still in flight — the poll loop's continue condition.
//
// Written as "== running" rather than "not done and not error" on purpose. The
// enum is exactly three values today, so the two spellings agree; they differ
// only on a status added server-side later, and there the failure modes are not
// symmetric. Ending the poll on an unrecognised status reports it (a caller
// sees "status: cancelled" and knows more than the CLI does), while treating it
// as still-running hangs until --timeout on a run that already finished.
func (r *RunResponse) Running() bool {
	return r != nil && r.Status == RunStatusRunning
}

// Table is a minimal mirror of rdb.ModelV2: an id and a name, which is all the
// CLI wants — a table id in a refusal message is correct but unreadable.
//
// Name is optional server-side, hence the pointer: an unnamed table is a real
// state (a freshly created one), not a decode failure.
type Table struct {
	ID   string  `json:"id"`
	Name *string `json:"name"`
}

// RunWorkflow starts a run and returns IMMEDIATELY with the created run row at
// status "running" — execution is asynchronous, and GetWorkflowRun is how you
// learn what happened.
//
// Two refusals arrive here as 400s with the server's own message: an approval
// gate (which no HTTP caller can satisfy — only an agent session produces an
// approval) and the per-tenant credit kill-stop. Both are passed through rather
// than paraphrased.
//
// parameterValues is always sent, as {} when there are none. NOT `omitempty`:
// that omits the key, which is the null map it is meant to avoid — the server
// decodes a missing key to a nil map exactly as it does an explicit null.
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

// GetWorkflowRun reads one run: the row, its steps and its derived health.
//
// Note the route is /workflow/run/:runID — keyed by the RUN, not nested under
// the workflow — so polling needs nothing but the id the run started with.
func (c *Client) GetWorkflowRun(ctx context.Context, runID string) (*RunResponse, error) {
	var out RunResponse
	if err := c.Do(ctx, "GET", "workflow/run/"+url.PathEscape(runID), nil, &out); err != nil {
		return nil, err
	}
	return &out, nil
}

// GetTable reads a table's identity so an id can be shown with its name.
//
// Best-effort by contract: the caller is expected to fall back to printing the
// bare id. A token scoped away from the data surface, or a table the caller
// cannot read, fails here — and neither is a reason to refuse to tell someone
// which tables a run would overwrite.
func (c *Client) GetTable(ctx context.Context, id string) (*Table, error) {
	var out Table
	if err := c.Do(ctx, "GET", "feature/model/"+url.PathEscape(id), nil, &out); err != nil {
		return nil, err
	}
	return &out, nil
}
