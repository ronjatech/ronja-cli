package commands

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/ronjatech/ronja-cli/internal/api"
	"github.com/ronjatech/ronja-cli/internal/wfdir"
)

// ── The declared reporting timezone in a workflow folder (migration 000499) ──
//
// The manifest key has the same three states `parameters` does, and the same
// failure mode if any of them is got wrong: absent must mean "the folder does
// not manage the calendar", because a folder created before the key existed
// looks exactly like one declaring nothing, and reading it as a declaration
// would re-bucket a workflow's persisted output on the next ordinary push.
//
// What makes the zone harder than `parameters` is that "" is not the empty
// declaration — it is the RESET, which the server stores as the literal "UTC".
// Every comparison here therefore runs on the EFFECTIVE value, or a folder
// declaring the reset drifts against the row forever.

// setManifestZone marks a folder as MANAGING the declared zone, the way a
// hand-edited ronja.json does.
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

// A clone is a faithful copy, so a folder cloned from a workflow that declares
// a calendar manages that calendar — otherwise the first push would silently
// not sync a field the folder can plainly see.
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

// A row that declares NOTHING falls back to the caller's zone at run time — a
// permanent, legitimate state the API cannot express, since an explicit "" is the
// reset to UTC. Writing the key for it would make the very first push stamp UTC
// onto a workflow whose calendar nobody asked to change.
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

// `init` deliberately does NOT write the key, and the asymmetry with
// `SetParameters(nil)` on the line above it is the point: the only value init
// could write without a round trip is "", which means "declare UTC" — so every
// fresh folder would silently override its organization's default. Absent means
// "inherit it".
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
	// The contrast that makes it deliberate rather than forgotten.
	if !m.ManagesParameters() {
		t.Error("init stopped managing parameters — the zone's absence is only correct next to that")
	}
}

// The create carries the declaration, so a first push does not have to create
// the workflow and then immediately patch it onto the right calendar.
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

// POST /workflow refuses an explicit "" (unlike the patch, where it is the
// reset), so a folder declaring the reset has to send the literal UTC on a
// create — the one form the create accepts for the same intent.
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

// An unmanaged folder sends nothing at all, so the server stamps the
// organization default rather than the CLI choosing one for it.
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

// The point of the feature on an existing workflow: the manifest declares the
// calendar and a push makes the row match — onto the DRAFT, never the live row.
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

