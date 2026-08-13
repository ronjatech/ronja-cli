package commands

import (
	"context"
	"fmt"
	"os"
	"reflect"
	"sort"
	"strings"

	"github.com/ronjatech/ronja-cli/internal/api"
	"github.com/ronjatech/ronja-cli/internal/wfdir"
	"github.com/spf13/cobra"
)

// `ronja wf push` syncs the folder into YOUR draft — never into the live
// workflow, which the server refuses to accept file writes on anyway.
//
// The order of operations is the design. Local guards first (they need no
// network and their diagnosis is entirely local), then a whole-folder validate
// (so a binding error is reported against the file that actually contains it),
// then the drift guard (so a web-builder edit to the same per-user draft is not
// silently overwritten), and only then the writes. Everything that can refuse
// does so before the first byte is persisted.
func newWorkflowPushCmd() *cobra.Command {
	var (
		noValidate bool
		force      bool
	)
	cmd := &cobra.Command{
		Use:   "push",
		Short: "Sync this folder into your draft of the workflow",
		Long: `Sync this folder into your draft of the workflow.

Pushes to a DRAFT, always. If the workflow is live and you have no draft, one
is checked out for you; if the folder has never been pushed to this instance,
the workflow is created and the binding recorded in ronja.json.

Before writing anything, push validates the whole folder server-side and
refuses on any error — the file-save path reports binding problems against
whichever file you happened to be writing, so one extra round trip buys an
error that names the right file. --no-validate skips it.

It also refuses when the draft changed on the server since your last sync (the
web builder edits the same draft), listing what moved. --force overwrites.

Publishing is a separate step:

  ronja wf publish                 commit the draft, or submit it for review`,
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			resolved, err := resolveInstance()
			if err != nil {
				return err
			}
			f, err := openFolder(cmd.Context(), resolved, wfdir.WorkflowKind)
			if err != nil {
				return err
			}
			result, err := runPush(cmd.Context(), f, pushOptions{Validate: !noValidate, Force: force})
			// The report is emitted even on failure: a push that stopped
			// halfway has already written files, and "which ones" is the first
			// thing anyone needs to know.
			if result != nil {
				if flagJSON {
					if emitErr := emitJSON(result); emitErr != nil {
						return emitErr
					}
				} else {
					printPushReport(result)
				}
			}
			return err
		},
	}
	cmd.Flags().BoolVar(&noValidate, "no-validate", false,
		"skip the server-side validation pass before pushing")
	cmd.Flags().BoolVar(&force, "force", false,
		"push even though the draft changed on the server since your last sync")
	return cmd
}

type pushOptions struct {
	Validate bool
	Force    bool
}

// pushResult is the --json shape and the human renderer's input, so the two
// cannot describe different things.
type pushResult struct {
	// Created reports that this push brought the workflow into existence.
	Created bool `json:"created"`
	// WorkflowID is the STABLE identity (what ronja.json records); DraftID is
	// the row the files were actually written to. For a parentless draft they
	// are the same id.
	WorkflowID string   `json:"workflowID"`
	DraftID    string   `json:"draftID"`
	Pushed     []string `json:"pushed"`
	Deleted    []string `json:"deleted"`
	Unchanged  int      `json:"unchanged"`
	// UpToDate reports a push that had nothing to do: the folder, the draft and
	// the baseline already agree, so not one byte was written.
	UpToDate bool `json:"upToDate"`
	// DraftUnderReview reports that the draft written to has already been
	// submitted for admin review — the push changed what someone is reviewing.
	DraftUnderReview bool `json:"draftUnderReview"`
	// Warnings are the server's soft save-time notices, verbatim.
	Warnings []string              `json:"warnings"`
	Findings []api.ValidateFinding `json:"findings,omitempty"`
	Bindings *api.ValidateBindings `json:"bindings,omitempty"`
	// Error describes a push that stopped part-way. The result above then
	// describes what DID happen before it stopped, which is the state the
	// draft is now in.
	Error string `json:"error,omitempty"`
	// Conflict reports that what stopped this push was a file precondition
	// refusing — somebody wrote to the draft between the listing this push
	// compared against and its own write.
	//
	// It exists because Error is PROSE. A caller that has to tell "somebody
	// got there first, re-read and re-apply" from "this file was rejected" has
	// otherwise only substring-matching to do it with, and that breaks the day
	// the server rewords a message. Same reason publish reports
	// outcome:"conflict".
	Conflict bool `json:"conflict"`
	// Target names the instance and organization this push landed in, so a
	// mistake is visible where it happens rather than only from a later status.
	Target string `json:"target,omitempty"`
	// URL is the frontend page for the draft this push wrote to, as the SERVER
	// reported it, and is rendered by the human report only.
	//
	// Deliberately NOT in the --json payload: this shape is a contract for
	// anything scripting the CLI, and a caller that wants the link machine-
	// readably reads it straight off the API response (`ronja api
	// /api/v2/workflow/<id> --jq .url`), which is where it comes from anyway.
	URL string `json:"-"`
}

// newClient builds the client a push talks to. A package var, not a call to
// api.New, for one reason — the same one that makes stdin.go's isTerminal a var.
//
// The branch below that a mistake would turn into silent data loss is the one
// behind a TIMED-OUT write, and a timeout is a deadline: the only ways to reach
// it are to wait a real one out or to hand the client a transport that reports
// one. Tests do the second. Nothing in the CLI ever reassigns this.
var newClient = api.New

