package commands

import (
	"fmt"
	"strings"
)

// failedFile is one file a push or publish did not land, for the list that ends
// its summary.
type failedFile struct{ path, reason string }

// maxListedFailures caps the list, so a folder where everything failed does not
// bury the count sentence under hundreds of lines.
const maxListedFailures = 20

// listFailedFiles is the tail a count-only summary gains: one line per failed
// file with the first line of its reason. Each reason was printed as it
// happened, far above the summary; this repeats it where the last lines are
// read. The result starts with ":" so the count sentence before it stays a
// prefix of the whole.
func listFailedFiles(files []failedFile) string {
	if len(files) == 0 {
		return ""
	}
	var b strings.Builder
	b.WriteString(":")
	for i, file := range files {
		if i == maxListedFailures {
			fmt.Fprintf(&b, "\n    and %d more", len(files)-i)
			break
		}
		reason := firstLine(&file.reason)
		if reason == "" {
			reason = "no reason was given"
		}
		fmt.Fprintf(&b, "\n    %s: %s", file.path, reason)
	}
	return b.String()
}

// failureSummary is a loop's one-line summary of what did not land, "" for none
// or no result.
func (r *pipelinePushResult) failureSummary() string {
	if r == nil {
		return ""
	}
	return r.Error
}

func (r *pipelinePublishResult) failureSummary() string {
	if r == nil {
		return ""
	}
	return r.Error
}

func (r *automationPushResult) failureSummary() string {
	if r == nil {
		return ""
	}
	return r.Error
}

// errorLine is a result's --json error: the command's own error line, which
// ends with the failed-file list the result's one-line summary leaves out.
func errorLine(summary string, err error) string {
	if summary == "" || err == nil {
		return summary
	}
	return err.Error()
}

// checksFailureReason is the first thing that went wrong in a checks file: the
// whole file's error, else the first failing check's.
func checksFailureReason(r pipelineChecksResult) string {
	if r.Error != "" {
		return r.Error
	}
	for _, c := range r.Checks {
		if checksOutcomeFails(c.Outcome) {
			if c.Error != "" {
				return c.Name + ": " + c.Error
			}
			return c.Name + ": " + c.Outcome
		}
	}
	return r.Outcome
}
