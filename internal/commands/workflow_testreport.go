package commands

import (
	"fmt"
	"os"
	"strings"
	"time"

	"github.com/ronjatech/ronja-cli/internal/api"
)

// The terminal report `ronja wf test` prints when it is not in --json mode:
// the finished run, its steps, what it produced, and as much of its log as
// --logs asks for.
//
// Its own file because it is all presentation — it decides nothing and touches
// nothing — and keeping it apart from the command means the ordering of the
// refusals in workflow_testcmd.go, which is the part with safety properties,
// reads top to bottom without a page of Fprintf in the middle.

func printTestReport(o *testOutcome, logsMode string) {
	out := os.Stdout
	run := o.Run

	fmt.Fprintf(out, "\n  Run:      %s\n", run.ID)
	if o.TimedOut {
		fmt.Fprintf(out, "  Status:   still running (this command stopped waiting)\n")
	} else {
		fmt.Fprintf(out, "  Status:   %s\n", describeRunStatus(run))
	}
	if run.CompletedAt != nil && !run.ExecutedAt.IsZero() {
		fmt.Fprintf(out, "  Took:     %s\n", run.CompletedAt.Sub(run.ExecutedAt).Round(time.Millisecond))
	}
	if run.Error != nil && *run.Error != "" {
		fmt.Fprintf(out, "\n  Error:    %s\n", *run.Error)
	}

	if len(run.Steps) > 0 {
		fmt.Fprintf(out, "\n  Steps\n")
		for _, step := range run.Steps {
			// "replayed" rather than the raw status for a journal hit: it and a
			// continue_on_error skip both arrive as status "skipped", and on a
			// resume the difference is the whole point — one step's work is
			// already done, the other's never happened.
			label := step.Status
			if step.Replayed {
				label = "replayed"
			}
			fmt.Fprintf(out, "    %-9s %s%s\n", label, step.Name, formatDuration(step.DurationMs))
			if step.Error != nil && *step.Error != "" {
				fmt.Fprintf(out, "              %s\n", *step.Error)
			}
		}
	}

	if len(run.Outputs) > 0 {
		fmt.Fprintf(out, "\n  Files produced\n")
		for _, output := range run.Outputs {
			name := output.Name
			if name == "" {
				name = output.FileKey
			}
			fmt.Fprintf(out, "    %s (%s)\n", name, output.Format)
		}
	}
	if len(run.TableOutputs) > 0 {
		fmt.Fprintf(out, "\n  Tables written\n")
		for _, table := range run.TableOutputs {
			fmt.Fprintf(out, "    %s  %s, %d %s\n", table.DisplayName, table.WriteMode,
				table.FileCount, plural(table.FileCount, "file"))
		}
	}

	printRunLogs(out, run.Logs, logsMode)

	switch {
	// A live run says nothing here on purpose: the code is already published,
	// and there is no next verb in the loop to name.
	case run.Status == api.RunStatusDone && !o.Live:
		fmt.Fprintf(out, "\n  Next: ronja wf publish\n")
	// The same pause, said louder, because a live run is the one somebody is
	// about to record as a verification — and a run that has not finished is not
	// one. The exit code is still zero (nothing failed, see runVerdict), so this
	// line is all that stands between a paused run and a green tick.
	//
	// WAITING and CONTINUED are the product's own words for these two states
	// (docs-site/TERMINOLOGY.md), which bans "parked" and "resuming" for them
	// precisely because both read as trouble: one as stuck, the other as this
	// run still going. The engineering prose in these files still says "park" —
	// that is internal vocabulary, and it is not what the reader sees.
	case run.Status == api.RunStatusWaiting && o.Live:
		fmt.Fprintf(out, "\n  WAITING:  the run has NOT finished — it is waiting on an agent, an approval or a\n")
		fmt.Fprintf(out, "            timer, so this is NOT a completed verification.\n")
		fmt.Fprintf(out, "            It resumes on its own: nothing failed, and there is nothing to re-run.\n")
		fmt.Fprintf(out, "            Check on it later: ronja api /api/v2/workflow/run/%s\n", run.ID)
	// A waiting run gets NEITHER of the two hints below, and both omissions are
	// the point. The publish hint would invite taking code live on the strength
	// of a run that has not finished; the resume hint would offer to continue a
	// run that nothing has stopped. What it gets instead is the sentence that
	// keeps somebody from re-running the work by hand: it is coming back on its
	// own.
	case run.Status == api.RunStatusWaiting:
		fmt.Fprintf(out, "\n  Waiting:  the run paused — a durable workflow waiting on an agent or a timer.\n")
		fmt.Fprintf(out, "            It resumes on its own: nothing failed, and there is nothing to re-run.\n")
		fmt.Fprintf(out, "            Check on it later: ronja api /api/v2/workflow/run/%s\n", run.ID)
	// The wake latch, said louder, for the reason the pause is: a poll can land
	// on this status and end there — the wait ended and the successor run was
	// minted between two polls — and that is no more a finished verification
	// than a pause is. Same zero exit, same reader about to record a green tick,
	// so the same loud line has to stand in the way. What differs is why it is
	// not finished: the work did not stop, it moved.
	case run.Status == api.RunStatusResuming && o.Live:
		fmt.Fprintf(out, "\n  CONTINUED: the run has NOT finished — the wait ended and the work continued in a\n")
		fmt.Fprintf(out, "             NEWER run that is still executing, so this is NOT a completed verification.\n")
		fmt.Fprintf(out, "             Nothing failed, and there is nothing to re-run: follow the newer run in Ronja.\n")
	// The wake latch. This row is finished with — its successor carries the
	// lineage from here — so the run id printed above is not the one to follow,
	// which is the whole reason this says anything at all.
	case run.Status == api.RunStatusResuming:
		fmt.Fprintf(out, "\n  Continued: the wait ended and the work continued in a NEWER run, so this one is\n")
		fmt.Fprintf(out, "             finished with. Nothing failed; follow the newer run in Ronja.\n")
	// The hint is offered for a durable workflow OR for any failed run whose
	// lineage actually has a journal — resume is not durable-only, and a
	// standard-runtime workflow with explicitly-keyed steps has real work to
	// skip. Gating on the runtime alone would hide the offer from exactly the
	// authors who already took the trouble to write the keys.
	// Offered for a DRAFT run only: `wf test --resume` continues a run of the
	// draft, and a live failure has no draft to continue it from.
	case run.Status == api.RunStatusError && !o.Live && (o.Durable || run.JournalEntries > 0):
		printResumeHint(out, run.JournalEntries)
	}
}

