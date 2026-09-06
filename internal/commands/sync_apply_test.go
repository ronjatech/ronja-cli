package commands

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/ronjatech/ronja-cli/internal/api"
	"github.com/ronjatech/ronja-cli/internal/wfdir"
)

// `ronja sync apply`.
//
// Every test here is about a REFUSAL, and that is the shape of the command
// rather than a gap in the tests: apply adds no writer of its own, so what is
// worth pinning is the three things it declines to do and the one lie it must
// not tell about a publish.

// applyPipelineTree lays out a tree holding one stack-shaped pipeline folder and
// returns (tree, folder root).
//
// STACK-shaped rather than the legacy instances[] shape every other pipeline
// fixture uses, and deliberately: a legacy folder is one decideApplyFolder
// refuses outright (acting on it rewrites the committed manifest), so a fixture
// built that way would test the manifest-rewrite refusal by accident and nothing
// else.
func applyPipelineTree(t *testing.T, f *fakePipelineInstance, files, bound, live map[string]string) (tree, root string) {
	t.Helper()
	tree = t.TempDir()
	root = filepath.Join(tree, "sales")
	if err := os.MkdirAll(root, 0o755); err != nil {
		t.Fatal(err)
	}
	manifest := &wfdir.Manifest{
		Kind:  wfdir.KindPipeline,
		Title: "Sales pipeline",
		Stacks: map[string]wfdir.Stack{
			"prod": {URL: f.URL(), TenantID: testTenantID, FeatureID: "collection-1"},
		},
	}
	lock := &wfdir.Lock{}
	if len(bound) > 0 {
		lock.SetBinding("prod", wfdir.Binding{Tables: bound})
		for path, tableID := range bound {
			// The LIVE fingerprint the drift guard reads. Defaulting it to the
			// row's own code is the shape a clone leaves behind, and it is what
			// ARMS the guard — an empty one disarms it, so a fixture seeded that
			// way would pass every drift test by never checking.
			code, given := live[path]
			if !given {
				code = f.CodeOf(tableID)
			}
			lock.SetTableLive("prod", path, tableID, wfdir.HashString(code))
		}
	}
	if err := wfdir.SaveFolder(root, manifest, lock); err != nil {
		t.Fatalf("write folder: %v", err)
	}
	for path, content := range files {
		if err := wfdir.WriteFile(root, path, content); err != nil {
			t.Fatalf("write %s: %v", path, err)
		}
	}
	return tree, root
}

func applyReportOf(t *testing.T, out string) *syncApplyReport {
	t.Helper()
	report := &syncApplyReport{}
	decodeJSONInto(t, out, report)
	return report
}

func applyFolderLine(t *testing.T, report *syncApplyReport, path string) syncApplyFolderReport {
	t.Helper()
	for _, folder := range report.Folders {
		if folder.Path == path {
			return folder
		}
	}
	t.Fatalf("no folder %q in %+v", path, report.Folders)
	return syncApplyFolderReport{}
}

// THE HOLE THE FILE-GRAIN GATE CLOSES. A pipeline folder with one bound .sql and
// one new one has a lock row, a featureID and a stack: every folder-grain answer
// calls it "bound" and lets the push through — and runPipelinePush then calls
// CreateTable for the new file, making a live empty table in a live organization
// out of a command that promised to create nothing.
//
// The assertion that matters is f.created, not the exit code: a gate that
// refused after the create would still report a refusal.
func TestSyncApplyRefusesACreateAtFileGrainInABoundFolder(t *testing.T) {
	f := newFakePipelineInstance(t)
	signInPipeline(t, f)
	seedFeature(f, "private")
	tree, _ := applyPipelineTree(t,
		f,
		map[string]string{
			"orders.sql": "SELECT * FROM raw",
			// Bound to nothing. One file, in an otherwise perfectly bound folder.
			"refunds.sql": "SELECT * FROM raw WHERE refunded",
		},
		map[string]string{"orders.sql": "table-orders"},
		nil)

	out, err := runCLI(t, tree, "sync", "apply", "--stack", "prod", "--json")
	// THE ASSERTION THAT MATTERS COMES FIRST, and the exit code last: a gate that
	// refused after the create would still report a refusal.
	if len(f.created) != 0 {
		t.Fatalf("apply created %d table(s) in a live organization: %+v\n%s", len(f.created), f.created, out)
	}
	if code := exitCodeOf(err); code != syncExitDrifted {
		t.Fatalf("exit = %d (%v), want %d\n%s", code, err, syncExitDrifted, out)
	}
	report := applyReportOf(t, out)
	folder := applyFolderLine(t, report, "sales")
	if folder.Verdict != verdictRefused || folder.Reason != syncReasonWouldCreate {
		t.Fatalf("folder = %+v, want %s/%s", folder, verdictRefused, syncReasonWouldCreate)
	}
	if !strings.Contains(folder.Detail, "refunds.sql") {
		t.Errorf("the refusal does not name the file it is about: %q", folder.Detail)
	}
	// And it names the escape, because apply has no --force and the reader is
	// looking at an exit code rather than at this command's help.
	if !strings.Contains(folder.Detail, "pipeline push") {
		t.Errorf("the refusal does not name the per-folder escape: %q", folder.Detail)
	}
	// The unit grain is the machine half of the same claim: one create, one
	// update, in a folder a folder-grain answer would have called bound.
	actions := map[string]string{}
	for _, unit := range folder.Units {
		actions[unit.Path] = unit.Action
	}
	if actions["refunds.sql"] != string(applyActionCreate) || actions["orders.sql"] != string(applyActionUpdate) {
		t.Errorf("units = %+v", folder.Units)
	}
	// Nothing was published either: the gate runs before the loop, not after it.
	if len(f.committed) != 0 {
		t.Errorf("a refused folder still committed %v", f.committed)
	}
}

// A DISARMED DRIFT ANCHOR IS A REFUSAL, NOT AN UPDATE. An empty
// LockAutomation.UpdatedAt turns off both automation guards at once while the
// AutomationID is still there, so the unit looks bound and sails through a
// create gate — and the write would overwrite whatever moved on the server AND
// flip `enabled` false→true on a row somebody paused during an incident.
func TestSyncApplyRefusesAnUnanchoredAutomation(t *testing.T) {
	f := newFakeAutomationInstance(t)
	signInAutomation(t, f)
	// PAUSED on the server, which is the state the re-enable guard exists for:
	// the file says enabled, the row says no, and nothing in the folder records
	// when the two last agreed.
	f.AddAutomation(&api.Automation{ID: "job-1", Name: "nightly", CronExpr: "0 2 * * *", Enabled: false})

	tree := t.TempDir()
	root := filepath.Join(tree, "orders")
	if err := os.MkdirAll(root, 0o755); err != nil {
		t.Fatal(err)
	}
	manifest, lock := automationStack(f, "collection-1",
		map[string]string{"nightly.json": "job-1"},
		// No anchor. This is the whole fixture.
		map[string]string{"nightly.json": ""})
	// A VALID declaration that really would be written: it turns the row back on
	// and moves the schedule. Without that the push would refuse on the FILE and
	// this test would pass whether or not the anchor guard existed at all.
	writeAutomationFolder(t, root, manifest, lock, map[string]string{
		"nightly.json": `{"enabled": true, "cronExpr": "0 3 * * *"}`,
	})

	out, err := runCLI(t, tree, "sync", "apply", "--stack", "prod", "--json")
	// THE ASSERTIONS THAT MATTER COME FIRST, and the exit code last: a gate that
	// refused after the write would still report a refusal.
	if len(f.updates) != 0 {
		t.Fatalf("apply wrote an unanchored automation: %+v\n%s", f.updates, out)
	}
	if row := f.RowOf("job-1"); row.Enabled {
		t.Fatalf("apply re-enabled an automation somebody had paused\n%s", out)
	}
	if code := exitCodeOf(err); code != syncExitDrifted {
		t.Fatalf("exit = %d (%v), want %d\n%s", code, err, syncExitDrifted, out)
	}
	folder := applyFolderLine(t, applyReportOf(t, out), "orders")
	if folder.Verdict != verdictRefused || folder.Reason != syncReasonNoDriftAnchor {
		t.Fatalf("folder = %+v, want %s/%s", folder, verdictRefused, syncReasonNoDriftAnchor)
	}
	if len(folder.Units) != 1 || folder.Units[0].Action != string(applyActionUnanchored) {
		t.Errorf("units = %+v, want one unanchored unit", folder.Units)
	}
}

