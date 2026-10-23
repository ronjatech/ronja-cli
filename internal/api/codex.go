package api

import (
	"context"
	"net/url"
)

// Codex is the subset of GET /api/v2/codex/:id (rdb.Codex,
// backend/ronja/rdb/table_codex.go) the write gate reads: which feature a codex
// lives in. A codex is feature-owned (feature_id NOT NULL), so the feature IS
// the answer to "would writing it write somebody else's".
type Codex struct {
	ID        string `json:"id"`
	FeatureID string `json:"featureID"`
	Name      string `json:"name"`
}

// GetCodex reads one codex through the access-checked path
// (backend/api/v2/codex/handler.go, `GET /codex/:id` → GetForAgent). A codex the
// caller cannot read answers 404, which a gate reads as "outside".
func (c *Client) GetCodex(ctx context.Context, id string) (*Codex, error) {
	var out Codex
	if err := c.Do(ctx, "GET", "codex/"+url.PathEscape(id), nil, &out); err != nil {
		return nil, err
	}
	return &out, nil
}
