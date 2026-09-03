package commands

import (
	"context"
	"errors"
	"fmt"
	"os"
	"strings"
	"time"

	"github.com/ronjatech/ronja-cli/internal/api"
	"github.com/ronjatech/ronja-cli/internal/config"
	"github.com/ronjatech/ronja-cli/internal/dbdir"
	"github.com/ronjatech/ronja-cli/internal/wfdir"
	"github.com/spf13/cobra"
)

// `ronja db migrate` keeps a folder of .sql files in step with a managed
// database's own migration ledger.
//
// The design decision worth knowing is what this does NOT do: it keeps no local
// baseline and computes no hashes. The database's ledger already records exactly
// what was applied and a digest of the bytes it applied, and the server's
// migration endpoint is name-idempotent — it skips what matches, applies what is
// new, and refuses the whole set when something has been edited after the fact.
//
// So `status` is a dry run of `push`, not a local comparison. That is not just
// less code: a client computing its own hashes would have to agree with the
// server about whitespace forever, and the first time it did not, every applied
// migration would report as drifted. Drift is the one signal that must never cry
// wolf, so the only thing entitled to raise it is the thing that stored the hash.
func newDatabaseMigrateCmd() *cobra.Command {
	migrate := &cobra.Command{
		Use:   "migrate",
		Short: "Apply a folder of .sql migrations to a managed database",
		Long: `Apply a folder of .sql migrations to a managed database.

Put your migrations in a migrations/ folder. Each file is one migration, named
after the file:

  migrations/0001_leads.sql      -> the migration "0001_leads"
  migrations/0002_add_score.sql  -> the migration "0002_add_score"

They apply in filename order, so zero-pad the prefix — 10_x.sql sorts BEFORE
9_x.sql, and order is recorded in the ledger once it has run.

  ronja db migrate status     what is applied, pending or drifted — changes nothing
  ronja db migrate push       apply everything not yet applied

Both send the whole folder every time and let the database decide what is new;
running push twice is safe, and the second run reports everything as skipped.

The database is remembered per instance and organization after the first
successful push, so --database is only needed once:

  ronja db migrate push --database mdb-abc

DRIFT is a migration that is already in the ledger under DIFFERENT SQL — you
edited a file after it ran. push refuses the whole set and names the file. The
fix is always a new migration, never an edit to the old one: the ledger records
what actually ran, and rewriting history would make it a lie.

--env dev runs against the database's DEV COPY instead — a schema-identical
sandbox with its own ledger, which is where a migration you are unsure of
belongs:

  ronja db migrate push --env dev     apply to the dev copy
  ronja db promote <database-id>      move what worked onto production

You never name the copy: the server resolves it from the production id, which
is the only id this CLI ever sees or records.`,
	}
	migrate.AddCommand(newDatabaseMigrateStatusCmd(), newDatabaseMigratePushCmd())
	return migrate
}

func newDatabaseMigrateStatusCmd() *cobra.Command {
	var (
		database string
		dir      string
		env      string
		timeout  time.Duration
	)
	cmd := &cobra.Command{
		Use:   "status",
		Short: "Report which migrations are applied, pending or drifted",
		Long: `Report which migrations are applied, pending or drifted.

This is a dry run of push: the server compares your folder against the
database's ledger and reports what it WOULD do. Nothing is applied, no
transaction is opened, and no DDL runs, so it is safe to run as often as you
like.

Exits non-zero when anything has drifted — that is a broken state, not a
status worth reporting quietly.

--env dev asks the same question of the database's dev copy, which keeps its
own ledger and therefore has its own answer.`,
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			return runMigrate(cmd.Context(), migrateOpts{
				dir: dir, database: database, env: env, timeout: timeout, dryRun: true,
			})
		},
	}
	bindMigrateFlags(cmd, &database, &dir, &env, &timeout)
	return cmd
}

func newDatabaseMigratePushCmd() *cobra.Command {
	var (
		database string
		dir      string
		env      string
		timeout  time.Duration
	)
	cmd := &cobra.Command{
		Use:   "push",
		Short: "Apply every migration not yet in the database's ledger",
		Long: `Apply every migration not yet in the database's ledger.

The whole folder is sent and applied in ONE transaction: if the last migration
fails, none of them applied. Migrations already in the ledger with identical
SQL are skipped, so running this twice is safe and the second run changes
nothing.

Refuses the whole set if any migration has drifted. Run ` + "`status`" + ` first if you
want to see what would happen.

--env dev applies to the database's dev copy instead, so a change can be tried
where getting it wrong costs nothing. ` + "`ronja db promote`" + ` then moves what
worked onto production.`,
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			return runMigrate(cmd.Context(), migrateOpts{
				dir: dir, database: database, env: env, timeout: timeout, dryRun: false,
			})
		},
	}
	bindMigrateFlags(cmd, &database, &dir, &env, &timeout)
	return cmd
}

