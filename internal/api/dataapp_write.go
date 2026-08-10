package api

// The WRITE half of the data-app surface: create, metadata + allowlist patch,
// file sync, and the four lifecycle verbs a dev loop needs (checkout, validate,
// commit, discard) plus the review request.
//
// Split from dataapp.go — which mirrors the rows the CLI READS — because these
// are the calls that change something, and every one of them is a place where a
// wrong json tag is silently destructive rather than merely blank. Same
// hand-mirror rule as everywhere else in this package: every tag below is copied
// from the server struct named in its comment.

import (
	"context"
	"net/url"
	"strings"
)

// CreateDataAppInput is the body of POST /dataapp, mirroring the subset of
// rdataapp.CreateInput the CLI writes.
//
// What this call PRODUCES matches the workflow equivalent as of migration
// 000495: a hidden PARENTLESS DRAFT, visible only to its drafter, which the
// first commit promotes to live IN PLACE — so the id this returns is the app's
// id forever and is safe to record in ronja.json before any publish. (Before
// 000495 the row was born LIVE and empty, serving the pending page to the whole
// feature until someone published it.)
//
// The allowlists are sent at creation because a data app with none can query
// nothing, and the alternative (create, then patch) leaves a window where the
// app exists in a state its author never asked for.
type CreateDataAppInput struct {
	FeatureID   string `json:"featureID"`
	Name        string `json:"name"`
	Description string `json:"description,omitempty"`

	DataAppAccess
}

// DataAppPatch is the body of PUT /dataapp/:id, mirroring the subset of
// rdataapp.Patch the CLI writes.
//
// Every field is a POINTER because every field of the server's Patch is an
// optional.V: an absent key means "unchanged", while an explicit value —
// including an empty list — REPLACES what is there. Both are things a folder
// legitimately means ("I do not manage the allowlists" and "this app is allowed
// nothing"), and a plain slice with omitempty collapses them into the first,
// making it impossible to remove the last table from an app.
//
// There is deliberately no Entrypoint: rdataapp.Patch has none, because a data
// app's entrypoint is stamped at create and never moves.
type DataAppPatch struct {
	Name *string `json:"name,omitempty"`

	AllowedTableIDs    *[]string `json:"allowedTableIDs,omitempty"`
	AllowedSecretIDs   *[]string `json:"allowedSecretIDs,omitempty"`
	AllowedAgentIDs    *[]string `json:"allowedAgentIDs,omitempty"`
	AllowedWorkflowIDs *[]string `json:"allowedWorkflowIDs,omitempty"`
	AllowedCodexIDs    *[]string `json:"allowedCodexIDs,omitempty"`
	AllowedMetricIDs   *[]string `json:"allowedMetricIDs,omitempty"`
	Capabilities       *[]string `json:"capabilities,omitempty"`
}

// Empty reports a patch that would change nothing, so the caller can skip the
// round trip rather than send `{}`.
func (p DataAppPatch) Empty() bool {
	return p.Name == nil && p.AllowedTableIDs == nil && p.AllowedSecretIDs == nil &&
		p.AllowedAgentIDs == nil && p.AllowedWorkflowIDs == nil &&
		p.AllowedCodexIDs == nil && p.AllowedMetricIDs == nil && p.Capabilities == nil
}

// SetAccess fills in every allowlist field from a declared set.
//
// All seven or none: the allowlists are one decision, and patching a subset
// would let a folder that manages access leave a list it happens not to use
// pointing at whatever the row had.
func (p *DataAppPatch) SetAccess(access DataAppAccess) {
	n := access.Normalized()
	p.AllowedTableIDs = &n.AllowedTableIDs
	p.AllowedSecretIDs = &n.AllowedSecretIDs
	p.AllowedAgentIDs = &n.AllowedAgentIDs
	p.AllowedWorkflowIDs = &n.AllowedWorkflowIDs
	p.AllowedCodexIDs = &n.AllowedCodexIDs
	p.AllowedMetricIDs = &n.AllowedMetricIDs
	p.Capabilities = &n.Capabilities
}

// CompileDiagnostic is one author-readable build error, mirroring
// dataappbundle.Diagnostic.
type CompileDiagnostic struct {
	Message string `json:"message"`
	Line    int    `json:"line"`
	Column  int    `json:"column"`
	File    string `json:"file,omitempty"`
}

