package commands

import (
	"slices"
	"strings"
	"testing"

	"github.com/ronjatech/ronja-cli/internal/api"
	"github.com/ronjatech/ronja-cli/internal/wfdir"
)

// --- validate ---------------------------------------------------------------

func TestValidateReportsFindingsAndExitsNonZero(t *testing.T) {
	f := newFakeInstance(t)
	f.validate = &api.ValidateResult{
		Findings: []api.ValidateFinding{
			{Severity: "warning", Code: "secret_dropped",
				Message: "secret sec-1 isn't reachable to you", Path: "lib/helpers.py"},
			{Severity: api.SeverityError, Code: "unresolved_ref",
				Message: `"table-gone" isn't a table`, Path: "main.py", Marker: "{{ ref('table-gone') }}"},
		},
		Resolved: api.ValidateBindings{InputTableIDs: []string{"tbl-1", "tbl-2"}, SecretIDs: []string{"sec-2"}},
	}
	signIn(t, f)
	root := initFolder(t, f, map[string]string{"main.py": "x\n", "lib/helpers.py": "y\n"})

	out, err := runCLI(t, root, "wf", "validate", "--json")
	if err == nil {
		t.Fatal("validate exited zero with an error finding")
	}
	payload := decodeJSON(t, out)
	if payload["ok"].(bool) {
		t.Error("ok = true with an error finding")
	}
	if got := payload["findings"].([]any); len(got) != 2 {
		t.Errorf("findings = %v, want both severities passed through", got)
	}
	// The resolved block rides along verbatim — it is what the folder WOULD
	// bind to, and it is useful even when there are errors.
	resolved := payload["resolved"].(map[string]any)
	if len(resolved["inputTableIDs"].([]any)) != 2 {
		t.Errorf("resolved = %+v", resolved)
	}
}

func TestValidateCleanFolderExitsZero(t *testing.T) {
	f := newFakeInstance(t)
	f.validate = &api.ValidateResult{Findings: []api.ValidateFinding{{
		Severity: "warning", Code: "secret_dropped",
		Message: "secret sec-1 isn't reachable", Path: "main.py",
	}}}
	signIn(t, f)
	root := initFolder(t, f, map[string]string{"main.py": "x\n"})

	out, err := runCLI(t, root, "wf", "validate", "--json")
	if err != nil {
		t.Fatalf("a warning made validate fail: %v", err)
	}
	if !decodeJSON(t, out)["ok"].(bool) {
		t.Error("ok = false with only warnings")
	}
}

// Validation needs a feature, not a workflow — that is what makes it usable on
// a folder created by `wf init` that has never been pushed.
// Validate is the command people run FIRST, which makes it the worst place to
// skip the local guard: a 200 MB CSV somebody dropped in the folder has to fail
// by NAME, locally, rather than as a 400 from somewhere deep in the request
// path — or, before the server grew a cap, as a request that simply sat there.
func TestValidateRefusesBinaryAndOversizedFilesBeforeAsking(t *testing.T) {
	for _, tc := range []struct {
		name    string
		file    string
		content string
	}{
		{"null bytes", "blob.dat", "abc\x00def"},
		{"oversized", "big.py", strings.Repeat("x", maxFileBytes+1)},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newFakeInstance(t)
			signIn(t, f)
			root := initFolder(t, f, map[string]string{"main.py": "x\n", tc.file: tc.content})

			_, err := runCLI(t, root, "wf", "validate", "--json")
			if err == nil {
				t.Fatalf("validate accepted %s", tc.file)
			}
			if !strings.Contains(err.Error(), tc.file) {
				t.Errorf("error = %v, want it to name the file", err)
			}
			for _, req := range f.Requests {
				if strings.Contains(req, "validate") {
					t.Errorf("posted the folder anyway: %v", f.Requests)
				}
			}
		})
	}
}

func TestValidateWorksBeforeTheWorkflowExists(t *testing.T) {
	f := newFakeInstance(t)
	signIn(t, f)
	root := initFolder(t, f, map[string]string{"main.py": "x\n"})

	if _, err := runCLI(t, root, "wf", "validate", "--json"); err != nil {
		t.Fatalf("validate: %v", err)
	}
	// Two requests, and only two: the organization lookup an environment token
	// owes before this folder's binding may be trusted (it supplies the featureID
	// validate posts), and the validate itself. Nothing about the WORKFLOW is
	// read, which is what makes validate work before one exists.
	for _, req := range f.Requests {
		if !strings.HasSuffix(req, "/validate") && req != "GET /api/v2/authentication/me" {
			t.Errorf("validate made an extra request: %v", f.Requests)
		}
	}
}

// The bound row's SAVED parameters are part of what a save is checked against,
// so a validate that omitted them would pass a folder whose push then fails.
func TestValidateSendsTheBoundRowsParameters(t *testing.T) {
	f := newFakeInstance(t)
	f.AddWorkflow(&api.Workflow{ID: "wf-1", Lifecycle: api.LifecycleLive,
		Parameters: []api.WorkflowParameter{{Name: "region", Type: "select", OptionsQuery: "SELECT r FROM {{ ref('tbl-1') }}"}}},
		api.WorkflowFile{Path: "main.py", Content: "M\n"})
	signIn(t, f)
	root := cloneFolder(t, f, "wf-1")

	if _, err := runCLI(t, root, "wf", "validate", "--json"); err != nil {
		t.Fatalf("validate: %v", err)
	}
	if len(f.validated) != 1 {
		t.Fatalf("validate calls = %d, want 1", len(f.validated))
	}
	if params := f.validated[0].Parameters; len(params) != 1 || params[0].Name != "region" {
		t.Errorf("validated parameters = %+v, want the row's", params)
	}
	// Still a dry run: reading the row to build the request must not open a
	// draft or write anything.
	for _, req := range f.Requests {
		if strings.HasPrefix(req, "PUT ") || strings.HasPrefix(req, "DELETE ") || strings.Contains(req, "/checkout") {
			t.Errorf("validate changed something: %v", f.Requests)
		}
	}
}