// MIGRATION IS A PER-FOLDER ACT SOMEBODY WATCHES. A lock recording a stack the
// manifest does not declare is one resolveStackForFolder deliberately accepts —
// selectNamed adopts it — and adoptStack then WRITES that declaration into a
// committed file. A tree-wide command must not do that in thirty folders nobody
// is looking at, so it refuses; and the proof is the file on disk.
func TestSyncApplyRefusesAManifestRewriteAndLeavesTheFileAlone(t *testing.T) {
	f := newFakePipelineInstance(t)
	signInPipeline(t, f)
	seedFeature(f, "private")
	tree, root := applyPipelineTree(t, f,
		map[string]string{"orders.sql": "SELECT * FROM raw"},
		map[string]string{"orders.sql": "table-orders"},
		nil)
	// Strand the lock stack: the lock records "prod" and the manifest declares
	// only "dev", which is the state a push whose lock write landed and whose
	// manifest write did not leaves behind.
	manifest := pipelineManifestOf(t, root)
	manifest.Stacks = map[string]wfdir.Stack{
		"dev": {URL: f.URL(), TenantID: testTenantID, FeatureID: "collection-1"},
	}
	if err := wfdir.SaveManifest(root, manifest); err != nil {
		t.Fatal(err)
	}
	before, err := os.ReadFile(wfdir.ManifestPath(root))
	if err != nil {
		t.Fatal(err)
	}

	out, runErr := runCLI(t, tree, "sync", "apply", "--stack", "prod", "--json")
	if code := exitCodeOf(runErr); code != syncExitDrifted {
		t.Fatalf("exit = %d (%v), want %d\n%s", code, runErr, syncExitDrifted, out)
	}
	after, err := os.ReadFile(wfdir.ManifestPath(root))
	if err != nil {
		t.Fatal(err)
	}
	// BYTE-IDENTICAL. Anything else is a committed file this command edited as a
	// side effect of deploying, which is the failure the refusal exists for.
	if string(before) != string(after) {
		t.Fatalf("apply rewrote a committed manifest:\nbefore %s\nafter  %s", before, after)
	}
	folder := applyFolderLine(t, applyReportOf(t, out), "sales")
	if folder.Verdict != verdictRefused || folder.Reason != syncReasonManifestRewrite {
		t.Fatalf("folder = %+v, want %s/%s", folder, verdictRefused, syncReasonManifestRewrite)
	}
}

// A REVIEW REQUEST IS NOT A DEPLOY, and the naive version of this command files
// thirty of them and exits zero.
//
// publishRouting sends a non-admin caller on a SHARED feature to request-review,
// and runPipelinePublish counts only `refused` and `conflict` as failed — so
// `submitted_for_review` comes back with a nil error and reads as success. Apply
// passes --no-request-review, which turns that route into an outright failure.
func TestSyncApplyDoesNotCountAReviewRequestAsADeploy(t *testing.T) {
	f := newFakePipelineInstance(t)
	signInPipeline(t, f)
	seedFeature(f, "organization")
	// An ordinary user, which is what a CI credential usually is. Privilege
	// levels count DOWN; 50 is the fake's default and is not admin.
	f.privilegeLevel = 50
	tree, _ := applyPipelineTree(t, f,
		// EDITED, so the push has something to do and a draft to publish.
		map[string]string{"orders.sql": "SELECT * FROM raw WHERE shipped"},
		map[string]string{"orders.sql": "table-orders"},
		nil)

	out, err := runCLI(t, tree, "sync", "apply", "--stack", "prod", "--json")
	if code := exitCodeOf(err); code != syncExitDrifted {
		t.Fatalf("exit = %d (%v), want %d\n%s", code, err, syncExitDrifted, out)
	}
	// The first half: nothing was filed. An apply that spams an admin's inbox
	// once per folder is the behaviour --no-request-review exists to prevent.
	if len(f.reviewRequested) != 0 {
		t.Fatalf("apply filed a review request: %v", f.reviewRequested)
	}
	// The second half: the folder is NOT reported as deployed. The live table
	// still holds what it held.
	folder := applyFolderLine(t, applyReportOf(t, out), "sales")
	if folder.Verdict != verdictRefused {
		t.Fatalf("folder = %+v, want %s", folder, verdictRefused)
	}
	if f.CodeOf("table-orders") != "SELECT * FROM raw" {
		t.Errorf("the commit landed despite the governance gate: %q", f.CodeOf("table-orders"))
	}
}

// THE SCAN --no-request-review DOES NOT COVER, and the one data-app kind under
// apply at all.
//
// --no-request-review closes publishRouting's leg, so it is tempting to read the
// `submitted_for_review` scan in applyFolder as defensive. It is not:
// runAppPublish sets that outcome a SECOND way, on the path where the commit
// SUCCEEDS and the server answers `proposed` — a first publish of an app into a
// shared feature by a non-admin, with a nil commitErr and past no
// noRequestReview guard at all. Without the scan apply reports `applied` for an
// app that only became somebody's proposal.
//
// The folder here is bound to a never-published draft, which is what `app push`
// leaves behind after it creates one; `--allow-create` reaches the same branch
// by making that row itself.
func TestSyncApplyDoesNotCallAProposedDataAppDeployed(t *testing.T) {
	f := newFakeAppInstance(t)
	f.AddApp(&api.DataApp{ID: "data_app-new-1", Lifecycle: api.LifecycleDraft},
		api.DataAppFile{Path: "App.tsx", Content: "const x = 1;\n"})
	f.featureScope["feat-1"] = "organization"
	f.privilegeLevel = 50 // an ordinary user, which is what a CI credential is
	f.commitProposes = true
	signInApp(t, f)

	tree := t.TempDir()
	root := filepath.Join(tree, "explorer")
	if err := os.MkdirAll(root, 0o755); err != nil {
		t.Fatal(err)
	}
	manifest := &wfdir.Manifest{
		Kind: wfdir.KindDataApp, Title: "Revenue explorer",
		Stacks: map[string]wfdir.Stack{
			"prod": {URL: f.URL(), TenantID: testTenantID, FeatureID: "feat-1"},
		},
	}
	// EDITED against the row, so the push has something to send and a draft to
	// publish — otherwise the publish never runs and the scan is never reached.
	writeAppFolder(t, root, manifest, map[string]string{"App.tsx": "const x = 2;\n"})
	lock := &wfdir.Lock{}
	lock.SetBinding("prod", wfdir.Binding{DataAppID: "data_app-new-1", FeatureID: "feat-1"})
	if err := wfdir.SaveLock(root, lock); err != nil {
		t.Fatal(err)
	}
	// The sync baseline the push compares against — the row's own content, which
	// is the state a clone leaves behind. Without one the push refuses for a
	// different (correct) reason and never reaches the publish.
	state := &wfdir.State{}
	state.Set(f.Key(), baselineFromApp(aliasCodec{}, f.apps["data_app-new-1"],
		[]api.DataAppFile{{Path: "App.tsx", Content: "const x = 1;\n"}}))
	if err := wfdir.SaveState(root, state); err != nil {
		t.Fatal(err)
	}

	out, err := runCLI(t, tree, "sync", "apply", "--stack", "prod", "--json")
	if code := exitCodeOf(err); code != syncExitDrifted {
		t.Fatalf("exit = %d (%v), want %d\n%s", code, err, syncExitDrifted, out)
	}
	// The commit really did happen and really did answer 200 — which is what makes
	// reading only the error a lie rather than a missing case.
	if len(f.committed) != 1 {
		t.Fatalf("committed = %v, want the one commit the server answered `proposed` to\n%s", f.committed, out)
	}
	folder := applyFolderLine(t, applyReportOf(t, out), "explorer")
	if folder.Verdict != verdictRefused || folder.Reason != syncReasonSubmittedForReview {
		t.Fatalf("folder = %+v, want %s/%s", folder, verdictRefused, syncReasonSubmittedForReview)
	}
}

// The ordinary path, which everything above is a refusal of: a bound folder with
// an edited file is pushed AND published, so what comes out is live rather than
// a staged draft.
func TestSyncApplyPushesAndPublishes(t *testing.T) {
	f := newFakePipelineInstance(t)
	signInPipeline(t, f)
	seedFeature(f, "private")
	tree, _ := applyPipelineTree(t, f,
		map[string]string{"orders.sql": "SELECT * FROM raw WHERE shipped"},
		map[string]string{"orders.sql": "table-orders"},
		nil)

	out, err := runCLI(t, tree, "sync", "apply", "--stack", "prod", "--json")
	if code := exitCodeOf(err); code != syncExitClean {
		t.Fatalf("exit = %d (%v), want %d\n%s", code, err, syncExitClean, out)
	}
	// PUBLISHED, not merely pushed. A push alone leaves a draft, and an apply
	// that stopped there would print success over a repository nobody can see.
	if len(f.committed) != 1 {
		t.Fatalf("committed = %v, want one draft", f.committed)
	}
	if f.CodeOf("table-orders") != "SELECT * FROM raw WHERE shipped" {
		t.Errorf("the live table does not hold the folder's SQL: %q", f.CodeOf("table-orders"))
	}
	report := applyReportOf(t, out)
	if report.Verdict != verdictApplied {
		t.Fatalf("verdict = %q, want %q", report.Verdict, verdictApplied)
	}
	// The report names the order it used, because a CI log that does not leaves
	// the reader guessing whether a failure was an ordering problem.
	if report.Order != "path" {
		t.Errorf("order = %q", report.Order)
	}
}

