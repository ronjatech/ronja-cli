package tabledocs

import "strings"

// RewriteHeader returns sql with the VALUES its opt-in header already declares
// replaced — the inverse of ParseHeader and ParseName for `ronja pipeline
// pull`, which writes what a live table now says back into the file:
//
//   - the first `-- @name`'s name, when name is non-nil;
//   - the `-- @table` description, when docs.Description is non-nil;
//   - each `-- @column <c>:` text, for a c in docs.Columns.
//
// It never ADDS a declaration: a key the header leaves out is a field the file
// does not manage, and rewriting it in would start managing it. A replaced
// value is written on its directive's own line and its continuation lines are
// dropped; a value holding a line break continues on `-- ` lines, as an author
// would write it. A column whose new text is empty is removed — `-- @column c:`
// with nothing after the colon documents nothing (ParseHeader warns and leaves
// the column alone), which is what an emptied column means. Everything else is
// kept byte for byte: the SQL, the indentation, the commentary below a bare
// `--`, and every directive whose value did not change.
//
// A file with no opt-in header comes back unchanged.
func RewriteHeader(sql string, name *string, docs Docs) string {
	lines := strings.Split(sql, "\n")
	i := 0
	for i < len(lines) && strings.TrimSpace(lines[i]) == "" {
		i++
	}
	payload, isComment := commentPayload(lines, i)
	if !isComment || !(isDirective(payload, nameDirective) || isDirective(payload, tableDirective)) {
		return sql
	}

	out := append([]string{}, lines[:i]...)
	// The documentation is read only when the first non-@name line opens with
	// `-- @table` — exactly ParseHeader's rule — so column and table values are
	// replaced only inside such a header.
	documented := false
	for j := i; j < len(lines); j++ {
		p, c := commentPayload(lines, j)
		if !c {
			break
		}
		if isDirective(p, nameDirective) {
			continue
		}
		documented = isDirective(p, tableDirective)
		break
	}

	nameDone, tableSeen := false, false
	// dropping is set while the lines below a replaced value are its old
	// continuation, which the new value replaces.
	dropping := false
	j := i
	for ; j < len(lines); j++ {
		line := lines[j]
		p, c := commentPayload(lines, j)
		if !c {
			break
		}
		indent := line[:strings.Index(line, "--")]
		switch {
		case p == "":
			// The end of the header: the rest is the author's commentary.
			out = append(out, lines[j:]...)
			return strings.Join(out, "\n")
		case isDirective(p, nameDirective):
			dropping = false
			if name != nil && !nameDone {
				out = append(out, directiveLines(indent, nameDirective, "", *name)...)
				nameDone = true
				continue
			}
			nameDone = true
		case documented && isDirective(p, tableDirective) && !tableSeen:
			tableSeen = true
			dropping = false
			if docs.Description != nil {
				out = append(out, directiveLines(indent, tableDirective, "", *docs.Description)...)
				dropping = true
				continue
			}
		case documented && isDirective(p, columnDirective):
			dropping = false
			rest, _ := cutDirective(p, columnDirective)
			col, _, ok := strings.Cut(rest, ":")
			col = strings.TrimSpace(col)
			if text, replace := docs.Columns[col]; ok && col != "" && replace {
				if text != "" {
					out = append(out, directiveLines(indent, columnDirective, col+":", text)...)
				}
				dropping = true
				continue
			}
		case strings.HasPrefix(p, "@"):
			dropping = false
		case dropping:
			continue
		}
		out = append(out, line)
	}
	out = append(out, lines[j:]...)
	return strings.Join(out, "\n")
}

// directiveLines writes one directive and its value, continuing a multi-line
// value on `-- ` lines.
func directiveLines(indent, directive, label, value string) []string {
	parts := strings.Split(strings.ReplaceAll(value, "\r\n", "\n"), "\n")
	head := indent + "-- " + directive
	if label != "" {
		head += " " + label
	}
	if first := strings.TrimSpace(parts[0]); first != "" {
		head += " " + first
	}
	out := []string{head}
	for _, more := range parts[1:] {
		if more = strings.TrimSpace(more); more != "" {
			out = append(out, indent+"-- "+more)
		}
	}
	return out
}
