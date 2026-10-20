package commands

import (
	"bytes"
	"strings"
	"testing"

	"github.com/ronjatech/ronja-cli/internal/api"
)

func TestSQLSemanticsNotice(t *testing.T) {
	t.Run("a legacy row names itself and what the next code push does", func(t *testing.T) {
		line := sqlSemanticsNotice("Invoice report", api.SQLSemanticsLegacy)
		want := "Invoice report: legacy SQL escapes — publishing a change to its code moves it to the current behaviour"
		if line != want {
			t.Fatalf("notice = %q, want %q", line, want)
		}
	})

	// The server moves the row on its own; a folder declaration would be a second switch.
	t.Run("the notice asks for no declaration", func(t *testing.T) {
		line := sqlSemanticsNotice("tables/invoices.sql", api.SQLSemanticsLegacy)
		for _, banned := range []string{"sqlSemantics", "ronja.json", "@sqlSemantics", "set "} {
			if strings.Contains(line, banned) {
				t.Errorf("notice = %q, want it NOT to contain %q", line, banned)
			}
		}
	})

	t.Run("a row the server reports nothing for says nothing", func(t *testing.T) {
		for _, row := range []string{"", "raw"} {
			if line := sqlSemanticsNotice("Invoice report", row); line != "" {
				t.Errorf("notice(%q) = %q, want none", row, line)
			}
		}
	})

	t.Run("nothing to say prints nothing", func(t *testing.T) {
		var buf bytes.Buffer
		printSQLSemanticsLine(&buf, sqlSemanticsNotice("Invoice report", ""))
		if buf.Len() != 0 {
			t.Fatalf("wrote %q, want nothing", buf.String())
		}
	})

	// Negative control for the silent case above.
	t.Run("something to say is written to the writer the caller named", func(t *testing.T) {
		var buf bytes.Buffer
		printSQLSemanticsLine(&buf, sqlSemanticsNotice("Invoice report", api.SQLSemanticsLegacy))
		got := buf.String()
		for _, want := range []string{"  Note: ", "Invoice report", "legacy SQL escapes"} {
			if !strings.Contains(got, want) {
				t.Errorf("wrote %q, want it to contain %q", got, want)
			}
		}
		if lines := strings.Count(got, "\n"); lines != 1 {
			t.Errorf("wrote %q, want exactly one line, got %d", got, lines)
		}
	})
}

