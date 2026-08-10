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
			fmt.Fprintf(out, "    %-9s %s%s\n", step.Status, step.Name, formatDuration(step.DurationMs))
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

	if run.Status == api.RunStatusDone {
		fmt.Fprintf(out, "\n  Next: ronja wf publish\n")
	}
}

// describeRunStatus renders status and health together, saying what a degraded
// run means rather than leaving a one-word label to carry it.
func describeRunStatus(run *api.RunResponse) string {
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
