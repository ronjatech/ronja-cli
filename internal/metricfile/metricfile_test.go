package metricfile

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"strings"
	"testing"
)

const wholeFile = `{
  "recipe": {
    "source": "orders",
    "time": {"column": "created_at", "native_grain": "day"},
    "dimensions": [{"name": "country"}],
    "base_measures": [
      {"name": "revenue", "agg": "sum",   "column": "amount"},
      {"name": "orders",  "agg": "count", "column": "*"}
    ],
    "value": "revenue / orders"
  },
  "description": "Average order value, per day.",
  "reportingTimezone": "Europe/Stockholm"
}`

// TestParseReadsTheThreeKeys: the whole of what a metric file may say.
func TestParseReadsTheThreeKeys(t *testing.T) {
	file, err := Parse([]byte(wholeFile))
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if file.Description == nil || *file.Description != "Average order value, per day." {
		t.Errorf("description = %v", file.Description)
	}
	if file.ReportingTimezone == nil || *file.ReportingTimezone != "Europe/Stockholm" {
		t.Errorf("reportingTimezone = %v", file.ReportingTimezone)
	}
	source, err := file.Source()
	if err != nil {
		t.Fatalf("source: %v", err)
	}
	if source != "orders" {
		t.Errorf("source = %q", source)
	}
}

// TestParseRefusesAnUnknownTopLevelKey is the sidecar's rule, and it is the
// whole reason this file has a parser rather than a json.Unmarshal: a dropped
// key is the failure the sync idea exists to remove. Somebody who writes
// `"verified": true` expecting it to be honoured has to be told where they wrote
// it, not ignored on every push for ever.
func TestParseRefusesAnUnknownTopLevelKey(t *testing.T) {
	_, err := Parse([]byte(`{"recipe":{"source":"orders"},"verified":true}`))
	if err == nil {
		t.Fatal("an unknown top-level key must be refused")
	}
	if !strings.Contains(err.Error(), "verified") {
		t.Errorf("the refusal must name the key: %v", err)
	}
}

// TestParseKeepsUnknownKeysInsideTheRecipe is the OPPOSITE rule one level down,
// and the asymmetry is the design: the recipe grammar belongs to
// rdb.ValidateMetricRecipe, so a key this build has never heard of has to reach
// the server intact rather than being silently dropped by a client that is not
// entitled to an opinion about it.
func TestParseKeepsUnknownKeysInsideTheRecipe(t *testing.T) {
	file, err := Parse([]byte(`{"recipe":{"source":"orders","filters":[{"column":"status","eq":"paid"}],"value":"revenue"}}`))
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	resolved, err := file.Resolve("table-orders")
	if err != nil {
		t.Fatalf("resolve: %v", err)
	}
	if !strings.Contains(string(resolved), `"filters"`) {
		t.Errorf("an unknown recipe key was dropped: %s", resolved)
	}
	if !strings.Contains(string(resolved), `"status"`) {
		t.Errorf("an unknown recipe key's contents were dropped: %s", resolved)
	}
	if !strings.Contains(string(resolved), `"source":"table-orders"`) {
		t.Errorf("the source was not rewritten: %s", resolved)
	}
}

// TestParseRefusesAFileWithNoRecipe: a metric IS its recipe, and the schema's
// own CHECK refuses a metric row without one — so a file that declares none is
// a file no push could ever send.
func TestParseRefusesAFileWithNoRecipe(t *testing.T) {
	for _, body := range []string{`{}`, `{"recipe":null}`, `{"description":"x"}`} {
		if _, err := Parse([]byte(body)); err == nil {
			t.Errorf("%s must be refused", body)
		}
	}
}

// TestSourceRefusesWhatTheServerCannotUse: a missing or non-string source is
// caught HERE rather than left to the server, because the refusal a caller can
// act on names the FILE.
func TestSourceRefusesWhatTheServerCannotUse(t *testing.T) {
	for _, body := range []string{
		`{"recipe":{"value":"revenue"}}`,
		`{"recipe":{"source":42}}`,
		`{"recipe":{"source":"  "}}`,
	} {
		file, err := Parse([]byte(body))
		if err != nil {
			continue
		}
		if _, err := file.Source(); err == nil {
			t.Errorf("%s must be refused", body)
		}
	}
}