// A LEGACY FOLDER DEPLOYS. The `instances[]` shape is the one every repository
// written before stacks is in, and apply has to be able to keep one up to date:
// Manifest.Record's !sel.Named() branch calls SetBinding in place and returns
// BEFORE the instances[] drop, so a push here migrates nothing and there is no
// committed-file rewrite to refuse.
//
// It was refused anyway, and with no route out — manifest_rewrite with no
// --stack, unnamed_stack (exit 2) with one — so the whole legacy half of the
// world was locked out of the deploy verb.
func TestSyncApplyDeploysALegacyInstancesFolder(t *testing.T) {
	f := newFakePipelineInstance(t)
	signInPipeline(t, f)
	seedFeature(f, "private")

	tree := t.TempDir()
	root := filepath.Join(tree, "sales")
	if err := os.MkdirAll(root, 0o755); err != nil {
		t.Fatal(err)
	}
	// No "stacks" at all, and the binding in ronja.json where the v1 format keeps
	// it. This is the shape, not an approximation of it.
	manifest := &wfdir.Manifest{Kind: wfdir.KindPipeline, Title: "Sales pipeline"}
	manifest.SetBinding(wfdir.InstanceKey{URL: f.URL(), TenantID: testTenantID},
		wfdir.Binding{FeatureID: "collection-1", Tables: map[string]string{"orders.sql": "table-orders"}})
	if err := wfdir.SaveManifest(root, manifest); err != nil {
		t.Fatal(err)
	}
	if err := wfdir.WriteFile(root, "orders.sql", "SELECT * FROM raw WHERE shipped"); err != nil {
		t.Fatal(err)
	}

	// NO --stack: a legacy folder names none, and the selection apply makes for it
	// is the unnamed one Record leaves alone.
	out, err := runCLI(t, tree, "sync", "apply", "--json")
	if code := exitCodeOf(err); code != syncExitClean {
		t.Fatalf("exit = %d (%v), want %d\n%s", code, err, syncExitClean, out)
	}
	folder := applyFolderLine(t, applyReportOf(t, out), "sales")
	if folder.Verdict != verdictApplied {
		t.Fatalf("folder = %+v, want %s", folder, verdictApplied)
	}
	// Deployed, not merely reported green: the live table holds the folder's SQL.
	if f.CodeOf("table-orders") != "SELECT * FROM raw WHERE shipped" {
		t.Errorf("the live table does not hold the folder's SQL: %q", f.CodeOf("table-orders"))
	}
	if len(f.committed) != 1 {
		t.Errorf("committed = %v, want the draft published", f.committed)
	}
}

// The other half of the same rule: a folder that declares a stack AND still
// keeps a legacy entry for the place that stack names is refused, because
// Manifest.Record's NAMED branch really does drop that entry.
func TestSyncApplyRefusesALegacyEntryBesideTheNamedStack(t *testing.T) {
	f := newFakePipelineInstance(t)
	signInPipeline(t, f)
	seedFeature(f, "private")
	tree, root := applyPipelineTree(t, f,
		map[string]string{"orders.sql": "SELECT * FROM raw WHERE shipped"},
		map[string]string{"orders.sql": "table-orders"},
		nil)
	manifest := pipelineManifestOf(t, root)
	manifest.SetBinding(wfdir.InstanceKey{URL: f.URL(), TenantID: testTenantID},
		wfdir.Binding{FeatureID: "collection-1"})
	if err := wfdir.SaveManifest(root, manifest); err != nil {
		t.Fatal(err)
	}
	before, err := os.ReadFile(wfdir.ManifestPath(root))
	if err != nil {
		t.Fatal(err)
	}

	out, runErr := runCLI(t, tree, "sync", "apply", "--stack", "prod", "--json")
	if code := exitCodeOf(runErr); code != syncExitDrifted {
		t.Fatalf("exit = %d (%v), want %d\n%s", code, runErr, syncExitDrifted, out)
	}
	after, err := os.ReadFile(wfdir.ManifestPath(root))
	if err != nil {
		t.Fatal(err)
	}
	if string(before) != string(after) {
		t.Fatalf("apply rewrote a committed manifest:\nbefore %s\nafter  %s", before, after)
	}
	folder := applyFolderLine(t, applyReportOf(t, out), "sales")
	if folder.Verdict != verdictRefused || folder.Reason != syncReasonManifestRewrite {
		t.Fatalf("folder = %+v, want %s/%s", folder, verdictRefused, syncReasonManifestRewrite)
	}
}

// --dry-run comes off apply's OWN derivation, so the two cannot disagree about
// what apply will attempt — which is the whole reason the decision was extracted
// before the verb was written.
//
// Asserted as agreement between two RUNS rather than by inspecting one: a
// dry-run that re-derived independently would still look right on its own.
func TestSyncApplyDryRunAgreesWithTheApply(t *testing.T) {
	f := newFakePipelineInstance(t)
	signInPipeline(t, f)
	seedFeature(f, "private")
	// Two folders with opposite answers, so the agreement is about something: one
	// deploys, one is refused for a create at file grain.
	tree, _ := applyPipelineTree(t, f,
		map[string]string{"orders.sql": "SELECT * FROM raw WHERE shipped"},
		map[string]string{"orders.sql": "table-orders"},
		nil)
	second := filepath.Join(tree, "marts")
	if err := os.MkdirAll(second, 0o755); err != nil {
		t.Fatal(err)
	}
	manifest := &wfdir.Manifest{
		Kind: wfdir.KindPipeline, Title: "Marts",
		Stacks: map[string]wfdir.Stack{
			"prod": {URL: f.URL(), TenantID: testTenantID, FeatureID: "collection-1"},
		},
	}
	if err := wfdir.SaveFolder(second, manifest, &wfdir.Lock{}); err != nil {
		t.Fatal(err)
	}
	if err := wfdir.WriteFile(second, "revenue.sql", "SELECT 1"); err != nil {
		t.Fatal(err)
	}

	dry, dryErr := runCLI(t, tree, "sync", "apply", "--stack", "prod", "--dry-run", "--json")
	if code := exitCodeOf(dryErr); code != syncExitDrifted {
		t.Fatalf("dry-run exit = %d (%v), want %d\n%s", code, dryErr, syncExitDrifted, dry)
	}
	// A DRY RUN WRITES NOTHING. Not a create, not a checkout, not a commit.
	if len(f.created) != 0 || len(f.checkouts) != 0 || len(f.committed) != 0 {
		t.Fatalf("--dry-run wrote: created=%v checkouts=%v committed=%v", f.created, f.checkouts, f.committed)
	}
	dryReport := applyReportOf(t, dry)
	if !dryReport.DryRun {
		t.Error("the report does not say it was a dry run, so a reader cannot tell a preview from a deploy")
	}

	wet, wetErr := runCLI(t, tree, "sync", "apply", "--stack", "prod", "--json")
	if exitCodeOf(wetErr) != exitCodeOf(dryErr) {
		t.Fatalf("dry-run exit %d, apply exit %d — a preview that disagrees with the run is worse than none",
			exitCodeOf(dryErr), exitCodeOf(wetErr))
	}
	wetReport := applyReportOf(t, wet)
	if len(dryReport.Folders) != len(wetReport.Folders) {
		t.Fatalf("dry-run saw %d folders, apply saw %d", len(dryReport.Folders), len(wetReport.Folders))
	}
	for i, dryFolder := range dryReport.Folders {
		wetFolder := wetReport.Folders[i]
		if dryFolder.Path != wetFolder.Path {
			t.Fatalf("order differs: dry %q, apply %q", dryFolder.Path, wetFolder.Path)
		}
		if dryFolder.Verdict != wetFolder.Verdict {
			t.Errorf("%s: dry-run said %q, apply said %q", dryFolder.Path, dryFolder.Verdict, wetFolder.Verdict)
		}
		if len(dryFolder.Units) != len(wetFolder.Units) {
			t.Fatalf("%s: dry-run listed %d units, apply listed %d", dryFolder.Path, len(dryFolder.Units), len(wetFolder.Units))
		}
		for j, unit := range dryFolder.Units {
			if unit.Action != wetFolder.Units[j].Action || unit.Path != wetFolder.Units[j].Path {
				t.Errorf("%s: unit %d differs — dry %+v, apply %+v", dryFolder.Path, j, unit, wetFolder.Units[j])
			}
		}
	}
	// And the refusal really is the create one, so the agreement above is about
	// the answer this command exists to give.
	if refused := applyFolderLine(t, wetReport, "marts"); refused.Reason != syncReasonWouldCreate {
		t.Errorf("marts = %+v, want %s", refused, syncReasonWouldCreate)
	}
}

