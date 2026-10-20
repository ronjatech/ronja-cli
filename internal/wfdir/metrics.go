package wfdir

import (
	"path"
	"strings"
)

// A metric file, `metrics/<alias>.json` in a pipeline folder, holds the whole
// definition of one metric (see internal/metricfile); the design is in
// cli/README.md, "Metrics — a definition, not SQL". The stem is a `table`-kind
// alias, since search reports a metric as kind `table`.
//
// ⚠️ SyncExt ".sql" makes these files invisible to Enumerate; the metric pass
// alone reads them, and it must run AFTER the .sql pass because a metric has no
// `{{ ref }}` edge to place it after a table this folder builds. A metric and a
// .sql file share one server-side name namespace (`requireNameFreeTx` has no
// kind predicate), which is guarded locally before a push starts.
const (
	// MetricsDirName is the directory a pipeline folder keeps its metric files
	// in, relative to the folder root.
	MetricsDirName = "metrics"
	// MetricExt is the extension one carries.
	MetricExt = ".json"
)

// MetricPath is the folder-relative path of one alias's metric file, in the
// slash-separated spelling every map in this package is keyed by.
func MetricPath(alias string) string {
	return MetricsDirName + "/" + alias + MetricExt
}

// MetricAlias is the inverse: the alias a metric file names, and whether the
// path is a metric file at all. Flat, deliberately, for TableDocsAlias's reason.
func MetricAlias(p string) (string, bool) {
	dir, file := path.Split(p)
	if strings.TrimSuffix(dir, "/") != MetricsDirName {
		return "", false
	}
	if !strings.HasSuffix(file, MetricExt) {
		return "", false
	}
	alias := strings.TrimSuffix(file, MetricExt)
	if alias == "" {
		return "", false
	}
	return alias, true
}
