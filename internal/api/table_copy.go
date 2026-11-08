package api

import (
	"context"
	"net/url"
)

// CopyTableInput is the body of POST /feature/model/:id/copy, mirroring
// feature.CopyTableRequest (backend/api/v2/feature/api_model_copy.go).
type CopyTableInput struct {
	FeatureID string `json:"featureID"`
	Name      string `json:"name,omitempty"`
	// Rows false creates (or empties) the copy with the source's columns and
	// no rows; nil copies the rows.
	Rows *bool `json:"rows,omitempty"`
}

// CopyTableResult mirrors feature.CopyTableResponse.
type CopyTableResult struct {
	Table struct {
		ID        string `json:"id"`
		Name      string `json:"name"`
		FeatureID string `json:"featureID"`
	} `json:"table"`
	RowsCopied int `json:"rowsCopied"`
	// Outcome is "created" or "refreshed", printed as the server says it.
	Outcome string `json:"outcome"`
}

// CopyTimeoutCode is the 504's code: the copy's read did not finish within the
// route's limit, and nothing was written over the target's rows.
const CopyTimeoutCode = "copy_timeout"

// CopyTable copies a table into a feature as a dynamic table, or refreshes the
// copy of that source the feature already holds. The route allows itself 80 s
// before answering copy_timeout, so the slow deadline lets that answer arrive.
func (c *Client) CopyTable(ctx context.Context, sourceID string, in CopyTableInput) (*CopyTableResult, error) {
	var out CopyTableResult
	if err := c.doSlow(ctx, "POST", "feature/model/"+url.PathEscape(sourceID)+"/copy", in, &out); err != nil {
		return nil, err
	}
	return &out, nil
}
