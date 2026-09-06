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

// GetFeature reads a feature's metadata. A feature the caller cannot reach is
// refused with the sentence "feature not found" — whether it does not exist, is
// in another organization, or is someone else's private one, which the server
// deliberately does not tell apart.
//
// ⚠️ Do NOT depend on the STATUS to tell those cases apart, and note that it has
// already moved once: an older backend reaches the wire with a lookup miss as
// 400 and an unreadable-but-present feature as 404, while a current one answers
// 404 for both. That split was never a contract, and the older instances still
// in the field also answer "no rows" or a bare "not found" here. So the matcher
// (commands.explainFeatureUnreachable) accepts EITHER status for this sentence
// and branches on the message, which is what carried it through the retype
// without a client change. Anything new reading this error should do the same.
//
// Two callers, and the second is a READ FOR EXISTENCE rather than for a field:
// commands.confirmFeatureIn calls it so `init` can refuse a --feature it cannot
// reach before writing a manifest that names it. That is still not a general
// feature client — nothing here enumerates, and discovery stays on plain HTTP.
func (c *Client) GetFeature(ctx context.Context, id string) (*Feature, error) {
	var out Feature
	if err := c.Do(ctx, "GET", "feature/"+url.PathEscape(id), nil, &out); err != nil {
		return nil, err
	}
	return &out, nil
}