// A folder that refuses does not stop the run. With creates refused by default
// the blast radius of carrying on is bounded, which is the argument the first
// draft of this design was missing.
func TestSyncApplyCarriesOnPastARefusal(t *testing.T) {
	f := newFakePipelineInstance(t)
	signInPipeline(t, f)
	seedFeature(f, "private")
	// `aaa` sorts first and refuses; `sales` sorts after it and must still be
	// deployed, which is what the assertion is about.
	tree, _ := applyPipelineTree(t, f,
		map[string]string{"orders.sql": "SELECT * FROM raw WHERE shipped"},
		map[string]string{"orders.sql": "table-orders"},
		nil)
	first := filepath.Join(tree, "aaa")
	if err := os.MkdirAll(first, 0o755); err != nil {
		t.Fatal(err)
	}
	manifest := &wfdir.Manifest{
		Kind: wfdir.KindPipeline, Title: "New",
		Stacks: map[string]wfdir.Stack{
			"prod": {URL: f.URL(), TenantID: testTenantID, FeatureID: "collection-1"},
		},
	}
	if err := wfdir.SaveFolder(first, manifest, &wfdir.Lock{}); err != nil {
		t.Fatal(err)
	}
	if err := wfdir.WriteFile(first, "new.sql", "SELECT 1"); err != nil {
		t.Fatal(err)
	}

	out, err := runCLI(t, tree, "sync", "apply", "--stack", "prod", "--json")
	if code := exitCodeOf(err); code != syncExitDrifted {
		t.Fatalf("exit = %d (%v), want %d\n%s", code, err, syncExitDrifted, out)
	}
	report := applyReportOf(t, out)
	if applyFolderLine(t, report, "aaa").Verdict != verdictRefused {
		t.Errorf("aaa = %+v", applyFolderLine(t, report, "aaa"))
	}
	if got := applyFolderLine(t, report, "sales"); got.Verdict != verdictApplied {
		t.Fatalf("the run stopped at the first refusal: sales = %+v", got)
	}
	if len(f.committed) != 1 {
		t.Errorf("committed = %v, want the folder after the refusal to have deployed", f.committed)
	}
	// Exactly one refusal and one deploy, and the SUMMARY has to say so — the
	// number a CI reader acts on.
	if report.Summary.Refused != 1 || report.Summary.Applied != 1 {
		t.Errorf("summary = %+v", report.Summary)
	}
}

// A --stack nothing in the tree recognises is never green, and is `unknown`
// rather than `refused`: nothing was checked, and a job that read a refusal
// would go looking for a problem in the repository.
func TestSyncApplyUnknownStackIsNeverGreen(t *testing.T) {
	f := newFakePipelineInstance(t)
	signInPipeline(t, f)
	seedFeature(f, "private")
	tree, _ := applyPipelineTree(t, f,
		map[string]string{"orders.sql": "SELECT * FROM raw"},
		map[string]string{"orders.sql": "table-orders"},
		nil)

	out, err := runCLI(t, tree, "sync", "apply", "--stack", "nope", "--json")
	if code := exitCodeOf(err); code != syncExitUnknown {
		t.Fatalf("exit = %d (%v), want %d\n%s", code, err, syncExitUnknown, out)
	}
	if len(f.created) != 0 || len(f.committed) != 0 {
		t.Fatalf("a folder nothing selected was written to: created=%v committed=%v", f.created, f.committed)
	}
	folder := applyFolderLine(t, applyReportOf(t, out), "sales")
	if folder.Verdict != verdictUnknown || folder.Reason != syncReasonStackAbsent {
		t.Errorf("folder = %+v, want %s/%s", folder, verdictUnknown, syncReasonStackAbsent)
	}
}

// An empty walk is unknown, never green — the same rule as `sync status`. A
// renamed directory must not report a deploy that never touched anything.
func TestSyncApplyEmptyWalkIsUnknown(t *testing.T) {
	f := newFakePipelineInstance(t)
	signInPipeline(t, f)

	out, err := runCLI(t, t.TempDir(), "sync", "apply", "--json")
	if code := exitCodeOf(err); code != syncExitUnknown {
		t.Fatalf("exit = %d (%v), want %d\n%s", code, err, syncExitUnknown, out)
	}
	if applyReportOf(t, out).Verdict != verdictUnknown {
		t.Errorf("verdict = %q", applyReportOf(t, out).Verdict)
	}
}

// --- --allow-create ---------------------------------------------------------

// The opt-in creates, and NAMES THE COUNT FIRST — in rows, before the first
// write. Rows rather than folders because they are different numbers: Tables and
// Automations are per-file maps, so a merge adding one file to each of thirty
// folders is thirty rows, and volume is what makes this dangerous.
func TestSyncApplyAllowCreateNamesTheRowCountBeforeCreating(t *testing.T) {
	f := newFakePipelineInstance(t)
	signInPipeline(t, f)
	seedFeature(f, "private")
	// ONE folder, TWO new files. A count that said "1" here would be the folder
	// count, which is the number this flag must not print.
	tree, _ := applyPipelineTree(t, f,
		map[string]string{
			"orders.sql":  "SELECT * FROM raw",
			"refunds.sql": "SELECT * FROM raw WHERE refunded",
			"returns.sql": "SELECT * FROM raw WHERE returned",
		},
		map[string]string{"orders.sql": "table-orders"},
		nil)

	var out string
	var err error
	stderr := captureStderr(t, func() {
		out, err = runCLI(t, tree, "sync", "apply", "--stack", "prod", "--allow-create", "--json")
	})
	if code := exitCodeOf(err); code != syncExitClean {
		t.Fatalf("exit = %d (%v), want %d\n%s\n%s", code, err, syncExitClean, out, stderr)
	}
	if len(f.created) != 2 {
		t.Fatalf("created = %+v, want the two unbound files", f.created)
	}
	// TWO ROWS, not one folder, and the organization it is about to make them in.
	if !strings.Contains(stderr, "will create 2 rows") {
		t.Errorf("the count was not named before the writes:\n%s", stderr)
	}
	if !strings.Contains(stderr, testTenantID) {
		t.Errorf("the organization was not named:\n%s", stderr)
	}
	report := applyReportOf(t, out)
	if !report.AllowCreate {
		t.Error("a run that created rows looks identical to one that did not")
	}
	if applyFolderLine(t, report, "sales").Verdict != verdictApplied {
		t.Errorf("folder = %+v", applyFolderLine(t, report, "sales"))
	}
}

// The flag opts into ONE refusal. A disarmed automation anchor is not about a
// row that does not exist, and --allow-create must not quietly relax it — a
// single flag that loosened all three would be one nobody could reason about
// from its name.
func TestSyncApplyAllowCreateDoesNotUnblockAnUnanchoredAutomation(t *testing.T) {
	f := newFakeAutomationInstance(t)
	signInAutomation(t, f)
	f.AddAutomation(&api.Automation{ID: "job-1", Name: "nightly", CronExpr: "0 2 * * *", Enabled: false})

	tree := t.TempDir()
	root := filepath.Join(tree, "orders")
	if err := os.MkdirAll(root, 0o755); err != nil {
		t.Fatal(err)
	}
	manifest, lock := automationStack(f, "collection-1",
		map[string]string{"nightly.json": "job-1"},
		map[string]string{"nightly.json": ""})
	writeAutomationFolder(t, root, manifest, lock, map[string]string{
		"nightly.json": `{"enabled": true, "cronExpr": "0 3 * * *"}`,
	})

	out, err := runCLI(t, tree, "sync", "apply", "--stack", "prod", "--allow-create", "--json")
	if len(f.updates) != 0 {
		t.Fatalf("--allow-create wrote an unanchored automation: %+v\n%s", f.updates, out)
	}
	if row := f.RowOf("job-1"); row.Enabled {
		t.Fatalf("--allow-create re-enabled a paused automation\n%s", out)
	}
	if code := exitCodeOf(err); code != syncExitDrifted {
		t.Fatalf("exit = %d (%v), want %d\n%s", code, err, syncExitDrifted, out)
	}
	if got := applyFolderLine(t, applyReportOf(t, out), "orders"); got.Reason != syncReasonNoDriftAnchor {
		t.Errorf("folder = %+v, want %s", got, syncReasonNoDriftAnchor)
	}
}

