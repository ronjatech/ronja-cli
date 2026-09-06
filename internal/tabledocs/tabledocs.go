// Package tabledocs is the ONE contract behind both of the CLI's column
// documentation carriers: the opt-in `-- @table` / `-- @column` header a
// pipeline .sql file may open with, and the committed docs sidecar a pipeline
// folder may keep for a table it does not build.
//
// It exists as a package, and a stdlib-only LEAF one, because the two carriers
// must produce the same value: the push path, the drift guard, `status` and
// `sync check` all read a folder's documentation, and a second decode of either
// spelling is a second place for the three-state rule below to be got wrong.
//
// THE THREE-STATE RULE, which is the whole reason this is not a plain map. An
// ABSENT key is UNMANAGED — the folder makes no claim about it and a push sends
// nothing, so a folder that documents three columns of a twelve-column table
// leaves the other nine exactly as they are. A PRESENT key is a claim, and an
// empty one is the claim "there is no prose here", which CLEARS what the row
// holds. The same rule the `ronja automation` loop's curated JSON follows, and
// for the same reason: a committed file is a partial description of a row that
// other people and the agent also write to.
//
// ⚠️ ONLY THE SIDECAR CAN SPELL THE EMPTY CLAIM. Docs is three-state, and the
// sidecar's JSON expresses all three (`"amount": ""` clears), but the HEADER has
// no syntax for it: `-- @column amount:` with nothing after the colon is a
// WARNING and leaves the column alone (see ParseHeader). That is deliberate —
// the line is far more often half-typed than a deliberate clearance, and a
// carrier that read it as one would delete an admin's prose on a typo — but it
// means a .sql file cannot clear what it once wrote. Deleting the `-- @column`
// line UNMANAGES it, which leaves the row as it is; clearing it is done in the
// web app or by the agent.
//
// THE JOIN RULE is the server's half and is not implemented here — the measured
// column set is the truth, prose is joined onto it, and a documented name the
// table does not have is stored, reported and never turned into a column. This
// package therefore never checks a name against a schema: it cannot, it has no
// schema, and a local guess would either refuse a file that is perfectly correct
// against tomorrow's data or claim an attachment the server did not make. What
// attached and what did not is READ BACK from the write, never predicted.
package tabledocs

import (
	"crypto/sha256"
	"encoding/hex"
	"sort"
	"strings"
)

// Docs is the documentation ONE carrier declares for ONE table.
//
// Both fields are three-state, in the two spellings Go has for it:
//
//	Description == nil     the folder does not manage the table's description
//	Description != nil     it does; the pointed-at value is what it should be,
//	                       and "" means "clear it"
//	Columns[name] absent   the folder does not manage that column's prose
//	Columns[name] present  it does; "" means "clear it"
//
// There is deliberately NO declaration-order field. Nothing needs one: the wire
// order is Names()'s, which is sorted, so two checkouts of the same folder send
// byte-identical bodies, and what a push reports back comes from the server
// sorted as well.
type Docs struct {
	Description *string
	Columns     map[string]string
}

// Empty reports a carrier that declares nothing at all, which is the state of
// every .sql file that does not opt in and of every folder with no sidecars.
// It is what the push path branches on: an empty Docs sends no field, arms no
// drift leg and costs no request, so a folder written before any of this existed
// behaves exactly as it did.
func (d Docs) Empty() bool { return d.Description == nil && len(d.Columns) == 0 }

// Names is the documented column names, sorted. Sorted rather than in
// declaration order because it decides the order of the `fields` array on the
// wire, and a stable body is what lets two checkouts of the same folder be
// compared at all.
func (d Docs) Names() []string {
	out := make([]string, 0, len(d.Columns))
	for name := range d.Columns {
		out = append(out, name)
	}
	sort.Strings(out)
	return out
}

// Fingerprint is a hash of what this CARRIER DECLARES — the file's side of the
// sync, not the row's.
//
// It is the sidecar's answer to "has this file changed since we last pushed it",
// which for a .sql file is answered by the file's own content hash. A sidecar
// needs its own because it is not in the folder's enumeration at all: a
// pipeline folder syncs `.sql`, so the baseline that tracks local edits never
// sees these files.
//
// ⚠️ IT IS NOT MetaSHA256 AND MUST NOT BE COMPARED AGAINST ONE. That one is
// taken from a ROW and answers "did the organization move"; this one is taken
// from a FILE and answers "did we move". They are deliberately different
// functions over different inputs, and the three-state rule is the reason they
// cannot be the same one: a MANAGED-but-empty claim ("clear this") is a real
// declaration here and hashes differently from silence, while on a row an empty
// description and an absent one say exactly the same thing.
//
// Comparing the file against the ROW would not do this job. A documented name
// the table does not have is stored server-side and never reported back — that
// is the join rule — so a row-vs-file comparison would report such a sidecar as
// pending for ever and re-send it on every push.
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

// MetaSHA256 is the fingerprint of a TABLE ROW's whole documentation state:
// its description, plus every column that carries prose, sorted by name.
//
// It is the third leg of the pipeline loop's drift guard, and it obeys the
// invariant the other two state (see wfdir.TableState): A HASH IS ONLY EVER
// COMPARED AGAINST THE ROW IT WAS TAKEN FROM. This one is taken from the LIVE
// table and compared against the live table, which is why the recording sites
// are the same three moments the live SQL fingerprint is recorded at.
//
// ⚠️ IT COVERS THE WHOLE ROW, not the subset the folder manages, and that is
// deliberate rather than lazy. The alternative — hashing only the managed names
// — moves with the FOLDER: adding a `-- @column` line changes the set the hash
// is taken over, so the next push would compare a hash of six columns against a
// recording of five and report drift nobody caused. Hashing the row means the
// value only ever moves when the ROW moves, which is the only thing this leg is
// asking about. The cost is that prose written by somebody else on a column this
// folder does not manage also pauses the next documenting push once, with a
// message saying exactly that; --force is the answer, and the agreement is
// re-recorded so it fires once rather than for ever.
//
// A column whose prose is EMPTY contributes nothing, so a table whose columns
// have never been documented hashes identically to one whose prose was cleared.
// That is the honest reading: the row says the same thing in both cases.
func MetaSHA256(description string, columns map[string]string) string {
	var b strings.Builder
	// The description is written under a fixed key rather than concatenated raw,
	// so a description of "x" with no columns cannot collide with a column named
	// "x" and no description.
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
