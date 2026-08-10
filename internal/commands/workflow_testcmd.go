package commands

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/signal"
	"strings"
	"time"

	"github.com/ronjatech/ronja-cli/internal/api"
	"github.com/ronjatech/ronja-cli/internal/wfdir"
	"github.com/spf13/cobra"
)

// The file is workflow_testcmd.go, not workflow_test.go, because the latter is
// a TEST file to the Go toolchain and would never be compiled into the binary.

// Log rendering modes for the terminal report.
const (
	logsFull = "full"
	logsTail = "tail"
	logsNone = "none"
)

// logsTailLines is how much of a run's log the default mode shows. A workflow
// log is unbounded and the interesting part of a failure is nearly always at
// the bottom; --logs=full is one flag away when it is not.
const logsTailLines = 40

// runPollInterval is how often a running workflow is re-read to BEGIN with.
//
// The web builder polls at 3s; 2.5s here is the same order, chosen so a short
// run feels responsive without a long one (they can last an hour on dedicated
// compute) making thousands of requests. A package var rather than a const so
// the tests can run the whole poll loop without sleeping through it.
var runPollInterval = 2500 * time.Millisecond

// maxPollInterval caps how far the poll interval backs off.
//
// Each poll re-reads the run in full, logs included, and a run's log only
// grows — so a long run's polling cost is quadratic in its length at a fixed
// cadence. Backing off trades a little latency at the END of a long run (where
// nobody is watching a second-by-second timeline anyway) for a bounded number
// of whole-log downloads. 10s is the ceiling because that is about the longest
// a person will accept between "the run finished" and being told so; a
// package var, like runPollInterval, so the tests can collapse it.
var maxPollInterval = 10 * time.Second

// pollsAtStartInterval is how many polls happen at runPollInterval before the
// backoff starts, and pollBackoffFactor is how fast it grows after that.
//
// The first polls MUST stay at the starting cadence: most test runs finish in
// seconds, and a command that answered a 4-second run 10 seconds late would
// have made the common case worse to protect the rare one. Eight polls is
// ~20s of unchanged behaviour, after which 1.5x per poll reaches the 10s cap
// about 48 seconds in — by which point this is a long run and one poll every
// 10s is plenty.
const (
	pollsAtStartInterval = 8
	pollBackoffFactor    = 1.5
)

// maxConsecutivePollFailures bounds an UNBROKEN run of failed polls before the
// command gives up.
//
// Transient failures are expected — a pod restarting mid-deploy, a dropped
// connection — and treating the first one as fatal would abandon runs that are
// fine. Consecutive rather than cumulative: a poll that succeeds proves the
// instance is reachable, so the budget resets. Deliberately simpler than the
// device flow's elapsed-time window, because a run poll has a --timeout
// bounding total wall time already and does not need a second clock.
const maxConsecutivePollFailures = 3

// maxNamedTables bounds the per-table name lookups the --write-live refusal
// makes. Ten is more than any hand-written workflow binds, and the ids past it
// are still counted — what must never happen is a refusal that takes a minute
// to print because it looked up every table one at a time.
const maxNamedTables = 10

