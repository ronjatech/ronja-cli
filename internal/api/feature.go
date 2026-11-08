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
	// SpaceID is the Space the feature is in. Empty from an instance that
	// predates Spaces.
	SpaceID string `json:"spaceID,omitempty"`
}

// Space hand-mirrors the sliver of api/v2/space.SpaceView the CLI reads.
type Space struct {
	ID        string `json:"id"`
	Name      string `json:"name"`
	IsDefault bool   `json:"isDefault"`
}

// GetSpace reads one Space. A Space the caller cannot enter answers 404, the
// same as an unknown id.
func (c *Client) GetSpace(ctx context.Context, id string) (*Space, error) {
	var out Space
	if err := c.Do(ctx, "GET", "space/"+url.PathEscape(id), nil, &out); err != nil {
		return nil, err
	}
	return &out, nil
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
