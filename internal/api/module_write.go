package api

// The WRITE half of the module surface: create, metadata patch, file sync, and
// the lifecycle verbs a dev loop needs (checkout, commit, discard).
//
// Split from module.go for the reason workflow_write.go is split from
// workflow.go: these are the calls that change something, and every one of them
// is a place where a wrong json tag is silently destructive rather than merely
// blank. Same hand-mirror rule — every tag below is copied from the server
// struct named in its comment.
//
// ⚠️ There is deliberately no VALIDATE call here, and its absence is a fact
// about the primitive rather than a gap. A workflow's validate endpoint exists
// to resolve `{{ ref }}` / `{{ secret }}` / `{{ agent }}` markers against the
// author's access before a save; a module refuses EVERY Ronja marker at save
// (plan §3.3), so there is nothing to resolve and nothing an extra round trip
// could tell the author that the local guards do not. `module validate` is
// therefore local-only — see runModuleValidate.

import (
	"context"
	"net/url"
	"strings"
)

// CreateModuleInput is the body of POST /module, mirroring rmodule.CreateInput.
//
// There is deliberately NO entrypoint field: a module is a package, not a
// program. The server seeds an `__init__.py` in the create transaction, so
// `import <name>` resolves from the first version — which is why the first push
// reads the row's files before writing (the create is not "empty") exactly as
// the workflow loop does.
type CreateModuleInput struct {
	FeatureID string `json:"featureID"`
	// Name is the Python package consumers import. Required, and validated
	// server-side: a valid identifier, unique among live modules, and not a
	// shadow of a stdlib/runtime module.
	Name        string `json:"name"`
	Title       string `json:"title"`
	Description string `json:"description,omitempty"`
}

// ModulePatch is the body of PUT /module/:id, mirroring the subset of
// rmodule.Patch the CLI writes.
//
// Both fields omitempty: every field of the server's Patch is an optional.V, so
// an absent key means "unchanged" while an explicit empty string would CLEAR
// the value. A folder whose manifest has lost its title must not be able to
// blank the module's — and an empty NAME is refused outright server-side, since
// a module without a package name is not importable.
//
// ⚠️ PUT /module/:id takes the LIVE module's id and refuses a draft id — a
// module's name is claimed against the live-uniqueness index and a draft has no
// claim to make — which is where the two loops diverge. `wf push` patches ONE
// row, the draft, and the commit publishes it. `module push` has to patch TWO:
// the live row through this body, and the open draft through
// CheckoutModule/DraftEdit. Patching only the live row is what made a title
// edit followed by a publish REVERT the title, because the commit overwrites
// the parent's name and title with the draft's copies (rmodule's
// commitDraftInTx). This one type is sent to both, so they cannot disagree.
type ModulePatch struct {
	Name  string `json:"name,omitempty"`
	Title string `json:"title,omitempty"`
}

// Empty reports a patch that would change nothing, so the caller can skip the
// round trip rather than send `{}`.
func (p ModulePatch) Empty() bool { return p.Name == "" && p.Title == "" }

// ModuleFileSaveResponse is what PUT :id/files/*path answers with: the saved
// file itself.
//
// No Warnings field, unlike the workflow's FileSaveResponse, and the absence is
// the marker-free rule showing through: a workflow's soft save-time notices are
// all about bindings it filtered out, and a module has no bindings to filter.
type ModuleFileSaveResponse = ModuleFile

// CreateModule creates a module inside a feature.
//
// The row it returns is a parentless DRAFT in a private feature — hidden until
// published, which is why the first push needs no separate checkout — and a
// PROPOSAL in a shared one, because a module edit fans out to every workflow
// that imports it and gets at least the review a single workflow edit gets.
// Both are rows this loop can write files into, and `Lifecycle` on the returned
// row is what says which happened.
func (c *Client) CreateModule(ctx context.Context, in CreateModuleInput) (*Module, error) {
	var out Module
	if err := c.Do(ctx, "POST", "module", in, &out); err != nil {
		return nil, err
	}
	return &out, nil
}

