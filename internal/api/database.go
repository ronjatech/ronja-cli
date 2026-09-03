package api

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
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

// Environment names WHICH COPY of a managed database a request runs against.
//
// A managed database can have a dev copy — a schema-identical sandbox with its
// own ledger — and the server resolves it. The client never learns the copy's
// id and never sends one: it names the PRODUCTION database and says which
// environment it means. That asymmetry is the whole safety property, so do not
// "simplify" this into a second database id the caller passes around.
type Environment string

const (
	// EnvironmentProd is the database itself, and the default everywhere.
	EnvironmentProd Environment = "prod"
	// EnvironmentDev is the database's dev copy, resolved server-side.
	EnvironmentDev Environment = "dev"
)

// ParseEnvironment validates a caller-typed environment name.
//
// Validated HERE rather than left to the server so a typo fails locally, before
// a migration set or a statement is sent: the server answers a bad value with a
// 400, which is correct but arrives as a message about a query parameter nobody
// typed. Trimmed and case-folded because the server does the same, and a value
// this command accepts must not become one the server rejects.
func ParseEnvironment(raw string) (Environment, error) {
	switch env := Environment(strings.ToLower(strings.TrimSpace(raw))); env {
	case "", EnvironmentProd:
		return EnvironmentProd, nil
	case EnvironmentDev:
		return EnvironmentDev, nil
	default:
		return "", fmt.Errorf("must be %q or %q (got %q)", EnvironmentProd, EnvironmentDev, raw)
	}
}

// withEnvironment appends ?environment= to a path, and ONLY for dev.
//
// The asymmetry is deliberate. An explicit `environment=prod` says exactly what
// sending nothing says, so adding it would change the bytes on the wire for
// every existing caller in exchange for nothing — and a request that is
// byte-identical to the one this CLI sent before dev copies existed is also the
// one an older instance, which knows no such parameter, still answers correctly.
//
// No url.Values: env is one of two constants produced by ParseEnvironment, never
// caller text, so there is nothing here to escape. Path-plus-query rides along
// on `do`, which joins the path to the instance verbatim — the same thing
// ListFeatureTables relies on.
//
// The separator is chosen rather than assumed. Every managed-database path is a
// bare constant today, but a caller that appends a query of its own and then
// gets a SECOND "?" sends a parameter no server parses — and the parameter that
// goes missing is the one that says "not production".
func withEnvironment(path string, env Environment) string {
	if env != EnvironmentDev {
		return path
	}
	sep := "?"
	if strings.Contains(path, "?") {
		sep = "&"
	}
	return path + sep + "environment=" + string(EnvironmentDev)
}

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
	// Environment is which copy actually answered: "prod", or "dev" when the
	// request was redirected to the database's dev copy.
	Environment string `json:"environment"`
	// ParentDatabaseID is the production database the caller named, and the
	// server sets it ONLY when the request was redirected. So it is the honest
	// signal that a redirect happened, where Environment is the label for it.
	//
	// It carries no omitempty here even though the server's does: this struct is
	// emitted verbatim by `ronja db sql --json`, and a field that vanishes when
	// it is empty makes the payload a different shape depending on the answer.
	ParentDatabaseID string `json:"parentDatabaseID"`
}

// DatabaseSQL runs one statement against a managed database.
//
// env picks which copy: EnvironmentProd (the zero value's meaning too) runs
// against the database itself, EnvironmentDev against its dev copy.
func (c *Client) DatabaseSQL(ctx context.Context, databaseID string, in DatabaseSQLInput, env Environment, timeout time.Duration) (*DatabaseSQLResult, error) {
	var out DatabaseSQLResult
	if err := c.do(ctx, timeout, "POST", withEnvironment("database/"+databaseID+"/sql", env), in, &out); err != nil {
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

// MigrationSetResult is the POST :id/migrations response, and — identically —
// the POST :id/promote one.
//
// Reused for promote rather than mirrored a second time, because it IS the same
// shape: promote applies a set of migrations through the same server-side path,
// so it returns the same verdicts. Sharing the type is what lets both commands
// print through one reporter, which is the only way the two stay describable in
// the same words.
//
// No omitempty anywhere, for the reason DatabaseSQLResult gives: `--json` emits
// this verbatim.
type MigrationSetResult struct {
	DatabaseID string            `json:"databaseID"`
	DryRun     bool              `json:"dryRun"`
	Results    []MigrationResult `json:"results"`
	// Environment is which copy the set was applied to, or planned against:
	// "prod", or "dev" when the request was redirected to the dev copy.
	Environment string `json:"environment"`
	// ParentDatabaseID is the production database the caller named, set only on
	// a redirect. See DatabaseSQLResult.ParentDatabaseID.
	ParentDatabaseID string `json:"parentDatabaseID"`
	// Notes are advisory and never block. Only promote emits them today: a
	// promoted migration that writes ROWS replays its SQL against production's
	// data rather than the dev copy's, which only the operator can judge.
	Notes []string `json:"notes"`
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
//
// env picks which copy the set lands on; see DatabaseSQL.
func (c *Client) ApplyMigrations(ctx context.Context, databaseID string, in MigrationSetInput, env Environment, timeout time.Duration) (*MigrationSetResult, error) {
	var out MigrationSetResult
	if err := c.do(ctx, timeout, "POST", withEnvironment("database/"+databaseID+"/migrations", env), in, &out); err != nil {
		return nil, err
	}
	return &out, nil
}

// PromoteInput is the POST :id/promote body.
//
// It carries no migration list, and that is the design rather than an omission:
// what promotes is exactly the tail the dev copy's ledger has past production's,
// which the server computes from the two ledgers. There is nothing for a caller
// to choose, so there is nothing for a caller to get wrong.
type PromoteInput struct {
	DryRun bool `json:"dryRun,omitempty"`
}

// Promote moves the migrations a database's dev copy has and the database does
// not onto the database itself.
//
// databaseID is the PRODUCTION database, never the copy — the copy has no id the
// CLI ever sees. There is no environment parameter for the same reason the
// server refuses a dev session here: this call names production explicitly, and
// redirecting it would promote the copy into itself and report success.
//
// The tail is applied through the same server-side path ApplyMigrations uses, so
// the response is a MigrationSetResult with the same four outcomes.
func (c *Client) Promote(ctx context.Context, databaseID string, in PromoteInput, timeout time.Duration) (*MigrationSetResult, error) {
	var out MigrationSetResult
	if err := c.do(ctx, timeout, "POST", "database/"+databaseID+"/promote", in, &out); err != nil {
		return nil, err
	}
	return &out, nil
}
