package api

import (
	"context"
	"fmt"
	"net/url"
	"time"
)

// Workflow lifecycle values, mirrored from rdb.WorkflowLifecycle
// (backend/ronja/rdb/table_workflow.go). The CLI branches on these by exact
// string, so they are a wire contract in the same sense the device-flow poll
// codes are.
const (
	LifecycleLive     = "live"
	LifecycleDraft    = "draft"
	LifecycleVersion  = "version"
	LifecycleArchived = "archived"
	LifecycleProposed = "proposed"
)

// Workflow is a HAND-MIRROR of the subset of rdb.Workflow the CLI reads.
//
// Same rule as Me in auth.go, and the same hazard: a json tag that does not
// exist on the wire decodes to the zero value SILENTLY, so a typo here shows up
// as an empty column of output or — worse — as "no approval gate" on a workflow
// that has one. Every tag below is copied from
// backend/ronja/rdb/table_workflow.go; keep them in step with that file.
//
// Two mirroring decisions worth knowing about:
//
//   - The server's optional.V[T] marshals as the bare value or JSON `null`.
//     Unmarshalling `null` into a non-pointer is a documented no-op in
//     encoding/json, so optional STRINGS are plain strings here ("" is exactly
//     what "unset" means for an id) while optional TIMESTAMPS are pointers,
//     because "is there a submitted-for-review stamp at all" is a question the
//     CLI genuinely asks.
//   - The ID slices carry `omitempty` server-side, so an empty binding set is
//     ABSENT from the JSON rather than `[]`. Normalize() replaces the resulting
//     nils with empty slices so callers can range and compare without
//     nil-checking every field.
//
// This is a SUBSET, not the whole row: rdb.Workflow carries roughly twenty more
// columns (schedule and run bookkeeping, logic summaries, governance stamps)
// that no CLI command reads. Adding a field here is cheap and adding one that
// nothing consumes is dead weight nobody keeps in step — so the rule is
// "everything the CLI reads, plus what makes a read field legible", and the
// fixture test in workflow_test.go is what checks those tags against the wire.
type Workflow struct {
	ID          string `json:"id"`
	WorkspaceID string `json:"workspaceID"`
	FeatureID   string `json:"featureID"`

	Title       string `json:"title"`
	Description string `json:"description"`
	Entrypoint  string `json:"entrypoint"`
	// Kind is the output-channel discriminator: unspecified|function|report|pipeline.
	Kind string `json:"kind"`
	// RuntimeVersion is the semantics generation the code is written against: 1
	// (the standard runtime) or 2 (durable — steps are journaled, so a failed
	// run can be resumed). Stamped at create and afterwards raisable ONE WAY,
	// 1 -> 2, through WorkflowPatch; the server refuses 2 -> 1 outright.
	//
	// ZERO MEANS THE INSTANCE DID NOT SAY, not "runtime 1". The column is NOT
	// NULL server-side, so an instance that has it always sends a value; a 0
	// here is an instance predating durable workflows, and the caller falls back
	// to what the folder's manifest declares rather than concluding v1.
	RuntimeVersion int `json:"runtimeVersion"`

	Parameters []WorkflowParameter `json:"parameters"`
	Variables  map[string]any      `json:"variables"`

	// ReportingTimezone is the IANA calendar the workflow's DuckDB session runs
	// at on EVERY run (migration 000499) — what date_trunc and date casts bucket
	// against, so it decides which day or month a timestamp near midnight lands
	// in. A plain string for the reason the other optional strings are: the
	// server's optional.V marshals `null` when unset, and unmarshalling null into
	// a string is a documented no-op.
	//
	// "" therefore means the row declares NO zone, and falls back to the caller's
	// context zone at run time. That is a PERMANENT state, not a migration
	// backlog: 000499 backfills nothing, and a workflow created while the
	// organization has no default declares nothing either. It is NOT the same as
	// the literal "UTC" a reset writes, and the CLI keeps the two apart.
	ReportingTimezone string `json:"reportingTimezone"`

	UseDedicatedCompute bool `json:"useDedicatedCompute"`
	// ApprovalGate is null on a workflow with no gate. `wf test` (Phase 4)
	// preflights Enabled: a gated workflow cannot be run over HTTP at all,
	// because only an agent session can produce an approval.
	ApprovalGate *WorkflowApprovalGate `json:"approvalGate"`

	// ParentWorkflowID is empty for a live row AND for a parentless draft (a
	// brand-new workflow nobody has published yet). Non-empty on an edit draft
	// and on a committed version snapshot.
	ParentWorkflowID string     `json:"parentWorkflowID"`
	CommittedAt      *time.Time `json:"committedAt"`
	Hidden           bool       `json:"hidden"`

	SecretIDs         []string `json:"secretIDs"`
	ExplicitSecretIDs []string `json:"explicitSecretIDs"`
	InputTableIDs     []string `json:"inputTableIDs"`
	OutputTableIDs    []string `json:"outputTableIDs"`
	AgentIDs          []string `json:"agentIDs"`
	CodexIDs          []string `json:"codexIDs"`
	QuerySecretIDs    []string `json:"querySecretIDs"`
	PipPackages       []string `json:"pipPackages"`

	// Lifecycle is trigger-computed server-side: live|draft|version|archived|proposed.
	Lifecycle string `json:"lifecycle"`
	// SubmittedForReviewAt is set once a drafter asks an admin to commit their
	// draft; null on every other row, and on a draft they have not submitted.
	SubmittedForReviewAt *time.Time `json:"submittedForReviewAt"`
	DrafterUserID        string     `json:"drafterUserID"`
	// BaseVersionID is the committed version a DRAFT was forked from — the
	// server's own record of what this draft's content descends from
	// (rdb.Workflow.BaseVersionID, stamped at checkout and cleared on commit).
	//
	// Null on every row that is not an open draft, and null on a draft of a
	// workflow that has never been versioned, so an empty value means "no
	// anchor" rather than "no such version". It is read ONLY by `wf clone`, to
	// record the right head pointer when the files came from a draft rather
	// than from live: the current head may be NEWER than the draft's base, and
	// a folder that recorded the newer one would claim to have seen a version
	// whose changes it does not contain.
	BaseVersionID string `json:"baseVersionID"`
	// FeatureScope is the joined feature's scope (private|workspace|
	// organization), stamped at read time — the field to branch on for privacy,
	// not the legacy scope/private columns.
	FeatureScope string `json:"featureScope"`

	CreatedAt time.Time `json:"createdAt"`
	UpdatedAt time.Time `json:"updatedAt"`

	// URL is the absolute frontend page for this row, stamped by the SERVER
	// (api/v2/workflow.WorkflowView) on the single-row routes. Never built here:
	// the instance URL a profile records is the API origin, and the deploy
	// template puts the backend on api.* and the frontend on app.*, so a link
	// assembled from it points at a host that serves no pages. In local dev the
	// two differ even more sharply.
	//
	// EMPTY IS NORMAL AND SILENT. The field is omitempty server-side and an
	// instance with no configured frontend origin emits none at all (see
	// backend/lib/deeplink), which is the ordinary state of a dev box. A caller
	// with no url prints no link — it never falls back to deriving one, and it
	// never treats absence as an error.
	//
	// Absent on the LIST routes (GET /workflow/query, GET :id/versions) and on
	// rows the server refuses to link, so read it off the response you have
	// rather than assuming every Workflow value carries one.
	URL string `json:"url,omitempty"`
}

