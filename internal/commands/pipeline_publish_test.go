package commands

import (
	"strings"
	"testing"
	"time"

	"github.com/ronjatech/ronja-cli/internal/api"
	"github.com/ronjatech/ronja-cli/internal/wfdir"
)

// `ronja pipeline publish`.
//
// The command is mostly a DECISION — commit, or ask an admin to — taken up front
// from the two server facts that decide it, plus two warnings about staleness
// that exist because a table commit is last-writer-wins with no compare-and-swap
// anywhere behind it.

// seedStagedFolder is seedBoundFolder plus an open, built draft of orders.sql,
// recorded in the baseline the way a successful push would have left it.
func seedStagedFolder(t *testing.T, f *fakePipelineInstance, scope string) string {
	t.Helper()
	root := seedBoundFolder(t, f)
	f.features["collection-1"].Scope = scope
	f.AddDraft("table-orders", "table-draft-5", "SELECT staged FROM raw")
	writePipelineBaseline(t, root, f.Key(), "collection-1",
		map[string]string{
			"orders.sql":  "SELECT * FROM raw",
			"revenue.sql": "SELECT * FROM {{ ref('table-orders') }}",
		},
		map[string]wfdir.TableState{
			"orders.sql":  {TableID: "table-orders", DraftID: "table-draft-5"},
			"revenue.sql": {TableID: "table-revenue"},
		})
	return root
}

// TestPipelinePublishCommitsInAPrivateFeature: the ordinary path, plus the two
// things that have to happen AFTER the server-side change — the draft pointer
// goes, and the baseline is re-pointed at the live table it was just committed
// onto.
func TestPipelinePublishCommitsInAPrivateFeature(t *testing.T) {
	f := newFakePipelineInstance(t)
	signInPipeline(t, f)
	root := seedStagedFolder(t, f, "private")

	out, stderr, err := runPipelineCLI(t, root, "pipeline", "publish", "--json")
	if err != nil {
		t.Fatalf("publish: %v\n%s", err, stderr)
	}
	files := decodeJSON(t, out)["files"].([]any)
	if len(files) != 1 {
		t.Fatalf("files = %+v", files)
	}
	file := files[0].(map[string]any)
	if file["outcome"] != outcomePublished {
		t.Errorf("outcome = %v", file["outcome"])
	}
	if len(f.committed) != 1 || f.committed[0] != "table-draft-5" {
		t.Errorf("committed = %+v", f.committed)
	}
	if len(f.reviewRequested) != 0 {
		t.Errorf("a private feature must not file a review request: %v", f.reviewRequested)
	}
	// The draft's SQL is now the live table's.
	if f.CodeOf("table-orders") != "SELECT staged FROM raw" {
		t.Errorf("the commit did not land: %q", f.CodeOf("table-orders"))
	}

	inst := pipelineStateOf(t, root, f.Key())
	if inst.Tables["orders.sql"].DraftID != "" {
		t.Errorf("the draft pointer survived a commit: %+v", inst.Tables["orders.sql"])
	}
	// The baseline now describes the LIVE table, so the next status compares
	// against the row that actually exists.
	if inst.Files["orders.sql"].SHA256 != wfdir.HashString("SELECT staged FROM raw") {
		t.Error("the baseline was not refreshed from the live table after the commit")
	}
}

// TestPipelinePublishRefusesToCallARejectedDraftPublished: the other half of the
// commit-timeout reconcile. A draft stops existing on a discard and on a
// reviewer's rejection just as surely as on a commit, so "your draft is gone"
// proves nothing on its own — and an author told they published is an author who
// stops looking.
func TestPipelinePublishRefusesToCallARejectedDraftPublished(t *testing.T) {
	f := newFakePipelineInstance(t)
	signInPipeline(t, f)
	root := seedStagedFolder(t, f, "private")
	f.rejectCommit["table-draft-5"] = true
	landOnceThenTimeOut(t, "POST", "/api/v2/feature/model/table-draft-5/commit")

	out, _, err := runPipelineCLI(t, root, "pipeline", "publish", "--json")
	if err == nil {
		t.Fatal("a draft that vanished without landing must not be reported as published")
	}
	file := decodeJSON(t, out)["files"].([]any)[0].(map[string]any)
	if file["outcome"] != pushOutcomeRefused {
		t.Fatalf("file = %+v", file)
	}
	if !strings.Contains(file["error"].(string), "does not hold the SQL that draft held") {
		t.Errorf("the refusal does not say what was checked: %v", file["error"])
	}
	// And the baseline is not advanced onto a publish that never happened.
	inst := pipelineStateOf(t, root, f.Key())
	if inst.Files["orders.sql"].SHA256 == wfdir.HashString("SELECT staged FROM raw") {
		t.Error("the baseline was refreshed as if the commit had landed")
	}
}

