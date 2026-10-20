package api

// The two tag verbs the metric loop needs, mirrored from
// backend/api/v2/tag/patch.go and backend/ronja/rdb/table_tag.go. Table-shaped on
// purpose: the server's `/tag/of/:type/:id` takes any target kind, but the only
// caller tags metrics, which are `table` rows.

import (
	"context"
	"net/url"
)

// Tag mirrors the subset of rdb.Tag the CLI reads. A tag renamed in the web app
// keeps its ID, and the metric loop records applied ids so a rename is followed
// rather than reverted (see planMetricTags in the commands package).
type Tag struct {
	ID   string `json:"id"`
	Name string `json:"name"`
}

// PatchTagsInput is the body of PATCH /tag/of/:type/:id, mirroring
// tag.PatchTagsInput. The server refuses a body with every list empty, and it
// echoes trimmed values in `unmatchedRemoves`, so callers send names and ids trimmed.
type PatchTagsInput struct {
	// Add holds tag NAMES: an existing name is reused whatever its case, a new
	// one is created in the organization's catalog.
	Add []string `json:"add,omitempty"`
	// Remove holds tag NAMES, matched case-insensitively on the target.
	Remove []string `json:"remove,omitempty"`
	// RemoveIDs holds tag IDS, which still match a tag renamed since the caller
	// recorded its id.
	RemoveIDs []string `json:"removeIDs,omitempty"`
}

// PatchTagsResult mirrors tag.PatchTagsResponse; the server sends every list
// present, never null. ⚠️ `Created` (the add names THIS call minted) is a hint for
// a push message, never state: the lock records ids from `Tags`, the final set.
type PatchTagsResult struct {
	// Tags is the target's FINAL tag list after the merge.
	Tags             []Tag    `json:"tags"`
	Added            []string `json:"added"`
	Removed          []string `json:"removed"`
	UnmatchedRemoves []string `json:"unmatchedRemoves"`
	Created          []string `json:"created"`
}

// TagOverlapCode is the wire code on PATCH /tag/of's 400 for a name both added
// and removed (by name or by id), mirrored from tag.TagOverlapCode. For a caller
// that planned its removeIDs from an earlier read, it means a tag was renamed in
// between, so it is the one 400 worth re-reading and re-planning on.
const TagOverlapCode = "tag_overlap"

// TagLimitCode is the wire code on PATCH /tag/of's 400 when the change would
// leave the target over 20 tags, mirrored from tag.TagLimitCode. It is the one tag
// refusal the metric loop records, being the only one that answers the same way
// for every caller.
const TagLimitCode = "tag_limit"

// ListTableTags reads the tags on one table (a metric is a table). The server
// answers an invisible or unknown id with a 403, never a 404, so a 404 here is an
// instance that does not serve the route.
func (c *Client) ListTableTags(ctx context.Context, tableID string) ([]Tag, error) {
	var out []Tag
	if err := c.Do(ctx, "GET", "tag/of/table/"+url.PathEscape(tableID), nil, &out); err != nil {
		return nil, err
	}
	if out == nil {
		out = []Tag{}
	}
	return out, nil
}

// PatchTableTags adds and removes tags on one table in one atomic merge under
// the server's per-target lock. Tags not named in the body are KEPT, which is why
// this is not PUT (which replaces the set and so overwrites a concurrent web-app
// edit). A 404 or 405 is an instance too old to serve the route.
func (c *Client) PatchTableTags(ctx context.Context, tableID string, in PatchTagsInput) (*PatchTagsResult, error) {
	var out PatchTagsResult
	if err := c.Do(ctx, "PATCH", "tag/of/table/"+url.PathEscape(tableID), in, &out); err != nil {
		return nil, err
	}
	if out.Tags == nil {
		out.Tags = []Tag{}
	}
	return &out, nil
}