// Validate persists nothing and is advisory, so a row it cannot read costs the
// parameters rather than the command.
func TestValidateSurvivesAnUnreadableRow(t *testing.T) {
	f := newFakeInstance(t)
	f.AddWorkflow(&api.Workflow{ID: "wf-1", Lifecycle: api.LifecycleLive},
		api.WorkflowFile{Path: "main.py", Content: "M\n"})
	signIn(t, f)
	root := cloneFolder(t, f, "wf-1")
	delete(f.workflows, "wf-1")

	if _, err := runCLI(t, root, "wf", "validate", "--json"); err != nil {
		t.Fatalf("validate failed over a row it did not need: %v", err)
	}
	if len(f.validated) != 1 || len(f.validated[0].Parameters) != 0 {
		t.Errorf("validated = %+v, want the files alone", f.validated)
	}
}

// --- publish ----------------------------------------------------------------

// A parentless draft — a workflow created by a first push and never published —
// is published, not committed: there is nothing to commit onto.
func TestPublishParentlessDraftPublishes(t *testing.T) {
	f := newFakeInstance(t)
	signIn(t, f)
	root := initFolder(t, f, map[string]string{"main.py": "x\n"})
	if _, err := runCLI(t, root, "wf", "push", "--json"); err != nil {
		t.Fatalf("push: %v", err)
	}

	out, err := runCLI(t, root, "wf", "publish", "--json")
	if err != nil {
		t.Fatalf("publish: %v", err)
	}
	payload := decodeJSON(t, out)
	if payload["outcome"] != outcomePublished {
		t.Errorf("outcome = %v", payload["outcome"])
	}
	if len(f.publishedIDs) != 1 || len(f.committed) != 0 {
		t.Errorf("published %v, committed %v", f.publishedIDs, f.committed)
	}
}

// The post-publish baseline refresh is the SECOND way a folder ends up with a
// baseline claiming a file no walk will ever produce: it rebuilds from the
// server's file list exactly the way clone does. A `.env` added in the web
// builder between the push and the publish must not be recorded — a baseline
// that claims it is on disk turns the next status into a phantom deletion and
// the next push into a real, server-side one.
//
// It degrades rather than failing: the publish already happened, so the note is
// the honest report and the PREVIOUS baseline (stale, and healed by the next
// push) is the safe thing to leave behind.
func TestPublishDoesNotRecordABaselineTheWalkCanNeverMatch(t *testing.T) {
	f := newFakeInstance(t)
	signIn(t, f)
	root := initFolder(t, f, map[string]string{"main.py": "x\n"})
	if _, err := runCLI(t, root, "wf", "push", "--json"); err != nil {
		t.Fatalf("push: %v", err)
	}
	// Someone adds a dotfile in the web builder. The server is perfectly happy
	// with the path; the folder can never enumerate it.
	f.putFile("wf-new-1", ".env", "SECRET=1\n")

	out, err := runCLI(t, root, "wf", "publish", "--json")
	if err != nil {
		t.Fatalf("publish failed over a baseline it could not refresh: %v", err)
	}
	if decodeJSON(t, out)["outcome"] != outcomePublished {
		t.Fatalf("outcome = %s", out)
	}
	state, err := wfdir.LoadState(root)
	if err != nil {
		t.Fatalf("load state: %v", err)
	}
	baseline := state.For(f.Key())
	if baseline == nil {
		t.Fatal("the refusal wiped the baseline instead of leaving the previous one")
	}
	if _, claimed := baseline.Files[".env"]; claimed {
		t.Errorf("baseline claims .env is on disk: %+v", baseline.Files)
	}
	// And the folder reads as LOCALLY clean rather than as having deleted a
	// file nobody ever had. (Remote drift is a different, honest report: the
	// file really was added on the server. What must not happen is the CLI
	// attributing it to the user as a local deletion, which is the thing a push
	// would then act on.)
	statusOut, err := runCLI(t, root, "wf", "status", "--json")
	if err != nil {
		t.Fatalf("status: %v", err)
	}
	local, _ := decodeJSON(t, statusOut)["local"].(map[string]any)
	if deleted, _ := local["deleted"].([]any); len(deleted) != 0 {
		t.Errorf("status invented a local deletion %v:\n%s", deleted, statusOut)
	}
}

func TestPublishPrivateParentCommits(t *testing.T) {
	f := newFakeInstance(t)
	f.AddWorkflow(&api.Workflow{ID: "wf-1", Lifecycle: api.LifecycleLive, FeatureScope: "private"},
		api.WorkflowFile{Path: "main.py", Content: "old\n"})
	signIn(t, f)
	root := cloneFolder(t, f, "wf-1")
	writeLocal(t, root, "main.py", "new\n")
	if _, err := runCLI(t, root, "wf", "push", "--json"); err != nil {
		t.Fatalf("push: %v", err)
	}

	out, err := runCLI(t, root, "wf", "publish", "--json")
	if err != nil {
		t.Fatalf("publish: %v", err)
	}
	payload := decodeJSON(t, out)
	if payload["outcome"] != outcomePublished {
		t.Errorf("outcome = %v", payload["outcome"])
	}
	if len(f.committed) != 1 {
		t.Errorf("committed = %v, want the draft", f.committed)
	}
	if len(f.reviewRequested) != 0 {
		t.Errorf("asked for a review on a private workflow: %v", f.reviewRequested)
	}
	// The baseline now describes the live row the commit landed on.
	assertBaselineMatchesDisk(t, root, f.Key())
}