// TestPipelinePublishReconcilesACommitThatTimedOutAndLanded: a commit whose
// request died on a deadline may well have landed — and if it did, the cascade
// fired too.
//
// Reported as a failure it is a lie with a tail: the draft pointer is never
// cleared, so the next publish reports "committed or discarded elsewhere" about
// the author's own successful publish, and the baseline never advances. Our own
// draft being GONE is the test, because that is what a commit does and nothing
// else this call could have done.
func TestPipelinePublishReconcilesACommitThatTimedOutAndLanded(t *testing.T) {
	f := newFakePipelineInstance(t)
	signInPipeline(t, f)
	root := seedStagedFolder(t, f, "private")
	landOnceThenTimeOut(t, "POST", "/api/v2/feature/model/table-draft-5/commit")

	out, stderr, err := runPipelineCLI(t, root, "pipeline", "publish", "--json")
	if err != nil {
		t.Fatalf("publish: %v\n%s", err, stderr)
	}
	file := decodeJSON(t, out)["files"].([]any)[0].(map[string]any)
	if file["outcome"] != outcomePublished {
		t.Fatalf("file = %+v\n%s", file, stderr)
	}
	if len(f.committed) != 1 || f.committed[0] != "table-draft-5" {
		t.Errorf("committed = %+v", f.committed)
	}
	if len(f.reviewRequested) != 0 {
		t.Errorf("a timeout must not be mistaken for the needs-review refusal: %v", f.reviewRequested)
	}
	inst := pipelineStateOf(t, root, f.Key())
	if inst.Tables["orders.sql"].DraftID != "" {
		t.Errorf("the draft pointer survived a commit that landed: %+v", inst.Tables["orders.sql"])
	}
	if inst.Files["orders.sql"].SHA256 != wfdir.HashString("SELECT staged FROM raw") {
		t.Error("the baseline was not refreshed from the live table after the commit")
	}
}

// TestPipelinePublishReportsACommitThatTimedOutAndDidNot: the other half. The
// draft is still open, so the commit did not land and saying so is the whole
// answer — no review request, no baseline move.
func TestPipelinePublishReportsACommitThatTimedOutAndDidNot(t *testing.T) {
	f := newFakePipelineInstance(t)
	signInPipeline(t, f)
	root := seedStagedFolder(t, f, "private")
	failOnceWithATimeout(t, "POST", "/api/v2/feature/model/table-draft-5/commit")

	out, _, err := runPipelineCLI(t, root, "pipeline", "publish", "--json")
	if err == nil {
		t.Fatal("a commit that did not land must exit non-zero")
	}
	file := decodeJSON(t, out)["files"].([]any)[0].(map[string]any)
	if file["outcome"] != pushOutcomeRefused {
		t.Fatalf("file = %+v", file)
	}
	if !strings.Contains(file["error"].(string), "still open") {
		t.Errorf("the refusal does not say the draft is still there: %v", file["error"])
	}
	if len(f.committed) != 0 {
		t.Errorf("committed = %+v", f.committed)
	}
	inst := pipelineStateOf(t, root, f.Key())
	if inst.Tables["orders.sql"].DraftID != "table-draft-5" {
		t.Errorf("a draft that is still on the server was dropped from the baseline: %+v", inst.Tables["orders.sql"])
	}
}