func bindMigrateFlags(cmd *cobra.Command, database, dir, env *string, timeout *time.Duration) {
	cmd.Flags().StringVar(database, "database", "",
		"managed database id (remembered per instance after the first push)")
	cmd.Flags().StringVar(dir, "dir", ".",
		"folder holding the migrations/ directory")
	cmd.Flags().StringVar(env, "env", string(api.EnvironmentProd),
		`which copy to run against: "prod" or "dev" (the database's dev copy)`)
	cmd.Flags().DurationVar(timeout, "timeout", api.DefaultMigrationTimeout,
		"how long to wait (0 waits as long as it takes)")
}

type migrateOpts struct {
	dir      string
	database string
	// env is the RAW flag value, validated in runMigrate so the failure names
	// --env rather than arriving as a server 400 about a query parameter the
	// caller never typed.
	env     string
	timeout time.Duration
	dryRun  bool
}

func runMigrate(ctx context.Context, opts migrateOpts) error {
	if opts.timeout < 0 {
		return fmt.Errorf("--timeout cannot be negative (got %s) — pass 0 to wait for as long as it takes", opts.timeout)
	}
	env, err := parseEnvFlag(opts.env)
	if err != nil {
		return err
	}
	migrations, err := dbdir.ReadMigrations(opts.dir)
	if err != nil {
		return err
	}

	resolved, err := resolveInstance()
	if err != nil {
		return err
	}
	client := api.New(resolved.URL, resolved.Token)

	databaseID, key, err := resolveDatabaseBinding(ctx, resolved, opts)
	if err != nil {
		return err
	}

	result, err := client.ApplyMigrations(ctx, databaseID, api.MigrationSetInput{
		Migrations: migrations,
		DryRun:     opts.dryRun,
	}, env, opts.timeout)
	if err != nil {
		return err
	}

	// Record the binding only on a successful PUSH. A dry run is a question, and
	// a folder should not become bound to a database by asking about it — least
	// of all to one the push would have been refused against.
	//
	// The environment is deliberately NOT part of this condition. The binding
	// records the PRODUCTION database id — the only id this CLI ever holds — and
	// a dev push names exactly that id, so an --env dev push binds the folder to
	// the same database a prod push would. Skipping the write for dev would leave
	// a folder whose first successful push is remembered nowhere; keying the
	// binding by environment would invent a second id that does not exist.
	if !opts.dryRun && opts.database != "" {
		binding, lerr := dbdir.Load(opts.dir)
		if lerr == nil {
			if serr := binding.Save(opts.dir, key, databaseID); serr != nil {
				// Not fatal: the migrations applied, which is what was asked for.
				// Losing the convenience of a remembered id is worth a warning, not
				// an error that implies the work did not happen.
				fmt.Fprintf(os.Stderr, "  Note: could not record the database binding: %v\n", serr)
			}
		}
	}

	// Which database this landed on, said BEFORE anything else and in EVERY mode
	// — including --json, whose envelope goes to stdout for something to parse.
	reportEnvironment(env, result.Environment, result.DatabaseID, result.ParentDatabaseID)

	if flagJSON {
		return emitJSON(result)
	}
	return reportMigrations(result)
}

// resolveDatabaseBinding works out which database to target: the flag if given,
// otherwise the folder's remembered binding.
//
// The organization is resolved LAZILY, and only when it is needed — a folder
// with an explicit --database and nothing to look up costs no round trip.
func resolveDatabaseBinding(ctx context.Context, resolved *config.Resolved, opts migrateOpts) (string, wfdir.InstanceKey, error) {
	if opts.database != "" {
		// An explicit id still needs the organization resolved, because a
		// successful push records the binding under it — writing an untagged entry
		// would produce exactly the ambiguity the key exists to prevent. This is
		// the one round trip this path costs, and only when signed in via an env
		// token that carries no organization of its own.
		if err := ensureTenant(ctx, resolved); err != nil {
			return "", wfdir.InstanceKey{URL: resolved.URL}, err
		}
		return opts.database, wfdir.InstanceKey{URL: resolved.URL, TenantID: resolved.TenantID}, nil
	}

	key := wfdir.InstanceKey{URL: resolved.URL, TenantID: resolved.TenantID}

	binding, err := dbdir.Load(opts.dir)
	if err != nil {
		return "", key, err
	}
	databaseID, err := binding.DatabaseFor(key)
	if err != nil {
		if errors.Is(err, wfdir.ErrAmbiguousInstance) {
			return "", key, fmt.Errorf("this folder is bound to several organizations on %s — pass --database to say which one you mean", resolved.URL)
		}
		return "", key, err
	}
	if databaseID == "" {
		return "", key, fmt.Errorf("this folder is not bound to a database on %s yet — pass --database <id> once and it will be remembered.\n"+
			"List your databases with: ronja api /api/v2/database/query", resolved.URL)
	}
	return databaseID, key, nil
}