// The routing decision, made from the parent's scope and the caller's role
// rather than from a rejection message: a non-admin cannot commit onto a shared
// workflow, so the review request is the FIRST call, not a fallback.
func TestPublishSharedParentAsNonAdminGoesStraightToReview(t *testing.T) {
	f := newFakeInstance(t)
	f.privilegeLevel = 50 // ordinary user
	f.AddWorkflow(&api.Workflow{ID: "wf-1", Lifecycle: api.LifecycleLive, FeatureScope: "organization"},
		api.WorkflowFile{Path: "main.py", Content: "old\n"})
	signIn(t, f)
	root := cloneFolder(t, f, "wf-1")
	writeLocal(t, root, "main.py", "new\n")
	if _, err := runCLI(t, root, "wf", "push", "--json"); err != nil {
		t.Fatalf("push: %v", err)
	}

	out, err := runCLI(t, root, "wf", "publish", "--json")
	if err != nil {
		t.Fatalf("publish: %v", err)
	}
	payload := decodeJSON(t, out)
	if payload["outcome"] != outcomeSubmittedForReview {
		t.Errorf("outcome = %v, want %s", payload["outcome"], outcomeSubmittedForReview)
	}
	if len(f.reviewRequested) != 1 {
		t.Errorf("reviewRequested = %v", f.reviewRequested)
	}
	for _, req := range f.Requests {
		if strings.HasSuffix(req, "/commit") {
			t.Errorf("attempted a commit it could not make: %v", f.Requests)
		}
	}
}

func TestPublishAdminCommitsSharedParent(t *testing.T) {
	f := newFakeInstance(t)
	f.privilegeLevel = 10 // admin
	f.AddWorkflow(&api.Workflow{ID: "wf-1", Lifecycle: api.LifecycleLive, FeatureScope: "organization"},
		api.WorkflowFile{Path: "main.py", Content: "old\n"})
	signIn(t, f)
	root := cloneFolder(t, f, "wf-1")
	writeLocal(t, root, "main.py", "new\n")
	if _, err := runCLI(t, root, "wf", "push", "--json"); err != nil {
		t.Fatalf("push: %v", err)
	}

	out, err := runCLI(t, root, "wf", "publish", "--json")
	if err != nil {
		t.Fatalf("publish: %v", err)
	}
	if decodeJSON(t, out)["outcome"] != outcomePublished {
		t.Errorf("an admin's commit did not report as published: %s", out)
	}
	if len(f.committed) != 1 {
		t.Errorf("committed = %v", f.committed)
	}
}

// The race the up-front routing cannot close: the role read said admin, the
// commit was refused anyway. The 400 falls back to a review request, and says
// so rather than reporting a publish that did not happen.
func TestPublishFallsBackToReviewOnACommitRejection(t *testing.T) {
	f := newFakeInstance(t)
	f.privilegeLevel = 10
	f.failCommit = 400
	f.AddWorkflow(&api.Workflow{ID: "wf-1", Lifecycle: api.LifecycleLive, FeatureScope: "organization"},
		api.WorkflowFile{Path: "main.py", Content: "old\n"})
	signIn(t, f)
	root := cloneFolder(t, f, "wf-1")
	writeLocal(t, root, "main.py", "new\n")
	if _, err := runCLI(t, root, "wf", "push", "--json"); err != nil {
		t.Fatalf("push: %v", err)
	}

	out, err := runCLI(t, root, "wf", "publish", "--json")
	if err != nil {
		t.Fatalf("publish: %v", err)
	}
	payload := decodeJSON(t, out)
	if payload["outcome"] != outcomeSubmittedForReview {
		t.Errorf("outcome = %v", payload["outcome"])
	}
	if len(f.reviewRequested) != 1 {
		t.Errorf("reviewRequested = %v", f.reviewRequested)
	}
	if detail, _ := payload["detail"].(string); !strings.Contains(detail, "refused") {
		t.Errorf("detail = %q, want it to say the commit was refused", detail)
	}
}

func TestPublishNoRequestReviewSurfacesTheRejection(t *testing.T) {
	f := newFakeInstance(t)
	f.privilegeLevel = 50
	f.failCommit = 400
	f.AddWorkflow(&api.Workflow{ID: "wf-1", Lifecycle: api.LifecycleLive, FeatureScope: "organization"},
		api.WorkflowFile{Path: "main.py", Content: "old\n"})
	signIn(t, f)
	root := cloneFolder(t, f, "wf-1")
	writeLocal(t, root, "main.py", "new\n")
	if _, err := runCLI(t, root, "wf", "push", "--json"); err != nil {
		t.Fatalf("push: %v", err)
	}

	_, err := runCLI(t, root, "wf", "publish", "--no-request-review", "--json")
	if err == nil {
		t.Fatal("publish swallowed the rejection")
	}
	if len(f.reviewRequested) != 0 {
		t.Errorf("asked for a review despite --no-request-review: %v", f.reviewRequested)
	}
}

// A parent whose featureScope never arrived — an older instance, or a row the
// read path did not stamp — falls through the shared-workflow branch entirely.
// The CLI attempts the commit and reports whatever the server says, rather than
// guessing at a scope it was not told.
func TestPublishUnstampedParentScopeAttemptsTheCommit(t *testing.T) {
	f := newFakeInstance(t)
	f.privilegeLevel = 50 // ordinary user: the scope, not the role, is the fork
	f.AddWorkflow(&api.Workflow{ID: "wf-1", Lifecycle: api.LifecycleLive, FeatureScope: ""},
		api.WorkflowFile{Path: "main.py", Content: "old\n"})
	signIn(t, f)
	root := cloneFolder(t, f, "wf-1")
	writeLocal(t, root, "main.py", "new\n")
	if _, err := runCLI(t, root, "wf", "push", "--json"); err != nil {
		t.Fatalf("push: %v", err)
	}

	out, err := runCLI(t, root, "wf", "publish", "--json")
	if err != nil {
		t.Fatalf("publish: %v", err)
	}
	if decodeJSON(t, out)["outcome"] != outcomePublished {
		t.Errorf("outcome = %s", out)
	}
	if len(f.committed) != 1 {
		t.Errorf("committed = %v, want the commit attempt", f.committed)
	}
	if len(f.reviewRequested) != 0 {
		t.Errorf("asked for a review on an unstamped scope: %v", f.reviewRequested)
	}
}

