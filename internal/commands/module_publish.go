package commands

import (
	"context"
	"fmt"
	"os"

	"github.com/ronjatech/ronja-cli/internal/api"
	"github.com/ronjatech/ronja-cli/internal/wfdir"
	"github.com/spf13/cobra"
)

// `ronja module publish` takes the draft live — or, when the caller may not,
// asks an admin to.
//
// The two-outcome honesty is `wf publish`'s and matters more here: a module edit
// fans out to every workflow that imports it, so a shared module's commit is
// admin-only, and reporting "published" when what actually happened was "an
// admin now has a review request" is the kind of lie people discover a week
// later — after they have republished three consumers against a version that
// does not exist.
func newModulePublishCmd() *cobra.Command {
	var opts publishOptions
	cmd := &cobra.Command{
		Use:   "publish",
		Short: "Publish the draft, or submit it for review",
		Long: `Publish the draft, or submit it for review.

What happens depends on the module:

  never published yet     the draft is published and the module becomes live
  a private feature       the draft is committed onto the live module
  a shared feature        an admin commits it, so the draft is submitted for
                          review and you are told so

--no-request-review turns the last case into an error instead of a review
request, which is what CI wants when a merge is expected to land directly.

The commit is refused when somebody else published a new version of the module
since the draft was created — nothing is written and the draft is untouched.
Review what they changed and re-apply on top of it, or re-run with
--overwrite-remote to commit over their version deliberately.

A FIRST publish is refused for a different reason: another live module already
holds the package name. There is no earlier version of this module to overwrite,
so --overwrite-remote does not apply — rename it in ronja.json and push again.

⚠️ Publishing a module changes NOTHING for the workflows that import it. Each
one pins the module version its author last saved and tested with, and picks up
this new version only when it is itself republished. The report says how many
workflows that is.

Push first: publish sends what is already on the server, and warns when the
folder has local changes that are not in the draft.`,
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			resolved, err := resolveInstance()
			if err != nil {
				return err
			}
			f, err := openFolder(cmd.Context(), resolved, wfdir.ModuleKind)
			if err != nil {
				return err
			}
			result, err := runModulePublish(cmd.Context(), f, opts)
			// Emitted even on failure, exactly like `wf publish`: a refusal that
			// carries a result is one the caller has to be able to READ, and the
			// conflict refusal is the whole reason.
			if result != nil {
				if flagJSON {
					if emitErr := emitJSON(result); emitErr != nil {
						return emitErr
					}
				} else {
					printModulePublishReport(result)
				}
			}
			return err
		},
	}
	cmd.Flags().BoolVar(&opts.NoRequestReview, "no-request-review", false,
		"fail instead of submitting the draft for admin review")
	cmd.Flags().BoolVar(&opts.OverwriteRemote, "overwrite-remote", false,
		"commit even though someone published a new version since the draft was created, discarding their changes")
	return cmd
}

type modulePublishResult struct {
	Outcome  string `json:"outcome"`
	ModuleID string `json:"moduleID"`
	DraftID  string `json:"draftID"`
	// HeadVersionID is the module's committed head AFTER this publish — the
	// version a consumer workflow will pin the next time it is saved. Empty when
	// the server could not read it back (best-effort, on its side), which is
	// never a failure of the publish itself.
	HeadVersionID string `json:"headVersionID,omitempty"`
	// DependentWorkflowCount is how many live workflows import this module, as
	// the SERVER counted them. Reported rather than computed here: it is the
	// delete gate's own reverse lookup, tenant-scoped by RLS — the same set the
	// delete gate refuses on, which the caller may well not be able to
	// enumerate — so a client-side count would be a second, weaker answer to a
	// question the server already owns.
	//
	// 0 means "none, or the count could not be taken", and the report says
	// nothing at all for 0 either way, so the two collapse harmlessly. The
	// server caps it at 100, like every other reverse lookup.
	DependentWorkflowCount int `json:"dependentWorkflowCount"`
	// Detail is a human sentence about what actually happened.
	Detail string `json:"detail,omitempty"`
	// Error is the server's refusal, verbatim, on a publish that landed nothing.
	// It is prose and must not be branched on — `outcome` is the
	// machine-readable half.
	Error string `json:"error,omitempty"`
	// Target names the instance and organization this landed in.
	Target string `json:"target,omitempty"`
	// OverwroteVersionID names the committed version this publish deliberately
	// wrote over, and is set ONLY on the --overwrite-remote path. Absent on
	// every ordinary publish, which is what makes its presence meaningful: it is
	// the record that somebody else's work was discarded, and by which version.
	OverwroteVersionID string `json:"overwroteVersionID,omitempty"`
}

