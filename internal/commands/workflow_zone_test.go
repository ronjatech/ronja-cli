package commands

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/ronjatech/ronja-cli/internal/api"
	"github.com/ronjatech/ronja-cli/internal/wfdir"
)

// ── The declared reporting timezone in a workflow folder ──
//
// An absent key means the folder does not manage the calendar: a pre-key
// folder read as a declaration would re-bucket persisted output on the next
// push. An empty string is the reset, stored as "UTC", so every comparison runs
// on the effective value.

func setManifestZone(t *testing.T, root, zone string) {
	t.Helper()
	m, err := wfdir.LoadManifest(root, wfdir.WorkflowKind)
	if err != nil {
		t.Fatalf("load manifest: %v", err)
	}
	m.SetReportingTimezone(zone)
	if err := wfdir.SaveManifest(root, m); err != nil {
		t.Fatalf("save manifest: %v", err)
	}
}

func TestCloneRecordsTheWorkflowsDeclaredZone(t *testing.T) {
	f := newFakeInstance(t)
	f.AddWorkflow(&api.Workflow{ID: "wf-1", Lifecycle: api.LifecycleLive, ReportingTimezone: "Europe/Stockholm"},
		api.WorkflowFile{Path: "main.py", Content: "M\n"})
	signIn(t, f)
	root := cloneFolder(t, f, "wf-1")

	m, err := wfdir.LoadManifest(root, wfdir.WorkflowKind)
	if err != nil {
		t.Fatalf("load manifest: %v", err)
	}
	if !m.ManagesReportingTimezone() {
		t.Fatalf("cloned folder does not manage the declared zone")
	}
	if got := m.DeclaredReportingTimezone(); got != "Europe/Stockholm" {
		t.Errorf("manifest zone = %q, want the workflow's", got)
	}
}

// An undeclared row falls back to the caller's zone at run time, a state the
// API cannot express; writing the key would make the first push stamp UTC.
func TestCloneLeavesTheZoneUnmanagedWhenTheRowDeclaresNone(t *testing.T) {
	f := newFakeInstance(t)
	f.AddWorkflow(&api.Workflow{ID: "wf-1", Lifecycle: api.LifecycleLive},
		api.WorkflowFile{Path: "main.py", Content: "M\n"})
	signIn(t, f)
	root := cloneFolder(t, f, "wf-1")

	m, err := wfdir.LoadManifest(root, wfdir.WorkflowKind)
	if err != nil {
		t.Fatalf("load manifest: %v", err)
	}
	if m.ManagesReportingTimezone() {
		t.Errorf("cloned folder manages a zone the row does not declare (%q)", m.DeclaredReportingTimezone())
	}
}

// Unlike parameters, init must not write the key: the only value it could
// write without a round trip declares UTC, overriding the organization default.
func TestInitDoesNotDeclareAZone(t *testing.T) {
	f := newFakeInstance(t)
	signIn(t, f)
	dir := t.TempDir()

	if _, err := runCLI(t, dir, "wf", "init", "--feature", "feat-1", "--json"); err != nil {
		t.Fatalf("init: %v", err)
	}
	m, err := wfdir.LoadManifest(dir, wfdir.WorkflowKind)
	if err != nil {
		t.Fatalf("load manifest: %v", err)
	}
	if m.ManagesReportingTimezone() {
		t.Errorf("init declared a zone (%q); absent is what lets the create inherit the organization default",
			m.DeclaredReportingTimezone())
	}
	if !m.ManagesParameters() {
		t.Error("init stopped managing parameters — the zone's absence is only correct next to that")
	}
}

func TestPushCreatesWithTheDeclaredZone(t *testing.T) {
	f := newFakeInstance(t)
	signIn(t, f)
	root := initFolder(t, f, map[string]string{"main.py": "x\n"})
	setManifestZone(t, root, "Europe/Stockholm")

	if _, err := runCLI(t, root, "wf", "push", "--json"); err != nil {
		t.Fatalf("push: %v", err)
	}
	if len(f.created) != 1 {
		t.Fatalf("created = %d, want 1", len(f.created))
	}
	if got := f.created[0].ReportingTimezone; got != "Europe/Stockholm" {
		t.Errorf("created with zone %q, want the declared one", got)
	}
}

