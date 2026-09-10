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
