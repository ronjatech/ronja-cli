package api

import (
	"context"
	"net/url"
)

// Feature hand-mirrors the sliver of rdb.Feature (table_feature.go) the CLI
// reads. It exists because rdb.DataApp, unlike a workflow row, carries no
// featureScope, so `ronja app publish` must ask; it is not a general feature
// client, since discovery belongs on plain HTTP.
type Feature struct {
	ID   string `json:"id"`
	Name string `json:"name"`
	// Scope decides whether a commit is admin-only. Callers treat an unknown
	// value as not-shared, so the commit is attempted and the server decides.
	Scope string `json:"scope"`
}

// GetFeature reads a feature's metadata. A feature the caller cannot reach,
// whatever the reason, is refused as "feature not found", deliberately
// indistinguishable.
//
// ⚠️ Do NOT branch on the STATUS: instances in the field answer 400 or 404
// (and "no rows" or a bare "not found"), so commands.explainFeatureUnreachable
// accepts either status and branches on the message.
func (c *Client) GetFeature(ctx context.Context, id string) (*Feature, error) {
	var out Feature
	if err := c.Do(ctx, "GET", "feature/"+url.PathEscape(id), nil, &out); err != nil {
		return nil, err
	}
	return &out, nil
}
