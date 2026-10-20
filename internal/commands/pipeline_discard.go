package commands

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"os"
	"sort"

	"github.com/ronjatech/ronja-cli/internal/api"
	"github.com/ronjatech/ronja-cli/internal/wfdir"
	"github.com/spf13/cobra"
)

// A resource verb the CLI allows because a draft wedged by a deleted input
// rejects every build; it never touches a live table.
func newPipelineDiscardCmd() *cobra.Command {
	var yes bool
	cmd := &cobra.Command{
		Use:   "discard [paths...]",
		Short: "Throw away your staged drafts",
		Long: `Throw away your staged drafts.

Deletes YOUR draft of each table and metric you have pushed. Name paths to
discard only those; with no arguments, every file with a staged draft.

The live tables are not touched, and neither are your local files — the folder
keeps everything you have written, so ` + "`ronja pipeline status`" + ` will show it as
changed against the live versions afterwards. Push again to open fresh drafts.

Asks for confirmation on a terminal; --yes is required without one.`,
		// File paths, validated by resolveArgPaths.
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
			// Emitted even on failure: a discard that stopped part-way has
			// really deleted drafts.
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
	Target  string                  `json:"target,omitempty"`
	Files   []pipelineDiscardedFile `json:"files"`
	Metrics []pipelineDiscardedFile `json:"metrics,omitempty"`
	// Nothing reports a folder with no staged draft at all — a different answer
	// from one whose drafts were all discarded.
	Nothing bool   `json:"nothing"`
	Error   string `json:"error,omitempty"`
}

type pipelineDiscardedFile struct {
	Path    string `json:"path"`
	TableID string `json:"tableID"`
	DraftID string `json:"draftID,omitempty"`
	// Outcome is outcomeDiscarded, outcomeNoDraft or pushOutcomeRefused.
	Outcome string `json:"outcome"`
	Error   string `json:"error,omitempty"`
}

func runPipelineDiscard(ctx context.Context, f *folder, args []string, yes bool) (*pipelineDiscardResult, error) {
	client := newClient(f.Resolved.URL, f.Resolved.Token)
	result := &pipelineDiscardResult{Target: describeTarget(f.Resolved), Files: []pipelineDiscardedFile{}}

	inst := pipelineBaseline(f)

	local, _, err := readPipelineFiles(f.Root)
	if err != nil {
		return nil, err
	}
	metricFiles, err := readMetricFiles(f.Root)
	if err != nil {
		return nil, err
	}
	metricFiles = resolveMetricFiles(f, newPipelineCodec(f, local), f.live(inst), metricFiles)

	// Metrics are read before this refusal: a metric's identity lives in the
	// lock, not Binding.Tables, so a metrics-only folder has an empty map.
	if !f.Bound || (len(f.Binding.Tables) == 0 && len(metricFiles) == 0) {
		return nil, fmt.Errorf("nothing to discard — this folder has no tables or metrics on %s yet", f.Resolved.URL)
	}

	var targets []string
	if len(args) > 0 {
		// Sidecars are read only so a path naming one gets its own refusal
		// below; passing nil would send it to the shape check's wrong message.
		sidecars, readErr := readTableDocsSidecars(f.Root)
		if readErr != nil {
			return nil, readErr
		}
		sqlArgs, namedDocs, namedMetrics, splitErr := splitFileKindArgs(f.Root, args, sidecars, metricFiles)
		if splitErr != nil {
			return nil, splitErr
		}
		if len(namedDocs) > 0 {
			return nil, fmt.Errorf("a docs sidecar has no draft to discard — it writes straight to the live table")
		}
		metricFiles = namedMetrics
		if targets, err = resolveArgPaths(f.Root, sqlArgs, local); err != nil {
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
	if len(targets) == 0 && len(metricFiles) == 0 {
		result.Nothing = true
		return result, nil
	}

	// The gate counts what this run could really delete. With no candidate it
	// asks nothing (in a job that would demand --yes for no write) but still
	// runs, so an unidentifiable file is reported rather than dropped.
	metrics := metricDraftCandidates(metricFiles)
	if len(targets) > 0 || metrics > 0 {
		ok, err := confirm(
			fmt.Sprintf("Discard your draft of %d table(s) and %d metric(s) in %s? Local files are kept.",
				len(targets), metrics, f.Root),
			"deletes your drafts on the server", yes)
		if err != nil {
			return nil, err
		}
		if !ok {
			return nil, fmt.Errorf("cancelled — nothing was discarded")
		}
	}

	failed := 0
	for _, path := range targets {
		// One Ctrl-C, one message, not a refusal per remaining draft.
		if ctx.Err() != nil {
			result.Error = interruptedMessage
			return result, errors.New(interruptedMessage)
		}
		file := discardOneTable(ctx, client, f, inst, path)
		result.Files = append(result.Files, file)
		if file.Outcome == pushOutcomeRefused {
			failed++
		}
		// Saved after every file: a discard is irreversible server-side, so one
		// save at the end could leave pointers at drafts that are already gone.
		if err := f.saveBaseline(); err != nil {
			// Stops: an unwritable baseline fails identically on every
			// remaining file.
			result.Error = fmt.Sprintf("the draft of %s was discarded, but the local baseline in %s could not be updated (%v) — the remaining drafts were left alone",
				path, wfdir.StatePath(f.Root), err)
			return result, fmt.Errorf("%s", result.Error)
		}
	}

	for _, file := range metricFiles {
		if ctx.Err() != nil {
			result.Error = interruptedMessage
			return result, errors.New(interruptedMessage)
		}
		metric := discardOneMetric(ctx, client, f, inst, file)
		if metric == nil {
			continue
		}
		result.Metrics = append(result.Metrics, *metric)
		if metric.Outcome == pushOutcomeRefused {
			failed++
		}
		if err := f.saveBaseline(); err != nil {
			result.Error = fmt.Sprintf("the draft of %s was discarded, but the local baseline in %s could not be updated (%v) — the remaining drafts were left alone",
				file.Path, wfdir.StatePath(f.Root), err)
			return result, fmt.Errorf("%s", result.Error)
		}
	}
	if len(result.Files) == 0 && len(result.Metrics) == 0 {
		result.Nothing = true
		return result, nil
	}

	if failed > 0 {
		result.Error = fmt.Sprintf("%d of %d draft(s) were not discarded", failed, len(result.Files)+len(result.Metrics))
		return result, fmt.Errorf("%s", result.Error)
	}
	return result, nil
}

// discardOneMetric answers nil for a file with nothing staged, as
// publishOneMetric does. It clears the declared fingerprint, which left in
// place would make the next bare push skip the file, and keeps the live one.
func discardOneMetric(ctx context.Context, client *api.Client, f *folder,
	inst *wfdir.InstanceState, file pipelineMetricFile) *pipelineDiscardedFile {

	out := &pipelineDiscardedFile{Path: file.Path, TableID: file.MetricID}
	refuse := func(format string, args ...any) *pipelineDiscardedFile {
		out.Outcome = pushOutcomeRefused
		out.Error = fmt.Sprintf(format, args...)
		fmt.Fprintf(os.Stderr, "  Refused: %s — %s\n", file.Path, out.Error)
		return out
	}
	if file.MetricID == "" {
		// An unreadable file is not one with nothing staged: a real metric
		// arrives with an empty MetricID once its file stops parsing, so the
		// Problem, not the empty id, tells them apart.
		if file.Problem != "" {
			return refuse("%s\n    Until that is fixed nothing here can tell which metric this file defines, so a draft it may have staged is still on the server. Fix the file, then run this again",
				file.Problem)
		}
		return nil
	}
	// The server is asked: a metric keeps no draft pointer.
	draft, err := client.GetTableDraft(ctx, file.MetricID)
	if api.StatusOf(err) == http.StatusNotFound {
		noteUnreachable(file.Path, file.MetricID)
		draft, err = nil, nil
	}
	if err != nil {
		return refuse("check for your draft of %s: %v", file.MetricID, err)
	}
	if draft == nil {
		return nil
	}
	out.DraftID = draft.ID
	if err := client.DiscardTableDraft(ctx, draft.ID); err != nil {
		return refuse("discard %s: %v", draft.ID, err)
	}
	out.Outcome = outcomeDiscarded
	live := f.live(inst)
	recordedID, liveSHA, _ := live.metricSeen(file.Path)
	if recordedID != file.MetricID {
		liveSHA = ""
	}
	live.setMetricSeen(file.Path, file.MetricID, liveSHA, "")
	return out
}

// discardOneTable returns failure as data, not an error, so one refusal never
// stops the rest or loses the baseline for drafts that did go.
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
		// Unbound: its draft cannot be named, but the stale pointer still goes.
		if recorded := inst.Tables[path]; recorded.DraftID != "" {
			recordDraftPointer(inst, path, recorded.TableID, "")
		}
		return out
	}
	// The recorded draft id is a HINT: a draft can be committed or discarded
	// from the web UI between two commands, so the server is asked.
	draft, err := client.GetTableDraft(ctx, out.TableID)
	if api.StatusOf(err) == http.StatusNotFound {
		noteUnreachable(path, out.TableID)
		draft, err = nil, nil
	}
	if err != nil {
		return refuse("check for your draft of %s: %v", out.TableID, err)
	}
	if draft == nil {
		out.Outcome = outcomeNoDraft
		// The same baseline write as success: the draft is gone either way,
		// and keeping its bytes would make the next push answer "Up to date".
		recordDiscarded(f.live(inst), path, out.TableID)
		return out
	}
	out.DraftID = draft.ID
	if err := client.DiscardTableDraft(ctx, draft.ID); err != nil {
		return refuse("discard %s: %v", draft.ID, err)
	}
	out.Outcome = outcomeDiscarded
	recordDiscarded(f.live(inst), path, out.TableID)
	return out
}

// noteUnreachable: a 404 from GET :id/draft is about the TABLE (no draft is a
// 200 null), so it is treated as no draft rather than refused.
func noteUnreachable(path, id string) {
	fmt.Fprintf(os.Stderr, "  Note: %s — %s is no longer reachable (it or its feature is in the trash, or you have no access), so there is no draft to discard.\n", path, id)
}

func printPipelineDiscardReport(r *pipelineDiscardResult) {
	out := os.Stdout
	if r.Nothing {
		fmt.Fprintf(out, "  Nothing to discard — no file in this folder has a staged draft.\n")
		return
	}
	discarded := 0
	for _, file := range append(append([]pipelineDiscardedFile{}, r.Files...), r.Metrics...) {
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

// metricDraftCandidates counts files bound to a metric, with no request. It may
// over-count one whose draft is not open, rather than a round trip per metric
// before the person has agreed.
func metricDraftCandidates(files []pipelineMetricFile) int {
	n := 0
	for _, file := range files {
		if file.MetricID != "" {
			n++
		}
	}
	return n
}
