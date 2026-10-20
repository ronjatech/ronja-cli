package commands

import (
	"fmt"
	"strings"
	"time"

	"github.com/ronjatech/ronja-cli/internal/api"
	"github.com/spf13/cobra"
)

// It sends no migrations: the server computes the tail from the two ledgers,
// because a client choosing what to move would be a second opinion about what
// the copy has (the same reason `db migrate` computes no hashes).
func newDatabasePromoteCmd() *cobra.Command {
	var (
		dryRun  bool
		yes     bool
		timeout time.Duration
	)

	cmd := &cobra.Command{
		Use:   "promote <database-id>",
		Short: "Apply a dev copy's new migrations to its production database",
		Long: `Apply a dev copy's new migrations to its production database.

Name the PRODUCTION database, not the copy — the copy has no id you ever see:

  ronja db promote mdb-abc --dry-run    what would be promoted
  ronja db promote mdb-abc              promote it (asks first)
  ronja db promote mdb-abc --yes        promote it without being asked

A real promote is the one command here that changes PRODUCTION's schema, so it
asks before it runs. --yes answers the question up front, and is required when
there is no terminal to ask on — a CI job or an agent that did not pass it did
not mean to change production. --dry-run never asks: it changes nothing.

What promotes is exactly the migrations the copy's ledger has past
production's. There is no list to send and nothing to choose. They apply
through the same path ` + "`db migrate push`" + ` uses — one transaction,
all-or-nothing, name-idempotent, ledgered — so promoting twice is safe and the
second run reports everything as skipped.

This does NOT copy data, and it does not sync the copy back down. It replays
SQL. A migration that seeded rows from whatever was in the dev copy runs
against production's rows instead, which is a different result; any migration
in the tail that writes rows is called out in a note. The note is advisory —
only you can tell a deliberate backfill from a surprise.

DIVERGENCE is the failure worth knowing about. Before anything runs, the shared
prefix of the two ledgers is compared position by position. If they disagree —
production moved on while you were working, or a migration was edited at a
position production already has — the promote is refused and nothing is
applied. The fix is to refresh the dev copy, which rebases it on production,
then re-apply your migrations on top and promote again.

Exits non-zero on divergence and on drift, so this works as a deployment gate
without parsing its output.`,
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			databaseID := strings.TrimSpace(args[0])
			if databaseID == "" {
				return fmt.Errorf("no database id given")
			}
			if timeout < 0 {
				return fmt.Errorf("--timeout cannot be negative (got %s) — pass 0 to wait for as long as it takes", timeout)
			}

			// Asked BEFORE the instance is resolved: nothing is sent on the
			// strength of an unconfirmed command.
			ok, err := confirmPromote(databaseID, dryRun, yes)
			if err != nil {
				return err
			}
			if !ok {
				return fmt.Errorf("cancelled — nothing was promoted to %s", databaseID)
			}

			resolved, err := resolveInstance()
			if err != nil {
				return err
			}
			client := api.New(resolved.URL, resolved.Token)

			// Every refusal (diverged history, no dev copy, copy not ready, id is a
			// copy) arrives as a 400 with the server's instruction; deliberately not
			// re-classified locally, which would be a second, staler definition.
			result, err := client.Promote(cmd.Context(), databaseID, api.PromoteInput{
				DryRun: dryRun,
			}, timeout)
			if err != nil {
				return err
			}

			// Prints nothing while promote only targets production; called anyway
			// so a promote that ever landed elsewhere would say so.
			reportEnvironment(api.EnvironmentProd, result.Environment, result.DatabaseID, result.ParentDatabaseID)

			if flagJSON {
				return emitJSON(result)
			}
			// Drift reaches the reporter, which turns it into the error return.
			return reportMigrations(result)
		},
	}

	cmd.Flags().BoolVar(&dryRun, "dry-run", false,
		"report what would be promoted and change nothing")
	cmd.Flags().BoolVar(&yes, "yes", false,
		"confirm the promote without a prompt (required when there is no terminal)")
	cmd.Flags().DurationVar(&timeout, "timeout", api.DefaultMigrationTimeout,
		"how long to wait (0 waits as long as it takes)")
	return cmd
}

// confirmPromote gates the one `db` verb that changes production's schema; with
// no terminal (or --json) --yes is required, not assumed. A dry run is exempt:
// asking there would train the operator to answer without reading.
func confirmPromote(databaseID string, dryRun, yes bool) (bool, error) {
	if dryRun {
		return true, nil
	}
	return confirm(
		fmt.Sprintf("Promote %s's dev-copy migrations onto production? They apply in one transaction and become part of its history.", databaseID),
		fmt.Sprintf("changes the schema of production database %s", databaseID),
		yes,
	)
}