// And when that commit is refused, the refusal is surfaced rather than turned
// into a review request: the fallback is deliberately scoped to a parent the
// CLI KNOWS is shared, so an unstamped scope reports what the server said.
func TestPublishUnstampedParentScopeSurfacesARejection(t *testing.T) {
	f := newFakeInstance(t)
	f.privilegeLevel = 50
	f.failCommit = 400
	f.AddWorkflow(&api.Workflow{ID: "wf-1", Lifecycle: api.LifecycleLive, FeatureScope: ""},
		api.WorkflowFile{Path: "main.py", Content: "old\n"})
	signIn(t, f)
	root := cloneFolder(t, f, "wf-1")
	writeLocal(t, root, "main.py", "new\n")
	if _, err := runCLI(t, root, "wf", "push", "--json"); err != nil {
		t.Fatalf("push: %v", err)
	}

	_, err := runCLI(t, root, "wf", "publish", "--json")
	if err == nil {
		t.Fatal("publish swallowed the server's rejection")
	}
	if !strings.Contains(err.Error(), "admin required") {
		t.Errorf("error = %v, want the server's own message", err)
	}
	if len(f.reviewRequested) != 0 {
		t.Errorf("invented a review request: %v", f.reviewRequested)
	}
}

func TestPublishWithNoDraftRefuses(t *testing.T) {
	f := newFakeInstance(t)
	f.AddWorkflow(&api.Workflow{ID: "wf-1", Lifecycle: api.LifecycleLive},
		api.WorkflowFile{Path: "main.py", Content: "M\n"})
	signIn(t, f)
	root := cloneFolder(t, f, "wf-1")

	_, err := runCLI(t, root, "wf", "publish", "--json")
	if err == nil {
		t.Fatal("publish invented something to publish")
	}
	if !strings.Contains(err.Error(), "nothing to publish") {
		t.Errorf("error = %v", err)
	}
}

// --- discard ----------------------------------------------------------------

func TestDiscardWithNoDraftIsANoOp(t *testing.T) {
	f := newFakeInstance(t)
	f.AddWorkflow(&api.Workflow{ID: "wf-1", Lifecycle: api.LifecycleLive},
		api.WorkflowFile{Path: "main.py", Content: "M\n"})
	signIn(t, f)
	root := cloneFolder(t, f, "wf-1")

	out, err := runCLI(t, root, "wf", "discard", "--yes", "--json")
	if err != nil {
		t.Fatalf("discard: %v", err)
	}
	if decodeJSON(t, out)["outcome"] != outcomeNoDraft {
		t.Errorf("outcome = %s", out)
	}
	if len(f.discardedIDs) != 0 {
		t.Errorf("discarded something: %v", f.discardedIDs)
	}
}

// No terminal, no --yes: refuse rather than delete, and rather than block on a
// prompt nobody will answer.
func TestDiscardWithoutConfirmationRefuses(t *testing.T) {
	f := newFakeInstance(t)
	f.AddWorkflow(&api.Workflow{ID: "wf-1", Lifecycle: api.LifecycleLive},
		api.WorkflowFile{Path: "main.py", Content: "M\n"})
	f.AddDraft("wf-1", "draft-1", api.WorkflowFile{Path: "main.py", Content: "M\n"})
	signIn(t, f)
	root := cloneFolder(t, f, "wf-1")

	_, err := runCLI(t, root, "wf", "discard", "--json")
	if err == nil {
		t.Fatal("discard deleted a draft with no confirmation")
	}
	if !strings.Contains(err.Error(), "--yes") {
		t.Errorf("error = %v, want it to name the flag", err)
	}
	if len(f.discardedIDs) != 0 {
		t.Errorf("discarded anyway: %v", f.discardedIDs)
	}
}

// --json says the same thing a missing terminal does, and says it even from one:
// whoever asked for a machine-readable answer is a program, and a program handed
// a [y/N] prompt waits for an answer nobody is going to type. Tested against
// confirm directly because the case that matters is the one this suite cannot
// stage — a real terminal, where the no-terminal rule would not fire.
func TestConfirmUnderJSONRequiresYes(t *testing.T) {
	flagJSON = true
	t.Cleanup(func() { flagJSON = false })

	// The message has to name --json as the reason, not just --yes as the
	// remedy: this suite has no terminal either, so a refusal that only
	// mentioned the flag would pass whether or not --json was ever consulted.
	if _, err := confirm("DELETE workflow wf-1?", "deletes workflow wf-1 on the server", false); err == nil {
		t.Fatal("confirm was willing to prompt under --json")
	} else {
		// The refusal also has to describe what it is refusing: the same gate
		// stands in front of a whole-workflow delete, and fixed "this deletes
		// your draft" prose understates that exactly where it matters most.
		for _, want := range []string{"--yes", "--json", "deletes workflow wf-1"} {
			if !strings.Contains(err.Error(), want) {
				t.Errorf("error = %v, want it to mention %q", err, want)
			}
		}
	}
	// --yes is still the way through, which is the whole point of requiring it.
	if ok, err := confirm("Discard your draft?", "deletes your draft on the server", true); !ok || err != nil {
		t.Errorf("confirm(--yes) = %v, %v", ok, err)
	}
}

