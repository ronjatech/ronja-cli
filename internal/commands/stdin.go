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
	return readStdinLimit(what, "", 0)
}

// readStdinLimit is readStdin with a ceiling on how much it will hold; a limit
// of zero or less reads everything.
//
// A pipe is the one input with no size to check in advance, so a caller that
// cannot afford an unbounded read has to bound the read itself — see -d @-,
// where the body is buffered whole before the request goes out.
//
// flag is the flag that would take a path instead ("-d", "-F"), named only in
// the over-limit message: the remedy for a pipe that is too big to hold is
// always the same one, and it is not a limit the caller can raise.
func readStdinLimit(what, flag string, limit int64) ([]byte, error) {
	if isTerminal(os.Stdin) {
		return nil, fmt.Errorf("%s reads from standard input, but standard input is a terminal — pipe the content in, or point at a file instead", what)
	}
	body, over, err := readLimit(os.Stdin, limit)
	if err != nil {
		return nil, fmt.Errorf("read standard input: %w", err)
	}
	if over {
		// "more than", not "exactly": the read is limit+1 bytes, so what is
		// known is that the pipe carried MORE than the cap, and how much more
		// was never measured.
		return nil, fmt.Errorf("%s read more than %d MiB from standard input and stopped: piped content is held in memory so a retry can re-send it, and the CLI caps that at %d MiB. Write it to a file and point %s at the path — a file streams from disk at any size the instance accepts.",
			what, limit>>20, limit>>20, flag)
	}
	return body, nil
}

// readLimit reads src whole and reports whether it carried MORE than limit
// bytes. A limit of zero or less reads everything and never reports over.
//
// Shared rather than inlined, because a pipe is no longer the only unseekable
// input the commands buffer: a path that is readable but not a regular file (a
// FIFO, a process substitution, a character device) is read the same way and
// has to be bounded by the same ceiling. Two copies of "read up to N and notice
// N+1" is two chances for one of them to be an off-by-one.
func readLimit(src io.Reader, limit int64) (body []byte, over bool, err error) {
	if limit > 0 {
		// One byte past the limit, so being AT it and being over it are
		// distinguishable rather than both looking full.
		src = io.LimitReader(src, limit+1)
	}
	body, err = io.ReadAll(src)
	if err != nil {
		return nil, false, err
	}
	return body, limit > 0 && int64(len(body)) > limit, nil
}
