package wfdir

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// ── The one thing SyncExt ".json" could get wrong ───────────────────────────

// TestAutomationFolderNeverSyncsItsOwnMachinery is the test AutomationKind's
// doc points at, and it is the reason that comment exists.
//
// Every other kind's SyncExt names an extension the folder's own bookkeeping
// does not use: a pipeline syncs .sql and its manifest is .json, so the two
// cannot collide. An automation folder syncs `.json` and its manifest, its lock
// file and (were it not a dot-directory) its baseline are all .json — so the
// ONLY thing keeping them out of the customer's automations is that
// StructuralExclusion runs BEFORE the extension is consulted.
//
// If that ordering is ever inverted, nothing else fails: the push succeeds, the
// folder's machine state ships inside a customer's resource, and a read-back
// writes the server's stale copy over the real lock file — a folder inheriting
// another environment's ids, reported as a clean sync.
func TestAutomationFolderNeverSyncsItsOwnMachinery(t *testing.T) {
	for _, path := range []string{
		ManifestName,
		LockName,
		StateDirName + "/" + StateFileName,
		StateDirName + "/" + GitignoreName,
	} {
		if reason := NotSyncable(path, AutomationKind); reason == "" {
			t.Errorf("%s is syncable in an automation folder — the folder's own machinery would be pushed into a customer's automation", path)
		}
		// And it is machinery rather than a user file the reader should be
		// nagged about on every command, which is the OTHER half of the rule
		// isFolderMachinery states.
		if !isFolderMachinery(path) {
			t.Errorf("%s is excluded but not reported as machinery — it would be named as the user's mistake on every command", path)
		}
	}

	// The extension rule is still doing its own job: an ordinary .json file is
	// synced, and everything else in the folder is quietly ignored.
	if reason := NotSyncable("nightly-rebuild.json", AutomationKind); reason != "" {
		t.Errorf("an ordinary automation file must sync, got %q", reason)
	}
	if reason := NotSyncable("README.md", AutomationKind); reason == "" {
		t.Error("an automation folder must ignore non-.json files")
	}
}