// The back-compat invariant, and the reason Manifest.ReportingTimezone is a
// pointer: a folder created before the key existed does not manage the calendar,
// so a push must leave the row's declaration completely alone rather than read
// "no key" as "declares UTC" and re-bucket the workflow's output.
func TestPushLeavesTheZoneAloneForAFolderThatDoesNotManageIt(t *testing.T) {
	f := newFakeInstance(t)
	f.AddWorkflow(&api.Workflow{ID: "wf-1", Lifecycle: api.LifecycleLive, ReportingTimezone: "Europe/Stockholm"},
		api.WorkflowFile{Path: "main.py", Content: "M\n"})
	signIn(t, f)
	root := cloneFolder(t, f, "wf-1")

	// Drop the key the clone wrote — this is a pre-zone folder.
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

// A folder declaring the reset and a row already holding the literal UTC AGREE.
// Comparing the raw declaration instead would patch on every single push — ""
// never equals the "UTC" the server just wrote — and no push would ever report
// itself up to date.
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

// status has to report the zone as drift, or the one field a push changes
// without touching a single file is invisible until it has already happened.
func TestStatusReportsZoneDrift(t *testing.T) {
	f := newFakeInstance(t)
	f.AddWorkflow(&api.Workflow{ID: "wf-1", Lifecycle: api.LifecycleLive, ReportingTimezone: "UTC"},
		api.WorkflowFile{Path: "main.py", Content: "M\n"})
	signIn(t, f)
	root := cloneFolder(t, f, "wf-1")

	// Cloned as-is: the folder and the row agree, so nothing is reported.
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
// The whole reason these exist: `reportingTimezone` is an ordinary JSON key on
// POST /workflow and PUT /workflow/:id, and gin binds request bodies WITHOUT
// DisallowUnknownFields — so an instance predating migration 000499 accepts the
// key, drops it, and answers 2xx. Every other field a push writes is confirmed
// by the write itself; this one is accepted, discarded and reported as success.
// A folder would then declare Europe/Stockholm, push clean forever, and every
// run would keep executing on whatever calendar the caller happened to carry.
//
// The fix is a READ-BACK, and its two halves are tested here: the author is
// told, and the unapplied value never reaches the local baseline — so
// `wf status` keeps reporting it as unsynced and a later push against an
// upgraded instance applies it.

// pushBaseline is the folder's recorded baseline for this instance.
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
	// It TRIED — the skew is on the server side, not a CLI that gave up.
	if len(f.timezonePatches) == 0 {
		t.Fatal("push never sent the declared zone at all")
	}
	if !strings.Contains(stderr, "does not support reportingTimezone") {
		t.Errorf("no version-skew warning on stderr; got:\n%s", stderr)
	}
	if decodeJSON(t, out)["reportingTimezoneIgnored"] != true {
		t.Errorf("reportingTimezoneIgnored not reported in --json: %s", out)
	}
	// The half that matters most: the baseline records the SERVER's answer, so
	// the folder does not claim a declaration nobody holds.
	baseline := pushBaseline(t, f, root)
	if baseline.ReportingTimezone == nil {
		t.Fatal("baseline recorded no zone at all — nil is UNRECORDED, which disarms the drift guard")
	}
	if *baseline.ReportingTimezone != "" {
		t.Errorf("baseline recorded %q; the server holds none, and recording the pushed value would make `wf status` report clean forever",
			*baseline.ReportingTimezone)
	}
	// And the report does not claim a change that did not happen.
	if md, _ := decodeJSON(t, out)["metadata"].(string); strings.Contains(md, "timezone") {
		t.Errorf("push reported the zone as changed: %q", md)
	}
}

// The other side of the same read-back: a server that DOES apply it confirms,
// the baseline records it, and the report says what changed.
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
	// describePatch had exactly one call site — the ERROR path — so a push that
	// silently re-declared a workflow's calendar printed nothing at all.
	if !strings.Contains(out, `updated  the reporting timezone to "Europe/Stockholm"`) {
		t.Errorf("the success report does not name the metadata it changed:\n%s", out)
	}
	baseline := pushBaseline(t, f, root)
	if baseline.ReportingTimezone == nil || *baseline.ReportingTimezone != "Europe/Stockholm" {
		t.Errorf("baseline zone = %v, want the confirmed declaration", baseline.ReportingTimezone)
	}
}

// A push that stops AFTER the metadata patch writes a baseline from the
// in-memory row, and that row must carry what the server confirmed rather than
// what we sent. Baking the sent value in would leave a folder whose push
// visibly FAILED nonetheless claiming the calendar landed — and no later push
// would retry it.
func TestPartialPushDoesNotBakeAnUnappliedZoneIntoTheBaseline(t *testing.T) {
	f := newFakeInstance(t)
	f.zoneUnsupported = true
	f.AddWorkflow(&api.Workflow{ID: "wf-1", Lifecycle: api.LifecycleLive},
		api.WorkflowFile{Path: "main.py", Content: "M\n"},
		api.WorkflowFile{Path: "old.py", Content: "O\n"})
	signIn(t, f)
	root := cloneFolder(t, f, "wf-1")
	setManifestZone(t, root, "Europe/Stockholm")
	// old.py is gone locally, and its deletion — the last step of the push —
	// is refused.
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

// The other half of the same window: the patch landed and the READ-BACK failed,
// so the CLI does not know what the row holds.
//
// `target` still carries the PRE-patch zone there (applyPatch deliberately never
// touches the field — the zone is learned from the server, not from what we
// sent), so recording it would leave a baseline describing the world before our
// own successful patch. metadataDriftOf compares the row against that baseline
// on the next push, finds a difference nobody but us caused, and REFUSES —
// blaming a colleague who changed nothing.
//
// The warning already tells the author the zone "is left out of the local
// baseline, so the next push tries again"; this is the code saying the same
// thing. A nil zone reads as "not part of this baseline" in both readers
// (metadataDriftOf and baselineDescribes skip it), so the retry is unarmed.
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
	// The patch is accepted; the confirming read is not. The deletion then fails
	// so the push stops and writes its partial baseline.
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
