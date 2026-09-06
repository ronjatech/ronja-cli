package tabledocs

import (
	"strings"
	"testing"
)

// The goldens below are the header's whole contract. Read them as the spec: a
// change that makes one of them fail is a change to what a committed .sql file
// means, which is a change to what a push writes into a customer's table.
func TestParseHeader(t *testing.T) {
	tests := []struct {
		name        string
		sql         string
		description *string
		columns     map[string]string
		warnings    []string
	}{
		{
			name: "opt-out: a leading comment that is not @table is left completely alone",
			sql: "-- joins orders to customers, watch the fanout\n" +
				"-- @column id: this is NOT read, because the block did not opt in\n" +
				"SELECT 1",
		},
		{
			name: "no comment at all",
			sql:  "SELECT 1",
		},
		{
			name:        "table description only",
			sql:         "-- @table One row per invoice line.\nSELECT 1",
			description: ptr("One row per invoice line."),
		},
		{
			name: "continuation lines join with a single space",
			sql: "-- @table One row per invoice line.\n" +
				"--    Amounts are in SEK.\n" +
				"-- A credit note carries a negative quantity.\n" +
				"SELECT 1",
			description: ptr("One row per invoice line. Amounts are in SEK. A credit note carries a negative quantity."),
		},
		{
			name: "columns, with a colon in the text",
			sql: "-- @table Invoice lines.\n" +
				"-- @column invoice_no: the supplier's own number, not Ronja's id\n" +
				"-- @column amount: line total, excluding VAT: always SEK\n" +
				"SELECT 1",
			description: ptr("Invoice lines."),
			columns: map[string]string{
				"invoice_no": "the supplier's own number, not Ronja's id",
				"amount":     "line total, excluding VAT: always SEK",
			},
		},
		{
			name: "a column note continues over several lines",
			sql: "-- @table Invoice lines.\n" +
				"-- @column amount: line total\n" +
				"-- excluding VAT\n" +
				"-- @column qty: negative on a credit note\n" +
				"SELECT 1",
			description: ptr("Invoice lines."),
			columns:     map[string]string{"amount": "line total excluding VAT", "qty": "negative on a credit note"},
		},
		{
			name: "a bare -- ends the documentation; commentary under it is not read",
			sql: "-- @table Invoice lines.\n" +
				"--\n" +
				"-- TODO: the fanout below is deliberate, see ticket 41\n" +
				"-- @column amount: not read either, the block already ended\n" +
				"SELECT 1",
			description: ptr("Invoice lines."),
		},
		{
			name: "leading blank lines are skipped",
			sql:  "\n\n-- @table Invoice lines.\nSELECT 1",
			description: ptr(
				"Invoice lines."),
		},
		{
			name:        "indentation is tolerated",
			sql:         "   -- @table Invoice lines.\n\t--  and more\nSELECT 1",
			description: ptr("Invoice lines. and more"),
		},
		{
			name:        "the block ends at the first non-comment line",
			sql:         "-- @table Invoice lines.\nSELECT 1\n-- @column amount: too late\n",
			description: ptr("Invoice lines."),
		},
		{
			name: "a documented column the table does not have is NOT judged here",
			// The join rule lives on the server: this parser has no schema and
			// must not guess. The name is carried exactly as written and the
			// push reports back what did not attach.
			sql:         "-- @table T.\n-- @column no_such_column: prose about a column that is not in the data\nSELECT 1",
			description: ptr("T."),
			columns:     map[string]string{"no_such_column": "prose about a column that is not in the data"},
		},
		{
			name:        "@column with no colon is dropped and named",
			sql:         "-- @table T.\n-- @column amount the line total\nSELECT 1",
			description: ptr("T."),
			warnings:    []string{"has no colon"},
		},
		{
			name:        "@column with no name is dropped and named",
			sql:         "-- @table T.\n-- @column : the line total\nSELECT 1",
			description: ptr("T."),
			warnings:    []string{"names no column"},
		},
		{
			name:        "@column with no text is dropped and named, and never clears the row",
			sql:         "-- @table T.\n-- @column amount:\nSELECT 1",
			description: ptr("T."),
			warnings:    []string{"has no description after the colon"},
		},
		{
			name:     "a bare @table declares nothing rather than clearing the description",
			sql:      "-- @table\nSELECT 1",
			warnings: []string{"declares no text"},
		},
		{
			name:        "a bare @table with continuation lines does declare one",
			sql:         "-- @table\n-- One row per invoice line.\nSELECT 1",
			description: ptr("One row per invoice line."),
		},
		{
			name:        "a second @table is named and ignored",
			sql:         "-- @table T.\n-- @table U.\nSELECT 1",
			description: ptr("T."),
			warnings:    []string{"appears more than once"},
		},
		{
			name:        "an unknown directive is named, and does not swallow the next column",
			sql:         "-- @table T.\n-- @colunm amount: typo\n-- @column qty: fine\nSELECT 1",
			description: ptr("T."),
			columns:     map[string]string{"qty": "fine"},
			warnings:    []string{"is not a documentation directive"},
		},
		{
			name:        "@tablespace is not @table",
			sql:         "-- @tablespace fast\n-- @table not read\nSELECT 1",
			description: nil,
		},
		{
			name:        "a column documented twice keeps the last line and says so",
			sql:         "-- @table T.\n-- @column amount: first\n-- @column amount: second\nSELECT 1",
			description: ptr("T."),
			columns:     map[string]string{"amount": "second"},
			warnings:    []string{"documented more than once"},
		},
		{
			name:        "CRLF line endings",
			sql:         "-- @table T.\r\n-- @column amount: line total\r\nSELECT 1\r\n",
			description: ptr("T."),
			columns:     map[string]string{"amount": "line total"},
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			docs, warnings := ParseHeader(tc.sql)
			switch {
			case tc.description == nil && docs.Description != nil:
				t.Fatalf("description: want unmanaged, got %q", *docs.Description)
			case tc.description != nil && docs.Description == nil:
				t.Fatalf("description: want %q, got unmanaged", *tc.description)
			case tc.description != nil && *docs.Description != *tc.description:
				t.Fatalf("description: want %q, got %q", *tc.description, *docs.Description)
			}
			if len(docs.Columns) != len(tc.columns) {
				t.Fatalf("columns: want %v, got %v", tc.columns, docs.Columns)
			}
			for name, want := range tc.columns {
				if got, ok := docs.Columns[name]; !ok || got != want {
					t.Fatalf("column %q: want %q, got %q (present=%v)", name, want, got, ok)
				}
			}
			if len(warnings) != len(tc.warnings) {
				t.Fatalf("warnings: want %d %v, got %d %v", len(tc.warnings), tc.warnings, len(warnings), warnings)
			}
			for i, want := range tc.warnings {
				if !strings.Contains(warnings[i], want) {
					t.Fatalf("warning %d: want it to mention %q, got %q", i, want, warnings[i])
				}
			}
		})
	}
}

// TestParseHeaderOptInIsSilent: the opt-out path must produce no warnings at
// all. A folder that never adopts the header has to be able to run every command
// without a single new line of output, or the feature is a tax on everybody who
// did not ask for it.
func TestParseHeaderOptInIsSilent(t *testing.T) {
	docs, warnings := ParseHeader("-- nightly rollup, do not reorder the CTEs\n-- @column x: ignored\nSELECT 1")
	if !docs.Empty() {
		t.Fatalf("want nothing read, got %+v", docs)
	}
	if len(warnings) != 0 {
		t.Fatalf("want no warnings, got %v", warnings)
	}
}

func ptr(s string) *string { return &s }
