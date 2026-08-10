package commands

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"strings"
	"time"

	"github.com/ronjatech/ronja-cli/internal/api"
	"github.com/ronjatech/ronja-cli/internal/jqf"
	"github.com/spf13/cobra"
)

// `ronja query` is the one read that earns a verb of its own.
//
// It is a single endpoint, so on the face of it `ronja api -X POST
// /api/v2/duckdb/query -d '{"sql":"..."}'` covers it — and the doctrine says a
// wrapped call does not earn a command. Three things make this the exception,
// and all three are about what plain HTTP does BADLY rather than about
// convenience:
//
//   - Quoting. Real SQL is multi-line and full of quotes; embedding it in a
//     JSON string inside a shell string is a two-level escaping problem people
//     get wrong, and the failure is a syntax error attributed to the wrong
//     layer. Here the SQL is an argument, a file, or a pipe.
//   - Shape. The answer is a table, and the useful forms of it are "print it",
//     "write it to a file" and "hand me the envelope". A generic runner can only
//     ever give the third.
//   - The 200-with-an-error trap. A failed query is HTTP 200 with `error` set,
//     so a generic runner exits ZERO on a query that did not run. A pipeline
//     built on that continues over a failure it never saw.
//
// It stays a read. There is no `ronja list`, no `ronja delete`: discovery and
// mutation are still plain HTTP.
func newQueryCmd() *cobra.Command {
	var (
		file      string
		out       string
		maxRows   int
		timeout   time.Duration
		jqExpr    string
		rawOutput bool
	)

	cmd := &cobra.Command{
		Use:   "query [sql]",
		Short: "Run a read-only SQL query and print the result as CSV",
		Long: `Run a read-only SQL query and print the result as CSV.

The SQL comes from exactly one of three places — an argument, a file, or a
pipe:

  ronja query "SELECT * FROM {{ ref('table-abc') }} LIMIT 10"
  ronja query --file report.sql
  cat report.sql | ronja query

Tables are referenced as {{ ref('<table id>') }}, not by name. Table IDs come
from the API — see ` + "`ronja context`" + `.

Output is CSV on stdout, and nothing else is written there:

  --out <file>    write the CSV to a file instead of stdout
  --json          print the whole response envelope as one JSON object
  --jq <expr>     filter the response envelope through a jq expression
  -r, --raw       with --jq, print string results unquoted
  --max-rows N    ask for at most N rows
  --timeout       how long to wait (0 waits as long as the query takes)

With --out alone, stdout stays empty and the row count is reported on stderr.
With --out and --json together, the file gets the CSV and stdout still gets the
envelope.

--jq filters the ENVELOPE, not the rows — the rows are CSV, and jq does not
read CSV. It is for the metadata around them:

  ronja query --file report.sql --jq -r '.rowCount'
  ronja query --file report.sql --jq '.truncated'

A truncated result is reported on stderr and still exits zero — the rows you
got are real, there are simply more of them.

A large query can be routed to bigger compute server-side and take minutes.
Raise --timeout for those; the default is generous for an interactive query and
too short for a heavy one.

A query that fails comes back as HTTP 200 with an error in the envelope. This
command treats that as a failure: the message goes to stderr and the exit code
is non-zero.`,
		// One optional positional. Two would be somebody who forgot to quote
		// their SQL, and cobra's own error says so better than a merged string
		// that half-runs.
		Args: cobra.MaximumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			sql, err := querySQL(args, file)
			if err != nil {
				return err
			}
			if maxRows < 0 {
				return fmt.Errorf("--max-rows cannot be negative (got %d)", maxRows)
			}
			// Mirrors `wf test`: a negative duration has no meaning, and zero
			// already means "no deadline", so a negative one is a typo rather
			// than an intent worth guessing at.
			if timeout < 0 {
				return fmt.Errorf("--timeout cannot be negative (got %s) — pass 0 to wait for as long as the query takes", timeout)
			}
			if rawOutput && jqExpr == "" {
				return errors.New("--raw only means something with --jq: it unquotes the STRINGS a filter produces")
			}
			// Two contradictory descriptions of stdout. Refused rather than
			// resolved by precedence, because either choice would silently
			// discard a flag the caller typed on purpose, and the fix is one
			// word either way. Checked here rather than with cobra's
			// MarkFlagsMutuallyExclusive because --json is a PERSISTENT flag on
			// the root, which that helper cannot see from a subcommand.
			if flagJSON && jqExpr != "" {
				return errors.New("--json prints the whole envelope and --jq prints part of it — pass one or the other")
			}
			// Compiled before the query runs. A typo in the filter should not
			// cost a round trip to a Batch-compute query that takes minutes.
			var filter *jqf.Filter
			if jqExpr != "" {
				var compileErr error
				if filter, compileErr = jqf.Compile(jqExpr); compileErr != nil {
					return compileErr
				}
			}
			resolved, err := resolveInstance()
			if err != nil {
				return err
			}
			client := api.New(resolved.URL, resolved.Token)

			result, err := client.Query(cmd.Context(), api.QueryInput{
				SQL:     sql,
				MaxRows: maxRows,
				Format:  api.QueryFormatCSVMeta,
			}, timeout)
			if err != nil {
				return err
			}

			// The trap, handled once. Everything below this point is a query
			// that actually ran.
			if result.Failed() {
				// In --json (or --jq) the envelope is still the answer to what
				// was asked, and a caller left with only an exit code has to
				// run the query again to find out why it failed.
				switch {
				case filter != nil:
					if emitErr := emitFiltered(filter, result, rawOutput); emitErr != nil {
						return emitErr
					}
				case flagJSON:
					if emitErr := emitJSON(result); emitErr != nil {
						return emitErr
					}
				}
				return fmt.Errorf("query failed: %s", strings.TrimSpace(result.Error))
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
			if filter != nil {
				return emitFiltered(filter, result, rawOutput)
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
	cmd.Flags().IntVar(&maxRows, "max-rows", 0,
		"maximum rows to return (default: the server's cap)")
	cmd.Flags().DurationVar(&timeout, "timeout", api.DefaultQueryTimeout,
		"how long to wait for the query (0 waits as long as it takes)")
	cmd.Flags().StringVar(&jqExpr, "jq", "",
		"filter the response envelope through a jq expression")
	cmd.Flags().BoolVarP(&rawOutput, "raw", "r", false,
		"with --jq, print string results unquoted")
	return cmd
}

// emitFiltered runs a jq filter over the query envelope.
//
// The envelope is re-encoded rather than filtered from the raw response,
// because by this point it has already been decoded into a struct. That is not
// a lossy round trip — QueryResult carries no omitempty and mirrors the wire
// shape field for field, which is exactly the property `--json` already
// depends on.
func emitFiltered(filter *jqf.Filter, result *api.QueryResult, raw bool) error {
	encoded, err := json.Marshal(result)
	if err != nil {
		return fmt.Errorf("encode the query envelope for --jq: %w", err)
	}
	results, err := filter.RunBytes(encoded)
	if err != nil {
		return err
	}
	return jqf.Render(os.Stdout, results, raw)
}

// querySQL resolves the SQL from exactly one source.
//
// The precedence is argument, then --file, then a pipe — and the two states
// that must not silently become each other are "nothing was given" and "an
// empty thing was given". Both are errors here, because a query nobody wrote
// cannot have a meaningful answer, and running one anyway would spend a round
// trip to be told so less clearly.
func querySQL(args []string, file string) (string, error) {
	var sql string
	switch {
	case len(args) == 1 && file != "":
		return "", errors.New("give the SQL as an argument or with --file, not both")

	case len(args) == 1:
		sql = args[0]

	case file == "-":
		body, err := readStdin("--file -")
		if err != nil {
			return "", err
		}
		sql = string(body)

	case file != "":
		body, err := os.ReadFile(file)
		if err != nil {
			return "", fmt.Errorf("read %s: %w", file, err)
		}
		sql = string(body)

	case !isTerminal(os.Stdin):
		// Piped in with no flag at all — the shape `cat report.sql | ronja
		// query` takes.
		body, err := readStdin("query")
		if err != nil {
			return "", err
		}
		sql = string(body)

	default:
		// A terminal, and no SQL. Prompting is the one thing this must not do.
		return "", errors.New("no SQL given — pass it as an argument, with --file <path>, or pipe it in")
	}

	if strings.TrimSpace(sql) == "" {
		return "", errors.New("the SQL is empty")
	}
	return sql, nil
}
