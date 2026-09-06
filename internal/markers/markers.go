// Package markers owns the CLI's view of the marker grammar a workflow's Python
// and a derived table's SQL are written in, and the one question a PORTABLE
// folder has to ask of it: which markers carry a reference to a row that exists
// in exactly one organization, and what does the source become once those
// references have been replaced.
//
// It exists as its own package, stdlib only and with no dependency on the API
// client or on wfdir, for the reason tablerefs does: it is a pure mirror of a
// server-side rule (backend/engine/pymarkers and backend/engine/esql) rather
// than anything about HTTP, and the decision it feeds is made BEFORE any
// request. That ordering is the whole design of the alias layer — a folder's
// declared names are resolved to ids HERE, in the client, so the server only
// ever sees ids and its authorization surface never grows a second grammar to
// check.
//
// # What is alias-eligible, and what deliberately is not
//
// A marker is alias-eligible when its first argument NAMES A ROW: an id minted
// in one organization, meaningless in the next. Those are the arguments a folder
// checked into git cannot carry literally, and they are the ones this package
// reports and rewrites.
//
// Three marker families take a first argument that LOOKS like it belongs here
// and does not. Two are excluded on the same principle — a bare non-id name in
// them ALREADY HAS A MEANING, so reading it as an alias would silently change
// what an existing folder deploys — and the third is excluded for a different
// reason entirely, which is worth reading before someone "completes" the set.
//
//   - `{{ write('name') }}` names an OUTPUT the run creates.
//     pymarkers.ExtractWriteTableIDsFromContent skips a non-RRN name on purpose
//     and the runtime auto-creates a table called that. The portable form
//     already exists, so there is nothing for an alias to fix and everything for
//     one to break.
//   - `{{ file('reports/monthly', 'write') }}` names a store-relative FOLDER
//     PATH, not a `file-` id — see pymarkers.fileMarkerPattern and
//     rworkflow.ExtractFileRefs, which bind a (path, mode) scope and never an id.
//     A path is already organization-independent, and every such marker in every
//     folder that exists today would otherwise be classified as an undeclared
//     alias. Worse, once someone declared a dependency called `reports` the
//     resolver would rewrite that path into an id and the scope would name a
//     folder nobody has.
//   - `{{ file_url('file::ilsxeiko') }}` DOES carry an organization-scoped id,
//     and it is excluded anyway because A WORKFLOW NEVER RESOLVES IT. The
//     pattern lives in esql (FileURLPattern) but its only resolver is
//     manalysis.inlineFileURLs, which is wired into PrepareRefBasedPythonScript
//     — the ad-hoc SESSION runPython path — and into nothing else;
//     PrepareRefBasedPythonFiles, the workflow run prep, never calls it, and no
//     rworkflow save-time binder scans for it. The second resolver,
//     esql.ResolveFileURLAliases, runs in the runPython TOOL on chat session
//     aliases. So the argument is an upload key belonging to a chat session, the
//     folder loops this package serves carry no such thing, and a marker in a
//     committed workflow file is inert text on both sides of the wire. Mirroring
//     it would add a dependency kind that resolves nothing and a refusal that
//     could only ever be wrong. ⚠️ This is a claim about WIRING, not about the
//     id: if a workflow prep ever starts inlining file URLs, the id is
//     organization-scoped and the family belongs in the table below.
//
// # A dependency kind is not always a marker family
//
// The two coincided until automations. An automation is a JSON document whose
// reference set is a structured FIELD, so an alias there is resolved by the CLI
// before the request is built and no marker is involved on either side. Such a
// kind is declared in markerlessKinds and joins Kinds(), and it deliberately does
// NOT get a pattern here — see that variable for why inventing one is worse than
// it looks.
//
// # Keeping this in step with the server
//
// The patterns below are COPIED, byte for byte, from the server files named
// against each one, including the `\{\{+` brace tolerance that consumes the
// extra braces an agent produces when it reaches for f-string escaping. A
// pattern that drifts LOOSER here rewrites text the server would have left
// alone; one that drifts STRICTER leaves an alias in the source, where the
// server reads it as a literal id — and for the secret families that is a
// soft-failing warning rather than a refusal, so the push succeeds and the
// workflow runs with nothing bound. Both directions are silent. When a marker
// family is added or changed on the server, it is added or changed here.
package markers

