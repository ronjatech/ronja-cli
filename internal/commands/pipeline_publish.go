package commands

import (
	"context"
	"errors"
	"fmt"
	"os"
	"sort"
	"strings"
	"time"

	"github.com/ronjatech/ronja-cli/internal/api"
	"github.com/ronjatech/ronja-cli/internal/metricfile"
	"github.com/ronjatech/ronja-cli/internal/tabledocs"
	"github.com/ronjatech/ronja-cli/internal/tablerefs"
	"github.com/ronjatech/ronja-cli/internal/wfdir"
	"github.com/spf13/cobra"
)

// `ronja pipeline publish` commits your staged drafts onto their tables — or,
// when you may not, asks an admin to.
//
// The two-outcome honesty is the point, exactly as in `wf publish`: a non-admin
// publishing into a shared feature cannot commit, and reporting "published" when
// what actually happened was "an admin now has a review request" is the kind of
// lie people discover a week later.
//
// A commit CASCADES: the server invalidates and rebuilds every table downstream
// of the one just published. That is why this loop needs no `run` verb, and it
// is worth saying out loud at the moment it happens.
func newPipelinePublishCmd() *cobra.Command {
	var noRequestReview bool
	var overwriteRemote bool
	cmd := &cobra.Command{
		Use:   "publish [paths...]",
		Short: "Commit your staged drafts, or submit them for review",
		Long: `Commit your staged drafts, or submit them for review.

Publishes the draft behind each file you have pushed. Name paths to publish only
those; with no arguments, every file with a staged draft.

What happens depends on the feature:

  your own feature      each draft is committed onto its table
  a shared feature      an admin commits it, so the draft is submitted for
                        review and you are told so

--no-request-review turns the second case into an error instead of a review
request, which is what CI wants when a merge is expected to land directly.

A draft whose build failed is refused: publishing it would put a table live with
no data behind it. Fix the SQL and push again.

Metrics publish the same way. Committing a definition change re-fingerprints the
metric, so one an admin had VERIFIED reads "verified · review pending" until it
is verified again — this command says so when it happens.

A draft whose table has been published to since it forked is refused too, and
nothing is committed — landing it would revert whoever got there first. Push
again to start from where the table is now, or re-run with --overwrite-remote to
commit over their version deliberately.

Committing cascades. Every table that reads from one you publish is invalidated
and rebuilt server-side, so there is nothing else to run afterwards.

Push first: publish sends what is already on the server, and warns when the
folder has local changes that are not in the drafts.`,
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
			result, err := runPipelinePublish(cmd.Context(), f, args, noRequestReview, overwriteRemote)
			// Emitted even on failure, exactly like push: a refusal that carries a
			// result is one the caller has to be able to READ.
			if result != nil {
				if flagJSON {
					if emitErr := emitJSON(result); emitErr != nil {
						return emitErr
					}
				} else {
					printPipelinePublishReport(result)
				}
			}
			return err
		},
	}
	cmd.Flags().BoolVar(&noRequestReview, "no-request-review", false,
		"fail instead of submitting a draft for admin review")
	cmd.Flags().BoolVar(&overwriteRemote, "overwrite-remote", false,
		"commit even though someone published a new version since your draft was created, discarding their changes")
	return cmd
}

// pipelinePublishResult is the --json shape and the human renderer's input.
type pipelinePublishResult struct {
	FeatureID string                  `json:"featureID,omitempty"`
	Target    string                  `json:"target,omitempty"`
	Files     []pipelinePublishedFile `json:"files"`
	// Metrics is one entry per METRIC FILE whose draft this publish acted on.
	// Absent for the folders that keep none.
	Metrics []pipelinePublishedMetric `json:"metrics,omitempty"`
	// Nothing reports a publish that found no staged draft at all, which is a
	// different answer from a publish that refused everything it found.
	Nothing bool   `json:"nothing"`
	Error   string `json:"error,omitempty"`
}

// pipelinePublishedMetric is what happened to one metric file's draft.
//
// Its own type rather than a reuse of pipelinePublishedFile, for the reason
// api.TableListItem is its own type: the two are genuinely different shapes and
// pretending otherwise would be a lie in the direction that costs. A metric has
// no `cascade` (nothing in this folder reads a metric — no marker resolves one)
// and it has a governance consequence a table does not, which is the field
// below that no table result carries.
type pipelinePublishedMetric struct {
	Path     string `json:"path"`
	MetricID string `json:"metricID"`
	DraftID  string `json:"draftID,omitempty"`
	// Outcome is outcomePublished, outcomeSubmittedForReview, outcomeConflict,
	// outcomeNoDraft or pushOutcomeRefused — the same vocabulary the .sql half
	// uses, so a caller scripting both branches on one set of strings.
	Outcome string `json:"outcome"`
	Detail  string `json:"detail,omitempty"`
	Error   string `json:"error,omitempty"`
	// Reverified reports the ONE governance consequence publishing a metric has
	// and publishing a table does not: this metric was VERIFIED, the commit moved
	// its definition, so it re-fingerprints and now reads "verified · review
	// pending" until an admin verifies it again.
	//
	// A field rather than only a sentence because it is the thing a script
	// deploying a repository most needs to be able to see: an admin has to be
	// told, and nothing else in this output says so.
	Reverified         bool     `json:"reverified,omitempty"`
	Warnings           []string `json:"warnings,omitempty"`
	OverwroteVersionID string   `json:"overwroteVersionID,omitempty"`
	URL                string   `json:"-"`
}

type pipelinePublishedFile struct {
	Path    string `json:"path"`
	TableID string `json:"tableID"`
	DraftID string `json:"draftID,omitempty"`
	// Outcome is outcomePublished, outcomeSubmittedForReview, outcomeConflict or
	// pushOutcomeRefused — the same vocabulary the other loops use, so a caller
	// scripting several of them branches on one set of strings. `conflict` is a
	// refusal, and is separate from `refused` for the reason `wf publish` splits
	// them: an agent has to tell "somebody committed first, re-apply and try
	// again" from "you may not do this at all".
	Outcome string `json:"outcome"`
	Detail  string `json:"detail,omitempty"`
	Error   string `json:"error,omitempty"`
	// Cascade is how many files IN THIS FOLDER read DIRECTLY from this table.
	// Never a tenant-wide number and never a transitive one: GetDependents is not
	// exposed over HTTP, and the real cascade is both wider (it leaves the folder)
	// and conditional (a zero-partition parent defers it entirely) in ways a
	// confident count would paper over.
	Cascade  int      `json:"cascade"`
	Warnings []string `json:"warnings,omitempty"`
	// OverwroteVersionID names the committed version this publish deliberately
	// wrote over, and is set ONLY on the --overwrite-remote path. Absent on every
	// ordinary publish, which is what makes its presence meaningful to anything
	// scripting the CLI: it is the record that somebody else's work was
	// discarded, and by which version id. Mirrors publishResult's field.
	OverwroteVersionID string `json:"overwroteVersionID,omitempty"`
	URL                string `json:"-"`
}