// WorkflowParameter mirrors rdb.WorkflowParameter. OptionsQuery is SQL carrying
// `{{ ref('tableID') }}` markers, which is why validation attributes findings
// from it to a synthetic parameters[N].optionsQuery path.
type WorkflowParameter struct {
	Name         string   `json:"name"`
	Label        string   `json:"label"`
	Description  string   `json:"description,omitempty"`
	Type         string   `json:"type"` // "string" | "number" | "date" | "select"
	DefaultValue any      `json:"defaultValue,omitempty"`
	Required     bool     `json:"required"`
	Options      []string `json:"options,omitempty"`
	OptionsQuery string   `json:"optionsQuery,omitempty"`
}

// WorkflowApprovalGate is a SUBSET of rdb.WorkflowApprovalGate: what the CLI
// acts on, which is whether the gate is switched on at all.
//
// The rest of the server's shape — the approvers, the author's message, the
// context parameter names, and the whole email/Slack delivery tree — is
// deliberately NOT mirrored. A gate the CLI cannot run is a gate the CLI has
// nothing to say about, and a mirrored field nobody reads is a field nobody
// keeps in step with backend/ronja/rdb/table_workflow.go. Add them back when a
// command genuinely needs them (a `wf status` that names the approvers and
// says where the request goes is the obvious candidate), not before.
type WorkflowApprovalGate struct {
	Enabled bool `json:"enabled"`
}

