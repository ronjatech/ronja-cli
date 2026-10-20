package wfdir

import (
	"path"
	"strings"
)

// A pipeline folder's health checks live in `checks/<stem>.json`, one per
// `.sql` file it builds. Not a `-- @check` header, because a header is part of
// the table's code and every check edit would become a SQL change. The stem
// names a `.sql` file in this folder, not an alias (unlike a docs sidecar).
//
// ⚠️ A pipeline folder's SyncExt is ".sql", so Enumerate never sees these
// files; only the checks pass reads them.
const (
	ChecksDirName = "checks"
	ChecksExt     = ".json"
)

// ChecksPath is the folder-relative path of one stem's checks file.
func ChecksPath(stem string) string {
	return ChecksDirName + "/" + stem + ChecksExt
}

// ChecksStem is the inverse: the `.sql` stem a checks path names, and whether
// the path is a checks file at all. FLAT, for TableDocsAlias's reason.
func ChecksStem(p string) (string, bool) {
	dir, file := path.Split(p)
	if strings.TrimSuffix(dir, "/") != ChecksDirName {
		return "", false
	}
	if !strings.HasSuffix(file, ChecksExt) {
		return "", false
	}
	stem := strings.TrimSuffix(file, ChecksExt)
	if stem == "" {
		return "", false
	}
	return stem, true
}
