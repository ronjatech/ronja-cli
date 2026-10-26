package commands

import (
	"fmt"
	"io"
	"strings"
	"time"

	"github.com/ronjatech/ronja-cli/internal/api"
)

// queryHealthTextRunes bounds each name and reason on a warning line, so one
// table cannot flood the stderr a coding agent reads.
const queryHealthTextRunes = 200

// printQueryTableHealth prints one line per table naming its largest fact, as
// chat does: its own failing build, else its broken upstream (a table that is
// not updating at all makes its inputs moot). Names and reasons are user- or
// connector-authored text bound for a terminal and an agent, so each is held to
// one bounded line.
func printQueryTableHealth(w io.Writer, entries []api.QueryTableHealth) {
	for _, e := range entries {
		table := "table " + healthText(e.ID)
		if name := healthText(e.Name); name != "" {
			table += " (" + name + ")"
		}
		if f := e.BuildFailing; f != nil {
			line := table + " has failed to update"
			if f.Since != nil {
				line += " since " + healthTime(*f.Since)
			}
			if reason := healthText(f.Reason); reason != "" {
				line += " (" + reason + ")"
			}
			fmt.Fprintf(w, "  Warning: %s; the result is its last good data.\n", line)
		} else if len(e.Upstream) > 0 {
			fmt.Fprintf(w, "  Warning: %s.\n", upstreamWarning(table, e))
		}
	}
}

// upstreamWarning names the oldest upstream problem (usually the root cause)
// and counts the rest, one per problem as the server lists them.
func upstreamWarning(table string, e api.QueryTableHealth) string {
	is := e.Upstream[0]
	reason := healthText(is.Reason)
	var b strings.Builder
	b.WriteString(table)
	switch {
	case is.Readable != nil && !*is.Readable:
		if clause := unreadableInputClause(is.Problem); clause != "" {
			fmt.Fprintf(&b, " is built on an input you cannot open, which %s", clause)
		} else {
			fmt.Fprintf(&b, ": %s", reason)
		}
	case is.ID == e.ID:
		// The table's own connection counts as its upstream.
		fmt.Fprintf(&b, ": %s", reason)
	default:
		fmt.Fprintf(&b, " is built on %s", healthText(is.ID))
		if name := healthText(is.Name); name != "" {
			fmt.Fprintf(&b, " (%s)", name)
		}
		fmt.Fprintf(&b, ", where %s", reason)
	}
	switch {
	case is.Since != nil:
		fmt.Fprintf(&b, " since %s", healthTime(*is.Since))
	case is.LastBuiltAt != nil:
		fmt.Fprintf(&b, "; its data is from %s", healthTime(*is.LastBuiltAt))
	}
	if rest := len(e.Upstream) - 1 + e.UpstreamMore; rest > 0 {
		fmt.Fprintf(&b, " (and %d more upstream %s)", rest, plural(rest, "problem"))
	}
	return b.String()
}

// unreadableInputClause is rtablecheck.UnreadableInputClause, which this
// module cannot import. Empty for a problem a newer server adds: the caller
// then prints the server's own reason.
func unreadableInputClause(problem string) string {
	switch problem {
	case "build_failing":
		return "has a failing build"
	case "check_failing":
		return "has a failing health check"
	case "connection_broken":
		return "has a broken connection"
	}
	return ""
}

// healthText collapses newlines, tabs and runs of spaces to one space, drops
// the characters terminalSafe drops, and bounds the result.
func healthText(s string) string {
	s = strings.Join(strings.Fields(terminalSafe(s)), " ")
	if r := []rune(s); len(r) > queryHealthTextRunes {
		s = strings.TrimSpace(string(r[:queryHealthTextRunes])) + "…"
	}
	return s
}

func healthTime(t time.Time) string {
	return t.UTC().Format(time.RFC3339)
}
