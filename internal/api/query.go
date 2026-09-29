package api

import (
	"context"
	"time"
)

// DefaultQueryTimeout bounds one query when the caller names no deadline.
//
// The slow bound rather than the read bound, for the usual reason — a query is
// real work server-side. It is only a DEFAULT, though: a large query legitimately
// routes to Batch compute and takes minutes, so `ronja query --timeout` can move
// it, and Query honours a value past the client's own ceiling. See httpFor.
const DefaultQueryTimeout = slowRequestTimeout

// The query endpoint, hand-mirrored from backend/api/v2 (POST
// /api/v2/duckdb/query) like every other shape in this package.

// QueryFormatCSVMeta asks for the envelope the CLI is built around: a CSV
// string plus the row count, the truncation flag and the failure field.
//
// The CLI always sends it. The endpoint has other formats, and picking between
// them would be a flag nobody can answer — CSV is the one shape that is both
// human-legible at a terminal and directly consumable by every data tool, and
// the "meta" half is what makes truncation and a failed query distinguishable
// from an empty result.
const QueryFormatCSVMeta = "csv_meta"

// QueryInput is the request body.
//
// MaxRows omitted means the server's own default cap; the server also clamps
// anything above its ceiling, so an over-large value is not an error the CLI
// has to pre-empt.
type QueryInput struct {
	SQL     string `json:"sql"`
	MaxRows int    `json:"maxRows,omitempty"`
	Format  string `json:"format"`
}

// QueryResult is the csv_meta envelope.
//
// The trap worth stating loudly: a FAILED query comes back as HTTP 200 with
// Error set. Nothing in the transport layer will flag it, so every caller has
// to check Error before trusting Result — a query that fails and one that
// returns no rows are otherwise indistinguishable.
//
// No omitempty anywhere: this struct is emitted verbatim by `ronja query
// --json`, and a field that vanishes when it is zero makes the payload a
// different shape depending on the answer.
type QueryResult struct {
	// Result is the CSV, header row included.
	Result   string `json:"result"`
	RowCount int    `json:"rowCount"`
	// Truncated reports that the row cap cut the result off, so what is in
	// Result is a prefix rather than the answer.
	Truncated bool `json:"truncated"`
	// Error is the query's own failure — a syntax error, an unresolvable
	// {{ ref }}, a permission refusal. Non-empty means the query did not run.
	Error        string `json:"error"`
	SQL          string `json:"sql"`
	InstanceKind string `json:"instanceKind"`
	// ZoneUsed is the IANA reporting timezone the query's DuckDB session ran
	// at. The CLI carries no client timezone, so it is normally "UTC" — while
	// the same SQL in the browser runs at the signed-in user's zone. Any query
	// that buckets by day/week/month can therefore return different numbers on
	// the two surfaces, and this is the only field that says so.
	ZoneUsed string `json:"zoneUsed"`
	// Advice is NON-FATAL authoring advice about the SQL — today, backslash
	// escapes that mean one thing under the old embed and another under raw
	// semantics (see api/sqlsemantics.go).
	//
	// It says nothing about whether the query ran: a query with advice on it has
	// succeeded, and Error is still the only field that reports a failure. An
	// instance older than the field sends none, so an ABSENT array is the
	// ordinary case rather than a signal, and the CLI prints nothing for it.
	//
	// No omitempty, on this struct's own rule — `ronja query --json` emits the
	// envelope verbatim and a key that disappears when it is empty makes the
	// payload a different shape depending on the answer.
	Advice []QueryAdvice `json:"advice"`
}

// QueryAdvice is one non-fatal note about the SQL that was run.
//
// Every field is optional on the wire and the renderer treats it that way: a
// server that grows a new advice kind with no snippet must still print
// something legible here, and an offset of 0 is a real position rather than
// "unknown". Message is the only field a reader is guaranteed.
type QueryAdvice struct {
	// Kind is the machine-readable class of the advice, for a --json caller
	// that wants to branch rather than match on prose.
	Kind string `json:"kind"`
	// Message is the sentence a person reads.
	Message string `json:"message"`
	// Snippet is the fragment of SQL the advice is about, empty when the advice
	// is about the statement as a whole.
	Snippet string `json:"snippet"`
	// Offset is the byte offset of Snippet within the SQL that was sent.
	Offset int `json:"offset"`
}

// Failed reports a query the server accepted and could not run.
func (q *QueryResult) Failed() bool {
	return q != nil && q.Error != ""
}

// Query runs one SQL statement and returns the csv_meta envelope.
//
// timeout bounds the request. Zero or less means no client-side deadline at
// all, which the command surfaces as `--timeout 0` — a query that has been
// routed to Batch compute can legitimately outlast any number anyone would
// think to type, and being told to go and look for a result that is still
// coming is not an improvement on waiting for it.
func (c *Client) Query(ctx context.Context, in QueryInput, timeout time.Duration) (*QueryResult, error) {
	if in.Format == "" {
		in.Format = QueryFormatCSVMeta
	}
	var out QueryResult
	if err := c.do(ctx, timeout, "POST", "duckdb/query", in, &out); err != nil {
		return nil, err
	}
	// The server omits `advice` when it has nothing to say, and an older one
	// never sends it. `--json` and `--jq` re-encode this struct, so an absent
	// array would come out as `"advice": null` — normalised to `[]`, so a
	// caller iterating `.advice[]` never has to guard against null.
	if out.Advice == nil {
		out.Advice = []QueryAdvice{}
	}
	return &out, nil
}