// pipelineStagedDrafts names the files this folder has recorded a draft for, in
// path order — exactly the set a publish with no arguments acts on.
//
// A function rather than a loop inside the publish because `sync apply` has to
// ask the same question BEFORE deciding to call the publish at all: "is there
// anything staged here", asked of the one record that answers it. A second loop
// over inst.Tables would be a second definition of what a publish targets, and
// the two would drift the day the selection grows a condition.
func pipelineStagedDrafts(inst *wfdir.InstanceState) []string {
	var out []string
	for path, state := range inst.Tables {
		if state.DraftID != "" {
			out = append(out, path)
		}
	}
	sort.Strings(out)
	return out
}

func runPipelinePublish(ctx context.Context, f *folder, args []string, noRequestReview, overwriteRemote bool) (*pipelinePublishResult, error) {
	client := newClient(f.Resolved.URL, f.Resolved.Token)
	result := &pipelinePublishResult{
		FeatureID: f.Binding.FeatureID,
		Target:    describeTarget(f.Resolved),
		Files:     []pipelinePublishedFile{},
	}

	// The METRIC FILES are read before the "nothing here" refusal, because a
	// metric is not in Binding.Tables: that map is keyed by .sql path, and a
	// metric's identity lives in the lock. A folder that defines only metrics has
	// an empty map and perfectly real drafts.
	metricsOnDisk, err := readMetricFiles(f.Root)
	if err != nil {
		return nil, err
	}
	if !f.Bound || (len(f.Binding.Tables) == 0 && len(metricsOnDisk) == 0) {
		return nil, fmt.Errorf("nothing to publish — this folder has no tables or metrics on %s yet; run `ronja pipeline push` first",
			f.Resolved.URL)
	}

	// A dirty folder is a WARNING, not a refusal: publishing the drafts as they
	// stand is a perfectly reasonable thing to want. But it is also the one way
	// to publish something other than what you are looking at, so it is said out
	// loud. Kind-aware, so a README does not count as a local change.
	if err := warnIfDirty(f); err != nil {
		return nil, err
	}

	local, _, err := readPipelineFiles(f.Root)
	if err != nil {
		return nil, err
	}
	codec := newPipelineCodec(f, local)
	// Reported, never refused: publish acts on drafts that are already staged and
	// built, and a bind that has gone missing since the push cannot un-build them.
	noteAliasReport(checkAliases(f.Manifest, f.selection(), f.Codec, local, folderStems(f.Kind, local), folderFieldRefs(f.Kind, f.Root, local)))
	inst := pipelineBaseline(f)

	// The METRIC FILES, resolved with no network in sight. Resolved before the
	// argument split, because a path naming one has to be recognised rather than
	// handed to the .sql resolver — which would refuse it with a message about a
	// file that is not SQL.
	metricFiles := resolveMetricFiles(f, codec, f.live(inst), metricsOnDisk)

	var targets []string
	if len(args) > 0 {
		// The docs sidecars are read here ONLY so a path naming one is recognised
		// and refused in its own words below — publish never acts on them.
		sidecars, readErr := readTableDocsSidecars(f.Root)
		if readErr != nil {
			return nil, readErr
		}
		sqlArgs, namedDocs, namedMetrics, splitErr := splitFileKindArgs(f.Root, args, sidecars, metricFiles)
		if splitErr != nil {
			return nil, splitErr
		}
		// Refused rather than ignored. A docs sidecar writes STRAIGHT TO THE LIVE
		// ROW — there is no draft in that path at all — so naming one here is
		// somebody expecting a publish step that does not exist, and silently
		// dropping it would look like it had happened.
		if len(namedDocs) > 0 {
			return nil, fmt.Errorf("a docs sidecar has no draft to publish — `ronja pipeline push` writes one straight to the live table")
		}
		metricFiles = namedMetrics
		if targets, err = resolveArgPaths(f.Root, sqlArgs, local); err != nil {
			return nil, err
		}
	} else {
		targets = pipelineStagedDrafts(inst)
	}
	if len(targets) == 0 && len(metricFiles) == 0 {
		result.Nothing = true
		return result, nil
	}
	// Dependency order, as a push has. A commit cascades, so publishing a
	// downstream before its upstream lands a table computed from data that is
	// about to be replaced and then rebuilt again seconds later — and it makes the
	// input-staleness warning below fire or not fire depending on nothing more
	// than the alphabet. Ordered, the warning at least means the same thing every
	// run.
	targets = orderPublishTargets(codec, local, targets)

	// The route is decided UP FRONT from the two facts that decide it
	// server-side — the feature's scope and the caller's role — rather than by
	// trying a commit and reading the rejection prose. Matching on an error
	// message is how a client silently starts doing the wrong thing the day
	// somebody rewords it. Read ONCE for the whole run; both are properties of
	// the feature and the caller, not of any one table.
	routing := publishRouting{NoRequestReview: noRequestReview, OverwriteRemote: overwriteRemote}
	if f.Binding.FeatureID != "" {
		shared, err := featureIsShared(ctx, client, f.Binding.FeatureID)
		if err != nil {
			// UNKNOWN, not "private". The distinction is the whole point of the
			// separate flag: read as private, an unreadable scope would ALSO disarm
			// the 400 fallback below, so a shared-feature publish whose scope read
			// happened to fail would die on the commit instead of filing the review
			// request it needed. Unknown commits first and still falls back.
			routing.ScopeUnknown = true
			fmt.Fprintf(os.Stderr, "  Note: could not read the scope of feature %s (%v) — trying to commit, and treating a refusal as the needs-review case.\n",
				f.Binding.FeatureID, err)
		} else {
			routing.SharedFeature = shared
		}
	}
	// The role is only asked for when it could change the route. Under
	// --no-request-review there is no review branch to take, so the request would
	// buy nothing.
	if routing.SharedFeature && !noRequestReview {
		admin, err := callerIsAdmin(ctx, client)
		if err != nil {
			// Fall through to the commit attempt: the 400 fallback below still
			// catches the needs-review case, so an unreadable role costs an extra
			// round trip rather than the command.
			fmt.Fprintf(os.Stderr, "  Note: could not read your role (%v) — trying to commit.\n", err)
		} else {
			routing.ReviewUpFront = !admin
		}
	}

	// The feature's tables, once, for the input-staleness check below. Cheap
	// (one request) and best-effort: a staleness warning nobody could produce is
	// not a reason to refuse a publish.
	var byID map[string]*api.TableListItem
	if f.Binding.FeatureID != "" {
		items, err := client.ListFeatureTables(ctx, f.Binding.FeatureID)
		if err != nil {
			// Said out loud, like every other degraded read here. Swallowed, the
			// ABSENCE of a staleness warning was indistinguishable from a clean
			// check — and "no warning" is exactly what somebody reads before
			// deciding to publish.
			fmt.Fprintf(os.Stderr, "  Note: could not list the tables of %s (%v) — publishing without the upstream-staleness check.\n",
				f.Binding.FeatureID, err)
		} else {
			byID = make(map[string]*api.TableListItem, len(items))
			for _, item := range items {
				if item != nil {
					byID[item.ID] = item
				}
			}
		}
	}

	failed := 0
	for _, path := range targets {
		// One Ctrl-C, one message. Without this every remaining draft makes its
		// own doomed commit attempt and prints its own refusal, turning a single
		// interrupt into a page of them.
		if ctx.Err() != nil {
			result.Error = interruptedMessage
			return result, errors.New(interruptedMessage)
		}
		file := publishOneTable(ctx, client, f, codec, inst, path, local, byID, routing)
		result.Files = append(result.Files, file)
		if err := f.saveBaseline(); err != nil {
			// A commit is irreversible server-side, and a baseline that cannot be
			// written does not get better by committing more drafts into it — a
			// read-only .ronja/ or a full disk fails identically on every remaining
			// file. So it stops, says so, and exits non-zero, exactly as discard
			// does.
			result.Error = fmt.Sprintf("%s was published, but the local baseline in %s could not be updated (%v) — the remaining drafts were left alone",
				path, wfdir.StatePath(f.Root), err)
			return result, errors.New(result.Error)
		}
		if file.Outcome == pushOutcomeRefused || file.Outcome == outcomeConflict {
			failed++
		}
	}

	// The METRIC FILES, after the tables. A metric reads a table rather than the
	// other way round — no marker resolves a metric, so nothing in this folder
	// can read one — so publishing a metric after the table it reads is the same
	// dependency order the .sql half computes, arrived at by construction.
	for _, file := range metricFiles {
		if ctx.Err() != nil {
			result.Error = interruptedMessage
			return result, errors.New(interruptedMessage)
		}
		metric := publishOneMetric(ctx, client, f, inst, file, routing)
		if metric == nil {
			continue
		}
		result.Metrics = append(result.Metrics, *metric)
		if err := f.saveBaseline(); err != nil {
			// A commit is irreversible server-side, and a baseline that cannot be
			// written does not get better by committing more drafts into it. Same
			// stop-and-say-so as the .sql loop above.
			result.Error = fmt.Sprintf("%s was published, but the local baseline in %s could not be updated (%v) — the remaining drafts were left alone",
				file.Path, wfdir.StatePath(f.Root), err)
			return result, errors.New(result.Error)
		}
		if metric.Outcome == pushOutcomeRefused || metric.Outcome == outcomeConflict {
			failed++
		}
	}
	if len(result.Files) == 0 && len(result.Metrics) == 0 {
		// Every metric file this run looked at had nothing staged, and there were
		// no .sql targets either. The same answer the early return gives, reached
		// from the other side.
		result.Nothing = true
		return result, nil
	}

	if failed > 0 {
		result.Error = fmt.Sprintf("%d of %d draft(s) were not published", failed, len(result.Files)+len(result.Metrics))
		return result, fmt.Errorf("%s", result.Error)
	}
	return result, nil
}

