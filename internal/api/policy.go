package api

import "context"

// Policy mirrors the organization standing-instructions document
// (PolicyView in backend/api/v2/policy/views.go). Only the fields `context`
// renders are mirrored; the rest of the view (id, scope, maxChars, updatedBy)
// is nothing this CLI reads, and a field named here that the server does not
// serialize would decode to its zero value in silence.
type Policy struct {
	Content string `json:"content"`
	// Version is the document's compare-and-swap token. Reported by `context`
	// as the expectedVersion an admin's `ronja api -X PUT /api/v2/policy/org`
	// must carry; the CLI itself never sends it (there is no policy verb — the
	// transport is `ronja api`).
	Version int `json:"version"`
	// UpdatedAt is RFC 3339, and ABSENT for a document nobody has written yet:
	// the read does not create, so the genesis document (empty content at
	// version 1) has no row and no timestamp.
	UpdatedAt string `json:"updatedAt,omitempty"`
}

// OrgPolicy reads the organization's policy. The route's role floor is the
// lowest there is, so every member clears it; a 403 here can only be a scoped
// token that lacks analytics:read. Unlike Me, a scoped token CAN call this —
// /api/v2/policy is not admin-scoped — which is why `context` reads it whether
// or not Me succeeded.
func (c *Client) OrgPolicy(ctx context.Context) (*Policy, error) {
	var out Policy
	if err := c.Do(ctx, "GET", "policy/org", nil, &out); err != nil {
		return nil, err
	}
	return &out, nil
}