// `ronja wf test` runs YOUR DRAFT and reports what happened.
//
// Two safety properties are the whole design. It refuses an approval-gated
// workflow up front rather than letting the run 400 (and never suggests turning
// the gate off — that edit would ride the draft into the live workflow on
// publish). And it refuses to run a draft with bound output tables unless
// --write-live says so: a workflow's table writes target the LIVE table, there
// is no sandbox, and "replace" means replace.
func newWorkflowTestCmd() *cobra.Command {
	var (
		params    []string
		writeLive bool
		staleOK   bool
		timeout   time.Duration
		logsMode  string
	)
	cmd := &cobra.Command{
		Use:   "test",
		Short: "Run your draft of the workflow and report what happened",
		Long: `Run your draft of the workflow and report what happened.

Runs the DRAFT — the code you pushed, not the live workflow — waits for it to
finish, and prints its status, logs and outputs. Push first: a folder with
unpushed changes is refused, because otherwise you would be testing code you
are not looking at (--stale-ok runs the draft as it stands).

Parameters are passed one at a time and checked against the workflow's declared
parameters before anything runs:

  ronja wf test --param month=2026-07 --param region=EU

Output tables are the reason this command has a flag nobody wants to type. A
workflow that writes to a table writes to the LIVE table — there is no test
sandbox — so a draft with bound output tables refuses to run without
--write-live. A workflow that binds no output tables yet — before you push a
{{ write }} marker — runs without it.

Approval-gated workflows cannot be run from here at all: an approval can only be
produced inside an agent session, so test those from a Ronja chat.

Exits zero only when the run finishes successfully. With --json, the finished
run — row, steps and health — as one object on stdout, with progress on stderr.

Ctrl-C stops the waiting, not the run: it keeps going server-side either way.`,
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			// Ctrl-C has to be caught HERE rather than left to the runtime's
			// default: this command waits on somebody else's run, and the thing a
			// person needs to be told at that moment is that killing the wait does
			// not kill the run. cmd.Context() is Background (root calls Execute,
			// not ExecuteContext), so without this the poll loop's cancellation
			// branch is unreachable and the process just dies mid-poll.
			ctx, stop := signal.NotifyContext(cmd.Context(), os.Interrupt)
			defer stop()

			resolved, err := resolveInstance()
			if err != nil {
				return err
			}
			if logsMode != logsFull && logsMode != logsTail && logsMode != logsNone {
				return fmt.Errorf("--logs must be one of %s, %s or %s (got %q)",
					logsFull, logsTail, logsNone, logsMode)
			}
			// 0 means "wait forever" — a documented value. A negative duration has
			// no such meaning and used to land on the same branch, so `--timeout
			// -5m` silently waited forever, which is the opposite of what anybody
			// typing a negative timeout could have meant.
			if timeout < 0 {
				return fmt.Errorf("--timeout cannot be negative (got %s) — pass 0 to wait for as long as the run takes", timeout)
			}
			f, err := openFolder(cmd.Context(), resolved, wfdir.WorkflowKind)
			if err != nil {
				return err
			}
			outcome, err := runTest(ctx, f, testOptions{
				Params:    params,
				WriteLive: writeLive,
				StaleOK:   staleOK,
				Timeout:   timeout,
			})
			// The report is emitted even on failure: a failed run IS the answer
			// this command was asked for, and a timed-out one still has a run id
			// somebody needs.
			if outcome != nil && outcome.Run != nil {
				if flagJSON {
					if emitErr := emitJSON(outcome.Run); emitErr != nil {
						// A run that failed is the answer; a stdout that would not
						// take it is a second, lesser problem. Returning the encode
						// error instead would report the run as fine and the
						// printing as broken, which is exactly backwards.
						if err == nil {
							return emitErr
						}
						fmt.Fprintln(os.Stderr, "error: could not write the run as JSON:", emitErr)
					}
				} else {
					printTestReport(outcome, logsMode)
				}
			}
			return err
		},
	}
	cmd.Flags().StringArrayVar(&params, "param", nil,
		"a parameter value as key=value (repeat for each parameter)")
	cmd.Flags().BoolVar(&writeLive, "write-live", false,
		"allow a run that writes to the workflow's live output tables")
	cmd.Flags().BoolVar(&staleOK, "stale-ok", false,
		"run even though this folder has changes you have not pushed")
	cmd.Flags().DurationVar(&timeout, "timeout", 15*time.Minute,
		"give up waiting after this long (0 waits forever; the run continues either way)")
	cmd.Flags().StringVar(&logsMode, "logs", logsTail,
		"how much of the run log to print: full, tail or none")
	return cmd
}

type testOptions struct {
	Params    []string
	WriteLive bool
	StaleOK   bool
	Timeout   time.Duration
}

// testOutcome is what the reporters read. Run is the last thing the server
// said about the run, which on the timeout path is a run still going rather
// than a finished one — hence TimedOut, so neither reporter has to infer it
// from a status.
type testOutcome struct {
	Run      *api.RunResponse
	TimedOut bool
}