// TestRecipeSHA256HashesTheBytesItWasGiven is the compare-and-swap's whole
// contract, and it is the one thing in this package a "tidy" refactor would
// break invisibly.
//
// The server takes the digest over the bytes it has STORED. A client that
// decoded the recipe into a map and re-marshalled it would hash a DIFFERENT key
// order — Go marshals a map in sorted order — and the precondition would fail
// against an edit nobody made. So the fixture below is deliberately spelled with
// its keys OUT of alphabetical order: if anything in this path ever normalises,
// this digest moves and this test says so.
func TestRecipeSHA256HashesTheBytesItWasGiven(t *testing.T) {
	// `value` before `source` before `version` — not sorted, and not what a
	// re-marshal would produce.
	raw := json.RawMessage(`{"value":"revenue / orders","source":"table-orders","version":1}`)
	want := sha256.Sum256(raw)
	if got := RecipeSHA256(raw); got != hex.EncodeToString(want[:]) {
		t.Errorf("RecipeSHA256 = %q, want the digest of the exact bytes", got)
	}
	// A re-marshal really does produce different bytes, which is what makes the
	// assertion above worth making rather than tautological.
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(raw, &fields); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	remarshalled, err := json.Marshal(fields)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	if RecipeSHA256(remarshalled) == RecipeSHA256(raw) {
		t.Fatal("the fixture's keys are already sorted, so this test proves nothing — respell it")
	}
}

// TestRecipeSHA256TreatsAbsenceAsNoAnswer: empty is what every guard in this
// loop reads as "no recording", and a real digest over the four bytes "null"
// would arm a comparison against a recipe that does not exist.
func TestRecipeSHA256TreatsAbsenceAsNoAnswer(t *testing.T) {
	if got := RecipeSHA256(nil); got != "" {
		t.Errorf("nil recipe hashed to %q", got)
	}
	if got := RecipeSHA256(json.RawMessage("null")); got != "" {
		t.Errorf("a JSON null recipe hashed to %q", got)
	}
}

// TestFingerprintSeparatesAbsentFromAnEmptyClaim: the two are different
// instructions — one leaves the field alone, one clears it — and a fingerprint
// that folded them would miss the edit that deletes a description.
func TestFingerprintSeparatesAbsentFromAnEmptyClaim(t *testing.T) {
	recipe := json.RawMessage(`{"source":"table-orders"}`)
	empty := ""
	absent := Fingerprint(recipe, nil, nil)
	cleared := Fingerprint(recipe, &empty, nil)
	if absent == cleared {
		t.Error("an absent description and an explicit empty one fingerprint the same")
	}
	// And the two three-state fields are told apart from each other: a
	// description of "x" with no timezone must not fingerprint as a timezone of
	// "x" with no description.
	value := "x"
	if Fingerprint(recipe, &value, nil) == Fingerprint(recipe, nil, &value) {
		t.Error("the description and the timezone are not separated in the fingerprint")
	}
}

// TestFingerprintIgnoresReindentation: whitespace is insignificant to the server
// and to this file's meaning, so re-indenting a committed file must not read as
// a change worth re-staging and re-BUILDING a metric for.
func TestFingerprintIgnoresReindentation(t *testing.T) {
	compact, err := Parse([]byte(`{"recipe":{"source":"orders","value":"revenue"}}`))
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	spread, err := Parse([]byte("{\n  \"recipe\": {\n    \"source\": \"orders\",\n    \"value\":   \"revenue\"\n  }\n}"))
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	one, err := compact.Resolve("table-orders")
	if err != nil {
		t.Fatalf("resolve: %v", err)
	}
	two, err := spread.Resolve("table-orders")
	if err != nil {
		t.Fatalf("resolve: %v", err)
	}
	if Fingerprint(one, nil, nil) != Fingerprint(two, nil, nil) {
		t.Errorf("re-indentation changed the fingerprint:\n%s\n%s", one, two)
	}
}

// TestResolveDoesNotMutateTheParsedFile: `status` and `push` both resolve, and a
// resolve that wrote into the parsed recipe would make the second caller see the
// first one's substitution.
func TestResolveDoesNotMutateTheParsedFile(t *testing.T) {
	file, err := Parse([]byte(`{"recipe":{"source":"orders","value":"revenue"}}`))
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if _, err := file.Resolve("table-one"); err != nil {
		t.Fatalf("resolve: %v", err)
	}
	source, err := file.Source()
	if err != nil {
		t.Fatalf("source: %v", err)
	}
	if source != "orders" {
		t.Errorf("resolve mutated the parsed recipe: source is now %q", source)
	}
}

// --- tags -------------------------------------------------------------------

// TestParseTagsIsThreeState: ABSENT is a folder that does not manage the
// metric's tags, a list is the names it asserts, and `[]` asserts none. The
// three have to stay distinguishable all the way out of the parser, or "stop
// managing tags" and "assert no tags" become the same edit.
func TestParseTagsIsThreeState(t *testing.T) {
	absent, err := Parse([]byte(`{"recipe":{"source":"orders"}}`))
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if absent.Tags != nil {
		t.Errorf("an absent key parsed as %v, want nil", *absent.Tags)
	}
	empty, err := Parse([]byte(`{"recipe":{"source":"orders"},"tags":[]}`))
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if empty.Tags == nil || len(*empty.Tags) != 0 {
		t.Errorf("[] parsed as %v, want a non-nil empty list", empty.Tags)
	}
	listed, err := Parse([]byte(`{"recipe":{"source":"orders"},"tags":[" Finance ","Sales"]}`))
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	// TRIMMED, as the server trims, and otherwise spelled as written — the case
	// is the server's to fold, not this parser's.
	if listed.Tags == nil || strings.Join(*listed.Tags, "|") != "Finance|Sales" {
		t.Errorf("tags = %v", listed.Tags)
	}
}

