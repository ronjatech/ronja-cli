// Package tabledocs is the one contract behind the CLI's two column
// documentation carriers, a pipeline .sql file's `-- @table` / `-- @column`
// header and a folder's docs sidecar, so both decode to the same three-state
// Docs. It never checks a name against a schema: the server joins prose onto
// the measured columns and reports what attached. See cli/README.md.
package tabledocs

import (
	"crypto/sha256"
	"encoding/hex"
	"sort"
	"strings"
)

// Docs is the documentation one carrier declares for one table. Both fields are
// three-state: a nil Description or an absent column key is unmanaged (a push
// sends nothing for it); a present one is a claim, and an empty value clears
// what the row holds.
//
// ⚠️ Only the sidecar can spell the clearing claim: a header `-- @column x:`
// with nothing after the colon is a warning that leaves the column alone (see
// ParseHeader), so deleting a header line unmanages a column, never clears it.
type Docs struct {
	Description *string
	Columns     map[string]string
}

// Empty reports a carrier that declares nothing, such as a .sql file that does
// not opt in.
func (d Docs) Empty() bool { return d.Description == nil && len(d.Columns) == 0 }

// Names is the documented column names, sorted so two checkouts of a folder
// send byte-identical bodies.
func (d Docs) Names() []string {
	out := make([]string, 0, len(d.Columns))
	for name := range d.Columns {
		out = append(out, name)
	}
	sort.Strings(out)
	return out
}

// Fingerprint hashes what this carrier declares, so a sidecar (outside a
// pipeline folder's .sql enumeration) can tell whether the file changed since
// its last push. A file-vs-row comparison would never settle: an unmatched
// name is stored server-side and never reported back.
//
// ⚠️ It is not MetaSHA256 and must never be compared against one: here a
// managed-but-empty claim hashes differently from silence; on a row the two
// are the same.
func (d Docs) Fingerprint() string {
	var b strings.Builder
	if d.Description == nil {
		b.WriteString("description\x00absent\n")
	} else {
		b.WriteString("description\x00present\x00")
		b.WriteString(*d.Description)
		b.WriteString("\x00\n")
	}
	for _, name := range d.Names() {
		b.WriteString("column\x00")
		b.WriteString(name)
		b.WriteString("\x00")
		b.WriteString(d.Columns[name])
		b.WriteString("\x00\n")
	}
	sum := sha256.Sum256([]byte(b.String()))
	return hex.EncodeToString(sum[:])
}

// MetaSHA256 fingerprints a table row's whole documentation state: its
// description plus every column with non-empty prose, sorted by name. It is the
// pipeline drift guard's documentation leg and, like the others, is only ever
// compared against the live row it was taken from (see wfdir.TableState).
//
// ⚠️ It covers the whole row, not the columns the folder manages, so adding a
// `-- @column` line does not read as drift; the cost is that someone else's
// prose on an unmanaged column pauses the next documenting push once.
func MetaSHA256(description string, columns map[string]string) string {
	var b strings.Builder
	// A fixed key, so a description "x" cannot collide with a column named "x".
	b.WriteString("description\x00")
	b.WriteString(description)
	b.WriteString("\x00\n")
	names := make([]string, 0, len(columns))
	for name := range columns {
		if columns[name] != "" {
			names = append(names, name)
		}
	}
	sort.Strings(names)
	for _, name := range names {
		b.WriteString("column\x00")
		b.WriteString(name)
		b.WriteString("\x00")
		b.WriteString(columns[name])
		b.WriteString("\x00\n")
	}
	sum := sha256.Sum256([]byte(b.String()))
	return hex.EncodeToString(sum[:])
}