// POST /workflow refuses an explicit empty zone (the patch's reset), so a
// create sends the literal UTC for the same intent.
func TestPushCreatesTheResetAsAnExplicitUTC(t *testing.T) {
	f := newFakeInstance(t)
	signIn(t, f)
	root := initFolder(t, f, map[string]string{"main.py": "x\n"})
	setManifestZone(t, root, "")

	if _, err := runCLI(t, root, "wf", "push", "--json"); err != nil {
		t.Fatalf("push: %v", err)
	}
	if len(f.created) != 1 {
		t.Fatalf("created = %d, want 1", len(f.created))
	}
	if got := f.created[0].ReportingTimezone; got != "UTC" {
		t.Errorf("created with zone %q, want an explicit \"UTC\" — the create refuses \"\"", got)
	}
}

func TestPushCreatesWithNoZoneForAnUnmanagedFolder(t *testing.T) {
	f := newFakeInstance(t)
	f.tenantZone = "Europe/Stockholm"
	signIn(t, f)
	root := initFolder(t, f, map[string]string{"main.py": "x\n"})

	if _, err := runCLI(t, root, "wf", "push", "--json"); err != nil {
		t.Fatalf("push: %v", err)
	}
	if len(f.created) != 1 {
		t.Fatalf("created = %d, want 1", len(f.created))
	}
	if got := f.created[0].ReportingTimezone; got != "" {
		t.Errorf("created with zone %q, want none sent so the server stamps its default", got)
	}
	if got := f.workflows["wf-new-1"].ReportingTimezone; got != "Europe/Stockholm" {
		t.Errorf("row zone = %q, want the organization default the server stamped", got)
	}
}

func TestPushPatchesTheDeclaredZoneOntoTheDraft(t *testing.T) {
	f := newFakeInstance(t)
	f.AddWorkflow(&api.Workflow{ID: "wf-1", Lifecycle: api.LifecycleLive, ReportingTimezone: "UTC"},
		api.WorkflowFile{Path: "main.py", Content: "M\n"})
	signIn(t, f)
	root := cloneFolder(t, f, "wf-1")
	setManifestZone(t, root, "Europe/Stockholm")

	writeLocal(t, root, "main.py", "M2\n")
	if _, err := runCLI(t, root, "wf", "push", "--json"); err != nil {
		t.Fatalf("push: %v", err)
	}
	draftID := f.draftOf["wf-1"]
	patched, ok := f.timezonePatches[draftID]
	if !ok {
		t.Fatalf("no zone patch sent to draft %q; patches = %+v", draftID, f.timezonePatches)
	}
	if patched == nil || *patched != "Europe/Stockholm" {
		t.Errorf("patched zone = %v, want the declaration", patched)
	}
	if got := f.workflows["wf-1"].ReportingTimezone; got != "UTC" {
		t.Errorf("live row zone = %q — a push must never write to it", got)
	}
}

// Why Manifest.ReportingTimezone is a pointer: reading "no key" as "declares
// UTC" would re-bucket the workflow's output.
func TestPushLeavesTheZoneAloneForAFolderThatDoesNotManageIt(t *testing.T) {
	f := newFakeInstance(t)
	f.AddWorkflow(&api.Workflow{ID: "wf-1", Lifecycle: api.LifecycleLive, ReportingTimezone: "Europe/Stockholm"},
		api.WorkflowFile{Path: "main.py", Content: "M\n"})
	signIn(t, f)
	root := cloneFolder(t, f, "wf-1")

	// A pre-zone folder.
	m, err := wfdir.LoadManifest(root, wfdir.WorkflowKind)
	if err != nil {
		t.Fatalf("load manifest: %v", err)
	}
	m.ReportingTimezone = nil
	if err := wfdir.SaveManifest(root, m); err != nil {
		t.Fatalf("save manifest: %v", err)
	}

	writeLocal(t, root, "main.py", "M2\n")
	if _, err := runCLI(t, root, "wf", "push", "--json"); err != nil {
		t.Fatalf("push: %v", err)
	}
	for id, z := range f.timezonePatches {
		t.Fatalf("push patched the zone on %s (%v) for an unmanaged folder", id, z)
	}
	if got := f.workflows["wf-1"].ReportingTimezone; got != "Europe/Stockholm" {
		t.Errorf("row zone = %q, want the original left intact", got)
	}
}

// Comparing the raw declaration would patch on every push: the reset never
// equals the "UTC" the server stored.
func TestPushDoesNotRePatchAResetTheRowAlreadyHolds(t *testing.T) {
	f := newFakeInstance(t)
	f.AddWorkflow(&api.Workflow{ID: "wf-1", Lifecycle: api.LifecycleLive, ReportingTimezone: "UTC"},
		api.WorkflowFile{Path: "main.py", Content: "M\n"})
	signIn(t, f)
	root := cloneFolder(t, f, "wf-1")
	setManifestZone(t, root, "")

	writeLocal(t, root, "main.py", "M2\n")
	if _, err := runCLI(t, root, "wf", "push", "--json"); err != nil {
		t.Fatalf("push: %v", err)
	}
	for id, z := range f.timezonePatches {
		t.Fatalf("push patched the zone on %s (%v) although the row already holds UTC", id, z)
	}
}

