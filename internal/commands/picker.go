package commands

import (
	"errors"
	"fmt"
	"io"
	"os"
	"strings"
	"unicode/utf8"

	"golang.org/x/term"
)

// An arrow-key list picker, for the one command that has something to pick
// between: `ronja profile use` with no name.
//
// The rule from stdin.go still holds and is if anything stricter here — never
// prompt without a TTY. A picker that drew itself into a pipe would emit escape
// sequences to a machine and then block forever on input nobody is typing,
// which is the worst failure an agent can be handed. Without a terminal this
// refuses immediately and names the non-interactive form.
//
// Rendering goes to STDERR, like every other piece of human narration in this
// CLI, so a command's stdout stays exactly its output.

// errPickerCancelled reports that the human backed out. Callers turn it into a
// quiet exit rather than an error message: cancelling is a choice, not a fault.
var errPickerCancelled = errors.New("cancelled")

// pickerItem is one selectable row. Label is the value; Detail and Note are
// alignment-padded context, and Note is where a warning like "(expired)" goes.
type pickerItem struct {
	Label  string
	Detail string
	Note   string
}

// keyAction is what one keypress means. Extracted so the decoding of terminal
// input — which is where the fiddly cases live — is a pure function a test can
// drive with byte slices instead of a pty.
type keyAction int

const (
	keyNone keyAction = iota
	keyUp
	keyDown
	keySelect
	keyCancel
)

// decodeKey maps the FRONT of a read to an action, and reports how many bytes
// it consumed so the caller can decode the rest.
//
// One read is not one keypress. Key repeat — holding an arrow down to scroll a
// list, which is the most ordinary thing anyone does with a picker — delivers
// several escape sequences in a single read, and so does typing quickly.
// Decoding only the front of the buffer and discarding the remainder drops
// every keypress but the first, so the cursor moves one row however long you
// hold the key.
//
// Arrow keys arrive as a three-byte escape sequence (ESC [ A). A LONE escape is
// the Escape key, and the two are only distinguishable by what follows — so a
// one-byte ESC is treated as cancel, which is what pressing Escape means
// anyway.
//
// Ctrl-C arrives as byte 0x03 rather than a signal: raw mode turns off ISIG, so
// the terminal stops translating it. Handling it here is not an extra — it is
// the ONLY thing standing between the user and a picker they cannot escape.
func decodeKey(buf []byte) (keyAction, int) {
	if len(buf) == 0 {
		return keyNone, 0
	}
	if buf[0] == 27 && len(buf) >= 2 && buf[1] == '[' {
		if len(buf) < 3 {
			// A sequence split across reads. Consuming the whole remainder
			// rather than stalling: the alternative is buffering across reads
			// for a case terminals do not actually produce.
			return keyNone, len(buf)
		}
		switch buf[2] {
		case 'A':
			return keyUp, 3
		case 'B':
			return keyDown, 3
		}
		return keyNone, 3
	}
	switch buf[0] {
	case 27: // a bare Escape
		return keyCancel, 1
	case 3, 4: // Ctrl-C, Ctrl-D
		return keyCancel, 1
	case 'q', 'Q':
		return keyCancel, 1
	case '\r', '\n':
		return keySelect, 1
	case 'k':
		return keyUp, 1
	case 'j':
		return keyDown, 1
	}
	return keyNone, 1
}

// clampCursor moves the cursor and keeps it in range.
//
// Clamping rather than wrapping: with three profiles on screen, arrowing past
// the end and landing back on the first is a good way to select something other
// than what you were looking at.
func clampCursor(cursor, delta, n int) int {
	next := cursor + delta
	if next < 0 {
		return 0
	}
	if next >= n {
		return n - 1
	}
	return next
}