// TestPipelinePublishAsksForReviewInASharedFeature: the two-outcome honesty. A
// non-admin publishing into a shared feature cannot commit, and reporting
// "published" when what happened was "an admin now has a review request" is the
// kind of lie people discover a week later.
//
// The route is decided UP FRONT from the feature's scope and the caller's role,
// never by trying a commit and reading the rejection prose.
func TestPipelinePublishAsksForReviewInASharedFeature(t *testing.T) {
	f := newFakePipelineInstance(t)
	signInPipeline(t, f)
	f.privilegeLevel = 50 // an ordinary user
	root := seedStagedFolder(t, f, scopeOrganization)

	out, stderr, err := runPipelineCLI(t, root, "pipeline", "publish", "--json")
	if err != nil {
		t.Fatalf("publish: %v\n%s", err, stderr)
	}
	file := decodeJSON(t, out)["files"].([]any)[0].(map[string]any)
	if file["outcome"] != outcomeSubmittedForReview {
		t.Errorf("outcome = %v", file["outcome"])
	}
	if len(f.reviewRequested) != 1 || f.reviewRequested[0] != "table-draft-5" {
		t.Errorf("reviewRequested = %+v", f.reviewRequested)
	}
	// Never attempted, because the decision was taken from the facts rather than
	// from a refusal.
	if len(f.committed) != 0 {
		t.Errorf("a shared feature was committed to by a non-admin: %v", f.committed)
	}
	// And the draft is still there, so nothing local pretends it landed.
	inst := pipelineStateOf(t, root, f.Key())
	if inst.Tables["orders.sql"].DraftID != "table-draft-5" {
		t.Errorf("a review request must not clear the draft: %+v", inst.Tables["orders.sql"])
	}
}

// TestPipelinePublishCommitsAsAnAdminInASharedFeature: the other half of the
// same decision — an admin may commit into a shared feature, so the review
// branch must not fire on scope alone.
func TestPipelinePublishCommitsAsAnAdminInASharedFeature(t *testing.T) {
	f := newFakePipelineInstance(t)
	signInPipeline(t, f)
	f.privilegeLevel = 10 // USR_ADMIN; privilege counts DOWN
	root := seedStagedFolder(t, f, scopeOrganization)

	if _, stderr, err := runPipelineCLI(t, root, "pipeline", "publish"); err != nil {
		t.Fatalf("publish: %v\n%s", err, stderr)
	}
	if len(f.committed) != 1 {
		t.Errorf("committed = %+v", f.committed)
	}
	if len(f.reviewRequested) != 0 {
		t.Errorf("an admin filed a review request: %v", f.reviewRequested)
	}
}

// TestPipelinePublishFallsBackToReviewOnA400: the race the up-front routing
// cannot close — the caller's role changed between the read and the commit, so
// an admin's commit into a shared feature comes back needing review.
func TestPipelinePublishFallsBackToReviewOnA400(t *testing.T) {
	f := newFakePipelineInstance(t)
	signInPipeline(t, f)
	f.privilegeLevel = 10 // reads as committable up front
	root := seedStagedFolder(t, f, scopeOrganization)
	f.failCommit["table-draft-5"] = 400

	out, stderr, err := runPipelineCLI(t, root, "pipeline", "publish", "--json")
	if err != nil {
		t.Fatalf("publish: %v\n%s", err, stderr)
	}
	file := decodeJSON(t, out)["files"].([]any)[0].(map[string]any)
	if file["outcome"] != outcomeSubmittedForReview {
		t.Errorf("outcome = %v", file["outcome"])
	}
	if len(f.reviewRequested) != 1 {
		t.Errorf("reviewRequested = %+v", f.reviewRequested)
	}
}

// TestPipelinePublishDoesNotAskForReviewOnAPrivateFeature: a 400 on a private
// feature is some OTHER refusal, which a review request would neither fix nor
// describe — and filing one would report a publish that never happened as a
// successful hand-off.
func TestPipelinePublishDoesNotAskForReviewOnAPrivateFeature(t *testing.T) {
	f := newFakePipelineInstance(t)
	signInPipeline(t, f)
	root := seedStagedFolder(t, f, "private")
	f.failCommit["table-draft-5"] = 400

	_, _, err := runPipelineCLI(t, root, "pipeline", "publish")
	if err == nil {
		t.Fatal("a 400 on a private feature must be an error")
	}
	if len(f.reviewRequested) != 0 {
		t.Errorf("a private feature filed a review request: %v", f.reviewRequested)
	}
}

