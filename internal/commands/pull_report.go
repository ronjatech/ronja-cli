package commands

import (
	"fmt"
	"os"
	"strings"
	"unicode"

	"github.com/ronjatech/ronja-cli/internal/wfdir"
)

// The REPORT of `ronja <kind> pull`: the --json shape's diff and the human
// rendering, which escapes what it prints from the server. Split from pull.go.

func (r *pullReport) fill(paths []string, before, after map[string]*string) {
	var diff strings.Builder
	for _, p := range paths {
		d := unifiedDiff(p, before[p], after[p])
		if d == "" {
			continue
		}
		change := "modified"
		switch {
		case before[p] == nil:
			change = "added"
		case after[p] == nil:
			change = "deleted"
		}
		r.Files = append(r.Files, pullFileReport{Path: p, Change: change})
		diff.WriteString(d)
	}
	r.Diff = diff.String()
}

// escapeTerminal ESCAPES every C0 and C1 control character but tab and newline
// (`\x1b`, `\r`, `\x9b`) and the bidi and zero-width characters
// context_policy.go's terminalSafe strips (`\u202e`), for text printed to a
// terminal from somewhere the author does not control — and escapes rather
// than strips, because the human pull report prints the LIVE row's bytes as
// the diff an author reviews before committing: an escape sequence in them
// (ESC[1A ESC[2K erases the line above, a bare CR overwrites one) could hide a
// line the pull writes, and a dropped one would hide that the bytes are there.
// --json needs none of it: encoding/json escapes control characters already.
func escapeTerminal(s string) string {
	if !strings.ContainsFunc(s, unsafeOnTerminal) {
		return s
	}
	var b strings.Builder
	for _, r := range s {
		switch {
		case r == '\r':
			b.WriteString(`\r`)
		case !unsafeOnTerminal(r):
			b.WriteRune(r)
		case r <= 0xff:
			fmt.Fprintf(&b, `\x%02x`, r)
		default:
			fmt.Fprintf(&b, `\u%04x`, r)
		}
	}
	return b.String()
}

func unsafeOnTerminal(r rune) bool {
	switch {
	case r == '\n' || r == '\t':
		return false
	case unicode.IsControl(r):
		return true
	}
	return r >= 0x202A && r <= 0x202E || r >= 0x2066 && r <= 0x2069 || r >= 0x200B && r <= 0x200D || r == 0xFEFF
}

func printPullReport(kind wfdir.Kind, r *pullReport) {
	out := os.Stdout
	if r.Diff != "" {
		fmt.Fprint(out, escapeTerminal(r.Diff))
		fmt.Fprintln(out)
	}
	switch {
	case len(r.Files) == 0 && len(r.Kept) > 0:
		fmt.Fprintf(out, "  Nothing to pull: the live %s on stack %q has not changed since this folder last applied it.\n", kind.Label, r.Stack)
	case len(r.Files) == 0:
		fmt.Fprintf(out, "  Nothing to pull: this folder already holds the live %s on stack %q.\n", kind.Label, r.Stack)
	case r.DryRun:
		fmt.Fprintf(out, "  Dry run — nothing was written. A pull would change %d %s to what is live.\n", len(r.Files), plural(len(r.Files), "file"))
	default:
		fmt.Fprintf(out, "  Rewrote %d %s with the live %s on stack %q.\n", len(r.Files), plural(len(r.Files), "file"), kind.Label, r.Stack)
	}
	if len(r.Replaced) > 0 {
		verb := "Replaced with what is live (--force)"
		if r.DryRun {
			verb = "A pull would replace these with what is live, and refuses to without --force"
		}
		fmt.Fprintf(out, "\n  %s — this folder's own changes, never deployed:\n", verb)
		for _, p := range r.Replaced {
			fmt.Fprintf(out, "    %s\n", p)
		}
		if !r.DryRun {
			fmt.Fprintf(out, "  The diff above shows what they were; they are not in this folder any more.\n")
		}
	}
	if len(r.Kept) > 0 {
		fmt.Fprintf(out, "\n  Left as they are — this folder's own changes, not deployed yet; the next apply deploys them:\n")
		for _, p := range r.Kept {
			fmt.Fprintf(out, "    %s\n", p)
		}
	}
	if len(r.Literals) > 0 {
		fmt.Fprintf(out, "\n  Written as literal ids — no alias in this folder names them, and pull adds none:\n")
		for _, l := range r.Literals {
			fmt.Fprintf(out, "    %s\n", l.describe())
		}
		fmt.Fprintf(out, "  Declare a dependency and bind it per stack (`ronja bind`) if another stack should reach a different row.\n")
	}
	for _, n := range r.Notes {
		fmt.Fprintf(out, "\n  Note: %s\n", escapeTerminal(n))
	}
	if len(r.Pending) > 0 {
		fmt.Fprintf(out, "\n  The next deploy still writes:\n")
		for _, p := range r.Pending {
			fmt.Fprintf(out, "    %s\n", escapeTerminal(p))
		}
	}
	if r.Anchored {
		next := "the next apply writes nothing for it"
		if len(r.Pending) > 0 {
			next = "the next apply writes only what is listed above"
		}
		fmt.Fprintf(out, "\n  Recorded on stack %q as what this folder last applied. Review the diff and commit it; %s.\n", r.Stack, next)
	}
}