// UpdateModule patches a LIVE module's metadata and answers with the row.
func (c *Client) UpdateModule(ctx context.Context, id string, patch ModulePatch) (*Module, error) {
	var out Module
	if err := c.Do(ctx, "PUT", "module/"+url.PathEscape(id), patch, &out); err != nil {
		return nil, err
	}
	return &out, nil
}

// CheckoutModule gets-or-creates the open draft of a live module and applies a
// metadata patch to it. Idempotent in both halves: calling it twice returns the
// same draft, and a patch the draft already satisfies changes nothing.
//
// `id` names the LIVE module, never the draft — the route is
// rmodule.DraftEdit(parentID, DraftEditInput), whose body carries the same
// `name`/`title` keys ModulePatch spells, with the same absent-means-unchanged
// optional semantics. So one type serves both calls, and it is the same type
// on purpose: the two rows must be patched with the SAME values.
//
// ⚠️ Pass an EMPTY patch when checking out ahead of the drift guard. The guard
// compares the manifest against the LIVE row, and seeding a draft with a title
// nothing has compared yet would apply an edit the guard was about to refuse.
// The push loop patches metadata in one place, after the guard, and reaches the
// draft through this call there — see runModulePush step 6.
func (c *Client) CheckoutModule(ctx context.Context, id string, patch ModulePatch) (*Module, error) {
	var out Module
	if err := c.Do(ctx, "POST", "module/"+url.PathEscape(id)+"/checkout", patch, &out); err != nil {
		return nil, err
	}
	return &out, nil
}

// putModuleFileInput is the body of PUT /module/:id/files/*path, mirroring
// module.FileWriteInput in backend/api/v2/module/handler.go.
//
// BaseSha256 is a POINTER for the reason the server's is: the field has THREE
// meanings and a plain string carries two.
//
//	nil           no precondition — the write always applies
//	Ptr("")       assert the file does NOT exist yet (a create that must not clobber)
//	Ptr(<64 hex>) assert the stored content hashes to exactly this
//
// The digest is sha256 over the raw bytes, lowercase hex — the same function
// wfdir.Hash computes, which is what lets the CLI send its local sync
// baseline's hashes as preconditions. The server's rmodule.ContentSHA256 is
// byte-identical to the workflow one for exactly this reason; changing either
// without the other turns every push into a 409 storm.
type putModuleFileInput struct {
	Content    string  `json:"content"`
	BaseSha256 *string `json:"baseSha256,omitempty"`
}

// deleteModuleFileInput is the OPTIONAL body of DELETE
// /module/:id/files/*path — same three-state pointer as the PUT above.
type deleteModuleFileInput struct {
	BaseSha256 *string `json:"baseSha256,omitempty"`
}

// PutModuleFile creates or replaces one file in a draft or proposed module.
//
// A failed precondition is a 409 and the write is rolled back — the file
// certainly keeps its previous content. The server also refuses a non-`.py`
// path and anything over the per-file / per-module caps; those messages are
// passed through rather than second-guessed.
//
// doSlow rather than Do, matching PutWorkflowFile: the save re-derives nothing
// for a module (there are no markers), but it does run the whole file-content
// rule set inside one transaction under the module's row lock, and holding it
// to a plain read's deadline is how a contended module produces spurious
// timeouts on a write that landed.
func (c *Client) PutModuleFile(ctx context.Context, id, path, content string, baseSha256 *string) (*ModuleFileSaveResponse, error) {
	var out ModuleFileSaveResponse
	body := putModuleFileInput{Content: content, BaseSha256: baseSha256}
	if err := c.doSlow(ctx, "PUT", moduleFilePath(id, path), body, &out); err != nil {
		return nil, err
	}
	return &out, nil
}