func TestDiscardResetsTheBaselineToLiveAndKeepsLocalFiles(t *testing.T) {
	f := newFakeInstance(t)
	f.AddWorkflow(&api.Workflow{ID: "wf-1", Lifecycle: api.LifecycleLive},
		api.WorkflowFile{Path: "main.py", Content: "live\n"})
	signIn(t, f)
	root := cloneFolder(t, f, "wf-1")
	writeLocal(t, root, "main.py", "mine\n")
	if _, err := runCLI(t, root, "wf", "push", "--json"); err != nil {
		t.Fatalf("push: %v", err)
	}

	out, err := runCLI(t, root, "wf", "discard", "--yes", "--json")
	if err != nil {
		t.Fatalf("discard: %v", err)
	}
	payload := decodeJSON(t, out)
	if payload["outcome"] != outcomeDiscarded {
		t.Fatalf("outcome = %s", out)
	}
	if len(f.discardedIDs) != 1 {
		t.Errorf("discardedIDs = %v", f.discardedIDs)
	}

	// Local files are the user's and are kept.
	if got := readFile(t, root, "main.py"); got != "mine\n" {
		t.Errorf("main.py = %q — discard touched local files", got)
	}
	// The baseline describes LIVE now, so the folder reads as changed against
	// it rather than as clean against a draft that no longer exists.
	state, err := wfdir.LoadState(root)
	if err != nil {
		t.Fatalf("load state: %v", err)
	}
	baseline := state.For(f.Key())
	if baseline == nil || baseline.SourceID != "wf-1" {
		t.Fatalf("baseline = %+v, want it pointing at the live row", baseline)
	}
	if baseline.Files["main.py"].SHA256 != wfdir.HashString("live\n") {
		t.Error("baseline does not hold the live content")
	}
}

// The draft is gone the moment the server says so. A baseline refresh that
// fails afterwards is a note, not a failure — reporting a discard that happened
// as one that did not would send the caller looking for a draft that is not
// there.
func TestDiscardSucceedsWhenTheBaselineCannotBeRefreshed(t *testing.T) {
	f := newFakeInstance(t)
	f.AddWorkflow(&api.Workflow{ID: "wf-1", Lifecycle: api.LifecycleLive},
		api.WorkflowFile{Path: "main.py", Content: "live\n"})
	f.AddDraft("wf-1", "draft-1", api.WorkflowFile{Path: "main.py", Content: "live\n"})
	signIn(t, f)
	root := cloneFolder(t, f, "wf-1")
	f.failFiles = 500

	out, err := runCLI(t, root, "wf", "discard", "--yes", "--json")
	if err != nil {
		t.Fatalf("discard failed over a baseline it could not refresh: %v", err)
	}
	if decodeJSON(t, out)["outcome"] != outcomeDiscarded {
		t.Errorf("outcome = %s", out)
	}
	if len(f.discardedIDs) != 1 {
		t.Errorf("discardedIDs = %v", f.discardedIDs)
	}
}

// A parentless draft IS the workflow: discarding it server-side would delete
// the whole thing, which is not what the word means here.
func TestDiscardRefusesANeverPublishedWorkflow(t *testing.T) {
	f := newFakeInstance(t)
	signIn(t, f)
	root := initFolder(t, f, map[string]string{"main.py": "x\n"})
	if _, err := runCLI(t, root, "wf", "push", "--json"); err != nil {
		t.Fatalf("push: %v", err)
	}

	_, err := runCLI(t, root, "wf", "discard", "--yes", "--json")
	if err == nil {
		t.Fatal("discard deleted a never-published workflow")
	}
	if !strings.Contains(err.Error(), "never been published") {
		t.Errorf("error = %v", err)
	}
	// The refusal has to name the way OUT, or the caller's only remaining move
	// is the web UI — which a terminal-only caller does not have.
	if !strings.Contains(err.Error(), "--delete-workflow") {
		t.Errorf("the refusal should point at --delete-workflow, got %v", err)
	}
	if len(f.discardedIDs) != 0 {
		t.Errorf("discarded anyway: %v", f.discardedIDs)
	}
	if len(f.deletedWorkflows) != 0 {
		t.Errorf("deleted without --delete-workflow: %v", f.deletedWorkflows)
	}
}

// --delete-workflow is the escape hatch for an abandoned first push: the
// workflow is deleted, local files survive, and the folder is unbound so the
// next push creates a fresh one in the same feature.
func TestDiscardDeleteWorkflowRemovesANeverPublishedWorkflow(t *testing.T) {
	f := newFakeInstance(t)
	signIn(t, f)
	root := initFolder(t, f, map[string]string{"main.py": "x\n"})
	out, err := runCLI(t, root, "wf", "push", "--json")
	if err != nil {
		t.Fatalf("push: %v", err)
	}
	created := decodeJSON(t, out)["workflowID"].(string)

	out, err = runCLI(t, root, "wf", "discard", "--delete-workflow", "--yes", "--json")
	if err != nil {
		t.Fatalf("discard --delete-workflow: %v", err)
	}
	if got := decodeJSON(t, out)["outcome"]; got != outcomeResourceDeleted {
		t.Errorf("outcome = %v, want %q", got, outcomeResourceDeleted)
	}
	if !slices.Contains(f.deletedWorkflows, created) {
		t.Errorf("workflow %s should have been deleted, got %v", created, f.deletedWorkflows)
	}
	// Local source is the author's and must survive — the whole point is that
	// the folder is reusable after an abandoned first push.
	if got := readFile(t, root, "main.py"); got != "x\n" {
		t.Errorf("local main.py should be untouched, got %q", got)
	}
	// Unbound from the workflow but still pointing at the feature, exactly as
	// `init` left it: the next push must create rather than look for a row that
	// is gone.
	m, err := wfdir.LoadManifest(root, wfdir.WorkflowKind)
	if err != nil {
		t.Fatalf("load manifest: %v", err)
	}
	binding, bound, err := m.Binding(f.Key())
	if err != nil {
		t.Fatalf("binding: %v", err)
	}
	if bound && binding.WorkflowID != "" {
		t.Errorf("the folder should no longer name a workflow, got %q", binding.WorkflowID)
	}
	if bound && binding.FeatureID == "" {
		t.Error("the feature binding should survive so the next push recreates in place")
	}
}

