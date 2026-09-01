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
// (backend/ronja/rdb/table_workflow.go). The enum is exactly these five, and
// the poll loop's terminal condition is "not running", so a status added
// server-side would end the poll rather than hang it.
//
// The last two belong to durable waits and only ever appear on a runtime-2 run
// that suspended:
//
//   - waiting is PARKED: the burst finished cleanly, its outputs are persisted,
//     and the server knows what has to happen before the run resumes. Nothing
//     failed and nothing is executing, so it is neither a success nor a
//     failure — it is a run that is not finished yet.
//   - resuming is the wake race's single-winner latch, between "a wake decided
//     to resume this run" and "the resume run exists". Terminal for THIS row:
//     the parked run never executes again, its successor carries the lineage.
//
// Both are real persisted statuses, so a poll can land on either — briefly on
// resuming, indefinitely on waiting — which is why the CLI has to name them
// rather than let them fall through to "a status this version does not know"
// and report a parked run as a failure.
const (
	RunStatusRunning  = "running"
	RunStatusDone     = "done"
	RunStatusError    = "error"
	RunStatusWaiting  = "waiting"
	RunStatusResuming = "resuming"
)

// Derived run health, mirrored from workflow.RunHealth
// (backend/api/v2/workflow/run_steps.go). Never stored: the server computes it
// on read from the run status plus the step rows. Degraded is the one worth
// knowing about — a run that finished but had a best-effort step fail, which a
// bare "done" would hide. Waiting is the parked run's health, and it is a
// SEPARATE value rather than a flavour of done for the reason the status is:
// a parked run reported as healthy tells the reader a stalled pipeline
// succeeded.
const (
	RunHealthDone     = "done"
	RunHealthDegraded = "degraded"
	RunHealthFailed   = "failed"
	RunHealthWaiting  = "waiting"
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
	// Status is running|done|error|waiting|resuming — see the constants above.
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

	// ResumeOfRunID marks this run as a RESUME of an earlier failed one: the
	// lineage's journaled step results were replayed instead of re-executed.
	// Absent for an ordinary run, which is every run of a v1 workflow.
	//
	// It always names the LINEAGE ROOT, never the run the caller asked to
	// resume — the server normalizes it — so a chain R1 → R2 → R3 has both R2
	// and R3 pointing at R1. Nothing in the CLI has to unwind that: a resume is
	// requested against the failed run you have, and the server does the rest.
	ResumeOfRunID *string `json:"resumeOfRunID"`

	// Kind is the workflow's output channel, denormalized at run-create time:
	// unspecified|function|report|pipeline.
	Kind string `json:"kind"`
	// RuntimeVersion is the workflow's runtime, denormalized at run-create time
	// — the semantics this run and every resume of its lineage execute under,
	// which is deliberately not re-read off the workflow row. 0 on an instance
	// that predates durable workflows.
	RuntimeVersion int `json:"runtimeVersion"`
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
	// Health is done|degraded|failed|waiting — see the constants above.
	Health string `json:"health"`
	// JournalEntries is how many steps this run's resume LINEAGE has journaled
	// — what a resume would replay instead of re-running. Derived on read, and
	// populated only for a FAILED run: 0 everywhere else, including on a
	// successful run that journaled plenty, because that run has nothing to
	// resume.
	//
	// It is NOT len(Steps). Steps are the observable timeline (run spans), a
	// different set from the journal, and a report that printed one as the other
	// would be a confident wrong number at the moment somebody decides whether
	// to trust a resume to skip work.
	JournalEntries int `json:"journalEntries"`
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
	// Replayed marks a step whose result came out of the resume lineage's
	// journal — the body never ran. Always present server-side, and false for
	// every step of a run that is not a resume. It is what separates a
	// durability skip from a continue_on_error skip, which share status
	// "skipped".
	Replayed bool `json:"replayed"`
}

