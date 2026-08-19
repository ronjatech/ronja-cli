package commands

import (
	"context"
	"errors"
	"fmt"
	"os"
	"sort"

	"github.com/ronjatech/ronja-cli/internal/api"
	"github.com/ronjatech/ronja-cli/internal/wfdir"
	"github.com/spf13/cobra"
)

// `ronja pipeline discard` throws your staged drafts away.
//
// The escape hatch, and the reason it belongs in a CLI whose rule is otherwise
// "no resource verbs": a draft wedged by an input somebody deleted rejects every
// build, and the only way forward is to drop it and check out a fresh one. It
// operates on the sync loop's own artifact — never on a live table, which is
// left exactly as it was.
func newPipelineDiscardCmd() *cobra.Command {
	var yes bool
	cmd := &cobra.Command{
		Use:   "discard [paths...]",
		Short: "Throw away your staged drafts",
		Long: `Throw away your staged drafts.

Deletes YOUR draft of each table you have pushed. Name paths to discard only
those; with no arguments, every file with a staged draft.

The live tables are not touched, and neither are your local files — the folder
keeps everything you have written, so ` + "`ronja pipeline status`" + ` will show it as
changed against the live versions afterwards. Push again to open fresh drafts.

Asks for confirmation on a terminal; --yes is required without one.`,
		// Positional arguments are FILE PATHS, resolved against the working
		// directory and validated by resolveArgPaths — which refuses anything
		// outside the folder or outside the syncable set, by name. Declared
		// explicitly, like every sibling command, so the shape of the command is
		// visible where cobra reads it rather than only in the resolver.
		Args: cobra.ArbitraryArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			resolved, err := resolveInstance()
			if err != nil {
				return err
			}
			f, err := openFolder(cmd.Context(), resolved, wfdir.PipelineKind)
			if err != nil {
				return err
			}
			result, err := runPipelineDiscard(cmd.Context(), f, args, yes)
			// Emitted even on failure, exactly like push and publish: a discard
			// that stopped part-way has really deleted drafts, and "which ones"
			// is the first thing anyone needs to know.
			if result != nil {
				if flagJSON {
					if emitErr := emitJSON(result); emitErr != nil {
						return emitErr
					}
				} else {
					printPipelineDiscardReport(result)
				}
			}
			return err
		},
	}
	cmd.Flags().BoolVar(&yes, "yes", false,
		"confirm the discard without a prompt (required when there is no terminal)")
	return cmd
}

type pipelineDiscardResult struct {
	Target string                  `json:"target,omitempty"`
	Files  []pipelineDiscardedFile `json:"files"`
	// Nothing reports a folder with no staged draft at all — a different answer
	// from one whose drafts were all discarded.
	Nothing bool `json:"nothing"`
	// Error summarises a discard that did not fully succeed. The per-file
	// entries say what actually happened; this is the one-line version.
	Error string `json:"error,omitempty"`
}

type pipelineDiscardedFile struct {
	Path    string `json:"path"`
	TableID string `json:"tableID"`
	DraftID string `json:"draftID,omitempty"`
	// Outcome is outcomeDiscarded, outcomeNoDraft or pushOutcomeRefused — the
	// kind-neutral vocabulary the workflow and data-app loops already use.
	Outcome string `json:"outcome"`
	// Error is why this file's draft was not thrown away, for a refusal.
	Error string `json:"error,omitempty"`
}

func runPipelineDiscard(ctx context.Context, f *folder, args []string, yes bool) (*pipelineDiscardResult, error) {
	client := newClient(f.Resolved.URL, f.Resolved.Token)
	result := &pipelineDiscardResult{Target: describeTarget(f.Resolved), Files: []pipelineDiscardedFile{}}

	if !f.Bound || len(f.Binding.Tables) == 0 {
		return nil, fmt.Errorf("nothing to discard — this folder has no tables on %s yet", f.Resolved.URL)
	}
	inst := pipelineBaseline(f)

	var targets []string
	if len(args) > 0 {
		local, _, err := readPipelineFiles(f.Root)
		if err != nil {
			return nil, err
		}
		if targets, err = resolveArgPaths(f.Root, args, local); err != nil {
			return nil, err
		}
	} else {
		for path, state := range inst.Tables {
			if state.DraftID != "" {
				targets = append(targets, path)
			}
		}
		sort.Strings(targets)
	}
	if len(targets) == 0 {
		result.Nothing = true
		return result, nil
	}

	ok, err := confirm(
		fmt.Sprintf("Discard your draft of %d table(s) in %s? Local files are kept.", len(targets), f.Root),
		"deletes your drafts on the server", yes)
	if err != nil {
		return nil, err
	}
	if !ok {
		return nil, fmt.Errorf("cancelled — nothing was discarded")
	}

	failed := 0
	for _, path := range targets {
		// One Ctrl-C, one message. Without this every remaining draft makes its
		// own doomed request and prints its own refusal, turning a single
		// interrupt into a page of them.
		if ctx.Err() != nil {
			result.Error = interruptedMessage
			return result, errors.New(interruptedMessage)
		}
		file := discardOneTable(ctx, client, f, inst, path)
		result.Files = append(result.Files, file)
		if file.Outcome == pushOutcomeRefused {
			failed++
		}
		// Saved after EVERY file, success or failure. A discard is irreversible
		// server-side, so a baseline written only once at the end is lost
		// wholesale by the file after it — leaving drafts that are gone from the
		// server still pointed at, with their content baseline still describing
		// them, which is exactly the "Up to date with nothing staged anywhere"
		// state this command's report promises it has not left behind.
		if err := wfdir.SaveState(f.Root, f.State); err != nil {
			// A baseline that cannot be written is that same state, and it does
			// not get better by discarding more drafts into it — a read-only
			// .ronja/ or a full disk fails identically on every remaining file.
			// So it stops, says so, and exits non-zero.
			result.Error = fmt.Sprintf("the draft of %s was discarded, but the local baseline in %s could not be updated (%v) — the remaining drafts were left alone",
				path, wfdir.StatePath(f.Root), err)
			return result, fmt.Errorf("%s", result.Error)
		}
	}

	if failed > 0 {
		result.Error = fmt.Sprintf("%d of %d draft(s) were not discarded", failed, len(result.Files))
		return result, fmt.Errorf("%s", result.Error)
	}
	return result, nil
}

