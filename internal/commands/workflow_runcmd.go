package commands

import (
	"context"
	"fmt"
	"os"
	"time"

	"github.com/ronjatech/ronja-cli/internal/api"
	"github.com/ronjatech/ronja-cli/internal/wfdir"
	"github.com/spf13/cobra"
)

// The file is workflow_runcmd.go for the reason workflow_testcmd.go is not
// workflow_test.go — and because `internal/api/workflow_run.go` already owns the
// client half of a run.

// `ronja wf run` runs the LIVE workflow this folder is bound to.
//
// It is the step AFTER publish, and it is a sync-loop verb rather than a
// wrapper: it runs THIS FOLDER's published artifact, resolved from the binding
// in ronja.json exactly as `wf status` and `wf publish` resolve it, and it takes
// no workflow id at all. Running an arbitrary workflow by id is a one-shot HTTP
// call and stays one (`ronja api POST /api/v2/workflow/<id>/run`, then
// `--wait-until`); what plain HTTP does badly here is the part this command
// owns — knowing which row this folder means, and refusing when the folder and
// that row have come apart.
//
// There is deliberately NO --write-live flag, which is the one place it departs
// from `wf test`. That flag exists because a DRAFT's output tables are the live
// workflow's output tables and testing would replace a production table by
// surprise. The live workflow writing its own bound output tables is not a
// surprise — it is the definition of the thing having been published — and a
// flag guarding it would be a flag everybody types every time, which is a flag
// that guards nothing.
func newWorkflowRunCmd() *cobra.Command {
	var (
		params   []string
		follow   bool
		timeout  time.Duration
		logsMode string
	)
	cmd := &cobra.Command{
		Use:   "run",
		Short: "Run the live workflow this folder is bound to",
		Long: `Run the live workflow this folder is bound to, and report what happened.

The step after ` + "`ronja wf publish`" + `: it runs the LIVE workflow — the one
automations and colleagues use — waits for it to finish, and prints its status,
logs and outputs.

It runs the workflow THIS FOLDER is bound to and takes no workflow id. To run
some other workflow, post to the API directly:

  ronja api POST /api/v2/workflow/<id>/run -f parameterValues='{}'

Everything that can refuse does so before the run starts. An open draft is
refused: your folder tracks the draft, ` + "`ronja wf test`" + ` runs it, and a live run
would not be a run of the code you are looking at. So is a folder holding
changes that are not live, for the same reason.

Parameters are passed one at a time and checked against the workflow's declared
parameters before anything runs:

  ronja wf run --param month=2026-07 --param region=EU

A workflow in a SHARED feature runs under admin access, so an ordinary user is
refused by the server; ask an admin, or let the automation that owns the
workflow trigger it.

A Durable workflow can PAUSE mid-run — waiting on an agent, an approval or a
timer. This stops waiting there and says so loudly: the run has NOT finished,
and this is not a completed verification.

--follow keeps waiting through those pauses instead, until the run finishes or
fails, which is what makes a live run verifiable end to end. A woken run does
not finish under its own id — a successor run carries the work — so a follow
asks the server which run now carries the lineage and reports that one, saying
"resumed as run ..." as it hops. Parks can outlast the 15m default: raise
--timeout, or pass --timeout 0 to wait for as long as it takes. On an instance
too old to have the route, --follow says so and reports the first pause, as it
would without the flag.

Exits non-zero when the run FAILED. With --json, the run as last read — row,
steps and health — as one object on stdout, with progress on stderr.

Ctrl-C stops the waiting, not the run: it keeps going server-side either way.`,
		// Not cobra.NoArgs: the refusal is the doctrine, so it says what the
		// doctrine is rather than "unknown command".
		Args: func(_ *cobra.Command, args []string) error {
			if len(args) > 0 {
				return fmt.Errorf("`ronja wf run` takes no workflow id (got %q) — it runs the workflow THIS FOLDER is bound to, which is what makes it part of the folder loop rather than a wrapper around one endpoint.\n  To run another workflow: ronja api POST /api/v2/workflow/%s/run -f parameterValues='{}'",
					args[0], args[0])
			}
			return nil
		},
		RunE: func(cmd *cobra.Command, args []string) error {
			// Ctrl-C is caught at the ROOT (see signalContext), so the poll loop
			// takes its cancellation branch rather than the process dying mid-poll.
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
			f, err := openFolder(ctx, resolved, wfdir.WorkflowKind)
			if err != nil {
				return err
			}
			outcome, err := runLive(ctx, f, runOptions{
				Params: params, Follow: follow, Timeout: timeout,
				TimeoutChosen: cmd.Flags().Changed("timeout"),
			})
			// The report is emitted even on failure, exactly as `wf test` emits
			// it: a failed run IS the answer, and a timed-out one still carries a
			// run id somebody needs.
			if outcome != nil && outcome.Run != nil {
				if flagJSON {
					if emitErr := emitJSON(outcome.Run); emitErr != nil {
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
	cmd.Flags().BoolVar(&follow, "follow", false, followFlagHelp)
	cmd.Flags().DurationVar(&timeout, "timeout", 15*time.Minute,
		"give up waiting after this long (0 waits forever; the run continues either way)")
	cmd.Flags().StringVar(&logsMode, "logs", logsTail,
		"how much of the run log to print: full, tail or none")
	return cmd
}

type runOptions struct {
	Params []string
	// Follow waits through a Durable run's pauses — see testOptions.Follow.
	Follow  bool
	Timeout time.Duration
	// TimeoutChosen reports that --timeout was typed — see
	// testOptions.TimeoutChosen.
	TimeoutChosen bool
}

// runLive is the whole command, ordered — like runTest — so that everything
// which can refuse does so before the run starts, which is the point at which
// the workflow's side effects stop being hypothetical.
func runLive(ctx context.Context, f *folder, opts runOptions) (*testOutcome, error) {
	// newClient rather than api.New, for the reason it is a var at all: the
	// timeout branch at step 7 is only reachable with a transport that reports
	// a deadline.
	client := newClient(f.Resolved.URL, f.Resolved.Token)

	// 1. Which row. The binding, and nothing else: there is no id argument, so
	// an unbound folder has nothing to run rather than a missing parameter.
	if !f.Bound || f.Binding.WorkflowID == "" {
		return nil, fmt.Errorf("nothing to run — this folder has no workflow on %s yet.\n  Run `ronja wf push` and then `ronja wf publish`: `wf run` runs the LIVE workflow this folder is bound to",
			f.Resolved.URL)
	}
	existing, err := inspectTarget(ctx, client, f)
	if err != nil {
		return nil, err
	}
	live := existing.Workflow
	if live.Lifecycle == api.LifecycleDraft {
		// A parentless draft: created by a first push and never published. There
		// is no live row to run, and publishing is what creates one.
		return nil, fmt.Errorf("workflow %s has never been published, so there is no live version to run.\n  `ronja wf test` runs your draft; `ronja wf publish` takes it live",
			live.ID)
	}

	// 2. An open draft, refused. This is the refusal `wf run` cannot do without,
	// because the staleness check below cannot cover it: the sync baseline
	// describes the row the folder last synced with, which for a folder with a
	// draft is the DRAFT — so after publish → edit → push, a folder that is
	// perfectly clean against its draft would green-light a run of the live
	// version the author stopped looking at two commands ago.
	if draft := existing.Draft; draft != nil {
		return nil, fmt.Errorf("you have an open draft of workflow %s (%s), and this folder tracks the DRAFT — so running live would not run the code you are looking at.\n  `ronja wf test` runs the draft. Publish it (`ronja wf publish`) or throw it away (`ronja wf discard`) before running live",
			live.ID, draft.ID)
	}

	// 3. Run what you are looking at. Same rule as `wf test`'s stale check and a
	// different comparison: there is no draft, so the baseline describes the LIVE
	// row, and a difference is exactly "live is not this folder".
	if err := checkFolderIsLive(f, live); err != nil {
		return nil, err
	}
	// ...and then the half that comparison cannot see, as a note rather than a
	// refusal: a republish since your last sync leaves disk and baseline
	// agreeing with each other and both behind the row that is about to run.
	noteLiveDrift(ctx, client, f, live)

	// 4. The approval gate, BEFORE the run POST rather than as a 400 from it —
	// and it names the CONVERSION rather than the off-switch, for the reason
	// spelled out at the same step of runTest: a caller who learns to disable the
	// gate has learned to commit that governance change onto the live workflow.
	if live.IsGated() {
		return nil, fmt.Errorf("workflow %s has an approval gate, and an approval can only be produced inside an agent session — a run started over HTTP has no way to carry one.\n  Run this workflow from a Ronja chat instead.\n  This is the legacy PRE-RUN gate and it is deprecated: ask Ronja to convert the workflow to a mid-run approval (tools.requireApproval in a Durable workflow), which runs from here, from automations and from apps",
			live.ID)
	}

	// 5. Parameters, through the same coerce-and-check chokepoint `wf test` uses,
	// against the LIVE row's declaration — which is the one the run is validated
	// against server-side.
	values, err := parseParams(opts.Params, live.Parameters)
	if err != nil {
		return nil, err
	}

	// 6. Where this is about to land, out loud and BEFORE the POST. $RONJA_URL
	// and $RONJA_TOKEN outrank the credential file, so the instance a person
	// believes they are pointed at and the one this writes to are not always the
	// same — and a live run is the command where that difference is expensive.
	fmt.Fprintf(os.Stderr, "  Running the live workflow %s on %s.\n", live.ID, describeTarget(f.Resolved))

	// 7. Everything above has passed; from here the run exists in the world.
	//
	// The clock is read BEFORE the POST because it is the only thing that can
	// tell a run this command started from one that was already there — see
	// adoptTimedOutRun, which is why a deadline of ours is not reported as a
	// refusal.
	askedAt := time.Now().UTC()
	started, err := client.RunWorkflow(ctx, live.ID, values)
	switch {
	case err == nil:
		fmt.Fprintf(os.Stderr, "  Started run %s of %s.\n", started.ID, live.ID)
	case api.IsTimeout(err):
		started, err = adoptTimedOutRun(ctx, client, live, askedAt, err)
		if err != nil {
			return nil, err
		}
		fmt.Fprintf(os.Stderr, "  The request timed out, but run %s of %s had already started — following that one rather than starting a second.\n",
			started.ID, live.ID)
	default:
		return nil, refusedRunPost(live, err)
	}

	outcome := &testOutcome{
		Live:    true,
		Durable: isDurable(f, live),
		// A synthesized response, for the reason runTest synthesizes one: the
		// first poll can fail, and a caller who has just started a run must still
		// be told its id. Health "done" is what the server would say about a run
		// with no steps yet — see deriveRunHealth.
		Run: &api.RunResponse{
			WorkflowRun: *started,
			Steps:       []api.StepDTO{},
			Health:      api.RunHealthDone,
		},
	}

	noteFollowTimeout(opts.Follow, opts.Timeout, opts.TimeoutChosen)
	final, err := runWaiter(opts.Follow)(ctx, client, started.ID, opts.Timeout, outcome)
	if final != nil {
		outcome.Run = final
	}
	if err != nil {
		return outcome, err
	}
	return outcome, runVerdict(outcome.Run)
}

// checkFolderIsLive refuses a run of a live workflow this folder cannot vouch
// for.
//
// Three ways not to be able to vouch, and all of them have to refuse rather
// than warn, because the whole claim `wf run` makes is "this ran the code in
// front of you". They are three messages and not one, because the remedies are
// not the same:
//
//   - there is no baseline at all. .ronja/ is local-only, so a copy cloned from
//     git starts without one and there is nothing to compare live against. A
//     fresh `wf clone` is the only way to get one.
//   - the baseline describes some OTHER row. Not a discarded draft — `wf
//     discard` re-anchors the baseline on live, as does every publish that
//     commits. The way in is the review path: on a shared workflow `wf publish`
//     SUBMITS the draft and returns without refreshing the baseline, so once an
//     admin approves it the folder holds a baseline naming a draft that is gone
//     and a live row it has never synced with. The files are usually exactly
//     what went live; what is missing is the record that says so, which is why
//     this must not be worded as "your folder is a stranger's".
//   - the folder differs from it. Same comparison `wf status` calls "local
//     changes" and `wf test` refuses on — except that with no draft in play the
//     baseline IS the live row, so the difference is between the files on disk
//     and the files that would run.
//
// Note what it does NOT do: read the live row's files. Both sides of this
// comparison are local, so what it guarantees is "this folder is what you last
// synced with the live row" and not "this folder is what the live row holds
// now" — a colleague who republished over a folder nobody has touched locally
// leaves disk and baseline in perfect agreement and both of them behind the
// code that would run. That gap is announced by noteLiveDrift, which costs a
// round trip and so notes rather than refuses.
func checkFolderIsLive(f *folder, live *api.Workflow) error {
	baseline := f.State.For(f.Key)
	if baseline == nil {
		return fmt.Errorf("this folder has no sync baseline for the live workflow %s on %s, so there is no way to tell whether it holds the code that would run — .ronja/ is local-only, so a copy cloned from git starts without one.\n  Run `ronja wf clone %s` into a fresh folder to get one",
			live.ID, f.Resolved.URL, live.ID)
	}
	if baseline.SourceID != live.ID {
		return fmt.Errorf("this folder's sync baseline came from %s (%s), not from the live workflow %s, so there is no way to tell whether it holds the code that would run.\n  Usually the folder is fine and only the record is missing: publishing a shared workflow submits your draft for review, and when an admin commits it nothing re-anchors this folder on the live row. `ronja wf status` shows the comparison.\n  Run `ronja wf clone %s` into a fresh folder to anchor one on live",
			baseline.SourceID, baseline.SourceLifecycle, live.ID, live.ID)
	}
	enumeration, err := wfdir.Enumerate(f.Root, wfdir.WorkflowKind)
	if err != nil {
		return err
	}
	diff := wfdir.DiffHashes(enumeration.Files, baseline.Hashes())
	if !diff.Dirty() {
		return nil
	}
	return fmt.Errorf("this folder has %d change(s) that are not in the live workflow %s, so running it would run code you are not looking at:\n%s  Run `ronja wf push` and `ronja wf publish` to take them live, or `ronja wf test` to try them out as a draft",
		diff.Total(), live.ID, localChangeSummary(diff))
}

// noteLiveDrift says so, without refusing, when the live workflow holds changes
// this folder never published.
//
// It is the drift checkFolderIsLive is structurally blind to: that comparison
// is local against local, so a colleague's republish over a folder with no
// local edits passes it clean. A note rather than a gate, for the reason
// noteDraftDrift is one: the live row is what this command runs BY DEFINITION,
// so refusing would only invite a flag whose meaning is "yes, run the published
// workflow" — but a clean folder otherwise reads as "you are running exactly
// what you are looking at", and after a republish it is not.
//
// Compared only when the baseline came from THIS live row, which is the state
// checkFolderIsLive has already established by the time this runs — and which
// noteRemoteDrift, which does the work, re-checks in any case.
func noteLiveDrift(ctx context.Context, client *api.Client, f *folder, live *api.Workflow) {
	noteRemoteDrift(ctx, client, f, live, "the live workflow "+live.ID,
		"somebody published over it since your last sync.")
}

// runsScannedAfterTimeout bounds the run history the reconcile below reads. The
// run it is looking for is the newest one there is, so a handful is plenty —
// the extra entries exist only so a colleague's run starting in the same second
// cannot push ours out of the window.
const runsScannedAfterTimeout = 5

// adoptTimedOutRun answers the question a run POST that died on OUR deadline
// leaves open: did the run start?
//
// It very likely did. The server commits the workflow_runs row and only then
// dispatches the work asynchronously, so by the time anything could be slow
// enough to time out, the run exists. Reported as a plain failure, the operator
// reads "it did not run" and types the command again — and a second run of a
// live workflow is not a harmless retry: replace-mode output tables are written
// twice, and side effects outside Ronja happen twice.
//
// So the history is read back and the newest run stamped at or after the moment
// we asked is adopted, which the caller then follows exactly as if the POST had
// answered. The comparison is against the SERVER's clock, so a server running
// behind ours matches nothing and falls through to the message below — the safe
// direction: refusing to adopt costs a sentence telling somebody to go and
// look, while a fudge factor wide enough to absorb skew is wide enough to adopt
// a colleague's run and report it as this command's.
//
// Nothing is retried and nothing is started. If the run cannot be found, the
// error says the run MAY have started and how to check, because the one thing
// this must never say is that it did not.
func adoptTimedOutRun(ctx context.Context, client *api.Client, live *api.Workflow, askedAt time.Time, cause error) (*api.WorkflowRun, error) {
	runs, err := client.ListWorkflowRuns(ctx, live.ID, runsScannedAfterTimeout)
	if err != nil {
		return nil, timedOutRunUnconfirmed(live, cause,
			fmt.Sprintf("and its run history could not be read either (%s)", serverMessage(err)))
	}
	var newest *api.WorkflowRun
	for i := range runs {
		run := &runs[i]
		if run.ExecutedAt.Before(askedAt) {
			continue
		}
		if newest == nil || run.ExecutedAt.After(newest.ExecutedAt) {
			newest = run
		}
	}
	if newest == nil {
		return nil, timedOutRunUnconfirmed(live, cause, "and no run of it has started since")
	}
	return newest, nil
}

// timedOutRunUnconfirmed is what this command says when it cannot tell. The
// sentence that matters is the second one: an unconfirmed run is not a run that
// did not happen, and the reader is about to decide whether to type the command
// again.
func timedOutRunUnconfirmed(live *api.Workflow, cause error, detail string) error {
	return fmt.Errorf("starting a run of workflow %s timed out (%s) %s.\n  It MAY STILL BE RUNNING: the server records the run before it dispatches the work, so a deadline of ours says nothing about what it did.\n  Look before running again — a second run does the work twice, and a replace-mode table write done twice is not the same as done once: ronja api \"/api/v2/workflow/%s/runs?orderBy=executed_at%%20desc&limit=5\"",
		live.ID, serverMessage(cause), detail, live.ID)
}

// refusedRunPost renders the two refusals only the server can make, so neither
// arrives as a bare status code.
//
// Everything else — the credit kill-stop above all — is passed through in the
// server's own words, exactly as runTest passes it: the sentence that says what
// to do about it is the server's, and paraphrasing it loses that.
func refusedRunPost(live *api.Workflow, err error) error {
	switch api.StatusOf(err) {
	case 403:
		// The gate the CLI cannot get past and must not pretend to: a workflow in
		// a SHARED feature is admin-only to run, and no flag here changes that.
		// Named as the LIKELY cause rather than the only one — a scope-restricted
		// token is refused with the same status, and a message that swore it was
		// the role would send somebody hunting a permission they already have.
		// The server's own sentence is carried either way, and naming the two ways
		// forward matters more than naming the rule.
		return fmt.Errorf("you may not run workflow %s (%s).\n  Usually this means the shared-feature gate: a workflow in a shared feature runs under admin access. A token restricted to narrower scopes is refused the same way.\n  Ask an admin to run it, or let the automation that owns this workflow trigger it",
			live.ID, serverMessage(err))
	case api.StatusConflict:
		return refusedForConcurrency(live, err)
	}
	return fmt.Errorf("run %s: %s", live.ID, serverMessage(err))
}

// refusedForConcurrency renders the `skip` concurrency policy's refusal.
//
// The one thing this must get across is that NOTHING RAN — a 409 here is not a
// run that failed, it is a run that never started — and the id of the run
// holding the slot, which is read from the response's own `blockingRunId`
// rather than out of the message. Both halves are the server's, so a reworded
// sentence cannot change what this reports.
//
// That id can be absent — the server omits the details entirely when the run
// that lost the race is anonymous (see gt.NewRunInFlightConflict) — and the
// clause naming it goes with it, rather than printing "run  holds the slot".
func refusedForConcurrency(live *api.Workflow, err error) error {
	blocking, ok := api.AsRunInFlight(err)
	if !ok || blocking.BlockingRunID == "" {
		return fmt.Errorf("workflow %s did not start a run: %s.\n  Nothing ran — this workflow is set to skip overlapping runs. Try again once the run in flight has finished",
			live.ID, serverMessage(err))
	}
	return fmt.Errorf("workflow %s did not start a run: %s.\n  Nothing ran — this workflow is set to skip overlapping runs, and run %s holds the slot (%s). Follow it with `ronja api /api/v2/workflow/run/%s`, then run again",
		live.ID, serverMessage(err), blocking.BlockingRunID, blocking.BlockingRunStatus, blocking.BlockingRunID)
}

// runVerdict turns the run's terminal status into this command's exit code, for
// both `wf test` and `wf run`.
//
// Exit zero means the run DID NOT FAIL — which since durable waits is a wider
// claim than "it finished", because a run can stop being this command's
// business without being over. The interesting branch is the last one: a status
// added server-side (a "cancelled", say) ends the poll — see
// api.RunResponse.Running — and would otherwise fall through to a zero exit,
// telling a script that something it never heard of was a pass.
//
// Shared, and not merely to save a switch: the two commands must agree about
// what a park is worth, and the day they disagree is the day a pipeline over one
// of them treats a parked run as a failure and the other does not.
func runVerdict(run *api.RunResponse) error {
	switch run.Status {
	case api.RunStatusDone:
		return nil
	case api.RunStatusWaiting:
		// A park is not a failure: the burst finished cleanly, its outputs are
		// persisted, and the run continues by itself when whatever it waits on
		// happens. A non-zero exit here would stop a pipeline over a durable
		// workflow doing exactly what it was written to do, and there would be
		// nothing to retry — the run is parked, not stuck. The report says so, and
		// says it louder for a live run, which is the one somebody is about to
		// call verified.
		return nil
	case api.RunStatusResuming:
		// The wake latch. This row handed the lineage to a successor run and will
		// never execute again, so it is terminal here — and it is terminal because
		// the work is CONTINUING, which is not a failure either.
		return nil
	case api.RunStatusError:
		return fmt.Errorf("run %s failed", run.ID)
	default:
		return fmt.Errorf("run %s ended with status %q, which this version of the CLI does not know — treating it as a failure rather than a pass.\n  The report above is what the instance said; upgrade the CLI (run: ronja update) if this status is a new one",
			run.ID, run.Status)
	}
}