// TestPipelinePublishNoRequestReviewFails: what CI wants when a merge is
// expected to land directly — an error rather than a review request nobody is
// watching for.
func TestPipelinePublishNoRequestReviewFails(t *testing.T) {
	f := newFakePipelineInstance(t)
	signInPipeline(t, f)
	f.privilegeLevel = 50
	root := seedStagedFolder(t, f, scopeOrganization)
	f.failCommit["table-draft-5"] = 400

	_, _, err := runPipelineCLI(t, root, "pipeline", "publish", "--no-request-review")
	if err == nil {
		t.Fatal("--no-request-review must fail rather than file a review request")
	}
	if len(f.reviewRequested) != 0 {
		t.Errorf("--no-request-review still filed %v", f.reviewRequested)
	}
}

// TestPipelinePublishRefusesAFailedDraft: publishing a draft that did not build
// would put a table live with nothing behind it. The server would too, but the
// CLI names the fix — and the refusal has to happen before the request, or the
// 400 it comes back with lands in the needs-review fallback and reports a
// publish that never happened as a review request.
func TestPipelinePublishRefusesAFailedDraft(t *testing.T) {
	f := newFakePipelineInstance(t)
	signInPipeline(t, f)
	root := seedStagedFolder(t, f, "private")
	f.tables["table-draft-5"].Status = api.TableStatusBuildFailed
	f.buildErr["table-draft-5"] = &api.TableBuildError{
		Message: "The transformation is impossible: raw has no column `staged`.", Type: "impossible"}

	out, stderr, err := runPipelineCLI(t, root, "pipeline", "publish", "--json")
	if err == nil {
		t.Fatal("a failed draft must refuse the publish")
	}
	file := decodeJSON(t, out)["files"].([]any)[0].(map[string]any)
	if file["outcome"] != pushOutcomeRefused {
		t.Errorf("outcome = %v", file["outcome"])
	}
	if !strings.Contains(file["error"].(string), "did not build") {
		t.Errorf("error = %v", file["error"])
	}
	// The fix is named, and nothing was sent.
	if !strings.Contains(stderr, "pipeline push") {
		t.Errorf("the refusal does not name the fix:\n%s", stderr)
	}
	if len(f.committed) != 0 || len(f.reviewRequested) != 0 {
		t.Errorf("a failed draft was still submitted: committed=%v reviewed=%v",
			f.committed, f.reviewRequested)
	}
}

// TestPipelinePublishRefusesANeverBuiltDraft: a checked-out draft sits at
// `pending` until something syncs it, so the commonest way to reach that verdict
// is a draft nobody has ever built — opened in the web builder, by chat, or by a
// push that died before its write. "Still building — wait for it to finish" is
// then advice to wait for something nobody started.
func TestPipelinePublishRefusesANeverBuiltDraft(t *testing.T) {
	f := newFakePipelineInstance(t)
	signInPipeline(t, f)
	root := seedBoundFolder(t, f)
	// A real checkout, from a push whose sync failed: the draft exists and holds
	// the SQL, and nothing has ever built it.
	editFile(t, root, "orders.sql", "SELECT 12 FROM raw")
	f.failSync["table-draft-1"] = 500
	if _, _, err := runPipelineCLI(t, root, "pipeline", "push"); err == nil {
		t.Fatal("a failed sync must exit non-zero")
	}
	if f.tables["table-draft-1"] == nil {
		t.Fatal("the checkout did not happen; this test is about the draft it leaves")
	}

	out, stderr, err := runPipelineCLI(t, root, "pipeline", "publish", "--json")
	if err == nil {
		t.Fatal("an unbuilt draft must refuse the publish")
	}
	file := decodeJSON(t, out)["files"].([]any)[0].(map[string]any)
	if file["outcome"] != pushOutcomeRefused {
		t.Errorf("outcome = %v", file["outcome"])
	}
	message, _ := file["error"].(string)
	if !strings.Contains(message, "has not been built") {
		t.Errorf("error = %q, want it to say the draft was never built", message)
	}
	if strings.Contains(message, "still building") {
		t.Errorf("an unbuilt draft was reported as one in flight: %q", message)
	}
	if !strings.Contains(stderr, "pipeline push") {
		t.Errorf("the refusal does not name the fix:\n%s", stderr)
	}
	if len(f.committed) != 0 {
		t.Errorf("an unbuilt draft was committed: %v", f.committed)
	}
}

