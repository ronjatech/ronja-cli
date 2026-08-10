package api

import (
	"context"
	"encoding/json"
	"time"
)

// Managed-database shapes, hand-mirrored from backend/api/v2/database like every
// other type in this package.
//
// One difference from Query is worth stating, because it changes what a caller
// has to remember: there is NO error field here. A statement Postgres refused
// comes back as HTTP 400 carrying the message, so an ordinary error return
// covers it — no 200-with-an-error trap to check for, and no way to mistake a
// failed statement for an empty result.

// DefaultDatabaseSQLTimeout bounds one `db sql` call when the caller names no
// deadline.
//
// The slow bound, matching Query: a statement against a real Postgres database
// is real work, and an aggregate over a large table legitimately outlasts a
// read timeout. It remains only a DEFAULT — --timeout moves it, and a value past
// the client's own ceiling is honoured (see httpFor).
const DefaultDatabaseSQLTimeout = slowRequestTimeout

// DefaultMigrationTimeout bounds one migration set.
//
// Deliberately far longer than a query default. The server runs the whole set in
// ONE transaction holding a per-database advisory lock, under a 20-minute
// ceiling of its own; a client that gave up sooner would abandon a migration
// that is still committing and leave the operator with no idea whether it
// landed. Better to wait for the answer than to guess at it.
const DefaultMigrationTimeout = 20 * time.Minute

// DatabaseSQLInput is the POST :id/sql body.
type DatabaseSQLInput struct {
	SQL string `json:"sql"`
	// Params are positional bound parameters for $1, $2 … placeholders, carried
	// as RAW JSON rather than []any.
	//
	// That is load-bearing, not fussiness. Decoding a parameter list into []any
	// turns every number into a float64, which silently loses precision above
	// 2^53 — 9007199254740993 becomes ...992, and 1234567890123456789 becomes
	// ...800. Re-encoding cannot recover what the decode threw away, so the wrong
	// number would reach the database with no error at any layer. Passing the
	// user's bytes through untouched means a bigint id survives the trip.
	//
	// Sent only when non-empty: the field is optional server-side, and an
	// explicit null adds nothing.
	Params  []json.RawMessage `json:"params,omitempty"`
	MaxRows int               `json:"maxRows,omitempty"`
}

// DatabaseSQLResult is the POST :id/sql response.
//
// No omitempty anywhere: this struct is emitted verbatim by `ronja db sql
// --json`, and a field that vanishes when it is zero makes the payload a
// different shape depending on the answer.
type DatabaseSQLResult struct {
	DatabaseID string `json:"databaseID"`
	// Result is the CSV, header row included. Empty for a statement with no
	// result set — a plain INSERT, say — which is not the same as no rows.
	Result   string `json:"result"`
	RowCount int    `json:"rowCount"`
	// Truncated reports that the row cap cut the result off, so Result is a
	// prefix rather than the answer.
	Truncated  bool  `json:"truncated"`
	DurationMs int64 `json:"durationMs"`
}

// DatabaseSQL runs one statement against a managed database.
func (c *Client) DatabaseSQL(ctx context.Context, databaseID string, in DatabaseSQLInput, timeout time.Duration) (*DatabaseSQLResult, error) {
	var out DatabaseSQLResult
	if err := c.do(ctx, timeout, "POST", "database/"+databaseID+"/sql", in, &out); err != nil {
		return nil, err
	}
	return &out, nil
}

// Migration is one named migration in a set.
type Migration struct {
	Name        string `json:"name"`
	Description string `json:"description,omitempty"`
	// SQL is sent VERBATIM. The server's ledger hash covers exactly these bytes,
	// so trimming or normalising here — a trailing newline is the obvious
	// temptation — would make a file hash differently from the way it was
	// applied, and every applied migration would then report as drifted.
	SQL string `json:"sql"`
}

// MigrationSetInput is the POST :id/migrations body.
type MigrationSetInput struct {
	Migrations []Migration `json:"migrations"`
	DryRun     bool        `json:"dryRun,omitempty"`
}

// Migration outcomes, as reported per migration. The first two are what a real
// apply returns; the last two only ever come back from a dry run.
const (
	// MigrationApplied — the DDL ran and a ledger row was written.
	MigrationApplied = "applied"
	// MigrationSkipped — already in the ledger with identical SQL.
	MigrationSkipped = "skipped"
	// MigrationPending — not in the ledger; a real apply would apply it.
	MigrationPending = "pending"
	// MigrationDrifted — in the ledger under DIFFERENT SQL. A real apply
	// refuses the whole set.
	MigrationDrifted = "drifted"
)

// MigrationResult is one migration's verdict.
type MigrationResult struct {
	Name    string `json:"name"`
	Outcome string `json:"outcome"`
	Seq     int    `json:"seq"`
}

// MigrationSetResult is the POST :id/migrations response.
type MigrationSetResult struct {
	DatabaseID string            `json:"databaseID"`
	DryRun     bool              `json:"dryRun"`
	Results    []MigrationResult `json:"results"`
}

// Drifted returns the migrations the server reported as drifted.
//
// Only a DRY RUN can produce these: a real apply refuses the whole set and
// returns an error instead, which is why `db migrate push` never needs to
// inspect this and `db migrate status` always does.
func (r *MigrationSetResult) Drifted() []MigrationResult {
	var out []MigrationResult
	for _, res := range r.Results {
		if res.Outcome == MigrationDrifted {
			out = append(out, res)
		}
	}
	return out
}

// Pending returns the migrations a real apply would apply.
func (r *MigrationSetResult) Pending() []MigrationResult {
	var out []MigrationResult
	for _, res := range r.Results {
		if res.Outcome == MigrationPending {
			out = append(out, res)
		}
	}
	return out
}

// ApplyMigrations applies (or, with DryRun, plans) an ordered migration set.
func (c *Client) ApplyMigrations(ctx context.Context, databaseID string, in MigrationSetInput, timeout time.Duration) (*MigrationSetResult, error) {
	var out MigrationSetResult
	if err := c.do(ctx, timeout, "POST", "database/"+databaseID+"/migrations", in, &out); err != nil {
		return nil, err
	}
	return &out, nil
}
