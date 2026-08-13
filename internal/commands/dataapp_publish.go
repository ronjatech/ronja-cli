package commands

import (
	"context"
	"fmt"
	"os"

	"github.com/ronjatech/ronja-cli/internal/api"
	"github.com/ronjatech/ronja-cli/internal/wfdir"
	"github.com/spf13/cobra"
)

// `ronja app publish` takes your draft live — or, when you may not, asks an
// admin to.
//
// The two-outcome honesty is the point, and it is the same one `wf publish`
// makes: a non-admin publishing a draft of an app in a shared feature cannot
// commit it, and reporting "published" when what actually happened was "an admin
// now has a review request" is the kind of lie people discover a week later.
//
// What is different here is the compile gate. commitDraftInTx refuses a draft
// whose validated_at is NULL, because committing one would publish a bundle that
// is not its source — so publish CHECKS first and refuses locally with the
// diagnostics, rather than letting the server answer with an error about a
// column.
func newDataAppPublishCmd() *cobra.Command {
	var noRequestReview bool
	cmd := &cobra.Command{
		Use:   "publish",
		Short: "Publish your draft, or submit it for review",
		Long: `Publish your draft, or submit it for review.

What happens depends on the app's feature:

  your own feature        the draft is committed and goes live
  a shared feature        an admin commits it, so the draft is submitted for
                          review and you are told so
  a shared feature, and   an admin approves the app itself, so it is submitted
  never published yet     for approval and you are told so

--no-request-review turns the second case into an error instead of a review
request, which is what CI wants when a merge is expected to land directly.

The draft must compile. A data app that does not compile cannot be published —
the live app would otherwise keep serving its previous bundle while claiming to
be the new code — so publish checks first and shows you the diagnostics.

Push first: publish sends what is already on the server, and warns when the
folder has local changes that are not in the draft.`,
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			resolved, err := resolveInstance()
			if err != nil {
				return err
			}
			f, err := openFolder(cmd.Context(), resolved, wfdir.DataAppKind)
			if err != nil {
				return err
			}
			result, err := runAppPublish(cmd.Context(), f, noRequestReview)
			if err != nil {
				return err
			}
			if flagJSON {
				return emitJSON(result)
			}
			printAppPublishReport(result)
			return nil
		},
	}
	cmd.Flags().BoolVar(&noRequestReview, "no-request-review", false,
		"fail instead of submitting the draft for admin review")
	return cmd
}

type appPublishResult struct {
	Outcome   string `json:"outcome"`
	DataAppID string `json:"dataAppID"`
	DraftID   string `json:"draftID"`
	// Detail is a human sentence about what actually happened.
	Detail string `json:"detail,omitempty"`
	// AppURL is where to look at the result, on a publish that went live — as
	// the SERVER reported it on the row, never templated from the instance URL
	// (which is the API origin; see printResourceURL). Omitted when the instance
	// has no configured frontend origin, which is silent, not an error.
	//
	// ⚠️ That omission is a CHANGE to a shipped --json shape. The field used to
	// be derived locally from the instance URL, so it was ALWAYS present — and
	// always pointing at the API host, i.e. wrong. `ronja app publish --json |
	// jq -r .appURL` therefore goes from a wrong string to `null` on any
	// instance with no frontend origin configured, which is every dev box and
	// every self-hosted install that has not set one.
	AppURL string `json:"appURL,omitempty"`
	Target string `json:"target,omitempty"`
}