// publishOneMetric commits one metric file's staged draft, or files it for
// review, and records what that leaves behind.
//
// It answers nil for a file with nothing staged, which is the ordinary case for
// most metric files on most publishes and is why publish reads the server rather
// than a recorded draft pointer: a metric's draft can be committed or discarded
// from the web UI between two commands, exactly as a table's can, and this loop
// keeps no per-file draft pointer for a metric to go stale in the first place.
// A file this run could not READ is not that case, and is refused below.
func publishOneMetric(ctx context.Context, client *api.Client, f *folder, inst *wfdir.InstanceState,
	file pipelineMetricFile, routing publishRouting) *pipelinePublishedMetric {

	out := &pipelinePublishedMetric{Path: file.Path, MetricID: file.MetricID}
	refuse := func(format string, args ...any) *pipelinePublishedMetric {
		out.Outcome = pushOutcomeRefused
		out.Error = fmt.Sprintf(format, args...)
		fmt.Fprintf(os.Stderr, "  Refused: %s — %s\n", file.Path, out.Error)
		return out
	}
	conflict := func(format string, args ...any) *pipelinePublishedMetric {
		out.Outcome = outcomeConflict
		out.Error = fmt.Sprintf(format, args...)
		fmt.Fprintf(os.Stderr, "  Conflict: %s — %s\n", file.Path, out.Error)
		return out
	}
	if file.MetricID == "" {
		// A FILE THIS RUN COULD NOT READ IS NOT A FILE WITH NOTHING STAGED, and
		// answering both with silence is how a broken file becomes an unmentioned
		// no-op: exit 0, no line in the report, and the draft it staged still on
		// the server with nothing having said so.
		//
		// The two are told apart by the Problem rather than by the empty id.
		// resolveMetricIdentities skips a file that already carries one, so a
		// metric that really exists — created from this folder, its id in the
		// lock — arrives here with an empty MetricID the moment its file stops
		// parsing. (A Problem raised by the SOURCE pass is different: identities
		// are resolved before it, and publish needs nothing else from the file —
		// what it commits is the draft on the server — so such a file keeps its id
		// and never reaches this branch.)
		//
		// Refused rather than skipped, which is the answer `push` gives the same
		// file and the one `status` reports about it, so three verbs say one thing.
		if file.Problem != "" {
			return refuse("%s\n    Until that is fixed nothing here can tell which metric this file defines, so a draft it may have staged was left where it is. Fix the file, then publish again",
				file.Problem)
		}
		// Nothing was ever created here, so there is nothing to commit. Silent
		// rather than refused: `publish` with no arguments is not the command that
		// should be teaching anybody that they have not pushed yet, and `push`
		// says it in the one place it is actionable.
		return nil
	}

	draft, err := client.GetTableDraft(ctx, file.MetricID)
	if err != nil {
		return refuse("check for your draft of %s: %v", file.MetricID, err)
	}
	if draft == nil {
		return nil
	}
	out.DraftID = draft.ID

	// The LIVE row BEFORE the commit, read for the one thing only it can say:
	// whether this metric is VERIFIED, and what its definition was. Both are
	// needed to report the governance consequence below, and both are gone the
	// moment the commit lands.
	liveBefore, liveBeforeErr := client.GetTable(ctx, file.MetricID)
	if liveBeforeErr != nil {
		fmt.Fprintf(os.Stderr, "  Note: could not read %s before committing it (%v) — publishing without the re-verification notice.\n",
			file.MetricID, liveBeforeErr)
	}

	// The verdict lives only on the single-row GET, so the draft is read by its
	// own id. Refusing a failed draft here rather than letting the commit through
	// is the CLI naming the fix: the server would happily commit a definition
	// that does not build, and the metric would go live with nothing behind it.
	staged, err := client.GetTable(ctx, draft.ID)
	if err != nil {
		return refuse("read draft %s: %v", draft.ID, err)
	}
	switch staged.BuildVerdict {
	case api.BuildVerdictFailed, api.BuildVerdictFailedStale:
		reason := ""
		if staged.LastBuildError != nil {
			reason = ": " + staged.LastBuildError.Message
		}
		return refuse("draft %s did not build%s\n    Fix the recipe in %s and run `ronja pipeline push` again", draft.ID, reason, file.Path)
	case api.BuildVerdictBuilding:
		return refuse("draft %s is still building — wait for it to finish, or run `ronja pipeline push %s` to watch it", draft.ID, file.Path)
	case api.BuildVerdictPending:
		return refuse("draft %s has not been built — publishing it would put %s live with nothing behind it.\n    Run `ronja pipeline push %s` to build it (a queued build finishes there too)",
			draft.ID, file.MetricID, file.Path)
	}

	if routing.ReviewUpFront {
		if err := client.RequestTableReview(ctx, draft.ID); err != nil {
			return refuse("submit %s for review: %v", draft.ID, err)
		}
		out.Outcome = outcomeSubmittedForReview
		out.Detail = "this feature is shared, so an admin commits changes to it"
		return out
	}

	commitErr := client.CommitTableDraft(ctx, draft.ID, "")
	if why, taken := api.AsNameTaken(commitErr); taken {
		return refuse("committing %s was refused — the name your draft would publish is taken:\n      %s\n    Nothing was committed and your draft is intact. A metric and a table share one namespace inside a feature; rename one of the two in the web app, then publish again",
			file.MetricID, why)
	}
	if commitErr != nil && api.StatusOf(commitErr) == api.StatusConflict {
		if !routing.OverwriteRemote {
			return conflict("%s has been published to since your draft forked — nothing was committed, and your draft is intact:\n      %v\n    Re-apply your change on top of theirs (`ronja pipeline discard %s`, then `ronja pipeline push %s`), or re-run with --overwrite-remote to commit over their version",
				file.MetricID, commitErr, file.Path, file.Path)
		}
		head, headErr := client.GetTableDraftReview(ctx, draft.ID)
		switch {
		case headErr != nil:
			return refuse("committing %s was refused (%v), and reading the current version of %s in order to overwrite it failed too: %v",
				draft.ID, commitErr, file.MetricID, headErr)
		case head.HeadVersionID == "":
			return refuse("committing %s was refused (%v), but %s reports no committed version to overwrite — --overwrite-remote has nothing to confirm",
				draft.ID, commitErr, file.MetricID)
		}
		fmt.Fprintf(os.Stderr, "  --overwrite-remote: committing %s over version %s of %s, discarding what it changed.\n",
			file.Path, head.HeadVersionID, file.MetricID)
		if err := client.CommitTableDraft(ctx, draft.ID, head.HeadVersionID); err != nil {
			// Deliberately not retried: an override authorizes overwriting the
			// version it was shown, not whatever happens to be there by the time
			// the request arrives.
			if api.StatusOf(err) == api.StatusConflict {
				return conflict("%s moved again while this was running — version %s is no longer the current one, so nothing was committed and your draft is intact: %v",
					file.MetricID, head.HeadVersionID, err)
			}
			return refuse("commit %s over version %s: %v", draft.ID, head.HeadVersionID, err)
		}
		out.OverwroteVersionID = head.HeadVersionID
		commitErr = nil
	}
	if commitErr != nil {
		// The race the up-front routing cannot close: the feature was shared, or
		// the caller's role changed, between the reads above and this commit.
		if routing.NoRequestReview || !(routing.SharedFeature || routing.ScopeUnknown) || api.StatusOf(commitErr) != 400 {
			return refuse("commit %s: %v", draft.ID, commitErr)
		}
		fmt.Fprintf(os.Stderr, "  Note: the commit of %s was refused (%v) — submitting the draft for review instead.\n", file.Path, commitErr)
		if err := client.RequestTableReview(ctx, draft.ID); err != nil {
			return refuse("commit %s was refused (%v), and submitting it for review failed too: %v", draft.ID, commitErr, err)
		}
		out.Outcome = outcomeSubmittedForReview
		out.Detail = fmt.Sprintf("the commit was refused (%v), so the draft was submitted for review", commitErr)
		return out
	}

	out.Outcome = outcomePublished
	out.Detail = fmt.Sprintf("committed onto %s", file.MetricID)
	if out.OverwroteVersionID != "" {
		out.Detail = fmt.Sprintf("committed onto %s, overwriting version %s that was published after your draft was created",
			file.MetricID, out.OverwroteVersionID)
	}
	out.URL = staged.URL

	// THE BASELINE, AND WHAT IS WITHHELD FROM IT. `publish` runs in a separate
	// process from the `push` that staged the recipe and remembers nothing about
	// it, so whether the definition this folder declares is now the LIVE one is a
	// question only the live row can answer — the same question docsLanded asks,
	// for the same reason, and answered here by comparing the two rows rather
	// than the file against a row: the commit copies the DRAFT onto the parent,
	// so "did it land" is exactly "does live now hold what the draft held".
	//
	// GONE IS NOT LANDED. A draft also stops existing on a discard and on a
	// reviewer's rejection, and reaching this point without checking would bank
	// an agreement for a definition nobody ever committed.
	live, liveErr := client.GetTable(ctx, file.MetricID)
	recordedID, _, declared := f.live(inst).metricSeen(file.Path)
	if recordedID != file.MetricID {
		declared = ""
	}
	if liveErr != nil {
		// The commit HAS happened; failing the command afterwards would report
		// something that did happen as something that did not. The fingerprints
		// are CLEARED rather than left: they describe a state that has certainly
		// moved — this publish just moved it — so keeping them would refuse the
		// next push and name a change this folder made itself.
		fmt.Fprintf(os.Stderr, "  Note: %s was published, but its baseline could not be refreshed (%v) — `ronja pipeline status` will show it as not compared until the next push.\n",
			file.Path, liveErr)
		f.live(inst).setMetricSeen(file.Path, file.MetricID, "", "")
		return out
	}
	landed := metricfile.RecipeSHA256(live.MetricRecipe) == metricfile.RecipeSHA256(staged.MetricRecipe)
	if !landed {
		fmt.Fprintf(os.Stderr, "  Note: %s was published, but %s does not hold the definition that draft held — it was discarded or rejected rather than committed, or something else was committed over it. The file is left as unsynced so the next `ronja pipeline push` stages it again.\n",
			file.Path, file.MetricID)
		declared = ""
	}
	f.live(inst).setMetricSeen(file.Path, file.MetricID, metricfile.RecipeSHA256(live.MetricRecipe), declared)

	// THE ONE GOVERNANCE CONSEQUENCE A METRIC HAS AND A TABLE DOES NOT.
	// Committing a definition change re-fingerprints the metric, and a metric
	// whose definition_hash no longer equals its verified_against_definition_hash
	// reads as DRIFTED — "verified · review pending" — until an admin verifies it
	// again. The author caused it and would otherwise meet it in the UI, days
	// later, as a badge that changed by itself.
	if landed && liveBeforeErr == nil && liveBefore.MetricStatus == api.MetricStatusVerified &&
		metricfile.RecipeSHA256(liveBefore.MetricRecipe) != metricfile.RecipeSHA256(live.MetricRecipe) {
		out.Reverified = true
		warning := fmt.Sprintf("%s was VERIFIED and its definition has now changed — it reads \"verified · review pending\" until an admin verifies it again", file.MetricID)
		out.Warnings = append(out.Warnings, warning)
		fmt.Fprintf(os.Stderr, "  Warning: %s\n", warning)
	}
	return out
}

