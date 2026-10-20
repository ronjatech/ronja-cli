// Package tablerefs mirrors backend/engine/esql's `{{ ref('…') }}` grammar for
// a derived table's SQL: what the code references, and its canonical form.
//
// Stored code carries either id refs (`{{ ref('table-abc') }}`) or positional
// ones (`{{ ref('0') }}`, an index into input_models, common on older rows and
// still accepted by the write API). A sync loop hashes remote code for drift,
// so both forms must canonicalize to one or the same SQL hashes two ways.
package tablerefs

import (
	"fmt"
	"regexp"
	"sort"
	"strconv"
	"strings"
)

// ⚠️ Both patterns are copied from backend/engine/esql/esql.go and must stay in
// step: a looser one rewrites text the server leaves alone, a stricter one
// leaves a positional ref the server re-reads against a different
// input_models. The `+` on each brace group is deliberate, matching the extra
// braces an f-string escape produces (`{{{{ ref('x') }}}}`).
var (
	indexRefPattern = regexp.MustCompile(`\{\{+\s*ref\(\s*['"](\d+)['"]\s*\)\s*\}\}+`)
	// idRefPattern also matches a positional ref, so an ids-only caller
	// filters with IsTableID.
	//
	// ⚠️ The quote alternation (a handle may double-quote a segment,
	// `Sales."v2.1 forecast"`) makes two capture groups with one participating:
	// read the argument with refArg, never by index.
	idRefPattern = regexp.MustCompile(`\{\{+\s*ref\(\s*(?:'([^']+)'|"([^"]+)")\s*\)\s*\}\}+`)
)

// The table id prefixes, mirroring rrn.IsTable; "modelv2-" is no longer minted
// but existing rows still carry it.
const (
	tableIDPrefix       = "table-"
	legacyTableIDPrefix = "modelv2-"
)

// IsTableID reports whether an id names a table row: the predicate esql applies
// before substituting a positional ref, so a corrupt input_models entry is never
// fabricated into a ref.
func IsTableID(id string) bool {
	return strings.HasPrefix(id, tableIDPrefix) || strings.HasPrefix(id, legacyTableIDPrefix)
}

// Canonicalize rewrites positional `{{ ref('N') }}` markers to
// `{{ ref('<inputModels[N]>') }}` and reports whether any could not be resolved.
// It mirrors esql.IndexToTableIDRefs byte for byte: an out-of-range index or a
// non-table target is left unchanged and flagged, and id-form code is a no-op.
// tablerefs_test.go carries esql.IndexToTableIDRefs's own cases; change both
// together.
//
// The bool is informational: the result is still what to hash and display, but
// a surviving positional ref will be re-resolved by the server against whatever
// input_models the next write declares.
func Canonicalize(code string, inputModels []string) (string, bool) {
	unresolved := false
	out := indexRefPattern.ReplaceAllStringFunc(code, func(match string) string {
		sub := indexRefPattern.FindStringSubmatch(match)
		if len(sub) < 2 {
			return match
		}
		idx, err := strconv.Atoi(sub[1])
		if err != nil {
			// Reached only by an integer overflow on a long run of digits.
			unresolved = true
			return match
		}
		if idx < 0 || idx >= len(inputModels) {
			unresolved = true
			return match
		}
		id := inputModels[idx]
		if !IsTableID(id) {
			unresolved = true
			return match
		}
		return fmt.Sprintf("{{ ref('%s') }}", id)
	})
	return out, unresolved
}

// PositionalRefs is every positional `{{ ref('N') }}` marker in some code,
// verbatim, in order and de-duplicated, so a refusal can name what it refused.
// It shares Canonicalize's regex so the two agree on what counts as positional.
func PositionalRefs(code string) []string {
	matches := indexRefPattern.FindAllString(code, -1)
	seen := make(map[string]struct{}, len(matches))
	var out []string
	for _, m := range matches {
		if _, dup := seen[m]; dup {
			continue
		}
		seen[m] = struct{}{}
		out = append(out, m)
	}
	return out
}

// DeriveInputModels is every table this code reads from: its id-shaped refs,
// de-duplicated and sorted (so reordering the SQL is not a lineage change).
// Refs that are not table ids are dropped, never turned into an input.
//
// ⚠️ Send it on every write: the server derives input_models
// (resolveDerivedRefs) only when the declared list is empty, and a checked-out
// draft inherits its parent's, so omitting it 400s when an edit adds a ref and
// leaves stale lineage edges when one is removed.
func DeriveInputModels(code string) []string {
	matches := idRefPattern.FindAllStringSubmatch(code, -1)
	seen := make(map[string]struct{}, len(matches))
	var ids []string
	for _, m := range matches {
		id := refArg(m)
		if id == "" {
			continue
		}
		if !IsTableID(id) {
			continue
		}
		if _, dup := seen[id]; dup {
			continue
		}
		seen[id] = struct{}{}
		ids = append(ids, id)
	}
	sort.Strings(ids)
	return ids
}

func refArg(sub []string) string {
	if len(sub) < 3 {
		return ""
	}
	if sub[1] != "" {
		return sub[1]
	}
	return sub[2]
}