import (
	"fmt"
	"regexp"
	"sort"
	"strings"
)

// The dependency kinds an alias may declare — one per alias-eligible marker
// family, with the two secret arities sharing one, PLUS the marker-less kinds
// below.
//
// These strings are written into a COMMITTED ronja.json as a dependency's
// "kind", so they are part of the file format: renaming one is a breaking
// change to every folder in the field, not a refactor.
const (
	KindTable    = "table"
	KindSecret   = "secret"
	KindAgent    = "agent"
	KindCodex    = "codex"
	KindMailbox  = "mailbox"
	KindWorkflow = "workflow"
	KindModule   = "module"
	// KindNote is the first dependency kind with NO marker family behind it,
	// and the distinction is the point rather than an omission — see
	// markerlessKinds.
	KindNote = "note"
)

// markerlessKinds are the dependency kinds an alias may declare that NO source
// marker resolves.
//
// The families table below mirrors the server's own marker patterns byte for
// byte, and the parity tests hold it there, because a marker is text BOTH sides
// parse: drift in either direction is silent. A kind here is the opposite
// situation. An automation is a JSON document whose reference set is a
// structured field —
//
//	"references": [{"kind": "note", "resourceID": "policy"}]
//
// — so the alias sits in a FIELD the CLI resolves before sending, not in a
// marker the server ever sees. There is no server pattern to be in parity with.
//
// Putting `note` in families anyway would have to invent a `{{ note('…') }}`
// grammar, and inventing one is worse than it looks. The parity test would fail
// by design (no serverVar to compare against, which is the right failure for the
// wrong reason). Scan and Rewrite run over WORKFLOW PYTHON and TABLE SQL as well
// as over a folder's other files, so the invented marker would be substituted
// into source the server binds nothing from — an id sitting in a workflow that
// reads, to anyone opening it, as a bound reference. And the day the server DOES
// grow a note marker, the CLI would already have a pattern nobody had checked
// against it.
//
// So the split is between "which markers carry a row reference" (families) and
// "which kinds may an alias declare" (Kinds, below): the first is a mirror of a
// server rule, the second is this CLI's own file format. They coincided until
// now, which is exactly why it is worth naming the moment they stop.
//
// ⚠️ A kind here is NOT in IDPrefixes() — see the note there.
var markerlessKinds = []string{KindNote}

// Family is one alias-eligible marker family: the verb it is spelled with, the
// dependency kind its first argument names, and how many quoted arguments it
// takes.
//
// Args is part of the identity because `secret` is arity-overloaded on the
// server — two arguments is a value-inject, one is a query-authorize — and the
// two are separate families that happen to declare the same kind.
type Family struct {
	Verb string
	Kind string
	Args int
}

// String names a family in a diagnostic that is about the FAMILY rather than
// about one occurrence of it: the verb and its arity, since "the secret marker"
// is ambiguous where `secret(id, …)` and `secret(id)` are two entries in the
// table below and only one of them may have drifted.
//
// Its callers are the parity tests, and that is the whole set on purpose. They
// are the only diagnostics here with no source to point at — they compare this
// package against the server's, so there is no file, no line and no marker text
// to quote. Every refusal the alias layer raises DOES have one, and quotes it
// (Occurrence.Marker): "orders.sql writes `{{ secret('acme') }}`" tells a reader
// where to put the cursor, and "the secret(id) family" does not. Adding a caller
// in internal/commands would almost certainly be replacing something better.
func (f Family) String() string {
	if f.Args == 2 {
		return f.Verb + "(id, …)"
	}
	return f.Verb + "(id)"
}

