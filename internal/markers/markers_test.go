package markers

import (
	"errors"
	"slices"
	"strings"
	"testing"
)

// fixtures is the source this package is checked against: every alias-eligible
// family, both quote styles, the multi-brace form an agent produces inside an
// f-string, and the two families that are deliberately NOT in the grammar.
const fixtures = `
import tools

# {{ ref('table-in-a-comment') }} — dead text, still text.
orders = tools.query("SELECT * FROM {{ ref('table-abc') }}")
extra  = tools.query("SELECT * FROM {{{{ ref("modelv2-legacy") }}}}")
first  = tools.query("SELECT * FROM {{ ref('0') }}")
token  = "{{ secret('secret-1', 'apiKey') }}"
tools.queryExternal({{ secret('secret-2') }}, "select 1")
tools.run_agent({{ agent("agent-9") }}, "go")
tools.searchCodex({{ codex('cdx-7') }}, "policy")
tools.sendEmail(mailbox={{ mailbox('mailbox-3') }})
tools.runWorkflow({{ workflow('workflow-5') }})
tools.write_table("{{ write('nightly_totals') }}")
out = {{ file('reports/monthly', 'write') }}
`

// TestScanFindsEveryAliasEligibleFamily pins the grammar: which markers carry a
// reference, in what order, and with which kind attached.
func TestScanFindsEveryAliasEligibleFamily(t *testing.T) {
	got := Scan(fixtures)
	want := []struct {
		verb, kind, arg string
		args            int
	}{
		{"ref", KindTable, "table-in-a-comment", 1},
		{"ref", KindTable, "table-abc", 1},
		{"ref", KindTable, "modelv2-legacy", 1},
		{"ref", KindTable, "0", 1},
		{"secret", KindSecret, "secret-1", 2},
		{"secret", KindSecret, "secret-2", 1},
		{"agent", KindAgent, "agent-9", 1},
		{"codex", KindCodex, "cdx-7", 1},
		{"mailbox", KindMailbox, "mailbox-3", 1},
		{"workflow", KindWorkflow, "workflow-5", 1},
	}
	if len(got) != len(want) {
		t.Fatalf("scanned %d markers, want %d: %+v", len(got), len(want), got)
	}
	for i, w := range want {
		if got[i].Family.Verb != w.verb || got[i].Family.Kind != w.kind || got[i].Family.Args != w.args || got[i].Arg != w.arg {
			t.Errorf("marker %d = %+v, want verb=%s kind=%s args=%d arg=%s", i, got[i], w.verb, w.kind, w.args, w.arg)
		}
	}
}

// TestWriteAndFileAreNotAliasEligible is the exclusion, asserted rather than
// documented. A bare name in either family ALREADY means something — an
// auto-created output table, a store-relative folder path — so reading one as
// an alias would change what an existing folder deploys.
func TestWriteAndFileAreNotAliasEligible(t *testing.T) {
	for _, o := range Scan(fixtures) {
		if o.Family.Verb == "write" || o.Family.Verb == "file" {
			t.Errorf("%s must not be alias-eligible, got %+v", o.Family.Verb, o)
		}
	}
	for _, kind := range Kinds() {
		if kind == "file" {
			t.Errorf("file must not be a dependency kind — the marker's first argument is a folder path, not an id")
		}
	}
}

// TestRewriteIsAByteForByteNoOp is the property that keeps this feature from
// touching code nobody asked it to touch. A folder with no aliases must come out
// of a push exactly as it went in — brace count, quote style and all — or every
// file reads as drifted for ever.
func TestRewriteIsAByteForByteNoOp(t *testing.T) {
	for _, src := range []string{
		fixtures,
		`{{{{ ref('table-x') }}}}`,
		`{{ ref("table-x") }}`,
		`{{  ref (  'table-x'  )  }}`,
		"no markers at all",
		"",
	} {
		got, err := Rewrite(src, func(o Occurrence) (string, error) { return o.Arg, nil })
		if err != nil {
			t.Fatalf("rewrite %q: %v", src, err)
		}
		if got != src {
			t.Errorf("identity rewrite changed the source\n got: %q\nwant: %q", got, src)
		}
	}
}

// TestRewriteReplacesOnlyTheFirstArgument: a secret's field name and a marker's
// surrounding text are not the substitution's business.
func TestRewriteReplacesOnlyTheFirstArgument(t *testing.T) {
	src := `token = "{{ secret('acme_api', 'apiKey') }}" and {{{{ ref("orders") }}}}`
	got, err := Rewrite(src, func(o Occurrence) (string, error) {
		switch o.Arg {
		case "acme_api":
			return "secret-77", nil
		case "orders":
			return "table-77", nil
		}
		return o.Arg, nil
	})
	if err != nil {
		t.Fatalf("rewrite: %v", err)
	}
	want := `token = "{{ secret('secret-77', 'apiKey') }}" and {{{{ ref("table-77") }}}}`
	if got != want {
		t.Errorf("rewrite\n got: %q\nwant: %q", got, want)
	}
}

