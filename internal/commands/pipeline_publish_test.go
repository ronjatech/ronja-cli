package commands

import (
	"strings"
	"testing"
	"time"

	"github.com/ronjatech/ronja-cli/internal/api"
	"github.com/ronjatech/ronja-cli/internal/wfdir"
)

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
	if f.CodeOf("table-orders") != "SELECT staged FROM raw" {
		t.Errorf("the commit did not land: %q", f.CodeOf("table-orders"))
	}

	inst := pipelineStateOf(t, root, f.Key())
	if inst.Tables["orders.sql"].DraftID != "" {
		t.Errorf("the draft pointer survived a commit: %+v", inst.Tables["orders.sql"])
	}
	if inst.Files["orders.sql"].SHA256 != wfdir.HashString("SELECT staged FROM raw") {
		t.Error("the baseline was not refreshed from the live table after the commit")
	}
}

// TestPipelinePublishRefusesToCallARejectedDraftPublished: after a commit timeout
// a vanished draft proves nothing alone, since a discard or a rejection also
// removes it; the live SQL must match what the draft held.
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
	inst := pipelineStateOf(t, root, f.Key())
	if inst.Files["orders.sql"].SHA256 == wfdir.HashString("SELECT staged FROM raw") {
		t.Error("the baseline was refreshed as if the commit had landed")
	}
}

// TestPipelinePublishReconcilesACommitThatTimedOutAndLanded: a commit that timed
// out may have landed; reported as a failure, the draft pointer and baseline
// would never advance past the author's own publish.
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

// TestPipelinePublishAsksForReviewInASharedFeature: the commit-or-review route is
// decided up front from the feature's scope and the caller's role, never by
// trying a commit and reading the rejection prose.
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
	if len(f.committed) != 0 {
		t.Errorf("a shared feature was committed to by a non-admin: %v", f.committed)
	}
	inst := pipelineStateOf(t, root, f.Key())
	if inst.Tables["orders.sql"].DraftID != "table-draft-5" {
		t.Errorf("a review request must not clear the draft: %+v", inst.Tables["orders.sql"])
	}
}

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

// TestPipelinePublishFallsBackToReviewOnA400: the race up-front routing cannot
// close, a role change between the read and the commit.
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
// feature is some other refusal, which a review request would neither fix nor
// describe.
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

// TestPipelinePublishRefusesAFailedDraft: the refusal must happen before the
// request, or the server's 400 lands in the needs-review fallback and is
// reported as a review request.
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
	if !strings.Contains(stderr, "pipeline push") {
		t.Errorf("the refusal does not name the fix:\n%s", stderr)
	}
	if len(f.committed) != 0 || len(f.reviewRequested) != 0 {
		t.Errorf("a failed draft was still submitted: committed=%v reviewed=%v",
			f.committed, f.reviewRequested)
	}
}

