package commands

import (
	"context"
	"fmt"
	"os"

	"github.com/ronjatech/ronja-cli/internal/api"
	"github.com/ronjatech/ronja-cli/internal/wfdir"
	"github.com/spf13/cobra"
)

// A resource verb the CLI allows because a wedged or abandoned draft blocks a
// module's one draft slot. ⚠️ That one draft may be a colleague's, so the
// confirmation names the drafter.
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
			// GET resolves live modules and the caller's own draft or proposal,
			// so a 404 is a module that is gone or somebody else's in-flight row.
			// --delete-module still acts on it deliberately: the server gates
			// whether the caller may, and unbinding alone would strand the row.
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

	// An unpublished module IS the module, so discarding it removes the row;
	// that stays behind an explicit flag because `discard` reads as reverting.
	if moduleIsUnpublished(mod) {
		if !deleteModule {
			return nil, fmt.Errorf("module %s has never been published, so it is a %s with nothing behind it — discarding would remove the module itself. Re-run with --delete-module to do that; your local files are kept",
				mod.ID, api.DescribeLifecycle(mod.Lifecycle))
		}
		return deleteUnpublishedModule(ctx, client, f, mod.ID, mod.Lifecycle, yes)
	}
	// Refused, not ignored, so the flag never reads as "delete the live module
	// too".
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

	// keepAnchor, deliberately: the files are unchanged, so re-anchoring on a
	// live version a colleague published meanwhile would let a later checkout
	// overwrite that publish with passing preconditions. See reanchorOnLive.
	noteBaselineRefresh(f.Kind, refreshModuleBaselineFromLive(ctx, client, f, mod.ID, keepAnchor))
	return &moduleDiscardResult{Outcome: outcomeDiscarded, ModuleID: mod.ID, DraftID: draft.ID}, nil
}

// deleteUnpublishedModule discards a parentless draft or withdraws a proposal
// and returns the folder to its post-`init` state. Ordering is load-bearing: the
// row goes first and the binding only after, or a failed delete strands a module
// no local command can name.
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
		// Lifecycle unknown (the 404 path): a refusal here is about the row's
		// shape, not authority, so fall through to the proposal verb.
		if err = client.DiscardModuleDraft(ctx, moduleID); err != nil {
			if withdrawErr := client.WithdrawModuleProposal(ctx, moduleID); withdrawErr == nil {
				err = nil
			}
		}
	}
	if err != nil {
		return nil, fmt.Errorf("remove %s: %w", moduleID, err)
	}

	f.recordBinding(wfdir.Binding{FeatureID: f.Binding.FeatureID})
	if err := f.saveFolder(); err != nil {
		return nil, fmt.Errorf("module %s was REMOVED, but %s still names it: %w\n  Remove the moduleID by hand, or the next push will look for a module that is gone",
			moduleID, f.bindingFiles(), err)
	}
	// Only this binding's baseline goes: the folder may be bound to other
	// stacks whose rows still exist.
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