// Running reports a run still in flight — the poll loop's continue condition.
//
// Written as "== running" rather than "not done and not error" on purpose, and
// durable waits are what turned that from a preference into the only correct
// spelling: a parked run is in flight in the SERVER's sense — it resumes on its
// own — but there is nothing here to wait for, because a park lasts until an
// agent answers or a timer fires, which can be days. "Not done and not error"
// would sit on it until --timeout.
//
// The same shape covers a status added server-side later, where the failure
// modes are not symmetric either. Ending the poll on an unrecognised status
// reports it (a caller sees "status: cancelled" and knows more than the CLI
// does), while treating it as still-running hangs on a run that has already
// stopped executing.
func (r *RunResponse) Running() bool {
	return r != nil && r.Status == RunStatusRunning
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

// RunInFlightCode is the wire discriminator on the 409 every run surface
// answers when a resource's `skip` concurrency policy refuses a start, mirrored
// from gt.RunInFlightCode (backend/lib/api/gt/error.go).
const RunInFlightCode = "run_in_flight"

// RunInFlight is the machine-readable half of that 409: the run already holding
// the workflow's concurrency slot.
//
// It exists because the message cannot be relied on to carry it. The server
// builds this refusal with the blocking run in DETAILS precisely so a client
// does not have to parse a sentence — rjerr's client message unwraps to the bare
// sentinel and drops anything a wrapper added — so reading these fields is the
// only way to name the run that is in the way, and matching prose is the thing
// that breaks the day somebody rewords it.
//
// BlockingRunID is empty when the server could not name the run: the partial
// unique index is the race-proof layer and works without a read, so the loser is
// sometimes anonymous. Callers must handle that rather than print an empty id.
type RunInFlight struct {
	Code              string `json:"code"`
	BlockingRunID     string `json:"blockingRunId"`
	BlockingRunStatus string `json:"blockingRunStatus"`
	BlockingSince     string `json:"blockingSince"`
}

// AsRunInFlight reports whether an error is the concurrency-skip refusal, and
// what it said.
//
// Gated on the CODE and not on the status: a 409 on this client also means a
// file precondition refusing (see workflow_write.go), and those two have nothing
// in common but a number. An unrecognised 409 comes back false, and the caller
// falls through to the server's own message.
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
// lineage's journaled step results instead of re-executing the work they
// record. Like RunWorkflow it returns immediately at status "running".
//
// It sends resumeOfRunID and NOTHING ELSE, which is the contract rather than a
// simplification. A resume inherits the target run's parameter values verbatim
// — the server copies them across — and refuses any parameterValues the caller
// supplies, because changed parameters would silently invalidate every step
// result computed under the old ones. So this deliberately does not share
// RunWorkflow's body struct: there, an empty map is sent to avoid a null; here,
// a parameterValues key is a thing to be unable to send at all.
//
// The server owns the rest of the rules (the target must be a failed run of
// THIS workflow, its lineage must have no run already running and none that has
// succeeded, and a gated workflow cannot be resumed) and its refusals are
// passed through verbatim.
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

// ListWorkflowRuns reads a workflow's most recent runs, newest first.
//
// The ordering is REQUESTED, not assumed: rworkflow.QueryRuns applies no
// default ORDER BY, so a limited query without one returns whatever rows
// Postgres happened to reach first — which for a "find the last failed run"
// caller is not a sort problem but a wrong answer. `executed_at desc` is
// validated server-side against the table's columns.
//
// Runs belong to a workflow ROW, and a draft is its own row, so this is asked
// of the draft `wf test` runs rather than of the live workflow. That matches
// the server's own resume rule: the run being resumed must belong to the same
// workflow the resume is posted to.
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

// `wf test` also reads table NAMES, to show which tables a run would overwrite.
// That lives on Client.GetTable in table.go — one mirror of the table row, not a
// second minimal one here. It stays best-effort by contract for this caller: a
// table it cannot read fails, and that is no reason to refuse to name the rest.
