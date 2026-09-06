package tabledocs

import (
	"fmt"
	"strings"
)

// The header's two directives. They are spelled with an `@` because a leading
// comment block is ordinary SQL commentary in almost every folder that exists,
// and the ONE thing this feature must not do is start pushing somebody's
// "-- joins orders to customers" note into their table's description.
const (
	tableDirective  = "@table"
	columnDirective = "@column"
)

// HeaderPrefix is the exact opening a .sql file must carry to opt in, quoted in
// help text and in the message a malformed header earns so the two cannot drift.
const HeaderPrefix = "-- " + tableDirective

// ParseHeader reads the OPT-IN documentation header of a pipeline .sql file:
// what the table is, and what its columns are.
//
//	-- @table One row per invoice line, from Fortnox, refreshed nightly.
//	-- Amounts are in SEK; a credit note carries a negative quantity.
//	-- @column invoice_no: The supplier's own invoice number, not Ronja's id.
//	-- @column amount: Line total excluding VAT.
//	SELECT ...
//
// OPT-IN IS THE WHOLE DESIGN. The block is read only when the FIRST comment
// line of the file opens with `-- @table`; a leading comment that does not is
// ordinary commentary and this function returns nothing at all, having judged
// nothing. Without that rule every pipeline folder in existence would start
// pushing its first comment as a table description on the next CLI release,
// silently, over prose an admin may have written by hand.
//
// The grammar, in full:
//
//   - Leading blank lines are skipped. The block is the run of `--` comment
//     lines that follows, and it ends at the first line that is not one.
//   - Text after `-- @table` is the table description. A plain `-- text` line
//     CONTINUES whatever was last opened, joined with a single space, so a
//     description or a column note may run over several lines.
//   - `-- @column <name>: <text>` opens a column. Everything after the FIRST
//     colon is the text, so a description may contain colons.
//   - A BARE `--` line ends the documentation. Anything after it in the same
//     comment block is ordinary commentary and is neither read nor reported —
//     which is how an author writes a normal note under a documented header.
//
// Warnings, never errors. A malformed line is dropped and named; nothing about a
// comment should be able to stop a push of SQL that is perfectly good, and a
// refusal here would make adopting the header a risk rather than an addition.
// Callers print them beside the file.
//
// ⚠️ The header STAYS IN `code`. It is sent to the server as part of the SQL
// and hashes as ordinary code, which is what makes an edit to a description
// show up as a changed file in `status` and get picked up by a bare push. The
// drift guard canonicalizes `{{ ref }}` markers and nothing else, so a comment
// is bytes like any other bytes.
func ParseHeader(sql string) (Docs, []string) {
	lines := strings.Split(sql, "\n")
	i := 0
	for i < len(lines) && strings.TrimSpace(lines[i]) == "" {
		i++
	}
	payload, isComment := commentPayload(lines, i)
	if !isComment {
		return Docs{}, nil
	}
	rest, opened := cutDirective(payload, tableDirective)
	if !opened {
		// The opt-out, and the ordinary case: a file whose first comment is a
		// note to a colleague. Nothing is read and nothing is said about it.
		return Docs{}, nil
	}

	var (
		docs     Docs
		warnings []string
		parts    []string
		target   string // "" for the table description, else a column name
		open     = true
	)
	// flush stores whatever has been accumulated against the currently open
	// target. Called at every directive and once at the end, so a target is
	// written exactly once however many continuation lines fed it.
	flush := func() {
		if !open {
			return
		}
		text := strings.TrimSpace(strings.Join(parts, " "))
		parts = parts[:0]
		if target == "" {
			if text == "" {
				// `-- @table` with nothing after it, and no continuation. Opting
				// in is not the same as declaring a description, and treating an
				// empty one as a declaration would CLEAR whatever the row holds —
				// a destructive reading of a line that looks like a header.
				warnings = append(warnings, fmt.Sprintf(
					"`%s` declares no text, so this file's table description is left alone — write the description after it, or on the `-- ` lines below it",
					HeaderPrefix))
				return
			}
			docs.Description = &text
			return
		}
		if text == "" {
			warnings = append(warnings, fmt.Sprintf(
				"`-- %s %s:` has no description after the colon, so that column is left alone — write the text after the colon, or drop the line",
				columnDirective, target))
			return
		}
		if _, already := docs.Columns[target]; already {
			warnings = append(warnings, fmt.Sprintf(
				"column %q is documented more than once in this file's header; the last line wins", target))
		}
		if docs.Columns == nil {
			docs.Columns = map[string]string{}
		}
		docs.Columns[target] = text
	}

	if rest != "" {
		parts = append(parts, rest)
	}
	for i++; i < len(lines); i++ {
		payload, isComment := commentPayload(lines, i)
		if !isComment {
			break
		}
		switch {
		case payload == "":
			// The explicit end of the documentation, and it ends the WHOLE block
			// rather than one target: everything below is the author's own
			// commentary — a directive included — and reading any of it would be
			// a header swallowing a note that has nothing to do with it.
			flush()
			return docs, warnings
		case strings.HasPrefix(payload, "@"):
			flush()
			open = true
			switch {
			case isDirective(payload, columnDirective):
				rest, _ := cutDirective(payload, columnDirective)
				name, text, ok := strings.Cut(rest, ":")
				name = strings.TrimSpace(name)
				switch {
				case !ok:
					warnings = append(warnings, fmt.Sprintf(
						"`-- %s %s` has no colon, so it documents nothing — write `-- %s <name>: <what the column holds>`",
						columnDirective, rest, columnDirective))
					open = false
				case name == "":
					warnings = append(warnings, fmt.Sprintf(
						"`-- %s%s` names no column, so it documents nothing — write `-- %s <name>: <what the column holds>`",
						columnDirective, payloadTail(payload, columnDirective), columnDirective))
					open = false
				default:
					target = name
					if text = strings.TrimSpace(text); text != "" {
						parts = append(parts, text)
					}
				}
			case isDirective(payload, tableDirective):
				warnings = append(warnings, fmt.Sprintf(
					"`%s` appears more than once in this file's header; only the first one is read", HeaderPrefix))
				open = false
			default:
				// An unknown directive is a TYPO far more often than it is a
				// deliberate `@`-opening sentence, and a typo silently swallowed is
				// a column that quietly stops being documented.
				warnings = append(warnings, fmt.Sprintf(
					"`-- %s` is not a documentation directive, so this line and any that continue it are ignored — the two are `%s` and `-- %s <name>: <text>`",
					payload, HeaderPrefix, columnDirective))
				open = false
			}
		case open:
			parts = append(parts, payload)
		}
	}
	flush()
	return docs, warnings
}

