package api

import (
	"context"
	"net/url"
	"sort"
	"time"
)

// DataApp is a HAND-MIRROR of the subset of rdb.DataApp the CLI reads.
//
// Same rule and same hazard as Workflow in workflow.go: a json tag that does not
// exist on the wire decodes to the zero value SILENTLY, so a typo here shows up
// as an empty column of output — or as an allowlist the CLI believes is empty
// and then pushes as empty, revoking the app's access to its own tables. Every
// tag below is copied from backend/ronja/rdb/table_data_app.go; keep them in
// step with that file, and see dataapp_test.go for the fixture that checks them.
//
// The lifecycle vocabulary is shared with workflows (rdb.DataAppLifecycle
// carries the same five values as rdb.WorkflowLifecycle), so the Lifecycle*
// constants and DescribeLifecycle are reused rather than duplicated.
//
// Mirroring decisions specific to this row:
//
//   - The server's optional.V[T] marshals as the bare value or JSON `null`, and
//     unmarshalling `null` into a non-pointer is a documented no-op — so optional
//     STRINGS are plain strings ("" is exactly what "unset" means for an id)
//     while optional TIMESTAMPS are pointers, because "has this been validated at
//     all" is the question the whole publish path turns on.
//   - The allowlist slices carry NO omitempty server-side (they are plain
//     []string columns), so they arrive as [] rather than absent. Normalize
//     still runs, because a nil from a hand-written fixture or an older instance
//     would otherwise range differently from an empty one.
type DataApp struct {
	ID        string `json:"id"`
	FeatureID string `json:"featureID"`

	Name        string `json:"name"`
	Description string `json:"description"`
	// Entrypoint names the file the bundle compiles from. Reported, never
	// pushed: rdataapp.Patch has no entrypoint field, so a data app's is fixed
	// at "App.tsx" from the moment the row is created.
	Entrypoint string `json:"entrypoint"`
	// BundleFileKey is the compiled bundle in S3, empty on an app that has never
	// compiled. Read as a FACT ABOUT THE PAST only — a failed compile leaves the
	// previous key in place, so a non-empty value never means "the current source
	// compiles". ValidatedAt is the field that answers that.
	BundleFileKey string `json:"bundleFileKey"`
	// ValidatedAt is cleared by every file mutation and re-stamped only by a
	// clean compile. It is the single gate POST :id/commit enforces, so it is
	// also what `ronja app publish` checks before it tries.
	ValidatedAt *time.Time `json:"validatedAt"`

	DataAppAccess

	// ParentDataAppID is empty on a live row AND on a parentless draft (an app
	// created by POST /dataapp and not yet published, since migration 000495),
	// and set on an edit shadow or a committed version snapshot. So it — not
	// the lifecycle, which reads "draft" for both — is what tells a never-
	// published app apart from somebody's open edits to a live one.
	ParentDataAppID string     `json:"parentDataAppID"`
	CommittedAt     *time.Time `json:"committedAt"`

	// Lifecycle is trigger-computed server-side: live|draft|version|archived|proposed.
	Lifecycle string `json:"lifecycle"`
	// SubmittedForReviewAt is set once a drafter asks an admin to commit their
	// draft; null on every other row, and on a draft they have not submitted.
	SubmittedForReviewAt *time.Time `json:"submittedForReviewAt"`
	DrafterUserID        string     `json:"drafterUserID"`
	// BaseVersionID is what the DRAFT was forked from — and ⚠️ it does NOT mean
	// what the identically-named field on Workflow means, which is why
	// `app clone` resolves its anchor through dataAppCloneAnchor rather than
	// through cloneAnchor.
	//
	// Two writers, two meanings:
	//
	//   - An ordinary checkout (rdataapp.buildDraftFromParent) stamps
	//     optional.Value(parent.ID) — the LIVE APP'S OWN ID — for every draft,
	//     whether or not the app has committed versions. rworkflow stamps the
	//     head VERSION row's id in the same place. So on a versioned app this
	//     value is not a version at all, and comparing it with what
	//     DataAppHeadVersionID answers can only ever fail.
	//   - RestoreFromVersion stamps the real version id being restored, which is
	//     the workflow meaning.
	//
	// The two are told apart by the value: equal to the app's identity id means
	// the parent-id sentinel, anything else is a genuine version snapshot.
	//
	// Null on every other row.
	BaseVersionID string `json:"baseVersionID"`

	CreatedBy string    `json:"createdBy"`
	CreatedAt time.Time `json:"createdAt"`
	UpdatedAt time.Time `json:"updatedAt"`

	// URL is the absolute frontend /apps/<id> page, stamped by the SERVER
	// (api/v2/dataapp.DataAppView) on the single-row routes. Never built here —
	// see the same field on Workflow for why, and for why an empty value is
	// normal, silent, and must not be filled in with a guess.
	URL string `json:"url,omitempty"`
}

