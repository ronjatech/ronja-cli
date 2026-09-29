package checkfile

import (
	"reflect"
	"strings"
	"testing"
)

// shorelinkFixture is the shape of the real Shorelink checks: names with spaces,
// `=`, `+` and å/ä/ö; expressions with quoted literals, `<`, FILTER clauses, a
// date literal and a row constructor; multi-sentence Swedish descriptions; and
// a freshness watchdog.
const shorelinkFixture = `{
  "checks": [
    {
      "name": "Alla rader har konto och kontoklass",
      "expression": "count(*) filter (where account_no is null or statement is null or pnl_group is null and statement = 'resultat') = 0",
      "severity": "fail",
      "description": "Rader utan konto eller kontoklass hamnar utanför resultat- och balansräkningen. Då stämmer inte summorna."
    },
    {
      "name": "Tillgångar = eget kapital + skulder",
      "expression": "abs(sum(amount)) < 1",
      "severity": "fail",
      "description": "Balansräkningen måste gå jämnt ut."
    },
    {
      "name": "Soliditet inom rimligt intervall för ShoreLink AB och Transport",
      "expression": "count(*) filter (where bolagid in (1, 2, 3) and month_start >= date '2022-01-01' and (roce_r12 is null or rorelsemarginal_r12 is null)) = 0",
      "severity": "warn",
      "description": "Nyckeltalen ska finnas för varje månad sedan 2022."
    },
    {
      "name": "Okänd ort är ovanligt",
      "expression": "count(*) filter (where ort = 'Okänd') < 0.15 * count(*)",
      "severity": "warn",
      "description": "Timmar utan ort går inte att fördela per kontor."
    },
    {
      "name": "En rad per bolag, månad och balansgrupp",
      "expression": "count(*) = count(distinct (bolagid, month_start, balance_group))",
      "severity": "fail",
      "description": "Dubbletter betyder oftast att en join har multiplicerat raderna."
    },
    {
      "name": "Bygger om dagligen",
      "expectedIntervalHours": 24,
      "severity": "warn",
      "description": "Tabellen ska byggas om varje dag."
    }
  ]
}
`

// TestShorelinkFixtureRoundTrips: parse → encode reproduces the file byte for
// byte, so a clone of these checks is exactly what an author would have written,
// and a re-parse is the same set.
func TestShorelinkFixtureRoundTrips(t *testing.T) {
	parsed, err := Parse([]byte(shorelinkFixture))
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if len(parsed.Checks) != 6 {
		t.Fatalf("checks = %d", len(parsed.Checks))
	}
	encoded, err := Encode(parsed)
	if err != nil {
		t.Fatalf("encode: %v", err)
	}
	if string(encoded) != shorelinkFixture {
		t.Fatalf("round trip changed the file:\n--- got\n%s\n--- want\n%s", encoded, shorelinkFixture)
	}
	again, err := Parse(encoded)
	if err != nil {
		t.Fatalf("re-parse: %v", err)
	}
	if !reflect.DeepEqual(again, parsed) {
		t.Fatalf("re-parse differs:\n%+v\n%+v", again, parsed)
	}
	if got := parsed.Checks[5]; got.Kind() != KindFreshness || got.ExpectedIntervalHours != 24 {
		t.Errorf("watchdog = %+v", got)
	}
}

// A live check equal to its entry matches, and each declared field that moves
// is named by Differs; an undeclared field that moves is not a difference.
func TestMatchesAndDiffersFollowTheDeclaredFields(t *testing.T) {
	parsed, err := Parse([]byte(`{"checks":[{"name":"Unik rad per id","expression":"count(*) = count(distinct id)","severity":"fail"}]}`))
	if err != nil {
		t.Fatal(err)
	}
	entry := parsed.Checks[0]
	live := Live{Name: "Unik rad per id", Kind: KindExpression, Expression: "count(*) = count(distinct id)", Severity: "fail", Description: "whatever", Enabled: true}
	if !entry.Matches(live) || len(entry.Differs(live)) != 0 {
		t.Fatalf("equal live check reads as different: %v", entry.Differs(live))
	}
	// Undeclared: silencing and rewording in the app are not differences.
	silenced := live
	silenced.Enabled = false
	silenced.Description = "changed"
	if !entry.Matches(silenced) {
		t.Errorf("an undeclared field moving must not be a difference: %v", entry.Differs(silenced))
	}
	moved := live
	moved.Expression = "count(*) > 0"
	moved.Severity = "warn"
	if entry.Matches(moved) {
		t.Error("a declared field moving must be a difference")
	}
	if got := entry.Differs(moved); !reflect.DeepEqual(got, []string{FieldExpression, FieldSeverity}) {
		t.Errorf("differs = %v", got)
	}
}

