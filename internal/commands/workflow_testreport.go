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
	// EVERY failed run whose code never started, on any runtime, live or draft.
	// The fact being reported is about the RUN — it died getting S3 credentials,
	// or launching the container, before anything the author wrote was evaluated
	// — and that is exactly as true of `wf run` against live and of a v1 draft as
	// it is of a durable one. Gating it on the resume hint's conditions, as it
	// was, meant a live run and a v1 run kept printing a bare error the author
	// had no way to read as anything but their own bug.
	//
	// FIRST, so it wins over the resume hint below: "fix the code and push first
	// if the failure was a bug" is advice about the author's files, and it is
	// only honest once the author's files have been run.
	case run.Status == api.RunStatusError && !userCodeRan(run):
		printNothingRanNotice(out, run.Error, retryInvocation(o.Live))
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

// userCodeRan reports whether the author's code reached the interpreter at all.
//
// processing_started_at is the primary signal, and it is a strong one: the
// Python harness calls tools._report_run_started() immediately before
// exec(code, ...) (infra, cmd/py-invoke/shared/execution.py), the entrypoint is
// COMPILED inside that exec — its SyntaxError is caught after it — and the
// workflow's own module imports run inside it too. So a NULL stamp means
// nothing the author wrote was ever evaluated, syntax errors included.
//
// The other three are fallbacks, and they exist because that ping is
// best-effort: it goes out on a background worker and is skipped outright on a
// network failure, so a run that DID execute can arrive here unstamped. Steps,
// captured logs and a Python traceback each prove execution on their own, so
// any of them outvotes a missing stamp. The asymmetry is deliberate: wrongly
// saying "nothing ran" hides a real bug from its author, which is worse than
// the message this replaces.
//
// ⚠️ JournalEntries is deliberately NOT one of them, though it reads like the
// obvious fourth. Every signal here has to be a fact about THIS run, and that
// one is not: it counts what the resume LINEAGE has journaled, derived on read
// (see api.RunResponse), so a `wf test --resume` inherits a non-zero count from
// the runs before it. A resumed run that dies before its container starts —
// exactly the S3-credential and container-launch failures this notice was
// written for — then arrives with no stamp, no steps and no logs, and a journal
// count from work that ran yesterday would vote yes and suppress the notice on
// the very run that needs it.
func userCodeRan(run *api.RunResponse) bool {
	return run.ProcessingStartedAt != nil ||
		len(run.Steps) > 0 ||
		run.Logs != "" ||
		looksLikeTraceback(run.Error)
}

// looksLikeTraceback reports an error that is a Python traceback. Only the
// interpreter writes one, so it is evidence of execution that survives the ping
// having been skipped.
func looksLikeTraceback(err *string) bool {
	if err == nil {
		return false
	}
	return strings.Contains(*err, "Traceback (most recent call last)") ||
		strings.Contains(*err, `File "`)
}

// printNothingRanNotice says that the failure happened before the author's code
// was reached — the one thing that turns an infrastructure failure from a
// puzzle about the folder into a re-run.
//
// It reports WHERE the run died, and nothing about WHOSE FAULT that is. The
// notice used to add "That is not a bug in your files", and it was not entitled
// to: userCodeRan answers whether the interpreter evaluated anything, which is a
// fact about the run, not about blame — and the pre-exec path has at least one
// author-caused member. A workflow whose ronja.json declares a package that does
// not exist is refused before ExecV2 ever starts (manalysis fails the run with
// "invalid pip packages: …"), and a declared package whose install fails lands
// on the same never-executed path. Telling that author their files are innocent
// sends them looking anywhere but at the one line they wrote wrong.
//
// The error's FIRST LINE only: the full message is already on the Error: line
// above, and repeating it in full here would bury the sentence that matters.
//
// retry is the invocation to repeat, because this notice is now printed for a
// LIVE run too and `ronja wf test` is not the command that produced one. A
// re-run advice naming the wrong verb is the same class of untruth the notice
// exists to remove.
func printNothingRanNotice(out *os.File, runErr *string, retry string) {
	// The sentence has to END either way. An instance that sent no error at all
	// leaves nothing for the parenthetical, and the version without it used to
	// stop mid-line — which reads as output the terminal cut off rather than as
	// a message with nothing more to say.
	reason := firstLine(runErr)
	if reason != "" {
		fmt.Fprintf(out, "\n  Nothing in your code ran — the run failed while Ronja was setting it up\n")
		fmt.Fprintf(out, "  (%s).\n", reason)
	} else {
		fmt.Fprintf(out, "\n  Nothing in your code ran — the run failed while Ronja was setting it up.\n")
	}
	fmt.Fprintf(out, "  Run `%s` again; if it repeats, report it.\n", retry)
	// The one pre-exec failure the author can act on, named exactly. The prefix
	// is the backend's own wording for a package list it refused before starting
	// the container, so matching on it points at the file that holds the list
	// rather than leaving a re-run as the only advice on offer — a re-run of an
	// unchanged package list fails identically every time.
	if strings.HasPrefix(reason, "invalid pip packages") {
		fmt.Fprintf(out, "  This one is about the folder's pipPackages — check them in ronja.json.\n")
	}
}

// retryInvocation names the command that produced this run, which is the one
// worth repeating.
func retryInvocation(live bool) string {
	if live {
		return "ronja wf run"
	}
	return "ronja wf test"
}

// firstLine is the leading line of a possibly-nil, possibly-multi-line message.
func firstLine(s *string) string {
	if s == nil {
		return ""
	}
	line, _, _ := strings.Cut(*s, "\n")
	return strings.TrimSpace(line)
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