// The count is named BEFORE the first write, which is only meaningful if it is
// the count for the WHOLE TREE: thirty small numbers announced folder by folder
// is not a number anybody adds up, and volume is the thing the flag exists to
// make visible.
func TestSyncApplyAllowCreateCountsTheWholeTreeBeforeWriting(t *testing.T) {
	f := newFakePipelineInstance(t)
	signInPipeline(t, f)
	seedFeature(f, "private")
	tree := t.TempDir()
	for _, name := range []string{"a", "b"} {
		root := filepath.Join(tree, name)
		if err := os.MkdirAll(root, 0o755); err != nil {
			t.Fatal(err)
		}
		manifest := &wfdir.Manifest{
			Kind: wfdir.KindPipeline, Title: name,
			Stacks: map[string]wfdir.Stack{
				"prod": {URL: f.URL(), TenantID: testTenantID, FeatureID: "collection-1"},
			},
		}
		if err := wfdir.SaveFolder(root, manifest, &wfdir.Lock{}); err != nil {
			t.Fatal(err)
		}
		if err := wfdir.WriteFile(root, "one.sql", "SELECT 1"); err != nil {
			t.Fatal(err)
		}
		if err := wfdir.WriteFile(root, "two.sql", "SELECT 2"); err != nil {
			t.Fatal(err)
		}
	}

	var out string
	var err error
	stderr := captureStderr(t, func() {
		out, err = runCLI(t, tree, "sync", "apply", "--stack", "prod", "--allow-create", "--json")
	})
	if code := exitCodeOf(err); code != syncExitClean {
		t.Fatalf("exit = %d (%v), want %d\n%s\n%s", code, err, syncExitClean, out, stderr)
	}
	// Four rows across two folders, in ONE sentence, and it must come before the
	// first create in the stream.
	if !strings.Contains(stderr, "will create 4 rows") {
		t.Fatalf("the tree-wide count was not named:\n%s", stderr)
	}
	if strings.Contains(stderr, "will create 2 rows") {
		t.Errorf("the count was announced per folder rather than for the tree:\n%s", stderr)
	}
	if len(f.created) != 4 {
		t.Errorf("created = %d rows, want 4", len(f.created))
	}
}

// --dry-run --allow-create previews the creates and still writes nothing, so the
// number can be read before anybody commits to it.
func TestSyncApplyAllowCreateDryRunWritesNothing(t *testing.T) {
	f := newFakePipelineInstance(t)
	signInPipeline(t, f)
	seedFeature(f, "private")
	tree, _ := applyPipelineTree(t, f,
		map[string]string{"orders.sql": "SELECT * FROM raw", "refunds.sql": "SELECT 1"},
		map[string]string{"orders.sql": "table-orders"},
		nil)

	var out string
	var err error
	stderr := captureStderr(t, func() {
		out, err = runCLI(t, tree, "sync", "apply", "--stack", "prod", "--allow-create", "--dry-run", "--json")
	})
	if code := exitCodeOf(err); code != syncExitClean {
		t.Fatalf("exit = %d (%v), want %d\n%s\n%s", code, err, syncExitClean, out, stderr)
	}
	if len(f.created) != 0 || len(f.committed) != 0 {
		t.Fatalf("--dry-run wrote: created=%v committed=%v", f.created, f.committed)
	}
	if !strings.Contains(stderr, "would create 1 row") {
		t.Errorf("the dry-run did not preview the count, or claimed it had made them:\n%s", stderr)
	}
}

// `Created` IS HISTORY, NOT THE PLAN. It was assigned from the pre-pass before
// the execute loop ran, so a folder that cleared the gate and then failed
// mid-push still reported its planned rows as created — "2 rows created" about a
// run that made none, on the line an operator reads first.
func TestSyncApplyCreatedCountsWhatLanded(t *testing.T) {
	f := newFakePipelineInstance(t)
	signInPipeline(t, f)
	seedFeature(f, "private")
	// Every create is refused by the instance. The PLAN still says two rows,
	// which is the point: the two numbers must not be the same number.
	f.failCreate = 403
	tree, _ := applyPipelineTree(t, f,
		map[string]string{"orders.sql": "SELECT 1", "refunds.sql": "SELECT 2"},
		nil, nil)

	var out string
	var err error
	stderr := captureStderr(t, func() {
		out, err = runCLI(t, tree, "sync", "apply", "--stack", "prod", "--allow-create", "--json")
	})
	if code := exitCodeOf(err); code != syncExitDrifted {
		t.Fatalf("exit = %d (%v), want %d\n%s\n%s", code, err, syncExitDrifted, out, stderr)
	}
	summary := applyReportOf(t, out).Summary
	if summary.Creates != 2 {
		t.Fatalf("summary = %+v, want the two creation units the tree holds", summary)
	}
	if summary.Created != 0 {
		t.Errorf("summary = %+v — apply reported rows created that the instance refused", summary)
	}
}

// THE HUMAN REPORT, which no other test here reads: `--json` skips
// printSyncApply entirely, so its sentences were never run.
//
// Two things it must not say. It must not tell somebody who just passed
// --allow-create to re-run with --allow-create — the report arguing with the
// command line it was given — and, per creation unit, a dry-run of an all-create
// folder must not say a push would write nothing immediately before it writes
// rows.
func TestSyncApplyHumanReportDoesNotArgueWithTheFlagItWasGiven(t *testing.T) {
	f := newFakePipelineInstance(t)
	signInPipeline(t, f)
	seedFeature(f, "private")
	f.failCreate = 403
	tree, _ := applyPipelineTree(t, f, map[string]string{"orders.sql": "SELECT 1"}, nil, nil)

	var out string
	captureStderr(t, func() {
		out, _ = runCLI(t, tree, "sync", "apply", "--stack", "prod", "--allow-create")
	})
	if strings.Contains(out, "re-run with --allow-create") {
		t.Errorf("the report told a caller who passed --allow-create to pass --allow-create:\n%s", out)
	}
	if !strings.Contains(out, "NOT created") {
		t.Errorf("the report does not say the rows were not created:\n%s", out)
	}

	// And the other way round: with no flag, the escape IS the flag.
	plain, _ := runCLI(t, tree, "sync", "apply", "--stack", "prod")
	if !strings.Contains(plain, "re-run with --allow-create") {
		t.Errorf("the default report does not name the escape:\n%s", plain)
	}
}

// --dry-run --allow-create must describe the creates. describeAttempt counted
// only updates, so an all-create folder previewed as "nothing here is bound to a
// row, so a push would write nothing" — and under --json that sentence is the
// folder line's only prose.
func TestSyncApplyDryRunDescribesTheCreatesItWouldMake(t *testing.T) {
	f := newFakePipelineInstance(t)
	signInPipeline(t, f)
	seedFeature(f, "private")
	tree, _ := applyPipelineTree(t, f,
		map[string]string{"orders.sql": "SELECT 1", "refunds.sql": "SELECT 2"},
		nil, nil)

	var out string
	captureStderr(t, func() {
		out, _ = runCLI(t, tree, "sync", "apply", "--stack", "prod", "--allow-create", "--dry-run", "--json")
	})
	folder := applyFolderLine(t, applyReportOf(t, out), "sales")
	if strings.Contains(folder.Detail, "write nothing") {
		t.Fatalf("the preview of an all-create folder says a push would write nothing: %q", folder.Detail)
	}
	if !strings.Contains(folder.Detail, "create 2 new rows") {
		t.Errorf("the preview does not name the rows it would make: %q", folder.Detail)
	}
}

// An automation folder has NO PUBLISH — its loop writes production directly —
// so the preview must not promise one.
func TestSyncApplyDryRunDoesNotPromiseAnAutomationPublish(t *testing.T) {
	f := newFakeAutomationInstance(t)
	signInAutomation(t, f)
	f.AddAutomation(&api.Automation{ID: "job-1", Name: "nightly", CronExpr: "0 2 * * *", Enabled: true})

	tree := t.TempDir()
	root := filepath.Join(tree, "orders")
	if err := os.MkdirAll(root, 0o755); err != nil {
		t.Fatal(err)
	}
	manifest, lock := automationStack(f, "collection-1",
		map[string]string{"nightly.json": "job-1"},
		map[string]string{"nightly.json": "2026-08-01T00:00:00Z"})
	writeAutomationFolder(t, root, manifest, lock, map[string]string{
		"nightly.json": `{"enabled": true, "cronExpr": "0 3 * * *"}`,
	})

	out, _ := runCLI(t, tree, "sync", "apply", "--stack", "prod", "--dry-run", "--json")
	folder := applyFolderLine(t, applyReportOf(t, out), "orders")
	if strings.Contains(folder.Detail, "publish") {
		t.Errorf("the preview promises a publish this kind does not have: %q", folder.Detail)
	}
}