// A folder can be bound to several instances at once — localhost, staging and
// production is the shape this whole per-(instance, organization) keying exists
// for — and each keeps its OWN baseline. Deleting an abandoned draft on one of
// them must not touch the others'.
//
// The failure this guards is not cosmetic: a wiped baseline is indistinguishable
// from "never synced here", so the next push against a non-empty remote trips
// the drift guard and the way out it names is --force. Throwing away a staging
// draft would end with production overwritten without comparison.
func TestDiscardDeleteWorkflowKeepsOtherInstancesBaselines(t *testing.T) {
	f := newFakeInstance(t)
	signIn(t, f)
	root := initFolder(t, f, map[string]string{"main.py": "x\n"})
	if _, err := runCLI(t, root, "wf", "push", "--json"); err != nil {
		t.Fatalf("push: %v", err)
	}

	// A production baseline this command has no business touching, alongside the
	// one the push just wrote for the fake instance.
	other := wfdir.InstanceKey{URL: "https://prod.example.com", TenantID: "ten-prod"}
	state, err := wfdir.LoadState(root)
	if err != nil {
		t.Fatalf("load state: %v", err)
	}
	state.Set(other, &wfdir.InstanceState{
		SourceID: "wf-prod-9", SourceLifecycle: api.LifecycleLive,
		Title: "Monthly report", Entrypoint: "main.py",
		Files: map[string]wfdir.FileState{"main.py": {SHA256: wfdir.HashString("shipped\n"), UpdatedAt: "2026-08-01T09:00:00Z"}},
	})
	if err := wfdir.SaveState(root, state); err != nil {
		t.Fatalf("write state: %v", err)
	}
	before := marshalJSON(t, state.For(other))

	if _, err := runCLI(t, root, "wf", "discard", "--delete-workflow", "--yes", "--json"); err != nil {
		t.Fatalf("discard --delete-workflow: %v", err)
	}

	after, err := wfdir.LoadState(root)
	if err != nil {
		t.Fatalf("load state: %v", err)
	}
	if got := marshalJSON(t, after.For(other)); got != before {
		t.Errorf("production's baseline was rewritten by a discard against another instance:\n  before %s\n  after  %s", before, got)
	}
	// And this instance's is gone, so the folder reads as freshly initialised
	// here — every local file added, nothing to drift against.
	if base := after.For(f.Key()); base != nil {
		t.Errorf("the discarded instance's baseline should be gone, got %+v", base)
	}
}

// A binding that names an EDIT SHADOW rather than the workflow is a hand-edited
// ronja.json, and --delete-workflow must not read it as "never published" —
// that would delete somebody's in-progress edits and unbind the folder from the
// live workflow, while reporting that nothing was ever published.
func TestDiscardDeleteWorkflowRefusesADraftOfALiveWorkflow(t *testing.T) {
	f := newFakeInstance(t)
	live := f.AddWorkflow(&api.Workflow{ID: "wf-1", Lifecycle: api.LifecycleLive},
		api.WorkflowFile{Path: "main.py", Content: "live\n"})
	shadow := f.AddWorkflow(&api.Workflow{ID: "wf-draft-1", Lifecycle: api.LifecycleDraft, ParentWorkflowID: live.ID},
		api.WorkflowFile{Path: "main.py", Content: "mine\n"})
	signIn(t, f)

	// Bound to the DRAFT's id, which no push writes but a hand edit can.
	root := initFolder(t, f, map[string]string{"main.py": "mine\n"})
	m, err := wfdir.LoadManifest(root, wfdir.WorkflowKind)
	if err != nil {
		t.Fatalf("load manifest: %v", err)
	}
	m.SetBinding(f.Key(), wfdir.Binding{WorkflowID: shadow.ID, FeatureID: "feat-1"})
	if err := wfdir.SaveManifest(root, m); err != nil {
		t.Fatalf("save manifest: %v", err)
	}

	_, err = runCLI(t, root, "wf", "discard", "--delete-workflow", "--yes", "--json")
	if err == nil {
		t.Fatal("expected a binding naming an edit shadow to be refused")
	}
	// The way out is the parent id, so the error has to name it.
	if !strings.Contains(err.Error(), live.ID) {
		t.Errorf("the refusal should name the workflow the draft belongs to, got %v", err)
	}
	if len(f.deletedWorkflows) != 0 {
		t.Errorf("nothing should have been deleted, got %v", f.deletedWorkflows)
	}
}

// --delete-workflow on a LIVE workflow is a caller error, not a licence to
// delete a published one. Refusing beats ignoring: the flag reads like it would.
func TestDiscardDeleteWorkflowRefusesALiveWorkflow(t *testing.T) {
	f := newFakeInstance(t)
	signIn(t, f)
	root := initFolder(t, f, map[string]string{"main.py": "x\n"})
	if _, err := runCLI(t, root, "wf", "push", "--json"); err != nil {
		t.Fatalf("push: %v", err)
	}
	if _, err := runCLI(t, root, "wf", "publish", "--json"); err != nil {
		t.Fatalf("publish: %v", err)
	}

	_, err := runCLI(t, root, "wf", "discard", "--delete-workflow", "--yes", "--json")
	if err == nil {
		t.Fatal("expected --delete-workflow to be refused on a live workflow")
	}
	if !strings.Contains(err.Error(), "never been published") {
		t.Errorf("the refusal should explain the flag's scope, got %v", err)
	}
	if len(f.deletedWorkflows) != 0 {
		t.Errorf("a live workflow must never be deleted, got %v", f.deletedWorkflows)
	}
}

