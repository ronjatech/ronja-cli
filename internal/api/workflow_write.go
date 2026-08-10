package api

// The WRITE half of the workflow surface: validation, file sync, and the four
// lifecycle verbs a dev loop needs (checkout, commit, publish, discard).
//
// Split from workflow.go — which mirrors the rows the CLI READS — because these
// are the calls that change something, and every one of them is a place where a
// wrong json tag is silently destructive rather than merely blank. Same
// hand-mirror rule as everywhere else in this package: every tag below is
// copied from the server struct named in its comment, and the fixture tests are
// what hold them in step.

import (
	"context"
	"net/url"
	"strings"
)

// SeverityError is mirrored from rworkflow's SeverityError. The CLI branches on
// it by exact string — an error finding is what makes `wf push` refuse — so it
// is a wire contract.
//
// The server's finding CODES (invalid_path, unresolved_ref, secret_dropped, …)
// are deliberately not mirrored as constants. The CLI never branches on a code:
// it prints Message and Path, so an unknown code is displayed as faithfully as
// a known one, and a mirrored list would only be one more thing to keep in step
// with backend/resource/rworkflow/validate_files.go.
const SeverityError = "error"

// ValidateFile is one candidate file, mirroring rworkflow.ValidateFile.
type ValidateFile struct {
	Path    string `json:"path"`
	Content string `json:"content"`
}

// ValidateInput mirrors rworkflow.ValidateFilesInput — a whole candidate
// workflow, with no workflow ID anywhere in it. That absence is the point: the
// endpoint persists nothing and needs nothing to exist, which is what makes
// "iterate an existing script to green before the first push" possible.
//
// Parameters and PipPackages are omitempty because the CLI does not persist
// either in the manifest today: a workflow's declared parameters live
// server-side on the draft, and sending an empty list would read as "this
// candidate declares no parameters" rather than "I am not telling you about
// them". The server treats an absent list as none, which is the honest answer
// for a folder that has no record of them.
type ValidateInput struct {
	FeatureID   string              `json:"featureID"`
	Entrypoint  string              `json:"entrypoint"`
	Parameters  []WorkflowParameter `json:"parameters,omitempty"`
	Files       []ValidateFile      `json:"files"`
	PipPackages []string            `json:"pipPackages,omitempty"`
}

// ValidateFinding mirrors rworkflow.ValidateFinding: one problem, attributed to
// the source that caused it.
//
// Path is a file path, or a synthetic source ("parameters[0].optionsQuery",
// "pipPackages", "inputTableIDs") when the problem does not belong to a file —
// so a client grouping by Path gets a sensible bucket either way.
type ValidateFinding struct {
	Severity string `json:"severity"`
	Code     string `json:"code"`
	Message  string `json:"message"`
	Path     string `json:"path"`
	// Marker is the canonical marker form for the offending binding, when the
	// finding is about one. A hint for locating the problem, not necessarily
	// the literal text in the file.
	Marker string `json:"marker,omitempty"`
}

// IsError reports whether a finding would block a save.
func (f ValidateFinding) IsError() bool { return f.Severity == SeverityError }

// ValidateBindings mirrors rworkflow.ValidateBindings — what the candidate
// WOULD bind to if saved as-is. Every slice is always present on the wire (no
// omitempty server-side), so unlike Workflow's binding columns these need no
// normalization.
type ValidateBindings struct {
	InputTableIDs  []string `json:"inputTableIDs"`
	OutputTableIDs []string `json:"outputTableIDs"`
	SecretIDs      []string `json:"secretIDs"`
	QuerySecretIDs []string `json:"querySecretIDs"`
	AgentIDs       []string `json:"agentIDs"`
	CodexIDs       []string `json:"codexIDs"`
}

// ValidateResult mirrors rworkflow.ValidateResult.
type ValidateResult struct {
	Findings []ValidateFinding `json:"findings"`
	Resolved ValidateBindings  `json:"resolved"`
}

// Errors returns the blocking findings, in the order the server reported them.
func (r *ValidateResult) Errors() []ValidateFinding {
	var out []ValidateFinding
	for _, f := range r.Findings {
		if f.IsError() {
			out = append(out, f)
		}
	}
	return out
}

// OK reports whether the candidate would save cleanly. Warnings do not make it
// false — a save succeeds through them, and so does a push.
func (r *ValidateResult) OK() bool { return len(r.Errors()) == 0 }

// FileSaveResponse mirrors the workflow handler's FileSaveResponse: the saved
// file at the response root, plus soft save-time notices.
//
// Warnings is the SOFT tier of the save path — an unreachable secret is
// filtered out of the binding rather than refused — and it is the only place
// those notices exist, so the CLI passes them through verbatim rather than
// paraphrasing.
type FileSaveResponse struct {
	WorkflowFile
	Warnings []string `json:"warnings,omitempty"`
}

