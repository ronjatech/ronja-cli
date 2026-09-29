package commands

import (
	"bytes"
	"strings"
	"testing"

	"github.com/ronjatech/ronja-cli/internal/api"
)

// The notice has one speaking state: a row the server reports as legacy. A raw
// row and an instance older than the field both omit it, and both must be
// silent — a line on either would tell an author their code is about to change
// meaning when it is not.
func TestSQLSemanticsNotice(t *testing.T) {
	t.Run("a legacy row names itself and what the next code push does", func(t *testing.T) {
		line := sqlSemanticsNotice("Invoice report", api.SQLSemanticsLegacy)
		want := "Invoice report: legacy SQL escapes — publishing a change to its code moves it to the current behaviour"
		if line != want {
			t.Fatalf("notice = %q, want %q", line, want)
		}
	})

	// Nothing tells the author to SET anything: the server moves the row on
	// its own, and a folder that could declare it would be a second switch.
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

	// The other half, so the silence above is not passing because the printer
	// prints nothing at all.
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

// Advice is narration about a query whose ANSWER is on stdout, so every line of
// it goes to the writer the caller names — stderr — and an instance that sends
// none must produce no output at all.
func TestPrintQueryAdvice(t *testing.T) {
	tests := []struct {
		name   string
		advice []api.QueryAdvice
		want   []string
		// wantLines is how many lines the output must have — one per DISTINCT
		// (kind, message), not one per entry: the server sends an entry per
		// occurrence and their messages are identical within a kind.
		wantLines int
		// absent asserts the output does NOT contain these, which is how the
		// grouping cases prove they collapsed rather than merely printed.
		absent []string
		// silent asserts that nothing at all was written.
		silent bool
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
			// Tolerant of a shape this build has not seen: an entry with no
			// snippet still prints its sentence rather than an empty bracket.
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
			// The reason this renderer groups. The server sends one entry per
			// OCCURRENCE and the message is identical for every occurrence of a
			// kind, so a statement with several doubled backslashes used to
			// print the same sentence once per backslash above the answer the
			// person asked for.
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
			// A repeated literal is one literal. The offsets differ, but the
			// person is being sent to the same text.
			name: "a repeated literal is named once",
			advice: []api.QueryAdvice{
				{Kind: "doubled_backslash", Message: "a doubled backslash", Snippet: `'[\\s]'`, Offset: 10},
				{Kind: "doubled_backslash", Message: "a doubled backslash", Snippet: `'[\\s]'`, Offset: 55},
			},
			want:      []string{`advice: a doubled backslash (at '[\\s]')`},
			wantLines: 1,
		},
		{
			// Two kinds stay two lines: the sentences differ, and merging them
			// would print one and lose the other.
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
			// An instance older than `kind`, or one that grew a kind this build
			// has never seen, sends entries this renderer cannot key on kind
			// alone. Grouping on (kind, message) keeps two different sentences
			// apart rather than printing the first and swallowing the second.
			name: "two unknown-kind sentences are not merged",
			advice: []api.QueryAdvice{
				{Message: "first sentence", Snippet: "'a'"},
				{Message: "second sentence", Snippet: "'b'"},
			},
			want:      []string{"advice: first sentence (at 'a')", "advice: second sentence (at 'b')"},
			wantLines: 2,
		},
		{
			// The floor under an instance that sends more than the server's own
			// per-kind cap. The count is on the line because a bounded list
			// read as a complete one is how somebody fixes five of seven.
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
			// A kind whose entries carry no snippet at all still says its
			// sentence once, with no empty bracket after it.
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
			// Nothing a person could read, so nothing is said. The alternative is
			// a bare "advice:" line that tells the reader only that something is
			// wrong with the CLI.
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
