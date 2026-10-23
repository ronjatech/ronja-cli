package wfdir

import (
	"fmt"
	"path/filepath"
	"strings"

	"github.com/ronjatech/ronja-cli/internal/markers"
)

// The SHAPE rules of a dependency declared by address, as a write target, as
// an output or as a run edge — which kinds each may name, which combinations
// mean nothing, and how a folder write scope's path is normalized. Split from
// dependencies.go, which declares and checks the dependencies themselves.

// ByAddress reports whether the dependency resolves through another folder's
// claim rather than through a bind.
func (d Dependency) ByAddress() bool { return d.From != "" }

// NeedsAddressDepsVersion reports whether the dependency uses a key a CLI older than
// format version 7 reads as something else (see ManifestFormatVersion).
//
// "run" is one of them, and needs no version of its own: no CLI that reads
// version 7 without it was ever released (7 shipped with "run"), so every CLI
// that opens a file stamped 7 knows what "run" means.
func (d Dependency) NeedsAddressDepsVersion() bool {
	return d.From != "" || d.Address != "" || d.Write || d.Output || d.Run
}

// AddressError is why an address dependency did not resolve for a stack: a
// stable reason code (the `sync` vocabulary) and a sentence naming the folder,
// the address and the remedy.
type AddressError struct {
	Reason string
	Detail string
}

// addressKinds are the kinds a dependency may name BY ADDRESS: the kinds a
// folder of this repository manages a row of. A metric is a `table` (its id is
// a table id; see accessDependencies), at its metrics/<stem>.json address.
var addressKinds = map[string]bool{
	markers.KindTable: true, markers.KindWorkflow: true, markers.KindDataApp: true, markers.KindModule: true,
}

// writeKinds are the kinds a write-capable data source may be: a table a
// workflow writes, a codex it may upsert into, a folder write scope — a PATH,
// bound per stack to the path itself (see markers.KindFolder) — and a feature
// an agent automation's reference set names, which the run expands to every
// table of that feature and may write.
var writeKinds = map[string]bool{markers.KindTable: true, markers.KindCodex: true, markers.KindFolder: true, markers.KindFeature: true}

// runKinds are the kinds a run edge may name: what a folder RUNS — a workflow
// or a saved agent.
var runKinds = map[string]bool{markers.KindWorkflow: true, markers.KindAgent: true}

// checkDependencyShape refuses a combination of the address / write / output /
// run keys that means nothing. Acceptance-time, on CheckDeclarations'
// discipline.
func checkDependencyShape(alias string, d Dependency) error {
	switch {
	case d.Run && d.From != "":
		return fmt.Errorf("dependency %q is declared by address and \"run\" — a row this repository manages in the stack is run by address with no flag; \"run\": true declares a row OUTSIDE it, bound per stack", alias)
	case d.Run && (d.Write || d.Output):
		return fmt.Errorf("dependency %q is declared \"run\" and \"write\" or \"output\" — a run edge names a workflow or agent the folder runs; a write source or an output names a table it writes", alias)
	case d.Run && !runKinds[d.Kind]:
		return fmt.Errorf("dependency %q is a %s declared \"run\", and only a workflow or an agent is run", alias, d.Kind)
	case d.Address != "" && d.From == "":
		return fmt.Errorf("dependency %q names an \"address\" and no \"from\" — the address is a key in the folder \"from\" names", alias)
	case d.From != "" && (d.Write || d.Output):
		return fmt.Errorf("dependency %q is declared by address and as a write target — a row another folder manages is read or run by address; one this folder writes is a write-capable data source (\"write\": true) bound per stack", alias)
	case d.Write && d.Output:
		return fmt.Errorf("dependency %q is declared both \"write\" and \"output\" — an output is the folder's own table, written by name; a write-capable data source is somebody else's, bound per stack", alias)
	case d.From != "" && !addressKinds[d.Kind]:
		return fmt.Errorf("dependency %q is a %s declared by address, and only a table, metric (kind \"table\"), workflow, dataapp or module is managed by a folder", alias, d.Kind)
	case d.Write && !writeKinds[d.Kind]:
		return fmt.Errorf("dependency %q is a write-capable %s, and only a table, a codex, a folder or a feature is written through a dependency", alias, d.Kind)
	case d.Kind == markers.KindFolder && !d.Write:
		return fmt.Errorf("dependency %q is a folder, and a folder is declared only as a write scope — add \"write\": true and bind it in each stack to the path that stack may write into", alias)
	case d.Output && d.Kind != markers.KindTable:
		return fmt.Errorf("dependency %q is an output of kind %s, and an output is a table", alias, d.Kind)
	case d.From != "" && (filepath.IsAbs(d.From) || strings.Contains(d.From, "\\")):
		return fmt.Errorf("dependency %q names \"from\": %q — it must be a path relative to this folder, with forward slashes", alias, d.From)
	}
	return nil
}

// NormalizeFolderPath canonicalizes a folder path the way the server normalizes
// a workflow's folder write scope (pymarkers.NormalizeFolder,
// backend/engine/pymarkers/folders.go):
// one leading "/", duplicate slashes collapsed, no trailing "/", a relative
// "reports" read as "/reports", and the root or an empty path answering "".
// A "." or ".." segment is not a path at all (ok false), which the server
// refuses too. Mirrored so a stack's folder bind compares equal to the
// targetPath validate reports for the same folder however it was spelled.
func NormalizeFolderPath(raw string) (string, bool) {
	raw = strings.TrimSpace(raw)
	if raw == "" || raw == "/" {
		return "", true
	}
	segs := make([]string, 0, 8)
	for _, part := range strings.Split(raw, "/") {
		seg := strings.TrimSpace(part)
		if seg == "" {
			continue
		}
		if seg == "." || seg == ".." {
			return "", false
		}
		segs = append(segs, seg)
	}
	if len(segs) == 0 {
		return "", true
	}
	return "/" + strings.Join(segs, "/"), true
}
