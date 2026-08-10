package api

import (
	"context"
	"net/url"
)

// Feature is a HAND-MIRROR of the sliver of rdb.Feature the CLI reads: what a
// feature is called, and how private it is.
//
// Scope is the only field anything branches on, and it decides whether a commit
// is admin-only. Workflows get it for free — the server stamps featureScope onto
// every workflow row at read time — but rdb.DataApp carries no such column, so
// `ronja app publish` has to ask. That asymmetry is the entire reason this type
// exists; it is not a general feature client and should not grow into one, since
// discovery belongs on plain HTTP.
//
// Tags copied from backend/ronja/rdb/table_feature.go.
type Feature struct {
	ID   string `json:"id"`
	Name string `json:"name"`
	// Scope is private|workspace|organization. Compare against
	// scopeOrganization; an unknown value is treated as not-shared by callers,
	// which fails toward attempting the commit and letting the server decide.
	Scope string `json:"scope"`
}

// GetFeature reads a feature's metadata. 404s for an id the caller cannot reach.
func (c *Client) GetFeature(ctx context.Context, id string) (*Feature, error) {
	var out Feature
	if err := c.Do(ctx, "GET", "feature/"+url.PathEscape(id), nil, &out); err != nil {
		return nil, err
	}
	return &out, nil
}
