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
//     comment block is ordinary commentary and is not read — which is how an
//     author writes a normal note under a documented header. A `@table` or
//     `@column` down there is still not read, but it IS reported, once, with a
//     count: a documented name that goes nowhere on a push that reports success
//     is the one drop nobody can see.
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
			//
			// Not read, but NAMED: a `@column` below the line is the one shape
			// where a documented name is lost on a push that reports success.
			flush()
			return docs, append(warnings, orphanedDirectives(lines, i+1)...)
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

// orphanedDirectives counts the documentation directives written BELOW a bare
// `--` and returns 0 or 1 warning naming how many there are. They are still not
// READ — the terminator is deliberate — but a `@column` that is silently not
// read is the one shape where a documented name is lost on a push that reports
// success.
//
// READ-ONLY over `lines`, and that is the property to check: it never touches
// docs, warnings, open, target or parts, so the directive state machine is
// structurally unreachable from here and this cannot write documentation. The
// count is the whole payload for the same reason — naming the columns would
// mean parsing `@column <name>:` below the terminator, which is exactly the
// boundary this keeps sharp. The caller already names the file.
//
// ONE warning, however many orphans: the push prints a stderr line AND appends
// a `notes[]` entry per warning, so a warning each would be 26 of both for a
// single mistake.
//
// It scans to the end of the comment RUN. A second bare `--` is more of the
// same commentary and the scan WALKS PAST IT — the reported shape was
// directives under bare `--` paragraph separators, PLURAL, and a scan that
// stopped at the second would name the first group, earn a clean push and lose
// the rest, which is worse than saying nothing. The first non-comment line — a
// BLANK one included — stops it, because that is where ParseHeader's own block
// ends; a directive in a later comment block was unreachable before the bare
// `--` existed, and blaming the terminator for it would print a false
// explanation.
//
// The trigger is `@table` and `@column` only, matched with isDirective so
// `@tablespace` is not `@table`. The region below the terminator is DEFINED as
// the author's own commentary, so an `-- @see ticket 41` convention there is
// legitimate — and a warning that fires on legitimate prose is one people learn
// to mute. The accepted cost is that a typo'd directive down there stays silent.
func orphanedDirectives(lines []string, from int) []string {
	count := 0
	for i := from; i < len(lines); i++ {
		payload, isComment := commentPayload(lines, i)
		if !isComment {
			break
		}
		if isDirective(payload, columnDirective) || isDirective(payload, tableDirective) {
			count++
		}
	}
	if count == 0 {
		return nil
	}
	// Five values, and the assignment is ORDER-SENSITIVE in a way no compiler
	// check reaches — `go vet` counts the `fmt` arguments and nothing reads what
	// is in them. Swapping the last two here ships "drop the `@` if a note it is
	// rather than documentation", green. TestParseHeaderOrphanRemedyIsNonDestructive
	// asserts the whole rendered string for BOTH branches for that reason.
	directives, are, them, theyAre, notes := "directives", "are", "them", "they are", "notes"
	if count == 1 {
		directives, are, them, theyAre, notes = "directive", "is", "it", "it is", "a note"
	}
	// The remedy must never be "delete the bare `--`": that does not just
	// activate the directives, it turns every prose line between the terminator
	// and them into a CONTINUATION of the table description, so an author's TODO
	// silently becomes the customer-visible prose on the table. Both branches
	// here are non-destructive, and dropping the `@` is also the way to park a
	// directive block without this warning.
	return []string{fmt.Sprintf(
		"a bare `--` ends the header: %d documentation %s below it %s not read, so that prose is left alone — move %s above the `--` line, or drop the `@` if %s %s rather than documentation",
		count, directives, are, them, theyAre, notes)}
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