func runModulePublish(ctx context.Context, f *folder, opts publishOptions) (*modulePublishResult, error) {
	client := api.New(f.Resolved.URL, f.Resolved.Token)
	parent, draft, err := resolveModuleDraft(ctx, client, f, "nothing to publish")
	if err != nil {
		return nil, err
	}

	// A dirty folder is a WARNING, not a refusal: publishing the draft as it
	// stands is a perfectly reasonable thing to want. But it is also the one way
	// to publish something other than what you are looking at, so it is said out
	// loud. warnIfDirty is shared and reads f.Kind, so it enumerates as a module
	// folder and names `ronja module push` in its advice.
	if err := warnIfDirty(f); err != nil {
		return nil, err
	}

	result := &modulePublishResult{
		ModuleID: f.Binding.ModuleID,
		DraftID:  draft.ID,
		Target:   describeTarget(f.Resolved),
	}

	// A PROPOSAL is already filed. Creating a module in a shared feature mints a
	// proposed row, so `module push` put it in front of the admins the moment it
	// created it, and there is nothing left for a publish to send.
	//
	// Short-circuited HERE, ahead of the routing below, because BOTH ways out of
	// that routing refuse a proposed row and neither says anything useful about
	// it: request-review is the governance engine's draft transition and answers
	// "row is not a draft", and the commit path answers the same and then falls
	// into request-review for a second copy of it. So the create lane every
	// non-admin uses in a shared feature — the one lane the docs call ordinary —
	// ended in a 400 naming a lifecycle. Approval is an admin's own decision and
	// has no CLI verb, which is why this reports rather than acts.
	if draft.Lifecycle == api.LifecycleProposed {
		result.Outcome = outcomeSubmittedForReview
		result.Detail = fmt.Sprintf(
			"%q was filed for approval when it was created — %s is in a shared feature, so a new module there is a proposal. Nothing more to send; push again to change what the admin will see",
			draft.Name, draft.ID)
		return result, nil
	}

	// A shared feature's commit is admin-only, and the route is decided UP FRONT
	// from the two facts that decide it server-side — the feature scope and the
	// caller's role — rather than by trying a commit and reading the rejection
	// prose. Matching on an error message is how a client silently starts doing
	// the wrong thing the day someone rewords it.
	//
	// The scope is read off the row the commit acts ON: the parent for an edit,
	// the draft itself for a first publish. rmodule stamps featureScope on every
	// row it reads, so neither costs a round trip.
	scoped := parent
	if scoped == nil {
		scoped = draft
	}
	if !opts.NoRequestReview && scoped.FeatureScope == scopeOrganization {
		admin, err := callerIsAdmin(ctx, client)
		if err != nil {
			// Fall through to the commit attempt: the refusal below still catches
			// the needs-review case, so an unreadable role costs an extra round
			// trip rather than the command.
			fmt.Fprintf(os.Stderr, "  Note: could not read your role (%v) — trying to commit.\n", err)
		} else if !admin {
			if err := client.RequestModuleReview(ctx, draft.ID); err != nil {
				return nil, fmt.Errorf("submit %s for review: %w", draft.ID, err)
			}
			result.Outcome = outcomeSubmittedForReview
			result.Detail = fmt.Sprintf("%q is in a shared feature, so an admin commits changes to it — a module edit reaches every workflow that imports it", scoped.Name)
			return result, nil
		}
	}

	outcome, commitErr := client.CommitModuleDraft(ctx, draft.ID, "")
	if commitErr == nil {
		finishModulePublish(ctx, client, f, result, outcome, parent, draft, "")
		return result, nil
	}

	// A 409 is the optimistic-concurrency refusal — somebody published a new
	// version of the parent after this draft was seeded — and it is checked
	// FIRST so it can never fall into the needs-review branch below. That
	// branch's `!= 400` already excludes it, and this ordering is what keeps
	// that true if the condition is ever widened.
	if api.StatusOf(commitErr) == api.StatusConflict {
		return resolveModuleCommitConflict(ctx, client, f, parent, draft, result, opts, commitErr)
	}

	// The race the up-front routing cannot close: the feature became shared, or
	// the caller's role changed, between the read above and the commit.
	if opts.NoRequestReview || scoped.FeatureScope != scopeOrganization || api.StatusOf(commitErr) != 400 {
		return nil, fmt.Errorf("commit %s: %w", draft.ID, commitErr)
	}
	fmt.Fprintf(os.Stderr, "  Note: the commit was refused (%v) — submitting the draft for review instead.\n", commitErr)
	if err := client.RequestModuleReview(ctx, draft.ID); err != nil {
		return nil, fmt.Errorf("commit %s was refused (%v), and submitting it for review failed too: %w",
			draft.ID, commitErr, err)
	}
	result.Outcome = outcomeSubmittedForReview
	result.Detail = fmt.Sprintf("the commit was refused (%v), so the draft was submitted for review", commitErr)
	return result, nil
}

