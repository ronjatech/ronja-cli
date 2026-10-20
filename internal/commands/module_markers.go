package commands

import (
	"fmt"
	"regexp"
	"slices"
	"strings"
)

// ⚠️ PORTED from backend/engine/pymarkers (markers.go FirstRonjaMarkerVerb,
// genericMarkerPattern, moduleFreeVerbs; extract.go stripTripleQuotedBlocks,
// stripLineComments) and kept in sync by hand: the CLI module cannot import the
// backend. Stricter than the server is a validate that lies; looser is the safe
// direction.
//
// ⚠️ Deliberately not `internal/markers`: that package scans raw source for the
// alias-eligible families, while this rule covers every family (`{{ file }}`
// included) in live Python only, skipping comments and docstrings as the server
// does.

// moduleMarkerProblem is the ONE wording of the marker refusal, shared by
// `module validate` and `module push`.
func moduleMarkerProblem(path, verb string) string {
	return fmt.Sprintf("%s carries a {{ %s(...) }} marker — modules are marker-free. A module runs inside the calling workflow's interpreter under that workflow's authority, so it declares no bindings of its own: put the marker in the consumer workflow and pass the result to this module's function as an argument",
		path, verb)
}

func refuseModuleMarkers(files map[string]string) error {
	for _, path := range sortedPaths(files) {
		verb := firstRonjaMarkerVerb(files[path])
		if verb == "" {
			continue
		}
		return fmt.Errorf("%s.\n  Nothing was pushed; `ronja module validate` lists every file that carries one",
			moduleMarkerProblem(path, verb))
	}
	return nil
}

var moduleFreeVerbs = []string{
	"ref", "write", "secret", "agent", "file", "codex", "mailbox", "workflow", "module",
}

// moduleMarkerPattern is pymarkers.genericMarkerPattern character for
// character. It requires quoted arguments, as the server does, so a construct
// that binds nothing (`{{ ref() }}`) is ordinary Python.
var moduleMarkerPattern = regexp.MustCompile(
	`\{\{+\s*([A-Za-z_][A-Za-z0-9_]*)\(\s*['"][^'"]+['"](?:\s*,\s*['"][^'"]+['"])?\s*\)\s*\}\}+`)

// firstRonjaMarkerVerb deliberately does not exempt a marker in an ordinary
// string literal: a module has no marker contract, so it is a substitution that
// will never happen.
func firstRonjaMarkerVerb(content string) string {
	stripped := stripLineComments(stripTripleQuotedBlocks(content))
	for _, m := range moduleMarkerPattern.FindAllStringSubmatch(stripped, -1) {
		if len(m) < 2 {
			continue
		}
		if slices.Contains(moduleFreeVerbs, m[1]) {
			return m[1]
		}
	}
	return ""
}

// stripTripleQuotedBlocks preserves newlines so nothing shifts line for line.
func stripTripleQuotedBlocks(s string) string {
	out := []byte(s)
	i := 0
	for i < len(out) {
		if i+3 <= len(out) && (matchAt(out, i, `"""`) || matchAt(out, i, `'''`)) {
			delim := string(out[i : i+3])
			j := i + 3
			for j+3 <= len(out) {
				if matchAt(out, j, delim) {
					break
				}
				if out[j] != '\n' {
					out[j] = ' '
				}
				j++
			}
			out[i], out[i+1], out[i+2] = ' ', ' ', ' '
			if j+3 <= len(out) {
				out[j], out[j+1], out[j+2] = ' ', ' ', ' '
				i = j + 3
			} else {
				i = len(out)
			}
			continue
		}
		i++
	}
	return string(out)
}

func matchAt(b []byte, i int, lit string) bool {
	if i+len(lit) > len(b) {
		return false
	}
	for k := 0; k < len(lit); k++ {
		if b[i+k] != lit[k] {
			return false
		}
	}
	return true
}

func stripLineComments(s string) string {
	var b strings.Builder
	b.Grow(len(s))
	for _, line := range strings.SplitAfter(s, "\n") {
		b.WriteString(stripOneLineComment(line))
	}
	return b.String()
}

func stripOneLineComment(line string) string {
	inSingle, inDouble := false, false
	for i := 0; i < len(line); i++ {
		c := line[i]
		switch {
		case c == '\\' && i+1 < len(line):
			i++
		case c == '\'' && !inDouble:
			inSingle = !inSingle
		case c == '"' && !inSingle:
			inDouble = !inDouble
		case c == '#' && !inSingle && !inDouble:
			tail := ""
			if strings.Contains(line[i:], "\n") {
				tail = "\n"
			}
			return line[:i] + tail
		}
	}
	return line
}