// publishRouting is the decision, taken once for the whole run: it is a property
// of the feature and the caller, not of any one table.
//
// The three fields are separate rather than collapsed into one bool because the
// FALLBACK needs them apart. A 400 from the commit is only the server's
// needs-review rejection when the feature is shared AND a review request is
// wanted; under --no-request-review the same 400 has to stay an error, and a
// single "should I ask for review" flag could not express that — which is the
// bug this shape replaced.
type publishRouting struct {
	// SharedFeature is the feature's scope, read whether or not the review
	// branch is wanted, because the fallback below is conditioned on it.
	SharedFeature bool
	// ScopeUnknown reports that the scope read FAILED, which is not the same as
	// reading `private`: the up-front review branch is not taken (nothing says it
	// is needed), but the 400 fallback stays armed, because the one thing we do
	// know is that we cannot rule the shared case out.
	ScopeUnknown bool
	// ReviewUpFront is the decision taken from the facts: shared feature,
	// non-admin caller, review requests allowed.
	ReviewUpFront bool
	// NoRequestReview turns every review route into an error, which is what CI
	// wants when a merge is expected to land directly.
	NoRequestReview bool
	// OverwriteRemote authorizes committing over a version of the parent that
	// landed after the draft was created — a deliberately unpleasant name for a
	// deliberately unpleasant thing, matching `wf publish`. It resolves the
	// current head and confirms it explicitly; it never loops, so a version that
	// lands while this is running produces a fresh refusal rather than a second
	// attempt.
	//
	// It rides on this struct rather than on its own parameter because it is a
	// property of the RUN, exactly as the other three are — and because the
	// commit's two refusals (400 needs-review, 409 moved) are decided in one
	// place, from one value.
	OverwriteRemote bool
}