// runPush is the whole state machine, kept out of the cobra closure so it is
// testable as a function and so the reporting path has exactly one shape.
//
// It returns (result, err): a non-nil result with a non-nil error is the
// partial-push case, and both halves matter.
func runPush(ctx context.Context, f *folder, opts pushOptions) (*pushResult, error) {
	client := newClient(f.Resolved.URL, f.Resolved.Token)
	result := &pushResult{
		WorkflowID: f.Binding.WorkflowID,
		Target:     describeTarget(f.Resolved),
		Pushed:     []string{},
		Deleted:    []string{},
		Warnings:   []string{},
	}

	// 1. Local guards. Nothing here needs the network, and each refusal names a
	// file the author can act on immediately.
	local, enumeration, err := readLocalFiles(f.Root, wfdir.WorkflowKind)
	if err != nil {
		return nil, err
	}
	for _, s := range enumeration.Skipped {
		fmt.Fprintf(os.Stderr, "  Note: skipping %s — %s\n", s.Path, s.Reason)
	}
	if err := checkPushable(local, maxFileBytes); err != nil {
		return nil, err
	}
	entrypoint := f.Manifest.Entrypoint
	if _, ok := local[entrypoint]; !ok {
		return nil, fmt.Errorf("the entrypoint %q is not in this folder — create it, or point \"entrypoint\" in %s at one of the files that is",
			entrypoint, wfdir.ManifestPath(f.Root))
	}

	// 2. Read what is already on the server for this binding. Pure reads, and
	// deliberately BEFORE the validate gate: the target's persisted parameters
	// are part of what a save is checked against (a stale
	// parameters[N].optionsQuery is refused by the first file write), so a
	// validate that omitted them would pass and the push would then fail with
	// no file to blame it on.
	existing, err := inspectTarget(ctx, client, f)
	if err != nil {
		return nil, err
	}
	featureID, err := featureIDFor(ctx, client, f, existing.Workflow)
	if err != nil {
		return nil, err
	}

	// 3. Validate, so a binding error names the file that contains it rather
	// than whichever file the sync happened to be writing.
	if opts.Validate {
		// The set this push is about to MAKE TRUE, not the one the row holds
		// now: a managed folder patches its declaration in step 5, so validating
		// the row's would check something this push is about to overwrite — and
		// would never check a newly declared select parameter's optionsQuery
		// markers at all. An unmanaged folder keeps borrowing the row's, which is
		// still the set that will be in force after it pushes.
		validateParams := existing.Parameters()
		if f.Manifest.ManagesParameters() {
			validateParams = f.Manifest.DeclaredParameters()
		}
		validated, err := client.ValidateWorkflowFiles(ctx, api.ValidateInput{
			FeatureID:  featureID,
			Entrypoint: entrypoint,
			Parameters: validateParams,
			Files:      validateFilesOf(local),
		})
		if err != nil {
			return nil, fmt.Errorf("validate before pushing: %w (use --no-validate to skip this check)", err)
		}
		result.Findings = validated.Findings
		if !validated.OK() {
			result.Error = countErrors(validated)
			return result, fmt.Errorf("%s", result.Error)
		}
		for _, finding := range validated.Findings {
			fmt.Fprintf(os.Stderr, "  Warning: %s — %s\n", finding.Path, finding.Message)
		}
	}

	// 4. Resolve the row to write to — the first step that changes anything.
	target, err := resolvePushTarget(ctx, client, f, featureID, existing, result)
	if err != nil {
		return nil, err
	}
	result.DraftID = target.ID
	// Recorded here rather than at the end, so every success path reports the
	// same link — including the up-to-date one, which returns before the
	// closing re-read. Both describe one row, and a row's page cannot move.
	result.URL = target.URL
	if target.SubmittedForReviewAt != nil {
		result.DraftUnderReview = true
		fmt.Fprintf(os.Stderr, "  Warning: draft %s has already been submitted for review — this push changes what the admin is reviewing.\n",
			target.ID)
	}

	// 5. Read what the row holds. ALWAYS, including for a workflow this push
	// just created: creating a workflow SEEDS a starter file at the entrypoint
	// inside the same transaction, so "just created" does not mean "empty". A
	// push that skipped this read and then failed on the entrypoint's own PUT
	// would record a baseline of zero files against a row holding that seed —
	// and the retry would read the seed as somebody else's drift and demand
	// --force for a workflow the CLI made seconds earlier.
	files, err := client.ListWorkflowFiles(ctx, target.ID)
	if err != nil {
		return nil, fmt.Errorf("read files of %s: %w", target.ID, err)
	}
	remoteFiles := contentByPath(files)

	// The drift GUARD is what a fresh create skips, and only that: nothing on
	// the server is older than this push, so there is nothing anyone could have
	// changed underneath it.
	baselineClean := false
	preconditions := filePreconditions{}
	if !result.Created {
		verdict, err := checkDrift(f, target, remoteFiles, opts.Force)
		if err != nil {
			return nil, err
		}
		baselineClean = verdict.BaselineClean
		// Server-side preconditions are armed EXACTLY where the drift guard
		// vouched for the baseline, and suppressed everywhere it was bypassed.
		// See filePreconditions for why that equivalence is the whole rule.
		if !verdict.Bypassed {
			preconditions = filePreconditions{Armed: true, Hashes: f.State.For(f.Key).Hashes()}
		}
	}

	// landed tracks what the baseline may honestly claim as the sync proceeds,
	// so a push that stops half way can still record an accurate one. stop is
	// the single exit for every failure after this point: whatever went wrong,
	// the baseline has to describe what DID land. Leaving it stale would make
	// the author's own half-finished push read as someone else's drift on the
	// next attempt — refusing the retry that fixes it, which is the opposite of
	// what the drift guard is for.
	//
	// It starts as what is ALREADY acknowledged rather than as the whole remote
	// (see acknowledgedRemote): a --force push that stops must not hand the
	// retry a baseline containing the very content it was forcing past.
	landed := acknowledgedRemote(f.State.For(f.Key), remoteFiles, local, result.Created)
	stop := func(err error) (*pushResult, error) {
		result.Error = err.Error()
		saveBaseline(f, target, landed)
		return result, err
	}

	// 6. Writes first, entrypoint leading: it is the file the server validates
	// the workflow's shape against, a half-finished sync still has a coherent
	// entrypoint — and the metadata patch below cannot name an entrypoint the
	// row holds no file for.
	warnings, err := putFiles(ctx, client, target.ID, entrypoint, local, remoteFiles, landed, preconditions, result)
	result.Warnings = append(result.Warnings, warnings...)
	if err != nil {
		return stop(err)
	}

	// 7. Then the metadata, BEFORE the deletions. An entrypoint RENAME only
	// works in this order: the new file has to exist on the row before the
	// server accepts the patch, and the row has to have stopped naming the old
	// file before it will let that one be deleted.
	patch := metadataPatch(f.Manifest, target)
	if !patch.Empty() {
		if err := client.UpdateWorkflow(ctx, target.ID, patch); err != nil {
			return stop(fmt.Errorf("update %s: %w", describePatch(patch), err))
		}
		// The row holds this now, and `target` is what a baseline recorded from
		// here on describes — including the one stop() writes if the deletions
		// below fail. Leaving it stale would record metadata the server stopped
		// holding a request ago, and the next push would read its own change
		// back as somebody else's.
		applyPatch(target, patch)
	}

	// 8. Deletions last.
	warnings, err = deleteFiles(ctx, client, target.ID, local, remoteFiles, landed, preconditions, result)
	result.Warnings = append(result.Warnings, warnings...)
	if err != nil {
		return stop(err)
	}

	// 9. A push that wrote nothing at all, against a baseline that already
	// described the server, has nothing to re-read.
	if !result.Created && baselineClean && len(result.Pushed) == 0 && len(result.Deleted) == 0 && patch.Empty() {
		result.UpToDate = true
		// It may still have something to RECORD: a push that had no files to
		// write can perfectly well have checked out a fresh draft on the way
		// here, and the baseline then names the row the files came from rather
		// than the one the next push will compare against. Written only when it
		// would actually change — a no-op push that re-stamped the baseline
		// would move its timestamp to describe a sync that never happened.
		if !baselineDescribes(f.State.For(f.Key), target) {
			f.State.Set(f.Key, baselineFromLocal(target, landed))
			if err := wfdir.SaveState(f.Root, f.State); err != nil {
				return result, err
			}
		}
		return result, nil
	}

	// A re-read: PUT :id answers with nothing and drops the save-time warnings,
	// and the file writes above have re-derived the row's bindings anyway, so
	// the current row has to come from a fresh GET.
	updated, err := client.GetWorkflow(ctx, target.ID)
	if err != nil {
		return stop(fmt.Errorf("re-read %s after pushing: %w", target.ID, err))
	}
	bindings := bindingsOf(updated)
	result.Bindings = &bindings

	// 10. The new baseline is what we just pushed: the server now holds exactly
	// these bytes, and `updated` is the freshest thing we know about the row.
	f.State.Set(f.Key, baselineFromLocal(updated, local))
	if err := wfdir.SaveState(f.Root, f.State); err != nil {
		return result, err
	}
	return result, nil
}