// runTest is the whole command, ordered so that everything which can refuse
// does so before the run starts — the point at which the workflow's side
// effects stop being hypothetical.
func runTest(ctx context.Context, f *folder, opts testOptions) (*testOutcome, error) {
	client := api.New(f.Resolved.URL, f.Resolved.Token)

	// 1. Which row. `wf test` runs the DRAFT, always: the live workflow is what
	// everyone else's automations use, and a command whose job is "try my
	// changes" running the version without them would be worse than useless.
	if !f.Bound || f.Binding.WorkflowID == "" {
		return nil, fmt.Errorf("nothing to test — this folder has no workflow on %s yet.\n  Run `ronja wf push` first: it creates the workflow and the draft this would run",
			f.Resolved.URL)
	}
	existing, err := inspectTarget(ctx, client, f)
	if err != nil {
		return nil, err
	}
	draft := existing.Row()
	if draft == nil {
		return nil, fmt.Errorf("you have no draft of workflow %s, and `wf test` runs the draft rather than the live version.\n  Run `ronja wf push` first — it opens a draft and syncs this folder into it",
			existing.Workflow.ID)
	}

	// 2. Test what you are looking at. A dirty folder means the draft holds
	// older code, and a green run of code you have since changed is the most
	// expensive kind of false confidence.
	if err := checkStale(f, opts.StaleOK); err != nil {
		return nil, err
	}

	// 3. The approval gate, BEFORE the run POST rather than as a 400 from it.
	//
	// A gated run needs proof of approval that only an agent session can
	// produce (the server verifies it against a tool-approval batch), so this is
	// not a permission the caller can acquire — it is a shape of workflow the
	// command line cannot execute. Note what is deliberately NOT suggested:
	// turning the gate off on the draft would work, and would then commit that
	// governance change onto the live workflow at publish. Nobody should learn
	// that trick from a CLI hint.
	if draft.IsGated() {
		return nil, fmt.Errorf("workflow %s has an approval gate, and an approval can only be produced inside an agent session — a run started over HTTP has no way to carry one.\n  Test this workflow from a Ronja chat instead",
			draft.ID)
	}

	// 4. Parameters, checked locally: the v2 run endpoint does not validate them
	// (only the automation and script paths do), so an unknown name would
	// otherwise silently do nothing and a mistyped number would fail somewhere
	// inside the script.
	values, err := parseParams(opts.Params, draft.Parameters)
	if err != nil {
		return nil, err
	}

	// 5. Output safety. Asymmetric on purpose: a draft with no bound output
	// tables — a workflow whose files carry no {{ write }} marker yet — runs
	// with zero friction, and one that would write to live tables needs a flag
	// whose name says what it does. Note that a bootstrap draft only stays in
	// the frictionless half until the first push of a {{ write }}: the file-save
	// path re-derives output_table_ids on EVERY save, so a converted script that
	// writes a table needs --write-live from its first push, published or not.
	// Not a y/N prompt: prompts get muscle-memoried, and the failure mode here
	// is a replaced production table.
	if len(draft.OutputTableIDs) > 0 && !opts.WriteLive {
		return nil, fmt.Errorf("%s", writeLiveRefusal(ctx, client, draft.OutputTableIDs))
	}

	// 6. A heads-up, never a refusal: the draft may hold edits this folder never
	// pushed. Last read before the run, so it costs nothing on the paths above
	// that refuse.
	noteDraftDrift(ctx, client, f, draft)

	// 7. Everything above has passed; from here the run exists in the world.
	started, err := client.RunWorkflow(ctx, draft.ID, values)
	if err != nil {
		return nil, fmt.Errorf("run %s: %s", draft.ID, serverMessage(err))
	}
	fmt.Fprintf(os.Stderr, "  Started run %s of draft %s.\n", started.ID, draft.ID)

	outcome := &testOutcome{
		// A synthesized response so there is ALWAYS something to report once a
		// run exists: the first poll can fail, and a caller who has just started
		// a run must still be told its id.
		//
		// Health is "done" rather than empty because that is what the server
		// would say about this exact run: deriveRunHealth (backend
		// api/v2/workflow/run_steps.go) returns done for anything that is not
		// errored and has no failed step, and a run one millisecond old has no
		// steps at all. An empty health would be a value the API never emits,
		// which is a worse thing to hand a --json consumer than a true one.
		Run: &api.RunResponse{
			WorkflowRun: *started,
			Steps:       []api.StepDTO{},
			Health:      api.RunHealthDone,
		},
	}

	final, err := pollRun(ctx, client, started.ID, opts.Timeout, outcome)
	if final != nil {
		outcome.Run = final
	}
	if err != nil {
		return outcome, err
	}
	// Exit zero means the run SUCCEEDED, and nothing else. The enum is three
	// values today, so the interesting branch is the last one: a status added
	// server-side (a "cancelled", say) ends the poll — see api.RunResponse.Running
	// — and used to fall through to a zero exit, which would tell a script that
	// something it never heard of was a pass.
	switch outcome.Run.Status {
	case api.RunStatusDone:
		return outcome, nil
	case api.RunStatusError:
		return outcome, fmt.Errorf("run %s failed", outcome.Run.ID)
	default:
		return outcome, fmt.Errorf("run %s ended with status %q, which this version of the CLI does not know — treating it as a failure rather than a pass.\n  The report above is what the instance said; upgrade the CLI if this status is a new one",
			outcome.Run.ID, outcome.Run.Status)
	}
}