func runAppPublish(ctx context.Context, f *folder, noRequestReview bool) (*appPublishResult, error) {
	client := api.New(f.Resolved.URL, f.Resolved.Token)
	parent, draft, err := resolveAppDraft(ctx, client, f, "nothing to publish")
	if err != nil {
		return nil, err
	}

	// A dirty folder is a WARNING, not a refusal: publishing the draft as it
	// stands is a perfectly reasonable thing to want. But it is also the one way
	// to publish something other than what you are looking at.
	if err := warnIfAppDirty(f); err != nil {
		return nil, err
	}

	// The compile gate, checked BEFORE anything else happens.
	//
	// Asked of the server rather than read off the row we already have: a draft
	// can be stale-validated in exactly one direction that matters — the row says
	// validated, but a file has changed since — and this command's whole job is
	// to not publish source that does not compile. One request buys certainty.
	validated, verr := client.ValidateDataApp(ctx, draft.ID)
	switch {
	case verr == nil && validated.IsValidated():
		// Good to go.
	case verr != nil && api.StatusOf(verr) != 400:
		return nil, fmt.Errorf("check whether %s compiles: %w", draft.ID, verr)
	default:
		message := ""
		if verr != nil {
			message = compileMessageOf(verr, nil)
		}
		return nil, fmt.Errorf("the draft does not compile, so it cannot be published — the app would keep serving its previous version while claiming to be this one.%s\n  Run `ronja app validate` for the full diagnostics",
			formatCompileDetail(message))
	}

	result := &appPublishResult{
		DataAppID: f.Binding.DataAppID,
		DraftID:   draft.ID,
		Target:    describeTarget(f.Resolved),
	}

	// Whether this is an app's FIRST publish decides which review route even
	// exists. resolveAppDraft returns the same row as both parent and draft when
	// the app has never been published, so identity IS the test.
	//
	// The distinction is load-bearing rather than cosmetic: request-review covers
	// only a draft proposing a change to an existing live app and REFUSES a
	// parentless one, so pre-routing a first publish there is a dead end. The
	// server handles that case inside the commit — raising a proposal and
	// reporting `proposed` — which is why a first publish always commits and
	// reads the answer rather than deciding up front.
	firstPublish := parent.ID == draft.ID

	// For an EDIT draft the route is decided UP FRONT from the two facts that
	// decide it server-side — the feature's scope and the caller's role — rather
	// than by trying a commit and reading the rejection prose. Matching on an
	// error message is how a client silently starts doing the wrong thing the day
	// someone rewords it.
	shared, err := appFeatureIsShared(ctx, client, parent)
	if err != nil {
		fmt.Fprintf(os.Stderr, "  Note: could not read the feature's scope (%v) — trying to commit.\n", err)
	}
	if !noRequestReview && shared && !firstPublish {
		admin, err := callerIsAdmin(ctx, client)
		if err != nil {
			// Fall through to the commit attempt: the 400 fallback below still
			// catches the needs-review case, so an unreadable role costs an extra
			// round trip rather than the command.
			fmt.Fprintf(os.Stderr, "  Note: could not read your role (%v) — trying to commit.\n", err)
		} else if !admin {
			if err := client.RequestDataAppReview(ctx, draft.ID); err != nil {
				return nil, fmt.Errorf("submit %s for review: %w", draft.ID, err)
			}
			result.Outcome = outcomeSubmittedForReview
			result.Detail = fmt.Sprintf("%q is in a shared feature, so an admin commits changes to it", parent.Name)
			return result, nil
		}
	}

	outcome, commitErr := client.CommitDataAppDraft(ctx, draft.ID)
	if commitErr == nil {
		// Nothing went live: a first publish into a shared feature by a
		// non-admin was handed to an admin as a proposal. Saying "published"
		// here is exactly the lie this command's two-outcome shape exists to
		// prevent — and there is no baseline to refresh, since the row still
		// holds the draft content that is already on disk.
		if outcome != nil && outcome.Proposed {
			result.Outcome = outcomeSubmittedForReview
			result.Detail = fmt.Sprintf("a new app in the shared feature %q needs an admin's approval, so it was submitted for one", parent.Name)
			return result, nil
		}
		result.Outcome = outcomePublished
		result.Detail = fmt.Sprintf("committed to %q", parent.Name)
		// The PARENT's page — which is also the app's page for a first publish,
		// since a parentless draft is promoted in place and resolveAppDraft
		// returns that one row as both halves.
		result.AppURL = parent.URL
		noteBaselineRefresh(f.Kind, refreshAppBaselineFromLive(ctx, client, f, parent.ID))
		return result, nil
	}

	// The race the up-front routing cannot close: the feature was shared, or the
	// caller's role changed, between the read above and the commit. A first
	// publish is excluded because request-review cannot express it — its refusal
	// would replace the commit's real diagnostic with an unrelated one.
	if noRequestReview || firstPublish || api.StatusOf(commitErr) != 400 {
		return nil, fmt.Errorf("commit %s: %w", draft.ID, commitErr)
	}
	fmt.Fprintf(os.Stderr, "  Note: the commit was refused (%v) — submitting the draft for review instead.\n", commitErr)
	if err := client.RequestDataAppReview(ctx, draft.ID); err != nil {
		return nil, fmt.Errorf("commit %s was refused (%v), and submitting it for review failed too: %w",
			draft.ID, commitErr, err)
	}
	result.Outcome = outcomeSubmittedForReview
	result.Detail = fmt.Sprintf("the commit was refused (%v), so the draft was submitted for review", commitErr)
	return result, nil
}