// TestRewriteAbortsOnError: a substitution that cannot be made leaves the source
// alone rather than half-rewritten.
func TestRewriteAbortsOnError(t *testing.T) {
	boom := errors.New("no binding for orders")
	got, err := Rewrite(`{{ ref('a') }} {{ ref('orders') }}`, func(o Occurrence) (string, error) {
		if o.Arg == "orders" {
			return "", boom
		}
		return "table-1", nil
	})
	if !errors.Is(err, boom) {
		t.Fatalf("err = %v, want the sub's own error", err)
	}
	if got != "" {
		t.Errorf("a failed rewrite must return nothing, got %q", got)
	}
}

// TestRewriteRefusesAQuotedReplacement: a value carrying a quote or a brace does
// not produce a marker the server reads differently — it produces one it cannot
// read at all, in a file the customer has committed.
func TestRewriteRefusesAQuotedReplacement(t *testing.T) {
	for _, bad := range []string{`table-'x`, `table-"x`, `table-{x`, `table-}x`} {
		if _, err := Rewrite(`{{ ref('orders') }}`, func(Occurrence) (string, error) { return bad, nil }); err == nil {
			t.Errorf("substituting %q must be refused", bad)
		}
	}
}

// TestPositionalRefsAreClassified: `{{ ref('0') }}` is an INDEX into
// input_models, which is what the AI build path persists, so it is the steady
// state of any table a colleague has touched through chat. A resolver that read
// it as a name would repoint the SQL at whatever table happened to be declared
// under that alias.
func TestPositionalRefsAreClassified(t *testing.T) {
	got := Scan(`{{ ref('0') }} {{ ref('12') }} {{ ref('orders') }} {{ ref('table-x') }} {{ secret('0', 'k') }}`)
	want := []bool{true, true, false, false, false}
	if len(got) != len(want) {
		t.Fatalf("scanned %d markers, want %d", len(got), len(want))
	}
	for i, w := range want {
		if got[i].Positional != w {
			t.Errorf("%q: Positional = %v, want %v", got[i].Marker, got[i].Positional, w)
		}
	}
}

// TestSecretAritiesAreDisjoint is the property the server states about the same
// pair of patterns: no input matches both, so a single secret marker is never
// reported — or rewritten — twice.
func TestSecretAritiesAreDisjoint(t *testing.T) {
	for _, src := range []string{
		`{{ secret('secret-1', 'apiKey') }}`,
		`{{ secret("secret-1","apiKey") }}`,
		`{{ secret('secret-1') }}`,
		`{{{{ secret( 'secret-1' ) }}}}`,
	} {
		got := Scan(src)
		if len(got) != 1 {
			t.Errorf("%q matched %d families, want exactly one: %+v", src, len(got), got)
		}
	}
	// And the arities are told apart, not merely counted once.
	if got := Scan(`{{ secret('s', 'f') }}`); len(got) != 1 || got[0].Family.Args != 2 {
		t.Errorf("two-argument secret misread: %+v", got)
	}
	if got := Scan(`{{ secret('s') }}`); len(got) != 1 || got[0].Family.Args != 1 {
		t.Errorf("one-argument secret misread: %+v", got)
	}
}

// TestIsResourceID covers the two prefixes nobody would guess: a codex is
// `cdx-`, and a table still rides the legacy `modelv2-`.
func TestIsResourceID(t *testing.T) {
	cases := []struct {
		kind, arg string
		want      bool
	}{
		{KindTable, "table-abc", true},
		{KindTable, "modelv2-abc", true},
		{KindTable, "orders", false},
		{KindTable, "secret-1", false},
		{KindCodex, "cdx-7", true},
		{KindCodex, "codex-7", false},
		{KindSecret, "secret-1", true},
		{KindAgent, "agent-1", true},
		{KindMailbox, "mailbox-1", true},
		{KindWorkflow, "workflow-1", true},
		// A note rides TWO prefixes: the rename preserved the old `skill-` ids,
		// and a client that knew only `note-` would read a live skill id as an
		// alias and hunt for a declaration nobody wrote.
		{KindNote, "note-1", true},
		{KindNote, "skill-1", true},
		{KindNote, "notebook", false},
		{"file", "file-1", false},
		{"nonsense", "nonsense-1", false},
	}
	for _, c := range cases {
		if got := IsResourceID(c.kind, c.arg); got != c.want {
			t.Errorf("IsResourceID(%q, %q) = %v, want %v", c.kind, c.arg, got, c.want)
		}
	}
}

