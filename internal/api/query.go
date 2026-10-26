package api

import (
	"context"
	"time"
)

// DefaultQueryTimeout bounds one query when the caller names no deadline. It is
// only a default: `ronja query --timeout` can move it, even past the client's
// own ceiling (see httpFor).
const DefaultQueryTimeout = slowRequestTimeout

// QueryFormatCSVMeta is the only format the CLI sends: CSV plus the row count,
// the truncation flag and the failure field, so a truncated or failed query is
// distinguishable from an empty result.
const QueryFormatCSVMeta = "csv_meta"

// QueryInput is the request body of POST /api/v2/duckdb/query. MaxRows omitted
// means the server's default cap, and the server clamps anything above its ceiling.
type QueryInput struct {
	SQL     string `json:"sql"`
	MaxRows int    `json:"maxRows,omitempty"`
	Format  string `json:"format"`
	// Attach names the databases a join reads, alias → secret id (the hidden
	// `--attach`).
	Attach map[string]string `json:"attach,omitempty"`
}

// QueryResult is the csv_meta envelope. A FAILED query comes back as HTTP 200
// with Error set, so every caller must check Error (or Failed) before trusting
// Result. No omitempty anywhere: `ronja query --json` emits this struct
// verbatim, and its shape must not depend on the answer.
type QueryResult struct {
	// Result is the CSV, header row included.
	Result   string `json:"result"`
	RowCount int    `json:"rowCount"`
	// Truncated reports that the row cap cut Result to a prefix.
	Truncated bool `json:"truncated"`
	// Error is the query's own failure; non-empty means the query did not run.
	Error        string `json:"error"`
	SQL          string `json:"sql"`
	InstanceKind string `json:"instanceKind"`
	// ZoneUsed is the IANA timezone the DuckDB session ran at: normally "UTC"
	// from the CLI but the user's zone in the browser, so day/week/month
	// buckets can differ between the two and this is the only field that says so.
	ZoneUsed string `json:"zoneUsed"`
	// Advice is non-fatal authoring advice about the SQL (see
	// api/sqlsemantics.go); a query with advice succeeded, and Error is still
	// the only failure field.
	Advice []QueryAdvice `json:"advice"`
	// TableHealth lists the referenced tables that are not updating; the query
	// ran regardless. An older server never sends it.
	TableHealth []QueryTableHealth `json:"tableHealth"`
}

// QueryTableHealth is one referenced table whose own build is failing, or that
// is built on a broken input (backend/api/v2/duckdb/handler.go).
type QueryTableHealth struct {
	ID           string              `json:"id"`
	Name         string              `json:"name"`
	BuildFailing *QueryBuildFailing  `json:"buildFailing,omitempty"`
	Upstream     []QueryUpstreamItem `json:"upstream"`
	// UpstreamMore counts the upstream problems left out of Upstream.
	UpstreamMore int `json:"upstreamMore"`
}

// QueryBuildFailing is a table's own failing build: its rows are its last good
// data.
type QueryBuildFailing struct {
	Since  *time.Time `json:"since,omitempty"`
	Reason string     `json:"reason"`
}

// QueryUpstreamItem is one upstream problem. For an input the reader cannot
// open, Readable is false and only Problem and Reason are set; Reason is then
// a whole clause ("an input you cannot open has a failing build").
type QueryUpstreamItem struct {
	ID          string     `json:"id,omitempty"`
	Name        string     `json:"name,omitempty"`
	Problem     string     `json:"problem"`
	Readable    *bool      `json:"readable,omitempty"`
	Reason      string     `json:"reason"`
	Since       *time.Time `json:"since,omitempty"`
	LastBuiltAt *time.Time `json:"lastBuiltAt,omitempty"`
}

// QueryAdvice is one non-fatal note about the SQL that was run. Every field but
// Message is optional on the wire, and an Offset of 0 is a real position, not "unknown".
type QueryAdvice struct {
	Kind    string `json:"kind"`
	Message string `json:"message"`
	// Snippet is empty when the advice is about the statement as a whole.
	Snippet string `json:"snippet"`
	// Offset is the byte offset of Snippet within the SQL that was sent.
	Offset int `json:"offset"`
}

// Failed reports a query the server accepted and could not run.
func (q *QueryResult) Failed() bool {
	return q != nil && q.Error != ""
}

// Query runs one SQL statement and returns the csv_meta envelope. A timeout of
// zero or less means no client-side deadline (`--timeout 0`).
func (c *Client) Query(ctx context.Context, in QueryInput, timeout time.Duration) (*QueryResult, error) {
	if in.Format == "" {
		in.Format = QueryFormatCSVMeta
	}
	var out QueryResult
	if err := c.do(ctx, timeout, "POST", "duckdb/query", in, &out); err != nil {
		return nil, err
	}
	// `--json` and `--jq` re-encode this struct, so an absent `advice` or
	// `tableHealth` is normalised to `[]` rather than emitted as null.
	if out.Advice == nil {
		out.Advice = []QueryAdvice{}
	}
	if out.TableHealth == nil {
		out.TableHealth = []QueryTableHealth{}
	}
	for i := range out.TableHealth {
		if out.TableHealth[i].Upstream == nil {
			out.TableHealth[i].Upstream = []QueryUpstreamItem{}
		}
	}
	return &out, nil
}