// finishModulePublish records what a successful commit did — into the result,
// into the local baseline, and into the committed anchor.
//
// The anchor comes STRAIGHT off the commit response rather than from a second
// read, which is the one place this loop is cheaper than the workflow's:
// rmodule.CommitOutcome carries headVersionID for exactly this purpose. It is
// best-effort server-side, so an empty value leaves the anchor where it was
// rather than clearing it — an anchor that is behind refuses a later
// baseline-less push, which is a nuisance, where one that is ahead would vouch
// for a version this folder never saw.
func finishModulePublish(ctx context.Context, client *api.Client, f *folder,
	result *modulePublishResult, outcome *api.ModuleCommitOutcome, parent, draft *api.Module, overwrote string) {

	result.Outcome = outcomePublished
	result.OverwroteVersionID = overwrote
	liveID := draft.ID
	if parent != nil {
		liveID = parent.ID
	}
	if outcome != nil {
		if outcome.ModuleID != "" {
			liveID = outcome.ModuleID
			result.ModuleID = outcome.ModuleID
		}
		result.HeadVersionID = outcome.HeadVersionID
		result.DependentWorkflowCount = outcome.DependentWorkflowCount
	}
	switch {
	case overwrote != "":
		result.Detail = fmt.Sprintf("committed to %q, overwriting version %s that was published after the draft was created", draft.Name, overwrote)
	case parent == nil:
		result.Detail = "the module is now live"
	default:
		result.Detail = fmt.Sprintf("committed to %q", parent.Name)
	}

	// The baseline now has to describe the LIVE row. keepAnchor, not
	// anchorOnLive: the anchor is written from the commit response below, which
	// is the same fact without the round trip, and letting reanchorOnLive read
	// it again would be a second answer to one question.
	noteBaselineRefresh(f.Kind, refreshModuleBaselineFromLive(ctx, client, f, liveID, keepAnchor))
	if result.HeadVersionID == "" || f.Stack == "" {
		return
	}
	f.setHeadVersion(result.HeadVersionID)
	if err := f.saveFolder(); err != nil {
		// Noted, never returned: the publish HAS happened by the time this runs,
		// and reporting it as failed would send the caller looking for a version
		// that is already live.
		fmt.Fprintf(os.Stderr, "  Note: could not record version %s in %s (%v) — a later push from a fresh checkout will ask for a baseline instead of vouching for it.\n",
			result.HeadVersionID, wfdir.LockPath(f.Root), err)
	}
}

