package tabledocs

import "testing"

func TestRewriteHeaderReplacesOnlyDeclaredValues(t *testing.T) {
	in := "-- @table One row per order.\n" +
		"-- Refreshed nightly.\n" +
		"-- @column amount: Line total.\n" +
		"-- @column id: The order id.\n" +
		"-- @name Orders\n" +
		"--\n" +
		"-- @column note: below the bare line, commentary\n" +
		"SELECT 1"
	desc, name := "One row per shipped order.", "Shipped orders"
	got := RewriteHeader(in, &name, Docs{Description: &desc, Columns: map[string]string{"amount": "Total, SEK.", "ghost": "not declared"}})
	want := "-- @table One row per shipped order.\n" +
		"-- @column amount: Total, SEK.\n" +
		"-- @column id: The order id.\n" +
		"-- @name Shipped orders\n" +
		"--\n" +
		"-- @column note: below the bare line, commentary\n" +
		"SELECT 1"
	if got != want {
		t.Fatalf("got\n%s\nwant\n%s", got, want)
	}
	docs, _ := ParseHeader(got)
	if *docs.Description != desc || docs.Columns["amount"] != "Total, SEK." || docs.Columns["id"] != "The order id." || len(docs.Columns) != 2 {
		t.Fatalf("does not read back: %+v", docs)
	}
	if n, _ := ParseName(got); n == nil || *n != name {
		t.Fatalf("name does not read back: %v", n)
	}
}

func TestRewriteHeaderLeavesAFileWithNoHeaderAlone(t *testing.T) {
	for _, in := range []string{"SELECT 1", "-- joins orders to customers\nSELECT 1", ""} {
		desc := "x"
		if got := RewriteHeader(in, &desc, Docs{Description: &desc}); got != in {
			t.Fatalf("%q became %q", in, got)
		}
	}
}

func TestRewriteHeaderDropsAnEmptiedColumn(t *testing.T) {
	in := "-- @table T.\n-- @column a: old\n-- continued\n-- @column b: keep\nSELECT 1"
	got := RewriteHeader(in, nil, Docs{Columns: map[string]string{"a": ""}})
	if want := "-- @table T.\n-- @column b: keep\nSELECT 1"; got != want {
		t.Fatalf("got %q, want %q", got, want)
	}
}

func TestRewriteHeaderNameOnlyHeader(t *testing.T) {
	in := "  -- @name old\nSELECT 1"
	name := "new"
	if got, want := RewriteHeader(in, &name, Docs{}), "  -- @name new\nSELECT 1"; got != want {
		t.Fatalf("got %q, want %q", got, want)
	}
}
