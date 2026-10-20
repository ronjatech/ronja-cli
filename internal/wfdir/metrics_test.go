package wfdir

import (
	"os"
	"strings"
	"testing"
)

// The flat rule is the one worth pinning: see TableDocsAlias.
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

// Both carriers are `.json` one level down, so only the directory tells them
// apart; a path both readers claimed would be pushed twice.
func TestMetricAndDocsCarriersDoNotOverlap(t *testing.T) {
	if _, ok := TableDocsAlias(MetricPath("orders")); ok {
		t.Error("a metric file was read as a docs sidecar")
	}
	if _, ok := MetricAlias(TableDocsPath("orders")); ok {
		t.Error("a docs sidecar was read as a metric file")
	}
}

// The unknown-key sidecars must reach every nesting level of the committed lock,
// or a key a newer CLI writes inside stacks.<name>.metrics.<path> is stripped by
// an older one's load/save.
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

	// Rewritten by a recording rather than an untouched round trip: a setter
	// that built a fresh value would drop the key even with the sidecar in place.
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

	// A path rebound to a different metric keeps nothing: a hash is only ever
	// compared against the row it was taken from.
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

// A legacy instances[] folder has no stack name; writing anyway would commit
// `"stacks":{"":{…}}`. Its recording lives in .ronja/state.json instead.
func TestSetMetricSeenRefusesAnEmptyStack(t *testing.T) {
	lock := &Lock{}
	lock.SetMetricSeen("", "metrics/aov.json", "table-aov", "abc", "def")
	if len(lock.Stacks) != 0 {
		t.Errorf("an empty stack name wrote %+v", lock.Stacks)
	}
}

// A fresh clone has no lock, and empty answers disarm both guards. The read runs
// in a `status` worker goroutine, where a nil-receiver panic would kill the process.
func TestMetricSeenOnANilLockAnswersNothing(t *testing.T) {
	var lock *Lock
	metricID, live, declared := lock.MetricSeen("dev", "metrics/aov.json")
	if metricID != "" || live != "" || declared != "" {
		t.Errorf("MetricSeen on a nil lock = %q %q %q", metricID, live, declared)
	}
}

// A tag write that cleared DeclaredSHA256 would rebuild the metric on the next
// run, and one that cleared LiveSHA256 would disarm the drift guard; conversely,
// recipe setters must carry the tag half forward.
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
	lock.SetMetricSeen("dev", "metrics/aov.json", "table-aov", "live-2", "")
	id, applied, refused := lock.MetricTags("dev", "metrics/aov.json")
	if id != "table-aov" || len(applied) != 1 || applied[0].ID != "tag-fin" || refused == nil || refused.Reason != "too many" {
		t.Fatalf("a recipe write dropped the tag half: %q %+v %+v", id, applied, refused)
	}
	lock.SetMetricTagsRefused("dev", "metrics/aov.json", "table-aov", nil)
	if _, _, refused := lock.MetricTags("dev", "metrics/aov.json"); refused != nil {
		t.Errorf("refusal not cleared: %+v", refused)
	}

	lock.SetMetricSeen("dev", "metrics/aov.json", "table-other", "live-3", "")
	if _, applied, _ := lock.MetricTags("dev", "metrics/aov.json"); len(applied) != 0 {
		t.Errorf("a rebinding kept the old row's tag record: %+v", applied)
	}
	lock.SetMetricTagsApplied("dev", "metrics/aov.json", "table-third", []AppliedTag{{ID: "t", Name: "T"}})
	if id, live, _ := lock.MetricSeen("dev", "metrics/aov.json"); id != "table-third" || live != "" {
		t.Errorf("a tag write against a new row kept the old one's fingerprint: %q %q", id, live)
	}

	empty := &Lock{}
	empty.SetMetricTagsApplied("", "metrics/aov.json", "table-aov", []AppliedTag{{ID: "t", Name: "T"}})
	if len(empty.Stacks) != 0 {
		t.Errorf("an empty stack name wrote %+v", empty.Stacks)
	}
}

// The unknown-key sidecar has to reach inside a tag entry too.
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