// A folder the run never reached is `unknown`, and the report is FINISHED rather
// than discarded. Returning an error instead printed nothing at all — --json
// included — for the one state that most needs a report: a tree 20 folders into
// 30, part live and part not.
func TestMarkApplyInterruptedKeepsLocalAnswersAndReportsTheRest(t *testing.T) {
	rest := []plannedFolder{
		{line: syncApplyFolderReport{Path: "a", Verdict: verdictRefused, Reason: syncReasonWouldCreate, Detail: "would create"}},
		{line: syncApplyFolderReport{Path: "b"}},
	}
	markApplyInterrupted(rest)
	// A folder already answered locally keeps its answer: the refusal is true
	// whether or not the run got as far as it.
	if rest[0].line.Verdict != verdictRefused || rest[0].line.Reason != syncReasonWouldCreate {
		t.Errorf("a local refusal was overwritten: %+v", rest[0].line)
	}
	if rest[1].line.Verdict != verdictUnknown || rest[1].line.Reason != syncReasonInterrupted {
		t.Errorf("an unattempted folder = %+v, want %s/%s", rest[1].line, verdictUnknown, syncReasonInterrupted)
	}
	if !strings.Contains(rest[1].line.Detail, "not attempted") {
		t.Errorf("the line does not say it was never attempted: %q", rest[1].line.Detail)
	}
}

// --- the local guards that live inside the loops ----------------------------

// A DRY-RUN THAT MISSES A PURELY LOCAL REFUSAL IS A LIAR. These three fire
// before the loop's first byte and live outside applyDecision, so a dry-run
// reported the folder `applied` at exit 0 and the apply exited 1 on it — the one
// class of disagreement the whole decision extraction exists to prevent.
func TestSyncApplyDryRunAgreesOnTheLoopsOwnLocalRefusals(t *testing.T) {
	for _, tc := range []struct {
		name   string
		files  map[string]string
		bound  map[string]string
		reason string
	}{
		{
			name:   "positional ref",
			files:  map[string]string{"orders.sql": "SELECT * FROM {{ ref('0') }}"},
			bound:  map[string]string{"orders.sql": "table-orders"},
			reason: syncReasonPositionalRef,
		},
		{
			name: "dependency cycle",
			files: map[string]string{
				"a.sql": "SELECT * FROM {{ ref('b') }}",
				"b.sql": "SELECT * FROM {{ ref('a') }}",
			},
			bound:  map[string]string{"a.sql": "table-a", "b.sql": "table-b"},
			reason: syncReasonCyclicDependency,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newFakePipelineInstance(t)
			signInPipeline(t, f)
			seedFeature(f, "private")
			tree, _ := applyPipelineTree(t, f, tc.files, tc.bound, nil)

			dry, dryErr := runCLI(t, tree, "sync", "apply", "--stack", "prod", "--dry-run", "--json")
			wet, wetErr := runCLI(t, tree, "sync", "apply", "--stack", "prod", "--json")
			if exitCodeOf(dryErr) != exitCodeOf(wetErr) {
				t.Fatalf("dry-run exit %d, apply exit %d — a preview that disagrees with the run is worse than none\n%s\n%s",
					exitCodeOf(dryErr), exitCodeOf(wetErr), dry, wet)
			}
			if got := applyFolderLine(t, applyReportOf(t, dry), "sales"); got.Reason != tc.reason {
				t.Errorf("dry-run = %+v, want %s", got, tc.reason)
			}
			if got := applyFolderLine(t, applyReportOf(t, wet), "sales"); got.Reason != tc.reason {
				t.Errorf("apply = %+v, want %s", got, tc.reason)
			}
		})
	}
}

// The same, for the automation loop's orphan refusal: a file bound to a live
// automation and gone from the folder. Apply never passes --prune, so it is
// simply never-green — and the dry-run has to say so.
func TestSyncApplyDryRunAgreesOnAnOrphanedAutomationFile(t *testing.T) {
	f := newFakeAutomationInstance(t)
	signInAutomation(t, f)
	f.AddAutomation(&api.Automation{ID: "job-1", Name: "nightly", CronExpr: "0 2 * * *", Enabled: true})

	tree := t.TempDir()
	root := filepath.Join(tree, "orders")
	if err := os.MkdirAll(root, 0o755); err != nil {
		t.Fatal(err)
	}
	// Bound to nightly.json, which is NOT on disk. The folder holds another file
	// so there is real work beside the orphan.
	manifest, lock := automationStack(f, "collection-1",
		map[string]string{"nightly.json": "job-1"},
		map[string]string{"nightly.json": "2026-08-01T00:00:00Z"})
	writeAutomationFolder(t, root, manifest, lock, map[string]string{
		"weekly.json": `{"enabled": true, "cronExpr": "0 4 * * 1"}`,
	})

	dry, dryErr := runCLI(t, tree, "sync", "apply", "--stack", "prod", "--dry-run", "--json")
	wet, wetErr := runCLI(t, tree, "sync", "apply", "--stack", "prod", "--json")
	if exitCodeOf(dryErr) != exitCodeOf(wetErr) {
		t.Fatalf("dry-run exit %d, apply exit %d\n%s\n%s", exitCodeOf(dryErr), exitCodeOf(wetErr), dry, wet)
	}
	if got := applyFolderLine(t, applyReportOf(t, dry), "orders"); got.Reason != syncReasonOrphanedFile {
		t.Errorf("dry-run = %+v, want %s", got, syncReasonOrphanedFile)
	}
	if got := applyFolderLine(t, applyReportOf(t, wet), "orders"); got.Reason != syncReasonOrphanedFile {
		t.Errorf("apply = %+v, want %s", got, syncReasonOrphanedFile)
	}
	// And nothing was written, which is the difference between a refusal decided
	// before the loop and one the loop reached.
	if len(f.updates) != 0 || len(f.created) != 0 {
		t.Errorf("a refused folder still wrote: updates=%+v created=%+v", f.updates, f.created)
	}
}

// The two create numbers really do differ, and the report must not collapse
// them: a run that made four rows and refused a fifth cannot honestly claim
// either four creation units or five created rows.
func TestSyncApplyAllowCreateCountsCreatedApartFromRefused(t *testing.T) {
	f := newFakeAutomationInstance(t)
	signInAutomation(t, f)
	f.AddAutomation(&api.Automation{ID: "job-1", Name: "nightly", CronExpr: "0 2 * * *", Enabled: false})

	tree := t.TempDir()
	// `held` is refused for its UNANCHORED unit and holds a create beside it;
	// `fresh` creates freely. --allow-create clears the second and not the first.
	held := filepath.Join(tree, "held")
	if err := os.MkdirAll(held, 0o755); err != nil {
		t.Fatal(err)
	}
	manifest, lock := automationStack(f, "collection-1",
		map[string]string{"nightly.json": "job-1"},
		map[string]string{"nightly.json": ""})
	writeAutomationFolder(t, held, manifest, lock, map[string]string{
		"nightly.json": `{"enabled": true, "cronExpr": "0 3 * * *"}`,
		"weekly.json":  `{"enabled": true, "cronExpr": "0 4 * * 1"}`,
	})
	fresh := filepath.Join(tree, "zfresh")
	if err := os.MkdirAll(fresh, 0o755); err != nil {
		t.Fatal(err)
	}
	freshManifest, freshLock := automationStack(f, "collection-1", nil, nil)
	writeAutomationFolder(t, fresh, freshManifest, freshLock, map[string]string{
		"hourly.json": `{"enabled": true, "cronExpr": "0 * * * *"}`,
	})

	var out string
	var err error
	stderr := captureStderr(t, func() {
		out, err = runCLI(t, tree, "sync", "apply", "--stack", "prod", "--allow-create", "--json")
	})
	// The refused folder keeps the run non-zero, and the other folder still runs.
	if code := exitCodeOf(err); code != syncExitDrifted {
		t.Fatalf("exit = %d (%v), want %d\n%s\n%s", code, err, syncExitDrifted, out, stderr)
	}
	// ONE row promised, not two: the create inside the held folder is never made.
	if !strings.Contains(stderr, "will create 1 row") {
		t.Errorf("the promise counted a create in a folder that was never going to run:\n%s", stderr)
	}
	if len(f.created) != 1 {
		t.Fatalf("created = %+v, want the one in the folder that ran", f.created)
	}
	summary := applyReportOf(t, out).Summary
	// Two creation units in the tree, one of them actually made.
	if summary.Creates != 2 || summary.Created != 1 {
		t.Errorf("summary = %+v, want 2 creation units and 1 created row", summary)
	}
}

