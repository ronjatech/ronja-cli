package commands

import (
	"bufio"
	"context"
	"fmt"
	"os"
	"strings"

	"github.com/ronjatech/ronja-cli/internal/api"
	"github.com/ronjatech/ronja-cli/internal/wfdir"
	"github.com/spf13/cobra"
	"golang.org/x/term"
)

// Discard outcomes, the --json `outcome` vocabulary, kind-neutral because the
// result's own workflowID / dataAppID names the row.
const (
	outcomeDiscarded       = "discarded"
	outcomeNoDraft         = "no_draft"
	outcomeResourceDeleted = "resource_deleted"
)

// `ronja wf discard` is allowed despite "no resource verbs": a draft wedged by an
// unresolvable binding rejects every file write, and it touches only the sync
// loop's own artifact, never anything live.
func newWorkflowDiscardCmd() *cobra.Command {
	var yes bool
	var deleteWorkflow bool
	cmd := &cobra.Command{
		Use:   "discard",
		Short: "Throw away your draft and reset the baseline to live",
		Long: `Throw away your draft and reset the baseline to live.

Deletes YOUR draft of the workflow server-side. The live workflow is not
touched, and neither are your local files — the folder keeps everything you
have written, so ` + "`ronja wf status`" + ` will show it as changed against the live
version afterwards. Push again to open a fresh draft.

Asks for confirmation on a terminal; --yes is required without one.

A workflow that has never been published is itself a draft, with no live version
to fall back to, so discarding it would delete the workflow. That is refused
unless you pass --delete-workflow — the case it exists for is a first push you
want to abandon. Your local files are kept either way, and the folder is unbound
so a later push creates a fresh workflow.`,
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
			result, err := runDiscard(cmd.Context(), f, yes, deleteWorkflow)
			if err != nil {
				return err
			}
			if flagJSON {
				return emitJSON(result)
			}
			printDiscardReport(result)
			return nil
		},
	}
	cmd.Flags().BoolVar(&yes, "yes", false,
		"confirm the discard without a prompt (required when there is no terminal)")
	cmd.Flags().BoolVar(&deleteWorkflow, "delete-workflow", false,
		"delete a workflow that has never been published (there is no live version to keep)")
	return cmd
}

type discardResult struct {
	Outcome    string `json:"outcome"`
	WorkflowID string `json:"workflowID"`
	DraftID    string `json:"draftID,omitempty"`
}

func runDiscard(ctx context.Context, f *folder, yes, deleteWorkflow bool) (*discardResult, error) {
	client := api.New(f.Resolved.URL, f.Resolved.Token)
	if !f.Bound || f.Binding.WorkflowID == "" {
		return nil, fmt.Errorf("nothing to discard — this folder has no workflow on %s yet", f.Resolved.URL)
	}
	wf, err := client.GetWorkflow(ctx, f.Binding.WorkflowID)
	if err != nil {
		if api.StatusOf(err) == 404 {
			return nil, fmt.Errorf("workflow %s no longer exists on %s (or you lost access to it)",
				f.Binding.WorkflowID, f.Resolved.URL)
		}
		return nil, err
	}
	if err := refuseUnclonable(wf); err != nil {
		return nil, err
	}

	// The parent field, not lifecycle alone: an edit shadow of a live workflow also
	// reads as lifecycle=draft, and treating it as never-published would delete
	// in-progress edits and unbind the folder.
	if wf.Lifecycle == api.LifecycleDraft && wf.ParentWorkflowID != "" {
		return nil, fmt.Errorf("%s names %s, which is a draft of workflow %s rather than the workflow itself — point the binding at %s (that is the id a push records) and try again",
			wfdir.ManifestPath(f.Root), wf.ID, wf.ParentWorkflowID, wf.ParentWorkflowID)
	}
	// A parentless draft IS the workflow, so deleting it stays behind an explicit
	// flag, but reachable: an abandoned first push needs a terminal-only way out.
	if wf.Lifecycle == api.LifecycleDraft {
		if !deleteWorkflow {
			return nil, fmt.Errorf("workflow %s has never been published, so it is a draft with nothing behind it — discarding would delete the workflow itself. Re-run with --delete-workflow to do that; your local files are kept",
				wf.ID)
		}
		return deleteUnpublishedWorkflow(ctx, client, f, wf, yes)
	}
	// Refused, not ignored, so the flag never reads as "delete the live one too".
	if deleteWorkflow {
		return nil, fmt.Errorf("--delete-workflow only applies to a workflow that has never been published; %s is live, so there is a published version to keep. Drop --delete-workflow to discard just your draft",
			wf.ID)
	}

	draft, err := client.GetWorkflowDraft(ctx, wf.ID)
	if err != nil {
		return nil, fmt.Errorf("check for your draft of %s: %w", wf.ID, err)
	}
	if draft == nil {
		return &discardResult{Outcome: outcomeNoDraft, WorkflowID: wf.ID}, nil
	}

	ok, err := confirm(fmt.Sprintf("Discard your draft %s of %q? Local files are kept.", draft.ID, wf.Title),
		"deletes your draft on the server", yes)
	if err != nil {
		return nil, err
	}
	if !ok {
		return nil, fmt.Errorf("cancelled — the draft was not discarded")
	}
	if err := client.DiscardWorkflowDraft(ctx, draft.ID); err != nil {
		return nil, fmt.Errorf("discard %s: %w", draft.ID, err)
	}

	// The baseline must describe the live row now, or every file reads as drift
	// against a gone draft. A failure is noted, not returned: the discard succeeded.
	noteBaselineRefresh(f.Kind, refreshBaselineFromLive(ctx, client, f, wf.ID, keepAnchor))
	return &discardResult{Outcome: outcomeDiscarded, WorkflowID: wf.ID, DraftID: draft.ID}, nil
}