// CompileError is a failed bundle compile, mirroring the api/v2/dataapp
// CompileDiagnostics response type.
type CompileError struct {
	Message     string              `json:"message"`
	Diagnostics []CompileDiagnostic `json:"diagnostics"`
}

// DataAppFileSaveResponse is the body of PUT :id/files/*path.
//
// The compile failure arrives IN a 200 rather than as an error, and that is the
// server being accurate rather than lenient: the file write COMMITS before the
// bundle is recompiled, so the save really did happen. Pushing a multi-file app
// walks through states that do not compile by construction — App.tsx importing a
// module that is one request away — so a client that treated this as a failure
// could not push more than one file.
//
// What it does NOT mean is that the app is fine. validated_at was cleared by the
// write and only a clean compile re-stamps it, so ValidateDataApp is the verdict
// and CommitDataAppDraft refuses until it passes.
type DataAppFileSaveResponse struct {
	DataAppFile
	CompileError *CompileError `json:"compileError,omitempty"`
}

// DataAppFileDeleteResponse is the body of DELETE :id/files/*path. DataAppID is
// the row the delete landed on — deleting from a live app auto-forks a draft.
type DataAppFileDeleteResponse struct {
	DataAppID    string        `json:"dataAppID"`
	CompileError *CompileError `json:"compileError,omitempty"`
}

// CreateDataApp creates a data app inside a feature. The row it returns is LIVE
// and empty — see CreateDataAppInput.
func (c *Client) CreateDataApp(ctx context.Context, in CreateDataAppInput) (*DataApp, error) {
	var out DataApp
	if err := c.Do(ctx, "POST", "dataapp", in, &out); err != nil {
		return nil, err
	}
	out.Normalize()
	return &out, nil
}

// UpdateDataApp patches a data app's metadata and allowlists.
//
// Returns the row that RECEIVED the edit, which is not necessarily the one
// addressed: patching a live app auto-forks a draft, exactly as a file write
// does. Callers that care which row they changed must read the returned id.
func (c *Client) UpdateDataApp(ctx context.Context, id string, patch DataAppPatch) (*DataApp, error) {
	var out DataApp
	if err := c.Do(ctx, "PUT", "dataapp/"+url.PathEscape(id), patch, &out); err != nil {
		return nil, err
	}
	out.Normalize()
	return &out, nil
}

// CheckoutDataApp forks a live data app into the caller's own draft, optionally
// overlaying a metadata + allowlist patch in the same call.
//
// Idempotent on the draft: a caller who already has one gets it back. The
// overlay is what makes a first push one request instead of two, and the server
// re-checks every bound id in it against the caller's access.
func (c *Client) CheckoutDataApp(ctx context.Context, id string, patch DataAppPatch) (*DataApp, error) {
	var out DataApp
	if err := c.Do(ctx, "POST", "dataapp/"+url.PathEscape(id)+"/checkout", patch, &out); err != nil {
		return nil, err
	}
	out.Normalize()
	return &out, nil
}

// PutDataAppFile creates or replaces one file, returning the saved row plus the
// recompile's diagnostics when it failed. See DataAppFileSaveResponse.
//
// SLOW: every write recompiles the whole bundle with esbuild and uploads a fresh
// bundle to S3, so this is seconds of real work rather than a row write.
func (c *Client) PutDataAppFile(ctx context.Context, id, path, content string) (*DataAppFileSaveResponse, error) {
	var out DataAppFileSaveResponse
	body := struct {
		Content string `json:"content"`
	}{Content: content}
	if err := c.doSlow(ctx, "PUT", dataAppFilePath(id, path), body, &out); err != nil {
		return nil, err
	}
	return &out, nil
}

// GetDataAppFile reads one file back by path, answering 404 when the row holds
// none.
//
// It lives on the write side because it exists for the write side: a PUT that
// died on a TIMEOUT may still have committed, and the only way to find out which
// is to ask. Note that it is subject to the same silent draft resolution as
// ListDataAppFiles — pass the id you mean.
func (c *Client) GetDataAppFile(ctx context.Context, id, path string) (*DataAppFile, error) {
	var out DataAppFile
	if err := c.Do(ctx, "GET", dataAppFilePath(id, path), nil, &out); err != nil {
		return nil, err
	}
	return &out, nil
}