// A push changes the zone without touching a file, so status must show it.
func TestStatusReportsZoneDrift(t *testing.T) {
	f := newFakeInstance(t)
	f.AddWorkflow(&api.Workflow{ID: "wf-1", Lifecycle: api.LifecycleLive, ReportingTimezone: "UTC"},
		api.WorkflowFile{Path: "main.py", Content: "M\n"})
	signIn(t, f)
	root := cloneFolder(t, f, "wf-1")

	out, err := runCLI(t, root, "wf", "status", "--json")
	if err != nil {
		t.Fatalf("status: %v", err)
	}
	payload := decodeJSON(t, out)
	if payload["managesTimezone"] != true {
		t.Errorf("managesTimezone = %v, want true after a clone of a declaring row", payload["managesTimezone"])
	}
	if payload["reportingTimezone"] != "UTC" {
		t.Errorf("reportingTimezone = %v, want the folder's declaration", payload["reportingTimezone"])
	}
	if z := payload["remote"].(map[string]any)["reportingTimezone"]; z != nil {
		t.Errorf("agreeing folder and row reported as drift: %v", z)
	}

	setManifestZone(t, root, "Europe/Stockholm")
	out, err = runCLI(t, root, "wf", "status", "--json")
	if err != nil {
		t.Fatalf("status after edit: %v", err)
	}
	drift, ok := decodeJSON(t, out)["remote"].(map[string]any)["reportingTimezone"].(map[string]any)
	if !ok {
		t.Fatalf("no zone drift reported after declaring a different calendar")
	}
	if drift["local"] != "Europe/Stockholm" || drift["remote"] != "UTC" {
		t.Errorf("drift = %+v, want Europe/Stockholm here and UTC there", drift)
	}
}

// ── Version skew: a server that does not have the column at all ──────────────
//
// An instance predating migration 000499 binds bodies without
// DisallowUnknownFields, so it drops `reportingTimezone` and answers 2xx. Hence
// the read-back: the author is told, and the unapplied value never reaches the
// baseline, so a later push against an upgraded instance applies it.

func pushBaseline(t *testing.T, f *fakeInstance, root string) *wfdir.InstanceState {
	t.Helper()
	state, err := wfdir.LoadState(root)
	if err != nil {
		t.Fatalf("load state: %v", err)
	}
	baseline := state.For(f.Key())
	if baseline == nil {
		t.Fatal("no baseline recorded")
	}
	return baseline
}

func TestPushWarnsAndRecordsNothingWhenTheServerIgnoresTheZone(t *testing.T) {
	f := newFakeInstance(t)
	f.zoneUnsupported = true
	f.AddWorkflow(&api.Workflow{ID: "wf-1", Lifecycle: api.LifecycleLive},
		api.WorkflowFile{Path: "main.py", Content: "M\n"})
	signIn(t, f)
	root := cloneFolder(t, f, "wf-1")
	setManifestZone(t, root, "Europe/Stockholm")
	writeLocal(t, root, "main.py", "M2\n")

	var out string
	var err error
	stderr := captureStderr(t, func() {
		out, err = runCLI(t, root, "wf", "push", "--json")
	})
	if err != nil {
		t.Fatalf("push failed over a zone the server ignored: %v", err)
	}
	if len(f.timezonePatches) == 0 {
		t.Fatal("push never sent the declared zone at all")
	}
	if !strings.Contains(stderr, "does not support reportingTimezone") {
		t.Errorf("no version-skew warning on stderr; got:\n%s", stderr)
	}
	if decodeJSON(t, out)["reportingTimezoneIgnored"] != true {
		t.Errorf("reportingTimezoneIgnored not reported in --json: %s", out)
	}
	// The baseline records the server's answer, not what was sent.
	baseline := pushBaseline(t, f, root)
	if baseline.ReportingTimezone == nil {
		t.Fatal("baseline recorded no zone at all — nil is UNRECORDED, which disarms the drift guard")
	}
	if *baseline.ReportingTimezone != "" {
		t.Errorf("baseline recorded %q; the server holds none, and recording the pushed value would make `wf status` report clean forever",
			*baseline.ReportingTimezone)
	}
	if md, _ := decodeJSON(t, out)["metadata"].(string); strings.Contains(md, "timezone") {
		t.Errorf("push reported the zone as changed: %q", md)
	}
}