// reportMigrations prints the human summary on stderr and nothing on stdout.
//
// Nothing on stdout is the point: this command's output is a report, not data,
// and a caller that wants it machine-readable asks for --json. Splitting it that
// way keeps `db migrate status --json | jq` clean.
//
// Shared by `db migrate status`, `db migrate push` and `db promote`, because all
// three are the same answer — a list of migrations and what happened to each.
// Three reporters would be three vocabularies for one thing.
// Which database the set landed on is NOT printed here: reportEnvironment says
// that, and its callers run it before this so the answer precedes the verdicts
// in every mode. By the time the counts are on screen the reader has already
// decided what they mean.
func reportMigrations(r *api.MigrationSetResult) error {
	if len(r.Results) == 0 {
		// Only promote can get here: a migrations folder with no .sql files is
		// refused before any request goes out. An empty tail means the copy's
		// ledger already matches production, which is a clean repeatable no-op
		// rather than a failure — so say so and still print the notes below.
		fmt.Fprintln(os.Stderr, "  Nothing to do — there is no migration here that the target does not already have.")
		reportMigrationNotes(r)
		return nil
	}

	counts := map[string]int{}
	for _, res := range r.Results {
		counts[res.Outcome]++
		symbol := "  "
		switch res.Outcome {
		case api.MigrationApplied:
			symbol = "+ "
		case api.MigrationSkipped:
			symbol = "= "
		case api.MigrationPending:
			symbol = "> "
		case api.MigrationDrifted:
			symbol = "! "
		}
		fmt.Fprintf(os.Stderr, "  %s%-40s %s\n", symbol, res.Name, res.Outcome)
	}

	fmt.Fprintln(os.Stderr)
	if r.DryRun {
		fmt.Fprintf(os.Stderr, "  %d applied, %d pending, %d drifted (nothing was changed)\n",
			counts[api.MigrationSkipped], counts[api.MigrationPending], counts[api.MigrationDrifted])
	} else {
		fmt.Fprintf(os.Stderr, "  %d applied, %d already up to date\n",
			counts[api.MigrationApplied], counts[api.MigrationSkipped])
	}

	reportMigrationNotes(r)

	// Drift only ever reaches here from a dry run — a real apply is refused
	// server-side and returns an error instead. Exiting non-zero makes `status`
	// usable as a CI gate without parsing its output.
	if drifted := r.Drifted(); len(drifted) > 0 {
		names := make([]string, 0, len(drifted))
		for _, d := range drifted {
			names = append(names, d.Name)
		}
		return fmt.Errorf("%s drifted: %s — already applied under different SQL. "+
			"Add a new migration rather than editing one that has run; the ledger records what actually happened",
			plural(len(names), "migration"), strings.Join(names, ", "))
	}
	return nil
}

// reportMigrationNotes prints the server's advisory notes, one per line.
//
// ADVISORY, and printed as such: a note never changes the exit code and never
// stops anything. Today only promote emits them, and what they warn about is a
// migration in the tail that writes ROWS — promote replays SQL, not data, so a
// migration that seeded rows from whatever happened to be in the dev copy runs
// against production's rows instead. Only the person promoting can tell a
// deliberate backfill from a surprise, which is exactly why this is a line of
// text and not a refusal.
//
// Anything the server sends is printed verbatim, so a note added server-side
// reaches the reader without a CLI release.
func reportMigrationNotes(r *api.MigrationSetResult) {
	if len(r.Notes) == 0 {
		return
	}
	fmt.Fprintln(os.Stderr)
	for _, note := range r.Notes {
		fmt.Fprintf(os.Stderr, "  Note: %s\n", note)
	}
}
