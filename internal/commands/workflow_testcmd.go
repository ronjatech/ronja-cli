package commands

import (
	"context"
	"errors"
	"fmt"
	"os"
	"sort"
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

// validateLogsMode checks --logs, which `wf test` and `wf run` both take.
//
// Shared, like validateRunTimeout below and runVerdict, because the two verbs
// are one loop: a flag that means something slightly different in each of them
// is a flag nobody can rely on.
func validateLogsMode(mode string) error {
	if mode == logsFull || mode == logsTail || mode == logsNone {
		return nil
	}
	return fmt.Errorf("--logs must be one of %s, %s or %s (got %q)",
		logsFull, logsTail, logsNone, mode)
}

// validateRunTimeout refuses a negative --timeout.
//
// 0 means "wait forever" — a documented value. A negative duration has no such
// meaning and used to land on the same branch, so `--timeout -5m` silently
// waited forever, which is the opposite of what anybody typing a negative
// timeout could have meant.
func validateRunTimeout(timeout time.Duration) error {
	if timeout < 0 {
		return fmt.Errorf("--timeout cannot be negative (got %s) — pass 0 to wait for as long as the run takes", timeout)
	}
	return nil
}

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
		resume    bool
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

--resume restarts the last FAILED run of this workflow instead of starting a
fresh one. Only a durable workflow (runtime 2) has anything to resume: its
journaled step results are replayed and only the work that never finished runs
again. A resume inherits the original run's parameters, so --param is refused
with it.

A durable workflow can also PAUSE mid-run — waiting on an agent it handed work
to, or on a timer. This stops waiting there and reports status "waiting" with
what ran so far: nothing failed, and the run resumes on its own, so the rest of
it happens in Ronja rather than here.

Approval-gated workflows cannot be run from here at all: an approval can only be
produced inside an agent session, so test those from a Ronja chat.

Exits non-zero when the run FAILED — a run that finished, and a durable run that
paused or was handed on to a successor run, all exit zero. With --json, the run
as last read — row, steps and health — as one object on stdout, with progress on
stderr.

Ctrl-C stops the waiting, not the run: it keeps going server-side either way.`,
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			// Ctrl-C is caught at the ROOT (see signalContext), so cmd.Context()
			// is already cancelled on a signal and this command's poll loop takes
			// its cancellation branch rather than the process dying mid-poll. What
			// this command adds is the sentence a person needs at that moment:
			// killing the wait does not kill the run.
			ctx := cmd.Context()

			resolved, err := resolveInstance()
			if err != nil {
				return err
			}
			if err := validateLogsMode(logsMode); err != nil {
				return err
			}
			if err := validateRunTimeout(timeout); err != nil {
				return err
			}
			f, err := openFolder(cmd.Context(), resolved, wfdir.WorkflowKind)
			if err != nil {
				return err
			}
			outcome, err := runTest(ctx, f, testOptions{
				Params:    params,
				WriteLive: writeLive,
				StaleOK:   staleOK,
				Resume:    resume,
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
	cmd.Flags().BoolVar(&resume, "resume", false,
		"resume the last failed run instead of starting a new one (durable workflows only)")
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
	Resume    bool
	Timeout   time.Duration
}

// testOutcome is what the reporters read. Run is the last thing the server
// said about the run, which on the timeout path is a run still going rather
// than a finished one — hence TimedOut, so neither reporter has to infer it
// from a status.
type testOutcome struct {
	Run      *api.RunResponse
	TimedOut bool
	// Durable reports a workflow whose steps are journaled, which is what makes
	// a FAILED run resumable rather than merely re-runnable.
	Durable bool
	// Live marks a run of the LIVE workflow (`wf run`) rather than of the draft.
	//
	// It is read by the report and by nothing else, because the two commands
	// differ only in what is worth saying at the end: a live run has already been
	// published, so "Next: ronja wf publish" would be nonsense; `--resume`
	// continues a run of the DRAFT, so offering it after a live failure would
	// point at a different row; and a park matters more here, because a live run
	// is the one somebody is about to call verified.
	Live bool
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
		return nil, fmt.Errorf("you have no draft of workflow %s, and `wf test` runs the draft rather than the live version.\n  Run `ronja wf push` first — it opens a draft and syncs this folder into it.\n  To run the LIVE workflow as it already stands, run `ronja wf run`",
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
	// that trick from a CLI hint — which is also why the second line names the
	// CONVERSION (a mid-run approval, which the author performs in Ronja with the
	// code change it belongs to) and not the off-switch that would appear to be
	// the same fix and is not.
	if draft.IsGated() {
		return nil, fmt.Errorf("workflow %s has an approval gate, and an approval can only be produced inside an agent session — a run started over HTTP has no way to carry one.\n  Test this workflow from a Ronja chat instead.\n  This is the legacy PRE-RUN gate and it is deprecated: ask Ronja to convert the workflow to a mid-run approval (tools.requireApproval in a Durable workflow), which runs from here, from automations and from apps",
			draft.ID)
	}

	// 4. Parameters, checked locally FIRST. The v2 run endpoint validates them
	// too now, but a --param typo caught here costs no round trip and reports
	// against the declaration already in hand.
	//
	// A resume declares none, and is refused rather than quietly ignoring them:
	// the server rejects parameterValues on a resume because a changed parameter
	// would invalidate every step result computed under the old ones, so a CLI
	// that dropped --param silently would be hiding exactly the mistake that
	// rule exists to catch.
	var values map[string]any
	if opts.Resume {
		if len(opts.Params) > 0 {
			return nil, fmt.Errorf("a resume inherits the failed run's parameters — drop --param, or run without --resume to start a fresh run with new values")
		}
	} else {
		values, err = parseParams(opts.Params, draft.Parameters)
		if err != nil {
			return nil, err
		}
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
	var started *api.WorkflowRun
	if opts.Resume {
		// A distinct name rather than a shadowing `:=`, so `err` below is
		// unambiguously the one the rest of this function uses.
		target, targetErr := lastFailedRun(ctx, client, f, draft)
		if targetErr != nil {
			return nil, targetErr
		}
		// A note, not a refusal. Resume is NOT a durable-only feature: on the
		// standard runtime a step is journaled when the author gives it an
		// explicit key (`tools.step("key", fn, ...)`), and resuming such a
		// workflow replays exactly those. What the durable runtime changes is
		// that the key is derived, so every step is journaled without the author
		// writing one — which is why a standard-runtime resume may well find
		// nothing to skip, and why that is worth saying before it happens.
		if !isDurable(f, draft) {
			fmt.Fprintf(os.Stderr, "  Note: workflow %s runs the standard runtime, so only steps written as tools.step(\"key\", fn, ...) are journaled; anything else re-runs.\n",
				draft.ID)
		}
		started, err = client.ResumeWorkflowRun(ctx, draft.ID, target)
		if err != nil {
			return nil, fmt.Errorf("resume run %s: %s", target, serverMessage(err))
		}
		fmt.Fprintf(os.Stderr, "  Resuming run %s as %s — journaled steps are replayed, not re-run.\n",
			target, started.ID)
	} else {
		started, err = client.RunWorkflow(ctx, draft.ID, values)
		if err != nil {
			return nil, fmt.Errorf("run %s: %s", draft.ID, serverMessage(err))
		}
		fmt.Fprintf(os.Stderr, "  Started run %s of draft %s.\n", started.ID, draft.ID)
	}

	outcome := &testOutcome{
		Durable: isDurable(f, draft),
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
	// What the run's terminal status is worth as an exit code — the same
	// question `wf run` asks, and deliberately answered in one place. See
	// runVerdict.
	return outcome, runVerdict(outcome.Run)
}

// isDurable reports whether the workflow being run journals its steps.
//
// The ROW is the authority — it is the thing the run funnel reads, and it is
// right about a workflow this folder did not create (a `wf clone` records no
// runtime at all, since the runtime is not the folder's to declare once the
// workflow exists). The manifest is the fallback for ONE case: an instance
// predating durable workflows sends no runtimeVersion, which decodes to 0, and
// 0 means "the instance did not say" rather than "runtime 1".
func isDurable(f *folder, row *api.Workflow) bool {
	if row != nil && row.RuntimeVersion > 0 {
		return row.RuntimeVersion >= wfdir.RuntimeDurable
	}
	return f.Manifest.IsDurable()
}

// recentRunsScanned bounds the run history --resume reads to find the last
// failed run. The answer is nearly always the first entry — you run, it fails,
// you fix, you resume — and a workflow with more than this many runs since its
// last failure is one nobody is resuming by hand.
const recentRunsScanned = 20

// lastFailedRun finds the run `--resume` should continue: the most recent
// FAILED run of the row `wf test` runs.
//
// Discovered from the SERVER (GET /workflow/:id/runs) rather than remembered
// locally, and that is the design rather than an economy. A local note of "the
// last run id" would be wrong in every case the folder is not the only way the
// workflow is run — the web builder, an automation, a colleague's checkout —
// and it would be absent exactly when it is most wanted, after the clone that
// follows a failure somebody else saw. The server already knows, per row, and
// the row is the same one the server's own resume rule is written against: the
// target must belong to the workflow the resume is posted to.
//
// It only NOMINATES a target. Every other resume rule (the lineage must have no
// run already running and none that has succeeded; a gated workflow cannot be
// resumed) is the server's, and re-implementing it here would produce a second
// opinion that goes stale — so a nominated run the server refuses comes back as
// the server's own refusal.
func lastFailedRun(ctx context.Context, client *api.Client, f *folder, row *api.Workflow) (string, error) {
	runs, err := client.ListWorkflowRuns(ctx, row.ID, recentRunsScanned)
	if err != nil {
		return "", fmt.Errorf("read the run history of %s: %s", row.ID, serverMessage(err))
	}
	// Sorted here as well as requested of the server: the ordering is what makes
	// "the last failed run" mean anything, and a listing that came back in
	// another order would otherwise resume an ARBITRARY failed run rather than
	// fail to find one — a wrong answer wearing a right one's clothes.
	sort.SliceStable(runs, func(i, j int) bool {
		return runs[i].ExecutedAt.After(runs[j].ExecutedAt)
	})
	for _, run := range runs {
		if run.Status == api.RunStatusError {
			return run.ID, nil
		}
	}

	// Nothing to resume, and there is only ONE situation: no failed run. A
	// standard-runtime workflow is deliberately NOT refused here — resume works
	// on both runtimes, it is the JOURNAL that decides how much a resume skips,
	// and a workflow whose steps carry explicit keys is as resumable as a
	// durable one.
	return "", fmt.Errorf("no failed run of %s to resume — the last %d runs on %s are all running or done.\n  Run `ronja wf test` to start a fresh one",
		row.ID, recentRunsScanned, f.Resolved.URL)
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
	noteRemoteDrift(ctx, client, f, draft, "draft "+draft.ID,
		"the web builder edits the same draft.")
}

// noteRemoteDrift is the body both drift notes share: read the row's files,
// compare them against the sync baseline, and say so when they differ.
//
// One body rather than two near-copies, because the callers differ only in
// which row they name and in the clause explaining how it drifted. Everything
// else — that the baseline has to describe THIS row, that a read failure is
// best-effort rather than a refusal, that silence is the answer when nothing
// differs — is the same rule twice, and a fix that landed in one copy would
// leave `wf test` and `wf run` disagreeing about what drift is.
//
// `subject` names the row as the sentence reads it ("draft draft-1", "the live
// workflow wf-1"); `because` is the clause that follows the dash, punctuation
// included.
func noteRemoteDrift(ctx context.Context, client *api.Client, f *folder, row *api.Workflow, subject, because string) {
	baseline := f.State.For(f.Key)
	if baseline == nil || baseline.SourceID != row.ID {
		return
	}
	files, err := client.ListWorkflowFiles(ctx, row.ID)
	if err != nil {
		// Best effort, like the table-name lookup: not being able to check for
		// drift is no reason to refuse to run.
		return
	}
	diff := wfdir.DiffHashes(hashFiles(files), baseline.Hashes())
	if !diff.Dirty() {
		return
	}
	fmt.Fprintf(os.Stderr, "  Note: %s holds %d change(s) that did not come from this folder — %s `ronja wf status` lists them.\n",
		subject, diff.Total(), because)
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
		// Best effort: an unreadable table (or an unnamed one — a real state for
		// a freshly created row) leaves the name empty and the id is printed
		// alone. Which of the two it was is not worth a second line here.
		name := ""
		if table, err := client.GetTable(ctx, id); err == nil {
			name = table.Name
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
