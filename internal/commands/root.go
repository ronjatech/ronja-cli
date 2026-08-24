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
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/signal"
	"runtime/debug"
	"syscall"

	"github.com/spf13/cobra"
)

// errAlreadyReported is returned by a command that has already written its own
// failure to stderr and wants only the exit code from here.
//
// It exists for `ronja api`, whose failure line is a contract — one
// "HTTP <status> <method> <path>" — and which would otherwise get a second,
// differently-worded line printed underneath it by Execute.
var errAlreadyReported = errors.New("already reported")

// Version is stamped at build time by the release pipeline:
//
//	go install -ldflags "-X github.com/ronjatech/ronja-cli/internal/commands.Version=$(git describe --tags)" ./cmd/ronja
//
// It stays "dev" for a plain `go build` from a checkout, and resolveVersion
// recovers the real one for `go install ...@version`.
var Version = "dev"

// resolveVersion reports the version to display.
//
// The ldflags stamp above only reaches binaries the release pipeline builds —
// the Homebrew cask and the release tarballs. A user who installs the
// documented way for every non-macOS platform,
//
//	go install github.com/ronjatech/ronja-cli/cmd/ronja@latest
//
// compiles from source with no stamp at all, so `ronja --version` reported
// "dev" for a real published release and a bug report could not say which one.
// Go records the module version it resolved in the build info, so read it back
// rather than leaving that install path unable to identify itself.
//
// Only consulted when the stamp is absent, so a pipeline build always wins.
func resolveVersion() string {
	return resolveVersionFrom(Version, debug.ReadBuildInfo)
}

// resolveVersionFrom is the testable half of resolveVersion.
//
// Split out because the case that matters cannot be reached from a test
// otherwise: build info is a property of how the BINARY was produced, so a
// `go test` binary always reports "(devel)" and can never exercise the
// published-module branch. Verifying that branch for real would mean
// publishing a release to see what it prints, which is a slow way to find out
// that a two-line function is wrong.
func resolveVersionFrom(stamp string, read func() (*debug.BuildInfo, bool)) string {
	if stamp != "dev" {
		return stamp
	}
	info, ok := read()
	// "(devel)" is what a local `go build` reports; it is no more useful than
	// "dev" and less recognisable, so keep ours.
	if !ok || info == nil || info.Main.Version == "" || info.Main.Version == "(devel)" {
		return stamp
	}
	return info.Main.Version
}

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

// signalContext is the context every command runs under: cancelled on SIGINT or
// SIGTERM.
//
// Installed at the ROOT rather than per command, because the alternative is
// per-command and was: cmd.Context() was context.Background(), so every
// cancellation branch a poll loop carries — `wf test`'s run wait,
// WaitForTableBuild's, `api --wait-until`'s — was unreachable, and Ctrl-C simply
// killed the process mid-request. A cancelled context ends the wait with a
// reason instead, and the exit is non-zero because a wait that was interrupted
// did not finish.
//
// It cancels the WAIT, never the work: a build, a run and a commit all continue
// server-side whether or not this process is watching, which is what the
// commands that wait say out loud.
func signalContext() (context.Context, context.CancelFunc) {
	return signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
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
	ctx, stop := signalContext()
	defer stop()
	if err := root.ExecuteContext(ctx); err != nil {
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

  ronja api /api/v2/feature/query --jq '.result[].id' -r
  ronja api -X POST /api/v2/file/upload/uploads -F file=@report.pdf
  ronja api /api/v2/workflow/run/$id --wait-until '.status != "running"'

The other exceptions are the folder dev loops, which are stateful in a way HTTP
alone handles badly. If you are editing a workflow, a data app or a feature's
derived tables from files on disk, use these rather than PUTting source into a
JSON body:

  ronja wf             develop a workflow from a local folder
                       (init, clone, status, validate, push, test, publish,
                       discard)
  ronja app            develop a data app from a local folder
                       (init, clone, status, push, validate, publish, discard)
  ronja pipeline       develop a feature's derived tables from a local folder
                       (init, clone, status, push, publish, discard)

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
		Version:       resolveVersion(),
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
		newPipelineCmd(), newAPICmd(), newQueryCmd(), newDatabaseCmd())
	return root
}

// emitJSON writes a value as indented JSON on stdout. Used by every command's
// --json path so the shape is consistent.
func emitJSON(v any) error {
	enc := json.NewEncoder(os.Stdout)
	enc.SetIndent("", "  ")
	return enc.Encode(v)
}