// GetModuleFile reads one file back by path, answering 404 when the row holds
// none.
//
// It lives on the write side because it exists for the write side: a PUT that
// died on a TIMEOUT may still have committed, and the only way to find out
// which is to ask. Nothing else reads a single file.
func (c *Client) GetModuleFile(ctx context.Context, id, path string) (*ModuleFile, error) {
	var out ModuleFile
	if err := c.Do(ctx, "GET", moduleFilePath(id, path), nil, &out); err != nil {
		return nil, err
	}
	return &out, nil
}

// DeleteModuleFile removes one file from a draft.
//
// The server refuses to delete `__init__.py` — it is what makes the module an
// importable package — and its message says to empty it instead. Passed through
// rather than paraphrased, for the reason the workflow entrypoint refusal is.
//
// baseSha256 asserts what is being deleted, and nil sends NO BODY AT ALL rather
// than `{}`: an unconditional delete has no reason to send one.
func (c *Client) DeleteModuleFile(ctx context.Context, id, path string, baseSha256 *string) error {
	var body any
	if baseSha256 != nil {
		body = deleteModuleFileInput{BaseSha256: baseSha256}
	}
	return c.Do(ctx, "DELETE", moduleFilePath(id, path), body, nil)
}

// ModuleCommitInput is the body of POST /module/:id/commit, mirroring
// module.CommitInput.
//
// An empty body is the normal case and means "no override": the commit then
// requires the parent to still be at the version this draft was seeded from,
// and is refused with a 409 when it is not.
//
// ConfirmHeadVersionID acknowledges the specific committed version this commit
// will OVERWRITE. It must equal the head AT COMMIT TIME, not merely be
// non-empty — a caller who learns the head, deliberates, and commits while a
// third version lands gets another 409. That re-refusal is the feature, which
// is why nothing here retries.
type ModuleCommitInput struct {
	ConfirmHeadVersionID string `json:"confirmHeadVersionID,omitempty"`
}

// ModuleCommitOutcome mirrors rmodule.CommitOutcome — what a commit did.
type ModuleCommitOutcome struct {
	// ModuleID is the id the module is known by afterwards: the parent's for an
	// edit, the draft's own for a first publish.
	ModuleID string `json:"moduleID"`
	// ParentAttached distinguishes the two.
	ParentAttached bool `json:"parentAttached"`
	// HeadVersionID is the module's committed head AFTER the commit — the
	// anchor this loop writes straight into ronja.lock.json, with no second
	// round trip to read it back.
	//
	// Best-effort SERVER-SIDE: the commit's guarantee does not include it, so an
	// empty value means "the server could not read it back", never "there is no
	// head". Empty therefore leaves the anchor alone rather than clearing it.
	HeadVersionID string `json:"headVersionID"`
	// DependentWorkflowCount is how many LIVE workflows import this module —
	// the blast radius of what was just published, counted by the server inside
	// the same access boundary the commit ran under.
	//
	// It is here rather than computed by the CLI because a count assembled
	// client-side would be a second, weaker answer to a question the server
	// already owns: this is the delete gate's own reverse lookup, read through
	// the RLS-scoped pool, so it is TENANT-scoped — the same set the delete gate
	// refuses on, which the caller may well not be able to enumerate. The
	// publish report spells out what the number MEANS — a pin does not move
	// until the consumer is itself republished — which is the half a bare count
	// would let a reader guess wrong.
	//
	// Best-effort, like HeadVersionID: 0 can mean "none" or "the count failed",
	// and the report says nothing at all for 0 either way, so the two collapse
	// harmlessly. It is also CAPPED at 100 server-side, like every reverse
	// lookup, so a module imported by more workflows than that reports the cap.
	DependentWorkflowCount int `json:"dependentWorkflowCount"`
}

// CommitModuleDraft applies a draft onto its parent, or publishes a parentless
// one in place. Takes the DRAFT id.
//
// confirmHeadVersionID is empty for an ordinary commit, which sends no body at
// all. Supply it only after a 409 has named the version being overwritten, and
// only on a deliberate override (`module publish --overwrite-remote`).
func (c *Client) CommitModuleDraft(ctx context.Context, draftID, confirmHeadVersionID string) (*ModuleCommitOutcome, error) {
	var body any
	if confirmHeadVersionID != "" {
		body = ModuleCommitInput{ConfirmHeadVersionID: confirmHeadVersionID}
	}
	var out ModuleCommitOutcome
	if err := c.Do(ctx, "POST", "module/"+url.PathEscape(draftID)+"/commit", body, &out); err != nil {
		return nil, err
	}
	return &out, nil
}