// TestPipelinePublishKeepsTheFallbackWhenTheScopeIsUnreadable: an unreadable
// scope is UNKNOWN, not private. Read as private it would also disarm the 400
// fallback, so a shared-feature publish whose scope read happened to fail would
// die on the commit instead of filing the review request it needed.
func TestPipelinePublishKeepsTheFallbackWhenTheScopeIsUnreadable(t *testing.T) {
	f := newFakePipelineInstance(t)
	signInPipeline(t, f)
	root := seedStagedFolder(t, f, scopeOrganization)
	f.failFeature = 500
	f.failCommit["table-draft-5"] = 400

	out, stderr, err := runPipelineCLI(t, root, "pipeline", "publish", "--json")
	if err != nil {
		t.Fatalf("publish: %v\n%s", err, stderr)
	}
	file := decodeJSON(t, out)["files"].([]any)[0].(map[string]any)
	if file["outcome"] != outcomeSubmittedForReview {
		t.Errorf("outcome = %v, want the fallback to still fire", file["outcome"])
	}
	if len(f.reviewRequested) != 1 {
		t.Errorf("reviewRequested = %+v", f.reviewRequested)
	}
}

// TestPipelinePublishDoesNotResubmitADraftAlreadyInReview: the review payload
// already says whether request-review has been called. Filing a second one is
// harmless server-side and reads as progress that did not happen — the admin has
// had it since the first time.
func TestPipelinePublishDoesNotResubmitADraftAlreadyInReview(t *testing.T) {
	f := newFakePipelineInstance(t)
	signInPipeline(t, f)
	f.privilegeLevel = 50 // an ordinary user: the review route
	root := seedStagedFolder(t, f, scopeOrganization)

	if _, stderr, err := runPipelineCLI(t, root, "pipeline", "publish"); err != nil {
		t.Fatalf("first publish: %v\n%s", err, stderr)
	}
	out, stderr, err := runPipelineCLI(t, root, "pipeline", "publish", "--json")
	if err != nil {
		t.Fatalf("second publish: %v\n%s", err, stderr)
	}
	file := decodeJSON(t, out)["files"].([]any)[0].(map[string]any)
	if file["outcome"] != outcomeSubmittedForReview {
		t.Errorf("outcome = %v", file["outcome"])
	}
	if detail, _ := file["detail"].(string); !strings.Contains(detail, "already submitted") {
		t.Errorf("detail = %q, want it to say the request was already filed", detail)
	}
	if len(f.reviewRequested) != 1 {
		t.Errorf("the draft was submitted twice: %+v", f.reviewRequested)
	}
}

// TestPipelinePublishWarnsAboutAStaleBase: commit is LAST-WRITER-WINS and there
// is no compare-and-swap on this route, so the review payload's baseStale is the
// only warning that exists anywhere.
func TestPipelinePublishWarnsAboutAStaleBase(t *testing.T) {
	f := newFakePipelineInstance(t)
	signInPipeline(t, f)
	root := seedStagedFolder(t, f, "private")
	f.intervening["table-orders"] = []api.TableInterveningVersion{
		{VersionID: "table-version-2", Name: "Orders", CommittedAt: time.Now()},
	}

	out, stderr, err := runPipelineCLI(t, root, "pipeline", "publish", "--json")
	if err != nil {
		t.Fatalf("publish: %v\n%s", err, stderr)
	}
	if !strings.Contains(stderr, "table-version-2") {
		t.Errorf("the intervening version was not named:\n%s", stderr)
	}
	file := decodeJSON(t, out)["files"].([]any)[0].(map[string]any)
	warnings, _ := file["warnings"].([]any)
	if len(warnings) == 0 || !strings.Contains(warnings[0].(string), "compare-and-swap") {
		t.Errorf("warnings = %+v", warnings)
	}
	// A warning, not a refusal: the commit still happens.
	if len(f.committed) != 1 {
		t.Errorf("committed = %+v", f.committed)
	}
}