func publishOneTable(ctx context.Context, client *api.Client, f *folder, codec pipelineCodec, inst *wfdir.InstanceState,
	path string, local map[string]string, byID map[string]*api.TableListItem, routing publishRouting) pipelinePublishedFile {

	out := pipelinePublishedFile{Path: path, TableID: f.Binding.Tables[path]}
	refuse := func(format string, args ...any) pipelinePublishedFile {
		out.Outcome = pushOutcomeRefused
		out.Error = fmt.Sprintf(format, args...)
		fmt.Fprintf(os.Stderr, "  Refused: %s — %s\n", path, out.Error)
		return out
	}
	// A CONFLICT is a refusal with its own outcome, for the reason `wf publish`
	// has one: an agent driving this loop has to tell "somebody committed first,
	// re-apply and try again" from "you may not do this at all", and both are a
	// refusal with prose on stderr. Both still count as not-published.
	conflict := func(format string, args ...any) pipelinePublishedFile {
		out.Outcome = outcomeConflict
		out.Error = fmt.Sprintf(format, args...)
		fmt.Fprintf(os.Stderr, "  Conflict: %s — %s\n", path, out.Error)
		return out
	}
	if out.TableID == "" {
		return refuse("%s has no table on %s yet — run `ronja pipeline push` first", path, f.Resolved.URL)
	}

	// The recorded draft id is a HINT, never authority: a draft can be committed
	// or discarded from the web UI between two commands, so the server is asked.
	draft, err := client.GetTableDraft(ctx, out.TableID)
	if err != nil {
		return refuse("check for your draft of %s: %v", out.TableID, err)
	}
	if draft == nil {
		// The state was stale — somebody already landed or dropped this draft.
		recordDraftPointer(inst, path, out.TableID, "")
		return refuse("you have no open draft of %s — it was committed or discarded elsewhere; run `ronja pipeline push` to stage a new one", out.TableID)
	}
	out.DraftID = draft.ID

	// The verdict lives only on the single-row GET, so the draft is read by its
	// own id. Refusing a failed draft here rather than letting the commit through
	// is the CLI naming the fix: the server would happily commit SQL that does
	// not build, and the table would go live with nothing behind it.
	staged, err := client.GetTable(ctx, draft.ID)
	if err != nil {
		return refuse("read draft %s: %v", draft.ID, err)
	}
	switch staged.BuildVerdict {
	case api.BuildVerdictFailed, api.BuildVerdictFailedStale:
		reason := ""
		if staged.LastBuildError != nil {
			reason = ": " + staged.LastBuildError.Message
		}
		return refuse("draft %s did not build%s\n    Fix the SQL in %s and run `ronja pipeline push` again", draft.ID, reason, path)
	case api.BuildVerdictBuilding:
		return refuse("draft %s is still building — wait for it to finish, or run `ronja pipeline push %s` to watch it", draft.ID, path)
	case api.BuildVerdictPending:
		// NOT "still building". A checked-out draft sits at `pending` until
		// something syncs it, so the commonest way to be here is a draft that was
		// opened — in the web builder, by chat, by a push that died before its
		// write — and never built at all. Telling that person to wait is telling
		// them to wait for something nobody has started.
		return refuse("draft %s has not been built — publishing it would put %s live with nothing behind it.\n    Run `ronja pipeline push %s` to build it (a queued build finishes there too)",
			draft.ID, out.TableID, path)
	}

	// Two staleness warnings, both before the commit and neither a refusal. The
	// first now says early what the commit's own CAS would say anyway, which is
	// worth the round trip: it names the intervening versions, where the server's
	// 409 can only name the head.
	review, reviewErr := client.GetTableDraftReview(ctx, draft.ID)
	if reviewErr != nil {
		fmt.Fprintf(os.Stderr, "  Note: no review payload for %s (%v) — publishing without the staleness check.\n", path, reviewErr)
	} else if review.BaseStale {
		ids := make([]string, 0, len(review.InterveningVersions))
		for _, v := range review.InterveningVersions {
			ids = append(ids, v.VersionID)
		}
		warning := fmt.Sprintf("%s has been published to since your draft forked (%s) — the commit will be refused unless you pass --overwrite-remote",
			out.TableID, strings.Join(ids, ", "))
		out.Warnings = append(out.Warnings, warning)
		fmt.Fprintf(os.Stderr, "  Warning: %s\n", warning)
	}
	// Resolved, so a ref spelled as an alias or a sibling's stem is checked too. A
	// name that could not be resolved is simply dropped by DeriveInputModels, which
	// is the right way for a heuristic warning to degrade — it under-reports rather
	// than naming a table nobody referenced.
	//
	// The FALLBACK is what keeps that true. toWire refuses a whole file on a
	// folder-level problem (an ambiguous stem, a self-reference, one sibling
	// spelled two ways) and answers "" when it does — and "" holds no refs at all,
	// so taking it would make this warning VANISH for that file rather than
	// under-report, which is the opposite of what the paragraph above promises.
	// An ambiguous stem needs nothing more than two same-named .sql files in
	// different subdirectories, so this is an ordinary folder, not a corrupt one.
	// The file as written is the honest substitute: its id-form refs are still
	// checked, and only the names this codec could not resolve go unexamined.
	wire := local[path]
	if resolved, wireErr := codec.toWire(path, wire); wireErr == nil {
		wire = resolved
	} else {
		fmt.Fprintf(os.Stderr, "  Note: %v — the upstream-staleness check for %s ran on the file as written, so it may miss a table named by an alias or by a sibling's stem.\n",
			wireErr, path)
	}
	if stale := staleInputs(wire, byID, draft.UpdatedAt); len(stale) > 0 {
		// GetDependents excludes shadow rows, so a staged draft is never
		// invalidated by an upstream publish: the confidence report silently
		// decays between push and publish, and nothing else would say so.
		warning := fmt.Sprintf("%s may have changed since this draft was built — if so, what you are about to publish was computed from older upstream data; push again to rebuild it",
			strings.Join(stale, ", "))
		out.Warnings = append(out.Warnings, warning)
		fmt.Fprintf(os.Stderr, "  Warning: %s %s\n", path, warning)
	}

	if routing.ReviewUpFront {
		// Already filed. The server accepts a second request-review and the draft
		// is not harmed by one, but reporting "submitted for review" to somebody
		// who submitted it yesterday reads as progress that did not happen — the
		// admin's inbox has had it all along.
		if review != nil && review.SubmittedForReview {
			out.Outcome = outcomeSubmittedForReview
			out.Detail = "already submitted for review — an admin has it; nothing was re-sent"
			return out
		}
		if err := client.RequestTableReview(ctx, draft.ID); err != nil {
			return refuse("submit %s for review: %v", draft.ID, err)
		}
		out.Outcome = outcomeSubmittedForReview
		out.Detail = "this feature is shared, so an admin commits changes to it"
		return out
	}

	// Set when the commit's outcome was not answered and has to be proved by the
	// live table instead.
	unconfirmed := false
	// The version --overwrite-remote confirmed, held locally until the commit is
	// KNOWN to have landed. On out.OverwroteVersionID it is the record that
	// somebody else's work was discarded, so it must not be reported by a run
	// that then refuses because it could not prove the commit landed at all.
	overwroteVersionID := ""
	commitErr := client.CommitTableDraft(ctx, draft.ID, "")
	// A commit lands the draft's NAME on the live row, so a second 409 now wears
	// this status: the name is taken in the feature. It is answered first and on
	// the CODE, because the branch below is about somebody else's work and sends
	// the author to `discard` or `--overwrite-remote` — neither of which frees a
	// name, and one of which throws away a colleague's version to no purpose.
	if why, taken := api.AsNameTaken(commitErr); taken {
		// The fix is in the web app, not in this folder: `push` sends a name on
		// CREATE only, so renaming the .sql file does not rename the table — it
		// unbinds the file and the next push creates a second one beside it.
		return refuse("committing %s was refused — the name your draft would publish is taken:\n      %s\n    Nothing was committed and your draft is intact. Rename one of the two tables in the web app, then publish again",
			out.TableID, why)
	}
	if commitErr != nil && api.StatusOf(commitErr) == api.StatusConflict {
		// The one refusal that is about somebody else's work rather than about
		// permission: the table moved after this draft forked, so committing would
		// revert a version nobody here has seen. Nothing was written — the CAS runs
		// inside the commit's own transaction, under the parent's row lock — so the
		// draft is fully intact, and the only question is whether the author means
		// to overwrite.
		if !routing.OverwriteRemote {
			// The server's own message names the current version, so it is passed
			// through rather than paraphrased into something less specific.
			return conflict("%s has been published to since your draft forked — nothing was committed, and your draft is intact:\n      %v\n    Re-apply your change on top of theirs (`ronja pipeline discard %s`, then `ronja pipeline push %s`), or re-run with --overwrite-remote to commit over their version",
				out.TableID, commitErr, path, path)
		}
		// The head is READ, never scraped out of the 409's prose: the HTTP error
		// body is the flat app-wide {"error": "<message>"} shape, and a regex over
		// prose starts overwriting the wrong version the day somebody rewords it.
		// Re-read HERE rather than reused from the payload above, so what is
		// confirmed is the head at the moment of the decision.
		head, headErr := client.GetTableDraftReview(ctx, draft.ID)
		switch {
		case headErr != nil:
			return refuse("committing %s was refused (%v), and reading the current version of %s in order to overwrite it failed too: %v",
				draft.ID, commitErr, out.TableID, headErr)
		case head.HeadVersionID == "":
			// Empty is the server's "this table has never been committed to", and
			// there is then no version to confirm — so the override has nothing to
			// say and sending an empty confirm would simply be refused again.
			return refuse("committing %s was refused (%v), but %s reports no committed version to overwrite — --overwrite-remote has nothing to confirm. Look at %s in the web app",
				draft.ID, commitErr, out.TableID, out.TableID)
		}
		fmt.Fprintf(os.Stderr, "  --overwrite-remote: committing %s over version %s of %s, discarding what it changed.\n", path, head.HeadVersionID, out.TableID)
		if err := client.CommitTableDraft(ctx, draft.ID, head.HeadVersionID); err != nil {
			// A SECOND 409 means a third commit landed between the read above and
			// this write. Deliberately not retried: an override authorizes
			// overwriting the version it was shown, not whatever happens to be there
			// by the time the request arrives — and a loop would authorize every one
			// of them.
			if api.StatusOf(err) == api.StatusConflict {
				return conflict("%s moved again while this was running — version %s is no longer the current one, so nothing was committed and your draft is intact; re-run to see where it is now: %v",
					out.TableID, head.HeadVersionID, err)
			}
			// A TIMEOUT is handed to the branch below rather than reported as a
			// failure, on exactly the reasoning that branch owns: the deadline
			// was ours, the transaction and the cascade it fires were the
			// server's, so a commit whose request timed out may well have
			// landed. Returning here left the draft pointer uncleared and the
			// baseline never refreshed -- on the ONE path that has already
			// discarded a colleague's version, which is the worst place in this
			// command to guess.
			if !api.IsTimeout(err) {
				return refuse("commit %s over version %s: %v", draft.ID, head.HeadVersionID, err)
			}
			overwroteVersionID = head.HeadVersionID
			commitErr = err
		} else {
			overwroteVersionID = head.HeadVersionID
			commitErr = nil
		}
	}
	if commitErr != nil && api.IsTimeout(commitErr) {
		// A commit whose request TIMED OUT may well have landed — the deadline
		// was ours, the transaction and the cascade it fires were the server's.
		// So the draft is read back rather than guessed at: reported as a plain
		// failure, a commit that DID land sends the author to a `publish` that
		// answers "committed or discarded elsewhere" about their own successful
		// publish, with the baseline never advanced.
		//
		// Our own draft being GONE is the whole test. It stops existing at the
		// commit and at nothing else this call could have done.
		open, readErr := client.GetTableDraft(ctx, out.TableID)
		switch {
		case readErr != nil:
			return refuse("committing %s timed out (%v) and its outcome could not be read back (%v) — look at %s in the web app before publishing again: the commit may have landed and cascaded",
				draft.ID, commitErr, readErr, out.TableID)
		case open == nil || open.ID != draft.ID:
			// GONE IS NOT LANDED. A draft stops existing on a discard and on a
			// reviewer's rejection too, and both leave the author told they
			// published something nobody ever committed — with the baseline
			// advanced to say so. The live table is the only thing that can tell
			// them apart, and it is read below in any case.
			fmt.Fprintf(os.Stderr, "  Note: committing %s timed out and your draft is gone — reading %s to find out whether it landed.\n", path, out.TableID)
			commitErr, unconfirmed = nil, true
		default:
			return refuse("committing %s timed out and your draft is still open, so it did not land (%v) — run `ronja pipeline publish %s` again",
				draft.ID, commitErr, path)
		}
	}
	if commitErr != nil {
		// The race the up-front routing cannot close: the feature was shared, or
		// the caller's role changed, between the reads above and this commit. A
		// 400 on a SHARED feature is the server's needs-review rejection. A 409 is
		// the head-version CAS and has already been dealt with above.
		//
		// Every condition matters. --no-request-review means the caller wants a
		// failure rather than a review request, and a fallback that ignored it
		// would file one anyway and report success; and a 400 on a feature KNOWN to
		// be private is some other refusal entirely, which a review request would
		// neither fix nor describe. A feature whose scope could not be read is not
		// known to be private, so it keeps the fallback.
		if routing.NoRequestReview || !(routing.SharedFeature || routing.ScopeUnknown) || api.StatusOf(commitErr) != 400 {
			return refuse("commit %s: %v", draft.ID, commitErr)
		}
		fmt.Fprintf(os.Stderr, "  Note: the commit of %s was refused (%v) — submitting the draft for review instead.\n", path, commitErr)
		if err := client.RequestTableReview(ctx, draft.ID); err != nil {
			return refuse("commit %s was refused (%v), and submitting it for review failed too: %v", draft.ID, commitErr, err)
		}
		out.Outcome = outcomeSubmittedForReview
		out.Detail = fmt.Sprintf("the commit was refused (%v), so the draft was submitted for review", commitErr)
		return out
	}

	// The draft is gone, so the recorded pointer has to go with it — otherwise
	// the next publish tries to commit a row that no longer exists.
	recordDraftPointer(inst, path, out.TableID, "")
	live, liveErr := client.GetTable(ctx, out.TableID)
	liveCode, unresolved := "", true
	if liveErr == nil {
		// De-aliased, because liveCode is what recordSynced writes as this file's
		// content baseline below — and that baseline is compared against the hash
		// of the bytes on disk. Recording the server's id form there would make
		// every aliased file read as modified the instant it was published.
		liveCode, unresolved = codec.canonicalDisk(local[path], live)
	}
	// The only proof a commit whose request timed out actually landed: the live
	// table holds what the draft held. Reported honestly when it does not, rather
	// than as a publish — this branch is reached just as readily by a reviewer
	// rejecting the draft, and an author told they published is an author who
	// stops looking.
	if unconfirmed {
		stagedCode, stagedUnresolved := codec.canonicalDisk(local[path], staged)
		switch {
		case liveErr != nil:
			return refuse("committing %s timed out and your draft is gone, but %s could not be read to find out whether it landed (%v) — look at it in the web app before publishing again",
				draft.ID, out.TableID, liveErr)
		case unresolved || stagedUnresolved || liveCode != stagedCode:
			return refuse("committing %s timed out and your draft is gone, but %s does not hold the SQL that draft held — it was discarded or rejected rather than committed, or something else was committed over it.\n    Look at %s in the web app; `ronja pipeline push %s` stages a fresh draft from your file",
				draft.ID, out.TableID, out.TableID, path)
		}
	}
	// Recorded only now: everything above that could still refuse has refused,
	// so this is the first point at which the overwrite is known to have landed.
	out.OverwroteVersionID = overwroteVersionID
	out.Outcome = outcomePublished
	out.Detail = fmt.Sprintf("committed onto %s", out.TableID)
	if out.OverwroteVersionID != "" {
		out.Detail = fmt.Sprintf("committed onto %s, overwriting version %s that was published after your draft was created",
			out.TableID, out.OverwroteVersionID)
	}
	out.Cascade = folderDependents(codec, local, out.TableID)
	// ARMED ONLY BY A FILE THAT CARRIES A HEADER, which is the rule
	// wfdir.LockTable.MetaSHA256 states and which this command was quietly
	// breaking: it recorded the fingerprint for every table it published,
	// documenting file or not. The cost is not a wasted hash — it is a diff in a
	// COMMITTED file (`metaSHA256` per table in ronja.lock.json) appearing in
	// every existing pipeline folder on its next publish, for a feature nobody in
	// that folder has adopted, arming a drift leg that then pauses a later push
	// for prose that was never this folder's business.
	docs, _ := tabledocs.ParseHeader(local[path])
	documented := !docs.Empty()
	// AND DOCUMENTED IS NOT THE SAME QUESTION AS DOCUMENTED SUCCESSFULLY. The
	// line above reads the FILE; this one reads the ROW, and they disagree in
	// exactly one case, which is the one that matters: a push whose SQL landed
	// and whose prose was refused (pushOutcomeDocsFailed). That push withheld
	// recordSynced on purpose so the file stays re-pushable — and publish, which
	// runs in a different process and remembers nothing, used to bank BOTH the
	// content baseline and the documentation agreement anyway, from a header it
	// had only parsed. The file then hashes clean, the bare push skips it, the
	// meta leg reads driftNone, and the committed header and the row disagree for
	// ever. See docsLanded for how the row is asked under the join rule.
	//
	// Only meaningful where the row could be read; the liveErr branch below never
	// consults it.
	landed := docsLanded(docs, live)
	if documented && liveErr == nil && !landed {
		fmt.Fprintf(os.Stderr, "  Note: %s was published, but %s does not hold the documentation this file declares — its prose never landed (a push can report that as `documentation NOT written`). The file is left as unsynced so the next `ronja pipeline push` sends it again.\n",
			path, out.TableID)
	}
	// Both fingerprints now describe the same thing, and it is the LIVE table:
	// the draft they were taken from does not exist any more. Best-effort — the
	// server-side change has already happened, and failing the command afterwards
	// would report something that did happen as something that did not.
	switch {
	case liveErr != nil:
		fmt.Fprintf(os.Stderr, "  Note: %s was published, but its baseline could not be refreshed (%v) — `ronja pipeline status` will show it as drifted until the next push.\n", path, liveErr)
		// The DOCUMENTATION fingerprint is CLEARED rather than left, and this is
		// the one branch where the two halves behave differently. The commit just
		// copied the draft's column rows onto the live table, so whatever was
		// recorded describes a state that has certainly moved — keeping it would
		// refuse the next documenting push and name a change this folder made
		// itself. Empty is "no answer", which the next push adopts from the row it
		// reads. (The SQL leg above is left alone deliberately: its note tells the
		// author status will show drift, and the code half is what --force exists
		// for. Prose has no such recovery worth spending, because it is re-derived
		// from the file on the next push anyway.)
		if documented {
			recordMetaAgreement(f.live(inst), path, out.TableID, "")
		}
	case unresolved:
		fmt.Fprintf(os.Stderr, "  Note: %s was published, but its stored SQL canonicalizes to something this client cannot read, so the baseline was left alone.\n", path)
		if documented && landed {
			recordMetaAgreement(f.live(inst), path, out.TableID, metaOf(live))
		}
	default:
		recordLiveAgreement(f.live(inst), path, out.TableID, liveCode)
		// The third leg's agreement, at the third moment it is true: the commit
		// copied the draft's prose onto the live row, and this read is of that
		// row. Without it the next documenting push would report the folder's own
		// publish as somebody else's edit. `live` is a GetTable answer, the only
		// response carrying the column catalog — see metaOf.
		//
		// AND ONLY WHEN IT REALLY IS TRUE. An agreement recorded here for prose
		// the row does not hold is the drift guard being disarmed by the very
		// command it is meant to protect against.
		if documented && landed {
			recordMetaAgreement(f.live(inst), path, out.TableID, metaOf(live))
		}
		// The SQL leg above is recorded either way — it DID land, and leg (a) has
		// to keep describing the row this folder just published. What is withheld
		// when the prose did not land is the CONTENT baseline, because that is
		// what the bare-push selector reads: leaving it as push left it is the
		// whole of "the file is still unsynced, send it again". The URL is
		// reported regardless; the table exists and the reader wants the link.
		if !documented || landed {
			recordSynced(inst, path, liveCode)
		}
		out.URL = live.URL
	}
	return out
}