// metadataPatch is the difference between what the manifest says and what the
// row says. Empty when they agree, so the round trip is skipped.
//
// A blank manifest field is skipped rather than sent as "": the server's Patch
// fields are optionals, and an explicit empty string CLEARS the value rather
// than leaving it alone.
func metadataPatch(manifest *wfdir.Manifest, target *api.Workflow) api.WorkflowPatch {
	var patch api.WorkflowPatch
	if manifest.Title != "" && manifest.Title != target.Title {
		patch.Title = manifest.Title
	}
	if manifest.Entrypoint != "" && manifest.Entrypoint != target.Entrypoint {
		patch.Entrypoint = manifest.Entrypoint
	}
	// A folder that does not manage parameters never patches them — that is the
	// whole point of the absent/empty distinction, and it is what keeps a push
	// from a pre-parameters folder off a workflow's declaration.
	if manifest.ManagesParameters() {
		declared := manifest.DeclaredParameters()
		if !sameParameters(declared, target.Parameters) {
			// Normalized, so "declares none" always goes up as [] rather than
			// null — the server reads null as absent, which would be a silent
			// no-op exactly when the author is removing their last parameter.
			out := append([]api.WorkflowParameter{}, declared...)
			patch.Parameters = &out
		}
	}
	return patch
}

// sameParameters compares two declarations for sync purposes. Order is
// significant: it is the order the parameters are presented in, the manifest
// controls it, and reordering is a change worth pushing.
//
// reflect.DeepEqual is safe here despite DefaultValue being `any`: both sides
// arrive by JSON decode (the manifest from ronja.json, the row from the API), so
// a number is float64 on both and there is no int/float64 mismatch to trip on.
func sameParameters(a, b []api.WorkflowParameter) bool {
	if len(a) == 0 && len(b) == 0 {
		return true
	}
	return reflect.DeepEqual(a, b)
}

// applyPatch mirrors an ACCEPTED metadata patch onto the in-memory row.
//
// PUT :id answers with nothing, so this is the only way `target` keeps
// describing the server without the re-read the success path does anyway.
func applyPatch(row *api.Workflow, patch api.WorkflowPatch) {
	if patch.Title != "" {
		row.Title = patch.Title
	}
	if patch.Entrypoint != "" {
		row.Entrypoint = patch.Entrypoint
	}
	if patch.Parameters != nil {
		row.Parameters = *patch.Parameters
	}
}

// baselineDescribes reports whether the recorded baseline already describes this
// row — the same id, and the same metadata. False for a baseline written before
// the metadata was recorded at all, which is exactly when it wants replacing.
func baselineDescribes(baseline *wfdir.InstanceState, row *api.Workflow) bool {
	return baseline != nil &&
		baseline.SourceID == row.ID &&
		baseline.Title == row.Title &&
		baseline.Entrypoint == row.Entrypoint &&
		// An unrecorded parameter set (a baseline predating the guard) does not
		// make the baseline stale on its own — same treatment the metadata
		// fields get, and the next push records it.
		(baseline.Parameters == nil || sameParameters(*baseline.Parameters, row.Parameters))
}

