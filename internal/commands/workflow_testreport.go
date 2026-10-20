package commands

import (
	"fmt"
	"os"
	"strings"
	"time"

	"github.com/ronjatech/ronja-cli/internal/api"
)

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
			// A journal hit and a continue_on_error skip both arrive as
			// "skipped"; on a resume one is done work, the other never ran.
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
		// A run adopted from the run list (adoptTimedOutRun) carries only its
		// first 20 tables.
		if more := run.RunTableOutputCount() - len(run.TableOutputs); more > 0 {
			fmt.Fprintf(out, "    and %d more (ronja api /api/v2/workflow/run/%s/table-outputs)\n", more, run.ID)
		}
	}

	printRunLogs(out, run.Logs, logsMode)

	switch {
	// A live run gets no next verb: the code is already published.
	case run.Status == api.RunStatusDone && !o.Live:
		fmt.Fprintf(out, "\n  Next: ronja wf publish\n")
	// Louder for a live run, which somebody is about to record as a
	// verification: the exit code is still zero (runVerdict), so this line is
	// all that stands between an unfinished run and a green tick. "Waiting" and
	// "Continued" are the product's words (docs-site/TERMINOLOGY.md).
	case run.Status == api.RunStatusWaiting && o.Live:
		fmt.Fprintf(out, "\n  WAITING:  the run has NOT finished — it is waiting on an agent, an approval or a\n")
		fmt.Fprintf(out, "            timer, so this is NOT a completed verification.\n")
		fmt.Fprintf(out, "            It resumes on its own: nothing failed, and there is nothing to re-run.\n")
		fmt.Fprintf(out, "            Check on it later: ronja api /api/v2/workflow/run/%s\n", run.ID)
	// Deliberately neither the publish nor the resume hint: the run has not
	// finished, and nothing has stopped it.
	case run.Status == api.RunStatusWaiting:
		fmt.Fprintf(out, "\n  Waiting:  the run paused — a durable workflow waiting on an agent or a timer.\n")
		fmt.Fprintf(out, "            It resumes on its own: nothing failed, and there is nothing to re-run.\n")
		fmt.Fprintf(out, "            Check on it later: ronja api /api/v2/workflow/run/%s\n", run.ID)
	// The wake latch, louder for a live run for the same reason: a poll can end
	// here when the successor was minted between two polls, and that is no
	// finished verification either.
	case run.Status == api.RunStatusResuming && o.Live:
		fmt.Fprintf(out, "\n  CONTINUED: the run has NOT finished — the wait ended and the work continued in a\n")
		fmt.Fprintf(out, "             NEWER run that is still executing, so this is NOT a completed verification.\n")
		fmt.Fprintf(out, "             Nothing failed, and there is nothing to re-run: follow the newer run in Ronja.\n")
	// The wake latch: the successor carries the lineage, so the run id printed
	// above is not the one to follow.
	case run.Status == api.RunStatusResuming:
		fmt.Fprintf(out, "\n  Continued: the wait ended and the work continued in a NEWER run, so this one is\n")
		fmt.Fprintf(out, "             finished with. Nothing failed; follow the newer run in Ronja.\n")
	// Every failed run whose code never started, on any runtime, live or
	// draft. Before the resume hint, whose "fix the code" advice is only honest
	// once the author's code has run.
	case run.Status == api.RunStatusError && !userCodeRan(run):
		printNothingRanNotice(out, run.Error, retryInvocation(o.Live))
	// Draft runs only (`wf test --resume` continues a draft run), for a durable
	// workflow or any lineage with a journal: a standard-runtime workflow with
	// explicitly keyed steps has work to skip too.
	case run.Status == api.RunStatusError && !o.Live && (o.Durable || run.JournalEntries > 0):
		printResumeHint(out, run.JournalEntries)
	}
}

// userCodeRan reports whether the author's code reached the interpreter. The
// harness stamps processing_started_at just before exec(code), which also
// compiles the entrypoint, so no stamp means nothing ran, syntax errors
// included. That ping is best-effort, so steps, logs or a traceback each
// outvote a missing stamp.
//
// ⚠️ JournalEntries must not be a signal: it counts the resume lineage, so a
// resumed run that died before its container started would inherit a yes.
func userCodeRan(run *api.RunResponse) bool {
	return run.ProcessingStartedAt != nil ||
		len(run.Steps) > 0 ||
		run.Logs != "" ||
		looksLikeTraceback(run.Error)
}

// looksLikeTraceback reports an error that is a Python traceback, which only
// the interpreter writes.
func looksLikeTraceback(err *string) bool {
	if err == nil {
		return false
	}
	return strings.Contains(*err, "Traceback (most recent call last)") ||
		strings.Contains(*err, `File "`)
}

// printNothingRanNotice says the failure happened before the author's code was
// reached. It says where the run died, never whose fault it is: a bad
// pipPackages list in ronja.json also fails before exec. retry is the
// invocation that produced the run.
func printNothingRanNotice(out *os.File, runErr *string, retry string) {
	reason := firstLine(runErr)
	if reason != "" {
		fmt.Fprintf(out, "\n  Nothing in your code ran — the run failed while Ronja was setting it up\n")
		fmt.Fprintf(out, "  (%s).\n", reason)
	} else {
		fmt.Fprintf(out, "\n  Nothing in your code ran — the run failed while Ronja was setting it up.\n")
	}
	fmt.Fprintf(out, "  Run `%s` again; if it repeats, report it.\n", retry)
	// The backend's wording for a refused package list, which a re-run cannot
	// fix.
	if strings.HasPrefix(reason, "invalid pip packages") {
		fmt.Fprintf(out, "  This one is about the folder's pipPackages — check them in ronja.json.\n")
	}
}

func retryInvocation(live bool) string {
	if live {
		return "ronja wf run"
	}
	return "ronja wf test"
}

func firstLine(s *string) string {
	if s == nil {
		return ""
	}
	line, _, _ := strings.Cut(*s, "\n")
	return strings.TrimSpace(line)
}

// printResumeHint offers `wf test --resume`. The count is the lineage's
// journalEntries, deliberately not len(Steps) (run spans, a different set); a
// zero prints no count, since an instance that omits the field also sends zero.
func printResumeHint(out *os.File, journaled int) {
	fmt.Fprintf(out, "\n  Resume:   ronja wf test --resume\n")
	if journaled > 0 {
		fmt.Fprintf(out, "            %d %s journaled — a resume skips them\n",
			journaled, plural(journaled, "step"))
	}
	fmt.Fprintf(out, "            Fix the code and push first if the failure was a bug — a resume runs the CURRENT files.\n")
}

// describeRunStatus renders status and health together. The two durable-wait
// statuses are answered first: a bare "waiting" reads as stuck and a bare
// "resuming" as still going, and both invite re-running the work.
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
