package wfdir

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// ⚠️ An automation folder syncs `.json`, the extension of its own manifest and
// lock, so only StructuralExclusion running before the extension check keeps
// them out of the customer's automations; inverted, nothing else fails.
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
		if !isFolderMachinery(path) {
			t.Errorf("%s is excluded but not reported as machinery — it would be named as the user's mistake on every command", path)
		}
	}

	if reason := NotSyncable("nightly-rebuild.json", AutomationKind); reason != "" {
		t.Errorf("an ordinary automation file must sync, got %q", reason)
	}
	if reason := NotSyncable("README.md", AutomationKind); reason == "" {
		t.Error("an automation folder must ignore non-.json files")
	}
}

// A JS dependency tree is made of .json, so without SkipDirs an automation
// folder would offer every package.json under node_modules/ as an automation.
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
	// Pruned by directory, so each subtree is reported once, not per file.
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
	// No entrypoint: LoadManifest falls back to Kind.DefaultEntrypoint, so a
	// non-empty default would invent a file nobody wrote.
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

// An automation folder binds many rows; a fall-through would read an empty
// WorkflowID as "never pushed" and create every automation a second time.
func TestAutomationBindingRefusesTheSingleIDHelpers(t *testing.T) {
	b := Binding{FeatureID: "collection-1", Automations: map[string]string{"nightly.json": "sj-1"}}

	if _, err := b.ResourceID(AutomationKind); err == nil {
		t.Error("ResourceID must refuse an automation folder rather than answering with WorkflowID")
	} else {
		// The kind comes from the wrapper, the remedy from the sentinel; both
		// are asserted, since "automation" alone would pass on a wrong remedy.
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

// Binding is a value type with a map field; writing through it would edit the
// Manifest or Lock it was read from.
func TestWithAutomationCopiesTheMap(t *testing.T) {
	original := Binding{Automations: map[string]string{"nightly.json": "sj-1"}}
	updated := original.WithAutomation("invoices.json", "sj-2")

	if len(original.Automations) != 1 {
		t.Errorf("WithAutomation wrote through to the caller's map: %v", original.Automations)
	}
	if updated.Automations["nightly.json"] != "sj-1" || updated.Automations["invoices.json"] != "sj-2" {
		t.Errorf("the copy lost an entry: %v", updated.Automations)
	}
	first := Binding{}.WithAutomation("nightly.json", "sj-1")
	if first.Automations["nightly.json"] != "sj-1" {
		t.Errorf("WithAutomation on an empty binding: %v", first.Automations)
	}
}

// ── The lock ────────────────────────────────────────────────────────────────

// A dropped updatedAt silently disarms the drift and re-enable guards.
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
	if id, seen := loaded.AutomationSeen("prod", "unknown.json"); id != "" || seen != "" {
		t.Errorf("an unrecorded path answered (%q, %q)", id, seen)
	}
	if id, seen := loaded.AutomationSeen("staging", "nightly.json"); id != "" || seen != "" {
		t.Errorf("an unrecorded stack answered (%q, %q)", id, seen)
	}
}

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

// Binding carries no timestamp, so rebuilding the map wholesale would drop every
// updatedAt and disarm this loop's only guard.
func TestSetBindingPreservesTheAutomationAnchor(t *testing.T) {
	l := &Lock{}
	l.SetBinding("prod", Binding{Automations: map[string]string{"nightly.json": "sj-1"}})
	l.SetAutomationSeen("prod", "nightly.json", "sj-1", "2026-09-02T09:15:00Z")

	l.SetBinding("prod", Binding{Automations: map[string]string{"nightly.json": "sj-1", "invoices.json": "sj-2"}})
	if _, seen := l.AutomationSeen("prod", "nightly.json"); seen != "2026-09-02T09:15:00Z" {
		t.Errorf("re-recording the binding dropped the anchor, got %q", seen)
	}

	// A rebound path drops its anchor: an anchor is only compared against the
	// row it was read from.
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

// `automation init --stack dev` creates nothing, so it must not commit
// `{"stacks":{"dev":{}}}`.
func TestSetBindingWritesNoEmptyAutomationStack(t *testing.T) {
	l := &Lock{}
	l.SetBinding("dev", Binding{FeatureID: "collection-1"})
	if len(l.Stacks) != 0 {
		t.Errorf("an empty binding recorded a stack: %v", l.Stacks)
	}
}

// A legacy instances[] folder has no stack name. An anchor recorded under ""
// would never be refreshed, so every later push would refuse as changed on the
// server, --force included.
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

// ronja.lock.json is committed and rewritten whole, so a key a newer CLI wrote
// into an automation entry must survive an older one saving around it.
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
	// The struct's own value wins over the preserved unknown-key sidecar.
	if entry["updatedAt"] != "2026-09-02T11:00:00Z" {
		t.Errorf("updatedAt = %v, want the value this push wrote", entry["updatedAt"])
	}
}

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
	if _, ok := decoded["entrypoint"]; ok {
		t.Errorf("the rewrite added an entrypoint:\n%s", body)
	}
}

// A forget written through the caller's map and lost to a failed save would
// leave the folder bound to a row that no longer exists.
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
	if got := (Binding{}).WithoutAutomation("x"); got.Automations != nil {
		t.Errorf("an empty binding grew a map: %v", got.Automations)
	}
}
