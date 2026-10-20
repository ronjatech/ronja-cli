package commands

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"strings"
	"time"

	"github.com/ronjatech/ronja-cli/internal/api"
	"github.com/ronjatech/ronja-cli/internal/jqf"
	"github.com/spf13/cobra"
)

// `ronja query` is the one read that earns a verb despite the doctrine, for
// what `ronja api` does badly: two-level quoting of SQL inside JSON inside a
// shell string, a table-shaped answer, and a failed query that is HTTP 200 with
// `error` set (a generic runner exits zero on it).
func newQueryCmd() *cobra.Command {
	var (
		file      string
		out       string
		maxRows   int
		timeout   time.Duration
		jqExpr    string
		rawOutput bool
		attach    []string
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

  ronja query --file report.sql --jq '.rowCount' -r
  ronja query --file report.sql --jq '.truncated'

--jq prints EVERY value the expression matched, or it fails and prints nothing;
there is no cap and no prefix. Two bounds can make it fail: a work and memory
budget, scaled to the size of the envelope, which refuses a filter that
manufactures data rather than selecting it; and a 10-second deadline on one
application of the filter, separate from --timeout, which bounds the QUERY.

A truncated result is reported on stderr and still exits zero — the rows you
got are real, there are simply more of them.

The reporting timezone the query ran at is also printed on stderr (and carried
as .zoneUsed). The CLI sends no client timezone, so queries read at UTC while
the browser reads at your own zone — a rollup bucketed by day, week or month
can legitimately differ between the two. Check it first when CLI numbers
disagree with the app.

Authoring advice about the SQL — a backslash escape that reads differently
under raw and legacy escape handling, say — is printed on stderr too, and
carried as .advice. It is never a failure: a query with advice on it ran.

A heavy query can take several minutes; raise --timeout for those (the default
is generous for an interactive query and too short for a heavy one). One that
runs out of memory or time fails with an error that says so; narrow the query.

A query that fails comes back as HTTP 200 with an error in the envelope. This
command treats that as a failure: the message goes to stderr and the exit code
is non-zero.`,
		// The --jq guard runs in Args, before the arity check: `--jq -r
		// '.rowCount'` binds "-r" as the expression and leaves '.rowCount' as
		// the SQL, a round trip that fails server-side with a syntax error
		// nobody typed.
		Args: func(cmd *cobra.Command, args []string) error {
			if err := refuseFlagAsJQExpr(cmd, jqExpr); err != nil {
				return err
			}
			return cobra.MaximumNArgs(1)(cmd, args)
		},
		RunE: func(cmd *cobra.Command, args []string) error {
			sql, err := querySQL(args, file)
			if err != nil {
				return err
			}
			if maxRows < 0 {
				return fmt.Errorf("--max-rows cannot be negative (got %d)", maxRows)
			}
			if timeout < 0 {
				return fmt.Errorf("--timeout cannot be negative (got %s) — pass 0 to wait for as long as the query takes", timeout)
			}
			attached, err := parseQueryAttach(attach)
			if err != nil {
				return err
			}
			if rawOutput && jqExpr == "" {
				return errors.New("--raw only means something with --jq: it unquotes the STRINGS a filter produces")
			}
			// Not MarkFlagsMutuallyExclusive: --json is a PERSISTENT root flag,
			// which that helper cannot see from a subcommand.
			if flagJSON && jqExpr != "" {
				return errors.New("--json prints the whole envelope and --jq prints part of it — pass one or the other")
			}
			// Compiled before the query, so a filter typo costs no round trip.
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
				Attach:  attached,
			}, timeout)
			if err != nil {
				return err
			}

			// Before the failure branch: the advice is often why the query
			// failed.
			printQueryAdvice(os.Stderr, result.Advice)

			// A failed query is HTTP 200 with `error` set; below here it ran.
			if result.Failed() {
				// The envelope still says why it failed; an exit code alone
				// would force a re-run.
				switch {
				case filter != nil:
					if emitErr := emitFiltered(cmd.Context(), filter, result, rawOutput); emitErr != nil {
						return emitErr
					}
				case flagJSON:
					if emitErr := emitJSON(result); emitErr != nil {
						return emitErr
					}
				}
				return fmt.Errorf("query failed: %s", strings.TrimSpace(result.Error))
			}

			// Narration is always stderr: on stdout it would land inside the
			// CSV. The zone prints on every run because the CLI sends no client
			// timezone (UTC) while the app reads at the user's zone.
			if result.ZoneUsed != "" {
				fmt.Fprintf(os.Stderr, "  Reporting timezone: %s\n", result.ZoneUsed)
			}
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
				return emitFiltered(cmd.Context(), filter, result, rawOutput)
			}
			if flagJSON {
				return emitJSON(result)
			}
			if out == "" {
				// Verbatim, no added newline: a byte the server did not send is
				// a byte a diff will find.
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
	// HIDDEN while the HTTP join stays unpublished; it unhides with the
	// server field (DATABASE.md → "What is documented, and what is not").
	cmd.Flags().StringArrayVar(&attach, "attach", nil,
		"join a database into the query as alias=secret-id (repeatable)")
	_ = cmd.Flags().MarkHidden("attach")
	return cmd
}