// THE SILENT NON-DEPLOY. An up-to-date push has two causes and only one of them
// means the folder is live: the other is an earlier run whose push landed and
// whose PUBLISH then failed. There is nothing left to push on the next run, so
// apply used to report the folder `applied` and exit 0 while the change sat in
// an uncommitted draft — and stickily, since every later run took the same
// short-circuit.
//
// The fake models the SERVER, and that is what makes this worth running: the
// draft in run two is one the CLI really left behind rather than one the test
// wrote, so a guard that reads the CLI's intent instead of the row's state
// cannot pass it.
func TestSyncApplyPublishesADraftAnEarlierRunLeftBehind(t *testing.T) {
	f := newFakeInstance(t)
	f.AddWorkflow(&api.Workflow{ID: "wf-1", Lifecycle: api.LifecycleLive},
		api.WorkflowFile{Path: "main.py", Content: "OLD\n"})
	signIn(t, f)
	root := cloneFolder(t, f, "wf-1")
	tree := filepath.Dir(root)
	writeLocal(t, root, "main.py", "NEW\n")

	// Run one: the push lands in a draft and the commit is refused. 403 rather
	// than a 5xx deliberately — this leg is about a DEFINITE refusal, so the
	// folder is reported refused rather than unknown.
	f.failCommit = 403
	out, err := runCLI(t, tree, "sync", "apply", "--json")
	if code := exitCodeOf(err); code != syncExitDrifted {
		t.Fatalf("run one: exit = %d (%v), want %d\n%s", code, err, syncExitDrifted, out)
	}
	if len(f.committed) != 0 {
		t.Fatalf("run one committed despite the refusal: %v", f.committed)
	}

	// Run two: nothing to push, everything still to publish.
	f.failCommit = 0
	out, err = runCLI(t, tree, "sync", "apply", "--json")
	if code := exitCodeOf(err); code != syncExitClean {
		t.Fatalf("run two: exit = %d (%v), want %d\n%s", code, err, syncExitClean, out)
	}
	if len(f.committed) != 1 {
		t.Fatalf("run two committed %v — the folder was reported deployed while its change sat in an uncommitted draft\n%s",
			f.committed, out)
	}
	if got := f.FileContents("wf-1")["main.py"]; got != "NEW\n" {
		t.Fatalf("the live workflow holds %q — apply reported a deploy that never reached it\n%s", got, out)
	}
}

// THE OTHER HALF, and the reason "is a draft open" is not the signal: an
// up-to-date push CHECKS ONE OUT on the way past, so after run one there is
// always a draft — identical to live, holding nothing anybody needs published.
//
// Two runs, because they exercise different legs of the same rule: run one has
// no draft and the push makes one, run two finds that draft already open. Both
// must leave the version history alone.
func TestSyncApplyOfAnUnchangedRepositoryMintsNoVersion(t *testing.T) {
	f := newFakeInstance(t)
	f.AddWorkflow(&api.Workflow{ID: "wf-1", Lifecycle: api.LifecycleLive},
		api.WorkflowFile{Path: "main.py", Content: "M\n"})
	signIn(t, f)
	root := cloneFolder(t, f, "wf-1")
	tree := filepath.Dir(root)

	for run := 1; run <= 2; run++ {
		out, err := runCLI(t, tree, "sync", "apply", "--json")
		if code := exitCodeOf(err); code != syncExitClean {
			t.Fatalf("run %d: exit = %d (%v), want %d\n%s", run, code, err, syncExitClean, out)
		}
		if len(f.committed) != 0 {
			t.Fatalf("run %d: apply committed %v — an unchanged repository must not grow a version per run\n%s",
				run, f.committed, out)
		}
	}
}

// THE PARENTLESS-DRAFT LEG, which is the same bug with no live row to compare
// against. A data app created by an earlier run and never published IS a draft:
// there is nothing left to push to it, so an apply that read the push's verdict
// reported it deployed for ever while nobody could see it.
func TestSyncApplyPublishesAnUnpublishedDataAppWithNothingLeftToPush(t *testing.T) {
	f := newFakeAppInstance(t)
	f.AddApp(&api.DataApp{ID: "data_app-new-1", Lifecycle: api.LifecycleDraft},
		api.DataAppFile{Path: "App.tsx", Content: "const x = 1;\n"})
	f.featureScope["feat-1"] = "private"
	signInApp(t, f)

	tree := t.TempDir()
	root := filepath.Join(tree, "explorer")
	if err := os.MkdirAll(root, 0o755); err != nil {
		t.Fatal(err)
	}
	manifest := &wfdir.Manifest{
		Kind: wfdir.KindDataApp, Title: "Revenue explorer",
		Stacks: map[string]wfdir.Stack{
			"prod": {URL: f.URL(), TenantID: testTenantID, FeatureID: "feat-1"},
		},
	}
	// IDENTICAL to the row, which is what makes the push up to date — the state
	// an earlier run's push leaves behind once its publish has failed.
	writeAppFolder(t, root, manifest, map[string]string{"App.tsx": "const x = 1;\n"})
	lock := &wfdir.Lock{}
	lock.SetBinding("prod", wfdir.Binding{DataAppID: "data_app-new-1", FeatureID: "feat-1"})
	if err := wfdir.SaveLock(root, lock); err != nil {
		t.Fatal(err)
	}
	state := &wfdir.State{}
	state.Set(f.Key(), baselineFromApp(aliasCodec{}, f.apps["data_app-new-1"],
		[]api.DataAppFile{{Path: "App.tsx", Content: "const x = 1;\n"}}))
	if err := wfdir.SaveState(root, state); err != nil {
		t.Fatal(err)
	}

	out, err := runCLI(t, tree, "sync", "apply", "--stack", "prod", "--json")
	if code := exitCodeOf(err); code != syncExitClean {
		t.Fatalf("exit = %d (%v), want %d\n%s", code, err, syncExitClean, out)
	}
	if len(f.committed) != 1 {
		t.Fatalf("committed = %v — the app was reported deployed while it was still nobody's but its author's\n%s",
			f.committed, out)
	}
}

// THE PIPELINE HALF, which the lock already answers: a file with a recorded
// draft id is a file whose publish has not landed, and that is exactly what
// runPipelinePublish's own no-argument selection targets. An apply that stopped
// at the push's verdict left the draft staged and reported the folder deployed.
func TestSyncApplyPublishesAPipelineDraftAnEarlierRunLeftStaged(t *testing.T) {
	f := newFakePipelineInstance(t)
	signInPipeline(t, f)
	// SHARED, so the commit is refused for an ordinary credential — the way a
	// real publish fails after a push that landed perfectly.
	seedFeature(f, "organization")
	f.privilegeLevel = 50
	tree, _ := applyPipelineTree(t, f,
		map[string]string{"orders.sql": "SELECT * FROM raw WHERE shipped"},
		map[string]string{"orders.sql": "table-orders"},
		nil)

	out, err := runCLI(t, tree, "sync", "apply", "--stack", "prod", "--json")
	if code := exitCodeOf(err); code != syncExitDrifted {
		t.Fatalf("run one: exit = %d (%v), want %d\n%s", code, err, syncExitDrifted, out)
	}
	if len(f.committed) != 0 {
		t.Fatalf("run one committed despite the refusal: %v", f.committed)
	}

	// Run two: the SQL is already in the draft, so there is nothing to push —
	// and everything still to publish.
	f.AddFeature("collection-1", "Sales pipeline", "private")
	out, err = runCLI(t, tree, "sync", "apply", "--stack", "prod", "--json")
	if code := exitCodeOf(err); code != syncExitClean {
		t.Fatalf("run two: exit = %d (%v), want %d\n%s", code, err, syncExitClean, out)
	}
	if len(f.committed) != 1 {
		t.Fatalf("run two committed %v — the staged draft was reported deployed\n%s", f.committed, out)
	}
	if got := f.CodeOf("table-orders"); got != "SELECT * FROM raw WHERE shipped" {
		t.Fatalf("the live table holds %q — apply reported a deploy that never reached it\n%s", got, out)
	}
}

