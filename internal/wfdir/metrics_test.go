package wfdir

import (
	"os"
	"strings"
	"testing"
)

// TestMetricPathAndAliasRoundTrip: the two directions agree, and the shape is
// FLAT.
//
// The nesting rule is the one worth pinning. The stem is an ALIAS, and an alias
// is a single name — allowing `metrics/a/b.json` would invent a second spelling
// for it (`a/b`) that ronja.json's dependency map has no way to express, so the
// file would be committed, never read, and never reported.
func TestMetricPathAndAliasRoundTrip(t *testing.T) {
	if got := MetricPath("average_order"); got != "metrics/average_order.json" {
		t.Errorf("MetricPath = %q", got)
	}
	for _, path := range []string{
		"metrics/average_order.json",
		"metrics/table-abc.json",
	} {
		alias, ok := MetricAlias(path)
		if !ok {
			t.Errorf("%s was not read as a metric file", path)
			continue
		}
		if MetricPath(alias) != path {
			t.Errorf("%s round-tripped to %s", path, MetricPath(alias))
		}
	}
	for _, path := range []string{
		"metrics/nested/one.json", // not flat
		"metrics/README.md",       // not JSON
		"metrics/.json",           // no stem
		"tables/orders.json",      // the docs sidecar's directory
		"orders.sql",
		"metrics",
	} {
		if alias, ok := MetricAlias(path); ok {
			t.Errorf("%s was read as a metric file called %q", path, alias)
		}
	}
}

// TestMetricAndDocsCarriersDoNotOverlap: both are `.json` and both sit one level
// down, so the ONLY thing telling them apart is the directory. A path that both
// readers claimed would be pushed twice, as two different kinds of thing.
func TestMetricAndDocsCarriersDoNotOverlap(t *testing.T) {
	if _, ok := TableDocsAlias(MetricPath("orders")); ok {
		t.Error("a metric file was read as a docs sidecar")
	}
	if _, ok := MetricAlias(TableDocsPath("orders")); ok {
		t.Error("a docs sidecar was read as a metric file")
	}
}

// TestLockMetricPreservesUnknownKeys is TestLockTablePreservesUnknownKeys for
// the metric entry, and it is worth having its own copy for the reason that one
// states: the sidecars have to reach every nesting level of a COMMITTED file, or
// a key a newer CLI writes inside stacks.<name>.metrics.<path> is stripped by any
// load/save an older one does.
func TestLockMetricPreservesUnknownKeys(t *testing.T) {
	root := t.TempDir()
	body := `{"stacks":{"dev":{"metrics":{"metrics/aov.json":{"metricID":"table-aov","liveSHA256":"abc","verifiedAt":"2026-08-31T00:00:00Z"}}}}}` + "\n"
	write(t, root, LockName, body)
	lock, err := LoadLock(root)
	if err != nil {
		t.Fatalf("LoadLock: %v", err)
	}
	metricID, live, declared := lock.MetricSeen("dev", "metrics/aov.json")
	if metricID != "table-aov" || live != "abc" || declared != "" {
		t.Fatalf("MetricSeen = %q %q %q", metricID, live, declared)
	}

	// Rewritten by an ordinary recording rather than by an untouched round trip:
	// SetMetricSeen edits the entry, and building a fresh value there is how the
	// key would be dropped even with the sidecar in place.
	lock.SetMetricSeen("dev", "metrics/aov.json", "table-aov", "def", "file-1")
	if err := SaveLock(root, lock); err != nil {
		t.Fatalf("SaveLock: %v", err)
	}
	written, err := os.ReadFile(LockPath(root))
	if err != nil {
		t.Fatalf("read back: %v", err)
	}
	if !strings.Contains(string(written), `"verifiedAt"`) {
		t.Fatalf("a load/save stripped an unknown key from a metric entry:\n%s", written)
	}
	if !strings.Contains(string(written), `"liveSHA256": "def"`) ||
		!strings.Contains(string(written), `"declaredSHA256": "file-1"`) {
		t.Fatalf("the recording did not survive:\n%s", written)
	}

	// A path rebound to a DIFFERENT metric keeps nothing, by the invariant on
	// LockMetric: a hash is only ever compared against the row it was taken from,
	// and the old row's keys describe something else entirely.
	lock.SetMetricSeen("dev", "metrics/aov.json", "table-other", "ghi", "file-2")
	if err := SaveLock(root, lock); err != nil {
		t.Fatalf("SaveLock: %v", err)
	}
	written, err = os.ReadFile(LockPath(root))
	if err != nil {
		t.Fatalf("read back: %v", err)
	}
	if strings.Contains(string(written), `"verifiedAt"`) {
		t.Fatalf("a rebinding kept the old row's keys:\n%s", written)
	}
}

// TestSetMetricSeenRefusesAnEmptyStack: a LEGACY instances[] folder has no stack
// name, and there is nowhere in a lock keyed by stack name to put its recording
// — writing one anyway puts `"stacks":{"":{…}}` into a COMMITTED file. Its
// recording lives in .ronja/state.json instead; see the liveHashes fork in the
// commands package.
func TestSetMetricSeenRefusesAnEmptyStack(t *testing.T) {
	lock := &Lock{}
	lock.SetMetricSeen("", "metrics/aov.json", "table-aov", "abc", "def")
	if len(lock.Stacks) != 0 {
		t.Errorf("an empty stack name wrote %+v", lock.Stacks)
	}
}

