package commands

import (
	"os"
	"strings"
	"testing"
	"unicode/utf8"
)

// runeIndex is strings.Index in screen columns rather than bytes.
func runeIndex(haystack, needle string) int {
	i := strings.Index(haystack, needle)
	if i < 0 {
		return -1
	}
	return utf8.RuneCountInString(haystack[:i])
}

func TestDecodeKey(t *testing.T) {
	tests := []struct {
		name     string
		in       []byte
		want     keyAction
		wantUsed int
	}{
		{"up arrow", []byte{27, '[', 'A'}, keyUp, 3},
		{"down arrow", []byte{27, '[', 'B'}, keyDown, 3},
		{"right arrow ignored", []byte{27, '[', 'C'}, keyNone, 3},
		{"left arrow ignored", []byte{27, '[', 'D'}, keyNone, 3},
		{"enter", []byte{'\r'}, keySelect, 1},
		{"newline", []byte{'\n'}, keySelect, 1},
		{"q", []byte{'q'}, keyCancel, 1},
		{"Q", []byte{'Q'}, keyCancel, 1},
		{"vim up", []byte{'k'}, keyUp, 1},
		{"vim down", []byte{'j'}, keyDown, 1},
		// Raw mode turns off ISIG, so Ctrl-C arrives as a byte; unhandled, the
		// picker could not be escaped.
		{"ctrl-c", []byte{3}, keyCancel, 1},
		{"ctrl-d", []byte{4}, keyCancel, 1},
		{"bare escape", []byte{27}, keyCancel, 1},
		{"unknown letter", []byte{'z'}, keyNone, 1},
		{"empty read", []byte{}, keyNone, 0},
		// A burst decodes only its front; the consumed count lets the caller
		// reach the rest. A truncated sequence is consumed whole, not read as
		// Escape.
		{"burst decodes the front", []byte{27, '[', 'B', 27, '[', 'B'}, keyDown, 3},
		{"truncated sequence", []byte{27, '['}, keyNone, 2},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, used := decodeKey(tt.in)
			if got != tt.want {
				t.Errorf("decodeKey(%v) = %v, want %v", tt.in, got, tt.want)
			}
			if used != tt.wantUsed {
				t.Errorf("decodeKey(%v) consumed %d bytes, want %d", tt.in, used, tt.wantUsed)
			}
		})
	}
}

// Key repeat delivers several sequences in one read.
func TestDecodeKeyDrainsABurst(t *testing.T) {
	burst := []byte{27, '[', 'B', 27, '[', 'B', 27, '[', 'B', '\r'}
	var got []keyAction
	for rest := burst; len(rest) > 0; {
		action, used := decodeKey(rest)
		if used == 0 {
			t.Fatal("decodeKey consumed nothing — the caller would spin forever")
		}
		rest = rest[used:]
		got = append(got, action)
	}
	want := []keyAction{keyDown, keyDown, keyDown, keySelect}
	if len(got) != len(want) {
		t.Fatalf("decoded %d keys from the burst, want %d: %v", len(got), len(want), got)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("key %d = %v, want %v", i, got[i], want[i])
		}
	}
}

func TestClampCursor(t *testing.T) {
	tests := []struct{ cursor, delta, n, want int }{
		{0, 1, 3, 1},
		{1, 1, 3, 2},
		{2, 1, 3, 2},  // at the end, stays
		{0, -1, 3, 0}, // at the start, stays
		{1, -1, 3, 0},
		{0, 1, 1, 0}, // a single item has nowhere to go
	}
	for _, tt := range tests {
		if got := clampCursor(tt.cursor, tt.delta, tt.n); got != tt.want {
			t.Errorf("clampCursor(%d, %d, %d) = %d, want %d", tt.cursor, tt.delta, tt.n, got, tt.want)
		}
	}
}

func TestPickerRefusesWithoutATerminal(t *testing.T) {
	restore := isTerminal
	isTerminal = func(*os.File) bool { return false }
	t.Cleanup(func() { isTerminal = restore })

	_, err := pick("Select:", []pickerItem{{Label: "a"}, {Label: "b"}}, 0)
	if err == nil {
		t.Fatal("the picker opened without a terminal; it would have hung")
	}
	if !strings.Contains(err.Error(), "terminal") {
		t.Errorf("error does not name the cause: %v", err)
	}
}

