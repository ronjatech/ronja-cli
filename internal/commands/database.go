package commands

import (
	"fmt"
	"os"

	"github.com/ronjatech/ronja-cli/internal/api"
	"github.com/spf13/cobra"
)

// `ronja db` holds the two managed-database loops that plain HTTP does badly.
//
// The doctrine is unchanged — sync verbs yes, resource verbs no — and it is what
// decides which of the fifteen /api/v2/database endpoints get a command. Neither
// of these is here for convenience:
//
//   - `db sql` has the same three problems `ronja query` has, for the same
//     reasons. Real SQL is multi-line and full of quotes, so embedding it in a
//     JSON string inside a shell string is a two-level escaping problem people
//     get wrong. The answer is a table, and the useful forms of it are "print
//     it", "write it to a file" and "hand me the envelope". A generic runner can
//     only ever give the third.
//   - `db migrate` is a stateful loop: which database, on which instance, and
//     which of these files has already been applied. That last question has to
//     be answered against the database's own ledger, and answering it by hand is
//     a sequence of calls with a comparison in the middle.
//   - `db env` and `db connect` are the `ronja env` kind of command, not a
//     wrapped endpoint: they move the credential the CLI already holds into
//     the place psql and pg_dump read it from, without it ever being displayed.
//     See database_proxy.go for why both exist and why there is no `db dump`.
//   - `db promote` is the other end of that same loop. It moves a ledger TAIL
//     between two ledgers: the shared prefix is compared position by position,
//     the divergence case has to be reported as an instruction rather than a
//     hash mismatch, and the tail applies all-or-nothing. Stateful, drift-guarded
//     and multi-step — the same test `migrate` passes, and it is not a list, a
//     browse or a delete.
//
// Everything else stays plain HTTP. There is deliberately no `db list`, no
// `db create` and no `db delete`: those are single calls that `ronja api` runs
// perfectly well, and wrapping them would make the CLI a permanently incomplete
// mirror of the API.
func newDatabaseCmd() *cobra.Command {
	db := &cobra.Command{
		Use:     "database",
		Aliases: []string{"db"},
		Short:   "Run SQL and migrations against a managed Postgres database",
		Long: `Run SQL and migrations against a managed Postgres database.

A managed database is a real Postgres database Ronja provisions and runs for
you — the thing to use when you are building a system that stores its own
state, rather than the analytical tables ` + "`ronja query`" + ` reads.

  ronja db sql <database-id> "SELECT * FROM leads"
  ronja db migrate status        what is applied, pending or drifted
  ronja db migrate push          apply the migrations/ folder
  ronja db promote <database-id> move the dev copy's migrations onto production
  ronja db env <database-id>     PG* variables for psql and pg_dump (eval it)
  ronja db connect <database-id> open psql, read-only

All of them are admin-only, as the underlying API is.

` + "`db env`" + ` and ` + "`db connect`" + ` reach the database from OUTSIDE Ronja, with
your own Postgres tools, over a read-only session:

  eval "$(ronja db env mdb-abc)" && pg_dump -Fc > dump.pgc

A database can have a DEV COPY — same schema, its own data, its own ledger.
` + "`db sql`" + ` and ` + "`db migrate`" + ` reach it with --env dev, always naming the
production database (the copy has no id you ever see), and ` + "`db promote`" + ` moves
what worked there onto production.

Creating, listing and deleting databases stay on plain HTTP:

  ronja api -X POST /api/v2/database -d '{"name":"crm"}'
  ronja api /api/v2/database/query
  ronja api -X POST /api/v2/database/<id>/user -d '{"access":"write","featureID":"<feature-id>"}'

That last one is not optional: a new database has no connection roles, and
` + "`db sql`" + ` runs as the write role, so mint one before your first statement.`,
	}
	db.AddCommand(newDatabaseSQLCmd(), newDatabaseMigrateCmd(), newDatabasePromoteCmd(),
		newDatabaseEnvCmd(), newDatabaseConnectCmd())
	return db
}

// parseEnvFlag validates a --env value before anything is sent.
//
// Locally, so a typo fails naming the FLAG the person typed. The server does
// validate it — it answers 400 for anything but "prod" or "dev" — but that
// message is about a query parameter, and the caller wrote a flag. It also fails
// after the migration set or the statement has already gone out.
//
// The accepted values come from api.ParseEnvironment so there is one definition
// of what they are; only the wording is restated here, to put --env in it.
func parseEnvFlag(raw string) (api.Environment, error) {
	env, err := api.ParseEnvironment(raw)
	if err != nil {
		return "", fmt.Errorf("--env %v", err)
	}
	return env, nil
}

// reportEnvironment says which copy of a managed database actually answered.
//
// stderr, in EVERY mode, and before anything else the command prints. `db sql`'s
// stdout is CSV somebody is parsing, and `--json` output is an envelope somebody
// is piping — but a dev result read as production's is a WRONG answer rather
// than an incomplete one, so the reader is told before they can decide what the
// rows mean.
//
// It is shared by `db sql`, `db migrate` and `db promote` because all three ask
// the same question of the same field, and three phrasings of "which database
// did this touch" would be three things to keep in step.
//
// Production answering a production request prints nothing: a banner on every
// invocation is a banner nobody reads. The two cases that speak are the ones
// where the reader could be wrong about where they are.
func reportEnvironment(requested api.Environment, environment, databaseID, parentDatabaseID string) {
	if environment == string(api.EnvironmentDev) {
		// The PRODUCTION id is what the reader typed and what they think in, so
		// name it rather than the copy's own id, which they have never seen.
		target := parentDatabaseID
		if target == "" {
			target = databaseID
		}
		fmt.Fprintf(os.Stderr,
			"  Note: this ran against the DEV COPY of %s — production was not touched.\n", target)
		return
	}
	if requested == api.EnvironmentDev {
		// Against a current instance this cannot happen: `?environment=dev` on a
		// database with no dev copy is refused server-side. An OLDER instance does
		// not know the query parameter at all and answers from production without
		// complaint — the one case where silence would be worst, because the
		// command looks like it did what was asked.
		fmt.Fprintln(os.Stderr,
			"  Warning: --env dev was asked for, but PRODUCTION answered. This instance may predate dev copies — check before trusting this result.")
	}
}
