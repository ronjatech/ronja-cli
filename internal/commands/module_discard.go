package commands

import (
	"context"
	"fmt"
	"os"

	"github.com/ronjatech/ronja-cli/internal/api"
	"github.com/ronjatech/ronja-cli/internal/wfdir"
	"github.com/spf13/cobra"
)

// `ronja module discard` throws the draft away and points the baseline back at
// the live module.
//
// The escape hatch, and the reason it belongs in a CLI whose rule is otherwise
// "no resource verbs": a draft nobody can commit — wedged, or simply abandoned
// — blocks the ONE draft slot a module has, so until it is gone nobody can
// check one out. It operates on the sync loop's own artifact, never on anything
// live.
//
// ⚠️ A module has ONE draft, not one per author, so this may throw away a
// COLLEAGUE'S work. The confirmation names the drafter for exactly that reason,
// and --yes is what a caller with no terminal has to type to mean it.
func newModuleDiscardCmd() *cobra.Command {
	var yes bool
	var deleteModule bool
	cmd := &cobra.Command{
		Use:   "discard",
		Short: "Throw away the module's draft and reset the baseline to live",
		Long: `Throw away the module's draft and reset the baseline to live.

Deletes the module's draft server-side. The live module is not touched, and
neither are your local files — the folder keeps everything you have written, so
` + "`ronja module status`" + ` will show it as changed against the live version
afterwards. Push again to open a fresh draft.

⚠️ A module has ONE draft, not one per author, so this can discard a colleague's
work in progress. The confirmation says whose it is.

Asks for confirmation on a terminal; --yes is required without one.

A module that has never been published is itself a draft (or, in a shared
feature, a proposal), with no live version to fall back to — so discarding it
would remove the module. That is refused unless you pass --delete-module. Your
local files are kept either way, and the folder is unbound so a later push
creates a fresh module.`,
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
			result, err := runModuleDiscard(cmd.Context(), f, yes, deleteModule)
			if err != nil {
				return err
			}
			if flagJSON {
				return emitJSON(result)
			}
			printModuleDiscardReport(result)
			return nil
		},
	}
	cmd.Flags().BoolVar(&yes, "yes", false,
		"confirm the discard without a prompt (required when there is no terminal)")
	cmd.Flags().BoolVar(&deleteModule, "delete-module", false,
		"delete a module that has never been published (there is no live version to keep)")
	return cmd
}

type moduleDiscardResult struct {
	Outcome  string `json:"outcome"`
	ModuleID string `json:"moduleID"`
	DraftID  string `json:"draftID,omitempty"`
}