func TestPrintQueryAdvice(t *testing.T) {
	tests := []struct {
		name   string
		advice []api.QueryAdvice
		want   []string
		// wantLines is one per DISTINCT (kind, message): the server sends an
		// entry per occurrence, with one message per kind.
		wantLines int
		absent    []string
		silent    bool
	}{
		{
			name:   "an instance older than the field sends none",
			advice: nil,
			silent: true,
		},
		{
			name:   "an empty array is the ordinary case",
			advice: []api.QueryAdvice{},
			silent: true,
		},
		{
			name: "a message and a snippet",
			advice: []api.QueryAdvice{{
				Kind:    "backslash_escape",
				Message: `'\s' is a backslash escape`,
				Snippet: `'\s'`,
				Offset:  42,
			}},
			want:      []string{"advice:", `'\s' is a backslash escape`, `'\s'`},
			wantLines: 1,
		},
		{
			name:      "a message with no snippet",
			advice:    []api.QueryAdvice{{Message: "this statement mixes escape conventions"}},
			want:      []string{"advice: this statement mixes escape conventions"},
			wantLines: 1,
		},
		{
			name: "several entries print one line each",
			advice: []api.QueryAdvice{
				{Message: "first"},
				{Message: "second"},
			},
			want:      []string{"advice: first", "advice: second"},
			wantLines: 2,
		},
		{
			name: "every occurrence of one kind collapses onto one line naming each literal",
			advice: []api.QueryAdvice{
				{Kind: "doubled_backslash", Message: "a doubled backslash", Snippet: `'[\\s]'`, Offset: 10},
				{Kind: "doubled_backslash", Message: "a doubled backslash", Snippet: `'[\\d]'`, Offset: 40},
				{Kind: "doubled_backslash", Message: "a doubled backslash", Snippet: `'[\\w]'`, Offset: 70},
			},
			want:      []string{`advice: a doubled backslash (at '[\\s]', '[\\d]', '[\\w]')`},
			wantLines: 1,
		},
		{
			name: "a repeated literal is named once",
			advice: []api.QueryAdvice{
				{Kind: "doubled_backslash", Message: "a doubled backslash", Snippet: `'[\\s]'`, Offset: 10},
				{Kind: "doubled_backslash", Message: "a doubled backslash", Snippet: `'[\\s]'`, Offset: 55},
			},
			want:      []string{`advice: a doubled backslash (at '[\\s]')`},
			wantLines: 1,
		},
		{
			name: "two kinds are two lines",
			advice: []api.QueryAdvice{
				{Kind: "doubled_backslash", Message: "a doubled backslash", Snippet: `'[\\s]'`},
				{Kind: "escape_in_plain_literal", Message: "an escape in a plain literal", Snippet: `'\n'`},
			},
			want: []string{
				`advice: a doubled backslash (at '[\\s]')`,
				`advice: an escape in a plain literal (at '\n')`,
			},
			wantLines: 2,
		},
		{
			// An older instance sends no kind, so grouping must key on
			// (kind, message), not kind alone.
			name: "two unknown-kind sentences are not merged",
			advice: []api.QueryAdvice{
				{Message: "first sentence", Snippet: "'a'"},
				{Message: "second sentence", Snippet: "'b'"},
			},
			want:      []string{"advice: first sentence (at 'a')", "advice: second sentence (at 'b')"},
			wantLines: 2,
		},
		{
			// The count is on the line because a bounded list read as complete
			// is how somebody fixes five of seven.
			name: "more literals than the line holds are counted, not dropped silently",
			advice: []api.QueryAdvice{
				{Kind: "doubled_backslash", Message: "a doubled backslash", Snippet: "'1'"},
				{Kind: "doubled_backslash", Message: "a doubled backslash", Snippet: "'2'"},
				{Kind: "doubled_backslash", Message: "a doubled backslash", Snippet: "'3'"},
				{Kind: "doubled_backslash", Message: "a doubled backslash", Snippet: "'4'"},
				{Kind: "doubled_backslash", Message: "a doubled backslash", Snippet: "'5'"},
				{Kind: "doubled_backslash", Message: "a doubled backslash", Snippet: "'6'"},
				{Kind: "doubled_backslash", Message: "a doubled backslash", Snippet: "'7'"},
			},
			want:      []string{"'1', '2', '3', '4', '5' and 2 more"},
			absent:    []string{"'6'", "'7'"},
			wantLines: 1,
		},
		{
			name: "a kind with no snippets prints its sentence once",
			advice: []api.QueryAdvice{
				{Kind: "statement_wide", Message: "this statement mixes escape conventions"},
				{Kind: "statement_wide", Message: "this statement mixes escape conventions"},
			},
			want:      []string{"advice: this statement mixes escape conventions"},
			absent:    []string{"(at"},
			wantLines: 1,
		},
		{
			name:   "an entry with no message is dropped",
			advice: []api.QueryAdvice{{Kind: "backslash_escape", Snippet: `'\s'`}},
			silent: true,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var buf bytes.Buffer
			printQueryAdvice(&buf, tt.advice)
			if tt.silent {
				if buf.Len() != 0 {
					t.Fatalf("wrote %q, want nothing", buf.String())
				}
				return
			}
			got := buf.String()
			for _, want := range tt.want {
				if !strings.Contains(got, want) {
					t.Errorf("advice output = %q, want it to contain %q", got, want)
				}
			}
			for _, absent := range tt.absent {
				if strings.Contains(got, absent) {
					t.Errorf("advice output = %q, want it NOT to contain %q", got, absent)
				}
			}
			if lines := strings.Count(got, "\n"); lines != tt.wantLines {
				t.Errorf("advice output = %q, want %d line(s), got %d", got, tt.wantLines, lines)
			}
		})
	}
}