// TestAutomationFolderPrunesDependencyTrees is the other half of what SyncExt
// ".json" costs, and it is the half a pipeline folder does not pay.
//
// `.sql` is an extension nothing but a pipeline's own files carries, so
// PipelineKind gets away with no SkipDirs. A JavaScript dependency tree is MADE
// of .json — and Enumerate prunes by DIRECTORY before it ever looks at an
// extension, so without SkipDirs an automation folder anywhere in a JS
// repository walks all of node_modules/ and offers every package.json in it as
// an automation to push.
func TestAutomationFolderPrunesDependencyTrees(t *testing.T) {
	root := t.TempDir()
	write := func(rel, body string) {
		t.Helper()
		if err := os.MkdirAll(filepath.Dir(filepath.Join(root, rel)), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(root, rel), []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	if err := SaveManifest(root, &Manifest{Kind: KindAutomation, Title: "Order automations"}); err != nil {
		t.Fatal(err)
	}
	write("nightly.json", `{"name":"nightly"}`)
	write("node_modules/left-pad/package.json", `{"name":"left-pad"}`)
	write("node_modules/left-pad/nested/deep/tsconfig.json", `{}`)
	write("dist/bundle.json", `{}`)
	write("build/manifest.json", `{}`)

	enumeration, err := Enumerate(root, AutomationKind)
	if err != nil {
		t.Fatalf("enumerate: %v", err)
	}
	if len(enumeration.Files) != 1 {
		t.Fatalf("enumerated %d files, want only nightly.json: %v", len(enumeration.Files), enumeration.Files)
	}
	if _, ok := enumeration.Files["nightly.json"]; !ok {
		t.Errorf("the one real automation is missing: %v", enumeration.Files)
	}
	// Pruned by DIRECTORY, so the whole subtree is reported once rather than
	// once per file — the rule StructuralExclusion states for every kind.
	for _, dir := range []string{"node_modules/", "dist/", "build/"} {
		var found bool
		for _, skipped := range enumeration.Skipped {
			if skipped.Path == dir {
				found = true
			}
			if strings.HasPrefix(skipped.Path, dir) && skipped.Path != dir {
				t.Errorf("%s was walked into rather than pruned at the directory", skipped.Path)
			}
		}
		if !found {
			t.Errorf("%s was not reported as skipped: %+v", dir, enumeration.Skipped)
		}
	}
}

// ── The kind registry ───────────────────────────────────────────────────────

// TestAutomationKindIsRegistered: an unregistered kind loads nowhere and its
// wrong-kind refusal names no command.
func TestAutomationKindIsRegistered(t *testing.T) {
	kind, ok := kindByName[KindAutomation]
	if !ok {
		t.Fatal("KindAutomation is missing from kindByName — LoadManifest cannot name `ronja automation`")
	}
	if kind.Command != "ronja automation" {
		t.Errorf("AutomationKind.Command = %q", kind.Command)
	}
	if kind.SyncExt != ".json" {
		t.Errorf("AutomationKind.SyncExt = %q", kind.SyncExt)
	}
	// No entrypoint: a folder of automations is a SET with no distinguished
	// member, and LoadManifest falls back to Kind.DefaultEntrypoint — so a
	// non-empty default here would invent a file nobody wrote.
	root := t.TempDir()
	if err := SaveManifest(root, &Manifest{Kind: KindAutomation, Title: "Order automations"}); err != nil {
		t.Fatal(err)
	}
	m, err := LoadManifest(root, AutomationKind)
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	if m.Entrypoint != "" {
		t.Errorf("an automation folder must keep an empty entrypoint, got %q", m.Entrypoint)
	}
}

// TestAutomationBindingRefusesTheSingleIDHelpers is
// TestPipelineBindingRefusesTheSingleIDHelpers for the fourth kind, and the
// reason is identical: an automation folder binds MANY rows, so "the" bound row
// has no answer. A fall-through would read an empty WorkflowID —
// indistinguishable from "never pushed here", which a push acts on by creating
// every automation in the folder a second time.
func TestAutomationBindingRefusesTheSingleIDHelpers(t *testing.T) {
	b := Binding{FeatureID: "collection-1", Automations: map[string]string{"nightly.json": "sj-1"}}

	if _, err := b.ResourceID(AutomationKind); err == nil {
		t.Error("ResourceID must refuse an automation folder rather than answering with WorkflowID")
	} else {
		// The KIND comes from the wrapper, the REMEDY from the sentinel — and
		// the remedy has to name the map this kind actually keeps. The sentinel
		// named only Binding.Tables while serving both multi-resource kinds, and
		// a test that asserted "automation" alone passed the whole time, because
		// that word comes from kind.Label and never from the misdirection.
		if !strings.Contains(err.Error(), "automation") {
			t.Errorf("the refusal should name the kind, got %v", err)
		}
		if !strings.Contains(err.Error(), "Binding.Automations") {
			t.Errorf("the refusal must point at the map an automation folder keeps, got %v", err)
		}
		if !errors.Is(err, ErrMultiResourceBinding) {
			t.Errorf("the refusal must wrap the sentinel, got %v", err)
		}
	}
	if _, err := b.WithResourceID(AutomationKind, "sj-2"); err == nil {
		t.Error("WithResourceID must refuse an automation folder rather than writing into WorkflowID")
	}
}

// TestWithAutomationCopiesTheMap: Binding is a value type with a reference
// field, so writing through the map would reach into whatever Manifest or Lock
// the binding was read from — recording a binding the save that should have
// persisted it never made.
func TestWithAutomationCopiesTheMap(t *testing.T) {
	original := Binding{Automations: map[string]string{"nightly.json": "sj-1"}}
	updated := original.WithAutomation("invoices.json", "sj-2")

	if len(original.Automations) != 1 {
		t.Errorf("WithAutomation wrote through to the caller's map: %v", original.Automations)
	}
	if updated.Automations["nightly.json"] != "sj-1" || updated.Automations["invoices.json"] != "sj-2" {
		t.Errorf("the copy lost an entry: %v", updated.Automations)
	}
	// A nil source map is the ordinary first-push case and must not panic.
	first := Binding{}.WithAutomation("nightly.json", "sj-1")
	if first.Automations["nightly.json"] != "sj-1" {
		t.Errorf("WithAutomation on an empty binding: %v", first.Automations)
	}
}

// ── The lock ────────────────────────────────────────────────────────────────

// TestLockAutomationRoundTrip: the id and the timestamp both have to survive a
// save/load, and the timestamp is the one that fails silently — a dropped
// updatedAt disarms the drift guard and the re-enable guard while leaving the
// folder looking perfectly bound.
func TestLockAutomationRoundTrip(t *testing.T) {
	root := t.TempDir()
	l := &Lock{}
	l.SetBinding("prod", Binding{Automations: map[string]string{"nightly.json": "sj-1"}})
	l.SetAutomationSeen("prod", "nightly.json", "sj-1", "2026-09-02T09:15:00Z")
	if err := SaveLock(root, l); err != nil {
		t.Fatal(err)
	}

	loaded, err := LoadLock(root)
	if err != nil {
		t.Fatal(err)
	}
	id, seen := loaded.AutomationSeen("prod", "nightly.json")
	if id != "sj-1" {
		t.Errorf("automationID = %q, want sj-1", id)
	}
	if seen != "2026-09-02T09:15:00Z" {
		t.Errorf("updatedAt = %q — the drift anchor did not survive the round trip", seen)
	}
	if got := loaded.Binding("prod").Automations["nightly.json"]; got != "sj-1" {
		t.Errorf("Binding did not carry the automation id, got %q", got)
	}
	// An unrecorded path answers empty rather than panicking, which is what
	// disarms both guards on a folder that has never pushed.
	if id, seen := loaded.AutomationSeen("prod", "unknown.json"); id != "" || seen != "" {
		t.Errorf("an unrecorded path answered (%q, %q)", id, seen)
	}
	if id, seen := loaded.AutomationSeen("staging", "nightly.json"); id != "" || seen != "" {
		t.Errorf("an unrecorded stack answered (%q, %q)", id, seen)
	}
}

// TestLockBindingDoesNotReachIntoTheLock: Lock.Binding hands out a map, and a
// caller that mutates it must not be editing the lock file's own state — the
// same hazard Binding.WithAutomation copies around, seen from the read side.
func TestLockBindingDoesNotReachIntoTheLock(t *testing.T) {
	l := &Lock{}
	l.SetBinding("prod", Binding{Automations: map[string]string{"nightly.json": "sj-1"}})

	b := l.Binding("prod")
	b.Automations["nightly.json"] = "sj-tampered"
	b.Automations["invoices.json"] = "sj-2"

	if got := l.Stacks["prod"].Automations["nightly.json"].AutomationID; got != "sj-1" {
		t.Errorf("mutating a returned Binding reached the lock: %q", got)
	}
	if _, leaked := l.Stacks["prod"].Automations["invoices.json"]; leaked {
		t.Error("a binding added to a returned Binding appeared in the lock")
	}
}

// TestSetBindingPreservesTheAutomationAnchor is SetBinding's counterpart to the
// LiveSHA256 rule: Binding is path → id and carries no timestamp, so a writer
// that rebuilt the map wholesale would drop every updatedAt on the first push
// after a clone. For a pipeline that degrades a guard; here it DISARMS the only
// one this loop has.
func TestSetBindingPreservesTheAutomationAnchor(t *testing.T) {
	l := &Lock{}
	l.SetBinding("prod", Binding{Automations: map[string]string{"nightly.json": "sj-1"}})
	l.SetAutomationSeen("prod", "nightly.json", "sj-1", "2026-09-02T09:15:00Z")

	// A second push re-records the same binding, as every push does.
	l.SetBinding("prod", Binding{Automations: map[string]string{"nightly.json": "sj-1", "invoices.json": "sj-2"}})
	if _, seen := l.AutomationSeen("prod", "nightly.json"); seen != "2026-09-02T09:15:00Z" {
		t.Errorf("re-recording the binding dropped the anchor, got %q", seen)
	}

	// A path REBOUND to a different row drops the old row's anchor with it: an
	// anchor is only ever compared against the row it was read from.
	l.SetBinding("prod", Binding{Automations: map[string]string{"nightly.json": "sj-9"}})
	if _, seen := l.AutomationSeen("prod", "nightly.json"); seen != "" {
		t.Errorf("a rebound path kept the previous row's anchor: %q", seen)
	}
	// SetAutomationSeen enforces the same invariant at the other write point.
	l.SetAutomationSeen("prod", "nightly.json", "sj-9", "2026-09-02T10:00:00Z")
	l.SetAutomationSeen("prod", "nightly.json", "sj-10", "")
	if _, seen := l.AutomationSeen("prod", "nightly.json"); seen != "" {
		t.Errorf("SetAutomationSeen kept a previous row's anchor: %q", seen)
	}
}

// TestSetBindingWritesNoEmptyAutomationStack: `automation init --stack dev`
// declares a stack and creates nothing, and a committed lock file saying
// `{"stacks":{"dev":{}}}` is the first thing a reader of that file would see.
func TestSetBindingWritesNoEmptyAutomationStack(t *testing.T) {
	l := &Lock{}
	l.SetBinding("dev", Binding{FeatureID: "collection-1"})
	if len(l.Stacks) != 0 {
		t.Errorf("an empty binding recorded a stack: %v", l.Stacks)
	}
}

// TestSetAutomationSeenIgnoresAnUnnamedStack: a legacy instances[] folder has no
// stack name and keeps its bindings in the manifest, so there is nowhere in a
// lock keyed by stack name to record its anchor.
//
// Written anyway, it produced `"stacks":{"":{…}}` in a COMMITTED file — and
// since nothing ever refreshed that entry, the next push read an anchor older
// than the row and refused "this changed on the server" for ever, --force
// included. A folder permanently unpushable because of a key nobody typed.
func TestSetAutomationSeenIgnoresAnUnnamedStack(t *testing.T) {
	l := &Lock{}
	l.SetAutomationSeen("", "nightly.json", "sj-1", "2026-09-02T09:15:00Z")
	if len(l.Stacks) != 0 {
		t.Errorf("an unnamed stack was written to the lock: %v", l.Stacks)
	}
	if id, seen := l.AutomationSeen("", "nightly.json"); id != "" || seen != "" {
		t.Errorf("an unnamed stack answered an anchor: %q %q", id, seen)
	}
}

// ── Forward compatibility ───────────────────────────────────────────────────

// TestAutomationLockKeepsUnknownKeys: ronja.lock.json is COMMITTED and rewritten
// whole by whoever opens it, so a key a newer CLI wrote into an automation entry
// has to survive an older one saving the file around it. The sidecars stopped
// one nesting level short of LockTable until that level was itself new; this is
// the same gap, one level over, and it is silent — what it drops is what a newer
// CLI recorded about a live row.
func TestAutomationLockKeepsUnknownKeys(t *testing.T) {
	root := t.TempDir()
	raw := `{
  "stacks": {
    "prod": {
      "automations": {
        "nightly.json": {
          "automationID": "sj-1",
          "updatedAt": "2026-09-02T09:15:00Z",
          "runHealthFromTheFuture": {"streak": 3}
        }
      },
      "somethingNewOnTheStack": true
    }
  },
  "somethingNewAtTheRoot": ["a"]
}
`
	if err := os.WriteFile(LockPath(root), []byte(raw), 0o644); err != nil {
		t.Fatal(err)
	}
	l, err := LoadLock(root)
	if err != nil {
		t.Fatal(err)
	}
	// Rewrite it exactly as a push would: touch the anchor, save.
	l.SetAutomationSeen("prod", "nightly.json", "sj-1", "2026-09-02T11:00:00Z")
	if err := SaveLock(root, l); err != nil {
		t.Fatal(err)
	}

	body, err := os.ReadFile(LockPath(root))
	if err != nil {
		t.Fatal(err)
	}
	var decoded map[string]any
	if err := json.Unmarshal(body, &decoded); err != nil {
		t.Fatalf("the rewritten lock does not parse: %v\n%s", err, body)
	}
	if _, ok := decoded["somethingNewAtTheRoot"]; !ok {
		t.Errorf("the root's unknown key was dropped:\n%s", body)
	}
	stack, _ := decoded["stacks"].(map[string]any)["prod"].(map[string]any)
	if _, ok := stack["somethingNewOnTheStack"]; !ok {
		t.Errorf("the stack's unknown key was dropped:\n%s", body)
	}
	entry, _ := stack["automations"].(map[string]any)["nightly.json"].(map[string]any)
	if _, ok := entry["runHealthFromTheFuture"]; !ok {
		t.Errorf("the automation entry's unknown key was dropped:\n%s", body)
	}
	// The struct's OWN value wins for the keys it produces — the sidecar must
	// not resurrect the timestamp this push just replaced.
	if entry["updatedAt"] != "2026-09-02T11:00:00Z" {
		t.Errorf("updatedAt = %v, want the value this push wrote", entry["updatedAt"])
	}
}

// TestAutomationManifestKeepsUnknownKeys is the same guarantee on the file a
// HUMAN edits. A manifest is where a folder's stacks, dependencies and binds
// live, so a key an older CLI drops here is a deployment decision gone from the
// customer's repo with nothing in the diff to explain it.
func TestAutomationManifestKeepsUnknownKeys(t *testing.T) {
	root := t.TempDir()
	raw := `{
  "kind": "automation",
  "title": "Order automations",
  "somethingNewAtTheRoot": {"k": 1},
  "stacks": {
    "prod": {
      "url": "https://api.example.com",
      "tenantID": "t-1",
      "featureID": "collection-1",
      "somethingNewOnTheStack": "keep me"
    }
  }
}
`
	if err := os.WriteFile(ManifestPath(root), []byte(raw), 0o644); err != nil {
		t.Fatal(err)
	}
	m, err := LoadManifest(root, AutomationKind)
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	if m.Kind != KindAutomation {
		t.Fatalf("kind = %q", m.Kind)
	}
	if err := SaveManifest(root, m); err != nil {
		t.Fatal(err)
	}

	body, err := os.ReadFile(ManifestPath(root))
	if err != nil {
		t.Fatal(err)
	}
	var decoded map[string]any
	if err := json.Unmarshal(body, &decoded); err != nil {
		t.Fatalf("the rewritten manifest does not parse: %v\n%s", err, body)
	}
	if _, ok := decoded["somethingNewAtTheRoot"]; !ok {
		t.Errorf("the root's unknown key was dropped:\n%s", body)
	}
	stack, _ := decoded["stacks"].(map[string]any)["prod"].(map[string]any)
	if _, ok := stack["somethingNewOnTheStack"]; !ok {
		t.Errorf("the stack's unknown key was dropped:\n%s", body)
	}
	// An automation folder has no entrypoint, and a rewrite must not invent one.
	if _, ok := decoded["entrypoint"]; ok {
		t.Errorf("the rewrite added an entrypoint:\n%s", body)
	}
}

// TestBindingWithoutAutomationCopies is the prune half of the pair, and it
// copies for the reason WithAutomation does — sharpened one turn. A forget
// written THROUGH the caller's map and then lost to a failed save leaves a
// folder still bound to a row that no longer exists, whose next push refuses on
// a binding the message cannot explain.
func TestBindingWithoutAutomationCopies(t *testing.T) {
	original := Binding{Automations: map[string]string{
		"nightly.json":  "job-1",
		"invoices.json": "job-2",
	}}
	reduced := original.WithoutAutomation("invoices.json")

	if len(original.Automations) != 2 {
		t.Errorf("the original binding was mutated: %v", original.Automations)
	}
	if len(reduced.Automations) != 1 || reduced.Automations["nightly.json"] != "job-1" {
		t.Errorf("reduced = %v", reduced.Automations)
	}
	if _, still := reduced.Automations["invoices.json"]; still {
		t.Error("the dropped path survived")
	}
	// Dropping from a binding that has none allocates nothing and answers the
	// same binding back.
	if got := (Binding{}).WithoutAutomation("x"); got.Automations != nil {
		t.Errorf("an empty binding grew a map: %v", got.Automations)
	}
}
