package commands

import (
	"fmt"
	"strings"
	"time"

	"github.com/ronjatech/ronja-cli/internal/api"
	"github.com/spf13/cobra"
)

// `ronja db promote` is the second half of the dev-copy loop `db migrate --env
// dev` opens: it moves the migrations the copy's ledger has and production's
// does not onto production.
//
// It sends no migrations. What promotes is the TAIL the server computes from the
// two ledgers, and that is not a convenience — a client choosing which
// migrations to move would be a second opinion about what the copy actually has,
// and the first time the two disagreed the ledgers would stop being comparable
// at all. Same reason `db migrate` computes no hashes.
//
// It prints through reportMigrations, the same reporter `migrate status` and
// `migrate push` use, because it is the same answer: a list of migrations and
// what happened to each. The outcomes are the existing four.
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

			// Asked BEFORE the instance is resolved: nothing should be sent
			// anywhere on the strength of a command the operator has not
			// confirmed yet.
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

			// Every refusal this command has — a diverged history, no dev copy, a
			// copy that is not ready, an id that is itself a copy — arrives as an
			// HTTP 400 carrying a written instruction. That is already a non-zero
			// exit with the server's own message on stderr, so there is deliberately
			// no second mechanism here: a local re-classification of those cases
			// would be a second, staler description of when a promote is impossible.
			result, err := client.Promote(cmd.Context(), databaseID, api.PromoteInput{
				DryRun: dryRun,
			}, timeout)
			if err != nil {
				return err
			}

			// Promote always targets production — it refuses in a dev session
			// server-side — so this prints nothing today. It is called anyway so
			// that "which database answered" comes from ONE place for all three
			// `db` commands, and a promote that ever did land elsewhere would say
			// so rather than being the one report that stays silent.
			reportEnvironment(api.EnvironmentProd, result.Environment, result.DatabaseID, result.ParentDatabaseID)

			if flagJSON {
				return emitJSON(result)
			}
			// Drift reaches the reporter rather than an error return, and the
			// reporter turns it into one. Same rule as `migrate status`.
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

// confirmPromote is the gate in front of the one `db` verb that changes
// production's schema.
//
// It reuses `confirm` — the same "prompt, or --yes" rule the discard commands
// keep — rather than growing a second idiom: no terminal (or --json, which says
// a program is reading) means nobody is there to ask, so --yes is required
// instead of assumed.
//
// A DRY RUN is exempt because it changes nothing. Making it ask would train the
// operator to answer the question without reading it, which is the opposite of
// what the gate is for.
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
