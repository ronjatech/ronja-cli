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

// Key decoding is where the fiddly cases are, and a pty is not needed to check
// any of them — which is the point of decodeKey being a pure function.
func TestDecodeKey(t *testing.T) {
	tests := []struct {
		name     string
		in       []byte
		want     keyAction
		wantUsed int
	}{
		{"up arrow", []byte{27, '[', 'A'}, keyUp, 3},
		{"down arrow", []byte{27, '[', 'B'}, keyDown, 3},
		// Left and right are not navigation here; they must not be mistaken for
		// it, or a stray keypress moves the selection.
		{"right arrow ignored", []byte{27, '[', 'C'}, keyNone, 3},
		{"left arrow ignored", []byte{27, '[', 'D'}, keyNone, 3},
		{"enter", []byte{'\r'}, keySelect, 1},
		{"newline", []byte{'\n'}, keySelect, 1},
		{"q", []byte{'q'}, keyCancel, 1},
		{"Q", []byte{'Q'}, keyCancel, 1},
		{"vim up", []byte{'k'}, keyUp, 1},
		{"vim down", []byte{'j'}, keyDown, 1},
		// Raw mode turns OFF ISIG, so Ctrl-C arrives as a byte rather than a
		// signal. If this were not handled the picker could not be escaped at
		// all, and the terminal would be left raw.
		{"ctrl-c", []byte{3}, keyCancel, 1},
		{"ctrl-d", []byte{4}, keyCancel, 1},
		// A lone ESC is the Escape key; the three-byte form above is an arrow.
		{"bare escape", []byte{27}, keyCancel, 1},
		{"unknown letter", []byte{'z'}, keyNone, 1},
		{"empty read", []byte{}, keyNone, 0},
		// A truncated sequence must not be read as its first byte and treated
		// as Escape-means-cancel... it IS ambiguous, and cancelling is the
		// harmless reading, but it must not panic on a short buffer.
		// A burst from key repeat: only the FIRST key is decoded here, and the
		// consumed count is what lets the caller reach the rest.
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

// Key repeat delivers several sequences in ONE read. Decoding only the front
// and discarding the rest means the cursor moves one row however long you hold
// the arrow down — which is the most ordinary way to use a picker.
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

// Clamping rather than wrapping: with three profiles on screen, arrowing past
// the end and landing back on the first is a good way to select something other
// than what you were looking at.
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

// The rule the whole picker is bounded by: without a terminal it must REFUSE,
// not draw itself into a pipe and then block on input nobody is typing. To an
// agent that is indistinguishable from a hang.
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

// ...and the same through the command, which is the path an agent actually
// takes. A bare `profile use` in a script must be an error with the fix in it,
// never a wait.
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

	// The stored current profile must be untouched by a refused pick.
	if current := currentProfile(t); current != "acme" {
		t.Errorf("current = %q, want it unchanged by a refused pick", current)
	}
}

// --json means a machine is reading. It cannot answer a picker, so the refusal
// has to come before anything is drawn — not after escape sequences have been
// emitted into whatever is parsing the output.
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

// Naming a profile explicitly still works and never reaches the picker, which
// is what every script and agent does.
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

// renderItems aligns columns and marks the cursor, and on a redraw walks back
// over exactly as many lines as it wrote — one too few or too many and the list
// smears down the terminal on every keypress.
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
	// Columns line up. Compared in RUNES, not bytes: the cursor marker is a
	// multi-byte glyph, so byte offsets differ between the marked row and the
	// others even when the two are drawn in the same screen column.
	if runeIndex(lines[0], "Acme Retail") != runeIndex(lines[1], "Development") {
		t.Errorf("detail column is not aligned:\n%s", first.String())
	}

	// Non-ASCII in a column that comes from a customer's organization name is
	// the case byte-width padding gets wrong.
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
