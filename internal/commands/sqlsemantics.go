package commands

import (
	"fmt"
	"io"

	"github.com/ronjatech/ronja-cli/internal/api"
)

// The SQL-semantics leg every sync loop's `status` shares. There is nothing to
// send: the server moves an artifact from legacy to raw on the first write that
// changes its code, so the CLI only names the rows that have not moved yet.

// sqlSemanticsNotice is empty unless semantics is legacy: the server omits the
// field on a raw row, and an older instance omits it entirely.
func sqlSemanticsNotice(name, semantics string) string {
	if semantics != api.SQLSemanticsLegacy {
		return ""
	}
	return fmt.Sprintf("%s: legacy SQL escapes — publishing a change to its code moves it to the current behaviour", name)
}

// sqlSemanticsSubject falls back to the id because an empty title is a real
// state (a data app created by chat and never named).
func sqlSemanticsSubject(name, id string) string {
	if name != "" {
		return name
	}
	return id
}

// printSQLSemanticsLine is called with os.Stderr in every mode: `status` takes
// --json, and a note on stdout would land inside the document a script parses.
func printSQLSemanticsLine(w io.Writer, line string) {
	if line == "" {
		return
	}
	fmt.Fprintf(w, "  Note: %s\n", line)
}