// WorkflowFile mirrors rdb.WorkflowFile — one source file of one workflow row.
// There is no content hash on the wire, so the CLI hashes Content itself.
type WorkflowFile struct {
	ID         string    `json:"id"`
	WorkflowID string    `json:"workflowID"`
	Path       string    `json:"path"`
	Content    string    `json:"content"`
	CreatedAt  time.Time `json:"createdAt"`
	UpdatedAt  time.Time `json:"updatedAt"`
}

// Normalize replaces nil binding slices with empty ones.
//
// The server omits them when empty, so "no output tables" and "field absent"
// arrive identically — and code that compares or ranges over them should not
// have to care which.
func (w *Workflow) Normalize() {
	if w == nil {
		return
	}
	for _, slot := range []*[]string{
		&w.SecretIDs, &w.ExplicitSecretIDs, &w.InputTableIDs, &w.OutputTableIDs,
		&w.AgentIDs, &w.CodexIDs, &w.QuerySecretIDs, &w.PipPackages,
	} {
		if *slot == nil {
			*slot = []string{}
		}
	}
	if w.Parameters == nil {
		w.Parameters = []WorkflowParameter{}
	}
}

// IsGated reports whether runs of this workflow require a human approval.
func (w *Workflow) IsGated() bool {
	return w != nil && w.ApprovalGate != nil && w.ApprovalGate.Enabled
}

// IdentityID is the STABLE id of the workflow this row belongs to: the parent
// for an edit draft or a version snapshot, the row itself for a live workflow
// or a parentless draft.
//
// This is what a manifest binding records. A draft's own id is per-user and
// disappears on commit, so a committed file naming one would point at a row
// that stops existing the moment its author publishes.
func (w *Workflow) IdentityID() string {
	if w == nil {
		return ""
	}
	if w.ParentWorkflowID != "" {
		return w.ParentWorkflowID
	}
	return w.ID
}

// GetWorkflow reads a workflow's metadata. 404s for an id the caller cannot
// reach — the read gate does not distinguish absent from invisible.
func (c *Client) GetWorkflow(ctx context.Context, id string) (*Workflow, error) {
	var out Workflow
	if err := c.Do(ctx, "GET", "workflow/"+url.PathEscape(id), nil, &out); err != nil {
		return nil, err
	}
	out.Normalize()
	return &out, nil
}

// GetWorkflowDraft reads the CALLER'S OWN open draft of a workflow, returning
// (nil, nil) when they have none.
//
// The endpoint answers 200 with a JSON `null` body rather than 404 in that case
// (see the route's own description in backend/api/v2/workflow/handler.go), so
// decoding into a **Workflow is not a stylistic choice: decoding into a struct
// would turn "no draft" into a zero-value Workflow with an empty id, and every
// caller would then act on a draft that does not exist.
func (c *Client) GetWorkflowDraft(ctx context.Context, id string) (*Workflow, error) {
	var out *Workflow
	if err := c.Do(ctx, "GET", "workflow/"+url.PathEscape(id)+"/draft", nil, &out); err != nil {
		return nil, err
	}
	out.Normalize()
	return out, nil
}

// ListWorkflowFiles reads every file of ONE workflow row — note that a draft
// has its own complete file set, so the id passed here decides whether you get
// the live code or the draft's.
//
// The route returns a bare JSON array ([]*rdb.WorkflowFile), not an envelope.
func (c *Client) ListWorkflowFiles(ctx context.Context, id string) ([]WorkflowFile, error) {
	var out []WorkflowFile
	if err := c.Do(ctx, "GET", "workflow/"+url.PathEscape(id)+"/files", nil, &out); err != nil {
		return nil, err
	}
	return out, nil
}

// DescribeLifecycle renders a lifecycle value for a human, including the ones
// the CLI refuses to work with.
func DescribeLifecycle(lifecycle string) string {
	switch lifecycle {
	case LifecycleLive:
		return "live"
	case LifecycleDraft:
		return "draft"
	case LifecycleVersion:
		return "committed version snapshot"
	case LifecycleArchived:
		return "archived"
	case LifecycleProposed:
		return "proposed (awaiting approval)"
	case "":
		return "unknown"
	default:
		return fmt.Sprintf("%q", lifecycle)
	}
}
