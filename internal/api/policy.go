package api

import "context"

// Policy mirrors the fields of the organization policy document (PolicyView in
// backend/api/v2/policy/views.go) that `context` renders; a field named here
// that the server does not serialize decodes to its zero value in silence.
type Policy struct {
	Content string `json:"content"`
	// Version is the compare-and-swap token `context` reports as the
	// expectedVersion a `ronja api -X PUT /api/v2/policy/org` must carry.
	Version int `json:"version"`
	// UpdatedAt is absent for a document nobody has written yet: the read does
	// not create a row.
	UpdatedAt string `json:"updatedAt,omitempty"`
}

// OrgPolicy reads the organization's policy. Every member clears the route's
// role floor, so a 403 means a scoped token lacking analytics:read; unlike Me
// it is not admin-scoped, which is why `context` reads it even when Me fails.
func (c *Client) OrgPolicy(ctx context.Context) (*Policy, error) {
	var out Policy
	if err := c.Do(ctx, "GET", "policy/org", nil, &out); err != nil {
		return nil, err
	}
	return &out, nil
}
