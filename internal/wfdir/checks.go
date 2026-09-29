package wfdir

import (
	"path"
	"strings"
)

// A pipeline folder's HEALTH CHECKS live in `checks/<stem>.json`, one file per
// `.sql` file the folder builds, beside the SQL rather than inside it.
//
// Not a `-- @check` header, deliberately: a header lives inside the table's
// `code`, so every check edit would become a SQL change (a new draft, a rebuild,
// a commit, a version-history entry) for a thing that is ungoverned by design.
// And unlike a docs sidecar the stem is NOT an alias — it names a `.sql` file in
// this folder, because a check is attached to the table the folder builds. A
// checks file whose stem names no `.sql` file is refused at parse.
//
// ⚠️ A pipeline folder's SyncExt is ".sql", so these files are invisible to
// Enumerate — never pushed as SQL, and never reported as skipped. They are read
// by the checks pass alone.
const (
	// ChecksDirName is the directory a pipeline folder keeps its checks files
	// in, relative to the folder root.
	ChecksDirName = "checks"
	// ChecksExt is the extension one carries.
	ChecksExt = ".json"
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