// Occurrence is one alias-eligible marker found in a source string.
type Occurrence struct {
	// Family is which marker family matched, and therefore which dependency
	// kind Arg names.
	Family Family
	// Arg is the marker's FIRST quoted argument, verbatim — the id or the alias.
	// The second argument of a two-arity family (a secret's field name) is never
	// reported and never substituted.
	Arg string
	// Marker is the whole matched marker, braces included, so a message can quote
	// what it is talking about rather than describe it.
	Marker string
	// Positional reports a `{{ ref('0') }}` — an INDEX into the row's
	// input_models, not a name. It is the form the AI build path persists (see
	// the tablerefs package doc), so it is the steady state of any table a
	// colleague has touched through chat, and a resolver must leave it exactly
	// as it found it.
	//
	// An alias can never collide with this grammar, because an all-digit alias
	// is refused where an alias is declared — the two rules are halves of one
	// guarantee and neither is safe alone.
	Positional bool

	// argStart / argEnd bound Arg inside the source, and matchStart / matchEnd
	// bound Marker. Unexported: they are how Rewrite splices, and a caller
	// holding byte offsets into a string it may have rewritten is a footgun with
	// no use case behind it.
	argStart, argEnd     int
	matchStart, matchEnd int
}

// families is every alias-eligible marker family and the pattern that finds it,
// with the server file each pattern is copied from.
//
// ⚠️ The two `secret` entries are DISJOINT BY CONSTRUCTION, which is a property
// to preserve rather than a coincidence to rely on: the two-argument pattern
// requires a comma after the first quoted argument and the one-argument pattern
// requires a `)` there, so no input can match both. The server states the same
// thing about the same pair (querySecretMarkerPattern), and it is what keeps a
// single secret marker from being reported — or rewritten — twice.
var families = []struct {
	Family
	// serverVar is the name of the var this pattern was copied from, in the
	// server file the comment above each entry names. It is not decoration: the
	// parity test reads both files and refuses a pattern that has stopped being
	// byte-identical to the one the server actually matches with, which is the
	// only mechanical guard there can be against the two drifting — the CLI is a
	// separate module and cannot import the thing it mirrors.
	serverVar string
	pattern   *regexp.Regexp
}{
	// backend/engine/esql/esql.go, tableIDRefPattern — also mirrored as
	// tablerefs.idRefPattern, which is this same regex read for a different
	// question. It matches a positional ref too (digits are an arbitrary
	// string); Occurrence.Positional is how the two are told apart.
	{Family{Verb: "ref", Kind: KindTable, Args: 1}, "tableIDRefPattern",
		regexp.MustCompile(`\{\{+\s*ref\(\s*['"]([^'"]+)['"]\s*\)\s*\}\}+`)},
	// backend/engine/pymarkers/markers.go, secretMarkerPattern.
	{Family{Verb: "secret", Kind: KindSecret, Args: 2}, "secretMarkerPattern",
		regexp.MustCompile(`\{\{+\s*secret\(\s*['"]([^'"]+)['"]\s*,\s*['"]([^'"]+)['"]\s*\)\s*\}\}+`)},
	// backend/engine/pymarkers/markers.go, querySecretMarkerPattern.
	{Family{Verb: "secret", Kind: KindSecret, Args: 1}, "querySecretMarkerPattern",
		regexp.MustCompile(`\{\{+\s*secret\(\s*['"]([^'"]+)['"]\s*\)\s*\}\}+`)},
	// backend/engine/pymarkers/markers.go, agentMarkerPattern.
	{Family{Verb: "agent", Kind: KindAgent, Args: 1}, "agentMarkerPattern",
		regexp.MustCompile(`\{\{+\s*agent\(\s*['"]([^'"]+)['"]\s*\)\s*\}\}+`)},
	// backend/engine/pymarkers/markers.go, codexMarkerPattern.
	{Family{Verb: "codex", Kind: KindCodex, Args: 1}, "codexMarkerPattern",
		regexp.MustCompile(`\{\{+\s*codex\(\s*['"]([^'"]+)['"]\s*\)\s*\}\}+`)},
	// backend/engine/pymarkers/markers.go, mailboxMarkerPattern.
	{Family{Verb: "mailbox", Kind: KindMailbox, Args: 1}, "mailboxMarkerPattern",
		regexp.MustCompile(`\{\{+\s*mailbox\(\s*['"]([^'"]+)['"]\s*\)\s*\}\}+`)},
	// backend/engine/pymarkers/markers.go, workflowMarkerPattern.
	{Family{Verb: "workflow", Kind: KindWorkflow, Args: 1}, "workflowMarkerPattern",
		regexp.MustCompile(`\{\{+\s*workflow\(\s*['"]([^'"]+)['"]\s*\)\s*\}\}+`)},
	// backend/engine/pymarkers/markers.go, moduleMarkerPattern. The one family
	// whose marker sits in IMPORT position and substitutes to a bare package
	// name rather than a quoted literal — which changes nothing here, because
	// this layer only ever reads and rewrites the marker's first ARGUMENT, and
	// that argument is a `module-` id like every other entry below.
	{Family{Verb: "module", Kind: KindModule, Args: 1}, "moduleMarkerPattern",
		regexp.MustCompile(`\{\{+\s*module\(\s*['"]([^'"]+)['"]\s*\)\s*\}\}+`)},
}