// discardOneTable throws away one file's draft and records the baseline that
// leaves behind.
//
// Failure is DATA, not an error return, on the same rule as pushOneTable's: a
// discard of twelve drafts that stopped dead on the third would leave nine
// perfectly discardable drafts staged for no reason, and the caller could not
// write a baseline for the two that DID land.
func discardOneTable(ctx context.Context, client *api.Client, f *folder,
	inst *wfdir.InstanceState, path string) pipelineDiscardedFile {

	out := pipelineDiscardedFile{Path: path, TableID: f.Binding.Tables[path]}
	refuse := func(format string, args ...any) pipelineDiscardedFile {
		out.Outcome = pushOutcomeRefused
		out.Error = fmt.Sprintf(format, args...)
		fmt.Fprintf(os.Stderr, "  Refused: %s — %s\n", path, out.Error)
		return out
	}
	if out.TableID == "" {
		out.Outcome = outcomeNoDraft
		// A file the manifest no longer binds, whose baseline can still point at a
		// draft of the table it used to. Nothing here can discard that draft —
		// there is no binding left to name it by — but leaving the pointer means
		// every later command goes on comparing against a row this folder cannot
		// even address.
		if recorded := inst.Tables[path]; recorded.DraftID != "" {
			recordDraftPointer(inst, path, recorded.TableID, "")
		}
		return out
	}
	// The recorded draft id is a HINT: a draft can be committed or discarded
	// from the web UI between two commands, so the server is asked.
	draft, err := client.GetTableDraft(ctx, out.TableID)
	if err != nil {
		return refuse("check for your draft of %s: %v", out.TableID, err)
	}
	if draft == nil {
		out.Outcome = outcomeNoDraft
		// The SAME baseline write the success branch makes, and for the same
		// reason: the draft is gone either way, and this is the DOCUMENTED normal
		// case (somebody discarded it from the web UI). Recording only the
		// pointer left inst.Files holding the vanished draft's bytes, so the
		// local file read as unchanged for ever and the next push answered "Up to
		// date" with no draft on the server at all.
		recordDiscarded(inst, path, out.TableID)
		return out
	}
	out.DraftID = draft.ID
	if err := client.DiscardTableDraft(ctx, draft.ID); err != nil {
		return refuse("discard %s: %v", draft.ID, err)
	}
	out.Outcome = outcomeDiscarded
	// The pointer goes, and the content baseline is re-pointed at the LIVE
	// table — the only row this file is synced with now.
	recordDiscarded(inst, path, out.TableID)
	return out
}

func printPipelineDiscardReport(r *pipelineDiscardResult) {
	out := os.Stdout
	if r.Nothing {
		fmt.Fprintf(out, "  Nothing to discard — no file in this folder has a staged draft.\n")
		return
	}
	discarded := 0
	for _, file := range r.Files {
		switch file.Outcome {
		case outcomeDiscarded:
			discarded++
			fmt.Fprintf(out, "    discarded  %s (%s)\n", file.Path, file.DraftID)
		case pushOutcomeRefused:
			fmt.Fprintf(out, "    kept       %s — %s\n", file.Path, file.Error)
		default:
			fmt.Fprintf(out, "    no draft   %s\n", file.Path)
		}
	}
	fmt.Fprintf(out, "\n  Discarded %d draft(s). The live tables and your local files are untouched —\n", discarded)
	fmt.Fprintf(out, "  `ronja pipeline status` will show the folder as changed against them. Push\n")
	fmt.Fprintf(out, "  again to open fresh drafts.\n")
	if r.Error != "" {
		fmt.Fprintf(out, "\n  %s.\n", r.Error)
	}
}
