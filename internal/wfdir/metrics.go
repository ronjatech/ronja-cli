package wfdir

import (
	"path"
	"strings"
)

// A METRIC FILE is a committed file in a pipeline folder that holds the whole
// definition of one metric: its recipe, its prose, and the calendar it is
// evaluated at. It carries nothing else — see internal/metricfile.
//
// WHY IT LIVES IN THE PIPELINE FOLDER rather than in a `ronja metric` loop of
// its own, which is the same argument the docs sidecar makes above and is worth
// making again because the alternative is superficially tidier. A metric is a
// recipe, so no .sql file can hold one — but everything a sync loop would need
// to reach it already exists here: one manifest, one bind map, one lock, one
// tree walk. A second top-level command would need its own manifest, its own
// lock, its own bind map and its own tree walk, all of them second copies of
// these. And the folder's verbs already mean exactly the right thing: a metric
// runs the IDENTICAL draft flow a derived table runs, so `push` builds a draft,
// `publish` commits it and `discard` drops it, with nothing invented.
//
// THE LAYOUT: `metrics/<alias>.json`, beside the folder's .sql files and its
// `tables/` sidecars.
//
//	orders_by_month.sql             a table this folder BUILDS
//	tables/fortnox_invoices.json    a table it does NOT build, documented here
//	metrics/average_order_value.json a METRIC this folder defines
//
// The stem is an ALIAS — a name declared in ronja.json's `dependencies` with
// kind `table` and bound per stack — exactly as a docs sidecar's is, which is
// what makes one committed folder deployable to several organizations. A
// literal `table-…` id is accepted too and means the folder can only ever be
// pushed to the organization that minted it. There is deliberately no `metric`
// dependency kind: `GET /api/v2/search` reports a metric as kind `table`, so a
// metric alias is spelled `{"kind": "table"}` and `ronja bind` binds it like
// any other.
//
// ⚠️ A pipeline folder's SyncExt is ".sql", so these files are invisible to
// Enumerate — never pushed as SQL, and never reported as skipped. They are read
// by the metric pass alone, which is also where their ORDER comes from: a
// metric contributes no `{{ ref }}` edge (no marker resolves a metric), so it
// cannot join the folder's topological order. The metric pass therefore runs
// AFTER the .sql pass, which buys the same guarantee — a metric reading a table
// this folder builds is always pushed after it. That is the MAIN case and the
// reason the ordering exists; unlike a docs sidecar there is no
// disjointness rule against it, because `recipe.source` is a REFERENCE to
// another row rather than a second carrier of it.
//
// What a metric file and a .sql file DO share is one namespace for their own
// names: `requireNameFreeTx` has no `kind` predicate, so `revenue.sql` and
// `metrics/Revenue.json` collide server-side. That is guarded locally so the
// refusal arrives before the first byte rather than half way through a push.
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
// path is a metric file at all.
//
// FLAT, deliberately, for TableDocsAlias's reason: `metrics/a/b.json` is not a
// metric file. The stem is an alias, and an alias is a single name — allowing a
// nested one would invent a second spelling for it (`a/b`) that ronja.json's
// dependency map has no way to express, so the file would be committed, never
// read, and never reported.
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
