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
//
// "conflict" is the one value that rides on a NON-ZERO exit, and it is the
// whole reason the vocabulary was extended rather than left at two: without it
// the only machine-readable difference between "somebody committed first" and
// "you may not commit at all" is substring-matching the server's prose on
// stderr — the exact fragility HeadVersionID refuses to accept from the 409
// body. A caller that does not know the value still behaves correctly: the exit
// code is non-zero and `outcome` is simply not one of the two it recognises.
const (
	outcomePublished          = "published"
	outcomeSubmittedForReview = "submitted_for_review"
	outcomeConflict           = "conflict"
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
	var opts publishOptions
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

The commit is refused when somebody else has published a new version of the
workflow since your draft was created — nothing is written and your draft is
untouched. Review what they changed and re-apply your work on top of it, or
re-run with --overwrite-remote to commit over their version deliberately.

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
			result, err := runPublish(cmd.Context(), f, opts)
			// Emitted even on failure, exactly like `wf push`: a refusal that
			// carries a result is one the caller has to be able to READ, and
			// the conflict refusal is the whole reason. Returning only the
			// error left `--json` with empty stdout and nothing but stderr
			// prose to tell a conflict from a permission failure.
			if result != nil {
				if flagJSON {
					if emitErr := emitJSON(result); emitErr != nil {
						return emitErr
					}
				} else {
					printPublishReport(result)
				}
			}
			return err
		},
	}
	cmd.Flags().BoolVar(&opts.NoRequestReview, "no-request-review", false,
		"fail instead of submitting the draft for admin review")
	cmd.Flags().BoolVar(&opts.OverwriteRemote, "overwrite-remote", false,
		"commit even though someone published a new version since your draft was created, discarding their changes")
	return cmd
}

// publishOptions are the two ways a publish can be told to depart from its
// default, both of them refusals turned into actions.
type publishOptions struct {
	NoRequestReview bool
	// OverwriteRemote authorizes committing over a version of the parent that
	// landed after this draft was created — a deliberately unpleasant name for
	// a deliberately unpleasant thing. It resolves the current head and
	// confirms it explicitly; it never loops, so a version that lands while
	// this is running produces a fresh refusal rather than a second attempt.
	OverwriteRemote bool
}

type publishResult struct {
	Outcome    string `json:"outcome"`
	WorkflowID string `json:"workflowID"`
	DraftID    string `json:"draftID"`
	// Detail is a human sentence about what actually happened — which parent
	// was committed to, or why the review route was taken.
	Detail string `json:"detail,omitempty"`
	// Error is the server's refusal, verbatim, on a publish that landed
	// nothing. It is prose and must not be branched on — `outcome` is the
	// machine-readable half — but it is the only place the server's own
	// account of the conflict (which version, which files) survives into the
	// --json payload.
	Error string `json:"error,omitempty"`
	// Target names the instance and organization this landed in. See
	// describeTarget.
	Target string `json:"target,omitempty"`
	// OverwroteVersionID names the committed version this publish deliberately
	// wrote over, and is set ONLY on the --overwrite-remote path. Absent on
	// every ordinary publish, which is what makes its presence meaningful to
	// anything scripting the CLI: it is the record that somebody else's work
	// was discarded, and by which version id.
	OverwroteVersionID string `json:"overwroteVersionID,omitempty"`
	// URL is the frontend page for the now-live workflow, as the SERVER
	// reported it, and is rendered by the human report only — see pushResult.URL
	// for why it stays out of the --json payload.
	//
	// Set on a PUBLISH and not on a review request, matching `app publish`:
	// a draft submitted for review put nothing live, and the honest close there
	// is "an admin must approve it", not a link to what has not changed.
	URL string `json:"-"`
}