// deleteUnpublishedWorkflow deletes the server row before unbinding: the reverse
// order, on a failed delete, would strand a workflow no local command can name.
func deleteUnpublishedWorkflow(ctx context.Context, client *api.Client, f *folder, wf *api.Workflow, yes bool) (*discardResult, error) {
	ok, err := confirm(fmt.Sprintf("DELETE workflow %s (%q)? It has never been published, so this removes the workflow itself — it goes to the trash, and is purged for good 30 days later. Local files are kept.", wf.ID, wf.Title),
		fmt.Sprintf("deletes workflow %s on the server", wf.ID), yes)
	if err != nil {
		return nil, err
	}
	if !ok {
		return nil, fmt.Errorf("cancelled — %s was not deleted", wf.ID)
	}
	if err := client.DeleteWorkflow(ctx, wf.ID); err != nil {
		return nil, fmt.Errorf("delete %s: %w", wf.ID, err)
	}

	f.recordBinding(wfdir.Binding{FeatureID: f.Binding.FeatureID})
	if err := f.saveFolder(); err != nil {
		return nil, fmt.Errorf("workflow %s was DELETED, but %s still names it: %w\n  Remove the workflowID from that file by hand, or the next push will look for a workflow that is gone",
			wf.ID, wfdir.ManifestPath(f.Root), err)
	}
	// Only this instance's baseline goes, as after `init`: the folder may still be
	// bound elsewhere, each with a row its baseline describes.
	f.State.Clear(f.Key)
	noteBaselineRefresh(f.Kind, wfdir.SaveState(f.Root, f.State))
	return &discardResult{Outcome: outcomeResourceDeleted, WorkflowID: wf.ID}, nil
}

// confirm requires --yes without a terminal or under --json: a program handed a
// [y/N] prompt deadlocks. effect is a parameter because the same gate fronts
// both discarding a draft and deleting a whole workflow or data app.
func confirm(question, effect string, yes bool) (bool, error) {
	if yes {
		return true, nil
	}
	if flagJSON {
		return false, fmt.Errorf("this %s — pass --yes to confirm (--json is for programs, so there is nobody to prompt)", effect)
	}
	if !term.IsTerminal(int(os.Stdin.Fd())) {
		return false, fmt.Errorf("this %s — pass --yes to confirm (there is no terminal to ask on)", effect)
	}
	fmt.Fprintf(os.Stderr, "  %s [y/N] ", question)
	answer, err := bufio.NewReader(os.Stdin).ReadString('\n')
	if err != nil {
		return false, nil
	}
	switch strings.ToLower(strings.TrimSpace(answer)) {
	case "y", "yes":
		return true, nil
	default:
		return false, nil
	}
}

func printDiscardReport(r *discardResult) {
	out := os.Stdout
	if r.Outcome == outcomeResourceDeleted {
		fmt.Fprintf(out, "  Deleted workflow %s — it had never been published.\n", r.WorkflowID)
		fmt.Fprintf(out, "  It is in the trash and is purged for good 30 days from now, so it can\n")
		fmt.Fprintf(out, "  still be restored until then.\n")
		fmt.Fprintf(out, "  Your local files are untouched and the folder is unbound, so the next\n")
		fmt.Fprintf(out, "  `ronja wf push` creates a fresh workflow in the same feature.\n")
		return
	}
	if r.Outcome == outcomeNoDraft {
		fmt.Fprintf(out, "  You have no open draft of %s — nothing to discard.\n", r.WorkflowID)
		return
	}
	fmt.Fprintf(out, "  Discarded draft %s.\n", r.DraftID)
	fmt.Fprintf(out, "  The live workflow %s is untouched, and so are your local files —\n", r.WorkflowID)
	fmt.Fprintf(out, "  the baseline now points at the live version, so `ronja wf status` will\n")
	fmt.Fprintf(out, "  show your folder as changed against it. Push again to open a fresh draft.\n")
}