// THE REFUSALS A FOLDER-GRAIN DECISION CANNOT SEE. Each of these folders is
// BOUND, so its only creation-unit answer is `update`: the gate clears it and a
// dry-run built from decisions alone reports it deployable, then the apply exits
// 1 on it — possibly twenty folders into a half-deployed tree.
//
// The assertion is the same one every other pre-flight test makes: the preview
// and the run agree, on the exit code AND on the reason.
func TestSyncApplyDryRunAgreesOnTheWholeFolderRefusals(t *testing.T) {
	// A workflow whose .py was renamed and whose manifest was not.
	t.Run("workflow entrypoint", func(t *testing.T) {
		f := newFakeInstance(t)
		f.AddWorkflow(&api.Workflow{ID: "wf-1", Lifecycle: api.LifecycleLive},
			api.WorkflowFile{Path: "main.py", Content: "M\n"})
		signIn(t, f)
		root := cloneFolder(t, f, "wf-1")
		tree := filepath.Dir(root)
		if err := os.Rename(filepath.Join(root, "main.py"), filepath.Join(root, "app.py")); err != nil {
			t.Fatal(err)
		}
		// No --stack: `wf clone` writes the folder in the shape it writes, and a
		// name it does not declare is a different refusal entirely.
		assertApplyAgrees(t, tree, filepath.Base(root), syncReasonMissingEntrypoint)
	})

	// The same rename on a data app, where the entrypoint is what the bundle
	// compiles from.
	t.Run("data app entrypoint", func(t *testing.T) {
		f := newFakeAppInstance(t)
		f.AddApp(&api.DataApp{ID: "data_app-1", Lifecycle: api.LifecycleLive},
			api.DataAppFile{Path: "App.tsx", Content: "const x = 1;\n"})
		signInApp(t, f)
		tree := t.TempDir()
		root := filepath.Join(tree, "explorer")
		if err := os.MkdirAll(root, 0o755); err != nil {
			t.Fatal(err)
		}
		manifest := &wfdir.Manifest{
			Kind: wfdir.KindDataApp, Title: "Revenue explorer",
			Stacks: map[string]wfdir.Stack{
				"prod": {URL: f.URL(), TenantID: testTenantID, FeatureID: "feat-1"},
			},
		}
		writeAppFolder(t, root, manifest, map[string]string{"Main.tsx": "const x = 1;\n"})
		lock := &wfdir.Lock{}
		lock.SetBinding("prod", wfdir.Binding{DataAppID: "data_app-1", FeatureID: "feat-1"})
		if err := wfdir.SaveLock(root, lock); err != nil {
			t.Fatal(err)
		}
		assertApplyAgrees(t, tree, "explorer", syncReasonMissingEntrypoint, "--stack", "prod")
	})

	// An automation folder bound to a live row with no feature to compare it in.
	// ListAutomations("") filters every row away rather than failing, so the loop
	// reads it as "every automation is gone" — which is why the loop refuses
	// before its listing, and why the preview has to say the same.
	t.Run("bound automations with no feature", func(t *testing.T) {
		f := newFakeAutomationInstance(t)
		signInAutomation(t, f)
		f.AddAutomation(&api.Automation{ID: "job-1", Name: "nightly", CronExpr: "0 2 * * *", Enabled: true})
		tree := t.TempDir()
		root := filepath.Join(tree, "orders")
		if err := os.MkdirAll(root, 0o755); err != nil {
			t.Fatal(err)
		}
		manifest, lock := automationStack(f, "",
			map[string]string{"nightly.json": "job-1"},
			map[string]string{"nightly.json": "2026-08-01T00:00:00Z"})
		writeAutomationFolder(t, root, manifest, lock, map[string]string{
			"nightly.json": `{"enabled": true, "cronExpr": "0 3 * * *"}`,
		})
		assertApplyAgrees(t, tree, "orders", syncReasonNoFeature, "--stack", "prod")
	})
}

// assertApplyAgrees runs the dry-run and the apply over one tree and pins that
// they answer identically about one folder — which is the whole promise
// decideApplyPushPreflight exists to keep.
func assertApplyAgrees(t *testing.T, tree, folder, reason string, args ...string) {
	t.Helper()
	dry, dryErr := runCLI(t, tree, append([]string{"sync", "apply", "--dry-run", "--json"}, args...)...)
	wet, wetErr := runCLI(t, tree, append([]string{"sync", "apply", "--json"}, args...)...)
	if exitCodeOf(dryErr) != exitCodeOf(wetErr) {
		t.Fatalf("dry-run exit %d, apply exit %d — a preview that disagrees with the run is worse than none\n%s\n%s",
			exitCodeOf(dryErr), exitCodeOf(wetErr), dry, wet)
	}
	if got := applyFolderLine(t, applyReportOf(t, dry), folder); got.Reason != reason {
		t.Errorf("dry-run = %+v, want %s", got, reason)
	}
	if got := applyFolderLine(t, applyReportOf(t, wet), folder); got.Reason != reason {
		t.Errorf("apply = %+v, want %s", got, reason)
	}
}

// A WRITE THE INSTANCE NEVER ANSWERED IS NOT A REFUSAL. `refused`/not_deployed
// says the server looked at this and said no; a 5xx, a 429, a dead connection or
// our own deadline say nothing of the kind, and a timed-out commit may perfectly
// well have landed. Reported as a refusal, a CI log reads "not deployed" beside
// a folder that may be deployed — which is exactly what exit 2 exists for.
//
// The contrast case is pinned next door: TestSyncApplyPublishesADraftAnEarlier
// RunLeftBehind fails the same commit with a 403 and expects exit 1.
func TestSyncApplyDoesNotCallAnUnansweredWriteNotDeployed(t *testing.T) {
	f := newFakeInstance(t)
	f.AddWorkflow(&api.Workflow{ID: "wf-1", Lifecycle: api.LifecycleLive},
		api.WorkflowFile{Path: "main.py", Content: "OLD\n"})
	signIn(t, f)
	root := cloneFolder(t, f, "wf-1")
	tree := filepath.Dir(root)
	writeLocal(t, root, "main.py", "NEW\n")
	f.failCommit = 500

	out, err := runCLI(t, tree, "sync", "apply", "--json")
	if code := exitCodeOf(err); code != syncExitUnknown {
		t.Fatalf("exit = %d (%v), want %d\n%s", code, err, syncExitUnknown, out)
	}
	folder := applyFolderLine(t, applyReportOf(t, out), filepath.Base(root))
	if folder.Verdict != verdictUnknown || folder.Reason != syncReasonUnanswered {
		t.Fatalf("folder = %+v, want %s/%s", folder, verdictUnknown, syncReasonUnanswered)
	}
}

// THE DESTINATION IS PART OF THE PROMISE. Every create resolves its feature from
// a committed, hand-editable ronja.json, and all four decisions gate on nothing
// but "featureID is not empty" — so a merge that edits one folder's featureID
// redirects its creates into any feature the runner's credential can reach, and
// a line naming only the count and the organization reads identically either
// way. Grouped by feature rather than one line per row, because the count is the
// volume failure mode and thirty lines is how you hide it.
func TestSyncApplyAllowCreateNamesTheFeaturesItWouldCreateIn(t *testing.T) {
	f := newFakePipelineInstance(t)
	signInPipeline(t, f)
	seedFeature(f, "private")
	f.AddFeature("collection-2", "Somebody else's", "private")
	tree := t.TempDir()
	for name, feature := range map[string]string{"a": "collection-1", "b": "collection-2"} {
		root := filepath.Join(tree, name)
		if err := os.MkdirAll(root, 0o755); err != nil {
			t.Fatal(err)
		}
		manifest := &wfdir.Manifest{
			Kind: wfdir.KindPipeline, Title: name,
			Stacks: map[string]wfdir.Stack{
				"prod": {URL: f.URL(), TenantID: testTenantID, FeatureID: feature},
			},
		}
		if err := wfdir.SaveFolder(root, manifest, &wfdir.Lock{}); err != nil {
			t.Fatal(err)
		}
		if err := wfdir.WriteFile(root, "one.sql", "SELECT 1"); err != nil {
			t.Fatal(err)
		}
	}

	var out string
	var err error
	stderr := captureStderr(t, func() {
		out, err = runCLI(t, tree, "sync", "apply", "--stack", "prod", "--allow-create", "--dry-run", "--json")
	})
	if code := exitCodeOf(err); code != syncExitClean {
		t.Fatalf("exit = %d (%v), want %d\n%s\n%s", code, err, syncExitClean, out, stderr)
	}
	// ONE grouped line naming both destinations and how many rows each takes.
	if !strings.Contains(stderr, "Into features collection-1 (1 row), collection-2 (1 row).") {
		t.Fatalf("the announcement did not name where the rows would land:\n%s", stderr)
	}
}
