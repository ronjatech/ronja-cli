package api

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"time"
)

// Managed-database shapes, hand-mirrored from backend/api/v2/database. Unlike
// Query there is NO error field: a statement Postgres refused is an HTTP 400, so
// an ordinary error return covers it.

// DefaultDatabaseSQLTimeout is the slow bound, as for Query: an aggregate over a
// large table legitimately outlasts a read timeout. --timeout moves it.
const DefaultDatabaseSQLTimeout = slowRequestTimeout

// DefaultMigrationTimeout matches the server's own 20-minute ceiling on the one
// transaction a set runs in: a client that gave up sooner would leave the
// operator not knowing whether a still-committing set landed.
const DefaultMigrationTimeout = 20 * time.Minute

// Environment names WHICH COPY of a managed database a request runs against.
// The client never learns the dev copy's id: it names the PRODUCTION database
// and the server resolves the copy. That asymmetry is the safety property, so do
// not "simplify" it into a second database id.
type Environment string

const (
	// EnvironmentProd is the default everywhere.
	EnvironmentProd Environment = "prod"
	EnvironmentDev  Environment = "dev"
)

// ParseEnvironment validates a caller-typed environment name locally, so a typo
// fails before anything is sent. It must trim and case-fold as the server does,
// so a value accepted here is never one the server rejects.
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

// withEnvironment appends ?environment= ONLY for dev, so a prod request stays
// byte-identical to what an instance without dev copies answers. The separator
// is chosen, not assumed: a second "?" would drop the parameter that says "not
// production".
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
	// Params bind $1, $2 … as RAW JSON, never []any: a float64 decode would
	// silently change a bigint id past 2^53.
	Params  []json.RawMessage `json:"params,omitempty"`
	MaxRows int               `json:"maxRows,omitempty"`
}

// DatabaseSQLResult is the POST :id/sql response. No omitempty anywhere: `ronja
// db sql --json` emits it verbatim, and its shape must not depend on the answer.
type DatabaseSQLResult struct {
	DatabaseID string `json:"databaseID"`
	// Result is the CSV, header included; empty for a statement with no result
	// set, which is not the same as no rows.
	Result   string `json:"result"`
	RowCount int    `json:"rowCount"`
	// RowsAffected counts the OUTER command only. ⚠️ Writes inside a
	// data-modifying CTE are not counted (Postgres returns one command tag).
	//
	// nil, NEVER zero, means not applicable or not knowable (a result set,
	// several statements, a command with no count), so it stays a pointer: an
	// int would publish "changed nothing" for every read.
	RowsAffected *int `json:"rowsAffected"`
	// Truncated: Result is a prefix the row cap cut off.
	Truncated  bool  `json:"truncated"`
	DurationMs int64 `json:"durationMs"`
	// Environment is which copy actually answered.
	Environment string `json:"environment"`
	// ParentDatabaseID is set ONLY when the request was redirected to the dev
	// copy: the signal a redirect happened. No omitempty, unlike the server's.
	ParentDatabaseID string `json:"parentDatabaseID"`
}

// DatabaseSQL runs one statement against a managed database or, with
// EnvironmentDev, its dev copy.
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
	// SQL is sent VERBATIM: the server's ledger hash covers these exact bytes,
	// and normalising them (even a trailing newline) would report every applied
	// migration as drifted.
	SQL string `json:"sql"`
}

// MigrationSetInput is the POST :id/migrations body.
type MigrationSetInput struct {
	Migrations []Migration `json:"migrations"`
	DryRun     bool        `json:"dryRun,omitempty"`
}

// Migration outcomes. Pending and drifted only ever come back from a dry run.
const (
	MigrationApplied = "applied"
	// MigrationSkipped: already in the ledger with identical SQL.
	MigrationSkipped = "skipped"
	MigrationPending = "pending"
	// MigrationDrifted: in the ledger under DIFFERENT SQL; a real apply refuses
	// the whole set.
	MigrationDrifted = "drifted"
)

// MigrationResult is one migration's verdict.
type MigrationResult struct {
	Name    string `json:"name"`
	Outcome string `json:"outcome"`
	Seq     int    `json:"seq"`
}

// MigrationSetResult is the POST :id/migrations and POST :id/promote response
// (promote applies through the same server path). No omitempty: `--json` emits
// it verbatim.
type MigrationSetResult struct {
	DatabaseID  string            `json:"databaseID"`
	DryRun      bool              `json:"dryRun"`
	Results     []MigrationResult `json:"results"`
	Environment string            `json:"environment"`
	// ParentDatabaseID is set only on a redirect, as on DatabaseSQLResult.
	ParentDatabaseID string `json:"parentDatabaseID"`
	// Notes are advisory and never block; print them verbatim.
	Notes []string `json:"notes"`
}

// Drifted returns the migrations reported as drifted, which only a DRY RUN can
// produce: a real apply refuses the whole set with an error.
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
func (c *Client) ApplyMigrations(ctx context.Context, databaseID string, in MigrationSetInput, env Environment, timeout time.Duration) (*MigrationSetResult, error) {
	var out MigrationSetResult
	if err := c.do(ctx, timeout, "POST", withEnvironment("database/"+databaseID+"/migrations", env), in, &out); err != nil {
		return nil, err
	}
	return &out, nil
}

// PromoteInput carries no migration list by design: what promotes is the tail
// the dev copy's ledger has past production's, which the server computes.
type PromoteInput struct {
	DryRun bool `json:"dryRun,omitempty"`
}

// Promote moves the migrations a database's dev copy has onto the database.
// It takes no environment: redirecting it would promote the copy into itself
// and report success.
func (c *Client) Promote(ctx context.Context, databaseID string, in PromoteInput, timeout time.Duration) (*MigrationSetResult, error) {
	var out MigrationSetResult
	if err := c.do(ctx, timeout, "POST", "database/"+databaseID+"/promote", in, &out); err != nil {
		return nil, err
	}
	return &out, nil
}

// DatabaseConnection mirrors only the GET :id/roles fields the CLI uses; the
// roles list is deliberately absent.
type DatabaseConnection struct {
	Host     string `json:"host"`
	Port     string `json:"port"`
	SSLMode  string `json:"sslMode"`
	Database string `json:"database"`
	// Proxy is nil when the instance has no Postgres-wire proxy OR the caller is
	// not an admin; the server does not say which, so neither may the client.
	Proxy *DatabaseProxy `json:"proxy"`
}

// DatabaseProxy is the psql/pg_dump coordinates for one managed database.
//
// ⚠️ ConnectionString carries the literal placeholder <YOUR_PAT>, and no command
// prints it with the real PAT substituted: the PAT goes into an environment
// variable or a child process, never onto a terminal.
type DatabaseProxy struct {
	Host string `json:"host"`
	Port string `json:"port"`
	// Username is "<database-id>.read": the proxy resolves the role from the
	// tier in the login.
	Username         string `json:"username"`
	Database         string `json:"database"`
	ConnectionString string `json:"connectionString"`
	// ReadRoleMinted is false on a freshly provisioned database. The proxy
	// refuses such a login only AFTER authenticating, so the commands refuse it
	// first.
	ReadRoleMinted bool `json:"readRoleMinted"`
}

// DatabaseConnectionInfo reads a managed database's connection details.
func (c *Client) DatabaseConnectionInfo(ctx context.Context, databaseID string) (*DatabaseConnection, error) {
	var out DatabaseConnection
	if err := c.Do(ctx, "GET", "database/"+databaseID+"/roles", nil, &out); err != nil {
		return nil, err
	}
	return &out, nil
}
