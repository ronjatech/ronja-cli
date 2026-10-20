package commands

import (
	"fmt"
	"io"

	"github.com/ronjatech/ronja-cli/internal/api"
)

// findingTier is the one classification every renderer that counts or labels
// a validate finding uses, so none can disagree. printFindings is not one: it
// prints the server's own word and sorts by IsError().
type findingTier int

const (
	tierError findingTier = iota
	tierWarning
	tierNote
)

// classifyFinding counts an empty or unknown severity as a warning, never as
// nothing, or the CLI would print "Clean" over a finding it did not understand.
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

func (c findingCounts) notesSuffix() string {
	if c.notes == 0 {
		return ""
	}
	return fmt.Sprintf(", %d %s", c.notes, plural(c.notes, "note"))
}

// printPushFindingLines runs only after validate cleared the push, so no
// finding here is an error.
func printPushFindingLines(out io.Writer, findings []api.ValidateFinding) {
	for _, finding := range findings {
		label := "Warning"
		if classifyFinding(finding) == tierNote {
			label = "Note"
		}
		fmt.Fprintf(out, "  %s: %s — %s\n", label, finding.Path, finding.Message)
	}
}
