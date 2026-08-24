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

// `ronja db sql` is `ronja query`'s counterpart for a managed Postgres database,
// and it reuses that command's machinery deliberately — the SQL-source
// resolution, the atomic 0600 file write, the stderr-only narration. Two
// commands that print a table should print it the same way.
//
// One thing is genuinely different, and it is an improvement rather than a
// quirk: a failed statement here is an HTTP 400, not a 200 with an error field.
// So there is no envelope to inspect and no trap to document — an ordinary error
// return covers it, and every layer above (a shell, `set -e`, CI) does the right
// thing without being told.
func newDatabaseSQLCmd() *cobra.Command {
	var (
		file    string
		out     string
		params  string
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

Output is CSV on stdout, and nothing else is written there:

  --out <file>    write the CSV to a file instead of stdout
  --json          print the whole response envelope as one JSON object
  --max-rows N    ask for at most N rows (the server caps at 100000)
  --timeout       how long to wait (0 waits as long as it takes)

With --out alone, stdout stays empty and the row count is reported on stderr.
A truncated result is reported on stderr and still exits zero — the rows you
got are real, there are simply more of them.

This runs as the database's WRITE role, so it does DML and SELECT but cannot
do DDL: CREATE, ALTER and DROP are refused by Postgres itself. Schema changes
go through ` + "`ronja db migrate`" + `, which records them in the database's ledger.

If the database has no write role yet, the first statement fails saying so.
Mint one with:

  ronja api -X POST /api/v2/database/<id>/user -d '{"access":"write","featureID":"<id>"}'`,
		// Two positionals: the database id and (optionally) the SQL. Three would
		// be somebody who forgot to quote their SQL, and cobra's own error says
		// so better than a merged string that half-runs.
		Args: cobra.RangeArgs(1, 2),
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

			// Always stderr, in every mode: a truncation notice on stdout would
			// land in the middle of the CSV a pipeline is parsing.
			if result.Truncated {
				fmt.Fprintf(os.Stderr,
					"  Note: truncated at %d %s — this is a prefix, not the whole answer. Raise --max-rows (with --out for a large result), or aggregate in SQL.\n",
					result.RowCount, plural(result.RowCount, "row"))
			}

			if out != "" {
				if err := writeResultFile(out, result.Result); err != nil {
					return err
				}
				fmt.Fprintf(os.Stderr, "  %d %s written to %s\n",
					result.RowCount, plural(result.RowCount, "row"), out)
			}
			if flagJSON {
				return emitJSON(result)
			}
			if out == "" {
				// Verbatim, with no added newline: this is data, and a byte the
				// server did not send is a byte a diff will find.
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
	cmd.Flags().IntVar(&maxRows, "max-rows", 0,
		"maximum rows to return (default: the server's cap)")
	cmd.Flags().DurationVar(&timeout, "timeout", api.DefaultDatabaseSQLTimeout,
		"how long to wait for the statement (0 waits as long as it takes)")
	return cmd
}

// parseSQLParams splits the --params JSON array into its raw elements.
//
// It is parsed here rather than forwarded as an opaque string so a malformed
// value fails locally, naming the flag, instead of arriving at the server as a
// type error about a field the user did not type. The accepted-value rule (which
// scalars, and where a list may bind as an array) is left to the server, which
// already enforces it for the agent's identical parameter path — duplicating it
// here would be a second definition to keep in step.
//
// The elements stay as RAW JSON. Decoding them into []any would turn every
// number into a float64 and silently lose precision above 2^53, so a bound
// parameter of 1234567890123456789 would be written to the database as
// ...800 — no error, wrong value. Splitting into json.RawMessage passes the
// user's own bytes through untouched. Pinned by TestParseSQLParamsKeepsPrecision.
//
// An empty flag means "no parameters", which is NOT the same as an empty array;
// both are accepted and both send nothing.
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
