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
// the live app.
//
// The escape hatch, and the reason it belongs in a CLI whose rule is otherwise
// "no resource verbs": a draft wedged by a binding you cannot resolve (a table
// somebody deleted) rejects every write, and the only way forward is to drop it
// and fork a fresh one. It operates on the sync loop's own artifact, never on
// anything live.
//
// Since migration 000495 a data app CAN be its own draft: POST /dataapp creates
// a parentless draft, exactly like a workflow, so a first push that is never
// published leaves a row with nothing behind it. `discard` refuses that row by
// default and --delete-app removes it, which is the same shape `wf discard`
// needs.
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
	// The PARENT field, not the lifecycle alone, is what makes a draft
	// parentless. A binding that names an EDIT SHADOW — a draft forked from a
	// live app, which a hand-edited ronja.json can perfectly well hold — also
	// reads as lifecycle=draft, and treating it as never-published would delete
	// somebody's in-progress edits and unbind the folder from the live app,
	// while reporting that nothing had ever been published.
	if target.App != nil && target.App.Lifecycle == api.LifecycleDraft && target.App.ParentDataAppID != "" {
		return nil, fmt.Errorf("%s names %s, which is a draft of data app %s rather than the app itself — point the binding at %s (that is the id a push records) and try again",
			wfdir.ManifestPath(f.Root), target.App.ID, target.App.ParentDataAppID, target.App.ParentDataAppID)
	}
	// A parentless draft IS the app, so there is no "undo my edits" to perform
	// on it — the only available action is deleting the whole row. That stays
	// behind an explicit flag because `discard` reads as reverting, and deleting
	// an app is not a surprise anyone should meet at the command line. But it
	// has to be reachable: this is what an abandoned first push leaves, the push
	// that failed to compile is when you most want it gone, and a CLI whose
	// premise is that an agent needs nothing but a terminal cannot answer with
	// "use the web UI".
	//
	// Reported separately from the no-draft case below, which would otherwise
	// say "you have no open draft" about a row that is nothing but a draft.
	if target.App != nil && target.App.Lifecycle == api.LifecycleDraft {
		if !deleteApp {
			return nil, fmt.Errorf("data app %s has never been published, so it is a draft with nothing behind it — discarding would delete the app itself. Re-run with --delete-app to do that; your local files are kept",
				target.App.ID)
		}
		return deleteUnpublishedApp(ctx, client, f, target.App, yes)
	}
	// --delete-app is only meaningful on the branch above. Refusing it here
	// rather than ignoring it keeps the flag from reading as "delete the live
	// app too" on a folder where it would silently do nothing of the sort.
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

	// The baseline now has to describe the LIVE row: the draft it was taken from
	// no longer exists, and leaving it in place would make every local file read
	// as drift against a row that is gone. A failure here is noted, not returned
	// — the discard itself succeeded.
	noteBaselineRefresh(f.Kind, refreshAppBaselineFromLive(ctx, client, f, target.App.ID, keepAnchor))
	return &appDiscardResult{
		Outcome:   outcomeDiscarded,
		DataAppID: target.App.ID,
		DraftID:   target.Draft.ID,
	}, nil
}

// deleteUnpublishedApp removes a parentless draft — an app that has never been
// published — and returns the folder to the state `init` left it in.
//
// Ordering is load-bearing: the server row goes first, and the manifest is only
// unbound once it is gone. Clearing the binding first and then failing the
// delete would strand a real app that no local command could name again, which
// is the exact failure this command exists to clean up.
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

	// Keep the feature, drop the app: the folder should behave exactly as it did
	// after `init`, so the next push creates a fresh app in the same place.
	f.recordBinding(wfdir.Binding{FeatureID: f.Binding.FeatureID})
	if err := f.saveFolder(); err != nil {
		return nil, fmt.Errorf("data app %s was DELETED, but %s still names it: %w\n  Remove the dataAppID from that file by hand, or the next push will look for an app that is gone",
			app.ID, wfdir.ManifestPath(f.Root), err)
	}
	// No baseline for THIS instance, for the same reason `init` leaves none:
	// there is no server-side row left to have synced from, so every local file
	// correctly reads as "added" until a push writes a real one. Only this
	// binding's entry goes — the folder may be bound to staging and production
	// too, and each of those still has a row its baseline describes. Noted
	// rather than returned — the delete and the unbind both succeeded.
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
