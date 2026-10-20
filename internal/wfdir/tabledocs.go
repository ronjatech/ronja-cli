package wfdir

import (
	"path"
	"strings"
)

// A docs sidecar, `tables/<alias>.json` in a pipeline folder, documents a table
// the folder does NOT build with the `{description, columns}` shape of a
// `-- @table` header (see internal/tabledocs); the design is in cli/README.md,
// "Documentation — two carriers, one contract".
//
// The header and the sidecar must be disjoint, and that is enforced on the id a
// sidecar resolves to (the builtBy check in resolveTableDocsSidecars), not on
// its name: a literal-id sidecar collides with no .sql stem, so
// CheckAliasCollisions cannot catch it.
//
// ⚠️ SyncExt ".sql" makes these files invisible to Enumerate; the docs pass
// alone reads them.
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
// Flat, deliberately: `tables/a/b.json` is not a sidecar, because a nested stem
// is an alias spelling ronja.json's dependency map cannot express, so the file
// would be committed, never read and never reported.
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