// noteDraftDrift says so, without refusing, when the draft on the server holds
// changes this folder never pushed.
//
// The web builder edits the SAME per-user draft, so the code about to run may
// carry an edit made in a browser tab — which `wf status` reports properly and
// `wf test` had no way to notice. A note rather than a gate: the draft is what
// this command runs BY DEFINITION, so refusing would only invite a flag whose
// meaning is "yes, run the thing I asked you to run".
//
// Compared only when the baseline came from THIS draft row. A baseline taken
// from the live workflow, or none at all, makes every remote file read as
// drifted — noise, not news, and `wf status` explains that case in full.
func noteDraftDrift(ctx context.Context, client *api.Client, f *folder, draft *api.Workflow) {
	baseline := f.State.For(f.Key)
	if baseline == nil || baseline.SourceID != draft.ID {
		return
	}
	files, err := client.ListWorkflowFiles(ctx, draft.ID)
	if err != nil {
		// Best effort, like the table-name lookup: not being able to check for
		// drift is no reason to refuse to run.
		return
	}
	diff := wfdir.DiffHashes(hashFiles(files), baseline.Hashes())
	if !diff.Dirty() {
		return
	}
	fmt.Fprintf(os.Stderr, "  Note: draft %s holds %d change(s) that did not come from this folder — the web builder edits the same draft. `ronja wf status` lists them.\n",
		draft.ID, diff.Total())
}

// checkStale refuses to test a folder whose contents have not been pushed.
//
// Local vs BASELINE, the same comparison `wf status` calls "local changes": the
// baseline is what the draft holds, so a difference is exactly "the draft is
// not this folder". A folder with no baseline at all reads as entirely
// unpushed, which is the honest answer for a copy cloned from git.
func checkStale(f *folder, staleOK bool) error {
	enumeration, err := wfdir.Enumerate(f.Root, wfdir.WorkflowKind)
	if err != nil {
		return err
	}
	diff := wfdir.DiffHashes(enumeration.Files, f.State.For(f.Key).Hashes())
	if !diff.Dirty() {
		return nil
	}
	if staleOK {
		fmt.Fprintf(os.Stderr, "  Note: --stale-ok — testing the draft as it stands; %d local change(s) are not in it.\n",
			diff.Total())
		return nil
	}
	return fmt.Errorf("this folder has %d change(s) that are not in your draft, so a test would run code you are not looking at:\n%s  Run `ronja wf push` first, or `ronja wf test --stale-ok` to run the draft as it stands",
		diff.Total(), localChangeSummary(diff))
}

// localChangeSummary renders the unpushed-change lines of the refusal above.
func localChangeSummary(diff wfdir.Diff) string {
	out := ""
	for _, group := range []struct {
		label string
		paths []string
	}{
		{"new here", diff.Added},
		{"changed here", diff.Modified},
		{"deleted here", diff.Deleted},
	} {
		for _, path := range group.paths {
			out += fmt.Sprintf("    %-14s %s\n", group.label, path)
		}
	}
	return out
}

// writeLiveRefusal builds the message for a test run that would write to live
// tables, naming them where it can.
//
// Names are best effort by design: they come from a second endpoint the
// caller's token may not reach, and being unable to pretty-print an id is no
// reason to fail to warn about it.
func writeLiveRefusal(ctx context.Context, client *api.Client, tableIDs []string) string {
	var lines strings.Builder
	fmt.Fprintf(&lines, "this test run writes to %d live %s:\n",
		len(tableIDs), plural(len(tableIDs), "table"))
	// One serial request per table, so the list is capped rather than made
	// concurrent: the count is the warning, the names are decoration, and a
	// workflow bound to fifty output tables should not make the refusal slower
	// than the run it is refusing.
	named := tableIDs
	if len(named) > maxNamedTables {
		named = named[:maxNamedTables]
	}
	for _, id := range named {
		name := ""
		if table, err := client.GetTable(ctx, id); err == nil && table.Name != nil {
			name = *table.Name
		}
		if name == "" {
			fmt.Fprintf(&lines, "    %s\n", id)
		} else {
			fmt.Fprintf(&lines, "    %s  %s\n", id, name)
		}
	}
	if rest := len(tableIDs) - len(named); rest > 0 {
		fmt.Fprintf(&lines, "    ... and %d more\n", rest)
	}
	lines.WriteString("  A workflow's table writes go to the LIVE table — there is no test sandbox, and a\n")
	lines.WriteString("  replace-mode write replaces what is there now.\n")
	lines.WriteString("  Re-run with --write-live if that is what you mean.")
	return lines.String()
}