// acknowledgedRemote seeds the map that becomes the baseline if this push stops
// part-way, with only what the folder may honestly claim: a remote file whose
// hash matches the recorded baseline (we already knew about it), or one whose
// content is exactly what the folder holds (a write of identical bytes takes
// nothing from anyone).
//
// Everything else is somebody's unacknowledged change. Recording that as the
// baseline DISARMS the drift guard, which is how a failed --force used to hand
// the retry a silent overwrite: seeded with the whole remote, a stop() wrote the
// colleague's bytes into the baseline, and the retry without --force then saw
// nothing to refuse.
//
// A push that CREATED the workflow is the exception, and a load-bearing one: the
// server seeds a starter file at the entrypoint inside the create transaction,
// so "just created" does not mean "empty" — and that seed is nobody else's
// content. A first push whose entrypoint PUT fails has to record it, or the
// retry reads the server's own starter as drift and demands --force for a
// workflow the CLI made seconds earlier.
func acknowledgedRemote(baseline *wfdir.InstanceState, remote, local map[string]string, created bool) map[string]string {
	out := make(map[string]string, len(remote))
	if created {
		for path, content := range remote {
			out[path] = content
		}
		return out
	}
	known := baseline.Hashes()
	for path, content := range remote {
		if hash, ok := known[path]; ok && hash == wfdir.HashString(content) {
			out[path] = content
			continue
		}
		if mine, ok := local[path]; ok && mine == content {
			out[path] = content
		}
	}
	return out
}

// acknowledgeIntendedWrite is that same rule seen from the other end — after a
// write whose outcome is unknown — and it is the ONE definition of it that both
// sync loops call.
//
// A reconciling read answers with the server's CURRENT content, which is not the
// question that was asked. Under --force those bytes are precisely the
// colleague's change this push was forcing past: recorded as acknowledged, they
// disarm the drift guard, and the next plain push overwrites that change in
// silence. So only what the write was TRYING to leave behind may ever be
// recorded, and whatever else the server answers with settles the opposite way —
// the write missed.
//
// Shared rather than written once per loop because the two reconcilers
// legitimately differ in their TRIGGER (see reconcileTimedOutPut) and must never
// differ in this.
//
// Reports whether the write landed.
func acknowledgeIntendedWrite(landed map[string]string, path, want, onServer string) bool {
	if want != onServer {
		return false
	}
	landed[path] = onServer
	return true
}

// describePatch names what a failed metadata patch was trying to change, so the
// error reads "update the entrypoint to ..." rather than "update".
func describePatch(patch api.WorkflowPatch) string {
	var parts []string
	if patch.Title != "" {
		parts = append(parts, "the title")
	}
	if patch.Entrypoint != "" {
		parts = append(parts, fmt.Sprintf("the entrypoint to %q", patch.Entrypoint))
	}
	if patch.Parameters != nil {
		parts = append(parts, fmt.Sprintf("the parameters to %s", describeParameters(*patch.Parameters)))
	}
	return strings.Join(parts, " and ")
}

// saveBaseline records a baseline on the partial-push path, where the push has
// already failed and a second failure must not replace the first in the report.
func saveBaseline(f *folder, source *api.Workflow, files map[string]string) {
	f.State.Set(f.Key, baselineFromLocal(source, files))
	if err := wfdir.SaveState(f.Root, f.State); err != nil {
		fmt.Fprintf(os.Stderr, "  Note: could not record what was pushed in the local baseline (%v).\n", err)
	}
}

// existingTarget is what the server already holds for a folder's binding: the
// row the binding names, plus the caller's own edit draft of it when that row
// is live and they have one.
//
// It exists so the READS a push needs — which row, which draft, which
// parameters — happen once, before the validate gate, and the WRITES
// (create, checkout) stay behind it.
type existingTarget struct {
	// Workflow is the row the binding names: nil for an unbound folder, and
	// itself a parentless draft when the workflow has never been published.
	Workflow *api.Workflow
	// Draft is the caller's own edit draft of a LIVE Workflow, nil when they
	// have none (or when Workflow is already a draft).
	Draft *api.Workflow
}

// Row returns the row a push would write to without creating anything, or nil
// when a create or a checkout is still needed.
func (e existingTarget) Row() *api.Workflow {
	if e.Draft != nil {
		return e.Draft
	}
	if e.Workflow != nil && e.Workflow.Lifecycle == api.LifecycleDraft {
		return e.Workflow
	}
	return nil
}

// Parameters are the target's PERSISTED parameters — what a save is checked
// against alongside the files, and what a validate that omitted them would miss
// (a stale parameters[N].optionsQuery naming a deleted table validates clean and
// then refuses the first file write).
//
// The draft's win when there is one: it is the row the push writes to. A folder
// with no row on this instance yet reports none, which is the honest answer.
func (e existingTarget) Parameters() []api.WorkflowParameter {
	if row := e.Row(); row != nil {
		return row.Parameters
	}
	if e.Workflow != nil {
		return e.Workflow.Parameters
	}
	return nil
}

// inspectTarget reads the binding's current state without changing anything.
//
// Every refusal a push makes about the row it was pointed at lives here, so
// they all happen before the first write — including before the workflow a
// first push would create.
func inspectTarget(ctx context.Context, client *api.Client, f *folder) (existingTarget, error) {
	if f.Binding.WorkflowID == "" {
		return existingTarget{}, nil
	}
	wf, err := client.GetWorkflow(ctx, f.Binding.WorkflowID)
	if err != nil {
		if api.StatusOf(err) == 404 {
			return existingTarget{}, fmt.Errorf("workflow %s no longer exists on %s (or you lost access to it) — clone the workflow you meant into a fresh folder, or remove the binding from %s to start a new one",
				f.Binding.WorkflowID, f.Resolved.URL, wfdir.ManifestPath(f.Root))
		}
		return existingTarget{}, err
	}
	if err := refuseUnclonable(wf); err != nil {
		return existingTarget{}, err
	}
	if wf.Lifecycle == api.LifecycleDraft {
		// A parentless draft (created by an earlier first push, never
		// published) IS the draft — asking for one of it is not a thing.
		return existingTarget{Workflow: wf}, nil
	}
	draft, err := client.GetWorkflowDraft(ctx, wf.ID)
	if err != nil {
		return existingTarget{}, fmt.Errorf("check for your draft of %s: %w", wf.ID, err)
	}
	return existingTarget{Workflow: wf, Draft: draft}, nil
}

