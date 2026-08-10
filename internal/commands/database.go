package commands

import (
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

Both are admin-only, as the underlying API is.

Creating, listing and deleting databases stay on plain HTTP:

  ronja api -X POST /api/v2/database -d '{"name":"crm"}'
  ronja api /api/v2/database/query
  ronja api -X POST /api/v2/database/<id>/user -d '{"access":"write","featureID":"<id>"}'

That last one is not optional: a new database has no connection roles, and
` + "`db sql`" + ` runs as the write role, so mint one before your first statement.`,
	}
	db.AddCommand(newDatabaseSQLCmd(), newDatabaseMigrateCmd())
	return db
}
