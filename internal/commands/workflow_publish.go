package commands

import (
	"context"
	"fmt"
	"os"

	"github.com/ronjatech/ronja-cli/internal/api"
	"github.com/ronjatech/ronja-cli/internal/wfdir"
	"github.com/spf13/cobra"
)

// Publish outcomes. These are the --json `outcome` values, so they are a
// contract for anything scripting the CLI: "submitted_for_review" is a
// SUCCESSFUL publish attempt that landed nothing, and treating it as "published"
// is the mistake this vocabulary exists to prevent.
const (
	outcomePublished          = "published"
	outcomeSubmittedForReview = "submitted_for_review"
)

// scopeOrganization is the feature scope that makes a workflow shared, mirrored
// from rdb.FeatureScopeOrganization. Committing a draft onto an org-scoped
// parent is admin-only server-side.
const scopeOrganization = "organization"

// `ronja wf publish` takes your draft live — or, when you may not, asks an
// admin to.
//
// The two-outcome honesty is the point. A non-admin publishing a draft of a
// shared workflow cannot commit it, and reporting "published" when what
// actually happened was "an admin now has a review request" is the kind of lie
// people discover a week later.
func newWorkflowPublishCmd() *cobra.Command {
	var noRequestReview bool
	cmd := &cobra.Command{
		Use:   "publish",
		Short: "Publish your draft, or submit it for review",
		Long: `Publish your draft, or submit it for review.

What happens depends on the workflow:

  never published yet     the draft is published and becomes live
  your own workflow       the draft is committed onto it
  a shared workflow       an admin commits it, so the draft is submitted for
                          review and you are told so

--no-request-review turns the last case into an error instead of a review
request, which is what CI wants when a merge is expected to land directly.

Push first: publish sends what is already on the server, and warns when the
folder has local changes that are not in the draft.`,
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
			result, err := runPublish(cmd.Context(), f, noRequestReview)
			if err != nil {
				return err
			}
			if flagJSON {
				return emitJSON(result)
			}
			printPublishReport(result)
			return nil
		},
	}
	cmd.Flags().BoolVar(&noRequestReview, "no-request-review", false,
		"fail instead of submitting the draft for admin review")
	return cmd
}

type publishResult struct {
	Outcome    string `json:"outcome"`
	WorkflowID string `json:"workflowID"`
	DraftID    string `json:"draftID"`
	// Detail is a human sentence about what actually happened — which parent
	// was committed to, or why the review route was taken.
	Detail string `json:"detail,omitempty"`
	// Target names the instance and organization this landed in. See
	// describeTarget.
	Target string `json:"target,omitempty"`
}