// TestKindsMatchTheFamilyTable: the legal set a refusal prints must be derived
// from the families that actually resolve plus the marker-less kinds, not kept
// beside them by hand.
func TestKindsMatchTheFamilyTable(t *testing.T) {
	got := strings.Join(Kinds(), ",")
	if want := "agent,codex,mailbox,note,secret,table,workflow"; got != want {
		t.Errorf("Kinds() = %s, want %s", got, want)
	}
	for _, kind := range Kinds() {
		if len(idPrefixes[kind]) == 0 {
			t.Errorf("kind %q has no id prefix — IsResourceID would read every id of that kind as an alias", kind)
		}
	}
	declared := map[string]bool{}
	for _, f := range families {
		declared[f.Kind] = true
	}
	for _, kind := range markerlessKinds {
		declared[kind] = true
	}
	for kind := range idPrefixes {
		if !declared[kind] {
			t.Errorf("idPrefixes carries %q, which nothing declares", kind)
		}
	}
}

// TestMarkerKindsPartitionKinds: MarkerKinds and MarkerlessKinds must be a
// PARTITION of Kinds, disjoint and complete.
//
// They are two answers to two different questions — "what does a source marker
// resolve" and "what may an alias declare" — and one caller depends on the
// narrower one being genuinely narrower: wfdir.ValidAliasName runs at LOAD, so
// widening what it scans retroactively refuses an already-committed alias name.
// If a marker-less kind ever leaked into MarkerKinds, that refusal would come
// back silently, on files nobody edited.
func TestMarkerKindsPartitionKinds(t *testing.T) {
	if got := strings.Join(MarkerKinds(), ","); got != "agent,codex,mailbox,secret,table,workflow" {
		t.Errorf("MarkerKinds() = %s", got)
	}
	if got := strings.Join(MarkerlessKinds(), ","); got != "note" {
		t.Errorf("MarkerlessKinds() = %s", got)
	}
	seen := map[string]int{}
	for _, kind := range append(MarkerKinds(), MarkerlessKinds()...) {
		seen[kind]++
	}
	for _, kind := range Kinds() {
		switch seen[kind] {
		case 0:
			t.Errorf("kind %q is in Kinds() but in neither half — an alias may declare it and nothing knows which side it is on", kind)
		case 1:
		default:
			t.Errorf("kind %q is in BOTH halves", kind)
		}
		delete(seen, kind)
	}
	for kind := range seen {
		t.Errorf("kind %q is in a half but not in Kinds()", kind)
	}
	// Mutating the returned slice must not reach the package's own list — both
	// are appended to by callers building a message.
	MarkerlessKinds()[0] = "mutated"
	if markerlessKinds[0] == "mutated" {
		t.Error("MarkerlessKinds() hands out the package's own backing array")
	}
}

// TestMarkerlessKindsHaveNoPattern is the guarantee markerlessKinds exists to
// make, asserted rather than only argued.
//
// A kind with no marker family must not acquire one by somebody "completing the
// set": Scan and Rewrite run over workflow Python and table SQL, so an invented
// `{{ note('…') }}` would be substituted into source the server binds nothing
// from — leaving an id that reads, to anyone opening the file, as a bound
// reference.
func TestMarkerlessKindsHaveNoPattern(t *testing.T) {
	for _, kind := range markerlessKinds {
		for _, f := range families {
			if f.Kind == kind {
				t.Errorf("%q is declared marker-less but the %s family claims it — one of the two is wrong", kind, f.Family)
			}
		}
		if len(idPrefixes[kind]) == 0 {
			t.Errorf("marker-less kind %q has no id prefix, so nothing can tell one of its ids from an alias", kind)
		}
	}
}

// TestIDPrefixesExcludesMarkerlessKinds pins the one place Kinds() and
// IDPrefixes() deliberately disagree.
//
// IDPrefixes' sole caller scans a data app's TSX for a bare quoted id and feeds
// `used_but_undeclared`, whose advertised remedy is to widen the app's access
// allowlist. A data app cannot reach a note, so there is nothing to widen — and
// `skill-` matches ordinary JSX (`className="skill-badge"`), which would fail a
// customer's tree over a CSS class.
func TestIDPrefixesExcludesMarkerlessKinds(t *testing.T) {
	for _, prefix := range IDPrefixes() {
		for _, kind := range markerlessKinds {
			for _, markerless := range idPrefixes[kind] {
				if prefix == markerless {
					t.Errorf("IDPrefixes() offers %q, a %s prefix — the TSX id scan would report notes a data app can never declare", prefix, kind)
				}
			}
		}
	}
	// The kinds that DO have a marker are all still there; the exclusion must
	// not have been applied to the wrong side of the list.
	for kind, prefixes := range idPrefixes {
		if kind == KindNote {
			continue
		}
		for _, want := range prefixes {
			if !slices.Contains(IDPrefixes(), want) {
				t.Errorf("IDPrefixes() dropped %q, a %s prefix", want, kind)
			}
		}
	}
}