// DataAppAccess is a data app's capability surface: what its bundle may reach.
//
// One type serving three roles on purpose — it is embedded in the row, it is the
// manifest's `access` block, and it is what the patch sends. A second struct
// would only add a conversion that silently drops whatever field was added to
// one side and not the other, and this is the type where a dropped field means a
// REVOKED capability rather than a cosmetic gap.
//
// Its json tags are therefore part of the ronja.json FILE FORMAT as well as the
// wire, and renaming one is a breaking change to committed manifests.
//
// These are grants, not descriptions: an id here is a capability the app's
// viewers inherit, which is why the server re-checks every one of them against
// the pusher's own access (governance.ValidateDataAppScope) rather than trusting
// the file.
type DataAppAccess struct {
	AllowedTableIDs    []string `json:"allowedTableIDs"`
	AllowedSecretIDs   []string `json:"allowedSecretIDs"`
	AllowedAgentIDs    []string `json:"allowedAgentIDs"`
	AllowedWorkflowIDs []string `json:"allowedWorkflowIDs"`
	AllowedCodexIDs    []string `json:"allowedCodexIDs"`
	AllowedMetricIDs   []string `json:"allowedMetricIDs"`
	// Capabilities are the coarse switches the minted script-token carries
	// alongside the id allowlists. Only four names ever belong here (ai,
	// query_external, write_external, upload_file) — everything else is
	// derived server-side from the allowlists. The list lives in
	// commands.declarableCapabilities, which `app push` refuses against;
	// `ronja app init --help` describes each one.
	Capabilities []string `json:"capabilities"`
}

// accessSlots returns every list in a stable order, so normalization, equality
// and rendering all walk the same seven fields. Adding a slot means adding it
// here once rather than in four places, and forgetting it in one of those four
// is how an allowlist silently stops being compared.
func (a *DataAppAccess) accessSlots() []*[]string {
	return []*[]string{
		&a.AllowedTableIDs, &a.AllowedSecretIDs, &a.AllowedAgentIDs,
		&a.AllowedWorkflowIDs, &a.AllowedCodexIDs, &a.AllowedMetricIDs,
		&a.Capabilities,
	}
}

// Normalized returns a copy with nil lists replaced by empty ones and every list
// sorted and de-duplicated.
//
// Sorting is what makes SameAccess a comparison of MEANING rather than of
// spelling. An allowlist is a set — the server stores it as one and the token
// mint reads it as one — so a manifest listing two tables in the other order is
// the same grant, and treating it as drift would make `app status` report a
// change forever and `app push` send a patch that changes nothing.
func (a DataAppAccess) Normalized() DataAppAccess {
	out := a
	for _, slot := range out.accessSlots() {
		*slot = normalizeIDs(*slot)
	}
	return out
}

// IsEmpty reports an access set that grants nothing.
func (a DataAppAccess) IsEmpty() bool {
	n := a.Normalized()
	for _, slot := range n.accessSlots() {
		if len(*slot) > 0 {
			return false
		}
	}
	return true
}

// normalizeIDs sorts, de-duplicates and never returns nil.
func normalizeIDs(ids []string) []string {
	out := make([]string, 0, len(ids))
	seen := make(map[string]bool, len(ids))
	for _, id := range ids {
		if id == "" || seen[id] {
			continue
		}
		seen[id] = true
		out = append(out, id)
	}
	sort.Strings(out)
	return out
}

