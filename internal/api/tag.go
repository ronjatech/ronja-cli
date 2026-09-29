package api

// The two tag verbs the metric loop needs: read a table's tags, and add or
// remove some without reading them first.
//
// SAME HAND-MIRROR RULE as everywhere else in this package: every tag below is
// copied from the server struct named in its comment
// (backend/api/v2/tag/patch.go and backend/ronja/rdb/table_tag.go), and the
// fixture tests hold them in step.
//
// TABLE-SHAPED ON PURPOSE. The server's routes take any of ten target kinds
// (`/tag/of/:type/:id`), and the only caller here tags metrics — which are
// `table` rows. A generic `kind` argument would be a parameter nothing passes
// anything but one value to, and the day a workflow or an app folder tags its
// row is the day its own verb is written against its own route.

import (
	"context"
	"net/url"
)

// Tag mirrors the subset of rdb.Tag the CLI reads: its identity and its name.
//
// The ID is the part that matters. A tag renamed in the web app keeps its id,
// and the metric loop records the ids it applied precisely so that a rename is
// followed rather than reverted — see planMetricTags in the commands package.
type Tag struct {
	ID   string `json:"id"`
	Name string `json:"name"`
}

// PatchTagsInput is the body of PATCH /tag/of/:type/:id, mirroring
// tag.PatchTagsInput.
//
// omitempty on all three: the server treats an absent list and an empty one
// the same, and refuses a body with every list empty — so a caller never sends
// one (the metric loop PATCHes only when its plan is non-empty).
//
// Every name and id is TRIMMED by the server before use, and
// `unmatchedRemoves` echoes the trimmed values; the loop sends them trimmed
// anyway, so the echo can be matched against what was sent by value.
type PatchTagsInput struct {
	// Add holds tag NAMES: an existing name is reused whatever its case, a new
	// one is created in the organization's catalog with an automatic color.
	Add []string `json:"add,omitempty"`
	// Remove holds tag NAMES, matched case-insensitively on the target.
	Remove []string `json:"remove,omitempty"`
	// RemoveIDs holds tag IDS — which still match a tag renamed since the
	// caller recorded its id. The metric loop removes by id only.
	RemoveIDs []string `json:"removeIDs,omitempty"`
}

// PatchTagsResult mirrors tag.PatchTagsResponse. The server sends every list
// present (never null).
//
// ⚠️ `Created` IS A HINT, NEVER STATE. It lists the add names THIS call minted
// into the catalog, and it exists so a push can say `created tag "Finanace"` the
// first time a typo lands. Nothing in the loop's lock or status logic may key on
// it: the lock records ids from `Tags`, which is the final set and the one
// answer that is always true.
type PatchTagsResult struct {
	// Tags is the target's FINAL tag list after the merge.
	Tags             []Tag    `json:"tags"`
	Added            []string `json:"added"`
	Removed          []string `json:"removed"`
	UnmatchedRemoves []string `json:"unmatchedRemoves"`
	Created          []string `json:"created"`
}

// TagOverlapCode is the wire discriminator on PATCH /tag/of's 400 for a name
// both added and removed — by name, or as a tag removed by id — mirrored from
// tag.TagOverlapCode (backend/api/v2/tag/patch.go). A caller that planned its
// removeIDs from a read gets it when a tag is renamed between that read and the
// PATCH, so it is the one 400 on the route worth re-reading and re-planning on.
const TagOverlapCode = "tag_overlap"

// TagLimitCode is the wire discriminator on PATCH /tag/of's 400 when the change
// would leave the target over 20 tags, mirrored from tag.TagLimitCode
// (backend/api/v2/tag/patch.go) — from the server's pre-mint count and from its
// re-check under the per-target lock alike. It is the one tag refusal the
// metric loop RECORDS, because it is the only one that answers the same way for
// every caller.
const TagLimitCode = "tag_limit"

// ListTableTags reads the tags on one table (a metric is a table).
//
// The server gates on visibility and answers an invisible or unknown id with a
// 403, never a 404 — so a 404 here is an instance that does not serve the route
// at all, which the metric loop reports as an older server.
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

// PatchTableTags adds and removes tags on one table in ONE atomic merge under
// the server's per-target lock, without the caller reading the set first.
//
// Tags already on the table and not named in the body are KEPT, which is the
// whole reason this route exists rather than PUT (which replaces the set, and
// so overwrites a concurrent edit in the web app).
//
// A 400 is a body the server refuses — a name both added and removed, a name
// over the 64-byte limit, a change that would leave more than 20 tags — and is
// refused BEFORE any add name is minted. A 404 or 405 is an instance too old to
// serve the route. Only the cap carries TagLimitCode.
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