// idPrefixes mirrors the per-kind predicates in backend/lib/rrn/rrn.go — the
// same table the server checks an id against before it will treat it as naming
// a row.
//
// Two entries are worth reading twice, because guessing either would be wrong:
// a codex is `cdx-`, not `codex-`, and a table rides a LEGACY `modelv2-` prefix
// beside the current one. Rows minted under the old prefix are still ordinary
// tables, so a client that only knew `table-` would read one as an alias and
// try to resolve it.
// A note is the third entry worth reading twice, and for a reason of its own: it
// rides TWO prefixes, `note-` and the legacy `skill-`, because the rename
// preserved the old ids. The server says the same thing in the same place
// (rscheduledjob.ValidateReferenceKind: "kind=note requires a note- or skill-
// prefix"), and a client that only knew `note-` would read a live skill id as an
// alias and go looking for a declaration nobody wrote.
var idPrefixes = map[string][]string{
	KindTable:    {"table-", "modelv2-"},
	KindSecret:   {"secret-"},
	KindAgent:    {"agent-"},
	KindCodex:    {"cdx-"},
	KindMailbox:  {"mailbox-"},
	KindWorkflow: {"workflow-"},
	KindModule:   {"module-"},
	KindNote:     {"note-", "skill-"},
}

// IsResourceID reports whether arg already names a row of that kind, and is
// therefore not an alias.
//
// It is a PREFIX test and nothing more, exactly as rrn.Is is: it says the string
// is shaped like an id of that kind, never that the row exists or that the
// caller can see it. An unknown kind reports false, which is the safe direction
// — a caller asking about a kind this build has no prefix for learns that it
// cannot vouch for the string.
func IsResourceID(kind, arg string) bool {
	for _, prefix := range idPrefixes[kind] {
		if strings.HasPrefix(arg, prefix) {
			return true
		}
	}
	return false
}

// IDPrefixes is every id prefix this package recognises, longest first.
//
// Exported for the ONE caller that has to find an id in text this package's
// grammar does not cover: a data app's TSX, where a resource id appears as a
// bare quoted literal (`const X = "secret-…"`) rather than as a marker
// argument. That caller builds its pattern FROM this table rather than spelling
// the prefixes again — a second hand-kept list is one somebody forgets, and
// forgetting it means a kind added below silently stops being covered, with no
// failing test to say so.
//
// LONGEST FIRST, because the caller joins these into a regex alternation and an
// alternation takes the first branch that matches. No two prefixes here overlap
// today, so the order is a guarantee about the future rather than a fix for the
// present. Ties broken lexically, so the order is stable.
//
// ⚠️ A MARKERLESS KIND IS EXCLUDED, and this is the one place the two lists
// deliberately disagree. The caller is scanning a data app's TSX for a bare
// quoted id, and it feeds `used_but_undeclared` — a finding whose advertised
// remedy is to widen the app's `access` allowlist. A data app cannot reach a
// note at all (there is no allowedNoteIDs to widen), so every hit would be a
// false positive with no fix. And `skill-` is a prefix ORDINARY TSX carries:
// `className="skill-badge"` matches the pattern exactly, which would fail a
// customer's whole tree over a CSS class. A false positive whose remedy widens
// privileges is worse than the miss it prevents — the rule appSourceExts is
// already written to, one kind over.
func IDPrefixes() []string {
	markerless := map[string]bool{}
	for _, kind := range markerlessKinds {
		markerless[kind] = true
	}
	seen := map[string]bool{}
	out := make([]string, 0, len(idPrefixes))
	for kind, prefixes := range idPrefixes {
		if markerless[kind] {
			continue
		}
		for _, prefix := range prefixes {
			if !seen[prefix] {
				seen[prefix] = true
				out = append(out, prefix)
			}
		}
	}
	sort.Slice(out, func(i, j int) bool {
		if len(out[i]) != len(out[j]) {
			return len(out[i]) > len(out[j])
		}
		return out[i] < out[j]
	})
	return out
}

