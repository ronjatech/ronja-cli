package commands

import (
	"fmt"
	"io"

	"github.com/ronjatech/ronja-cli/internal/api"
)

// findingTier is how the renderers that COUNT or LABEL a workflow validate
// finding bucket it: the `wf validate` headline, countErrors' refusal line,
// push's per-finding stderr lines, and `sync check`'s error-versus-rest split.
// One classification, so none of those can disagree about what a finding is.
// printFindings is not among them: it labels each line with the server's own
// word and sorts by IsError().
type findingTier int

const (
	tierError findingTier = iota
	tierWarning
	tierNote
)

// classifyFinding buckets one finding by the server's own severity. `info` is
// a note; `error` is an error; EVERYTHING else — `warning`, an empty tier, a
// tier this build has never heard of — is a warning. The last case is the one
// that matters: a CLI that counted an unknown tier as nothing would print
// "Clean" over a finding it did not understand. printFindings reaches the same
// place by its own route — it prints the server's word verbatim, with an empty
// one as "warning" — rather than by calling this.
func classifyFinding(f api.ValidateFinding) findingTier {
	switch f.Severity {
	case api.SeverityError:
		return tierError
	case api.SeverityInfo:
		return tierNote
	default:
		return tierWarning
	}
}

// findingCounts is a finding list tallied by classifyFinding.
type findingCounts struct {
	errors, warnings, notes int
}

func countFindings(findings []api.ValidateFinding) findingCounts {
	var c findingCounts
	for _, f := range findings {
		switch classifyFinding(f) {
		case tierError:
			c.errors++
		case tierNote:
			c.notes++
		default:
			c.warnings++
		}
	}
	return c
}

// notesSuffix is ", N notes" when there are any — appended wherever notes
// coexist with a tier the headline is actually about.
func (c findingCounts) notesSuffix() string {
	if c.notes == 0 {
		return ""
	}
	return fmt.Sprintf(", %d %s", c.notes, plural(c.notes, "note"))
}

// printPushFindingLines is the one-line-per-finding form `wf push` prints on
// stderr once validate has cleared it — so none of these is an error, and the
// only question is which of the other two tiers to call it. An `info`
// advisory is a Note; labelling it "Warning" reported a note about a shape as
// a problem with it.
func printPushFindingLines(out io.Writer, findings []api.ValidateFinding) {
	for _, finding := range findings {
		label := "Warning"
		if classifyFinding(finding) == tierNote {
			label = "Note"
		}
		fmt.Fprintf(out, "  %s: %s — %s\n", label, finding.Path, finding.Message)
	}
}
