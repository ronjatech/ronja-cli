package commands

import (
	"fmt"
	"os"

	"github.com/ronjatech/ronja-cli/internal/wfdir"
)

// LIVE EQUALS DESIRED IS NEVER DRIFT — the one rule every drift guard in the
// folder loops shares, in the one place the draft kinds read it from.
//
// A drift guard exists to stop a push overwriting work it has not seen. When
// the server already holds exactly what the push would leave there, there is
// nothing to overwrite, so refusing protects nothing — and it refuses the
// folder's OWN work in the case the rule is for: the anchor (a local baseline,
// or the committed head pointer) is written in a separate step after a push or
// a publish, so a crash, a lost lock commit or a discarded CI checkout leaves it
// behind a server this same repository wrote. The guard then names the
// folder's own publish as somebody else's change.
//
// What counts as "would leave there" is the push's own write rule: a file whose
// remote bytes equal the local ones is not written, a remote file absent locally
// is deleted, and a local file absent remotely is written. So a drifted path is
// excused when the remote holds the local bytes, or when neither side holds the
// file at all.

// excuseAgreedFiles drops from a drift diff every path where the server already
// holds what the push would leave there, and reports how many it dropped.
func excuseAgreedFiles(drift wfdir.Diff, remote, local map[string]string) (wfdir.Diff, int) {
	agreed := func(path string) bool {
		r, onRemote := remote[path]
		l, onLocal := local[path]
		return onRemote == onLocal && r == l
	}
	out := wfdir.Diff{Added: []string{}, Modified: []string{}, Deleted: []string{}, Unchanged: drift.Unchanged}
	excused := 0
	keep := func(into *[]string, paths []string) {
		for _, path := range paths {
			if agreed(path) {
				excused++
				continue
			}
			*into = append(*into, path)
		}
	}
	keep(&out.Added, drift.Added)
	keep(&out.Modified, drift.Modified)
	keep(&out.Deleted, drift.Deleted)
	return out, excused
}

// filesAgree reports that a row holds exactly a folder's files: the same paths,
// and the same bytes at each.
func filesAgree(remote, local map[string]string) bool {
	if len(remote) != len(local) {
		return false
	}
	for path, content := range remote {
		if l, ok := local[path]; !ok || l != content {
			return false
		}
	}
	return true
}

// noteLiveEqualsDesired says why a push with no baseline to hold it to was let
// through: the row already is this folder.
func noteLiveEqualsDesired(rowID string) {
	fmt.Fprintf(os.Stderr, "  Note: this folder has no sync baseline here, but %s already holds exactly its files and metadata — there is nothing to overwrite, so it is treated as this folder's own state.\n",
		rowID)
}
