package api

import (
	"context"
	"encoding/json"
	"errors"
	"net/url"
	"strings"
	"time"
)

// The server-side state behind an ADOPTED folder (backend api/v2/managed,
// resource/rmanaged): which repository address — (the stack's feature, the
// folder's id, the folder-relative file) — manages which row.
//
// Every shape here mirrors api/v2/managed/views.go on the branch that added the
// routes. The wire codes and states are contract on the server's side ("may be
// added to but never renamed"), so this file branches on them and never on
// prose.

// ManagedClaimInput is the `managed` object a create route takes, and the
// address half of a claim item. It names WHERE in a repository a row is
// declared; the feature half of the address is never sent — it is always the
// created row's own feature.
type ManagedClaimInput struct {
	FolderID    string `json:"folderID"`
	Address     string `json:"address"`
	StackLabel  string `json:"stackLabel"`
	SourceLabel string `json:"sourceLabel"`
}

// Claim states (backend/api/v2/managed views.go, StateOK …): one per outcome
// the CLI's refresh handles (commands.refreshManaged).
const (
	ManagedStateOK          = "ok"
	ManagedStateNotReadable = "not_readable"
	ManagedStateTrashed     = "trashed"
	ManagedStateLost        = "lost"
	ManagedStateMovedOut    = "moved_out"
)

// Wire codes (views.go CodeAddressClaimed …).
const (
	ManagedCodeAddressClaimed  = "address_claimed"
	ManagedCodeResourceClaimed = "resource_claimed"
	ManagedCodeMovedOut        = "moved_out"
	ManagedCodeTrashed         = "trashed"
	ManagedCodeNotRoot         = "not_root"
	ManagedCodeStaleRevision   = "stale_revision"
	ManagedCodeNotClaimed      = "not_claimed"
	ManagedCodeNotClaimable    = "not_claimable"
	ManagedCodeInternalError   = "internal_error"
	// ManagedCodeAppliedTooLarge — a folder anchor over the server's byte cap
	// (413). Nothing was written; the server never truncates one.
	ManagedCodeAppliedTooLarge = "applied_too_large"
	// ManagedCodeNoFolderAnchor — a release of a folder that has no anchor.
	ManagedCodeNoFolderAnchor = "no_folder_anchor"
)

// Claim item outcomes (views.go OutcomeClaimed …).
const (
	ManagedOutcomeClaimed        = "claimed"
	ManagedOutcomeAlreadyClaimed = "already_claimed"
	ManagedOutcomeRefused        = "refused"
)

// ManagedClaim is one claim as the state endpoints publish it (ClaimView).
//
// The ADDRESS half is always present. The ROW half — ResourceID, ResourceName,
// Lifecycle, Applied, CurrentFeatureID — only when Readable, except ResourceID
// on a `lost` claim, whose row no longer exists to be protected. Adopt the id
// only when State is ok.
//
// A TOMBSTONE is a claim whose row a cross-feature move took away
// (DepartedAt set): it keeps its address — a create there is refused
// address_claimed — always reads moved_out, and can be neither moved nor
// anchored (409 moved_out). Unmanaging it frees the address; moving the row
// back into the claim's feature revives it, address and anchor kept. A live
// claim that raced such a move also reads moved_out, with no DepartedAt.
type ManagedClaim struct {
	ID           string    `json:"id"`
	FeatureID    string    `json:"featureID"`
	FolderID     string    `json:"folderID"`
	Address      string    `json:"address"`
	ResourceKind string    `json:"resourceKind"`
	StackLabel   string    `json:"stackLabel"`
	SourceLabel  string    `json:"sourceLabel"`
	Claimant     string    `json:"claimant"`
	UpdatedBy    string    `json:"updatedBy"`
	CreatedAt    time.Time `json:"createdAt"`
	UpdatedAt    time.Time `json:"updatedAt"`
	Revision     int64     `json:"revision"`

	Readable bool   `json:"readable"`
	State    string `json:"state"`

	ResourceID       string         `json:"resourceID,omitempty"`
	ResourceName     string         `json:"resourceName,omitempty"`
	Lifecycle        string         `json:"lifecycle,omitempty"`
	CurrentFeatureID string         `json:"currentFeatureID,omitempty"`
	DepartedAt       *time.Time     `json:"departedAt,omitempty"`
	Applied          map[string]any `json:"applied,omitempty"`
}

// Departed reports whether the claim is a tombstone.
func (c *ManagedClaim) Departed() bool { return c != nil && c.DepartedAt != nil }

// ManagedBy is the `managedBy` object a claimed row's single-row GET carries
// (backend/resource/rmanaged/managed_by.go): the
// ADDRESS half of the row's live claim — which folder of which repository
// deploys it, and on which stack — and nothing of its bookkeeping. It is
// present only for a LIVE claim (a tombstone drops it), and a draft or version
// id answers its root's. Absent on a row no repository manages, and on an
// instance older than the field, which reads the same: nothing to say.
//
// The labels are display-only and client-chosen. SourceLabel is, by the CLI's
// own convention, `<normalized remote>//<folder path>` (SplitSourceLabel).
type ManagedBy struct {
	SourceLabel string    `json:"sourceLabel"`
	StackLabel  string    `json:"stackLabel"`
	FolderID    string    `json:"folderID"`
	Address     string    `json:"address"`
	FeatureID   string    `json:"featureID"`
	UpdatedAt   time.Time `json:"updatedAt"`
}