// Kinds is the set of dependency kinds an alias may declare, sorted, so a
// refusal that lists the legal set reads the same way twice.
//
// Derived from the family table PLUS markerlessKinds rather than written out
// beside them, so adding either cannot leave the legal set behind — a hand-kept
// second list is one somebody forgets, and forgetting it here refuses a kind the
// resolver would have handled perfectly.
func Kinds() []string {
	out := MarkerKinds()
	seen := map[string]bool{}
	for _, kind := range out {
		seen[kind] = true
	}
	for _, kind := range markerlessKinds {
		if seen[kind] {
			continue
		}
		seen[kind] = true
		out = append(out, kind)
	}
	sort.Strings(out)
	return out
}

// MarkerKinds is the half of Kinds() a SOURCE MARKER resolves — the family
// table only, markerlessKinds excluded. Sorted, like Kinds().
//
// It exists because one caller needs the NARROWER set and widening it would be
// a retroactive change to a committed file format. wfdir.ValidAliasName refuses
// an alias whose NAME is shaped like a resource id, and it runs at LOAD, over
// manifests written before this build existed: fed Kinds(), the day `note`
// joined the legal set it would start refusing an already-committed alias called
// `note-taking` or `skill-cache`, and refusing at load takes every command away
// from the person holding that folder — `status` included. Fed this, adding a
// marker-less kind cannot narrow what an existing manifest may be called. The
// marker-less half of the same rule is enforced at the acceptance point instead;
// see wfdir.Manifest.CheckDeclarations.
//
// ⚠️ Anything answering "what may an alias DECLARE" wants Kinds(), not this.
func MarkerKinds() []string {
	seen := map[string]bool{}
	out := make([]string, 0, len(families))
	for _, f := range families {
		if seen[f.Kind] {
			continue
		}
		seen[f.Kind] = true
		out = append(out, f.Kind)
	}
	sort.Strings(out)
	return out
}

// MarkerlessKinds is the other half: the dependency kinds no source marker
// resolves. See markerlessKinds for what that means and why the split exists.
func MarkerlessKinds() []string {
	out := make([]string, len(markerlessKinds))
	copy(out, markerlessKinds)
	sort.Strings(out)
	return out
}

// Scan reports every alias-eligible marker in content, in first-seen order.
//
// ⚠️ It reads the source as TEXT and does not model dead code. The server skips
// markers inside `#` comments and triple-quoted blocks when it BINDS them
// (pymarkers.stripLineComments / stripTripleQuotedBlocks), so a marker in a
// commented-out line binds nothing there — but it is still one this package
// reports, and one Rewrite will substitute into. Substituting is harmless
// (dead text stays dead, and an id reads better in a comment than a name that
// no longer resolves); REFUSING on it would not be. A caller that turns an
// unresolvable name into an error therefore has to decide what a commented-out
// marker means before it does, and the safe answer is to leave a name it cannot
// resolve exactly as it found it.
//
// That is what the alias layer does, and it costs nothing, because the check
// worth having is not a source-level one at all: "every alias this folder
// DECLARES is bound in the stack being pushed to" is answered from the manifest
// alone, names the fix (`ronja bind --stack <name>`), and has no opinion about
// where — or whether — the alias is used. A name that is neither declared nor
// id-shaped is left exactly as found and refused by the server, which is the
// same refusal it has always given and is loud on every marker family.
func Scan(content string) []Occurrence {
	return scan(content)
}

