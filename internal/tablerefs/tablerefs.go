// Package tablerefs owns the `{{ ref('…') }}` grammar a derived table's SQL is
// written in, and the two questions the CLI asks of it: what does this code
// reference, and what is its CANONICAL form.
//
// It exists as its own package, with no dependency on the API client, because
// it is a pure mirror of a server-side rule (backend/engine/esql) rather than
// anything about HTTP — and because both halves feed decisions that are made
// before any request: which files a push must order first, and which bytes a
// sync baseline is a fingerprint OF.
//
// # Why canonicalization is first-class
//
// A derived table's `code` is stored in one of two ref forms, and BOTH are
// current:
//
//   - ID form — `{{ ref('table-abc') }}`. What the write API stores verbatim,
//     and what a human or an HTTP caller writes.
//   - POSITIONAL form — `{{ ref('0') }}`, an index into the row's
//     `input_models`. What the AI build path persists: `POST /:id/build`,
//     `/fix` and the agent's own editDerivedTable all normalize to it.
//
// Positional is therefore the steady state of any table a colleague has touched
// through chat, not a legacy edge case. A folder-sync loop hashes remote code to
// decide whether the local file has drifted, so without a canonical form the
// same SQL would hash two ways and every table last edited in chat would read as
// permanently drifted.
//
// Canonicalize is the mirror of esql.IndexToTableIDRefs, byte-for-byte, and the
// fixture suite in tablerefs_test.go carries that function's own test cases so
// the two are checked against the same table. It is a no-op on id-form code, so
// a row written by this CLI round-trips exactly.
package tablerefs

import (
	"fmt"
	"regexp"
	"sort"
	"strconv"
	"strings"
)

// The two ref patterns, copied from backend/engine/esql/esql.go.
//
// The `+` on each brace group is deliberate and is NOT a typo: it makes the
// match tolerant of extra surrounding braces (`{{{{ ref('x') }}}}`), which an
// agent produces when it wraps a ref inside a Python f-string and reaches for
// brace escaping. Consuming the outer braces is what the server does, so a
// client that did not would disagree with it about what the code says.
//
// Keep both in step with esql: a pattern that drifts LOOSER here rewrites text
// the server would have left alone, and one that drifts STRICTER leaves a
// positional ref in a local file, where the next push sends it back and the
// server re-reads it against a DIFFERENT input_models — silently changing which
// table the SQL reads from.
var (
	// indexRefPattern matches a positional ref: {{ ref('0') }}.
	indexRefPattern = regexp.MustCompile(`\{\{+\s*ref\(\s*['"](\d+)['"]\s*\)\s*\}\}+`)
	// idRefPattern matches a ref carrying an arbitrary string, which is how an
	// id-form ref is found. It also matches a positional one — the digits are an
	// arbitrary string — so callers that mean "ids only" filter with IsTableID.
	idRefPattern = regexp.MustCompile(`\{\{+\s*ref\(\s*['"]([^'"]+)['"]\s*\)\s*\}\}+`)
)

// The id prefixes a table row can carry, mirroring rrn.IsTable
// (backend/lib/rrn/rrn.go). "modelv2-" is the legacy prefix; it is no longer
// minted but existing rows still carry it, so a client that only knew "table-"
// would refuse to canonicalize a perfectly ordinary old table.
const (
	tableIDPrefix       = "table-"
	legacyTableIDPrefix = "modelv2-"
)

// IsTableID reports whether an id names a table row.
//
// This is the same predicate esql applies before it substitutes a positional
// ref, and it is what keeps a corrupt `input_models` from being fabricated into
// a plausible-looking ref: an entry that is not a table id resolves to nothing,
// so the ref is left exactly as it was found.
func IsTableID(id string) bool {
	return strings.HasPrefix(id, tableIDPrefix) || strings.HasPrefix(id, legacyTableIDPrefix)
}

// Canonicalize rewrites positional `{{ ref('N') }}` markers into their id form
// `{{ ref('<inputModels[N]>') }}`, and reports whether any positional ref could
// NOT be resolved.
//
// An exact mirror of esql.IndexToTableIDRefs, including its two refusals:
//
//   - An index out of range for inputModels is left UNCHANGED and flagged.
//   - An index whose target is not itself a table id (a corrupt input_models —
//     a positional entry that was never a real table) is left UNCHANGED and
//     flagged. Nothing is fabricated: a wrong id here would point the SQL at a
//     different table than the one the server reads.
//
// Non-numeric refs — already id form — are never touched, so id-form code is a
// byte-for-byte no-op and round-trips exactly.
//
// The bool is informational, not a failure: the canonical form is still the
// best available answer and is what the caller should hash and display. It says
// "this row's stored code carries a positional ref this client could not read",
// which is worth surfacing before a push writes the local file back — the server
// would then re-resolve that surviving positional ref against whatever
// input_models the write declared.
func Canonicalize(code string, inputModels []string) (string, bool) {
	unresolved := false
	out := indexRefPattern.ReplaceAllStringFunc(code, func(match string) string {
		sub := indexRefPattern.FindStringSubmatch(match)
		if len(sub) < 2 {
			return match
		}
		idx, err := strconv.Atoi(sub[1])
		if err != nil {
			// The pattern only matches \d+, so this is unreachable in practice
			// — but an integer overflow on an absurdly long run of digits does
			// reach it. Leave the ref alone rather than guess.
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

// PositionalRefs is every positional `{{ ref('N') }}` marker in some code, in
// the order it appears and de-duplicated — the markers, verbatim, not the
// indices.
//
// It exists so a refusal can NAME what it refused. Canonicalize answers "could
// everything be resolved" with a bool, which is the right answer for a rewrite
// but useless in a message: "this table still carries an unresolvable positional
// ref" sends the reader to open the SQL and hunt, while "still carries
// {{ ref('3') }}" tells them where to look.
//
// The same regex Canonicalize rewrites with, so the two can never disagree about
// what counts as positional — which is the property that matters, since one of
// them refuses on exactly what the other could not rewrite.
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

// DeriveInputModels is every table this code reads from: the id-shaped refs in
// its `{{ ref('…') }}` markers, de-duplicated and SORTED.
//
// The CLI sends this on every write, and never omits it. Server-side derivation
// (resolveDerivedRefs) fires only when the effective declared list is EMPTY, and
// a checked-out draft inherits its parent's non-empty input_models — so an
// omitted field 400s the moment an edit adds a new upstream ref, which is the
// single commonest pipeline edit there is. Sending the exact list also REMOVES
// stale entries, which would otherwise linger as phantom lineage edges and
// trigger cascade rebuilds nobody asked for.
//
// Sorted rather than first-seen so the same code always produces the same list:
// the value is compared against what the row already stores to decide whether a
// write is needed, and a first-seen order would report a pure reordering of the
// SQL as a lineage change.
//
// Refs that are not id-shaped are dropped rather than reported. A positional ref
// surviving here means Canonicalize could not resolve it, which is that
// function's signal to raise — and a name-based ref (a follow-up) is simply not
// this function's grammar. Either way, inventing an input from a string that is
// not a table id is the one outcome that must not happen.
func DeriveInputModels(code string) []string {
	matches := idRefPattern.FindAllStringSubmatch(code, -1)
	seen := make(map[string]struct{}, len(matches))
	var ids []string
	for _, m := range matches {
		if len(m) < 2 {
			continue
		}
		id := m[1]
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
