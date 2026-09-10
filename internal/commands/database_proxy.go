package commands

import (
	"fmt"
	"os"
	"os/exec"
	"strings"

	"github.com/ronjatech/ronja-cli/internal/api"
	"github.com/spf13/cobra"
	"golang.org/x/term"
)

// `ronja db env` and `ronja db connect` point ORDINARY POSTGRES TOOLS at a
// managed database through Ronja's Postgres-wire proxy.
//
// Neither is a resource verb and neither wraps an endpoint. They are the same
// kind of thing `ronja env` is — moving a credential the CLI already holds into
// the place a different program reads it from, without it ever being displayed.
// `env` does that for curl; these two do it for psql and pg_dump.
//
// THERE IS DELIBERATELY NO `db dump`. `pg_dump` after `db env` is the whole
// feature, and a wrapper would have to re-expose every pg_dump flag — format,
// compression, parallelism, table selection, --no-owner — forever, to be worth
// using at all. What the CLI can add is the credential and the connection
// parameters, which is exactly what these two do.
//
// Why BOTH, when one prints the coordinates the other uses: libpq has no
// environment variable for keepalives_idle. The proxy sits behind a load
// balancer that idles a TCP flow out after ~350 seconds, and only the CLIENT's
// keepalives reset that timer, so a long interactive session needs
// keepalives_idle=60 in the CONNINFO — which only a command that builds the
// conninfo itself can supply. `db env` is the one that lets pg_dump (never idle)
// and psql (mostly not) work with no arguments at all; `db connect` is the one
// that survives a coffee break.

// psqlKeepaliveIdle is the client-side keepalive interval every conninfo this
// file builds carries. See the package note above: it is the only thing that
// resets the load balancer's idle timer on the customer's side.
const psqlKeepaliveIdle = "60"

// proxyForDatabase reads a managed database's proxy coordinates, or explains why
// there are none.
//
// The two refusals are separate on purpose. "No proxy block" has two causes the
// server deliberately does not distinguish — no proxy deployed, or a caller who
// is not an admin — so the message names both rather than guessing. "No read
// role" is a different thing entirely: the proxy IS there and the login would
// authenticate, then be refused for want of a role to resolve. Collapsing the
// two would send an admin looking for a deployment problem that does not exist.
//
// "no read role Ronja CAN USE", not "no read role", and the qualifier is load
// bearing: ReadRoleMinted is computed from the secrets.managed_role column
// alone, so a read secret minted before migration 000444 is reported as not
// minted ON PURPOSE (backend api/v2/database/views_proxy.go, hasMintedReadRole).
// This refusal therefore reaches admins whose database does have a read
// credential, and the flat "has no read role" told them something untrue.
//
// Which is also why the remedy names its own consequence — CONDITIONALLY.
// Re-minting is the right fix for that population (it is what the database
// console prescribes, and it grants the sequence privileges an older read role
// lacks), but minting a tier twice rotates the password, and anything holding
// the old secret by reference — a workflow, a data app, a saved agent — stops
// working. ReadRoleMinted cannot tell that population from a freshly
// provisioned database, which by construction has no read credential at all, so
// the warning says IF: telling someone their nonexistent credential is about to
// be replaced is its own kind of untrue, and it is the commoner case.
//
// ⚠️ The CLI does not fill featureID in from an existing role, and NOT because
// there is never one to read: this refusal fires when there is no READ role, so the
// common "wrote SQL first, so there is a write role" database reaches it with
// featureIDs sitting in the very response above. Two reasons, both deliberate.
// api.DatabaseConnection does not mirror the roles array at all, on purpose (see
// its doc comment — a second, permanently-stale copy of a shape `ronja api`
// already prints). And copying another role's feature would be the CLI silently
// making the access decision for the operator: if that feature is PRIVATE, the
// new credential is confined to its owner, which is invisible until someone
// else's workflow cannot resolve it. Naming a feature is the operator's call, so
// the message says where to find one — selecting `scope`, because the choice is
// conditional rather than a preference: an organization feature is what lets a
// workflow bind the credential for a member who does not own it, and a private one is the
// narrower answer when the role is only ever used from psql, which the proxy
// resolves either way (rsecret.ListManagedDatabaseSecrets has no feature leg).
func proxyForDatabase(cmd *cobra.Command, databaseID string) (*api.DatabaseProxy, *api.Client, error) {
	resolved, err := resolveInstance()
	if err != nil {
		return nil, nil, err
	}
	client := api.New(resolved.URL, resolved.Token)

	conn, err := client.DatabaseConnectionInfo(cmd.Context(), databaseID)
	if err != nil {
		return nil, nil, err
	}
	if conn.Proxy == nil {
		return nil, nil, fmt.Errorf(
			"%s cannot be reached with psql: this organization has no psql proxy configured, or your role is not admin",
			databaseID)
	}
	if !conn.Proxy.ReadRoleMinted {
		return nil, nil, fmt.Errorf(
			"%s has no read role Ronja can use — mint one:\n"+
				"  ronja api -X POST /api/v2/database/%s/user -d '{\"access\":\"read\",\"featureID\":\"<feature-id>\"}'\n"+
				"featureID is the feature that will own the stored credential; the database itself belongs to none. "+
				"Name an organization feature if a workflow, data app or agent will use this credential — that is the only leg "+
				"reaching a member who does not own the feature. For your own psql sessions a private one is enough and narrower; "+
				"the proxy resolves either. List them with:\n"+
				"  ronja api /api/v2/feature/query --jq '.result[] | [.id, .scope, .name] | @tsv' -r\n"+
				"If this database already has a read credential, minting replaces it, and anything still holding "+
				"the old one stops working.",
			databaseID, databaseID)
	}
	return conn.Proxy, client, nil
}