// printResumeHint offers the one thing a failed durable run makes possible:
// fixing the code and continuing, rather than starting over.
//
// The count comes from the run response's journalEntries — the size of the
// LINEAGE's journal, which the server derives on read and populates only for a
// failed run. It is deliberately not len(Steps): steps are the observable
// timeline (run spans), a different set from the journal, and printing one as
// the other would be a confident wrong number in exactly the place somebody
// decides whether to trust a resume to skip work. A zero prints no line at all
// rather than "0 steps journaled", which reads as a fact about the journal when
// it is equally the answer from an instance that does not send the field.
func printResumeHint(out *os.File, journaled int) {
	fmt.Fprintf(out, "\n  Resume:   ronja wf test --resume\n")
	if journaled > 0 {
		fmt.Fprintf(out, "            %d %s journaled — a resume skips them\n",
			journaled, plural(journaled, "step"))
	}
	fmt.Fprintf(out, "            Fix the code and push first if the failure was a bug — a resume runs the CURRENT files.\n")
}

// describeRunStatus renders status and health together, saying what a degraded
// run means rather than leaving a one-word label to carry it.
//
// The two durable-wait statuses are read off the STATUS and answered first,
// before health is consulted at all. Partly because a parked run's health is
// "waiting" too and repeating the word twice says nothing, but mostly because
// each of them reads wrong on its own: a bare "waiting" reads as stuck, and a
// bare "resuming" reads as this run still going — and both mistakes point at
// re-running work that is either coming back by itself or already under way.
func describeRunStatus(run *api.RunResponse) string {
	switch run.Status {
	case api.RunStatusWaiting:
		return "waiting — nothing failed, and it resumes on its own"
	case api.RunStatusResuming:
		return "resuming — continued in a newer run"
	}
	switch run.Health {
	case api.RunHealthDegraded:
		return run.Status + " — but a step failed (degraded)"
	case api.RunHealthFailed:
		return run.Status + " (failed)"
	default:
		return run.Status
	}
}

func printRunLogs(out *os.File, logs, mode string) {
	if mode == logsNone || strings.TrimSpace(logs) == "" {
		return
	}
	lines := strings.Split(strings.TrimRight(logs, "\n"), "\n")
	if mode == logsTail && len(lines) > logsTailLines {
		fmt.Fprintf(out, "\n  Logs (last %d of %d lines — --logs=full for all)\n", logsTailLines, len(lines))
		lines = lines[len(lines)-logsTailLines:]
	} else {
		fmt.Fprintf(out, "\n  Logs\n")
	}
	for _, line := range lines {
		fmt.Fprintf(out, "    %s\n", line)
	}
}
