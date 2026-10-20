package commands

import (
	"fmt"
	"io"
	"os"

	"golang.org/x/term"
)

// Never prompt without a TTY, and never block on one: a read of an interactive
// terminal waits forever for an EOF, which to an agent is a hang. So a terminal
// on stdin is an immediate error.

// isTerminal is a package var so tests can reach the terminal branch without a pty.
var isTerminal = func(f *os.File) bool { return term.IsTerminal(int(f.Fd())) }

// readStdin refuses when stdin is a terminal; what names the flag that asked
// for it, so the error says what to change.
func readStdin(what string) ([]byte, error) {
	return readStdinLimit(what, "", 0)
}

// readStdinLimit is readStdin with a ceiling; a limit of zero or less reads
// everything. flag is the flag that would take a path instead ("-d", "-F"),
// named in the over-limit message because a file is the only remedy.
func readStdinLimit(what, flag string, limit int64) ([]byte, error) {
	if isTerminal(os.Stdin) {
		return nil, fmt.Errorf("%s reads from standard input, but standard input is a terminal — pipe the content in, or point at a file instead", what)
	}
	body, over, err := readLimit(os.Stdin, limit)
	if err != nil {
		return nil, fmt.Errorf("read standard input: %w", err)
	}
	if over {
		return nil, fmt.Errorf("%s read more than %d MiB from standard input and stopped: piped content is held in memory so a retry can re-send it, and the CLI caps that at %d MiB. Write it to a file and point %s at the path — a file streams from disk at any size the instance accepts.",
			what, limit>>20, limit>>20, flag)
	}
	return body, nil
}

// readLimit reports whether src carried MORE than limit bytes; a limit of zero
// or less reads everything and never reports over.
func readLimit(src io.Reader, limit int64) (body []byte, over bool, err error) {
	if limit > 0 {
		// One byte past, so being AT the limit is distinguishable from over it.
		src = io.LimitReader(src, limit+1)
	}
	body, err = io.ReadAll(src)
	if err != nil {
		return nil, false, err
	}
	return body, limit > 0 && int64(len(body)) > limit, nil
}