// commentPayload returns the text of a `--` comment line with the marker and the
// surrounding whitespace removed, and whether the line was a comment at all.
//
// Indentation is tolerated: a header written inside an indented block is still a
// header, and refusing it would be a rule about whitespace in a file the server
// never sees the whitespace of.
func commentPayload(lines []string, i int) (string, bool) {
	if i >= len(lines) {
		return "", false
	}
	line := strings.TrimSpace(strings.TrimSuffix(lines[i], "\r"))
	if !strings.HasPrefix(line, "--") {
		return "", false
	}
	return strings.TrimSpace(strings.TrimPrefix(line, "--")), true
}

// isDirective reports whether a comment payload opens with exactly this
// directive — the word itself, or the word followed by a space. `@tablespace`
// is not `@table`, and matching on the prefix alone would read it as one.
func isDirective(payload, directive string) bool {
	return payload == directive || strings.HasPrefix(payload, directive+" ") ||
		strings.HasPrefix(payload, directive+"\t")
}

// cutDirective removes the directive from a payload that opens with it,
// returning the trimmed remainder.
func cutDirective(payload, directive string) (string, bool) {
	if !isDirective(payload, directive) {
		return "", false
	}
	return strings.TrimSpace(strings.TrimPrefix(payload, directive)), true
}

// payloadTail is the raw text after a directive, for a message that has to quote
// the line back as the author wrote it.
func payloadTail(payload, directive string) string {
	return strings.TrimPrefix(payload, directive)
}
