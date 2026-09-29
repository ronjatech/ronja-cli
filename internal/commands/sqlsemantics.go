package commands

import (
	"fmt"
	"io"

	"github.com/ronjatech/ronja-cli/internal/api"
)

// The SQL-SEMANTICS leg every sync loop shares: what `status` says about a row
// still on the old escape handling.
//
// There is nothing to SEND. The server moves an artifact from legacy to raw on
// its own the first time a write changes its code (see api/sqlsemantics.go), so
// no folder declares anything and no push carries a key; the CLI's whole part
// is to name the rows that have not moved yet, so their authors know the next
// code change will reinterpret their backslashes.

// sqlSemanticsNotice is the one line a legacy artifact earns, empty when there
// is nothing to say. `name` is what the reader calls the thing — a workflow's
// title, a table's file path.
//
// Only "legacy" earns a line. The server omits the field on a raw row, and an
// instance older than the field omits it too; neither has anything to report.
func sqlSemanticsNotice(name, semantics string) string {
	if semantics != api.SQLSemanticsLegacy {
		return ""
	}
	return fmt.Sprintf("%s: legacy SQL escapes — publishing a change to its code moves it to the current behaviour", name)
}

// sqlSemanticsSubject is what the notice calls a row: the name a person would
// use, or the id when the row has none. A title is the better half of the
// answer and an empty one is a real state (a data app created by chat and never
// named), which is the whole reason this is not just `row.Title`.
func sqlSemanticsSubject(name, id string) string {
	if name != "" {
		return name
	}
	return id
}

// printSQLSemanticsLine prints an already-composed notice. The four `status`
// commands compute the line where the row is in hand, carry it in the report so
// a --json caller sees the same finding, and print it from the command.
//
// STDERR in every mode, which is a deliberate choice rather than an inherited
// one: `status` takes --json, and a note on stdout would land inside the
// document a script is parsing. Empty says nothing.
func printSQLSemanticsLine(w io.Writer, line string) {
	if line == "" {
		return
	}
	fmt.Fprintf(w, "  Note: %s\n", line)
}
