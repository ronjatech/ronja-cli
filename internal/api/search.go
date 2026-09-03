package api

// The DISCOVERY read: GET /api/v2/search, the name → id lookup behind
// `ronja bind`.
//
// It sits oddly beside the rest of this package, which mirrors the endpoints a
// sync loop needs, and the rule it does NOT break is worth saying out loud. The
// CLI owns sync verbs and never resource verbs, so there is no `ronja search`
// and there will not be one — discovery over HTTP is `ronja api
// /api/v2/search?q=…`, which needs nothing here. What this exists for is one
// reconciliation: a folder DECLARES the names its code uses, and a stack has to
// say which of ITS organization's rows answers to each. Turning a name a person
// already committed into an id is not browsing.
//
// So it is deliberately narrow. It takes a term and a limit, it mirrors four
// fields of a hit, and it has no opinion about which hits are good — that
// judgement belongs to the caller that knows what it declared.

import (
	"context"
	"net/url"
	"strconv"
)

// SearchHit is a HAND-MIRROR of the subset of search.Hit the CLI reads
// (backend/lib/search/hit.go).
//
// Four fields of eleven, on table.go's rule: a mirrored field nobody reads is a
// field nobody keeps in step. The ones left out are left out for a reason —
// `featureID` and `updatedAt` describe where a row lives and when it moved,
// which says nothing about whether it is the row an alias means, and
// `verification` and `subKind` qualify hits `bind` never asks for.
type SearchHit struct {
	// Kind is which resource this is — one of the SearchKind* constants below.
	Kind string `json:"kind"`
	// ID is the resource id, prefixed by kind ("table-…", "workflow-…"). This is
	// the value a bind records.
	ID string `json:"id"`
	// Title is the resource's own name, which is what a hit's Score was computed
	// against for every kind that names one.
	Title string `json:"title"`
	// Score is the relevance BUCKET, not a distance: see SearchScoreExact.
	Score float64 `json:"score"`
}

// Search relevance buckets, mirrored from the `ORDER BY CASE` every resource's
// SearchForUser computes (backend/resource/rmodelv2/search.go and its
// siblings): 3 exact, 2 prefix, 1 substring, on the row's NAME.
//
// Only the exact bucket has a constant, because it is the only one anything
// here may act on. A prefix or substring hit is a guess, and a wrong bind
// deploys a folder against the wrong row while reporting success.
//
// ⚠️ A hit can carry this score without its title matching at all. The endpoint
// merges a TAG leg (backend/api/v2/search/tags.go) that stamps the matching
// TAG's score onto the resource it is attached to, so a table tagged `orders`
// arrives as an exact hit whatever it is called. A caller that means "exactly
// this name" has to check the name.
const SearchScoreExact = 3

// The kinds a hit can carry, mirrored from search.Hit's own list.
//
// Only the six `bind` can resolve are named. The full set the endpoint can
// return is `feature`, `table`, `workflow`, `note`, `dataapp`, `agent`,
// `scheduledjob`, `secret`, `mcpserver` and `mailbox` — and the omission that
// matters is on the other side: there is NO `codex` hit, because the endpoint
// does not fan out to codexes at all. A caller cannot tell that from an empty
// result, so it has to know.
const (
	SearchKindTable    = "table"
	SearchKindSecret   = "secret"
	SearchKindAgent    = "agent"
	SearchKindMailbox  = "mailbox"
	SearchKindWorkflow = "workflow"
	// SearchKindNote covers BOTH note sub-kinds. The endpoint reports a note's
	// skill/knowledge split in `subKind`, which this mirror does not carry and
	// `bind` does not ask about: an automation's reference set declares
	// kind="note" for either.
	SearchKindNote = "note"
)

// MinSearchTermLen mirrors minQueryLen in backend/api/v2/search/handler.go: a
// term shorter than this returns an empty result with no search performed, and
// with no error to say so.
//
// Mirrored rather than left to the server because the two answers are
// identical on the wire and mean opposite things — "nothing here is called
// that" and "nobody looked" — and a caller reporting the first when the second
// happened sends its reader hunting for a row that is sitting right there.
const MinSearchTermLen = 2

// MaxSearchLimit mirrors maxTotalCap in the same file. The server clamps
// silently, so asking for more is not an error; it is just not honoured.
const MaxSearchLimit = 40

// Search runs the global find-anything search and returns the merged hits, best
// first.
//
// Access-filtered per credential by construction — every kind is searched
// through its own read-access rules — so an empty result means "nothing you can
// see is called that", never "no such row". The one asymmetry worth knowing
// about a SCOPED token: `secret` hits are absent unless the token also holds
// `secrets:read`, since that is what reading secret metadata costs everywhere
// else.
//
// A limit of zero or less takes the server's default (which is also
// MaxSearchLimit).
func (c *Client) Search(ctx context.Context, term string, limit int) ([]SearchHit, error) {
	query := url.Values{}
	query.Set("q", term)
	if limit > 0 {
		query.Set("limit", strconv.Itoa(limit))
	}
	var out struct {
		Result []SearchHit `json:"result"`
	}
	if err := c.Do(ctx, "GET", "search?"+query.Encode(), nil, &out); err != nil {
		return nil, err
	}
	return out.Result, nil
}
