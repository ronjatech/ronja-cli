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
// database's own migration ledger. It deliberately keeps no local baseline and
// computes no hashes: `status` is a dry run of `push`, so only the server that
// stored the hash can report drift (cli/README.md).
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

Agents: ship with ` + "`ronja propose`" + `. It is the recommended way for an agent to
apply migrations: run it in the folder that holds migrations/ (or name that
folder: ronja propose db/crm), with the --database or --stack you would give
push. The person this login belongs to approves it in Ronja, which then applies
exactly the SQL they saw. push is for people and CI applying directly.

The database is remembered per instance and organization after the first
successful push, so --database is only needed once:

  ronja db migrate push --database mdb-abc

With --stack it is remembered for that stack instead, so the databases of two
Spaces of one organization are told apart:

  ronja db migrate push --stack dev --database mdb-dev   # once
  ronja db migrate push --stack dev
  ronja db migrate push --stack main

With --stack, a database outside the Space that stack declares in the
repository's ronja.json files is refused (bind_space_mismatch) before anything
is applied.

DRIFT is a migration that is already in the ledger under DIFFERENT SQL — you
edited a file after it ran. push refuses the whole set and names the file. The
fix is always a new migration, never an edit to the old one: the ledger records
what actually ran, and rewriting history would make it a lie.