// The drift question survives the author changing which fields an entry
// declares: a hash recorded through one declared set still recognises the same
// live check after the entry grows a field.
func TestUnmovedAcrossADeclaredSetChange(t *testing.T) {
	before, _ := Parse([]byte(`{"checks":[{"name":"A","expression":"true"}]}`))
	live := Live{Name: "A", Kind: KindExpression, Expression: "true", Severity: "warn", Enabled: true}
	recorded := before.Checks[0].LiveSHA256(live)
	if !Unmoved(live, recorded) {
		t.Fatal("an untouched check reads as moved")
	}
	moved := live
	moved.Expression = "false"
	if Unmoved(moved, recorded) {
		t.Error("an edited expression reads as unmoved")
	}
	// Undeclared at record time, so a UI silence is not movement.
	silenced := live
	silenced.Enabled = false
	if !Unmoved(silenced, recorded) {
		t.Error("a silence of a check whose file does not manage enabled reads as moved")
	}
}

func TestParseRefusals(t *testing.T) {
	cases := map[string]struct{ body, want string }{
		"both bodies":       {`{"checks":[{"name":"A","expression":"true","expectedIntervalHours":24}]}`, "both"},
		"neither body":      {`{"checks":[{"name":"A"}]}`, "needs a body"},
		"folded duplicate":  {`{"checks":[{"name":"Unik rad","expression":"true"},{"name":" unik RAD ","expression":"false"}]}`, "same name as checks[0]"},
		"two watchdogs":     {`{"checks":[{"name":"A","expectedIntervalHours":24},{"name":"B","expectedIntervalHours":1}]}`, "second freshness check"},
		"unknown entry key": {`{"checks":[{"name":"A","expression":"true","owner":"me"}]}`, "owner"},
		"unknown top key":   {`{"checks":[],"verified":true}`, "verified"},
		"missing checks":    {`{}`, "declares no \"checks\""},
		"blank name":        {`{"checks":[{"name":"  ","expression":"true"}]}`, "\"name\" is required"},
		"bad severity":      {`{"checks":[{"name":"A","expression":"true","severity":"error"}]}`, "severity"},
		"zero interval":     {`{"checks":[{"name":"A","expectedIntervalHours":0}]}`, "positive"},
		"string interval":   {`{"checks":[{"name":"A","expectedIntervalHours":"24"}]}`, "not a valid checks file"},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			_, err := Parse([]byte(tc.body))
			if err == nil {
				t.Fatalf("accepted %s", tc.body)
			}
			if !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("error %q does not say %q", err, tc.want)
			}
		})
	}
}

func TestResolveStem(t *testing.T) {
	sql := []string{"gl_lines.sql", "nested/kpi.sql", "a/dup.sql", "b/dup.sql"}
	if got, err := ResolveStem("gl_lines", sql); err != nil || got != "gl_lines.sql" {
		t.Errorf("gl_lines = %q, %v", got, err)
	}
	if got, err := ResolveStem("kpi", sql); err != nil || got != "nested/kpi.sql" {
		t.Errorf("kpi = %q, %v", got, err)
	}
	if _, err := ResolveStem("orders", sql); err == nil || !strings.Contains(err.Error(), "no orders.sql") {
		t.Errorf("a stem with no .sql must be refused: %v", err)
	}
	if _, err := ResolveStem("dup", sql); err == nil || !strings.Contains(err.Error(), "ambiguous") {
		t.Errorf("an ambiguous stem must be refused: %v", err)
	}
}
