package commands

import (
	"context"
	"fmt"
	"os"

	"github.com/ronjatech/ronja-cli/internal/api"
	"github.com/ronjatech/ronja-cli/internal/wfdir"
	"github.com/spf13/cobra"
)

// `ronja app discard` throws your draft away and points the baseline back at
// the live app. It acts only on the sync loop's own draft, never on anything
// live, which is why it fits the "no resource verbs" rule. A never-published
// app is a parentless draft, so discarding it is refused unless --delete-app.
func newDataAppDiscardCmd() *cobra.Command {
	var yes bool
	var deleteApp bool
	cmd := &cobra.Command{
		Use:   "discard",
		Short: "Throw away your draft and reset the baseline to live",
		Long: `Throw away your draft and reset the baseline to live.

Deletes YOUR draft of the data app server-side. The live app is not touched,
and neither are your local files — the folder keeps everything you have
written, so ` + "`ronja app status`" + ` will show it as changed against the live version
afterwards. Push again to open a fresh draft.

An app that has never been published is itself a draft, with no live version to
fall back to, so discarding it would delete the app. That is refused unless you
pass --delete-app — the case it exists for is a first push you want to abandon,
most often one that failed to compile. Your local files are kept either way, and
the folder is unbound so a later push creates a fresh app.

Asks for confirmation on a terminal; --yes is required without one.`,
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
			result, err := runAppDiscard(cmd.Context(), f, yes, deleteApp)
			if err != nil {
				return err
			}
			if flagJSON {
				return emitJSON(result)
			}
			printAppDiscardReport(result)
			return nil
		},
	}
	cmd.Flags().BoolVar(&yes, "yes", false,
		"confirm the discard without a prompt (required when there is no terminal)")
	cmd.Flags().BoolVar(&deleteApp, "delete-app", false,
		"delete an app that has never been published (there is no live version to keep)")
	return cmd
}

type appDiscardResult struct {
	Outcome   string `json:"outcome"`
	DataAppID string `json:"dataAppID"`
	DraftID   string `json:"draftID,omitempty"`
}

func runAppDiscard(ctx context.Context, f *folder, yes, deleteApp bool) (*appDiscardResult, error) {
	client := api.New(f.Resolved.URL, f.Resolved.Token)
	if !f.Bound || f.Binding.DataAppID == "" {
		return nil, fmt.Errorf("nothing to discard — this folder has no data app on %s yet", f.Resolved.URL)
	}
	target, err := inspectAppTarget(ctx, client, f)
	if err != nil {
		return nil, err
	}
	// ⚠️ Parentless is decided by the parent field, not lifecycle alone: a
	// binding naming an edit shadow also reads as a draft, and treating it as
	// never-published would delete someone's edits and unbind the live app.
	if target.App != nil && target.App.Lifecycle == api.LifecycleDraft && target.App.ParentDataAppID != "" {
		return nil, fmt.Errorf("%s names %s, which is a draft of data app %s rather than the app itself — point the binding at %s (that is the id a push records) and try again",
			wfdir.ManifestPath(f.Root), target.App.ID, target.App.ParentDataAppID, target.App.ParentDataAppID)
	}
	// A parentless draft is the app itself, so the only action is deleting it,
	// behind --delete-app because `discard` reads as reverting.
	if target.App != nil && target.App.Lifecycle == api.LifecycleDraft {
		if !deleteApp {
			return nil, fmt.Errorf("data app %s has never been published, so it is a draft with nothing behind it — discarding would delete the app itself. Re-run with --delete-app to do that; your local files are kept",
				target.App.ID)
		}
		return deleteUnpublishedApp(ctx, client, f, target.App, yes)
	}
	// Refused rather than ignored, so it never reads as "delete the live app".
	if deleteApp {
		return nil, fmt.Errorf("--delete-app only applies to an app that has never been published; %s is live, so there is a published version to keep. Drop --delete-app to discard just your draft",
			target.App.ID)
	}
	if target.Draft == nil {
		return &appDiscardResult{Outcome: outcomeNoDraft, DataAppID: target.App.ID}, nil
	}

	ok, err := confirm(fmt.Sprintf("Discard your draft %s of %q? Local files are kept.", target.Draft.ID, target.App.Name),
		"deletes your draft on the server", yes)
	if err != nil {
		return nil, err
	}
	if !ok {
		return nil, fmt.Errorf("cancelled — the draft was not discarded")
	}
	if err := client.DiscardDataAppDraft(ctx, target.Draft.ID); err != nil {
		return nil, fmt.Errorf("discard %s: %w", target.Draft.ID, err)
	}

	// The baseline must now describe the live row. A failure is noted, not
	// returned: the discard succeeded.
	noteBaselineRefresh(f.Kind, refreshAppBaselineFromLive(ctx, client, f, target.App.ID, keepAnchor))
	return &appDiscardResult{
		Outcome:   outcomeDiscarded,
		DataAppID: target.App.ID,
		DraftID:   target.Draft.ID,
	}, nil
}

