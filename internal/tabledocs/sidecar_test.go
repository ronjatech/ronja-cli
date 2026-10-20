package tabledocs

import (
	"encoding/json"
	"strings"
	"testing"
)

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

// Where Fingerprint and MetaSHA256 must differ: in a file, absent and empty are
// opposite instructions; on a row they say the same thing.
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

// The join against the table's columns is the server's; the parser must not
// anticipate it.
func TestParseSidecarKeepsUnknownColumnNames(t *testing.T) {
	docs, err := ParseSidecar([]byte(`{"columns":{"renamed_last_week":"still documented"}}`))
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if docs.Columns["renamed_last_week"] != "still documented" {
		t.Fatalf("columns: %v", docs.Columns)
	}
}

// renderSidecar is test-only because nothing in the CLI writes a sidecar; a
// production renderer with no caller would be a second definition of the format.
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
	if MetaSHA256("d", map[string]string{"a": ""}) != MetaSHA256("d", nil) {
		t.Fatal("an empty column description must not move the hash")
	}
	if MetaSHA256("x", nil) == MetaSHA256("", map[string]string{"description": "x"}) {
		t.Fatal("a description and a column named description must not collide")
	}
	if len(base) != 64 {
		t.Fatalf("want lowercase hex sha256, got %q", base)
	}
}
