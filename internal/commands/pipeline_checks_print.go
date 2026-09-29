package commands

import (
	"fmt"
	"os"
	"strings"
)

// Rendering a checks pass for a person: the per-file block push and publish print.

func quoteNames(names []string) string {
	quoted := make([]string, 0, len(names))
	for _, n := range names {
		quoted = append(quoted, fmt.Sprintf("%q", n))
	}
	return strings.Join(quoted, ", ")
}

// printPipelineChecksResult renders one checks file's outcome.
func printPipelineChecksResult(out *os.File, r pipelineChecksResult) {
	switch r.Outcome {
	case checksOutcomePendingPublish:
		fmt.Fprintf(out, "\n  %s — pending publish\n    %s\n", r.Path, r.Error)
		return
	case "":
		fmt.Fprintf(out, "\n  %s%s — %s\n", r.Path, deletedMark(r.FileDeleted), describeChecksCounts(r.Checks))
	default:
		fmt.Fprintf(out, "\n  %s%s — %s\n", r.Path, deletedMark(r.FileDeleted), strings.ReplaceAll(r.Outcome, "_", " "))
		if r.Error != "" {
			fmt.Fprintf(out, "    %s\n", r.Error)
		}
	}
	if r.TableID != "" {
		fmt.Fprintf(out, "    Table:  %s\n", r.TableID)
	}
	for _, c := range r.Checks {
		switch c.Outcome {
		case checksOutcomeUnchanged, checksOutcomeAdopted:
			continue
		}
		line := fmt.Sprintf("    %s: %s", c.Outcome, c.Name)
		if len(c.Changed) > 0 {
			line += " (" + strings.Join(c.Changed, ", ") + ")"
		}
		switch c.Verdict {
		case "pass":
			line += " — pass"
		case "fail":
			line += " — FAIL"
		}
		fmt.Fprintln(out, line)
		for _, extra := range []string{c.Detail, c.Error} {
			if extra != "" {
				fmt.Fprintf(out, "      %s\n", extra)
			}
		}
	}
	if n := len(r.Unmanaged); n > 0 {
		fmt.Fprintf(out, "    %d %s on this table %s not in %s and %s left alone: %s\n",
			n, plural(n, "check"), pluralVerb(n, "is", "are"), r.Path, pluralVerb(n, "was", "were"), quoteNames(r.Unmanaged))
	}
}

func deletedMark(deleted bool) string {
	if deleted {
		return " (deleted)"
	}
	return ""
}

func pluralVerb(n int, one, many string) string {
	if n == 1 {
		return one
	}
	return many
}

func describeChecksCounts(checks []pipelineCheckResult) string {
	counts := map[string]int{}
	for _, c := range checks {
		counts[c.Outcome]++
	}
	var parts []string
	for _, outcome := range []string{checksOutcomeApplied, checksOutcomeAdoptedChanged, checksOutcomeAdopted, checksOutcomeUnchanged,
		checksOutcomeSilenced, checksOutcomePendingPublish, checksOutcomeRefused, checksOutcomeNotApplied, checksOutcomeUnsupported} {
		if counts[outcome] > 0 {
			parts = append(parts, fmt.Sprintf("%d %s", counts[outcome], strings.ReplaceAll(outcome, "_", " ")))
		}
	}
	if len(parts) == 0 {
		return "no checks"
	}
	return strings.Join(parts, ", ")
}
