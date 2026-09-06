package commands

import (
	"fmt"
	"regexp"
	"slices"
	"strings"
)

// The local half of the ONE content rule a module save has: module files carry
// no Ronja markers at all.
//
// ⚠️ PORTED, not imported, and it has to stay in step by hand. The CLI is a
// separate Go module and deliberately does not depend on the backend one, so
// this is a copy of:
//
//	backend/engine/pymarkers/markers.go   FirstRonjaMarkerVerb, genericMarkerPattern,
//	                                      moduleFreeVerbs
//	backend/engine/pymarkers/extract.go   stripTripleQuotedBlocks, stripLineComments
//
// KEEP IN SYNC with those. A copy that is STRICTER than the server refuses a
// file the server would accept — which is a validate that lies about a push
// that would have worked. A copy that is LOOSER simply stops predicting one
// refusal, which is the safe direction and the one this file errs toward: when
// in doubt, leave it to the server, whose message `module push` passes through
// verbatim.
//
// ⚠️ NOT `internal/markers`, and the two answer different questions. That
// package knows the ALIAS-ELIGIBLE families by verb and scans raw source, so it
// can rewrite a marker's argument; this one asks "is there ANY Ronja marker in
// live Python here?" over a generic verb pattern, and it has to skip dead text
// (`#` comments, docstrings) because the server's refusal does. Folding this
// onto markers.Scan would either widen that package with a dead-text strip it
// does not want or narrow this rule to the families that happen to be
// alias-eligible — `{{ file }}` is not one of them. Keep them separate.
//
// Why the CLI carries it at all: `module validate` is local-only (a module has
// no markers to resolve, so there is no server pass to run), and the marker rule
// is the refusal an author walks straight into on the plan's own migration path
// — moving `tracker.py` out of a workflow folder, where it may well have carried
// `{{ ref }}` before somebody split the helper out. Finding that out from
// validate is a folder edit; finding it out from a push is a half-written draft.

// moduleMarkerProblem is the ONE wording of the marker refusal, shared by
// `module validate` (which reports every file) and `module push` (which refuses
// at the first). Two spellings of one rule is how a validate ends up explaining
// something different from the push it is meant to predict.
func moduleMarkerProblem(path, verb string) string {
	return fmt.Sprintf("%s carries a {{ %s(...) }} marker — modules are marker-free. A module runs inside the calling workflow's interpreter under that workflow's authority, so it declares no bindings of its own: put the marker in the consumer workflow and pass the result to this module's function as an argument",
		path, verb)
}

// refuseModuleMarkers is the PUSH-side half of the same rule: the first
// marker-carrying file, as an error, before anything is written.
//
// It stops at the first file rather than listing them all, because the caller is
// a push and a push refuses — the author who wants the full list runs `module
// validate`, which is named in the advice. Paths are walked in sorted order so
// the same folder always names the same file.
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

// moduleFreeVerbs is every Ronja marker family a module file is refused for.
// `ref` is in the list even though it lives in engine/esql rather than in
// pymarkers: the scan is verb-based, so covering it costs nothing and leaving it
// out would let the one read-side family through the one rule.
var moduleFreeVerbs = []string{
	"ref", "write", "secret", "agent", "file", "codex", "mailbox", "workflow", "module",
}

// moduleMarkerPattern matches `{{ <verb>('arg') }}` / `{{ <verb>('a', 'b') }}`
// for an arbitrary verb — pymarkers.genericMarkerPattern, character for
// character.
//
// It requires the verb's QUOTED arguments, which is what makes the boundary
// honest rather than total: a marker-shaped construct that could never bind
// anything (`{{ ref() }}`, `{{ secret(name) }}`, an unclosed `{{ agent('x'`) is
// not matched here and is not matched server-side either. Widening it would
// refuse ordinary Python that merely looks like a marker.
var moduleMarkerPattern = regexp.MustCompile(
	`\{\{+\s*([A-Za-z_][A-Za-z0-9_]*)\(\s*['"][^'"]+['"](?:\s*,\s*['"][^'"]+['"])?\s*\)\s*\}\}+`)

// firstRonjaMarkerVerb returns the verb of the first Ronja marker in LIVE Python
// source — `#` comments and triple-quoted blocks are skipped, exactly as the
// server's extraction skips them — or "" when there is none.
//
// A marker inside an ordinary string literal is deliberately NOT exempt: a
// module has no marker contract, so a marker-shaped string in one is an author
// expecting a substitution that will never happen.
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

// stripTripleQuotedBlocks blanks docstring bodies so a marker inside one is not
// matched. Newlines are preserved so nothing else shifts line for line.
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

// stripLineComments drops `#`-comment tails so a commented-out marker is not
// matched.
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