// Rewrite replaces the FIRST quoted argument of every alias-eligible marker
// with whatever sub returns for it, and returns the rewritten source.
//
// Returning the argument unchanged is a NO-OP, and byte-for-byte: an occurrence
// whose substitution equals its argument is not spliced at all, so a Rewrite
// whose sub is the identity returns the caller's own string. That property is
// what stops this feature from touching code nobody asked it to touch — a
// folder with no aliases in it must come out of a push exactly as it went in,
// down to the brace count and the quote style, or every file reads as drifted.
//
// Returning an error aborts and nothing is rewritten. The second argument of a
// two-arity family — a secret's field name, and nothing else today — is never
// passed and never substituted.
func Rewrite(content string, sub func(Occurrence) (string, error)) (string, error) {
	var b strings.Builder
	last := 0
	for _, o := range scan(content) {
		replacement, err := sub(o)
		if err != nil {
			return "", err
		}
		if replacement == o.Arg {
			continue
		}
		// A replacement carrying a quote or a brace would not produce a marker
		// the server can read — it would produce a marker that means something
		// else, or none at all, in a file the customer has committed. Nothing
		// this substitutes is ever meant to contain one (an id is
		// `<namespace>-<xid>`), so a value that does is a bug upstream and the
		// honest response is to refuse rather than to write it.
		if strings.ContainsAny(replacement, `'"{}`) {
			return "", fmt.Errorf("cannot substitute %q into %s: a marker argument may not contain a quote or a brace", replacement, o.Marker)
		}
		b.WriteString(content[last:o.argStart])
		b.WriteString(replacement)
		last = o.argEnd
	}
	if last == 0 {
		// Nothing was substituted. Returned as it came in rather than rebuilt
		// through the builder, so the no-op is a no-op by construction and not
		// by the builder happening to be faithful.
		return content, nil
	}
	b.WriteString(content[last:])
	return b.String(), nil
}

// scan is the shared half of Scan and Rewrite, so the two can never disagree
// about what a marker is or where it sits.
func scan(content string) []Occurrence {
	var out []Occurrence
	for _, f := range families {
		for _, span := range f.pattern.FindAllStringSubmatchIndex(content, -1) {
			if len(span) < 4 || span[2] < 0 {
				continue
			}
			arg := content[span[2]:span[3]]
			out = append(out, Occurrence{
				Family:     f.Family,
				Arg:        arg,
				Marker:     content[span[0]:span[1]],
				Positional: f.Kind == KindTable && isAllDigits(arg),
				argStart:   span[2],
				argEnd:     span[3],
				matchStart: span[0],
				matchEnd:   span[1],
			})
		}
	}
	sort.SliceStable(out, func(i, j int) bool { return out[i].matchStart < out[j].matchStart })
	// Two families cannot match the same text — each pattern pins its own verb,
	// and the two secret arities are disjoint — so this drops nothing today. It
	// is here because Rewrite splices by offset, and a single overlapping pair
	// would splice one marker's bytes into the middle of another's: a corrupt
	// file rather than a wrong one. A family added later that can overlap an
	// existing one is caught here as a dropped occurrence, which is visible,
	// rather than as mangled output, which is not.
	kept := out[:0]
	end := 0
	for _, o := range out {
		if o.matchStart < end {
			continue
		}
		end = o.matchEnd
		kept = append(kept, o)
	}
	return kept
}

// isAllDigits reports the positional-ref grammar: one or more ASCII digits and
// nothing else. An empty string is not positional — the patterns cannot produce
// one, since each requires at least one character between the quotes.
func isAllDigits(s string) bool {
	if s == "" {
		return false
	}
	for _, r := range s {
		if r < '0' || r > '9' {
			return false
		}
	}
	return true
}