// pollRun waits for a run to reach a terminal status, narrating steps to stderr
// as they move.
//
// Returns the last response it managed to read even when it gives up, because
// "still running, here is its id" is a useful answer and a timeout is not a
// reason to throw away what we know.
func pollRun(ctx context.Context, client *api.Client, runID string, timeout time.Duration, outcome *testOutcome) (*api.RunResponse, error) {
	var deadline time.Time
	if timeout > 0 {
		deadline = time.Now().Add(timeout)
	}

	seen := map[string]string{}
	var last *api.RunResponse
	failures := 0
	// polls counts every attempt, failed ones included: a poll that could not
	// be read still cost a request, and backing off on those too is if anything
	// the friendlier thing to do to an instance that is struggling.
	polls := 0
	interval := runPollInterval

	for {
		if !deadline.IsZero() && time.Now().After(deadline) {
			outcome.TimedOut = true
			return last, fmt.Errorf("gave up waiting for run %s after %s — it is still going, and it keeps going whether or not this command is watching.\n  Check it later with `curl -H \"Authorization: Bearer $RONJA_TOKEN\" \"$RONJA_URL/api/v2/workflow/run/%s\"`, or raise --timeout",
				runID, timeout, runID)
		}

		resp, err := client.GetWorkflowRun(ctx, runID)
		if err != nil {
			if ctx.Err() != nil {
				return last, interruptedError(runID, ctx.Err())
			}
			failures++
			if failures >= maxConsecutivePollFailures {
				return last, fmt.Errorf("lost contact with %s while waiting for run %s (%d polls in a row failed, last: %s) — the run itself is unaffected and continues server-side",
					client.BaseURL, runID, failures, serverMessage(err))
			}
			fmt.Fprintf(os.Stderr, "  Note: could not read run %s (%s) — retrying.\n", runID, serverMessage(err))
		} else {
			// A successful read proves the instance is reachable, so the budget
			// for transient failures starts over.
			failures = 0
			last = resp
			narrateSteps(resp.Steps, seen)
			if !resp.Running() {
				return resp, nil
			}
		}

		polls++
		interval = nextPollInterval(interval, polls)

		select {
		case <-ctx.Done():
			return last, interruptedError(runID, ctx.Err())
		case <-time.After(interval):
		}
	}
}

// nextPollInterval is the wait before poll number polls+1, given the wait that
// preceded poll number polls.
//
// A separate function because the SCHEDULE is the thing worth pinning: it has
// to hold the starting cadence for the first polls and it has to stop growing,
// and neither property is visible from inside the loop.
func nextPollInterval(interval time.Duration, polls int) time.Duration {
	if polls <= pollsAtStartInterval || interval >= maxPollInterval {
		return interval
	}
	grown := time.Duration(float64(interval) * pollBackoffFactor)
	if grown > maxPollInterval {
		return maxPollInterval
	}
	return grown
}

// interruptedError explains a cancelled wait, and says the thing a Ctrl-C at
// this moment makes people wonder about.
func interruptedError(runID string, cause error) error {
	if errors.Is(cause, context.Canceled) {
		return fmt.Errorf("stopped waiting for run %s — the run continues server-side", runID)
	}
	return fmt.Errorf("stopped waiting for run %s: %w", runID, cause)
}

// narrateSteps prints the steps that are new or have moved since the last poll.
//
// Keyed on status rather than on the whole step, so a step whose logs grow does
// not reprint: this is a timeline, and a line per poll per step would bury it.
func narrateSteps(steps []api.StepDTO, seen map[string]string) {
	for _, step := range steps {
		if seen[step.ID] == step.Status {
			continue
		}
		seen[step.ID] = step.Status
		fmt.Fprintf(os.Stderr, "    %-9s %s%s\n", step.Status, step.Name, formatDuration(step.DurationMs))
	}
}

// formatDuration renders a step's duration as a parenthesised suffix, or
// nothing at all while it is still running.
func formatDuration(ms *int64) string {
	if ms == nil {
		return ""
	}
	return fmt.Sprintf(" (%s)", (time.Duration(*ms) * time.Millisecond).Round(time.Millisecond))
}

// serverMessage extracts the server's own words from an API error, so a
// refusal the server explained well is not paraphrased into something worse.
func serverMessage(err error) string {
	if code := api.CodeOf(err); code != "" {
		return code
	}
	return err.Error()
}