// resolveModuleCommitConflict handles the refusals that are about somebody
// else's work rather than about permission. There are TWO, and which one it is
// depends entirely on whether this draft has a parent:
//
//	parent != nil   the parent moved after this draft was created, so
//	                committing would discard a version nobody here has seen
//	parent == nil   a FIRST publish whose package name is already taken by
//	                another live module — a different module, not this one
//
// Nothing was written when this is reached — both checks run inside the
// commit's own transaction, under the row lock — so the draft is fully intact
// either way, and the only question is what the author does next.
func resolveModuleCommitConflict(ctx context.Context, client *api.Client, f *folder,
	parent, draft *api.Module, result *modulePublishResult, opts publishOptions, commitErr error) (*modulePublishResult, error) {

	conflict := func(err error) (*modulePublishResult, error) {
		result.Outcome = outcomeConflict
		result.Error = err.Error()
		return result, err
	}

	// A FIRST publish can 409 too, and it is a different conflict entirely: a
	// parentless commit has no base version to have moved, so what refused it is
	// the live-name uniqueness index — somebody else's module already holds this
	// package name (rmodule's parentless branch, `a live module named %q already
	// exists`).
	//
	// There is therefore nothing to overwrite and no --overwrite-remote leg to
	// offer: an override confirms a specific committed version of THIS module,
	// and this module has none. Offering one would either read the head of a row
	// that does not exist or, worse, invite the author to think they can commit
	// over somebody else's module. The remedy is to rename, which is a manifest
	// edit and a push, so the server's own message is passed through and the
	// author is pointed at `name`.
	//
	// DEFENSIVE mirror of the server branch it describes: a parentless commit is
	// a PRIVATE-feature first publish (a shared create is a proposal, which the
	// caller returns on before reaching any commit), so the only 409 that lands
	// here is the live-name index. It is written as a branch rather than an
	// assumption because the alternative is offering --overwrite-remote for a
	// version that does not exist.
	if parent == nil {
		fmt.Fprintf(os.Stderr, "  Conflict: %q could not be published — nothing was committed, and the draft is intact.\n", draft.Name)
		fmt.Fprintf(os.Stderr, "  %v\n", commitErr)
		fmt.Fprintf(os.Stderr, "  This module has never been published, so there is no version of it to overwrite and --overwrite-remote does not apply: change \"name\" in %s and push again.\n",
			wfdir.ManifestPath(f.Root))
		result.Detail = fmt.Sprintf("the first publish of %q was refused — a module that has never been published has no earlier version, so this is not a version conflict and --overwrite-remote does not apply",
			draft.Name)
		return conflict(fmt.Errorf("commit %s: %w", draft.ID, commitErr))
	}

	if !opts.OverwriteRemote {
		// The server's own message names the current version and, when it can
		// tell, the files that changed — so it is passed through rather than
		// paraphrased into something less specific.
		fmt.Fprintf(os.Stderr, "  Conflict: %q has been published to since this draft was created — nothing was committed, and the draft is intact.\n", draft.Name)
		fmt.Fprintf(os.Stderr, "  %v\n", commitErr)
		fmt.Fprintf(os.Stderr, "  Re-apply on top of theirs (`ronja module discard` then `ronja module clone %s`), or re-run with --overwrite-remote to commit over their version.\n", parent.ID)
		result.Detail = fmt.Sprintf("%q has been published to since this draft was created", draft.Name)
		return conflict(fmt.Errorf("commit %s: %w", draft.ID, commitErr))
	}

	// The head is READ, never scraped out of the 409's prose: the HTTP error
	// body is the flat app-wide {"error": "<message>"} shape with no structured
	// payload, and a regex over prose starts overwriting the wrong version the
	// day somebody rewords it.
	head, err := client.ModuleHeadVersionID(ctx, parent.ID)
	if err != nil {
		return nil, fmt.Errorf("the commit of %s was refused (%v), and reading the current version of %s in order to overwrite it failed too: %w",
			draft.ID, commitErr, parent.ID, err)
	}
	fmt.Fprintf(os.Stderr, "  --overwrite-remote: committing over version %s of %q, discarding what it changed.\n", head, draft.Name)

	outcome, err := client.CommitModuleDraft(ctx, draft.ID, head)
	if err != nil {
		// A SECOND 409 means a third commit landed between the read above and
		// this write. Deliberately not retried: an override authorizes
		// overwriting the version it was shown, not whatever happens to be there
		// by the time the request arrives — and a loop would authorize every one.
		if api.StatusOf(err) == api.StatusConflict {
			return conflict(fmt.Errorf("commit %s: %s moved again while this was running — version %s is no longer the current one, so nothing was committed and the draft is intact; re-run to see where it is now: %w",
				draft.ID, parent.ID, head, err))
		}
		return nil, fmt.Errorf("commit %s over version %s: %w", draft.ID, head, err)
	}
	finishModulePublish(ctx, client, f, result, outcome, parent, draft, head)
	return result, nil
}