// resolvePushTarget decides which row the files go to, creating the workflow on
// a first push and checking out a draft when there is none. Everything it could
// refuse has already been refused by inspectTarget; what is left is the two
// writes.
//
// Never auto-creates a REPLACEMENT: a binding that names a workflow which has
// been deleted, archived or turned into a proposal is reported as broken. The
// alternative — quietly creating a second workflow — leaves the old one's
// consumers pointing at a row nobody edits any more.
func resolvePushTarget(ctx context.Context, client *api.Client, f *folder, featureID string, existing existingTarget, result *pushResult) (*api.Workflow, error) {
	if existing.Workflow == nil {
		// A first push is the one path here that WRITES a binding, so this is
		// where the organization has to be authoritative rather than adopted
		// from an entry that does not exist yet.
		if _, err := f.bindNew(ctx); err != nil {
			return nil, err
		}
		created, err := client.CreateWorkflow(ctx, api.CreateWorkflowInput{
			FeatureID:  featureID,
			Title:      f.Manifest.Title,
			Entrypoint: f.Manifest.Entrypoint,
			// Declared at creation rather than left to the metadata patch below:
			// the patch only fires when the manifest and the row disagree, and a
			// row created without them would disagree — this just saves the round
			// trip. Nil for an unmanaged folder, which creates none either way.
			Parameters: f.Manifest.DeclaredParameters(),
		})
		if err != nil {
			return nil, fmt.Errorf("create workflow in feature %s: %w", featureID, err)
		}
		// Recorded IMMEDIATELY, before any file is written: the workflow now
		// exists, and a push that died before saving the manifest would leave
		// an orphan the next push could not find and would create again.
		f.Manifest.SetBinding(f.Key, wfdir.Binding{WorkflowID: created.ID, FeatureID: featureID})
		if err := wfdir.SaveManifest(f.Root, f.Manifest); err != nil {
			return nil, fmt.Errorf("record the new workflow %s in %s: %w",
				created.ID, wfdir.ManifestPath(f.Root), err)
		}
		result.Created = true
		result.WorkflowID = created.ID
		return created, nil
	}

	if row := existing.Row(); row != nil {
		return row, nil
	}
	draft, err := client.CheckoutWorkflow(ctx, existing.Workflow.ID)
	if err != nil {
		return nil, fmt.Errorf("check out a draft of %s: %w", existing.Workflow.ID, err)
	}
	return draft, nil
}

// driftVerdict is what the drift guard decided, and it carries two independent
// answers because two later steps need different halves of it.
type driftVerdict struct {
	// BaselineClean reports an EXISTING baseline that already described the
	// server exactly. Only that answer lets a push with nothing to write leave
	// the baseline alone: a forced push past real drift, or one with no
	// baseline at all, has a baseline to record even when it writes nothing.
	BaselineClean bool
	// Bypassed reports that this push is not to be held to the baseline —
	// --force, or the CLI's own half-finished first push. It is what
	// suppresses the server-side preconditions.
	//
	// --force sets it unconditionally, including when the baseline turned out
	// to be clean and nothing needed bypassing. The flag's meaning is
	// "overwrite the remote with what I have", and it does not become
	// conditional on what the drift check happened to see one request earlier:
	// a precondition armed on a clean baseline refuses precisely the mid-push
	// race --force exists to proceed past, with advice to run --force.
	Bypassed bool
}

// checkDrift refuses a push that would overwrite server-side changes made since
// the last sync.
//
// The comparison is REMOTE vs BASELINE, not remote vs local: the question is
// whether anything moved underneath us, and a file the author edited locally is
// exactly what a push is for. It covers the row's METADATA as well as its files
// — the title and the entrypoint are pushed just as blindly as the code, and a
// colleague renaming the workflow in the web builder used to be reverted
// without a word.
//
// It is a check at ONE INSTANT — the moment the file list was read — which is
// why the writes that follow carry their own per-file preconditions: those
// close the window between this comparison and the byte that overwrites
// something, which nothing local can see.
func checkDrift(f *folder, target *api.Workflow, remote map[string]string, force bool) (driftVerdict, error) {
	baseline := f.State.For(f.Key)
	remoteHashes := make(map[string]string, len(remote))
	for path, content := range remote {
		remoteHashes[path] = wfdir.HashString(content)
	}
	drift := wfdir.DiffHashes(remoteHashes, baseline.Hashes())
	meta := metadataDriftOf(baseline, target, metadataPatch(f.Manifest, target))
	if !drift.Dirty() && len(meta) == 0 {
		// Bypassed still tracks --force here, even though nothing needed
		// bypassing: it is what suppresses the per-file preconditions, and
		// --force means "overwrite the remote with what I have" whether or not
		// the baseline happened to be clean when this check ran. Reporting
		// false would arm preconditions in exactly the window the flag exists
		// to override — a remote that moved AFTER this comparison read it —
		// and answer it with a 409 whose advice is to run --force, which is
		// what the caller just did.
		return driftVerdict{BaselineClean: baseline != nil, Bypassed: force}, nil
	}
	if force {
		fmt.Fprintf(os.Stderr, "  Note: --force — overwriting %s that changed on the server since your last sync.\n",
			describeDrifted(drift, meta))
		return driftVerdict{Bypassed: true}, nil
	}
	if baseline == nil {
		// A first push that died between recording the binding and writing the
		// baseline leaves exactly this: bound, no baseline, and a row holding
		// nothing but the entrypoint file the CREATE seeded. Refusing it would
		// be refusing the CLI's own half-finished work with a message about
		// somebody else's — so it proceeds as the first push it is.
		//
		// Deliberately narrow: a parentless draft (never published, so nobody
		// else has ever had a reason to touch it) holding ONE file, at the
		// entrypoint. A colleague's folder from git names a live workflow, or a
		// draft with a file set someone actually built, and still refuses.
		if isUntouchedFirstPush(target, remote) {
			return driftVerdict{Bypassed: true}, nil
		}
		return driftVerdict{}, fmt.Errorf("this folder has no sync baseline for %s, and the workflow already has %d file(s) there — .ronja/ is local-only, so a copy cloned from git starts without one.\n  Clone the workflow into a fresh folder to get one, or push --force to overwrite the remote files with what you have here",
			f.Resolved.URL, len(remote))
	}
	return driftVerdict{}, fmt.Errorf("the draft changed on the server since your last sync — pushing would overwrite it:\n%s  Run `ronja wf status` to see the detail, or push --force to overwrite",
		driftSummary(drift, meta))
}