// orderPublishTargets puts the drafts to publish in dependency order.
//
// Degrades rather than refuses, twice over, because publish is not the command
// that should be teaching anyone about their ref graph: a folder whose SQL
// contains a cycle keeps the caller's order (push already refuses that, and the
// drafts here are built and committable), and a target the graph does not cover
// — a file that has left the folder but still has a staged draft — is appended
// rather than DROPPED. Dropping it would silently publish fewer drafts than the
// caller asked for.
func orderPublishTargets(c pipelineCodec, local map[string]string, targets []string) []string {
	ordered, err := topoOrder(c, local, targets)
	if err != nil {
		return targets
	}
	covered := make(map[string]bool, len(ordered))
	for _, path := range ordered {
		covered[path] = true
	}
	for _, path := range targets {
		if !covered[path] {
			ordered = append(ordered, path)
		}
	}
	return ordered
}

// staleInputs names the tables this file reads from whose row was updated after
// the draft's was — a HEURISTIC for "this draft was built from older upstream
// data", and worded as one wherever it is reported.
//
// It is a heuristic because neither timestamp is a materialization time. The API
// exposes none: `updatedAt` moves for a rename or a description edit as readily
// as for a rebuild, and the draft's own moves on the PUT this loop makes. So it
// over-reports (a renamed upstream) and under-reports (an upstream rebuilt with
// no row write). Inventing a materialization time the API does not serve would
// be worse — a number that looks authoritative and is not.
//
// Only the ones the feature's own listing knows about, which is the folder-local
// answer and is deliberately as far as v1 goes: an input from another feature
// would cost a GET each, and the case that actually bites is an upstream in the
// same pipeline being published while a downstream draft sits staged.
func staleInputs(code string, byID map[string]*api.TableListItem, builtAt time.Time) []string {
	if byID == nil || builtAt.IsZero() {
		return nil
	}
	var out []string
	for _, id := range tablerefs.DeriveInputModels(code) {
		item, known := byID[id]
		if !known {
			continue
		}
		if item.UpdatedAt.After(builtAt) {
			out = append(out, fmt.Sprintf("%s (%s)", item.Name, id))
		}
	}
	sort.Strings(out)
	return out
}