// SplitSourceLabel splits a source label into the repository and the folder
// inside it, on the first `//` that is not a scheme's `://` — the rule the web
// app's chip reads it by (frontend-web/src/lib/atoms/managedInGit.ts,
// splitSourceLabel). A label that
// does not follow the convention — no such `//`, or an empty half — comes back
// whole as the repository, with no folder.
func SplitSourceLabel(label string) (repository, folder string) {
	from := 0
	if i := strings.Index(label, "://"); i >= 0 {
		from = i + 3
	}
	at := strings.Index(label[from:], "//")
	if at < 0 {
		return label, ""
	}
	at += from
	repository, folder = label[:at], label[at+2:]
	if repository == "" || folder == "" {
		return label, ""
	}
	return repository, folder
}

// ManagedFolderAnchor is a folder's ANCHOR (FolderAnchorView): the per-folder
// applied state that belongs to no claim — today the drift baselines of the
// folder's docs sidecars. It names no row, so nothing in it is redacted. A
// folder with no anchor reads Revision 0 and an empty Applied.
type ManagedFolderAnchor struct {
	FeatureID string         `json:"featureID"`
	FolderID  string         `json:"folderID"`
	Applied   map[string]any `json:"applied"`
	Revision  int64          `json:"revision"`
	UpdatedBy string         `json:"updatedBy,omitempty"`
	UpdatedAt *time.Time     `json:"updatedAt,omitempty"`
}

// ManagedState is one GET /managed for one folder: its claims, and its anchor.
type ManagedState struct {
	Claims []ManagedClaim
	// Folder is never nil: a server that answered none (a folder with no
	// anchor reads revision 0 and an empty applied) is the same answer.
	Folder *ManagedFolderAnchor
}

// ReadManaged reads one folder's claims in a feature AND that folder's anchor,
// in the one request the server answers both with (ClaimListView). An empty
// folder lists every folder's claims in the feature, and no anchor.
func (c *Client) ReadManaged(ctx context.Context, featureID, folderID string) (*ManagedState, error) {
	query := url.Values{}
	query.Set("feature", featureID)
	if folderID != "" {
		query.Set("folder", folderID)
	}
	var out struct {
		Claims []ManagedClaim       `json:"claims"`
		Folder *ManagedFolderAnchor `json:"folder"`
	}
	if err := c.Do(ctx, "GET", "managed?"+query.Encode(), nil, &out); err != nil {
		return nil, err
	}
	folder := out.Folder
	if folder == nil {
		folder = &ManagedFolderAnchor{FeatureID: featureID, FolderID: folderID}
	}
	if folder.Applied == nil {
		folder.Applied = map[string]any{}
	}
	return &ManagedState{Claims: out.Claims, Folder: folder}, nil
}

// ListManaged reads one folder's claims in a feature (GET /managed). An empty
// folder lists every folder's claims in the feature.
func (c *Client) ListManaged(ctx context.Context, featureID, folderID string) ([]ManagedClaim, error) {
	st, err := c.ReadManaged(ctx, featureID, folderID)
	if err != nil {
		return nil, err
	}
	return st.Claims, nil
}

// ManagedFolderAppliedInput is one PUT /managed/folder/applied. The field is
// `revision`, not `expectedRevision` as on a claim's anchor: 0 creates the
// anchor, any other value updates it only if it is still the anchor's.
type ManagedFolderAppliedInput struct {
	FeatureID string         `json:"featureID"`
	FolderID  string         `json:"folderID"`
	Applied   map[string]any `json:"applied"`
	Revision  int64          `json:"revision"`
}

// RecordManagedFolderApplied writes a folder's anchor, a compare-and-set on its
// revision: a stale one (or a create of an anchor that exists, or an update of
// one released meanwhile) is a 409 stale_revision, and an `applied` over the
// server's cap is a 413 applied_too_large with nothing written. `applied`
// replaces the previous state whole and is never sent as null — the server
// refuses that rather than read it as "clear".
func (c *Client) RecordManagedFolderApplied(ctx context.Context, in ManagedFolderAppliedInput) (*ManagedFolderAnchor, error) {
	if in.Applied == nil {
		in.Applied = map[string]any{}
	}
	var out ManagedFolderAnchor
	if err := c.Do(ctx, "PUT", "managed/folder/applied", in, &out); err != nil {
		return nil, err
	}
	if out.Applied == nil {
		out.Applied = map[string]any{}
	}
	return &out, nil
}