func runModuleDiscard(ctx context.Context, f *folder, yes, deleteModule bool) (*moduleDiscardResult, error) {
	client := api.New(f.Resolved.URL, f.Resolved.Token)
	if !f.Bound || f.Binding.ModuleID == "" {
		return nil, fmt.Errorf("nothing to discard — this folder has no module on %s yet", f.Resolved.URL)
	}
	mod, err := client.GetModule(ctx, f.Binding.ModuleID)
	if err != nil {
		if api.StatusOf(err) == 404 {
			// GET /module/:id resolves live modules AND the caller's own draft or
			// proposal, so the ORDINARY unpublished case now falls through to the
			// resolved branch below (moduleIsUnpublished) rather than arriving
			// here. What is left is a module that is gone, or somebody else's
			// in-flight row.
			//
			// --delete-module still acts on it, and that is deliberate rather
			// than leftover: the folder holds the id, the withdraw/discard verbs
			// take it, and the server is the gate on whether the caller may. An
			// unbind-only fallback would leave a row nobody here can name.
			if deleteModule {
				return deleteUnpublishedModule(ctx, client, f, f.Binding.ModuleID, "", yes)
			}
			return nil, fmt.Errorf("cannot read module %s on %s.\n  Either it no longer exists (or you lost access to it), or it is an unpublished draft or proposal belonging to somebody else.\n  If it is yours and was never published, `ronja module discard --delete-module` removes it and unbinds the folder",
				f.Binding.ModuleID, f.Resolved.URL)
		}
		return nil, err
	}
	if err := refuseUnclonableModule(mod); err != nil {
		return nil, err
	}

	// An unpublished module IS the module, so there is no "undo my edits" to
	// perform on it — the only available action is removing the whole row. That
	// stays behind an explicit flag because `discard` reads as reverting, and
	// removing a module is not a surprise anyone should meet at the command line.
	// But it has to be reachable: this is what an abandoned first push leaves,
	// and a CLI whose premise is that an agent needs nothing but a terminal
	// cannot answer with "use the web UI".
	if moduleIsUnpublished(mod) {
		if !deleteModule {
			return nil, fmt.Errorf("module %s has never been published, so it is a %s with nothing behind it — discarding would remove the module itself. Re-run with --delete-module to do that; your local files are kept",
				mod.ID, api.DescribeLifecycle(mod.Lifecycle))
		}
		return deleteUnpublishedModule(ctx, client, f, mod.ID, mod.Lifecycle, yes)
	}
	// --delete-module is only meaningful on the branch above. Refusing it here
	// rather than ignoring it keeps the flag from reading as "delete the live
	// module too" on a folder where it would do nothing of the sort.
	if deleteModule {
		return nil, fmt.Errorf("--delete-module only applies to a module that has never been published; %s is live, so there is a published version to keep. Drop --delete-module to discard just the draft",
			mod.ID)
	}

	draft, err := client.GetModuleDraft(ctx, mod.ID)
	if err != nil {
		return nil, fmt.Errorf("check for the open draft of %s: %w", mod.ID, err)
	}
	if draft == nil {
		return &moduleDiscardResult{Outcome: outcomeNoDraft, ModuleID: mod.ID}, nil
	}

	// The drafter is NAMED, because a module has one draft and it may not be
	// yours. "Discard your draft" would be a false statement half the time, and
	// the half where it is false is the half that costs somebody their work.
	whose := "the draft"
	if draft.DrafterUserID != "" {
		whose = fmt.Sprintf("the draft opened by %s", draft.DrafterUserID)
	}
	ok, err := confirm(
		fmt.Sprintf("Discard %s (%s) of module %q? A module has ONE draft, so this may not be yours. Local files are kept.",
			whose, draft.ID, mod.Name),
		"deletes the module's draft on the server", yes)
	if err != nil {
		return nil, err
	}
	if !ok {
		return nil, fmt.Errorf("cancelled — the draft was not discarded")
	}
	if err := client.DiscardModuleDraft(ctx, draft.ID); err != nil {
		return nil, fmt.Errorf("discard %s: %w", draft.ID, err)
	}

	// The baseline now has to describe the LIVE row: the draft it was taken from
	// no longer exists, and leaving it in place would make every local file read
	// as drift against a row that is gone.
	//
	// keepAnchor, deliberately: a discard leaves the folder's files exactly as
	// they are, so if a colleague published while that draft was open,
	// re-anchoring on live would stamp a version whose content the committed
	// files do not contain — and the next fresh checkout would vouch on it and
	// overwrite their publish with preconditions that pass. See reanchorOnLive.
	noteBaselineRefresh(f.Kind, refreshModuleBaselineFromLive(ctx, client, f, mod.ID, keepAnchor))
	return &moduleDiscardResult{Outcome: outcomeDiscarded, ModuleID: mod.ID, DraftID: draft.ID}, nil
}