// TestPipelinePublishWarnsAboutStaleInputs: GetDependents excludes shadow rows,
// so a staged draft is NEVER invalidated by an upstream publish. The confidence
// report silently decays between push and publish, and nothing else would say
// so.
func TestPipelinePublishWarnsAboutStaleInputs(t *testing.T) {
	f := newFakePipelineInstance(t)
	signInPipeline(t, f)
	root := seedBoundFolder(t, f)
	f.AddDraft("table-revenue", "table-draft-6", "SELECT * FROM {{ ref('table-orders') }}")
	// The upstream materialized AFTER the draft was built.
	f.tables["table-orders"].UpdatedAt = f.tables["table-draft-6"].UpdatedAt.Add(time.Hour)
	writePipelineBaseline(t, root, f.Key(), "collection-1",
		map[string]string{
			"orders.sql":  "SELECT * FROM raw",
			"revenue.sql": "SELECT * FROM {{ ref('table-orders') }}",
		},
		map[string]wfdir.TableState{
			"orders.sql":  {TableID: "table-orders"},
			"revenue.sql": {TableID: "table-revenue", DraftID: "table-draft-6"},
		})

	out, stderr, err := runPipelineCLI(t, root, "pipeline", "publish", "--json")
	if err != nil {
		t.Fatalf("publish: %v\n%s", err, stderr)
	}
	if !strings.Contains(stderr, "table-orders") || !strings.Contains(stderr, "older upstream data") {
		t.Errorf("no input-staleness warning:\n%s", stderr)
	}
	file := decodeJSON(t, out)["files"].([]any)[0].(map[string]any)
	warnings, _ := file["warnings"].([]any)
	if len(warnings) == 0 {
		t.Errorf("the warning is not machine-readable:\n%s", out)
	}
	if len(f.committed) != 1 {
		t.Errorf("a staleness warning must not refuse the publish: %+v", f.committed)
	}
}

// TestPipelinePublishCountsTheFolderLocalCascade: committing cascades
// server-side, which is why this loop needs no `run` verb. The number reported
// is the FOLDER's own, counted from its ref graph — never a tenant-wide N, which
// the CLI cannot know (GetDependents is not exposed over HTTP) and which has
// conditions a confident count would paper over.
func TestPipelinePublishCountsTheFolderLocalCascade(t *testing.T) {
	f := newFakePipelineInstance(t)
	signInPipeline(t, f)
	root := seedStagedFolder(t, f, "private")
	// revenue.sql already reads table-orders; add a second reader.
	editFile(t, root, "margins.sql", "SELECT * FROM {{ ref('table-orders') }}")

	out, _, err := runPipelineCLI(t, root, "pipeline", "publish", "--json")
	if err != nil {
		t.Fatalf("publish: %v", err)
	}
	file := decodeJSON(t, out)["files"].([]any)[0].(map[string]any)
	if file["cascade"] != float64(2) {
		t.Errorf("cascade = %v, want the 2 files in this folder that read table-orders", file["cascade"])
	}
}

// TestPipelinePublishCommitsInDependencyOrder: a commit cascades, so publishing
// a downstream before its upstream lands a table computed from data that is
// about to be replaced and rebuilt again seconds later. The filenames are chosen
// so alphabetical order is the WRONG order, otherwise a publish that never
// sorted at all would pass.
func TestPipelinePublishCommitsInDependencyOrder(t *testing.T) {
	f := newFakePipelineInstance(t)
	signInPipeline(t, f)
	f.AddFeature("collection-1", "Sales", "private")
	f.AddTable(&api.Table{ID: "table-orders", Name: "Orders", FeatureID: "collection-1", Code: "SELECT 0"})
	f.AddTable(&api.Table{ID: "table-revenue", Name: "Revenue", FeatureID: "collection-1", Code: "SELECT 0"})
	f.AddDraft("table-orders", "table-draft-orders", "SELECT 1 FROM raw")
	f.AddDraft("table-revenue", "table-draft-revenue", "SELECT 1 FROM {{ ref('table-orders') }}")

	root := t.TempDir()
	manifest := &wfdir.Manifest{Kind: wfdir.KindPipeline, Title: "Sales"}
	manifest.SetBinding(f.Key(), wfdir.Binding{
		FeatureID: "collection-1",
		Tables:    map[string]string{"a_revenue.sql": "table-revenue", "b_orders.sql": "table-orders"},
	})
	files := map[string]string{
		"a_revenue.sql": "SELECT 1 FROM {{ ref('table-orders') }}",
		"b_orders.sql":  "SELECT 1 FROM raw",
	}
	writePipelineFolder(t, root, manifest, files)
	writePipelineBaseline(t, root, f.Key(), "collection-1", files, map[string]wfdir.TableState{
		"a_revenue.sql": {TableID: "table-revenue", DraftID: "table-draft-revenue"},
		"b_orders.sql":  {TableID: "table-orders", DraftID: "table-draft-orders"},
	})

	if _, stderr, err := runPipelineCLI(t, root, "pipeline", "publish"); err != nil {
		t.Fatalf("publish: %v\n%s", err, stderr)
	}
	if len(f.committed) != 2 {
		t.Fatalf("committed = %+v", f.committed)
	}
	if f.committed[0] != "table-draft-orders" {
		t.Errorf("committed %v; the upstream b_orders.sql must land first", f.committed)
	}
}