// folderDependents counts the files IN THIS FOLDER that read from one table
// DIRECTLY. Not the transitive closure: a table two hops downstream rebuilds
// too, but counting it here would quietly turn a number the reader can check by
// grepping the folder into one they cannot.
//
// Folder-local on purpose. The real cascade is server-side and wider than this,
// but its size is not something the CLI can know — GetDependents is not exposed
// over HTTP — and its conditions (a zero-partition parent defers the cascade
// entirely, a mid-build dependent is skipped, a dependent with a dangling input
// is parked) mean a confident tenant-wide N would be wrong in ways nobody could
// check. What this folder holds is a number the reader can verify by looking.
func folderDependents(c pipelineCodec, local map[string]string, tableID string) int {
	self := ""
	for path, id := range c.f.Binding.Tables {
		if id == tableID {
			self = path
		}
	}
	count := 0
	for path, code := range local {
		if path == self {
			continue
		}
		reads := false
		for _, id := range tablerefs.DeriveInputModels(code) {
			if id == tableID {
				reads = true
				break
			}
		}
		// The other spelling: a sibling ref by STEM names no id at all, so the
		// scan above cannot see it. Only meaningful when the table is one this
		// folder builds — a stem names a file, and a table with no file has none.
		if !reads && self != "" {
			for _, dep := range c.folderUpstreams(code) {
				if dep == self {
					reads = true
					break
				}
			}
		}
		if reads {
			count++
		}
	}
	return count
}