// TestParseTagsRefusals: every shape the server would refuse, refused where it
// was written and naming the offending entry.
func TestParseTagsRefusals(t *testing.T) {
	long := strings.Repeat("a", 65)
	// 22 bytes of 2-byte runes: 11 characters, well under 64 CHARACTERS, and 66
	// BYTES — the server's cap is len(), and a character count here would pass
	// a name the server then refuses.
	multibyte := strings.Repeat("ö", 33)
	many := make([]string, 21)
	for i := range many {
		many[i] = `"t` + strings.Repeat("x", i) + `"`
	}
	for name, tc := range map[string]struct {
		tags string
		want string
	}{
		"not a list":       {`"Finance"`, "list"},
		"null":             {`null`, "list"},
		"a number":         {`["Finance", 3]`, "3"},
		"an object":        {`[{"name":"x"}]`, "not a string"},
		"blank":            {`["Finance", "  "]`, "empty"},
		"over 64 bytes":    {`["` + long + `"]`, "64"},
		"multibyte bytes":  {`["` + multibyte + `"]`, "64"},
		"more than 20":     {`[` + strings.Join(many, ",") + `]`, "20"},
		"duplicate folded": {`["Finance", " finance"]`, "finance"},
	} {
		t.Run(name, func(t *testing.T) {
			_, err := Parse([]byte(`{"recipe":{"source":"orders"},"tags":` + tc.tags + `}`))
			if err == nil {
				t.Fatalf("tags %s must be refused", tc.tags)
			}
			if !strings.Contains(err.Error(), tc.want) {
				t.Errorf("refusal %q does not mention %q", err, tc.want)
			}
		})
	}
	// Exactly 64 bytes and exactly 20 entries are fine: the caps are inclusive.
	edge := make([]string, 20)
	for i := range edge {
		edge[i] = `"t` + strings.Repeat("x", i) + `"`
	}
	edge[0] = `"` + strings.Repeat("a", 64) + `"`
	if _, err := Parse([]byte(`{"recipe":{"source":"orders"},"tags":[` + strings.Join(edge, ",") + `]}`)); err != nil {
		t.Errorf("64 bytes and 20 entries must parse: %v", err)
	}
}

// TestTagsAreNotPartOfTheRecipeFingerprint: a tag-only edit must never read as
// a recipe change. If it did, every tag edit would check out a draft, stage an
// identical recipe and rebuild the metric — and on a VERIFIED metric a publish
// would then re-fingerprint a definition nobody changed.
func TestTagsAreNotPartOfTheRecipeFingerprint(t *testing.T) {
	a, err := Parse([]byte(`{"recipe":{"source":"orders"},"tags":["Finance"]}`))
	if err != nil {
		t.Fatal(err)
	}
	b, err := Parse([]byte(`{"recipe":{"source":"orders"},"tags":["Sales","Ops"]}`))
	if err != nil {
		t.Fatal(err)
	}
	ra, _ := a.Resolve("table-orders")
	rb, _ := b.Resolve("table-orders")
	if Fingerprint(ra, a.Description, a.ReportingTimezone) != Fingerprint(rb, b.Description, b.ReportingTimezone) {
		t.Error("the recipe fingerprint moved with the tags")
	}
}

// TestTagsFingerprintFollowsTheFoldedSet: the key a recorded refusal is held
// against. It moves when the SET of names moves — a name added, removed or
// renamed — and not when the file is merely reordered or recased, which the
// server would answer identically.
func TestTagsFingerprintFollowsTheFoldedSet(t *testing.T) {
	base := TagsFingerprint([]string{"Finance", "Sales"})
	if base == "" {
		t.Fatal("empty fingerprint")
	}
	if TagsFingerprint([]string{"sales", " FINANCE"}) != base {
		t.Error("reordering or recasing moved the fingerprint")
	}
	if TagsFingerprint([]string{"Finance"}) == base || TagsFingerprint([]string{"Finance", "Sales", "Ops"}) == base {
		t.Error("a changed set kept the fingerprint")
	}
	// No separator collision: ["a b"] is not ["a", "b"].
	if TagsFingerprint([]string{"a b"}) == TagsFingerprint([]string{"a", "b"}) {
		t.Error("two different sets share a fingerprint")
	}
}