// deleteUnpublishedApp removes a never-published app and returns the folder to
// the state `init` left it in. ⚠️ The server row goes first and the manifest is
// unbound only after: the other order can strand an app no local command can
// name.
func deleteUnpublishedApp(ctx context.Context, client *api.Client, f *folder, app *api.DataApp, yes bool) (*appDiscardResult, error) {
	ok, err := confirm(fmt.Sprintf("DELETE data app %s (%q)? It has never been published, so this removes the app itself — it goes to the trash, and is purged for good 30 days later. Local files are kept.", app.ID, app.Name),
		fmt.Sprintf("deletes data app %s on the server", app.ID), yes)
	if err != nil {
		return nil, err
	}
	if !ok {
		return nil, fmt.Errorf("cancelled — %s was not deleted", app.ID)
	}
	if err := client.DeleteDataApp(ctx, app.ID); err != nil {
		return nil, fmt.Errorf("delete %s: %w", app.ID, err)
	}

	// Keep the feature, drop the app, so the next push creates a fresh one.
	f.recordBinding(wfdir.Binding{FeatureID: f.Binding.FeatureID})
	if err := f.saveFolder(); err != nil {
		return nil, fmt.Errorf("data app %s was DELETED, but %s still names it: %w\n  Remove the dataAppID from that file by hand, or the next push will look for an app that is gone",
			app.ID, wfdir.ManifestPath(f.Root), err)
	}
	// Only this binding's baseline is cleared: the folder may be bound to other
	// stacks whose rows still exist. A save failure is noted, not returned.
	f.State.Clear(f.Key)
	noteBaselineRefresh(f.Kind, wfdir.SaveState(f.Root, f.State))
	return &appDiscardResult{Outcome: outcomeResourceDeleted, DataAppID: app.ID}, nil
}

func printAppDiscardReport(r *appDiscardResult) {
	out := os.Stdout
	if r.Outcome == outcomeResourceDeleted {
		fmt.Fprintf(out, "  Deleted data app %s — it had never been published.\n", r.DataAppID)
		fmt.Fprintf(out, "  It is in the trash and is purged for good 30 days from now, so it can\n")
		fmt.Fprintf(out, "  still be restored until then.\n")
		fmt.Fprintf(out, "  Your local files are untouched and the folder is unbound, so the next\n")
		fmt.Fprintf(out, "  `ronja app push` creates a fresh app in the same feature.\n")
		return
	}
	if r.Outcome == outcomeNoDraft {
		fmt.Fprintf(out, "  You have no open draft of %s — nothing to discard.\n", r.DataAppID)
		return
	}
	fmt.Fprintf(out, "  Discarded draft %s.\n", r.DraftID)
	fmt.Fprintf(out, "  The live data app %s is untouched, and so are your local files —\n", r.DataAppID)
	fmt.Fprintf(out, "  the baseline now points at the live version, so `ronja app status` will\n")
	fmt.Fprintf(out, "  show your folder as changed against it. Push again to open a fresh draft.\n")
}