// pick runs the interactive picker and returns the chosen index.
//
// start is where the cursor begins, which callers set to whatever is current —
// so enter alone is a no-op rather than a surprise.
func pick(prompt string, items []pickerItem, start int) (int, error) {
	if len(items) == 0 {
		return 0, errors.New("nothing to choose from")
	}
	if !isTerminal(os.Stdin) {
		return 0, errors.New("this needs a terminal to choose in — name the one you want instead")
	}

	restore, err := makeRaw(os.Stdin)
	if err != nil {
		return 0, fmt.Errorf("put the terminal in raw mode: %w", err)
	}
	// Restores on every ordinary exit INCLUDING cancel, which is why Ctrl-C is
	// decoded as a key rather than left to a signal — a signal would kill the
	// process with the terminal still raw, leaving the user's shell with no
	// echo and no line editing.
	defer restore()

	out := os.Stderr
	fmt.Fprintf(out, "\n  %s\n\n", prompt)
	fmt.Fprint(out, hideCursor)
	defer fmt.Fprint(out, showCursor)

	cursor := clampCursor(start, 0, len(items))
	renderItems(out, items, cursor, false)

	buf := make([]byte, 8)
	for {
		n, err := os.Stdin.Read(buf)
		if err != nil {
			if errors.Is(err, io.EOF) {
				return 0, errPickerCancelled
			}
			return 0, fmt.Errorf("read key: %w", err)
		}
		// Every key in the read, not just the first — see decodeKey. The
		// redraw is deliberately OUTSIDE this loop: holding an arrow down
		// delivers a burst, and redrawing per key would repaint the list
		// several times to show one net movement.
		moved := false
		for rest := buf[:n]; len(rest) > 0; {
			action, used := decodeKey(rest)
			rest = rest[used:]
			switch action {
			case keyUp, keyDown:
				delta := -1
				if action == keyDown {
					delta = 1
				}
				if next := clampCursor(cursor, delta, len(items)); next != cursor {
					cursor = next
					moved = true
				}
			case keySelect:
				return cursor, nil
			case keyCancel:
				return 0, errPickerCancelled
			}
		}
		if moved {
			renderItems(out, items, cursor, true)
		}
	}
}

const (
	hideCursor = "\033[?25l"
	showCursor = "\033[?25h"
	// cursorUp and clearLine are the whole redraw mechanism: walk back over the
	// rows just written and overwrite them in place, so the list does not
	// scroll a new copy of itself on every keypress.
	cursorUp  = "\033[A"
	clearLine = "\r\033[K"
)

// renderItems draws the list, overwriting the previous draw when redraw is set.
func renderItems(out io.Writer, items []pickerItem, cursor int, redraw bool) {
	if redraw {
		for range items {
			fmt.Fprint(out, cursorUp+clearLine)
		}
	}
	labelWidth, detailWidth := 0, 0
	for _, it := range items {
		labelWidth = max(labelWidth, utf8.RuneCountInString(it.Label))
		detailWidth = max(detailWidth, utf8.RuneCountInString(it.Detail))
	}
	for i, it := range items {
		marker := "  "
		if i == cursor {
			marker = "❯ "
		}
		line := "  " + marker + padRight(it.Label, labelWidth) + "  " + padRight(it.Detail, detailWidth)
		if it.Note != "" {
			line += "  " + it.Note
		}
		fmt.Fprintln(out, strings.TrimRight(line, " "))
	}
}

// padRight pads to a width counted in RUNES, not bytes.
//
// fmt's %-*s counts bytes, so an organization called "Ässä Oy" pads two columns
// short and knocks every following column out of line. Rune count is not true
// display width either — a CJK name occupies two columns per rune — but it is
// right for the Latin-with-accents case that actually turns up in a customer
// list, and wrong only cosmetically for the rest.
func padRight(s string, width int) string {
	if pad := width - utf8.RuneCountInString(s); pad > 0 {
		return s + strings.Repeat(" ", pad)
	}
	return s
}

// makeRaw switches the terminal to raw mode and returns its restorer.
//
// A package var for the same reason isTerminal is one: the tests drive the
// picker's decisions, not a pty.
var makeRaw = func(f *os.File) (func(), error) {
	state, err := term.MakeRaw(int(f.Fd()))
	if err != nil {
		return nil, err
	}
	return func() { _ = term.Restore(int(f.Fd()), state) }, nil
}
