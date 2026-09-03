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

// exitCodeError is a failure that names its OWN process exit code, for a
// command whose answer has more than two values.
//
// Every command until now has had a two-valued answer — it worked, or it did
// not — so `run` mapping every error to 1 was the whole contract. `ronja sync
// status` does not: "something drifted" is an actionable finding a CI job fixes
// by pushing, and "I could not tell" is a broken credential or a folder that
// would not open, which the same job must NOT treat as drift. Collapsing them
// into one non-zero makes the difference unreadable to the caller that most
// needs it (see syncExitDrifted / syncExitUnknown).
//
// Deliberately additive: nothing else returns one, so every existing command
// keeps exiting exactly 0 or 1 as it always has.
type exitCodeError struct {
	code int
	err  error
}

func (e *exitCodeError) Error() string { return e.err.Error() }

// Unwrap keeps errors.Is working through the wrapper — errAlreadyReported above
// is matched that way, and a coded error that has already spoken for itself
// must still suppress the second line `run` would otherwise print.
func (e *exitCodeError) Unwrap() error { return e.err }

// withExitCode tags an error with the process exit code it should produce.
func withExitCode(code int, err error) error {
	if err == nil {
		return nil
	}
	return &exitCodeError{code: code, err: err}
}

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
	// flagStack names the STACK — the environment in the folder's committed
	// manifest — that a folder command acts on. Registered on the three folder
	// command groups rather than at the root, because it means nothing to
	// `login`, `query` or `api`: those talk to an instance, not to a folder.
	//
	// TWO selectors and no third: --stack names an environment (shared, in the
	// repo), --profile names a credential (local, on this machine). Keeping them
	// separate is the whole reason ronja.json can be committed at all — see
	// wfdir.Stack.
	flagStack string
)

// addStackFlag registers --stack on a folder command group.
//
// One function rather than three copies of the same line, so the three loops
// cannot drift on the flag's name or its help text — the CLI has been bitten by
// exactly that shape before (wfdir.Kind.Command exists because a per-kind string
// was spelled twice and a third kind was routed to the wrong one by both).
func addStackFlag(cmd *cobra.Command) {
	cmd.PersistentFlags().StringVar(&flagStack, "stack", "",
		"stack in ronja.json to act on (default: the one matching this credential)")
}

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
	code := 0
	if err := root.ExecuteContext(ctx); err != nil {
		// Cobra has already printed the error for usage problems; this covers
		// the RunE path. Keep it on stderr so --json output stays parseable.
		if !errors.Is(err, errAlreadyReported) {
			fmt.Fprintln(os.Stderr, "error:", err)
		}
		// A command that named its own exit code gets it. Checked AFTER the
		// reporting above rather than instead of it, so a coded failure is still
		// explained on stderr like every other one.
		code = 1
		var coded *exitCodeError
		if errors.As(err, &coded) {
			code = coded.code
		}
	}
	// The daily update notice goes out LAST: after the command's own output and
	// after any error line, so it is never interleaved with a report. It cannot
	// change code, and by construction it cannot fail.
	finishUpdateCheck(pendingUpdateCheck)
	return code
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

And one command that talks to nothing on the server at all:

  ronja update         bring this binary forward to the newest release
                       (never runs on its own)

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

A folder of any of those three kinds can name the rows it reads but does not
own, so the same folder deploys to more than one organization:

  ronja bind           point this folder's declared dependencies at the rows
                       this organization calls by those names

And a repository holds many folders, so one command asks about all of them:

  ronja sync status    walk a directory for every folder and report which ones
                       have drifted — read-only, and a CI gate on its own
                       (0 clean, 1 drifted, 2 could not tell)

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

	// Every tree starts with no check in flight and no update performed, so a
	// second tree built in the same process — which is what every test does —
	// cannot inherit either from the first.
	pendingUpdateCheck, updateRanThisProcess = nil, false

	// The daily update check starts HERE rather than from a scan of os.Args,
	// because by PersistentPreRunE cobra has resolved which command is running
	// and parsed --json, so both are plain reads. It returns nil
	// unconditionally: a check that cannot run must never fail a command
	// somebody asked for. See update_check.go.
	root.PersistentPreRunE = func(cmd *cobra.Command, args []string) error {
		pendingUpdateCheck = startUpdateCheck(cmd.Context(), cmd)
		return nil
	}

	root.PersistentFlags().StringVar(&flagURL, "url", "",
		"Ronja instance to talk to (default: $RONJA_URL, else the current profile)")
	root.PersistentFlags().StringVar(&flagProfile, "profile", "",
		"stored profile to use (default: $RONJA_PROFILE, else the current one)")
	root.PersistentFlags().BoolVar(&flagJSON, "json", false,
		"emit machine-readable JSON on stdout")

	// `bind` sits at the ROOT rather than inside one of the three folder groups,
	// and that is not a filing preference: `dependencies` is a manifest key every
	// folder kind carries, so the command is kind-agnostic and works in whichever
	// folder it is run from. Registering it three times would be three commands
	// that have to stay identical, and the kind it acted on would be decided by
	// which verb somebody happened to type.
	// `sync` sits at the ROOT for the same reason `bind` does, one level up: it
	// is about a TREE of folders rather than the one you are standing in, and
	// the folders in it are of several kinds. Filing it under any single kind's
	// verb would be filing a repository-wide question under one of its answers.
	root.AddCommand(newLoginCmd(), newLogoutCmd(), newWhoamiCmd(), newProfileCmd(),
		newContextCmd(), newEnvCmd(), newWorkflowCmd(), newDataAppCmd(),
		newPipelineCmd(), newAutomationCmd(), newBindCmd(), newSyncCmd(),
		newAPICmd(), newQueryCmd(), newDatabaseCmd(), newUpdateCmd())
	return root
}

// emitJSON writes a value as indented JSON on stdout. Used by every command's
// --json path so the shape is consistent.
func emitJSON(v any) error {
	enc := json.NewEncoder(os.Stdout)
	enc.SetIndent("", "  ")
	return enc.Encode(v)
}
