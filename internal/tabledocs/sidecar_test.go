package tabledocs

import (
	"encoding/json"
	"strings"
	"testing"
)

// TestParseSidecarThreeState is the rule the whole carrier rests on: an ABSENT
// key is UNMANAGED. A folder that documents three columns of a twelve-column
// table must leave the other nine exactly as they are, and a file with no
// "description" key must not clear the one the row holds.
func TestParseSidecarThreeState(t *testing.T) {
	docs, err := ParseSidecar([]byte(`{"columns":{"amount":"line total"}}`))
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if docs.Description != nil {
		t.Fatalf("an absent description must be unmanaged, got %q", *docs.Description)
	}
	if len(docs.Columns) != 1 || docs.Columns["amount"] != "line total" {
		t.Fatalf("columns: %v", docs.Columns)
	}
	if _, managed := docs.Columns["qty"]; managed {
		t.Fatal("a column the file never names must not be managed")
	}
	// Fingerprint is the "has this FILE changed" question, and an unmanaged key
	// is not part of it: a file that documents `amount` says the same thing
	// however much prose other people write on `qty`.
	same, err := ParseSidecar([]byte(`{"columns":{"amount":"line total"}}`))
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if same.Fingerprint() != docs.Fingerprint() {
		t.Fatal("the same declaration must fingerprint the same")
	}
	changed, err := ParseSidecar([]byte(`{"columns":{"amount":"line total","qty":"count"}}`))
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if changed.Fingerprint() == docs.Fingerprint() {
		t.Fatal("documenting another column must move the fingerprint")
	}
}

// TestFingerprintSeparatesAbsentFromEmpty is the three-state rule made
// checkable: a MANAGED-but-empty claim ("clear this") is a declaration, and it
// must not hash the same as saying nothing.
//
// This is exactly where Fingerprint and MetaSHA256 have to differ. On a ROW an
// empty description and an absent one say the same thing, so MetaSHA256 folds
// them together; in a FILE they are opposite instructions.
func TestFingerprintSeparatesAbsentFromEmpty(t *testing.T) {
	absent, err := ParseSidecar([]byte(`{"columns":{"amount":"x"}}`))
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	empty, err := ParseSidecar([]byte(`{"description":"","columns":{"amount":"x"}}`))
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if absent.Fingerprint() == empty.Fingerprint() {
		t.Fatal("an absent description and an explicit empty one are different declarations")
	}
	if MetaSHA256("", nil) != MetaSHA256("", map[string]string{}) {
		t.Fatal("a ROW's fingerprint has no such distinction to make")
	}
}

// TestParseSidecarExplicitEmptyClears: present-but-empty is a CLAIM, and the
// claim is "there is no prose here". The distinction from absent is the only
// way a folder can ever take a description back.
func TestParseSidecarExplicitEmptyClears(t *testing.T) {
	docs, err := ParseSidecar([]byte(`{"description":"","columns":{"amount":"","qty":null}}`))
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if docs.Description == nil || *docs.Description != "" {
		t.Fatalf("an explicit empty description must be managed and empty, got %v", docs.Description)
	}
	for _, name := range []string{"amount", "qty"} {
		text, managed := docs.Columns[name]
		if !managed || text != "" {
			t.Fatalf("%s: want managed and empty, got %q (managed=%v)", name, text, managed)
		}
	}
}

func TestParseSidecarRefusals(t *testing.T) {
	tests := []struct {
		name, body, want string
	}{
		{"unknown key", `{"name":"orders"}`, "not a valid table-documentation file"},
		{"a schema is not documentation", `{"columns":{"a":"x"},"type":"integration"}`, "not a valid table-documentation file"},
		{"not json", `nope`, "not a valid table-documentation file"},
		{"two documents", `{"description":"a"}{"description":"b"}`, "more than one JSON document"},
		{"blank column name", `{"columns":{"  ":"x"}}`, "blank name"},
		{"padded column name", `{"columns":{" amount":"x"}}`, "whitespace around it"},
		{"columns is not an object", `{"columns":["amount"]}`, "not a valid table-documentation file"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if _, err := ParseSidecar([]byte(tc.body)); err == nil {
				t.Fatal("want a refusal, got none")
			} else if !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("want the refusal to mention %q, got %v", tc.want, err)
			}
		})
	}
}

// TestParseSidecarKeepsUnknownColumnNames: the JOIN RULE is the server's, and
// this parser must not anticipate it. A name the table does not have is carried
// exactly as written; the push reports it back as unmatched.
func TestParseSidecarKeepsUnknownColumnNames(t *testing.T) {
	docs, err := ParseSidecar([]byte(`{"columns":{"renamed_last_week":"still documented"}}`))
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if docs.Columns["renamed_last_week"] != "still documented" {
		t.Fatalf("columns: %v", docs.Columns)
	}
}

// renderSidecar writes a Docs back out in the sidecar's own shape.
//
// TEST-ONLY, and it lives here rather than beside ParseSidecar because nothing
// in the CLI writes one of these files: the sidecar is authored by a person and
// only ever read. An exported renderer with no caller is a second definition of
// the on-disk format waiting to drift from the one that matters.
func renderSidecar(d Docs) ([]byte, error) {
	file := sidecarFile{Description: d.Description}
	if len(d.Columns) > 0 {
		file.Columns = make(map[string]string, len(d.Columns))
		for name, text := range d.Columns {
			file.Columns[name] = text
		}
	}
	out, err := json.MarshalIndent(file, "", "  ")
	if err != nil {
		return nil, err
	}
	return append(out, '\n'), nil
}

func TestRenderSidecarRoundTrips(t *testing.T) {
	body := []byte("{\"description\":\"One row per invoice.\",\"columns\":{\"amount\":\"line total\"}}")
	docs, err := ParseSidecar(body)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	out, err := renderSidecar(docs)
	if err != nil {
		t.Fatalf("render: %v", err)
	}
	again, err := ParseSidecar(out)
	if err != nil {
		t.Fatalf("re-parse: %v", err)
	}
	if *again.Description != *docs.Description || again.Columns["amount"] != docs.Columns["amount"] {
		t.Fatalf("round trip lost content: %+v", again)
	}
}

// TestMetaSHA256 pins the third drift leg's fingerprint. It is taken from a ROW,
// compared against that row, and must not move for reasons the row did not.
func TestMetaSHA256(t *testing.T) {
	base := MetaSHA256("One row per invoice.", map[string]string{"amount": "line total", "qty": "count"})
	if base != MetaSHA256("One row per invoice.", map[string]string{"qty": "count", "amount": "line total"}) {
		t.Fatal("map order must not move the hash")
	}
	if base == MetaSHA256("One row per invoice.", map[string]string{"amount": "line total"}) {
		t.Fatal("dropping a column's prose must move the hash")
	}
	if base == MetaSHA256("changed", map[string]string{"amount": "line total", "qty": "count"}) {
		t.Fatal("changing the description must move the hash")
	}
	// An empty column description contributes nothing: a column that has never
	// been documented and one whose prose was cleared say the same thing.
	if MetaSHA256("d", map[string]string{"a": ""}) != MetaSHA256("d", nil) {
		t.Fatal("an empty column description must not move the hash")
	}
	// The description is written under its own key, so it cannot collide with a
	// column that happens to be named after it.
	if MetaSHA256("x", nil) == MetaSHA256("", map[string]string{"description": "x"}) {
		t.Fatal("a description and a column named description must not collide")
	}
	if len(base) != 64 {
		t.Fatalf("want lowercase hex sha256, got %q", base)
	}
}