// filePreconditions is the compare-and-swap guard a push attaches to each file
// write: the sha256 the server is asserted to hold right now, checked INSIDE
// the write's own transaction and refused with a 409 when it does not match.
//
// The hashes are the local sync baseline's — what the last sync recorded the
// server as holding — which is the same thing checkDrift compares against. That
// is deliberate: the drift guard answers the question once, from a file listing
// read a few requests earlier, and these answer it again at the instant of each
// write. The window between them is small and entirely real, and it is exactly
// the one two agents editing one workflow fall into.
//
// Armed is FALSE in the two cases the drift guard was bypassed, and suppressing
// them is not a weakening — in both, a precondition built from the baseline
// could only ever produce a 409 for a difference the push was already told to
// proceed past:
//
//   - --force. Forcing means "overwrite the remote with what I have". Sending
//     the baseline's hashes would 409 in precisely the case --force exists to
//     override, which is a flag that no longer does anything. That holds
//     whether or not the drift check found anything: on a CLEAN baseline the
//     only difference a precondition could still catch is one that landed
//     after the check read the listing, which is the same race, arriving a
//     little later.
//   - a workflow this push CREATED. There is no baseline at all, and the create
//     SEEDS a starter file at the entrypoint inside its own transaction — so
//     "" (must-not-exist) for that path would 409 on a workflow the CLI made
//     seconds earlier. This is the same reason the drift guard itself is
//     skipped for a fresh create and for the crashed-first-push resume that
//     isUntouchedFirstPush recognises.
type filePreconditions struct {
	Armed  bool
	Hashes map[string]string
}

// ForWrite returns the baseSha256 a PUT should assert, or nil for none.
//
// A path the baseline has never seen asserts "" — the file must NOT exist yet.
// That is the right assertion for a write: the folder believes it is creating
// this file, and a row that already holds one at that path is a change nobody
// here has seen.
func (p filePreconditions) ForWrite(path string) *string {
	if !p.Armed {
		return nil
	}
	hash := p.Hashes[path]
	return &hash
}

// ForDelete returns the baseSha256 a DELETE should assert, or nil for none.
//
// Unlike ForWrite it sends NOTHING for a path the baseline does not know,
// rather than "": asserting that the file you are deleting does not exist is a
// contradiction, and a precondition that can only ever fail is worse than
// having none. The drift guard already refuses that state — a remote file the
// baseline never recorded is drift — so this is a guard against a contradiction
// rather than a hole anyone can reach.
func (p filePreconditions) ForDelete(path string) *string {
	if !p.Armed {
		return nil
	}
	hash, known := p.Hashes[path]
	if !known {
		return nil
	}
	return &hash
}

// noteFileConflict explains a 409 on a file write in the terms the author has
// to act in. It never retries and never escalates to --force on their behalf:
// the whole value of the refusal is that a human decides what happens to the
// change it just protected.
//
// A DELETE gets its own wording because its 409 has TWO causes the CLI cannot
// tell apart — the file's content moved, or somebody deleted it first — and
// only the first leaves anything on the server. "It was NOT deleted" covers
// both but reads as "the file is still there", which is a claim this code is in
// no position to make: separating the two means parsing the server's prose,
// which is the fragility this whole feature refuses. So the delete note reports
// what it actually knows — that this push did not do it — and leads with the
// remedy that covers the already-gone case for free, since the next push reads
// a listing without the file and does not try to delete it at all.
func noteFileConflict(verb, path string) {
	if verb == "deleted" {
		fmt.Fprintf(os.Stderr, "  Conflict: %s is not what your last sync recorded — somebody changed or removed it while this push was running, so this push did not delete it.\n", path)
		fmt.Fprintf(os.Stderr, "  Run `ronja wf push` again: if they deleted it too, the retry simply moves on. Otherwise `ronja wf status` shows what moved, and push --force overwrites.\n")
		return
	}
	fmt.Fprintf(os.Stderr, "  Conflict: %s changed on the server while this push was running — it was NOT %s.\n", path, verb)
	fmt.Fprintf(os.Stderr, "  Run `ronja wf status` to see what moved, clone the workflow into a fresh folder to re-apply your change on top of it, or push --force to overwrite.\n")
}

// metadataDrift is one metadata field that moved on the server since the last
// sync: the title/entrypoint analogue of a file changed there.
type metadataDrift struct {
	Field    string
	Baseline string
	Remote   string
}

