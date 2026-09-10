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

Deletes YOUR draft of each table and metric you have pushed. Name paths to
discard only those; with no arguments, every file with a staged draft.

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
	// Metrics is one entry per METRIC FILE whose draft this run looked at.
	// Absent for the folders that keep none.
	Metrics []pipelineDiscardedFile `json:"metrics,omitempty"`
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

	inst := pipelineBaseline(f)

	local, _, err := readPipelineFiles(f.Root)
	if err != nil {
		return nil, err
	}
	// The METRIC FILES. A staged metric draft is discardable on exactly the same
	// terms a table draft is — it is the same row kind running the same draft
	// flow — and leaving it out would make `discard` the one verb in this loop
	// that quietly does not cover half the folder.
	metricFiles, err := readMetricFiles(f.Root)
	if err != nil {
		return nil, err
	}
	metricFiles = resolveMetricFiles(f, newPipelineCodec(f, local), f.live(inst), metricFiles)

	// READ BEFORE THE "nothing here" REFUSAL, because a metric is not in
	// Binding.Tables: that map is keyed by .sql path, and a metric's identity
	// lives in the lock. A folder that defines only metrics has an empty map and
	// perfectly real drafts, and refusing it would make `discard` unreachable for
	// exactly the folder shape this feature adds.
	if !f.Bound || (len(f.Binding.Tables) == 0 && len(metricFiles) == 0) {
		return nil, fmt.Errorf("nothing to discard — this folder has no tables or metrics on %s yet", f.Resolved.URL)
	}

	var targets []string
	if len(args) > 0 {
		// The docs sidecars are read here ONLY so a path naming one is recognised
		// and refused in its own words below — discard never acts on them. They
		// used to be passed as nil, which made that refusal unreachable: no
		// argument could match an empty set, so every sidecar path fell through to
		// splitFileKindArgs' shape check and was told it "is not a docs file in
		// this folder" — about a file sitting in the folder.
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

	// THE GATE COUNTS WHAT THIS RUN COULD REALLY DELETE. `targets` is already the
	// filtered set — the paths carrying a recorded draft pointer, or the ones
	// named on the command line — and counting `metricFiles` beside it counted
	// every metric file on disk instead, so a folder holding eleven of them and
	// nothing staged asked to discard eleven drafts and then answered "Nothing".
	//
	// A gate in front of no deletion at all is worse than noise, because there is
	// no terminal in a job: it is a refusal demanding --yes for a command that was
	// never going to write. So a run with no candidate on either side asks
	// nothing — and still RUNS, because a file it cannot identify is reported
	// rather than dropped.
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
		if err := f.saveBaseline(); err != nil {
			// A baseline that cannot be written is that same state, and it does
			// not get better by discarding more drafts into it — a read-only
			// .ronja/ or a full disk fails identically on every remaining file.
			// So it stops, says so, and exits non-zero.
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

// discardOneMetric throws away one metric file's draft.
//
// It answers nil for a file with nothing staged, exactly as publishOneMetric
// does and for the same reason: with no arguments this walks every metric file
// in the folder, and reporting "no draft" for each of eleven files that never
// had one is noise rather than an answer. A file this run could not READ is not
// that case, and is refused below.
//
// THE DECLARED FINGERPRINT IS CLEARED and the live one is kept, which is the
// whole of what a discard means here. The live metric did not move — a discard
// touches only the draft — so the recording taken from it is still true. What is
// no longer true is "this folder has pushed this file's current content", since
// the row that held it has just been deleted; leaving it would let the next bare
// push skip the file and leave the folder with nothing staged anywhere, which is
// the state this command's report promises it has not left behind.
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
		// A FILE THIS RUN COULD NOT READ IS NOT A FILE WITH NOTHING STAGED, and
		// here the difference is a draft somebody asked to delete, was not told
		// about, and which still exists.
		//
		// The two are told apart by the Problem rather than by the empty id.
		// resolveMetricIdentities skips a file that already carries one, so a
		// metric that really exists — created from this folder, its id in the
		// lock — arrives here with an empty MetricID the moment its file stops
		// parsing, and nothing here can name the draft to drop it. (A Problem
		// raised by the SOURCE pass is different: identities are resolved before
		// it, so such a file keeps its id and goes on being discardable — which is
		// what this command is FOR, since a draft wedged by an input somebody
		// deleted is exactly the state its own header describes.)
		if file.Problem != "" {
			return refuse("%s\n    Until that is fixed nothing here can tell which metric this file defines, so a draft it may have staged is still on the server. Fix the file, then run this again",
				file.Problem)
		}
		return nil
	}
	// The server is asked rather than a recorded pointer, exactly as it is for a
	// table: a draft can be committed or discarded from the web UI between two
	// commands. For a metric there is no pointer to consult in any case.
	draft, err := client.GetTableDraft(ctx, file.MetricID)
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
		recordDiscarded(f.live(inst), path, out.TableID)
		return out
	}
	out.DraftID = draft.ID
	if err := client.DiscardTableDraft(ctx, draft.ID); err != nil {
		return refuse("discard %s: %v", draft.ID, err)
	}
	out.Outcome = outcomeDiscarded
	// The pointer goes, and the content baseline is re-pointed at the LIVE
	// table — the only row this file is synced with now.
	recordDiscarded(f.live(inst), path, out.TableID)
	return out
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

// metricDraftCandidates counts the metric files this run could discard a draft
// of, which is what the confirmation above quotes.
//
// A metric keeps no local draft pointer to filter on — discardOneMetric asks the
// server for it, one file at a time — so the closest thing this folder can say
// without a request is "these are the files that name a row here at all". A file
// with no metric behind it certainly has no draft of one, and neither has one
// this run could not identify well enough to ask about.
//
// It can still over-count by a bound metric whose draft is simply not open. The
// alternative is a round trip per metric BEFORE the person has agreed to
// anything, which is a worse trade than a number that is occasionally one too
// many — and the report afterwards says exactly what was dropped.
func metricDraftCandidates(files []pipelineMetricFile) int {
	n := 0
	for _, file := range files {
		if file.MetricID != "" {
			n++
		}
	}
	return n
}