// --- commit CAS --------------------------------------------------------------

// stagedDraft is the state every commit-CAS test starts from: a live workflow,
// a folder cloned from it, and a pushed draft ready to commit.
func stagedDraft(t *testing.T, f *fakeInstance, scope string) string {
	t.Helper()
	f.AddWorkflow(&api.Workflow{ID: "wf-1", Lifecycle: api.LifecycleLive, FeatureScope: scope},
		api.WorkflowFile{Path: "main.py", Content: "old\n"})
	signIn(t, f)
	root := cloneFolder(t, f, "wf-1")
	writeLocal(t, root, "main.py", "mine\n")
	if _, err := runCLI(t, root, "wf", "push", "--json"); err != nil {
		t.Fatalf("push: %v", err)
	}
	return root
}

// Somebody published a new version while this draft was open. Nothing is
// committed, the draft is intact, and the way forward is named — but it is
// never taken automatically.
func TestPublishRefusesACommitWhoseParentMoved(t *testing.T) {
	f := newFakeInstance(t)
	f.privilegeLevel = 10
	// Org-scoped, so this ALSO pins that a 409 never falls into the
	// needs-review fallback: that branch is keyed on 400, and turning "your
	// draft is based on stale code" into "an admin will review it" would report
	// a publish that never happened as a successful one.
	root := stagedDraft(t, f, "organization")
	f.commitHeadVersionID = "ver-2"

	var err error
	narration := captureStderr(t, func() {
		_, err = runCLI(t, root, "wf", "publish", "--json")
	})
	if err == nil {
		t.Fatal("publish committed over a version it had never seen")
	}
	if !strings.Contains(err.Error(), "ver-2") {
		t.Errorf("error = %v, want the server's message naming the current version", err)
	}
	// What the author is TOLD, which is the difference between this refusal and
	// a bare rejection: what moved, and the two ways forward.
	for _, want := range []string{"ver-2", "--overwrite-remote", "your draft is intact"} {
		if !strings.Contains(narration, want) {
			t.Errorf("stderr did not mention %q:\n%s", want, narration)
		}
	}
	if len(f.committed) != 0 {
		t.Errorf("committed = %v, want nothing", f.committed)
	}
	if len(f.reviewRequested) != 0 {
		t.Errorf("a conflict was turned into a review request: %v", f.reviewRequested)
	}
	// Exactly one attempt, and it confirmed nothing. No auto-retry, no
	// auto-override.
	if got := f.commitConfirmations; len(got) != 1 || got[0] != "" {
		t.Errorf("commit confirmations = %q, want one empty attempt", got)
	}
	// The draft survives, which is what makes re-applying possible at all.
	if _, alive := f.workflows[f.draftOf["wf-1"]]; !alive {
		t.Error("the refused commit destroyed the draft")
	}
}

// A conflict has to be legible to a MACHINE, not only to the person reading
// stderr. `wf publish` is the command an agent runs unattended, and the two
// refusals it has to tell apart — "somebody committed first, re-apply and try
// again" and "you may not commit this at all" — are both a non-zero exit with
// prose on stderr. Substring-matching that prose is the exact fragility
// HeadVersionID refuses to accept from the 409 body, so the payload says which
// it was, and is emitted even though the command fails.
func TestPublishReportsAConflictInTheJSONPayload(t *testing.T) {
	t.Run("refused", func(t *testing.T) {
		f := newFakeInstance(t)
		root := stagedDraft(t, f, "private")
		f.commitHeadVersionID = "ver-2"

		var out string
		var err error
		captureStderr(t, func() {
			out, err = runCLI(t, root, "wf", "publish", "--json")
		})
		if err == nil {
			t.Fatal("publish reported success through a conflict")
		}
		// The whole point: stdout is a parseable object rather than empty.
		payload := decodeJSON(t, out)
		if payload["outcome"] != outcomeConflict {
			t.Errorf("outcome = %v, want %q", payload["outcome"], outcomeConflict)
		}
		// And it is NOT one of the two success values — a caller that only
		// knows those must not read this as a publish that happened.
		if payload["outcome"] == outcomePublished || payload["outcome"] == outcomeSubmittedForReview {
			t.Error("a conflict reported itself as a successful outcome")
		}
		// The server's own account survives into the payload, prose and all,
		// so a human reading a CI log still learns which version won.
		if msg, _ := payload["error"].(string); !strings.Contains(msg, "ver-2") {
			t.Errorf("error = %q, want the server's message naming the current version", msg)
		}
		if payload["draftID"] == nil || payload["workflowID"] == nil {
			t.Errorf("payload identifies nothing to act on: %v", payload)
		}
	})

	// The second 409, mid-override. Same class of refusal — somebody got there
	// first — so it must report the same way rather than looking like a
	// different kind of failure because it arrived on a different code path.
	t.Run("second conflict during --overwrite-remote", func(t *testing.T) {
		f := newFakeInstance(t)
		root := stagedDraft(t, f, "private")
		f.versions["wf-1"] = []api.Workflow{{ID: "ver-2", Lifecycle: api.LifecycleVersion}}
		f.commitHeadVersionID = "ver-3"

		var out string
		var err error
		captureStderr(t, func() {
			out, err = runCLI(t, root, "wf", "publish", "--overwrite-remote", "--json")
		})
		if err == nil {
			t.Fatal("publish reported success through a second conflict")
		}
		if payload := decodeJSON(t, out); payload["outcome"] != outcomeConflict {
			t.Errorf("outcome = %v, want %q", payload["outcome"], outcomeConflict)
		}
	})

	// The negative half of the same contract: a refusal that is NOT a conflict
	// must not claim to be one. A commit the server rejects for any other
	// reason is a different problem with a different remedy, and an agent
	// keyed on "conflict" would re-clone and re-apply for nothing.
	t.Run("a non-conflict refusal is not a conflict", func(t *testing.T) {
		f := newFakeInstance(t)
		root := stagedDraft(t, f, "private")
		f.failCommit = 500

		var out string
		var err error
		captureStderr(t, func() {
			out, err = runCLI(t, root, "wf", "publish", "--json")
		})
		if err == nil {
			t.Fatal("publish reported success through a server error")
		}
		// This path has nothing to report, so it emits nothing — but it must
		// certainly not emit a conflict.
		if out != "" {
			if payload := decodeJSON(t, out); payload["outcome"] == outcomeConflict {
				t.Errorf("a 500 was reported as a conflict: %v", payload)
			}
		}
	})
}

