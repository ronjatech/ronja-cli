// Package cmd wires the `ronja` command tree.
//
// Design constraints, in priority order:
//
//  1. An agent must be able to drive this without a TTY. Every command takes
//     --json, never prompts when stdin is not a terminal, and exits non-zero on
//     failure with the reason on stderr.
//  2. A human must find it pleasant. Human output is short, and the next action
//     is always spelled out.
package commands

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"

	"github.com/spf13/cobra"
)

// errAlreadyReported is returned by a command that has already written its own
// failure to stderr and wants only the exit code from here.
//
// It exists for `ronja api`, whose failure line is a contract — one
// "HTTP <status> <method> <path>" — and which would otherwise get a second,
// differently-worded line printed underneath it by Execute.
var errAlreadyReported = errors.New("already reported")

// Version is stamped at build time:
//
//	go install -ldflags "-X github.com/ronjatech/ronja-cli/internal/commands.Version=$(git describe --tags)" ./cmd/ronja
var Version = "dev"

// Global flags. Kept on package vars because cobra binds them once at the root
// and every subcommand reads the same resolved values.
var (
	flagURL     string
	flagProfile string
	flagJSON    bool
)

func Execute() {
	if code := run(nil); code != 0 {
		os.Exit(code)
	}
}

// run executes the command tree and returns the process exit code, reporting
// any failure on stderr.
//
// Split out from Execute purely so the reporting RULES are testable: that a
// failure produces exactly one line, and that a command which already spoke for
// itself produces none. An os.Exit in the middle of that makes both unassertable
// without spawning a subprocess. args is the argument vector, or nil to take
// the process's own.
func run(args []string) int {
	root := newRootCmd()
	if args != nil {
		root.SetArgs(args)
	}
	if err := root.Execute(); err != nil {
		// Cobra has already printed the error for usage problems; this covers
		// the RunE path. Keep it on stderr so --json output stays parseable.
		if !errors.Is(err, errAlreadyReported) {
			fmt.Fprintln(os.Stderr, "error:", err)
		}
		return 1
	}
	return 0
}

func newRootCmd() *cobra.Command {
	root := &cobra.Command{
		Use:   "ronja",
		Short: "Build and explore in Ronja from the command line",
		Long: `Build and explore in Ronja from the command line.

This CLI is a bootstrap, not a wrapper around the API. Start here:

  ronja login          sign in through the browser (the one thing an agent
                       can't do for itself)
  ronja whoami         who you are, on which instance, in which organization
  ronja context        print everything needed to call the API directly —
                       instance, identity, auth header, and the API index
  ronja env            load the credential into a shell without printing it:
                       eval "$(ronja env)"

After that, work over plain HTTP — either directly, or through the two
commands that carry the credential for you so nothing has to echo it:

  ronja api            make an authenticated request to any path
                       (transport and auth only; it knows no endpoints)
  ronja query          run read-only SQL and get CSV back

Both take --jq to pull a field out of the answer, so nothing has to be piped
through another interpreter to read it. api also has -F for file uploads and
--wait-until for asynchronous work:

  ronja api /api/v2/feature/query --jq -r '.items[].id'
  ronja api -X POST /api/v2/file/upload/uploads -F file=@report.pdf
  ronja api /api/v2/workflow/run/$id --wait-until '.status != "running"'

The other exceptions are the two folder dev loops, which are stateful in a way
HTTP alone handles badly. If you are editing a workflow or a data app from
files on disk, use these rather than PUTting source into a JSON body:

  ronja wf             develop a workflow from a local folder
                       (init, clone, status, validate, push, test, publish,
                       discard)
  ronja app            develop a data app from a local folder
                       (init, clone, status, push, validate, publish, discard)

Each login is stored as a named PROFILE — one instance, one organization, one
token. An access token belongs to a single organization, so belonging to two
means two profiles, and a local backend and production can be signed in at the
same time:

  ronja login --url http://localhost:8082
  ronja profile list
  ronja profile use acme-retail
  ronja query --profile local-8082 "SELECT 1"

For scripts and agents, RONJA_URL and RONJA_TOKEN override the stored
credentials entirely and never touch disk.`,
		Version:       Version,
		SilenceUsage:  true,
		SilenceErrors: true,
	}

	root.PersistentFlags().StringVar(&flagURL, "url", "",
		"Ronja instance to talk to (default: $RONJA_URL, else the current profile)")
	root.PersistentFlags().StringVar(&flagProfile, "profile", "",
		"stored profile to use (default: $RONJA_PROFILE, else the current one)")
	root.PersistentFlags().BoolVar(&flagJSON, "json", false,
		"emit machine-readable JSON on stdout")

	root.AddCommand(newLoginCmd(), newLogoutCmd(), newWhoamiCmd(), newProfileCmd(),
		newContextCmd(), newEnvCmd(), newWorkflowCmd(), newDataAppCmd(),
		newAPICmd(), newQueryCmd(), newDatabaseCmd())
	return root
}

// emitJSON writes a value as indented JSON on stdout. Used by every command's
// --json path so the shape is consistent.
func emitJSON(v any) error {
	enc := json.NewEncoder(os.Stdout)
	enc.SetIndent("", "  ")
	return enc.Encode(v)
}