// FileDeleteResponse mirrors the handler's FileDeleteResponse. Deleting the
// last file that referenced a secret drops that binding, which is exactly the
// kind of change nobody asked for that a warning exists to surface.
type FileDeleteResponse struct {
	Warnings []string `json:"warnings,omitempty"`
}

// CreateWorkflowInput is the body of POST /workflow.
//
// The server's input type is the whole rdb.Workflow, and this deliberately is
// not: everything else on that row is either derived (the binding columns are
// stamped from the code at file save), stamped by the server (drafter
// attribution is scrubbed from HTTP input on purpose), or not the CLI's to set.
// Sending three fields makes it obvious that a first push creates an EMPTY
// workflow whose bindings then come from its files.
type CreateWorkflowInput struct {
	FeatureID  string `json:"featureID"`
	Title      string `json:"title"`
	Entrypoint string `json:"entrypoint"`
	// Parameters is the declared parameter set, when the folder manages one.
	// omitempty because a folder that does not manage parameters must create a
	// workflow with none rather than assert an empty declaration.
	Parameters []WorkflowParameter `json:"parameters,omitempty"`
}

// WorkflowPatch is the body of PUT /workflow/:id, mirroring the subset of
// rworkflow.Patch the CLI writes.
//
// Two fields, both omitempty: every field of the server's Patch is an
// optional.V, so an absent key means "unchanged" while an explicit empty string
// would CLEAR the value. A folder whose manifest has lost its title must not be
// able to blank the workflow's.
//
// Entrypoint is here because the manifest owns it: without this the row keeps
// running whatever file it was created with, and an entrypoint RENAME is
// impossible — the server refuses to delete the file the row still names.
// Ordering matters and belongs to the caller: rworkflow's Update refuses an
// entrypoint the workflow has no file for, so the new file must be written
// BEFORE this patch, and the old one deleted after it.
// Parameters is a POINTER for the reason the string fields are omitempty: the
// server's Patch.Parameters is an optional slice, so an absent key leaves the
// declaration alone while an explicit [] CLEARS it. Both are things a folder
// legitimately means — "I don't manage parameters" and "I declare none" — and a
// plain slice with omitempty collapses them into the first, making it impossible
// to remove the last parameter from a workflow.
type WorkflowPatch struct {
	Title      string               `json:"title,omitempty"`
	Entrypoint string               `json:"entrypoint,omitempty"`
	Parameters *[]WorkflowParameter `json:"parameters,omitempty"`
}

// Empty reports a patch that would change nothing, so the caller can skip the
// round trip rather than send `{}`.
func (p WorkflowPatch) Empty() bool {
	return p.Title == "" && p.Entrypoint == "" && p.Parameters == nil
}

// ValidateWorkflowFiles dry-runs a candidate workflow. Findings are data: a
// candidate full of errors still comes back as a successful call, and only a
// malformed request (no featureID, a feature the caller cannot read) is an
// error here.
//
// One of the two SLOW calls: the whole folder goes up in one body and every
// marker in it is resolved against the caller's access, so this is doing real
// work rather than reading a row.
func (c *Client) ValidateWorkflowFiles(ctx context.Context, in ValidateInput) (*ValidateResult, error) {
	var out ValidateResult
	if err := c.doSlow(ctx, "POST", "workflow/validate", in, &out); err != nil {
		return nil, err
	}
	return &out, nil
}

// CreateWorkflow creates a workflow inside a feature. The row it returns is a
// parentless DRAFT — hidden until published — which is why the first push
// needs no separate checkout.
func (c *Client) CreateWorkflow(ctx context.Context, in CreateWorkflowInput) (*Workflow, error) {
	var out Workflow
	if err := c.Do(ctx, "POST", "workflow", in, &out); err != nil {
		return nil, err
	}
	out.Normalize()
	return &out, nil
}

// UpdateWorkflow patches a workflow's metadata.
//
// The endpoint answers with nothing, and it DROPS the save-time warnings the
// file path returns (the server's own comment says to plumb a new endpoint
// rather than overload this one) — so a caller that needs the row's current
// state re-reads it afterwards.
func (c *Client) UpdateWorkflow(ctx context.Context, id string, patch WorkflowPatch) error {
	return c.Do(ctx, "PUT", "workflow/"+url.PathEscape(id), patch, nil)
}

// CheckoutWorkflow gets-or-creates the caller's own draft of a live workflow.
// Idempotent: calling it twice returns the same draft.
func (c *Client) CheckoutWorkflow(ctx context.Context, id string) (*Workflow, error) {
	var out Workflow
	if err := c.Do(ctx, "POST", "workflow/"+url.PathEscape(id)+"/checkout", nil, &out); err != nil {
		return nil, err
	}
	out.Normalize()
	return &out, nil
}