// refuseEnvFlag is the answer to `--env dev` on either of these commands.
//
// It exists because `db sql` and `db migrate` both take that flag, so somebody
// who used it five minutes ago will type it here — and the honest answer is not
// "unknown flag". A dev copy is a database in its own right at the proxy: it has
// its own id, its own physical name and its own read role, and it is addressed
// by that id. The server-side environment overlay that redirects prod→dev exists
// only for requests that name a production database, which a proxy login never
// does: the username IS the id.
func refuseEnvFlag(env string) error {
	if strings.TrimSpace(env) == "" {
		return nil
	}
	return fmt.Errorf("--env is not supported here: a dev copy is reached by its OWN database id, not by naming production. `ronja api /api/v2/database/<prod-id>` reports the copy's id")
}

func newDatabaseEnvCmd() *cobra.Command {
	var envFlag string

	cmd := &cobra.Command{
		Use:   "env <database-id>",
		Short: "Emit PG* shell assignments so psql and pg_dump need no arguments",
		Long: `Emit PG* shell assignments so psql and pg_dump need no arguments.

Meant to be evaluated, not read:

  eval "$(ronja db env mdb-abc)"
  psql
  pg_dump -Fc > dump.pgc

Used that way your access token moves from the credential file into PGPASSWORD
without ever being displayed — eval consumes this command's output. Running
'ronja db env' bare prints a live credential to your terminal.

Shell state does not persist between separate agent tool calls, so combine the
eval with the work in one command:

  eval "$(ronja db env mdb-abc)" && pg_dump -Fc > dump.pgc

The session is READ-ONLY. It resolves the database's read role server-side, so
it can SELECT and it cannot write, create or drop anything, whatever your own
role is.

A database whose objects live outside the public schema needs pg_dump -n public:
the read role holds no USAGE on other schemas.

For a long INTERACTIVE session use ` + "`ronja db connect`" + ` instead. libpq has
no environment variable for keepalives_idle, and without it the load balancer in
front of the proxy drops an idle connection after a few minutes.

With --json, emits the same values as an object for callers that are not a
POSIX shell.`,
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			if err := refuseEnvFlag(envFlag); err != nil {
				return err
			}
			databaseID := strings.TrimSpace(args[0])
			if databaseID == "" {
				return fmt.Errorf("no database id given")
			}

			proxy, client, err := proxyForDatabase(cmd, databaseID)
			if err != nil {
				return err
			}

			// The PROFILE's token, not a new credential: this command mints
			// nothing and asks for nothing. It is the same PAT `ronja env`
			// exports and the same one `db sql` authenticates with — the proxy
			// simply takes it as the Postgres password.
			token := client.Token
			if token == "" {
				return fmt.Errorf("no credential loaded — run `ronja login` first")
			}

			// A terminal on stdout means nothing is capturing this, so the
			// credential is about to be displayed. Warn on stderr (which eval
			// does not consume) rather than refusing — exactly as `ronja env`
			// does, for the same reason.
			if term.IsTerminal(int(os.Stdout.Fd())) {
				fmt.Fprintln(os.Stderr,
					"warning: this prints a live credential. Use: eval \"$(ronja db env "+databaseID+")\"")
			}

			vars := [][2]string{
				{"PGHOST", proxy.Host},
				{"PGPORT", proxy.Port},
				{"PGUSER", proxy.Username},
				{"PGDATABASE", proxy.Database},
				// verify-full + the OS trust store, never `require`. The token
				// travels as the password, and `require` accepts ANY certificate
				// — so a downgrade here hands the credential to whoever answers
				// the socket. sslrootcert=system needs libpq 16 or newer.
				{"PGSSLMODE", "verify-full"},
				{"PGSSLROOTCERT", "system"},
				{"PGPASSWORD", token},
			}

			if flagJSON {
				out := make(map[string]string, len(vars))
				for _, kv := range vars {
					out[kv[0]] = kv[1]
				}
				return emitJSON(out)
			}
			for _, kv := range vars {
				fmt.Printf("export %s=%s\n", kv[0], shellQuote(kv[1]))
			}
			return nil
		},
	}
	cmd.Flags().StringVar(&envFlag, "env", "",
		"not supported here — a dev copy is reached by its own database id")
	return cmd
}