// TestPipelinePublishReportsNothingStaged: a folder with no staged draft is a
// different answer from one whose drafts were all refused.
func TestPipelinePublishReportsNothingStaged(t *testing.T) {
	f := newFakePipelineInstance(t)
	signInPipeline(t, f)
	root := seedBoundFolder(t, f)

	out, _, err := runPipelineCLI(t, root, "pipeline", "publish", "--json")
	if err != nil {
		t.Fatalf("publish: %v", err)
	}
	if decodeJSON(t, out)["nothing"] != true {
		t.Errorf("payload = %s", out)
	}
	if len(f.committed) != 0 {
		t.Errorf("committed = %+v", f.committed)
	}
}

// TestPipelinePublishRecoversFromAStaleDraftPointer: the recorded draft id is a
// HINT, never authority — a draft can be committed or discarded from the web UI
// between two commands.
func TestPipelinePublishRecoversFromAStaleDraftPointer(t *testing.T) {
	f := newFakePipelineInstance(t)
	signInPipeline(t, f)
	root := seedStagedFolder(t, f, "private")
	// Somebody landed it from the web UI while we were away.
	f.dropDraft("table-draft-5")

	out, _, err := runPipelineCLI(t, root, "pipeline", "publish", "--json")
	if err == nil {
		t.Fatal("a draft that is gone must be reported, not committed blind")
	}
	file := decodeJSON(t, out)["files"].([]any)[0].(map[string]any)
	if file["outcome"] != pushOutcomeRefused {
		t.Errorf("outcome = %v", file["outcome"])
	}
	if !strings.Contains(file["error"].(string), "no open draft") {
		t.Errorf("error = %v", file["error"])
	}
	// And the stale pointer is cleared, so the next command does not repeat it.
	inst := pipelineStateOf(t, root, f.Key())
	if inst.Tables["orders.sql"].DraftID != "" {
		t.Errorf("the stale pointer survived: %+v", inst.Tables["orders.sql"])
	}
}

// TestPipelinePublishWarnsWhenTheFolderIsDirty: publishing the draft as it
// stands is reasonable, but it is also the one way to publish something other
// than what you are looking at.
func TestPipelinePublishWarnsWhenTheFolderIsDirty(t *testing.T) {
	f := newFakePipelineInstance(t)
	signInPipeline(t, f)
	root := seedStagedFolder(t, f, "private")
	editFile(t, root, "orders.sql", "SELECT something newer FROM raw")

	_, stderr, err := runPipelineCLI(t, root, "pipeline", "publish")
	if err != nil {
		t.Fatalf("publish: %v\n%s", err, stderr)
	}
	if !strings.Contains(stderr, "local file(s) differ") {
		t.Errorf("a dirty folder was not reported:\n%s", stderr)
	}
	// A warning, not a refusal.
	if len(f.committed) != 1 {
		t.Errorf("committed = %+v", f.committed)
	}
}

// TestPipelinePublishIgnoresNonSQLDirt: warnIfDirty walks the folder with the
// folder's OWN kind, so a README is not a local change being left behind.
// Warning about one before every publish would train the reader to ignore the
// warning that matters.
func TestPipelinePublishIgnoresNonSQLDirt(t *testing.T) {
	f := newFakePipelineInstance(t)
	signInPipeline(t, f)
	root := seedStagedFolder(t, f, "private")
	editFile(t, root, "README.md", "# rewritten\n")

	_, stderr, err := runPipelineCLI(t, root, "pipeline", "publish")
	if err != nil {
		t.Fatalf("publish: %v\n%s", err, stderr)
	}
	if strings.Contains(stderr, "local file(s) differ") {
		t.Errorf("a non-SQL file was counted as local dirt:\n%s", stderr)
	}
}
