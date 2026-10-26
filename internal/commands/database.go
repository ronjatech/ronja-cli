package commands

import (
	"errors"
	"strings"

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
  ronja db env <database-id>     PG* variables for psql and pg_dump (eval it)
  ronja db connect <database-id> open psql, read-only

All of them are admin-only, as the underlying API is.

` + "`db env`" + ` and ` + "`db connect`" + ` reach the database from OUTSIDE Ronja, with
your own Postgres tools, over a read-only session:

  eval "$(ronja db env mdb-abc)" && pg_dump -Fc > dump.pgc

Every command runs on exactly the database you name. To try a change without
touching live data, use a dev Space: its own database, named by its own id.

Creating, listing and deleting databases stay on plain HTTP:

  ronja api -X POST /api/v2/database -d '{"name":"crm"}'
  ronja api /api/v2/database/query
  ronja api -X POST /api/v2/database/<id>/user -d '{"access":"write","featureID":"<feature-id>"}'

That last one is not optional: a new database has no connection roles, and
` + "`db sql`" + ` runs as the write role, so mint one before your first statement.`,
	}
	db.AddCommand(newDatabaseSQLCmd(), newDatabaseMigrateCmd(),
		newDatabaseEnvCmd(), newDatabaseConnectCmd(), newRetiredDatabasePromoteCmd())
	return db
}

// errDevCopiesRemoved is the server's refusal for a caller with no role, word for
// word: the CLI cannot see the caller's role. Keep it on one line, because
// backend/lib/devcopyremoved's test matches it in this file.
var errDevCopiesRemoved = errors.New(devCopiesRemovedSentence) //nolint:staticcheck // ST1005: the sentence is shared verbatim with the server's refusal

const devCopiesRemovedSentence = "Dev copies of databases were removed. To test without touching live data, use a dev Space: ask an admin to create one, then build and test there against that Space's own database, and build the finished work in your real Space."

// bindRetiredEnvFlag adds the hidden --env that exists only to refuse stale
// callers by name rather than "unknown flag". See BL-291d.
func bindRetiredEnvFlag(cmd *cobra.Command, env *string) {
	cmd.Flags().StringVar(env, "env", "", "")
	_ = cmd.Flags().MarkHidden("env")
}

// refusingRetiredEnv is a command's Args validator with the refusal ahead of
// its arity check. Cobra validates arguments before PreRunE, so this is the
// only hook where `--env dev` with a missing argument is still refused by name,
// and it runs before anything is read or sent.
func refusingRetiredEnv(env *string, arity cobra.PositionalArgs) cobra.PositionalArgs {
	return func(cmd *cobra.Command, args []string) error {
		if err := refuseRetiredEnv(*env); err != nil {
			return err
		}
		return arity(cmd, args)
	}
}

// refuseRetiredEnv lets "prod" through because it always meant the database
// named; any other non-blank value once meant a copy.
func refuseRetiredEnv(env string) error {
	switch strings.ToLower(strings.TrimSpace(env)) {
	case "", "prod":
		return nil
	}
	return errDevCopiesRemoved
}

// newRetiredDatabasePromoteCmd exists only to refuse stale callers by name
// rather than "unknown command". See BL-291d.
func newRetiredDatabasePromoteCmd() *cobra.Command {
	return &cobra.Command{
		Use:    "promote",
		Hidden: true,
		Long:   devCopiesRemovedSentence,
		// The old --dry-run and --yes must reach the refusal, not "unknown flag".
		DisableFlagParsing: true,
		RunE: func(*cobra.Command, []string) error {
			return errDevCopiesRemoved
		},
	}
}
