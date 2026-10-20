package commands

import (
	"fmt"
	"strings"
	"testing"

	"github.com/ronjatech/ronja-cli/internal/api"
)

// A run from the run list (adoptTimedOutRun) carries only its first 20 table
// outputs plus the full count.
func TestPrintTestReportNamesTablesBeyondTheListedOnes(t *testing.T) {
	tables := make([]api.WorkflowRunTableOutput, 20)
	for i := range tables {
		tables[i] = api.WorkflowRunTableOutput{DisplayName: fmt.Sprintf("region_%02d", i), WriteMode: "replace", FileCount: 1}
	}
	report := func(count int) string {
		o := &testOutcome{Run: &api.RunResponse{WorkflowRun: api.WorkflowRun{
			ID:               "workflowrun-1",
			Status:           api.RunStatusDone,
			TableOutputs:     tables,
			TableOutputCount: count,
		}}}
		stdout, restore := captureStdout(t)
		printTestReport(o, logsNone)
		return restore(stdout)
	}

	out := report(1000)
	if want := "and 980 more (ronja api /api/v2/workflow/run/workflowrun-1/table-outputs)"; !strings.Contains(out, want) {
		t.Errorf("report for a 1,000-table run read from the list is missing %q:\n%s", want, out)
	}

	// A whole run, and a server that predates the count.
	for _, count := range []int{20, 0} {
		if out := report(count); strings.Contains(out, " more (") {
			t.Errorf("count %d: report claims tables beyond the 20 listed:\n%s", count, out)
		}
	}
}