// formatCompileDetail appends the compiler's own message when there is one.
func formatCompileDetail(message string) string {
	if message == "" {
		return ""
	}
	return "\n  " + message
}

// appFeatureIsShared reports whether the app's feature is organization-scoped,
// which is what makes committing admin-only.
//
// Unlike rdb.Workflow, rdb.DataApp carries no joined featureScope column, so
// this reads the feature. A failure is the caller's to interpret — publish
// degrades to attempting the commit.
func appFeatureIsShared(ctx context.Context, client *api.Client, app *api.DataApp) (bool, error) {
	if app == nil || app.FeatureID == "" {
		return false, nil
	}
	feature, err := client.GetFeature(ctx, app.FeatureID)
	if err != nil {
		return false, err
	}
	return feature.Scope == scopeOrganization, nil
}

// resolveAppDraft finds the draft a validate, publish or discard acts on,
// returning the app row alongside it.
//
// For an app that has never been published the two are THE SAME ROW — a
// parentless draft is the app. Returning it as both (where the workflow
// equivalent returns a nil parent and special-cases it) is what lets every
// caller keep one code path: the feature-scope check, the app's name and the
// `/apps/<id>` URL all read correctly off it, because it really is the app.
//
// noun prefixes the "there is nothing here" message so each command says its own
// thing about the same state.
func resolveAppDraft(ctx context.Context, client *api.Client, f *folder, noun string) (parent, draft *api.DataApp, err error) {
	if !f.Bound || f.Binding.DataAppID == "" {
		return nil, nil, fmt.Errorf("%s — this folder has no data app on %s yet; run `ronja app push` first",
			noun, f.Resolved.URL)
	}
	target, err := inspectAppTarget(ctx, client, f)
	if err != nil {
		return nil, nil, err
	}
	row := target.Row()
	if row == nil {
		return nil, nil, fmt.Errorf("%s — you have no open draft of %s; run `ronja app push` first",
			noun, target.App.ID)
	}
	if target.Draft == nil {
		// Never published: the row the binding names IS the draft.
		return row, row, nil
	}
	return target.App, target.Draft, nil
}

// refreshAppBaselineFromLive re-reads the now-live app and resets the sync
// baseline to it, reporting what went wrong when it cannot.
//
// It reports rather than prints because the CALLER decides what a failure means,
// and for every caller so far it means "note it and carry on": the server-side
// change has already happened, and failing the command afterwards would report
// something that did happen as something that did not.
func refreshAppBaselineFromLive(ctx context.Context, client *api.Client, f *folder, liveID string) error {
	live, err := client.GetDataApp(ctx, liveID)
	if err != nil {
		return fmt.Errorf("re-read %s: %w", liveID, err)
	}
	files, err := client.ListDataAppFiles(ctx, liveID)
	if err != nil {
		return fmt.Errorf("re-read the files of %s: %w", liveID, err)
	}
	// The same gate `app clone` applies before it writes a byte, for the same
	// reason: a baseline is a promise that these paths are on disk, and a path no
	// local walk can ever produce is a promise the next status reads as a LOCAL
	// DELETION and the next push acts on by deleting the file server-side.
	if err := wfdir.CheckLocalPaths(appPathsOf(files), wfdir.DataAppKind); err != nil {
		return fmt.Errorf("the files of %s cannot all be tracked locally: %w", liveID, err)
	}
	f.State.Set(f.Key, baselineFromApp(live, files))
	if err := wfdir.SaveState(f.Root, f.State); err != nil {
		return fmt.Errorf("write the local baseline: %w", err)
	}
	return nil
}

func printAppPublishReport(r *appPublishResult) {
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
	fmt.Fprintf(out, "\n  Data app: %s\n", r.DataAppID)
	fmt.Fprintf(out, "  Draft:    %s\n", r.DraftID)
	if r.Target != "" {
		fmt.Fprintf(out, "  Target:   %s\n", r.Target)
	}
	printResourceURL(out, reportKeyWidth, r.AppURL)
}