// TestMetricSeenOnANilLockAnswersNothing: a fresh clone has no lock at all, and
// three empty strings are what disarms both guards — which is exactly right
// there. The read also runs inside a `status` worker goroutine, so a miss would
// be a raw process panic rather than an error anybody could act on.
func TestMetricSeenOnANilLockAnswersNothing(t *testing.T) {
	var lock *Lock
	metricID, live, declared := lock.MetricSeen("dev", "metrics/aov.json")
	if metricID != "" || live != "" || declared != "" {
		t.Errorf("MetricSeen on a nil lock = %q %q %q", metricID, live, declared)
	}
}

// TestMetricTagSettersNeverTouchTheRecipeFingerprints: the TAG half is a
// separate base on the same entry, and the one property that matters is that
// writing it cannot move the recipe's. A tag-only push that cleared
// DeclaredSHA256 would re-stage and re-build the metric on the next run; one
// that cleared LiveSHA256 would disarm the drift guard.
//
// And the converse: every recipe setter carries the tag half forward, because
// SetMetricSeen runs at eight moments none of which is about tags.
func TestMetricTagSettersNeverTouchTheRecipeFingerprints(t *testing.T) {
	lock := &Lock{}
	lock.SetMetricSeen("dev", "metrics/aov.json", "table-aov", "live-1", "file-1")
	lock.SetMetricTagsApplied("dev", "metrics/aov.json", "table-aov",
		[]AppliedTag{{ID: "tag-fin", Name: "Finance"}})
	lock.SetMetricTagsRefused("dev", "metrics/aov.json", "table-aov",
		&TagsRefusal{FileTagsHash: "h", Reason: "too many"})

	if id, live, declared := lock.MetricSeen("dev", "metrics/aov.json"); id != "table-aov" || live != "live-1" || declared != "file-1" {
		t.Fatalf("a tag write moved the recipe fingerprints: %q %q %q", id, live, declared)
	}
	// A recipe recording — the up-to-date arm, a publish, a discard — keeps both.
	lock.SetMetricSeen("dev", "metrics/aov.json", "table-aov", "live-2", "")
	id, applied, refused := lock.MetricTags("dev", "metrics/aov.json")
	if id != "table-aov" || len(applied) != 1 || applied[0].ID != "tag-fin" || refused == nil || refused.Reason != "too many" {
		t.Fatalf("a recipe write dropped the tag half: %q %+v %+v", id, applied, refused)
	}
	// Clearing a refusal is its own write.
	lock.SetMetricTagsRefused("dev", "metrics/aov.json", "table-aov", nil)
	if _, _, refused := lock.MetricTags("dev", "metrics/aov.json"); refused != nil {
		t.Errorf("refusal not cleared: %+v", refused)
	}

	// Rebound to a DIFFERENT row: a tag record against another metric is reset,
	// by the same invariant the recipe fingerprints obey.
	lock.SetMetricSeen("dev", "metrics/aov.json", "table-other", "live-3", "")
	if _, applied, _ := lock.MetricTags("dev", "metrics/aov.json"); len(applied) != 0 {
		t.Errorf("a rebinding kept the old row's tag record: %+v", applied)
	}
	// And a tag write against a new id resets the old row's recipe fingerprints,
	// rather than leaving them attributed to a row they were not taken from.
	lock.SetMetricTagsApplied("dev", "metrics/aov.json", "table-third", []AppliedTag{{ID: "t", Name: "T"}})
	if id, live, _ := lock.MetricSeen("dev", "metrics/aov.json"); id != "table-third" || live != "" {
		t.Errorf("a tag write against a new row kept the old one's fingerprint: %q %q", id, live)
	}

	// An empty stack writes nothing, as every lock setter.
	empty := &Lock{}
	empty.SetMetricTagsApplied("", "metrics/aov.json", "table-aov", []AppliedTag{{ID: "t", Name: "T"}})
	if len(empty.Stacks) != 0 {
		t.Errorf("an empty stack name wrote %+v", empty.Stacks)
	}
}

// TestAppliedTagsRoundTripThroughTheCommittedFile, with an unknown key inside a
// tag entry surviving — the sidecar has to reach this nesting level too.
func TestAppliedTagsRoundTripThroughTheCommittedFile(t *testing.T) {
	root := t.TempDir()
	body := `{"stacks":{"dev":{"metrics":{"metrics/aov.json":{"metricID":"table-aov",` +
		`"tagsApplied":[{"id":"tag-fin","name":"Finance","addedBy":"future"}],` +
		`"tagsRefused":{"fileTagsHash":"h","reason":"r","code":"future"}}}}}}` + "\n"
	write(t, root, LockName, body)
	lock, err := LoadLock(root)
	if err != nil {
		t.Fatalf("LoadLock: %v", err)
	}
	_, applied, refused := lock.MetricTags("dev", "metrics/aov.json")
	if len(applied) != 1 || applied[0].Name != "Finance" || refused == nil || refused.FileTagsHash != "h" {
		t.Fatalf("MetricTags = %+v %+v", applied, refused)
	}
	if err := SaveLock(root, lock); err != nil {
		t.Fatalf("SaveLock: %v", err)
	}
	written, err := os.ReadFile(LockPath(root))
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{`"addedBy"`, `"code": "future"`, `"tagsApplied"`} {
		if !strings.Contains(string(written), want) {
			t.Errorf("a load/save lost %s:\n%s", want, written)
		}
	}
}