// PutWorkflowFile creates or replaces one file.
//
// This is the hard-validating path: unreachable `{{ ref }}` / `{{ write }}` /
// `{{ agent }}` / `{{ codex }}` markers roll the write back with a 400, and the
// file keeps its previous content. Secrets are the soft tier and come back as
// Warnings instead.
//
// The other SLOW call: every save re-derives the workflow's binding columns in
// the same transaction, so a file write is a row write with marker resolution
// attached — not something to hold to a read's deadline.
func (c *Client) PutWorkflowFile(ctx context.Context, id, path, content string) (*FileSaveResponse, error) {
	var out FileSaveResponse
	body := struct {
		Content string `json:"content"`
	}{Content: content}
	if err := c.doSlow(ctx, "PUT", workflowFilePath(id, path), body, &out); err != nil {
		return nil, err
	}
	return &out, nil
}

// GetWorkflowFile reads one file back by path, answering 404 when the row holds
// none.
//
// It lives on the write side because it exists for the write side: a PUT that
// died on a TIMEOUT may still have committed, and the only way to find out
// which is to ask. Nothing else reads a single file — the rest of the CLI wants
// the whole set — so this is a repair tool, not a read API.
func (c *Client) GetWorkflowFile(ctx context.Context, id, path string) (*WorkflowFile, error) {
	var out WorkflowFile
	if err := c.Do(ctx, "GET", workflowFilePath(id, path), nil, &out); err != nil {
		return nil, err
	}
	return &out, nil
}

// DeleteWorkflowFile removes one file from a draft. The server refuses to
// delete the entrypoint; its message is passed through rather than second-
// guessed, because the remedy (change the entrypoint first) is its to state.
func (c *Client) DeleteWorkflowFile(ctx context.Context, id, path string) (*FileDeleteResponse, error) {
	var out FileDeleteResponse
	if err := c.Do(ctx, "DELETE", workflowFilePath(id, path), nil, &out); err != nil {
		return nil, err
	}
	return &out, nil
}

// CommitWorkflowDraft applies a draft onto its parent. Takes the DRAFT id.
func (c *Client) CommitWorkflowDraft(ctx context.Context, draftID string) error {
	return c.Do(ctx, "POST", "workflow/"+url.PathEscape(draftID)+"/commit", nil, nil)
}

// PublishWorkflowDraft takes a PARENTLESS draft live. Takes the draft's own id,
// which for a parentless draft is also the workflow's stable identity.
func (c *Client) PublishWorkflowDraft(ctx context.Context, draftID string) error {
	return c.Do(ctx, "POST", "workflow/"+url.PathEscape(draftID)+"/publish", nil, nil)
}

// DiscardWorkflowDraft deletes a draft, throwing its uncommitted changes away.
// The parent live workflow is untouched.
func (c *Client) DiscardWorkflowDraft(ctx context.Context, draftID string) error {
	return c.Do(ctx, "POST", "workflow/"+url.PathEscape(draftID)+"/discard", nil, nil)
}

// DeleteWorkflow soft-deletes a workflow — the row survives a 30-day TTL before
// the janitor purges it. Unlike DiscardWorkflowDraft this removes the WORKFLOW,
// so it is only ever reached through an explicit opt-in.
//
// The one case the CLI needs it for is a parentless draft — a workflow created
// by a first push and then abandoned. Discard cannot express it (there is no
// parent for the row to fall back to), and without this the only remaining
// answer is the web UI, which is no answer at all for a caller that has nothing
// but a terminal.
func (c *Client) DeleteWorkflow(ctx context.Context, id string) error {
	return c.Do(ctx, "DELETE", "workflow/"+url.PathEscape(id), nil, nil)
}

// RequestWorkflowReview submits the caller's own edit draft for admin review.
//
// NOTE the different prefix: this route lives on the GOVERNANCE handler, whose
// group is scoped `admin` rather than the core workflow handler's `automation`.
// The CLI's full-access PAT covers both; a scoped token would half-work, which
// is why the failure is reported as-is rather than swallowed.
func (c *Client) RequestWorkflowReview(ctx context.Context, draftID string) error {
	return c.Do(ctx, "POST", "workflow/draft/"+url.PathEscape(draftID)+"/request-review", nil, nil)
}

// workflowFilePath builds a file route, escaping each segment but keeping the
// separators — the route is a gin wildcard, so the path's own slashes are part
// of it rather than something to encode away.
func workflowFilePath(id, path string) string {
	segments := strings.Split(path, "/")
	for i, s := range segments {
		segments[i] = url.PathEscape(s)
	}
	return "workflow/" + url.PathEscape(id) + "/files/" + strings.Join(segments, "/")
}