// DeleteDataAppFile removes one file. The server refuses to delete the
// entrypoint; its message is passed through rather than second-guessed.
func (c *Client) DeleteDataAppFile(ctx context.Context, id, path string) (*DataAppFileDeleteResponse, error) {
	var out DataAppFileDeleteResponse
	if err := c.doSlow(ctx, "DELETE", dataAppFilePath(id, path), nil, &out); err != nil {
		return nil, err
	}
	return &out, nil
}

// ValidateDataApp recompiles a draft and stamps it validated, which
// CommitDataAppDraft refuses without. Takes the DRAFT id.
//
// This is the authoritative answer to "does this compile": it always recompiles
// from the row's current files, so a draft left un-validated by a failed file
// write is re-checked here rather than trusted.
//
// SLOW for the same reason PutDataAppFile is.
func (c *Client) ValidateDataApp(ctx context.Context, draftID string) (*DataApp, error) {
	var out DataApp
	if err := c.doSlow(ctx, "POST", "dataapp/"+url.PathEscape(draftID)+"/validate", nil, &out); err != nil {
		return nil, err
	}
	out.Normalize()
	return &out, nil
}

// DataAppCommitOutcome is the body of POST /dataapp/:id/commit, mirroring
// rdataapp.CommitOutcome.
//
// It exists because the route has TWO successful endings and they are not the
// same event: a commit either publishes, or — for a non-admin publishing a
// brand-new app into a shared feature — raises a proposal an admin must
// approve. `proposed` is the only thing on the wire that tells them apart, so a
// caller that ignores this body necessarily reports one of them wrongly.
type DataAppCommitOutcome struct {
	DataAppID    string `json:"dataAppID"`
	FirstPublish bool   `json:"firstPublish"`
	Proposed     bool   `json:"proposed"`
}

// CommitDataAppDraft applies a draft onto its parent — or, for a never-published
// app the caller may not publish itself, submits it for an admin's approval.
// Takes the DRAFT id. Read the returned outcome before reporting a publish.
func (c *Client) CommitDataAppDraft(ctx context.Context, draftID string) (*DataAppCommitOutcome, error) {
	var out DataAppCommitOutcome
	if err := c.Do(ctx, "POST", "dataapp/"+url.PathEscape(draftID)+"/commit", nil, &out); err != nil {
		return nil, err
	}
	return &out, nil
}

// DiscardDataAppDraft deletes a draft, throwing its uncommitted changes away.
// The parent live app is untouched.
func (c *Client) DiscardDataAppDraft(ctx context.Context, draftID string) error {
	return c.Do(ctx, "POST", "dataapp/"+url.PathEscape(draftID)+"/discard", nil, nil)
}

// DeleteDataApp marks a data app for deletion. Unlike DiscardDataAppDraft this
// removes the APP, so it is only ever reached through an explicit opt-in.
//
// The one case the CLI needs it for is a parentless draft — an app created by a
// first push and then abandoned, typically because that push failed to compile.
// Discard cannot express it (there is no parent for the row to fall back to),
// and without this the only remaining answer is the web UI, which is no answer
// at all for a caller that has nothing but a terminal.
func (c *Client) DeleteDataApp(ctx context.Context, id string) error {
	return c.Do(ctx, "DELETE", "dataapp/"+url.PathEscape(id), nil, nil)
}

// RequestDataAppReview submits the caller's own edit draft for admin review.
//
// Unlike the workflow equivalent this lives on the data-app handler itself
// rather than the governance one, so the path has no /draft/ segment.
func (c *Client) RequestDataAppReview(ctx context.Context, draftID string) error {
	return c.Do(ctx, "POST", "dataapp/"+url.PathEscape(draftID)+"/request-review", nil, nil)
}

// dataAppFilePath builds a file route, escaping each segment but keeping the
// separators — the route is a gin wildcard, so the path's own slashes are part
// of it rather than something to encode away.
func dataAppFilePath(id, path string) string {
	segments := strings.Split(path, "/")
	for i, s := range segments {
		segments[i] = url.PathEscape(s)
	}
	return "dataapp/" + url.PathEscape(id) + "/files/" + strings.Join(segments, "/")
}
