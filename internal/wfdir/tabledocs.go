package wfdir

import (
	"path"
	"strings"
)

// A DOCS SIDECAR is a committed file in a pipeline folder that documents a table
// the folder does NOT build: an integration table, a foundation table, a dynamic
// one, one a workflow writes. It carries the same three-state
// `{description, columns}` shape the header carries (see internal/tabledocs) and
// nothing else.
//
// WHY IT LIVES IN THE PIPELINE FOLDER rather than in a loop of its own. Column
// prose matters most on exactly the tables a pipeline folder does not build, and
// those tables have no folder anywhere — but everything a sync loop needs to
// reach them already exists here: ronja.json's `dependencies` name a table this
// folder does not own, each stack's `bind` map says which of THIS organization's
// rows answers to that name, and `ronja bind` reconciles the two. A second
// top-level command would need its own manifest, its own lock, its own bind map
// and its own tree walk, all of them second copies of these.
//
// THE LAYOUT: `tables/<alias>.json`, beside the folder's .sql files.
//
//	orders_by_month.sql          a table this folder BUILDS  (documented by its
//	                             own `-- @table` header, in the SQL)
//	tables/fortnox_invoices.json a table it does NOT build   (documented here)
//
// The stem is an ALIAS — a name declared in ronja.json's `dependencies` with
// kind `table` and bound per stack — which is what makes one committed folder
// deployable to several organizations: the file says `fortnox_invoices`, and
// each stack's bind map says which id that is there. A literal `table-…` id is
// accepted too, exactly as it is everywhere else in the alias layer, and means
// the folder can only ever be pushed to the organization that minted it.
//
// The two carriers must be DISJOINT — one table, one carrier — and that is
// enforced on the ID a sidecar resolves to, not on its name: see the builtBy
// check in resolveTableDocsSidecars. CheckAliasCollisions is a different rule
// about a different thing (a declared alias NAME against a sibling .sql stem)
// and cannot do this job: a sidecar named after a literal `table-…` id, or an
// alias whose bind value happens to be a built table's id, collides with no stem
// at all and would otherwise document one table twice — the header onto the
// draft, the sidecar onto the live row.
//
// ⚠️ A pipeline folder's SyncExt is ".sql", so these files are invisible to
// Enumerate — never pushed as SQL, and never reported as skipped. They are read
// by the docs pass alone.
const (
	// TableDocsDirName is the directory a pipeline folder keeps its docs
	// sidecars in, relative to the folder root.
	TableDocsDirName = "tables"
	// TableDocsExt is the extension one carries.
	TableDocsExt = ".json"
)

// TableDocsPath is the folder-relative path of one alias's docs sidecar, in the
// slash-separated spelling every map in this package is keyed by.
func TableDocsPath(alias string) string {
	return TableDocsDirName + "/" + alias + TableDocsExt
}

// TableDocsAlias is the inverse: the alias a sidecar path names, and whether the
// path is a sidecar at all.
//
// FLAT, deliberately: `tables/a/b.json` is not a sidecar. The stem is an alias,
// and an alias is a single name — allowing a nested one would invent a second
// spelling for it (`a/b`) that ronja.json's dependency map has no way to
// express, so the file would be committed, never read, and never reported.
func TableDocsAlias(p string) (string, bool) {
	dir, file := path.Split(p)
	if strings.TrimSuffix(dir, "/") != TableDocsDirName {
		return "", false
	}
	if !strings.HasSuffix(file, TableDocsExt) {
		return "", false
	}
	alias := strings.TrimSuffix(file, TableDocsExt)
	if alias == "" {
		return "", false
	}
	return alias, true
}
