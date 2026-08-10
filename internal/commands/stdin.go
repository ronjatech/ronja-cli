package commands

import (
	"fmt"
	"io"
	"os"

	"golang.org/x/term"
)

// Reading input that may or may not be there.
//
// The rule this file exists to keep is "never prompt without a TTY", and its
// mirror image, which is the one that actually bites: never BLOCK on a TTY
// either. `ronja api -d @-` and `ronja query` both read stdin, and a read of an
// interactive terminal does not fail — it waits, forever, for an EOF nobody is
// going to type. To an agent driving the CLI that is indistinguishable from a
// hang, which is the single worst failure mode available to a command-line tool.
// So a terminal on stdin is an error, immediately, with the two ways to supply
// the input in the message.

// isTerminal reports whether f is an interactive terminal.
//
// A package var rather than a direct call so tests can reach the branch above
// without allocating a pty. The branch it guards is precisely the one that must
// never regress, and a test that cannot express "attached to a terminal" cannot
// check it.
var isTerminal = func(f *os.File) bool { return term.IsTerminal(int(f.Fd())) }

// readStdin reads all of standard input, refusing when it is a terminal.
// what names the flag that asked for it, so the error says what to change.
func readStdin(what string) ([]byte, error) {
	return readStdinLimit(what, 0)
}

// readStdinLimit is readStdin with a ceiling on how much it will hold; a limit
// of zero or less reads everything.
//
// A pipe is the one input with no size to check in advance, so a caller that
// cannot afford an unbounded read has to bound the read itself — see -d @-,
// where the body is buffered whole before the request goes out.
func readStdinLimit(what string, limit int64) ([]byte, error) {
	if isTerminal(os.Stdin) {
		return nil, fmt.Errorf("%s reads from standard input, but standard input is a terminal — pipe the content in, or point at a file instead", what)
	}
	var src io.Reader = os.Stdin
	if limit > 0 {
		// One byte past the limit, so being AT it and being over it are
		// distinguishable rather than both looking full.
		src = io.LimitReader(src, limit+1)
	}
	body, err := io.ReadAll(src)
	if err != nil {
		return nil, fmt.Errorf("read standard input: %w", err)
	}
	if limit > 0 && int64(len(body)) > limit {
		return nil, fmt.Errorf("%s read more than the %d MiB limit from standard input", what, limit>>20)
	}
	return body, nil
}