// TestPipelinePublishRefusesANeverBuiltDraft: a checked-out draft stays `pending`
// until something syncs it, so `pending` most often means never built, not
// still building.
func TestPipelinePublishRefusesANeverBuiltDraft(t *testing.T) {
	f := newFakePipelineInstance(t)
	signInPipeline(t, f)
	root := seedBoundFolder(t, f)
	// A push whose sync failed leaves a draft holding the SQL that nothing built.
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
// scope is unknown, not private, so the 400 review fallback stays armed.
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

// TestPipelinePublishDoesNotResubmitADraftAlreadyInReview: a second request is
// harmless server-side but reads as progress that did not happen.
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

// TestPipelinePublishRefusesAStaleBase: the commit's base-version CAS refuses a
// table published to since the draft forked. The review payload's baseStale
// still runs first because it names the intervening versions, where the 409
// names only the head.
func TestPipelinePublishRefusesAStaleBase(t *testing.T) {
	f := newFakePipelineInstance(t)
	signInPipeline(t, f)
	root := seedStagedFolder(t, f, "private")
	f.intervening["table-orders"] = []api.TableInterveningVersion{
		{VersionID: "table-version-2", Name: "Orders", CommittedAt: time.Now()},
	}

	out, stderr, err := runPipelineCLI(t, root, "pipeline", "publish", "--json")
	if err == nil {
		t.Fatalf("publish succeeded over a version nobody saw:\n%s", stderr)
	}
	if !strings.Contains(stderr, "table-version-2") {
		t.Errorf("the intervening version was not named:\n%s", stderr)
	}
	if !strings.Contains(stderr, "--overwrite-remote") {
		t.Errorf("the refusal did not say what to do next:\n%s", stderr)
	}
	file := decodeJSON(t, out)["files"].([]any)[0].(map[string]any)
	// `conflict`, not `refused`: an agent must tell "retry after re-applying" from
	// "you may not do this at all".
	if file["outcome"] != outcomeConflict {
		t.Errorf("outcome = %v", file["outcome"])
	}
	warnings, _ := file["warnings"].([]any)
	if len(warnings) == 0 || !strings.Contains(warnings[0].(string), "--overwrite-remote") {
		t.Errorf("warnings = %+v", warnings)
	}
	if len(f.committed) != 0 {
		t.Errorf("committed = %+v", f.committed)
	}
	if f.draftOf["table-orders"] != "table-draft-5" {
		t.Errorf("the draft is gone: %+v", f.draftOf)
	}
}

// TestPipelinePublishOverwriteRemoteConfirmsTheCurrentHead: the override confirms
// the resolved head explicitly rather than a bare "force", and reports the
// overwritten version, the only trace that somebody else's work was discarded.
func TestPipelinePublishOverwriteRemoteConfirmsTheCurrentHead(t *testing.T) {
	f := newFakePipelineInstance(t)
	signInPipeline(t, f)
	root := seedStagedFolder(t, f, "private")
	f.intervening["table-orders"] = []api.TableInterveningVersion{
		{VersionID: "table-version-2", Name: "Orders", CommittedAt: time.Now()},
	}

	out, stderr, err := runPipelineCLI(t, root, "pipeline", "publish", "--overwrite-remote", "--json")
	if err != nil {
		t.Fatalf("publish --overwrite-remote: %v\n%s", err, stderr)
	}
	file := decodeJSON(t, out)["files"].([]any)[0].(map[string]any)
	if file["outcome"] != outcomePublished {
		t.Errorf("outcome = %v", file["outcome"])
	}
	if file["overwroteVersionID"] != "table-version-2" {
		t.Errorf("overwroteVersionID = %v, want the version that was discarded", file["overwroteVersionID"])
	}
	if len(f.committed) != 1 || f.committed[0] != "table-draft-5" {
		t.Errorf("committed = %+v", f.committed)
	}
	if !strings.Contains(stderr, "discarding what it changed") {
		t.Errorf("the override did not say what it was doing:\n%s", stderr)
	}
}

// TestPipelinePublishOverwriteRemoteIsInertWithoutAConflict: with nothing to
// overwrite the flag sends no confirm, so habitual use never pre-authorizes
// whatever lands next.
func TestPipelinePublishOverwriteRemoteIsInertWithoutAConflict(t *testing.T) {
	f := newFakePipelineInstance(t)
	signInPipeline(t, f)
	root := seedStagedFolder(t, f, "private")

	out, stderr, err := runPipelineCLI(t, root, "pipeline", "publish", "--overwrite-remote", "--json")
	if err != nil {
		t.Fatalf("publish: %v\n%s", err, stderr)
	}
	file := decodeJSON(t, out)["files"].([]any)[0].(map[string]any)
	if file["outcome"] != outcomePublished {
		t.Errorf("outcome = %v", file["outcome"])
	}
	if _, present := file["overwroteVersionID"]; present {
		t.Errorf("overwroteVersionID was reported on a publish that overwrote nothing: %+v", file)
	}
}

// TestPipelinePublishWarnsAboutStaleInputs: the server's GetDependents excludes
// shadow rows, so an upstream publish never invalidates a staged draft and only
// this warning says it is stale.
func TestPipelinePublishWarnsAboutStaleInputs(t *testing.T) {
	f := newFakePipelineInstance(t)
	signInPipeline(t, f)
	root := seedBoundFolder(t, f)
	f.AddDraft("table-revenue", "table-draft-6", "SELECT * FROM {{ ref('table-orders') }}")
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

// TestPipelinePublishStillWarnsAboutStaleInputsWhenTheCodecRefusesTheFile: the
// codec returns an empty string for a file it refuses, so the staleness check must
// fall back to the file as written (its id-form refs) rather than see no refs.
// Two same-stem files in different subdirectories is a legal layout that makes
// every stem ref ambiguous.
func TestPipelinePublishStillWarnsAboutStaleInputsWhenTheCodecRefusesTheFile(t *testing.T) {
	f := newFakePipelineInstance(t)
	signInPipeline(t, f)
	seedFeature(f, "private")

	// An id-form ref plus an ambiguous stem, so pipelineCodec.toWire refuses it.
	revenue := "SELECT * FROM {{ ref('table-orders') }} JOIN {{ ref('dup') }} USING (id)"
	root := t.TempDir()
	manifest := &wfdir.Manifest{Kind: wfdir.KindPipeline, Title: "Sales pipeline"}
	manifest.SetBinding(f.Key(), wfdir.Binding{
		FeatureID: "collection-1",
		Tables: map[string]string{
			"revenue.sql":     "table-revenue",
			"raw/dup.sql":     "table-dup-a",
			"staging/dup.sql": "table-dup-b",
		},
	})
	writePipelineFolder(t, root, manifest, map[string]string{
		"revenue.sql":     revenue,
		"raw/dup.sql":     "SELECT 1",
		"staging/dup.sql": "SELECT 2",
	})
	f.AddDraft("table-revenue", "table-draft-9", revenue)
	f.tables["table-orders"].UpdatedAt = f.tables["table-draft-9"].UpdatedAt.Add(time.Hour)
	writePipelineBaseline(t, root, f.Key(), "collection-1",
		map[string]string{"revenue.sql": revenue},
		map[string]wfdir.TableState{"revenue.sql": {TableID: "table-revenue", DraftID: "table-draft-9"}})

	_, stderr, err := runPipelineCLI(t, root, "pipeline", "publish", "revenue.sql", "--json")
	if err != nil {
		t.Fatalf("publish: %v\n%s", err, stderr)
	}
	if !strings.Contains(stderr, "table-orders") || !strings.Contains(stderr, "older upstream data") {
		t.Fatalf("the staleness warning vanished for a file the codec refused:\n%s", stderr)
	}
}

// TestPipelinePublishCountsTheFolderLocalCascade: the count is the folder's own
// ref graph, never a tenant-wide number, because the real server cascade is
// wider and conditional.
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

// TestPipelinePublishCommitsInDependencyOrder: the filenames make alphabetical
// order the wrong order, so a publish that never sorted would fail.
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
// hint, never authority, since the web UI can commit or discard it meanwhile.
func TestPipelinePublishRecoversFromAStaleDraftPointer(t *testing.T) {
	f := newFakePipelineInstance(t)
	signInPipeline(t, f)
	root := seedStagedFolder(t, f, "private")
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
	inst := pipelineStateOf(t, root, f.Key())
	if inst.Tables["orders.sql"].DraftID != "" {
		t.Errorf("the stale pointer survived: %+v", inst.Tables["orders.sql"])
	}
}

// TestPipelinePublishWarnsWhenTheFolderIsDirty: a dirty folder warns but does not
// refuse; publish commits the draft, not the local files.
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
	if len(f.committed) != 1 {
		t.Errorf("committed = %+v", f.committed)
	}
}

// TestPipelinePublishIgnoresNonSQLDirt: warnIfDirty walks the folder with its own
// kind, so a README edit is not reported as local dirt.
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

// TestPipelinePublishTellsANameCollisionApartFromAFork: several refusals share a
// 409, so the CLI branches on the body's `code`; fork advice (discard,
// --overwrite-remote) for a name collision destroys work and never frees the name.
func TestPipelinePublishTellsANameCollisionApartFromAFork(t *testing.T) {
	f := newFakePipelineInstance(t)
	signInPipeline(t, f)
	root := seedStagedFolder(t, f, "private")
	const why = `a table named "Orders" already exists in this feature (table-d1abc)`
	f.nameTakenOnCommit["table-draft-5"] = why

	out, stderr, _ := runPipelineCLI(t, root, "pipeline", "publish", "--json")
	file := decodeJSON(t, out)["files"].([]any)[0].(map[string]any)
	if file["outcome"] != pushOutcomeRefused {
		t.Errorf("outcome = %v, want %q — a taken name is not a race somebody else won", file["outcome"], pushOutcomeRefused)
	}
	if msg, _ := file["error"].(string); !strings.Contains(msg, why) {
		t.Errorf("the server's own sentence was dropped:\n%s", msg)
	}
	for _, wrong := range []string{"pipeline discard", "--overwrite-remote", "forked"} {
		if strings.Contains(stderr, wrong) {
			t.Errorf("the fork advice %q was given for a name collision:\n%s", wrong, stderr)
		}
	}
	if len(f.committed) != 0 {
		t.Errorf("committed = %+v, want nothing", f.committed)
	}
}
