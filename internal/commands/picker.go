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

// Never prompt without a TTY: a picker drawn into a pipe would block forever on
// input nobody types, which to an agent is a hang. Rendering goes to stderr.

// errPickerCancelled is turned into a quiet exit by callers: cancelling is a
// choice, not a fault.
var errPickerCancelled = errors.New("cancelled")

type pickerItem struct {
	Label  string
	Detail string
	Note   string
}

type keyAction int

const (
	keyNone keyAction = iota
	keyUp
	keyDown
	keySelect
	keyCancel
)

// decodeKey maps the FRONT of a read to an action and reports the bytes used:
// key repeat delivers several sequences in one read, and the caller must decode
// them all. Raw mode turns off ISIG, so Ctrl-C arrives as 0x03 and this is the
// only way out of the picker.
func decodeKey(buf []byte) (keyAction, int) {
	if len(buf) == 0 {
		return keyNone, 0
	}
	if buf[0] == 27 && len(buf) >= 2 && buf[1] == '[' {
		if len(buf) < 3 {
			// Split across reads: consumed whole rather than buffered, since
			// terminals do not produce it.
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

// clampCursor clamps rather than wraps, so arrowing past the end never lands on
// a row you were not looking at.
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
	// Restores on every exit including cancel; a signal would kill the process
	// with the terminal still raw, which is why Ctrl-C is decoded as a key.
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
		// The redraw is deliberately outside this loop: a key-repeat burst
		// would otherwise repaint once per key.
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
	cursorUp   = "\033[A"
	clearLine  = "\r\033[K"
)

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

// padRight counts runes, not bytes (fmt's %-*s), so "Ässä Oy" aligns. Runes
// are not display width (CJK is two columns), but that is only cosmetic.
func padRight(s string, width int) string {
	if pad := width - utf8.RuneCountInString(s); pad > 0 {
		return s + strings.Repeat(" ", pad)
	}
	return s
}

// makeRaw is a package var, like isTerminal, so tests need no pty.
var makeRaw = func(f *os.File) (func(), error) {
	state, err := term.MakeRaw(int(f.Fd()))
	if err != nil {
		return nil, err
	}
	return func() { _ = term.Restore(int(f.Fd()), state) }, nil
}