// DiscardModuleDraft deletes a draft, throwing its uncommitted changes away.
// The live module is untouched.
func (c *Client) DiscardModuleDraft(ctx context.Context, draftID string) error {
	return c.Do(ctx, "POST", "module/"+url.PathEscape(draftID)+"/discard", nil, nil)
}

// WithdrawModuleProposal takes back a proposal the caller filed — the module
// equivalent of discarding a parentless draft in a SHARED feature, where a
// brand-new module is created `proposed` rather than `draft`.
func (c *Client) WithdrawModuleProposal(ctx context.Context, proposalID string) error {
	return c.Do(ctx, "POST", "module/proposal/"+url.PathEscape(proposalID)+"/withdraw", nil, nil)
}

// DeleteModule soft-deletes a module — the row goes to the trash and survives a
// retention window before the janitor purges it.
//
// REFUSED while any live workflow still imports the module, which is the delete
// gate doing its job; the server's message names the workflows and the
// `{{ module }}` markers to remove. The one case this loop needs it for is a
// parentless draft — a module created by a first push and then abandoned —
// exactly as `wf discard --delete-workflow` does.
func (c *Client) DeleteModule(ctx context.Context, id string) error {
	return c.Do(ctx, "DELETE", "module/"+url.PathEscape(id), nil, nil)
}

// RequestModuleReview submits a draft for admin review.
//
// Unlike the workflow's counterpart this route lives on the module handler
// itself (`POST /module/draft/:id/request-review`), not on the governance
// handler — the approval half of a module lives beside the module.
func (c *Client) RequestModuleReview(ctx context.Context, draftID string) error {
	return c.Do(ctx, "POST", "module/draft/"+url.PathEscape(draftID)+"/request-review", nil, nil)
}

// ListModuleVersions reads a module's committed version history, newest first.
// The server orders it the same way the commit CAS resolves the head, so
// element 0 is the head rather than merely a recent version.
//
// An EMPTY list means the module has never been committed.
func (c *Client) ListModuleVersions(ctx context.Context, parentID string) ([]Module, error) {
	var out []Module
	if err := c.Do(ctx, "GET", "module/"+url.PathEscape(parentID)+"/versions", nil, &out); err != nil {
		return nil, err
	}
	return out, nil
}

// ModuleHeadVersionID resolves the CAS anchor for a live module: the id of its
// most recently committed version, or — when it has never been versioned — the
// module's OWN id.
//
// The parent-id fallback mirrors rmodule's own head resolution, exactly as
// HeadVersionID does for a workflow: an unversioned module's drafts are seeded
// from the live row itself, so the live row IS the anchor.
//
// Deliberately NOT parsed out of the 409 message. The HTTP error body is the
// flat app-wide `{"error": "<prose>"}` shape with no structured payload, and a
// client that regexes an id out of prose starts silently overwriting the wrong
// version the day somebody rewords it.
func (c *Client) ModuleHeadVersionID(ctx context.Context, parentID string) (string, error) {
	versions, err := c.ListModuleVersions(ctx, parentID)
	if err != nil {
		return "", err
	}
	if len(versions) == 0 {
		return parentID, nil
	}
	return versions[0].ID, nil
}

// moduleFilePath builds a file route, escaping each segment but keeping the
// separators — the route is a gin wildcard, so the path's own slashes are part
// of it rather than something to encode away.
func moduleFilePath(id, path string) string {
	segments := strings.Split(path, "/")
	for i, s := range segments {
		segments[i] = url.PathEscape(s)
	}
	return "module/" + url.PathEscape(id) + "/files/" + strings.Join(segments, "/")
}