func TestProfileUseWithoutANameNeedsATerminal(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("RONJA_CONFIG_DIR", dir)
	t.Setenv("RONJA_URL", "")
	t.Setenv("RONJA_TOKEN", "")
	t.Setenv("RONJA_PROFILE", "")
	writeProfile(t, "acme", "https://app.ronja.tech", "ten-acme", "tok")

	restore := isTerminal
	isTerminal = func(*os.File) bool { return false }
	t.Cleanup(func() { isTerminal = restore })

	_, err := runCLI(t, t.TempDir(), "profile", "use")
	if err == nil {
		t.Fatal("a bare `profile use` was accepted without a terminal")
	}
	if !strings.Contains(err.Error(), "terminal") {
		t.Errorf("error does not name the cause: %v", err)
	}

	if current := currentProfile(t); current != "acme" {
		t.Errorf("current = %q, want it unchanged by a refused pick", current)
	}
}

// --json means a machine is reading, so the refusal comes before anything is
// drawn.
func TestProfileUseRefusesToPickUnderJSON(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("RONJA_CONFIG_DIR", dir)
	t.Setenv("RONJA_URL", "")
	t.Setenv("RONJA_TOKEN", "")
	t.Setenv("RONJA_PROFILE", "")
	writeProfile(t, "acme", "https://app.ronja.tech", "ten-acme", "tok")

	// A terminal IS present — this refusal is about the flag, not the tty.
	restore := isTerminal
	isTerminal = func(*os.File) bool { return true }
	t.Cleanup(func() { isTerminal = restore })

	_, err := runCLI(t, t.TempDir(), "profile", "use", "--json")
	if err == nil {
		t.Fatal("--json opened a picker")
	}
	if !strings.Contains(err.Error(), "--json") {
		t.Errorf("error does not name the flag: %v", err)
	}
}

func TestProfileUseByNameNeedsNoTerminal(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("RONJA_CONFIG_DIR", dir)
	t.Setenv("RONJA_URL", "")
	t.Setenv("RONJA_TOKEN", "")
	t.Setenv("RONJA_PROFILE", "")
	writeProfile(t, "acme", "https://app.ronja.tech", "ten-acme", "tok")

	restore := isTerminal
	isTerminal = func(*os.File) bool { return false }
	t.Cleanup(func() { isTerminal = restore })

	if _, err := runCLI(t, t.TempDir(), "profile", "use", "acme"); err != nil {
		t.Fatalf("profile use by name: %v", err)
	}
	if current := currentProfile(t); current != "acme" {
		t.Errorf("current = %q, want acme", current)
	}
}

// A redraw must walk back over exactly as many lines as it wrote, or the list
// smears down the terminal.
func TestRenderItemsRedrawsInPlace(t *testing.T) {
	items := []pickerItem{
		{Label: "acme-retail", Detail: "Acme Retail", Note: "app.ronja.tech"},
		{Label: "local", Detail: "Development", Note: "localhost:8080"},
	}
	var first strings.Builder
	renderItems(&first, items, 1, false)
	lines := strings.Split(strings.TrimRight(first.String(), "\n"), "\n")
	if len(lines) != len(items) {
		t.Fatalf("drew %d lines for %d items", len(lines), len(items))
	}
	if strings.Contains(lines[0], "❯") || !strings.Contains(lines[1], "❯") {
		t.Errorf("cursor marker is on the wrong row:\n%s", first.String())
	}
	// Compared in runes: the cursor marker is a multi-byte glyph.
	if runeIndex(lines[0], "Acme Retail") != runeIndex(lines[1], "Development") {
		t.Errorf("detail column is not aligned:\n%s", first.String())
	}

	wide := []pickerItem{
		{Label: "a", Detail: "Ässä Oy", Note: "x"},
		{Label: "b", Detail: "Acme", Note: "y"},
	}
	var third strings.Builder
	renderItems(&third, wide, 0, false)
	wideLines := strings.Split(strings.TrimRight(third.String(), "\n"), "\n")
	if runeIndex(wideLines[0], "x") != runeIndex(wideLines[1], "y") {
		t.Errorf("a non-ASCII organization name knocked the next column out of line:\n%s", third.String())
	}

	var second strings.Builder
	renderItems(&second, items, 0, true)
	if got := strings.Count(second.String(), cursorUp); got != len(items) {
		t.Errorf("redraw moved up %d lines, want %d — the list will smear", got, len(items))
	}
}