func runPublish(ctx context.Context, f *folder, opts publishOptions) (*publishResult, error) {
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
		// The draft's own page: publishing a parentless draft promotes that row
		// in place, so its id — and its link — survive the publish.
		result.URL = draft.URL
		noteBaselineRefresh(f.Kind, refreshBaselineFromLive(ctx, client, f, draft.ID))
		return result, nil
	}

	// The route is decided UP FRONT from the two facts that decide it
	// server-side — the parent's feature scope and the caller's role — rather
	// than by trying a commit and reading the rejection prose. Matching on an
	// error message is how a client silently starts doing the wrong thing the
	// day someone rewords it.
	if !opts.NoRequestReview && parent.FeatureScope == scopeOrganization {
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

	commitErr := client.CommitWorkflowDraft(ctx, draft.ID, "")
	if commitErr == nil {
		result.Outcome = outcomePublished
		result.Detail = fmt.Sprintf("committed to %q", parent.Title)
		// The PARENT's page, not the draft's: the draft is gone the moment it
		// commits, and what the reader wants to look at is what went live.
		result.URL = parent.URL
		noteBaselineRefresh(f.Kind, refreshBaselineFromLive(ctx, client, f, parent.ID))
		return result, nil
	}

	// A 409 is the optimistic-concurrency refusal — somebody published a new
	// version of the parent after this draft was seeded — and it is checked
	// FIRST so it can never fall into the needs-review branch below. That
	// branch's `!= 400` already excludes it, and this ordering is what keeps
	// that true if the condition is ever widened: turning "your draft is based
	// on stale code" into "an admin will review it" would file a review request
	// for a draft that still needs reconciling, and report a publish that never
	// happened as a successful one.
	if api.StatusOf(commitErr) == api.StatusConflict {
		return resolveCommitConflict(ctx, client, f, parent, draft, result, opts, commitErr)
	}

	// The race the up-front routing cannot close: the feature was shared, or
	// the caller's role changed, between the read above and the commit. A 400
	// on an org-scoped parent is the server's needs-review rejection.
	if opts.NoRequestReview || parent.FeatureScope != scopeOrganization || api.StatusOf(commitErr) != 400 {
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

// resolveCommitConflict handles the one refusal that is about somebody else's
// work rather than about permission: the parent moved after this draft was
// created, so committing would discard a version nobody here has seen.
//
// Nothing was written when this is reached — the CAS runs inside the commit's
// own transaction, under the parent's row lock — so the draft is fully intact
// either way, and the only question is whether the author wants to overwrite.
func resolveCommitConflict(ctx context.Context, client *api.Client, f *folder,
	parent, draft *api.Workflow, result *publishResult, opts publishOptions, commitErr error) (*publishResult, error) {

	// conflict finishes the result as the machine-readable refusal it is, and
	// hands back both halves: the caller prints the payload and then fails.
	conflict := func(err error) (*publishResult, error) {
		result.Outcome = outcomeConflict
		result.Error = err.Error()
		return result, err
	}

	if !opts.OverwriteRemote {
		// The server's own message names the current version and, when it can
		// tell, the files that changed — so it is passed through rather than
		// paraphrased into something less specific.
		fmt.Fprintf(os.Stderr, "  Conflict: %q has been published to since your draft was created — nothing was committed, and your draft is intact.\n", parent.Title)
		fmt.Fprintf(os.Stderr, "  %v\n", commitErr)
		fmt.Fprintf(os.Stderr, "  Re-apply your change on top of theirs (`ronja wf discard` then `ronja wf clone %s`), or re-run with --overwrite-remote to commit over their version.\n", parent.ID)
		return conflict(fmt.Errorf("commit %s: %w", draft.ID, commitErr))
	}

	// The head is READ, never scraped out of the 409's prose: the HTTP error
	// body is the flat app-wide {"error": "<message>"} shape with no structured
	// payload, and a regex over prose starts overwriting the wrong version the
	// day somebody rewords it.
	head, err := client.HeadVersionID(ctx, parent.ID)
	if err != nil {
		return nil, fmt.Errorf("the commit of %s was refused (%v), and reading the current version of %s in order to overwrite it failed too: %w",
			draft.ID, commitErr, parent.ID, err)
	}
	fmt.Fprintf(os.Stderr, "  --overwrite-remote: committing over version %s of %q, discarding what it changed.\n", head, parent.Title)

	if err := client.CommitWorkflowDraft(ctx, draft.ID, head); err != nil {
		// A SECOND 409 means a third commit landed between the read above and
		// this write. Deliberately not retried: an override authorizes
		// overwriting the version it was shown, not whatever happens to be
		// there by the time the request arrives — and a loop would authorize
		// every one of them.
		if api.StatusOf(err) == api.StatusConflict {
			// Still a conflict, and reported as one: a third commit landing
			// mid-override is the same "somebody got there first" the caller
			// has to be able to tell from a permission failure.
			return conflict(fmt.Errorf("commit %s: %s moved again while this was running — version %s is no longer the current one, so nothing was committed and your draft is intact; re-run to see where it is now: %w",
				draft.ID, parent.ID, head, err))
		}
		return nil, fmt.Errorf("commit %s over version %s: %w", draft.ID, head, err)
	}

	result.Outcome = outcomePublished
	result.OverwroteVersionID = head
	result.Detail = fmt.Sprintf("committed to %q, overwriting version %s that was published after your draft was created", parent.Title, head)
	result.URL = parent.URL
	noteBaselineRefresh(f.Kind, refreshBaselineFromLive(ctx, client, f, parent.ID))
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
	case outcomeConflict:
		// The actionable narration is already on stderr, where it was written
		// as it happened. This line exists so the report cannot say
		// "Published." about a publish that landed nothing.
		fmt.Fprintf(out, "  Not published — the workflow moved on since your draft was created.\n")
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
	printResourceURL(out, reportKeyWidth, r.URL)
}