func runPublish(ctx context.Context, f *folder, noRequestReview bool) (*publishResult, error) {
	client := api.New(f.Resolved.URL, f.Resolved.Token)
	parent, draft, err := resolveDraft(ctx, client, f, "nothing to publish")
	if err != nil {
		return nil, err
	}

	// A dirty folder is a WARNING, not a refusal: publishing the draft as it
	// stands is a perfectly reasonable thing to want (you may have edited
	// locally since, or be publishing something a colleague pushed to your
	// draft from the web builder). But it is also the one way to publish
	// something other than what you are looking at, so it is said out loud.
	if err := warnIfDirty(f); err != nil {
		return nil, err
	}

	result := &publishResult{
		WorkflowID: f.Binding.WorkflowID,
		DraftID:    draft.ID,
		Target:     describeTarget(f.Resolved),
	}

	// A parentless draft has nothing to commit ONTO — publishing it is what
	// makes the workflow exist for everyone else.
	if parent == nil {
		if err := client.PublishWorkflowDraft(ctx, draft.ID); err != nil {
			return nil, fmt.Errorf("publish %s: %w", draft.ID, err)
		}
		result.Outcome = outcomePublished
		result.Detail = "the workflow is now live"
		noteBaselineRefresh(f.Kind, refreshBaselineFromLive(ctx, client, f, draft.ID))
		return result, nil
	}

	// The route is decided UP FRONT from the two facts that decide it
	// server-side — the parent's feature scope and the caller's role — rather
	// than by trying a commit and reading the rejection prose. Matching on an
	// error message is how a client silently starts doing the wrong thing the
	// day someone rewords it.
	if !noRequestReview && parent.FeatureScope == scopeOrganization {
		admin, err := callerIsAdmin(ctx, client)
		if err != nil {
			// Fall through to the commit attempt: the 400 fallback below still
			// catches the needs-review case, so an unreadable role costs an
			// extra round trip rather than the command.
			fmt.Fprintf(os.Stderr, "  Note: could not read your role (%v) — trying to commit.\n", err)
		} else if !admin {
			if err := client.RequestWorkflowReview(ctx, draft.ID); err != nil {
				return nil, fmt.Errorf("submit %s for review: %w", draft.ID, err)
			}
			result.Outcome = outcomeSubmittedForReview
			result.Detail = fmt.Sprintf("%q is shared, so an admin commits changes to it", parent.Title)
			return result, nil
		}
	}

	commitErr := client.CommitWorkflowDraft(ctx, draft.ID)
	if commitErr == nil {
		result.Outcome = outcomePublished
		result.Detail = fmt.Sprintf("committed to %q", parent.Title)
		noteBaselineRefresh(f.Kind, refreshBaselineFromLive(ctx, client, f, parent.ID))
		return result, nil
	}

	// The race the up-front routing cannot close: the feature was shared, or
	// the caller's role changed, between the read above and the commit. A 400
	// on an org-scoped parent is the server's needs-review rejection.
	if noRequestReview || parent.FeatureScope != scopeOrganization || api.StatusOf(commitErr) != 400 {
		return nil, fmt.Errorf("commit %s: %w", draft.ID, commitErr)
	}
	fmt.Fprintf(os.Stderr, "  Note: the commit was refused (%v) — submitting the draft for review instead.\n", commitErr)
	if err := client.RequestWorkflowReview(ctx, draft.ID); err != nil {
		return nil, fmt.Errorf("commit %s was refused (%v), and submitting it for review failed too: %w",
			draft.ID, commitErr, err)
	}
	result.Outcome = outcomeSubmittedForReview
	result.Detail = fmt.Sprintf("the commit was refused (%v), so the draft was submitted for review", commitErr)
	return result, nil
}

// resolveDraft finds the draft a publish or discard acts on, returning the
// parent live row alongside it (nil for a parentless draft).
//
// noun prefixes the "there is nothing here" message so each command says its
// own thing about the same state.
func resolveDraft(ctx context.Context, client *api.Client, f *folder, noun string) (parent, draft *api.Workflow, err error) {
	if !f.Bound || f.Binding.WorkflowID == "" {
		return nil, nil, fmt.Errorf("%s — this folder has no workflow on %s yet; run `ronja wf push` first",
			noun, f.Resolved.URL)
	}
	wf, err := client.GetWorkflow(ctx, f.Binding.WorkflowID)
	if err != nil {
		if api.StatusOf(err) == 404 {
			return nil, nil, fmt.Errorf("workflow %s no longer exists on %s (or you lost access to it)",
				f.Binding.WorkflowID, f.Resolved.URL)
		}
		return nil, nil, err
	}
	if err := refuseUnclonable(wf); err != nil {
		return nil, nil, err
	}
	if wf.Lifecycle == api.LifecycleDraft {
		// The binding names a parentless draft: a workflow created by a first
		// push and never published. The row IS the draft.
		return nil, wf, nil
	}
	draft, err = client.GetWorkflowDraft(ctx, wf.ID)
	if err != nil {
		return nil, nil, fmt.Errorf("check for your draft of %s: %w", wf.ID, err)
	}
	if draft == nil {
		return nil, nil, fmt.Errorf("%s — you have no open draft of %s; run `ronja wf push` first",
			noun, wf.ID)
	}
	return wf, draft, nil
}