func newDatabaseConnectCmd() *cobra.Command {
	var envFlag string

	cmd := &cobra.Command{
		Use:   "connect <database-id> [-- <psql args>]",
		Short: "Open psql against a managed database",
		Long: `Open psql against a managed database.

  ronja db connect mdb-abc
  ronja db connect mdb-abc -- -c "select count(*) from orders"
  ronja db connect mdb-abc -- -n public -f report.sql

psql must be on your PATH; this command does not install it.

Nothing is printed. Your access token is handed to psql through the child
process's own environment, so it never reaches your terminal, your shell history
or a transcript.

This is the form to use for a LONG interactive session. It builds the whole
connection string itself, which is the only way to carry keepalives_idle=60 —
libpq has no environment variable for it, and the load balancer in front of the
proxy drops a TCP flow that has been idle for a few minutes unless the client
keeps it alive.

The session is READ-ONLY: it resolves the database's read role server-side, so
SELECT works and nothing else does, whatever your own role is.

Everything after -- is passed to psql unchanged.`,
		Args: cobra.MinimumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			if err := refuseEnvFlag(envFlag); err != nil {
				return err
			}
			// Everything before -- is ours; everything after is psql's. Cobra
			// reports the boundary rather than the two halves, so a database id
			// that happens to look like a psql flag is never mistaken for one.
			ownArgs, psqlArgs := splitAtDash(args, cmd.ArgsLenAtDash())
			if len(ownArgs) != 1 {
				return fmt.Errorf("expected exactly one database id before --, got %d", len(ownArgs))
			}
			databaseID := strings.TrimSpace(ownArgs[0])
			if databaseID == "" {
				return fmt.Errorf("no database id given")
			}

			psqlPath, err := exec.LookPath("psql")
			if err != nil {
				return fmt.Errorf("psql is not on your PATH — install the PostgreSQL client tools, or use `eval \"$(ronja db env %s)\"` with a client you have", databaseID)
			}

			proxy, client, err := proxyForDatabase(cmd, databaseID)
			if err != nil {
				return err
			}
			if client.Token == "" {
				return fmt.Errorf("no credential loaded — run `ronja login` first")
			}

			child := psqlCommand(psqlPath, proxy, client.Token, psqlArgs)
			child.Stdin, child.Stdout, child.Stderr = os.Stdin, os.Stdout, os.Stderr
			// psql's own exit status is the answer — a failed -c must fail this
			// command too — and its diagnostics are already on stderr, so
			// wrapping the error would print a second, worse copy.
			return child.Run()
		},
	}
	cmd.Flags().StringVar(&envFlag, "env", "",
		"not supported here — a dev copy is reached by its own database id")
	return cmd
}

// splitAtDash divides cobra's positional args at the `--` cobra recorded.
//
// ArgsLenAtDash is -1 when there was no `--` at all, and otherwise the number of
// positionals that preceded it.
func splitAtDash(args []string, atDash int) (own, rest []string) {
	if atDash < 0 || atDash > len(args) {
		return args, nil
	}
	return args[:atDash], args[atDash:]
}

// psqlCommand builds the child process `db connect` runs.
//
// exec.Command, NOT exec.CommandContext, and that is the whole point of this
// function existing separately.
//
// Every command in this tree runs under the root's signal context
// (signalContext, root.go), which is cancelled on SIGINT — and on Ctrl-C the
// terminal ALREADY delivers SIGINT to psql directly, because it is in this
// process's foreground process group. Handing the child a context as well means
// exec.Cmd's default Cancel fires on the same keystroke and SIGKILLs psql
// mid-session: psql never gets to restore the terminal's tty settings (echo
// off, raw mode) and never gets to cancel the running query, so Ctrl-C on a long
// `pg_sleep` leaves a wrecked shell instead of a prompt.
//
// psql owns SIGINT here. The parent's context is deliberately not consulted:
// there is nothing for this process to cancel that psql is not already
// cancelling better.
func psqlCommand(psqlPath string, proxy *api.DatabaseProxy, token string, psqlArgs []string) *exec.Cmd {
	child := exec.Command(psqlPath, append([]string{proxyConninfo(proxy)}, psqlArgs...)...)
	// PGPASSWORD on the CHILD's environment, appended last so it wins over
	// anything inherited. It is not in this process's environment and not on the
	// command line, which is what keeps it out of `ps`.
	child.Env = append(os.Environ(), "PGPASSWORD="+token)
	return child
}

// proxyConninfo builds the URI psql is launched with.
//
// Built from the parts rather than by substituting into the server's
// ConnectionString: that string carries a <YOUR_PAT> placeholder meant for a
// human to replace, and a client that string-replaced it would be one careless
// edit away from putting a real token on a command line. The password is not
// here at all — it goes to the child through PGPASSWORD.
func proxyConninfo(p *api.DatabaseProxy) string {
	return "postgresql://" + p.Username + "@" + p.Host + ":" + p.Port + "/" + p.Database +
		"?sslmode=verify-full&sslrootcert=system&keepalives_idle=" + psqlKeepaliveIdle
}
