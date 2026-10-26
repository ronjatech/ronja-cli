package commands

import (
	"encoding/json"
	"fmt"
	"os"
	"strings"
	"time"

	"github.com/ronjatech/ronja-cli/internal/api"
	"github.com/spf13/cobra"
)

// It reuses `ronja query`'s machinery (SQL-source resolution, the 0600 file
// write, stderr-only narration). Unlike `ronja query`, a failed statement is an
// HTTP 400, not a 200 with an error field, so a plain error return covers it.
func newDatabaseSQLCmd() *cobra.Command {
	var (
		file    string
		out     string
		params  string
		env     string
		maxRows int
		timeout time.Duration
	)

	cmd := &cobra.Command{
		Use:   "sql <database-id> [sql]",
		Short: "Run SQL against a managed database and print the result as CSV",
		Long: `Run SQL against a managed database and print the result as CSV.

The SQL comes from exactly one of three places — an argument, a file, or a
pipe:

  ronja db sql mdb-abc "SELECT * FROM leads LIMIT 10"
  ronja db sql mdb-abc --file report.sql
  cat report.sql | ronja db sql mdb-abc

Pass untrusted values as bound parameters rather than building the statement
around them:

  ronja db sql mdb-abc "INSERT INTO leads (email, score) VALUES (\$1, \$2)" \
    --params '["a@b.c", 42]'

A call usually runs one statement: a SELECT- or WITH-led body, one with
RETURNING, or one sent with --params is refused if it holds a second ("cannot
insert multiple commands into a prepared statement"). A plain write without
--params runs every statement in it. Each call is its own session: a
temporary table or a SET does not carry over, and a transaction left open
(BEGIN with no COMMIT) is rolled back after the call, though it exits zero.

Output is CSV on stdout, and nothing else is written there:

  --out <file>    write the CSV to a file instead of stdout
  --json          print the whole response envelope as one JSON object
  --max-rows N    ask for at most N rows (the server caps at 100000)
  --timeout       how long to wait (0 waits as long as it takes)

With --out alone, stdout stays empty and the row count is reported on stderr.
A truncated result is reported on stderr and still exits zero — the rows you
got are real, there are simply more of them.

A write with no result set prints no CSV at all, so how many rows it changed
is reported on stderr instead ("40 rows affected"). That count is the rows the
statement's OUTER command affected: a write performed inside a data-modifying
WITH clause is not counted in it. The line is absent, rather than zero, when
the server cannot honestly say: a statement that returned rows, more than one
statement sent in one call, or a command whose result carries no count (DO,
CALL, SET, TRUNCATE, COMMENT).

This runs as the database's WRITE role, so it does DML and SELECT but cannot
do DDL: CREATE, ALTER and DROP are refused by Postgres itself. Schema changes
go through ` + "`ronja db migrate`" + `, which records them in the database's ledger.

If the database has no write role yet, the first statement fails saying so.
Mint one with:

  ronja api -X POST /api/v2/database/<id>/user -d '{"access":"write","featureID":"<feature-id>"}'`,
		// A third positional is unquoted SQL; refusing it beats a merged string
		// that half-runs.
		Args: refusingRetiredEnv(&env, cobra.RangeArgs(1, 2)),
		RunE: func(cmd *cobra.Command, args []string) error {
			databaseID := strings.TrimSpace(args[0])
			if databaseID == "" {
				return fmt.Errorf("no database id given")
			}
			sql, err := querySQL(args[1:], file)
			if err != nil {
				return err
			}
			bound, err := parseSQLParams(params)
			if err != nil {
				return err
			}
			if maxRows < 0 {
				return fmt.Errorf("--max-rows cannot be negative (got %d)", maxRows)
			}
			if timeout < 0 {
				return fmt.Errorf("--timeout cannot be negative (got %s) — pass 0 to wait for as long as the statement takes", timeout)
			}

			resolved, err := resolveInstance()
			if err != nil {
				return err
			}
			client := api.New(resolved.URL, resolved.Token)

			result, err := client.DatabaseSQL(cmd.Context(), databaseID, api.DatabaseSQLInput{
				SQL:     sql,
				Params:  bound,
				MaxRows: maxRows,
			}, timeout)
			if err != nil {
				return err
			}

			// Always stderr: stdout is the CSV a pipeline is parsing.
			if result.Truncated {
				fmt.Fprintf(os.Stderr,
					"  Note: truncated at %d %s — this is a prefix, not the whole answer. Raise --max-rows (with --out for a large result), or aggregate in SQL.\n",
					result.RowCount, plural(result.RowCount, "row"))
			}

			// A write prints an empty CSV, so its affected count is otherwise
			// invisible; --json's envelope carries the field itself.
			if !flagJSON {
				reportRowsAffected(result.RowsAffected)
			}

			if out != "" {
				if err := writeResultFile(out, result.Result); err != nil {
					return err
				}
				// ⚠️ On a write RowCount is rows AFFECTED and the file is empty, so
				// the body, not RowCount, decides what this line says.
				if result.Result == "" {
					fmt.Fprintf(os.Stderr, "  no result set — %s is empty\n", out)
				} else {
					fmt.Fprintf(os.Stderr, "  %d %s written to %s\n",
						result.RowCount, plural(result.RowCount, "row"), out)
				}
			}
			if flagJSON {
				return emitJSON(result)
			}
			if out == "" {
				// Verbatim, no added newline: this is data.
				fmt.Print(result.Result)
			}
			return nil
		},
	}

	cmd.Flags().StringVar(&file, "file", "",
		"read the SQL from a file (- for standard input)")
	cmd.Flags().StringVar(&out, "out", "",
		"write the CSV to this file instead of stdout")
	cmd.Flags().StringVar(&params, "params", "",
		`bound parameters for $1, $2 … as a JSON array (e.g. '["a@b.c", 42]'). An element may itself be a list, bound as one Postgres array — match it with 'col = ANY($1)', not 'col IN ($1)'`)
	bindRetiredEnvFlag(cmd, &env)
	cmd.Flags().IntVar(&maxRows, "max-rows", 0,
		"maximum rows to return (default: the server's cap)")
	cmd.Flags().DurationVar(&timeout, "timeout", api.DefaultDatabaseSQLTimeout,
		"how long to wait for the statement (0 waits as long as it takes)")
	return cmd
}

// ⚠️ nil is NOT zero: it means not applicable or not knowable (rows returned,
// several statements, no driver count), so it prints nothing rather than "0".
func reportRowsAffected(rowsAffected *int) {
	if rowsAffected == nil {
		return
	}
	fmt.Fprintf(os.Stderr, "  %d %s affected\n", *rowsAffected, plural(*rowsAffected, "row"))
}

// parseSQLParams splits the --params JSON array into its raw elements so a
// malformed value fails naming the flag; which values are accepted is left to
// the server. Elements stay raw JSON: decoding into []any would round integers
// above 2^53 through float64 and write a wrong value.
func parseSQLParams(raw string) ([]json.RawMessage, error) {
	if strings.TrimSpace(raw) == "" {
		return nil, nil
	}
	var out []json.RawMessage
	if err := json.Unmarshal([]byte(raw), &out); err != nil {
		return nil, fmt.Errorf("--params must be a JSON array like '[\"a@b.c\", 42]': %w", err)
	}
	return out, nil
}