// resolveModuleDraft finds the draft a publish or discard acts on, returning the
// parent live row alongside it (nil for an unpublished module, whose row IS the
// draft).
//
// noun prefixes the "there is nothing here" message so each command says its own
// thing about the same state.
func resolveModuleDraft(ctx context.Context, client *api.Client, f *folder, noun string) (parent, draft *api.Module, err error) {
	if !f.Bound || f.Binding.ModuleID == "" {
		return nil, nil, fmt.Errorf("%s — this folder has no module on %s yet; run `ronja module push` first",
			noun, f.Resolved.URL)
	}
	mod, err := client.GetModule(ctx, f.Binding.ModuleID)
	if err != nil {
		if api.StatusOf(err) == 404 {
			// ⚠️ GET /module/:id resolves live modules AND the caller's own draft
			// or proposal, so this is a module that is gone, or somebody else's
			// in-flight row — which answers 404 rather than 403 so that it
			// discloses nothing. See inspectModuleTarget's identical note.
			//
			// Recovering the row anyway would mean reading it through a file
			// route, and a publish that guessed its way to a row nobody could read
			// is worse than one that says so.
			return nil, nil, fmt.Errorf("cannot read module %s on %s.\n  Either it no longer exists (or you lost access to it), or it is an unpublished draft or proposal belonging to somebody else — `ronja module status` says which",
				f.Binding.ModuleID, f.Resolved.URL)
		}
		return nil, nil, err
	}
	if err := refuseUnclonableModule(mod); err != nil {
		return nil, nil, err
	}
	if moduleIsUnpublished(mod) {
		// The binding names the module itself — a parentless draft or a proposal.
		// The row IS the draft, and publishing it is what makes the module exist
		// for everyone else.
		return nil, mod, nil
	}
	draft, err = client.GetModuleDraft(ctx, mod.ID)
	if err != nil {
		return nil, nil, fmt.Errorf("check for the open draft of %s: %w", mod.ID, err)
	}
	if draft == nil {
		return nil, nil, fmt.Errorf("%s — module %s has no open draft; run `ronja module push` first",
			noun, mod.ID)
	}
	return mod, draft, nil
}

func printModulePublishReport(r *modulePublishResult) {
	out := os.Stdout
	switch r.Outcome {
	case outcomeSubmittedForReview:
		fmt.Fprintf(out, "  Submitted for review — an admin must approve it.\n")
	case outcomeConflict:
		// The actionable narration is already on stderr, where it was written as
		// it happened, and the cause — a moved version, or a package name
		// somebody else's live module already holds — is in Detail below. This
		// line exists so the report cannot say "Published." about a publish that
		// landed nothing.
		fmt.Fprintf(out, "  Not published — nothing was committed, and the draft is intact.\n")
	default:
		fmt.Fprintf(out, "  Published.\n")
	}
	if r.Detail != "" {
		fmt.Fprintf(out, "  %s\n", r.Detail)
	}
	fmt.Fprintf(out, "\n  Module:   %s\n", r.ModuleID)
	fmt.Fprintf(out, "  Draft:    %s\n", r.DraftID)
	if r.HeadVersionID != "" {
		fmt.Fprintf(out, "  Version:  %s\n", r.HeadVersionID)
	}
	if r.Target != "" {
		fmt.Fprintf(out, "  Target:   %s\n", r.Target)
	}
	// The blast radius, and the rule that makes the number mean something.
	//
	// Only on a publish that actually LANDED: a review request put nothing live
	// and a conflict put nothing anywhere, so telling either reader that N
	// workflows pin "this module" would describe a version that does not exist.
	// And only for a positive count — 0 is both "none" and "the count failed",
	// and there is nothing to say about either.
	if r.Outcome == outcomePublished && r.DependentWorkflowCount > 0 {
		fmt.Fprintf(out, "\n  %d workflow(s) pin this module — each picks up the new version when it is next\n",
			r.DependentWorkflowCount)
		fmt.Fprintf(out, "  republished. Publishing a module changes nothing for a consumer on its own.\n")
	}
}