// deleteUnpublishedModule removes a module that has never been published and
// returns the folder to the state `init` left it in.
//
// TWO server-side shapes, one command: a parentless DRAFT (private feature) is
// discarded, a PROPOSAL (shared feature) is withdrawn. They are the same event
// from the folder's side — the module stops existing and the binding has to go —
// which is why one function owns both rather than the caller branching.
//
// A lifecycle we could not read (the 404 path, where the row is unpublished and
// GET refuses it) tries the discard first and falls back to the withdraw: both
// are idempotent-shaped refusals on the wrong row, and trying is cheaper than
// asking the caller which kind of feature their module is in.
//
// Ordering is load-bearing: the server row goes first, and the manifest is only
// unbound once it is gone. Clearing the binding first and then failing the
// delete would strand a real module that no local command could name again —
// and, because the package name is unique among live modules, would make every
// later push collide with a row nobody can reach.
func deleteUnpublishedModule(ctx context.Context, client *api.Client, f *folder, moduleID, lifecycle string, yes bool) (*moduleDiscardResult, error) {
	ok, err := confirm(fmt.Sprintf("DELETE module %s? It has never been published, so this removes the module itself. Local files are kept.", moduleID),
		fmt.Sprintf("removes module %s on the server", moduleID), yes)
	if err != nil {
		return nil, err
	}
	if !ok {
		return nil, fmt.Errorf("cancelled — %s was not deleted", moduleID)
	}

	switch lifecycle {
	case api.LifecycleProposed:
		err = client.WithdrawModuleProposal(ctx, moduleID)
	case api.LifecycleDraft:
		err = client.DiscardModuleDraft(ctx, moduleID)
	default:
		// Lifecycle unknown (the 404 path). Try the draft verb, then the
		// proposal one — the first refusal is about the row's shape, not about
		// authority, so falling through costs one request and answers the
		// question the CLI could not.
		if err = client.DiscardModuleDraft(ctx, moduleID); err != nil {
			if withdrawErr := client.WithdrawModuleProposal(ctx, moduleID); withdrawErr == nil {
				err = nil
			}
		}
	}
	if err != nil {
		return nil, fmt.Errorf("remove %s: %w", moduleID, err)
	}

	// Keep the feature, drop the module: the folder should behave exactly as it
	// did after `init`, so the next push creates a fresh one in the same place.
	f.recordBinding(wfdir.Binding{FeatureID: f.Binding.FeatureID})
	if err := f.saveFolder(); err != nil {
		return nil, fmt.Errorf("module %s was REMOVED, but %s still names it: %w\n  Remove the moduleID by hand, or the next push will look for a module that is gone",
			moduleID, f.bindingFiles(), err)
	}
	// No baseline for THIS instance, for the same reason `init` leaves none:
	// there is no server-side row left to have synced from, so every local file
	// correctly reads as "added" until a push writes a real one. Only this
	// binding's entry goes — the folder may be bound to staging and production
	// too, and each of those still has a row its baseline describes.
	f.State.Clear(f.Key)
	noteBaselineRefresh(f.Kind, wfdir.SaveState(f.Root, f.State))
	return &moduleDiscardResult{Outcome: outcomeResourceDeleted, ModuleID: moduleID}, nil
}

func printModuleDiscardReport(r *moduleDiscardResult) {
	out := os.Stdout
	switch r.Outcome {
	case outcomeResourceDeleted:
		fmt.Fprintf(out, "  Removed module %s — it had never been published.\n", r.ModuleID)
		fmt.Fprintf(out, "  Your local files are untouched and the folder is unbound, so the next\n")
		fmt.Fprintf(out, "  `ronja module push` creates a fresh module in the same feature.\n")
	case outcomeNoDraft:
		fmt.Fprintf(out, "  Module %s has no open draft — nothing to discard.\n", r.ModuleID)
	default:
		fmt.Fprintf(out, "  Discarded draft %s.\n", r.DraftID)
		fmt.Fprintf(out, "  The live module %s is untouched, and so are your local files —\n", r.ModuleID)
		fmt.Fprintf(out, "  the baseline now points at the live version, so `ronja module status` will\n")
		fmt.Fprintf(out, "  show your folder as changed against it. Push again to open a fresh draft.\n")
	}
}