To try a migration you are unsure of, push it to a dev Space's database first
(--stack dev), then to the real one (--stack main).`,
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

Exits non-zero when anything has drifted, with or without --json (the JSON is
printed in full first) — that is a broken state, not a status worth reporting
quietly.

When migrations are pending (and none has drifted), the human report ends with
the two ways to apply them, with your own flags, unless more than 40 are
pending. Agents: ship with ` + "`ronja propose`" + `, the recommended way for an agent;
push is for people and CI applying directly.

When more than 40 migrations are not yet applied (and none has drifted), it
also prints a note that push would refuse the set and names where to split it.
That note does not change the exit code: it is a warning about the next push,
not a broken state, so a passing status does not mean the next push will
succeed.`,
		Args: refusingRetiredEnv(&env, folderNoArgs),
		RunE: func(cmd *cobra.Command, args []string) error {
			return runMigrate(cmd.Context(), migrateOpts{
				dir: dir, database: database, timeout: timeout, dryRun: true,
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

The whole folder is sent, and the migrations not yet applied run in ONE
transaction: if the last one fails, none of them applied. Migrations already in
the ledger with identical SQL are skipped, so running this twice is safe and
the second run changes nothing.

At most 40 migrations that are not yet applied run in one push, because they
share that transaction. With more, push refuses the set, applies nothing and
names the last migration that fits: move the later files out of the folder,
push, then put them back and push again. The folder itself may hold up to 1000
migrations.

Refuses the whole set if any migration has drifted, before anything runs. Run
` + "`status`" + ` first if you want to see what would happen.

Agents: ship with ` + "`ronja propose`" + `, the recommended way for an agent, rather
than push. Run it in the folder that holds migrations/, with the same
--database or --stack; the person this login belongs to approves it in Ronja,
which then applies exactly the SQL they saw.`,
		Args: refusingRetiredEnv(&env, folderNoArgs),
		RunE: func(cmd *cobra.Command, args []string) error {
			return runMigrate(cmd.Context(), migrateOpts{
				dir: dir, database: database, timeout: timeout, dryRun: false,
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
	bindRetiredEnvFlag(cmd, env)
	durationFlag(cmd, timeout, "timeout", api.DefaultMigrationTimeout,
		"how long to wait (0 waits as long as it takes)")
	// Its own flag, not the folder commands' inherited one: a migrations folder
	// has no ronja.json, so the stack names only which remembered database.
	cmd.Flags().StringVar(&flagStack, "stack", "",
		"remember and use the database for this stack (default: the one remembered without --stack)")
}

type migrateOpts struct {
	dir      string
	database string
	timeout  time.Duration
	dryRun   bool
}

func runMigrate(ctx context.Context, opts migrateOpts) error {
	if opts.timeout < 0 {
		return fmt.Errorf("--timeout cannot be negative (got %s) — pass 0 to wait for as long as it takes", opts.timeout)
	}
	if flagStack != "" {
		if err := wfdir.ValidStackName(flagStack); err != nil {
			return err
		}
	}
	migrations, err := dbdir.ReadMigrations(opts.dir)
	if err != nil {
		return err
	}

	resolve := resolveInstance
	if !opts.dryRun {
		resolve = func() (*config.Resolved, error) { return resolveDeployInstance(false) }
	}
	resolved, err := resolve()
	if err != nil {
		return err
	}
	client := api.New(resolved.URL, resolved.Token)

	databaseID, key, err := resolveDatabaseBinding(ctx, resolved, opts)
	if err != nil {
		return err
	}
	if err := checkDatabaseSpace(ctx, client, resolved, key, opts.dir, databaseID, "applied", true); err != nil {
		return err
	}

	result, err := client.ApplyMigrations(ctx, databaseID, api.MigrationSetInput{
		Migrations: migrations,
		DryRun:     opts.dryRun,
	}, opts.timeout)
	if err != nil {
		return err
	}

	// Bound only on a successful push, never by a dry run.
	if !opts.dryRun && opts.database != "" {
		binding, lerr := dbdir.Load(opts.dir)
		if lerr == nil {
			if serr := binding.SaveStack(opts.dir, key, flagStack, databaseID); serr != nil {
				// Not fatal: the migrations applied.
				fmt.Fprintf(os.Stderr, "  Note: could not record the database binding: %v\n", serr)
			}
		}
	}

	if flagJSON {
		if err := emitJSON(result); err != nil {
			return err
		}
		return migrationDriftError(result)
	}
	return reportMigrations(result, opts)
}

// resolveDatabaseBinding works out which database to target: the flag if given,
// otherwise the folder's remembered binding.
func resolveDatabaseBinding(ctx context.Context, resolved *config.Resolved, opts migrateOpts) (string, wfdir.InstanceKey, error) {
	if opts.database != "" {
		// The organization is still needed: a push records the binding under it,
		// and an untagged entry would be ambiguous. ensureTenant asks the server
		// only when the credential carries no organization.
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
	databaseID, err := binding.DatabaseForStack(key, flagStack)
	if err != nil {
		if errors.Is(err, wfdir.ErrAmbiguousInstance) {
			return "", key, fmt.Errorf("this folder is bound to several organizations on %s — pass --database to say which one you mean", resolved.URL)
		}
		return "", key, err
	}
	if databaseID == "" && flagStack != "" {
		return "", key, fmt.Errorf("this folder remembers no database for stack %q on %s yet — pass --stack %s --database <id> once and it will be remembered for that stack.\n"+
			"List your databases with: ronja api /api/v2/database/query", flagStack, resolved.URL, flagStack)
	}
	if databaseID == "" {
		return "", key, fmt.Errorf("this folder is not bound to a database on %s yet — pass --database <id> once and it will be remembered.\n"+
			"List your databases with: ronja api /api/v2/database/query", resolved.URL)
	}
	return databaseID, key, nil
}

// reportMigrations prints the human summary on stderr and nothing on stdout.
func reportMigrations(r *api.MigrationSetResult, opts migrateOpts) error {
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
	if r.DryRun && counts[api.MigrationPending] > 0 && counts[api.MigrationPending] <= maxPendingPerPush && counts[api.MigrationDrifted] == 0 {
		fmt.Fprintln(os.Stderr)
		fmt.Fprintf(os.Stderr, "  %s\n", migrateNextStep(opts))
	}
	return migrationDriftError(r)
}

// migrateNextStep names both ways to apply what status found pending, with the
// caller's own flags so each command targets the database status checked.
// --profile and --url are carried as typed: an environment variable that chose
// them chooses them again on the next run.
func migrateNextStep(opts migrateOpts) string {
	var common []string
	if flagProfile != "" {
		common = append(common, "--profile", shellArg(flagProfile))
	}
	if flagURL != "" {
		common = append(common, "--url", shellArg(flagURL))
	}
	if opts.database != "" {
		common = append(common, "--database", shellArg(opts.database))
	}
	if flagStack != "" {
		common = append(common, "--stack", shellArg(flagStack))
	}
	propose, push := append([]string{"ronja", "propose"}, common...), append([]string{"ronja", "db", "migrate", "push"}, common...)
	if opts.dir != "" && opts.dir != "." {
		positional := opts.dir
		if strings.HasPrefix(positional, "-") {
			positional = "./" + positional
		}
		propose = append(propose, shellArg(positional))
		push = append(push, "--dir", shellArg(opts.dir))
	}
	return fmt.Sprintf("Recommended for agents: %s, then the step it names; the person this login belongs to approves the change list in Ronja, which then applies exactly the SQL it shows. To apply it yourself now: %s",
		strings.Join(propose, " "), strings.Join(push, " "))
}

// shellArg leaves a plain id or path as it is, so the line reads as typed, and
// quotes anything else for pasting into a shell.
func shellArg(s string) string {
	for _, r := range s {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9', strings.ContainsRune("._/:@=+-", r):
		default:
			return shellQuote(s)
		}
	}
	return s
}

// migrationDriftError is the verdict of both output modes, called after the
// report: an error on drift makes `status` a CI gate. Drift arrives only from a
// dry run, since a real apply is refused server-side.
func migrationDriftError(r *api.MigrationSetResult) error {
	drifted := r.Drifted()
	if len(drifted) == 0 {
		return nil
	}
	names := make([]string, 0, len(drifted))
	for _, d := range drifted {
		names = append(names, d.Name)
	}
	return fmt.Errorf("%s drifted: %s — already applied under different SQL. "+
		"Add a new migration rather than editing one that has run; the ledger records what actually happened",
		plural(len(names), "migration"), strings.Join(names, ", "))
}

// reportMigrationNotes prints the server's advisory notes verbatim, one per
// line; a note never changes the exit code.
func reportMigrationNotes(r *api.MigrationSetResult) {
	if len(r.Notes) == 0 {
		return
	}
	fmt.Fprintln(os.Stderr)
	for _, note := range r.Notes {
		fmt.Fprintf(os.Stderr, "  Note: %s\n", note)
	}
}