func TestPushRecordsAndReportsAZoneTheServerApplies(t *testing.T) {
	f := newFakeInstance(t)
	f.AddWorkflow(&api.Workflow{ID: "wf-1", Lifecycle: api.LifecycleLive, ReportingTimezone: "UTC"},
		api.WorkflowFile{Path: "main.py", Content: "M\n"})
	signIn(t, f)
	root := cloneFolder(t, f, "wf-1")
	setManifestZone(t, root, "Europe/Stockholm")
	writeLocal(t, root, "main.py", "M2\n")

	var out string
	var err error
	stderr := captureStderr(t, func() {
		out, err = runCLI(t, root, "wf", "push")
	})
	if err != nil {
		t.Fatalf("push: %v", err)
	}
	if strings.Contains(stderr, "does not support") {
		t.Errorf("warned about a zone the server applied:\n%s", stderr)
	}
	if !strings.Contains(out, `updated  the reporting timezone to "Europe/Stockholm"`) {
		t.Errorf("the success report does not name the metadata it changed:\n%s", out)
	}
	baseline := pushBaseline(t, f, root)
	if baseline.ReportingTimezone == nil || *baseline.ReportingTimezone != "Europe/Stockholm" {
		t.Errorf("baseline zone = %v, want the confirmed declaration", baseline.ReportingTimezone)
	}
}

// A push that stops after the metadata patch writes its baseline from the
// in-memory row, which must carry the confirmed zone, or no later push retries.
func TestPartialPushDoesNotBakeAnUnappliedZoneIntoTheBaseline(t *testing.T) {
	f := newFakeInstance(t)
	f.zoneUnsupported = true
	f.AddWorkflow(&api.Workflow{ID: "wf-1", Lifecycle: api.LifecycleLive},
		api.WorkflowFile{Path: "main.py", Content: "M\n"},
		api.WorkflowFile{Path: "old.py", Content: "O\n"})
	signIn(t, f)
	root := cloneFolder(t, f, "wf-1")
	setManifestZone(t, root, "Europe/Stockholm")
	// The deletion, the push's last step, is refused.
	if err := os.Remove(filepath.Join(root, "old.py")); err != nil {
		t.Fatalf("remove old.py: %v", err)
	}
	f.failDelete["old.py"] = 500

	captureStderr(t, func() {
		if _, err := runCLI(t, root, "wf", "push", "--json"); err == nil {
			t.Fatal("push reported success although the deletion was refused")
		}
	})
	baseline := pushBaseline(t, f, root)
	if baseline.ReportingTimezone == nil || *baseline.ReportingTimezone != "" {
		t.Errorf("partial push recorded zone %v; the server never stored one", baseline.ReportingTimezone)
	}
}

// The patch landed but the read-back failed. `target` still carries the
// pre-patch zone (applyPatch never touches it), so recording it would make
// metadataDriftOf refuse the next push over our own change; a nil zone is
// skipped by both readers, so the next push retries.
func TestPartialPushRecordsNoZoneWhenTheReadBackFails(t *testing.T) {
	f := newFakeInstance(t)
	f.tenantZone = "UTC"
	f.AddWorkflow(&api.Workflow{ID: "wf-1", Lifecycle: api.LifecycleLive, ReportingTimezone: "UTC"},
		api.WorkflowFile{Path: "main.py", Content: "M\n"},
		api.WorkflowFile{Path: "old.py", Content: "O\n"})
	signIn(t, f)
	root := cloneFolder(t, f, "wf-1")
	setManifestZone(t, root, "Europe/Stockholm")
	if err := os.Remove(filepath.Join(root, "old.py")); err != nil {
		t.Fatalf("remove old.py: %v", err)
	}
	// The deletion fails so the push stops and writes its partial baseline.
	f.failRowGetAfterPatch = true
	f.failDelete["old.py"] = 500

	captureStderr(t, func() {
		if _, err := runCLI(t, root, "wf", "push", "--json"); err == nil {
			t.Fatal("push reported success although the deletion was refused")
		}
	})
	baseline := pushBaseline(t, f, root)
	if baseline.ReportingTimezone != nil {
		t.Errorf("baseline recorded zone %q after an unconfirmed patch; it must record NOTHING, "+
			"or the next push refuses over a change this push made itself", *baseline.ReportingTimezone)
	}
}