// SameAccess compares two access sets for sync purposes, order-insensitively.
func SameAccess(a, b DataAppAccess) bool {
	na, nb := a.Normalized(), b.Normalized()
	sa, sb := na.accessSlots(), nb.accessSlots()
	for i := range sa {
		if len(*sa[i]) != len(*sb[i]) {
			return false
		}
		for j := range *sa[i] {
			if (*sa[i])[j] != (*sb[i])[j] {
				return false
			}
		}
	}
	return true
}

// DataAppFile mirrors rdb.DataAppFile — one source file of one data-app row.
// There is no content hash on the wire, so the CLI hashes Content itself.
type DataAppFile struct {
	ID        string    `json:"id"`
	DataAppID string    `json:"dataAppID"`
	Path      string    `json:"path"`
	Content   string    `json:"content"`
	CreatedAt time.Time `json:"createdAt"`
	UpdatedAt time.Time `json:"updatedAt"`
}

// Normalize replaces nil allowlists with empty ones.
func (d *DataApp) Normalize() {
	if d == nil {
		return
	}
	d.DataAppAccess = d.DataAppAccess.Normalized()
}

// IsValidated reports whether the row's current source has compiled cleanly
// since its last file edit — the gate POST :id/commit enforces.
func (d *DataApp) IsValidated() bool {
	return d != nil && d.ValidatedAt != nil && !d.ValidatedAt.IsZero()
}

// IdentityID is the STABLE id of the data app this row belongs to: the parent
// for a draft or a version snapshot, the row itself for a live app.
//
// This is what a manifest binding records. A draft's own id is per-user and
// disappears on commit, so a committed file naming one would point at a row that
// stops existing the moment its author publishes.
func (d *DataApp) IdentityID() string {
	if d == nil {
		return ""
	}
	if d.ParentDataAppID != "" {
		return d.ParentDataAppID
	}
	return d.ID
}

// GetDataApp reads a data app's metadata. 404s for an id the caller cannot
// reach — the read gate does not distinguish absent from invisible. An id
// matching nothing at all lands there too on a current backend; an older one
// answers it `400 {"error":"no rows"}`, and callers accept both.
func (c *Client) GetDataApp(ctx context.Context, id string) (*DataApp, error) {
	var out DataApp
	if err := c.Do(ctx, "GET", "dataapp/"+url.PathEscape(id), nil, &out); err != nil {
		return nil, err
	}
	out.Normalize()
	return &out, nil
}

// GetDataAppDraft reads the CALLER'S OWN open draft of a data app, returning
// (nil, nil) when they have none.
//
// The endpoint answers 200 with a JSON `null` body rather than 404 in that case,
// so decoding into a **DataApp is not a stylistic choice: decoding into a struct
// would turn "no draft" into a zero-value DataApp with an empty id, and every
// caller would then act on a draft that does not exist.
func (c *Client) GetDataAppDraft(ctx context.Context, id string) (*DataApp, error) {
	var out *DataApp
	if err := c.Do(ctx, "GET", "dataapp/"+url.PathEscape(id)+"/draft", nil, &out); err != nil {
		return nil, err
	}
	out.Normalize()
	return out, nil
}

// ListDataAppFiles reads every file of ONE data-app row — exactly the row whose
// id is passed. A draft is its own address: the server never substitutes the
// caller's open draft for a live id (it once did, silently, which is why every
// caller in this CLI resolves the draft explicitly with GetDataAppDraft and
// passes the id it actually means). A baseline is a claim about a specific row,
// so naming the row is what keeps `status` comparing the right two things.
func (c *Client) ListDataAppFiles(ctx context.Context, id string) ([]DataAppFile, error) {
	var out []DataAppFile
	if err := c.Do(ctx, "GET", "dataapp/"+url.PathEscape(id)+"/files", nil, &out); err != nil {
		return nil, err
	}
	return out, nil
}
