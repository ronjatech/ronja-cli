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
	// Nothing reports a publish that found no staged draft at all, which is a
	// different answer from a publish that refused everything it found.
	Nothing bool   `json:"nothing"`
	Error   string `json:"error,omitempty"`
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

func runPipelinePublish(ctx context.Context, f *folder, args []string, noRequestReview, overwriteRemote bool) (*pipelinePublishResult, error) {
	client := newClient(f.Resolved.URL, f.Resolved.Token)
	result := &pipelinePublishResult{
		FeatureID: f.Binding.FeatureID,
		Target:    describeTarget(f.Resolved),
		Files:     []pipelinePublishedFile{},
	}

	if !f.Bound || len(f.Binding.Tables) == 0 {
		return nil, fmt.Errorf("nothing to publish — this folder has no tables on %s yet; run `ronja pipeline push` first",
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
	noteAliasReport(checkAliases(f.Manifest, f.selection(), f.Codec, local, folderStems(f.Kind, local), folderFieldRefs(f.Kind, local)))
	inst := pipelineBaseline(f)

	var targets []string
	if len(args) > 0 {
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

	if failed > 0 {
		result.Error = fmt.Sprintf("%d of %d draft(s) were not published", failed, len(result.Files))
		return result, fmt.Errorf("%s", result.Error)
	}
	return result, nil
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
	// Both fingerprints now describe the same thing, and it is the LIVE table:
	// the draft they were taken from does not exist any more. Best-effort — the
	// server-side change has already happened, and failing the command afterwards
	// would report something that did happen as something that did not.
	switch {
	case liveErr != nil:
		fmt.Fprintf(os.Stderr, "  Note: %s was published, but its baseline could not be refreshed (%v) — `ronja pipeline status` will show it as drifted until the next push.\n", path, liveErr)
	case unresolved:
		fmt.Fprintf(os.Stderr, "  Note: %s was published, but its stored SQL canonicalizes to something this client cannot read, so the baseline was left alone.\n", path)
	default:
		recordLiveAgreement(f.live(inst), path, out.TableID, liveCode)
		recordSynced(inst, path, liveCode)
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
	if r.Target != "" {
		fmt.Fprintf(out, "\n  Target:   %s\n", r.Target)
	}
	if r.Error != "" {
		fmt.Fprintf(out, "  %s.\n", r.Error)
	}
}
