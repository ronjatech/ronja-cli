package api

import (
	"context"
	"net/url"
	"time"
)

// Module is a HAND-MIRROR of the subset of rdb.CodeModule the CLI reads.
//
// Same rule and same hazard as Workflow: a json tag that does not exist on the
// wire decodes to the zero value SILENTLY. Every tag below is copied from
// backend/ronja/rdb/table_code_module.go; keep them in step with that file.
//
// The lifecycle vocabulary is SHARED with workflows — the server computes both
// from the same trigger shape (live|draft|version|archived|proposed) — so this
// package's Lifecycle* constants and DescribeLifecycle apply here unchanged
// rather than being spelled a second time. A module's `proposed` is a
// brand-new module awaiting admin approval, exactly as a workflow proposal is.
//
// The server's optional.V[T] marshals as the bare value or JSON `null`, and
// unmarshalling `null` into a non-pointer is a documented no-op — so optional
// STRINGS are plain strings here ("" is exactly what "unset" means for an id)
// while the one optional TIMESTAMP the CLI reads is a pointer, because "is
// there a submitted-for-review stamp at all" is a question it genuinely asks.
//
// ⚠️ There is NO `url` field, unlike Workflow and DataApp. api/v2/module's
// ModuleView is `rdb.CodeModule` itself rather than a projection, so the server
// stamps no frontend link on it. That is why nothing in the module loop calls
// printResourceURL: the rule is "links come from the server", and this server
// gives none — a locally-templated one would point at the API origin, which
// serves no pages.
type Module struct {
	ID        string `json:"id"`
	FeatureID string `json:"featureID"`

	// Name is the Python package a consumer workflow imports — the module's one
	// piece of load-bearing metadata. Unique per tenant among live modules, and
	// refused when it shadows a stdlib or runtime-baseline module.
	Name        string `json:"name"`
	Title       string `json:"title"`
	Description string `json:"description"`

	// ParentModuleID is empty for a live row AND for a parentless draft (a
	// brand-new module nobody has published yet). Non-empty on an edit draft
	// and on a committed version snapshot.
	ParentModuleID string `json:"parentModuleID"`
	// BaseVersionID is the committed version a DRAFT was forked from. Empty on
	// every row that is not an open draft, and on a draft of a module that has
	// never been versioned — so empty means "no anchor" rather than "no such
	// version". Read by `module clone` for the same reason `wf clone` reads the
	// workflow field: a draft forked before somebody else published sits on an
	// OLDER version, and recording the newer one would claim to have seen a
	// publish this folder does not contain.
	BaseVersionID string `json:"baseVersionID"`

	// Lifecycle is trigger-computed server-side: live|draft|version|archived|proposed.
	Lifecycle string `json:"lifecycle"`
	// SubmittedForReviewAt is set once a drafter asks an admin to commit their
	// draft; null on every other row, and on a draft they have not submitted.
	SubmittedForReviewAt *time.Time `json:"submittedForReviewAt"`
	DrafterUserID        string     `json:"drafterUserID"`

	// FeatureScope is the joined feature's scope (private|workspace|
	// organization), stamped at read time. The field to branch on for the
	// shared-vs-private publish route, exactly as on a workflow.
	FeatureScope string `json:"featureScope"`

	CreatedAt time.Time `json:"createdAt"`
	UpdatedAt time.Time `json:"updatedAt"`
}

// ModuleFile mirrors rdb.CodeModuleFile — one source file of one module row.
// There is no content hash on the wire, so the CLI hashes Content itself.
type ModuleFile struct {
	ID        string    `json:"id"`
	ModuleID  string    `json:"moduleID"`
	Path      string    `json:"path"`
	Content   string    `json:"content"`
	CreatedAt time.Time `json:"createdAt"`
	UpdatedAt time.Time `json:"updatedAt"`
}

// IdentityID is the STABLE id of the module this row belongs to: the parent for
// an edit draft or a version snapshot, the row itself for a live module or a
// parentless draft.
//
// This is what a manifest binding records, for the reason Workflow.IdentityID
// gives: a draft's own id disappears on commit, and a committed file naming one
// would point at a row that stops existing the moment its author publishes.
func (m *Module) IdentityID() string {
	if m == nil {
		return ""
	}
	if m.ParentModuleID != "" {
		return m.ParentModuleID
	}
	return m.ID
}

// GetModule reads a module's metadata. 404s for an id the caller cannot reach —
// the read gate does not distinguish absent from invisible.
//
// ⚠️ GET /module/:id (rmodule.GetForAgent) resolves LIVE modules plus the
// CALLER'S OWN draft or proposal — which is what lets this loop read back the
// unpublished module its own first push created in a shared feature. It does
// NOT resolve a colleague's in-flight row (404, not 403, so the row's existence
// stays private) and never resolves a committed VERSION, which is addressed
// through its own file routes.
func (c *Client) GetModule(ctx context.Context, id string) (*Module, error) {
	var out Module
	if err := c.Do(ctx, "GET", "module/"+url.PathEscape(id), nil, &out); err != nil {
		return nil, err
	}
	return &out, nil
}

// GetModuleDraft reads the open draft of a module, returning (nil, nil) when
// there is none.
//
// ⚠️ The route is a LIST (`GET :id/draft` → rmodule.ListDrafts), not the
// nullable single row `GET /workflow/:id/draft` answers, so this decodes an
// array and takes the first element. There is ONE draft per module, not one per
// author — a module draft is a snapshot of the whole file set, so two would
// each silently drop the other's edits at commit — which is what makes "the
// first element" the right reading rather than an arbitrary pick.
//
// The consequence for the sync loop is worth stating: a colleague's open draft
// is THIS folder's draft too, so a push writes into it. That is the same row
// the drift guard compares against, and the per-file preconditions are what
// stop two people overwriting each other inside it.
func (c *Client) GetModuleDraft(ctx context.Context, id string) (*Module, error) {
	var out []Module
	if err := c.Do(ctx, "GET", "module/"+url.PathEscape(id)+"/draft", nil, &out); err != nil {
		return nil, err
	}
	if len(out) == 0 {
		return nil, nil
	}
	draft := out[0]
	return &draft, nil
}

// ListModuleFiles reads every file of ONE module row — a draft has its own
// complete file set, so the id passed here decides whether you get the live
// code or the draft's.
//
// The route returns a bare JSON array ([]*rdb.CodeModuleFile), not an envelope.
func (c *Client) ListModuleFiles(ctx context.Context, id string) ([]ModuleFile, error) {
	var out []ModuleFile
	if err := c.Do(ctx, "GET", "module/"+url.PathEscape(id)+"/files", nil, &out); err != nil {
		return nil, err
	}
	return out, nil
}