// callerIsAdmin reports whether the signed-in user may commit a draft onto a
// shared workflow. Privilege levels count DOWN (USR_ADMIN is 10), mirroring
// sherlock.RoleAllowed.
func callerIsAdmin(ctx context.Context, client *api.Client) (bool, error) {
	me, err := client.Me(ctx)
	if err != nil {
		return false, err
	}
	if me.Role == nil {
		return false, nil
	}
	const usrAdmin = 10
	return me.Role.PrivilegeLevel <= usrAdmin, nil
}

// warnIfDirty says so when the folder holds changes the draft does not.
func warnIfDirty(f *folder) error {
	enumeration, err := wfdir.Enumerate(f.Root, wfdir.WorkflowKind)
	if err != nil {
		return err
	}
	diff := wfdir.DiffHashes(enumeration.Files, f.State.For(f.Key).Hashes())
	if diff.Dirty() {
		fmt.Fprintf(os.Stderr,
			"  Warning: %d local file(s) differ from your last sync and are NOT in what is being published — run `ronja wf push` first if you meant to include them.\n",
			diff.Total())
	}
	return nil
}

// refreshBaselineFromLive re-reads the now-live workflow and resets the sync
// baseline to it, reporting what went wrong when it cannot.
//
// It reports rather than prints because the CALLER decides what a failure
// means, and for every caller so far it means "note it and carry on": the
// server-side change has already happened, and failing the command afterwards
// would report something that did happen as something that did not. Hence
// noteBaselineRefresh below — but the error is real, so a future caller that
// needs to act on it can.
func refreshBaselineFromLive(ctx context.Context, client *api.Client, f *folder, liveID string) error {
	live, err := client.GetWorkflow(ctx, liveID)
	if err != nil {
		return fmt.Errorf("re-read %s: %w", liveID, err)
	}
	files, err := client.ListWorkflowFiles(ctx, liveID)
	if err != nil {
		return fmt.Errorf("re-read the files of %s: %w", liveID, err)
	}
	// The same gate `wf clone` applies before it writes a byte, for the same
	// reason: a baseline is a promise that these paths are on disk, and a path
	// no local walk can ever produce (a dot-file, .ronja/…) is a promise the
	// next status reads as a LOCAL DELETION and the next push acts on by
	// deleting the file server-side. Refusing to record the baseline leaves the
	// PREVIOUS one in place — stale, which `wf status` shows and the next push
	// heals — rather than replacing it with one that is actively wrong.
	if err := wfdir.CheckLocalPaths(pathsOf(files), wfdir.WorkflowKind); err != nil {
		return fmt.Errorf("the files of %s cannot all be tracked locally: %w", liveID, err)
	}
	f.State.Set(f.Key, baselineFrom(live, files))
	if err := wfdir.SaveState(f.Root, f.State); err != nil {
		return fmt.Errorf("write the local baseline: %w", err)
	}
	return nil
}

// noteBaselineRefresh degrades a failed baseline refresh into a note.
//
// A stale baseline is recoverable — `status` shows the drift and the next push
// heals it — so it must not turn a completed publish or discard into a non-zero
// exit. The kind is threaded through so the command it names is the one the
// caller actually ran: a data-app failure that answers "run `ronja wf status`"
// sends them to a command that refuses their folder.
func noteBaselineRefresh(kind wfdir.Kind, err error) {
	if err == nil {
		return
	}
	fmt.Fprintf(os.Stderr, "  Note: baseline not refreshed: %v.\n", err)
	fmt.Fprintf(os.Stderr, "  `%s status` will show your folder as drifted until the next push heals it.\n",
		kindCommand(kind))
}

func printPublishReport(r *publishResult) {
	out := os.Stdout
	switch r.Outcome {
	case outcomeSubmittedForReview:
		fmt.Fprintf(out, "  Submitted for review — an admin must approve it.\n")
	default:
		fmt.Fprintf(out, "  Published.\n")
	}
	if r.Detail != "" {
		fmt.Fprintf(out, "  %s\n", r.Detail)
	}
	fmt.Fprintf(out, "\n  Workflow: %s\n", r.WorkflowID)
	fmt.Fprintf(out, "  Draft:    %s\n", r.DraftID)
	if r.Target != "" {
		fmt.Fprintf(out, "  Target:   %s\n", r.Target)
	}
}