// The flag: resolve the CURRENT head from the versions listing and confirm it
// explicitly. The id is READ, never scraped out of the 409's prose.
func TestPublishOverwriteRemoteConfirmsTheCurrentHead(t *testing.T) {
	f := newFakeInstance(t)
	root := stagedDraft(t, f, "private")
	f.commitHeadVersionID = "ver-2"
	f.versions["wf-1"] = []api.Workflow{
		{ID: "ver-2", Lifecycle: api.LifecycleVersion},
		{ID: "ver-1", Lifecycle: api.LifecycleVersion},
	}

	out, err := runCLI(t, root, "wf", "publish", "--overwrite-remote", "--json")
	if err != nil {
		t.Fatalf("publish --overwrite-remote: %v", err)
	}
	payload := decodeJSON(t, out)
	if payload["outcome"] != outcomePublished {
		t.Errorf("outcome = %v", payload["outcome"])
	}
	// Reported honestly: the payload records WHICH version was discarded, which
	// an ordinary publish never carries.
	if payload["overwroteVersionID"] != "ver-2" {
		t.Errorf("overwroteVersionID = %v, want ver-2", payload["overwroteVersionID"])
	}
	if detail, _ := payload["detail"].(string); !strings.Contains(detail, "ver-2") {
		t.Errorf("detail = %q, want it to name the version it overwrote", detail)
	}
	// The retry confirmed the head — element 0 of the listing — and there were
	// exactly two attempts.
	if got := f.commitConfirmations; len(got) != 2 || got[0] != "" || got[1] != "ver-2" {
		t.Fatalf("commit confirmations = %q, want [\"\", \"ver-2\"]", got)
	}
	if len(f.committed) != 1 {
		t.Errorf("committed = %v, want the one successful commit", f.committed)
	}
}

// A workflow that has never been versioned answers the listing with [], and its
// anchor is then its OWN id — mirroring rworkflow.resolveHeadVersionID. Sending
// "" instead reads server-side as "no override" and is refused all over again.
func TestPublishOverwriteRemoteUsesTheWorkflowIDWhenUnversioned(t *testing.T) {
	f := newFakeInstance(t)
	root := stagedDraft(t, f, "private")
	f.commitHeadVersionID = "wf-1"

	if _, err := runCLI(t, root, "wf", "publish", "--overwrite-remote", "--json"); err != nil {
		t.Fatalf("publish --overwrite-remote: %v", err)
	}
	if got := f.commitConfirmations; len(got) != 2 || got[1] != "wf-1" {
		t.Errorf("commit confirmations = %q, want the second to confirm the workflow's own id", got)
	}
}

// An override authorizes overwriting the version it was SHOWN, not whatever
// happens to be there by the time the request lands. So a third commit arriving
// mid-flight produces a fresh refusal — never a loop, which would authorize
// every version in turn.
func TestPublishOverwriteRemoteDoesNotRetryASecondConflict(t *testing.T) {
	f := newFakeInstance(t)
	root := stagedDraft(t, f, "private")
	// The listing says ver-2; the parent has already moved on to ver-3.
	f.versions["wf-1"] = []api.Workflow{{ID: "ver-2", Lifecycle: api.LifecycleVersion}}
	f.commitHeadVersionID = "ver-3"

	_, err := runCLI(t, root, "wf", "publish", "--overwrite-remote", "--json")
	if err == nil {
		t.Fatal("publish reported success through a second conflict")
	}
	if !strings.Contains(err.Error(), "moved again") {
		t.Errorf("error = %v, want it to say the workflow moved again", err)
	}
	// TWO attempts and no more. A loop would keep going until it won.
	if got := f.commitConfirmations; len(got) != 2 || got[0] != "" || got[1] != "ver-2" {
		t.Errorf("commit confirmations = %q, want exactly [\"\", \"ver-2\"]", got)
	}
	if len(f.committed) != 0 {
		t.Errorf("committed = %v, want nothing", f.committed)
	}
}

// The flag is not a general-purpose override: an ordinary commit with no
// conflict must not start confirming a head, because that would silently
// overwrite a version that landed between the read and the write.
func TestPublishOverwriteRemoteIsInertWithoutAConflict(t *testing.T) {
	f := newFakeInstance(t)
	root := stagedDraft(t, f, "private")

	out, err := runCLI(t, root, "wf", "publish", "--overwrite-remote", "--json")
	if err != nil {
		t.Fatalf("publish: %v", err)
	}
	if payload := decodeJSON(t, out); payload["overwroteVersionID"] != nil {
		t.Errorf("overwroteVersionID = %v on a publish that overwrote nothing", payload["overwroteVersionID"])
	}
	if got := f.commitConfirmations; len(got) != 1 || got[0] != "" {
		t.Errorf("commit confirmations = %q, want one unconfirmed commit", got)
	}
	for _, req := range f.Requests {
		if strings.HasSuffix(req, "/versions") {
			t.Errorf("read the version history without a conflict to resolve: %v", f.Requests)
		}
	}
}
