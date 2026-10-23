package markers

import (
	"regexp"
)

// The `{{ write(...) }}` marker, read on its own and never through Scan.
//
// The families table leaves it out on purpose (see the package doc): a bare
// non-id name there ALREADY means something — an output the run creates by
// that name — so treating every write argument as an alias would change what
// an existing folder deploys. What changes that is a DECLARATION: a dependency
// marked `"write": true` (somebody else's table this folder writes) or
// `"output": true` (the folder's own output, by its logical name) says that one
// name is to be resolved, and the alias codec rewrites exactly those names
// through this pass and nothing else.

// writePattern is backend/engine/pymarkers/markers.go writeMarkerPattern, byte
// for byte (TestWritePatternParity).
var writePattern = regexp.MustCompile(`\{\{+\s*write\(\s*['"]([^'"]+)['"]\s*\)\s*\}\}+`)

// WriteFamily is the family an Occurrence from ScanWrites carries.
var WriteFamily = Family{Verb: "write", Kind: KindTable, Args: 1}

// ScanWrites reports every `{{ write(...) }}` marker in content, in order.
// Positional is always false: a write's argument is a name or an id.
func ScanWrites(content string) []Occurrence {
	var out []Occurrence
	for _, span := range writePattern.FindAllStringSubmatchIndex(content, -1) {
		if len(span) < 4 || span[2] < 0 {
			continue
		}
		out = append(out, Occurrence{
			Family: WriteFamily, Arg: content[span[2]:span[3]], Marker: content[span[0]:span[1]],
			argStart: span[2], argEnd: span[3], matchStart: span[0], matchEnd: span[1],
		})
	}
	return out
}

// RewriteWrites is Rewrite for the write markers: sub answers each argument's
// replacement, and returning it unchanged is a byte-for-byte no-op.
func RewriteWrites(content string, sub func(Occurrence) string) (string, error) {
	return splice(content, ScanWrites(content), func(o Occurrence) (string, error) { return sub(o), nil })
}