// ReleaseManagedFolder deletes a folder's anchor and answers it as it was. A
// folder with none is a 404 no_folder_anchor.
func (c *Client) ReleaseManagedFolder(ctx context.Context, featureID, folderID string) (*ManagedFolderAnchor, error) {
	body := map[string]any{"featureID": featureID, "folderID": folderID}
	var out ManagedFolderAnchor
	if err := c.Do(ctx, "DELETE", "managed/folder", body, &out); err != nil {
		return nil, err
	}
	return &out, nil
}

// ManagedClaimItem is one row to adopt in a POST /managed/claim.
type ManagedClaimItem struct {
	ResourceKind string `json:"resourceKind"`
	ResourceID   string `json:"resourceID"`
	ManagedClaimInput
}

// ManagedClaimResult is one item's outcome. Exactly one per requested item, in
// request order.
type ManagedClaimResult struct {
	Index   int           `json:"index"`
	Outcome string        `json:"outcome"`
	Code    string        `json:"code,omitempty"`
	Error   string        `json:"error,omitempty"`
	Claim   *ManagedClaim `json:"claim,omitempty"`
}

// MaxManagedClaimItems is the server's bound on one POST /managed/claim
// (api/v2/managed maxClaimItems).
const MaxManagedClaimItems = 200

// ClaimManaged adopts existing rows into one feature's addresses. Each item is
// authorized and committed on its own and reports its own outcome; a refusal of
// the whole request (a feature the caller cannot write) is an error.
func (c *Client) ClaimManaged(ctx context.Context, featureID string, items []ManagedClaimItem) ([]ManagedClaimResult, error) {
	body := map[string]any{"featureID": featureID, "claims": items}
	var out struct {
		Results []ManagedClaimResult `json:"results"`
	}
	if err := c.Do(ctx, "POST", "managed/claim", body, &out); err != nil {
		return nil, err
	}
	return out.Results, nil
}

// MoveManaged re-keys one claim from one address to another inside one folder
// (a `moved` declaration). A `from` holding no claim is a 404 not_claimed; a
// `to` holding a row is a 409 address_claimed.
func (c *Client) MoveManaged(ctx context.Context, featureID, folderID, from, to string) (*ManagedClaim, error) {
	body := map[string]any{"featureID": featureID, "folderID": folderID, "from": from, "to": to}
	var out ManagedClaim
	if err := c.Do(ctx, "POST", "managed/move", body, &out); err != nil {
		return nil, err
	}
	return &out, nil
}

// UnmanageManaged deletes one claim. The ROW is untouched.
func (c *Client) UnmanageManaged(ctx context.Context, claimID string) (*ManagedClaim, error) {
	var out ManagedClaim
	if err := c.Do(ctx, "DELETE", "managed/"+url.PathEscape(claimID), nil, &out); err != nil {
		return nil, err
	}
	return &out, nil
}

// ManagedAppliedInput is one PUT /managed/:id/applied. The labels are optional:
// nil keeps the claim's own.
type ManagedAppliedInput struct {
	ExpectedRevision int64          `json:"expectedRevision"`
	Applied          map[string]any `json:"applied"`
	StackLabel       *string        `json:"stackLabel,omitempty"`
	SourceLabel      *string        `json:"sourceLabel,omitempty"`
}

// RecordManagedApplied writes a claim's last-applied anchor, a compare-and-set
// on its revision: a stale one is a 409 stale_revision, and a claim unmanaged
// meanwhile is a 404.
func (c *Client) RecordManagedApplied(ctx context.Context, claimID string, in ManagedAppliedInput) (*ManagedClaim, error) {
	if in.Applied == nil {
		in.Applied = map[string]any{}
	}
	var out ManagedClaim
	if err := c.Do(ctx, "PUT", "managed/"+url.PathEscape(claimID)+"/applied", in, &out); err != nil {
		return nil, err
	}
	return &out, nil
}

// AsAddressClaimed reports whether err is the 409 `address_claimed` a create or
// a move answers when the address already holds a row, and hands back the
// claim holding it — redacted exactly as GET /managed redacts it, and nil when
// the server could not name it (released again in between).
//
// ⚠️ The server renders an error's details as TOP-LEVEL keys beside `error`
// and `code` (gt.ErrorHandler), so the holder is the body's `claim` and its id
// the body's `resourceID` — not a nested `details` object.
func AsAddressClaimed(err error) (*ManagedClaim, bool) {
	var apiErr *Error
	if !errors.As(err, &apiErr) {
		return nil, false
	}
	var body struct {
		Code  string        `json:"code"`
		Claim *ManagedClaim `json:"claim"`
	}
	if json.Unmarshal([]byte(apiErr.Body), &body) != nil || body.Code != ManagedCodeAddressClaimed {
		return nil, false
	}
	return body.Claim, true
}

// ErrorMessage is the server's `error` sentence of an HTTP error, or the
// error's own rendering when the body carries none.
func ErrorMessage(err error) string {
	var apiErr *Error
	if !errors.As(err, &apiErr) {
		return err.Error()
	}
	var body struct {
		Error string `json:"error"`
	}
	if json.Unmarshal([]byte(apiErr.Body), &body) == nil && body.Error != "" {
		return body.Error
	}
	return apiErr.Error()
}