// parseQueryAttach splits on the FIRST = only, like --param; an empty half or
// an alias given twice is a usage error rather than a silently dropped database.
func parseQueryAttach(specs []string) (map[string]string, error) {
	if len(specs) == 0 {
		return nil, nil
	}
	out := make(map[string]string, len(specs))
	for _, spec := range specs {
		alias, secret, ok := strings.Cut(spec, "=")
		if !ok || strings.TrimSpace(alias) == "" || strings.TrimSpace(secret) == "" {
			return nil, fmt.Errorf("--attach takes alias=secret, got %q", spec)
		}
		if _, dup := out[alias]; dup {
			return nil, fmt.Errorf("--attach %s was given more than once; give each database its own alias", alias)
		}
		out[alias] = secret
	}
	return out, nil
}

// printQueryAdvice prints one line per (kind, message), not per entry: the
// server sends one entry per occurrence, so the differing snippets are gathered
// onto that line. Grouped on the pair, not kind alone, because an older or newer
// instance sends empty or unknown kinds that would merge unrelated sentences.
// Tolerant of any missing field; only an entry with no message is dropped.
func printQueryAdvice(w io.Writer, advice []api.QueryAdvice) {
	type group struct {
		message  string
		snippets []string
		seen     map[string]bool
		dropped  int
	}
	var order []string
	byKey := map[string]*group{}
	for _, note := range advice {
		message := strings.TrimSpace(note.Message)
		if message == "" {
			continue
		}
		key := note.Kind + "\x00" + message
		g, ok := byKey[key]
		if !ok {
			g = &group{message: message, seen: map[string]bool{}}
			byKey[key] = g
			order = append(order, key)
		}
		snippet := strings.TrimSpace(note.Snippet)
		if snippet == "" || g.seen[snippet] {
			continue
		}
		g.seen[snippet] = true
		if len(g.snippets) >= queryAdviceSnippetsPerLine {
			g.dropped++
			continue
		}
		g.snippets = append(g.snippets, snippet)
	}
	for _, key := range order {
		g := byKey[key]
		switch {
		case len(g.snippets) == 0:
			fmt.Fprintf(w, "  advice: %s\n", g.message)
		case g.dropped > 0:
			fmt.Fprintf(w, "  advice: %s (at %s and %d more)\n",
				g.message, strings.Join(g.snippets, ", "), g.dropped)
		default:
			fmt.Fprintf(w, "  advice: %s (at %s)\n", g.message, strings.Join(g.snippets, ", "))
		}
	}
}

// queryAdviceSnippetsPerLine is a floor under the server's own per-kind cap.
// What it drops is counted on the line: a bounded list read as complete is how
// somebody fixes five of twenty and calls it done.
const queryAdviceSnippetsPerLine = 5

// emitFiltered re-encodes the decoded envelope, which is lossless only while
// QueryResult carries no omitempty and mirrors the wire shape (as `--json`
// relies on too). It goes through runFilter for a deadline: --timeout bounds
// the query, and cmd.Context() carries none.
func emitFiltered(ctx context.Context, filter *jqf.Filter, result *api.QueryResult, raw bool) error {
	encoded, err := json.Marshal(result)
	if err != nil {
		return fmt.Errorf("encode the query envelope for --jq: %w", err)
	}
	results, err := runFilter(ctx, filter, encoded)
	if err != nil {
		return err
	}
	return jqf.Render(os.Stdout, results, raw)
}

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
		body, err := readStdin("query")
		if err != nil {
			return "", err
		}
		sql = string(body)

	default:
		// A terminal and no SQL: never prompt.
		return "", errors.New("no SQL given — pass it as an argument, with --file <path>, or pipe it in")
	}

	if strings.TrimSpace(sql) == "" {
		return "", errors.New("the SQL is empty")
	}
	return sql, nil
}