func printPipelinePublishReport(r *pipelinePublishResult) {
	out := os.Stdout
	if r.Nothing {
		fmt.Fprintf(out, "  Nothing to publish — no file in this folder has a staged draft.\n")
		fmt.Fprintf(out, "  Run `ronja pipeline push` first. If you know a draft IS open — staged from the\n")
		fmt.Fprintf(out, "  web builder, or from a checkout this folder's baseline lost track of — name the\n")
		fmt.Fprintf(out, "  file (`ronja pipeline publish orders.sql`); that asks the server rather than\n")
		fmt.Fprintf(out, "  the baseline.\n")
		return
	}
	for _, file := range r.Files {
		switch file.Outcome {
		case outcomeSubmittedForReview:
			fmt.Fprintf(out, "\n  %s — submitted for review\n", file.Path)
		case pushOutcomeRefused:
			fmt.Fprintf(out, "\n  %s — not published\n", file.Path)
		case outcomeConflict:
			fmt.Fprintf(out, "\n  %s — not published (somebody committed first)\n", file.Path)
		default:
			fmt.Fprintf(out, "\n  %s — published\n", file.Path)
		}
		if file.Detail != "" {
			fmt.Fprintf(out, "    %s\n", file.Detail)
		}
		if file.Error != "" {
			fmt.Fprintf(out, "    %s\n", file.Error)
		}
		for _, w := range file.Warnings {
			fmt.Fprintf(out, "    Warning: %s\n", w)
		}
		if file.Outcome == outcomePublished && file.Cascade > 0 {
			fmt.Fprintf(out, "    %d table(s) in this folder read this one directly and will rebuild automatically.\n", file.Cascade)
		}
		printResourceURL(out, reportKeyWidth, file.URL)
	}
	for _, metric := range r.Metrics {
		switch metric.Outcome {
		case outcomeSubmittedForReview:
			fmt.Fprintf(out, "\n  %s — submitted for review\n", metric.Path)
		case pushOutcomeRefused:
			fmt.Fprintf(out, "\n  %s — not published\n", metric.Path)
		case outcomeConflict:
			fmt.Fprintf(out, "\n  %s — not published (somebody committed first)\n", metric.Path)
		default:
			fmt.Fprintf(out, "\n  %s — published\n", metric.Path)
		}
		if metric.Detail != "" {
			fmt.Fprintf(out, "    %s\n", metric.Detail)
		}
		if metric.Error != "" {
			fmt.Fprintf(out, "    %s\n", metric.Error)
		}
		for _, w := range metric.Warnings {
			fmt.Fprintf(out, "    Warning: %s\n", w)
		}
		printResourceURL(out, reportKeyWidth, metric.URL)
	}
	if r.Target != "" {
		fmt.Fprintf(out, "\n  Target:   %s\n", r.Target)
	}
	if r.Error != "" {
		fmt.Fprintf(out, "  %s.\n", r.Error)
	}
}