// metadataDriftOf reports the metadata this push would overwrite.
//
// Same test as a file, in both halves: the remote has to differ from what the
// last sync recorded (something moved underneath us) AND from what the folder
// wants (we would overwrite it). The PATCH is the input rather than the manifest
// because the patch already answers the second half — it is empty for a field
// the push would leave alone, whether because the manifest agrees with the row
// or because the manifest has nothing to say.
//
// A baseline with no metadata recorded at all is a state file written before
// this existed. It reports NO drift rather than guessing: turning every folder
// that predates the guard into a refusal would be a strange way to start
// protecting them, and the first push to record a baseline arms it.
func metadataDriftOf(baseline *wfdir.InstanceState, target *api.Workflow, patch api.WorkflowPatch) []metadataDrift {
	if baseline == nil {
		return nil
	}
	var out []metadataDrift
	if patch.Title != "" && baseline.Title != "" && target.Title != baseline.Title {
		out = append(out, metadataDrift{Field: "title", Baseline: baseline.Title, Remote: target.Title})
	}
	if patch.Entrypoint != "" && baseline.Entrypoint != "" && target.Entrypoint != baseline.Entrypoint {
		out = append(out, metadataDrift{Field: "entrypoint", Baseline: baseline.Entrypoint, Remote: target.Entrypoint})
	}
	if patch.Parameters != nil && baseline.Parameters != nil && !sameParameters(target.Parameters, *baseline.Parameters) {
		out = append(out, metadataDrift{
			Field:    "parameters",
			Baseline: describeParameters(*baseline.Parameters),
			Remote:   describeParameters(target.Parameters),
		})
	}
	return out
}

// describeParameters renders a declaration for a drift line — the names, which
// is what identifies a parameter to the person reading the refusal. The full
// shape belongs in the manifest diff, not in a one-line summary.
func describeParameters(params []api.WorkflowParameter) string {
	if len(params) == 0 {
		return "none"
	}
	names := make([]string, 0, len(params))
	for _, p := range params {
		names = append(names, p.Name)
	}
	return strings.Join(names, ", ")
}

// isUntouchedFirstPush reports the row a crashed first push leaves behind: a
// never-published draft holding nothing but the entrypoint file the server
// seeded when it created the workflow.
func isUntouchedFirstPush(target *api.Workflow, remote map[string]string) bool {
	if target.Lifecycle != api.LifecycleDraft || target.ParentWorkflowID != "" {
		return false
	}
	if len(remote) != 1 {
		return false
	}
	_, seeded := remote[target.Entrypoint]
	return seeded
}

// driftSummary renders the per-file drift lines shared by the refusal message,
// with the metadata that moved in the same list — the author wants one answer to
// "what changed over there", not two.
func driftSummary(drift wfdir.Diff, meta []metadataDrift) string {
	out := ""
	for _, group := range []struct {
		label string
		paths []string
	}{
		{"added there", drift.Added},
		{"changed there", drift.Modified},
		{"removed there", drift.Deleted},
	} {
		for _, path := range group.paths {
			out += fmt.Sprintf("    %-14s %s\n", group.label, path)
		}
	}
	for _, m := range meta {
		out += fmt.Sprintf("    %-14s %s is now %q (was %q at your last sync)\n",
			"changed there", m.Field, m.Remote, m.Baseline)
	}
	return out
}

// describeDrifted names what a --force is about to overwrite, files and
// metadata in one breath.
func describeDrifted(drift wfdir.Diff, meta []metadataDrift) string {
	var parts []string
	if n := drift.Total(); n > 0 {
		parts = append(parts, fmt.Sprintf("%d file(s)", n))
	}
	for _, m := range meta {
		parts = append(parts, "the "+m.Field)
	}
	return joinAnd(parts)
}

// joinAnd renders a short list as "a", "a and b", or "a, b and c".
func joinAnd(parts []string) string {
	if len(parts) < 2 {
		return strings.Join(parts, "")
	}
	return strings.Join(parts[:len(parts)-1], ", ") + " and " + parts[len(parts)-1]
}

// putFiles writes the folder's files into the target row, entrypoint leading
// and the rest in path order so a push is reproducible request-for-request.
//
// Failure STOPS rather than continuing with the rest. A push is already
// non-atomic across files, and carrying on after a rejection would spread the
// damage while making the report harder to read; the caller reports what landed
// before the stop, and re-pushing heals.
//
// The two maps are different questions. `remote` is what the server held when
// this push started, and it DECIDES: a file whose content is already there is
// skipped. `landed` is what the baseline may claim, and it RECORDS: it arrives
// holding only what was already acknowledged and gains an entry per successful
// write, so it describes the folder's honest position whether the sync finishes
// or stops.
func putFiles(ctx context.Context, client *api.Client, targetID, entrypoint string, local, remote, landed map[string]string, pre filePreconditions, result *pushResult) ([]string, error) {
	var warnings []string

	order := []string{}
	if _, ok := local[entrypoint]; ok {
		order = append(order, entrypoint)
	}
	for _, path := range sortedPaths(local) {
		if path != entrypoint {
			order = append(order, path)
		}
	}
	for _, path := range order {
		if remoteContent, ok := remote[path]; ok && remoteContent == local[path] {
			result.Unchanged++
			continue
		}
		saved, err := client.PutWorkflowFile(ctx, targetID, path, local[path], pre.ForWrite(path))
		if err != nil {
			// A 409 is the precondition refusing: the file moved between the
			// listing this push compared against and this write. Nothing
			// landed, nothing is reconciled, and the author is told what to do
			// — never retried and never escalated to --force automatically.
			if api.StatusOf(err) == api.StatusConflict {
				result.Conflict = true
				noteFileConflict("overwritten", path)
				return warnings, fmt.Errorf("push %s: %w", path, err)
			}
			// A PUT that TIMED OUT may still have committed: the deadline was
			// ours, the transaction was the server's. A rejection is different —
			// the server considered the write and refused it, and the file
			// certainly kept its previous content.
			if api.IsTimeout(err) && reconcileTimedOutPut(ctx, client, targetID, path, local[path], landed) {
				// It landed after all, so the report has to say so — a report
				// that disagrees with the baseline written beside it is worse
				// than either of them being wrong on its own.
				result.Pushed = append(result.Pushed, path)
			}
			return warnings, fmt.Errorf("push %s: %w", path, err)
		}
		landed[path] = local[path]
		warnings = append(warnings, saved.Warnings...)
		result.Pushed = append(result.Pushed, path)
	}
	return warnings, nil
}

// reconcileTimedOutPut asks what the server actually holds for one file after a
// write whose outcome is unknown, and acknowledges it only when the answer is
// what the write meant to leave behind. `want` is that content.
//
// The trigger is a TIMEOUT and nothing else, which is deliberately narrower than
// the data-app loop's status split (writeOutcomeUncertain) rather than an
// oversight of it. rworkflow.UpsertFile does all of its work inside ONE
// transaction, so an ANSWERED failure of any status — 4xx or 5xx — rolled that
// transaction back and the file certainly kept its previous content; only a
// deadline of ours leaves the server's transaction unobserved. A data-app write
// commits and only then recompiles and uploads the bundle, out of transaction,
// which is why a 5xx there can come from a write that landed and a workflow's
// cannot. Widening this to match would mean asking after refusals that settled
// themselves — and asking is precisely how a colleague's content gets read back.
//
// One read, no retry loop: the question has a single answer and it is worth
// exactly one round trip. A read that ALSO fails changes nothing — the file
// keeps whatever the baseline already acknowledged, which is the conservative
// half of the same choice, and the next push either finds it clean or reports it
// as drift the author can look at.
//
// Reports whether the write is now known to have landed, so the push's report
// and the baseline written beside it describe the same set of files.
func reconcileTimedOutPut(ctx context.Context, client *api.Client, targetID, path, want string, landed map[string]string) bool {
	saved, err := client.GetWorkflowFile(ctx, targetID, path)
	if err != nil {
		return false
	}
	return acknowledgeIntendedWrite(landed, path, want, saved.Content)
}

// deleteFiles removes what the server holds and the folder does not.
//
// Runs LAST, after the metadata patch, so the row has already stopped naming a
// renamed-away entrypoint by the time its old file is deleted.
//
// Enumerated from `remote` — everything the server holds — rather than from the
// baseline-recording `landed`, which no longer describes the whole row: a forced
// push past a colleague's added file must still delete it, since that is what
// forcing the folder onto the row means.
func deleteFiles(ctx context.Context, client *api.Client, targetID string, local, remote, landed map[string]string, pre filePreconditions, result *pushResult) ([]string, error) {
	var warnings []string

	deletions := []string{}
	for path := range remote {
		if _, ok := local[path]; !ok {
			deletions = append(deletions, path)
		}
	}
	sort.Strings(deletions)
	for _, path := range deletions {
		deleted, err := client.DeleteWorkflowFile(ctx, targetID, path, pre.ForDelete(path))
		if err != nil {
			// Includes the server's refusal to delete the file the row still
			// names as its entrypoint, whose remedy (point "entrypoint" in
			// ronja.json at a file you are keeping) is its message to give.
			if api.StatusOf(err) == api.StatusConflict {
				result.Conflict = true
				noteFileConflict("deleted", path)
			}
			return warnings, fmt.Errorf("delete %s: %w", path, err)
		}
		delete(landed, path)
		warnings = append(warnings, deleted.Warnings...)
		result.Deleted = append(result.Deleted, path)
	}
	return warnings, nil
}

// contentByPath flattens a fetched file set for content comparison.
func contentByPath(files []api.WorkflowFile) map[string]string {
	out := make(map[string]string, len(files))
	for _, f := range files {
		out[f.Path] = f.Content
	}
	return out
}

// refusedByValidation reports the state the validate gate leaves behind when it
// stops a push: findings, an error, and no target ever resolved. Findings alone
// are not that state — a clean-with-warnings validate carries them too, and the
// push goes ahead.
func (r *pushResult) refusedByValidation() bool {
	return r.Error != "" && r.DraftID == "" && len(r.Findings) > 0
}

func printPushReport(r *pushResult) {
	out := os.Stdout
	if r.refusedByValidation() {
		printFindings(out, r.Findings)
		fmt.Fprintf(out, "\n  Nothing was pushed — %s\n", r.Error)
		return
	}

	if r.UpToDate {
		fmt.Fprintf(out, "  Up to date — the draft %s already holds this folder.\n", r.DraftID)
		printResourceURL(out, reportKeyWidth, r.URL)
		return
	}

	if r.Created {
		fmt.Fprintf(out, "  Created workflow %s\n", r.WorkflowID)
	}
	for _, path := range r.Pushed {
		fmt.Fprintf(out, "    pushed   %s\n", path)
	}
	for _, path := range r.Deleted {
		fmt.Fprintf(out, "    deleted  %s\n", path)
	}
	if r.Unchanged > 0 {
		fmt.Fprintf(out, "    %d unchanged\n", r.Unchanged)
	}
	for _, w := range r.Warnings {
		fmt.Fprintf(out, "    warning  %s\n", w)
	}

	if r.Error != "" {
		fmt.Fprintf(out, "\n  Push stopped part-way: %s\n", r.Error)
		fmt.Fprintf(out, "  The draft %s holds what is listed above; fix the problem and push again.\n", r.DraftID)
		return
	}
	fmt.Fprintf(out, "\n  Draft:    %s\n", r.DraftID)
	if r.Target != "" {
		fmt.Fprintf(out, "  Target:   %s\n", r.Target)
	}
	if r.Bindings != nil {
		fmt.Fprintf(out, "  Bindings: %s\n", describeBindings(*r.Bindings))
	}
	printResourceURL(out, reportKeyWidth, r.URL)
	fmt.Fprintf(out, "\n  Next: ronja wf publish\n")
}
