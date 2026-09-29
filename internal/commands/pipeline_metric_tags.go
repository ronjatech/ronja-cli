package commands

import (
	"context"
	"fmt"
	"os"
	"strings"

	"github.com/ronjatech/ronja-cli/internal/api"
	"github.com/ronjatech/ronja-cli/internal/metricfile"
	"github.com/ronjatech/ronja-cli/internal/wfdir"
)

// The metric loop's TAG half: `"tags": ["Finance", "Sales"]` in a metric file.
//
// TAGS ARE NOT DRAFTED, and everything in this file follows from that. A tag
// sits on the LIVE metric, so writing one needs no checkout, no build and no
// publish — one PATCH to /tag/of/table/<metric id>, which adds and removes
// atomically under the server's per-target lock and keeps every tag it does not
// name. That is why the tag half is its own pass with its own outcome rather
// than a field folded into the recipe: a tag-only edit must never check out,
// stage or build a draft, and a recipe refusal, a failed build or an up-to-date
// definition must never stop a tag edit from landing.
//
// THE FOLDER ASSERTS ITS NAMES, FOLLOWING TAG IDS (Adam, 2026-09-28):
//
//   - A name the file lists is put on the metric if it is not there — including
//     when a colleague REMOVED it in the web app. The folder wins; the push says
//     what it did.
//   - A tag a colleague ADDED in the web app is never touched: the folder only
//     ever removes a tag it recorded putting there (tagsApplied), and removes it
//     by ID.
//   - A tag an admin RENAMED in the web app is kept, never reverted and never
//     re-minted beside the new name: the recorded id still on the metric
//     satisfies the file's old name. Status says "renamed in the web app: Fin →
//     Finance — update the file".
//   - With no record at all — a legacy folder, a rebind, a folder's first `tags`
//     key — the reconcile is ADDITIVE ONLY, because nothing here can tell the
//     folder's tags from anybody else's.
//
// ONE PREDICATE, planMetricTags, and both `push` and `status` call it. Two
// copies of "what would a push change" is how status comes to promise a push
// that then does nothing, or the reverse.
//
// Every name comparison is the server's, lower(trim()) — metricfile.FoldTagName.
//
// ⚠️ NEVER KEY LOGIC ON THE RESPONSE'S `created`. It is a hint for the one line
// that lets an author see a typo land in the catalog; the record is built from
// the response's FINAL tag list, which is always true.

// Per-metric tag outcomes, the --json `tags.outcome` values.
const (
	tagOutcomeApplied         = "applied"
	tagOutcomeUnchanged       = "unchanged"
	tagOutcomeSkippedArchived = "skipped_archived"
	// tagOutcomeRefused is the 20-tag cap (the server's `tag_limit`) — the ONLY
	// refusal recorded, because it is the only one that answers the same way for
	// every caller: RECORDED, said once, and never a reason for a non-zero exit.
	// It is not purely the file's fault (tags added in the web app count too),
	// so a recorded cap stays until the file's tags change or --retry-tags.
	tagOutcomeRefused = "refused"
	// tagOutcomeFailed is everything else: a failure that says nothing lasting
	// about the FILE — a 5xx or a dropped connection, a role, visibility or
	// scope 403 (a fact about this caller, and a colleague's push may be
	// allowed), a 404/405 (an instance that does not serve the route yet), a
	// race's 400, a rename race that repeated on the re-planned PATCH. NEVER
	// recorded, so the next push tries again on its own; and a PROBLEM for the
	// push, which exits non-zero and is not up to date, so neither CI nor `sync
	// apply` reads an unapplied tag change as deployed.
	tagOutcomeFailed = "failed"
)

// Status tag states, the --json `tags.state` values of `pipeline status`.
const (
	tagStateInPlace         = "in_place"
	tagStatePending         = "pending"
	tagStateSkippedArchived = "skipped_archived"
	tagStateRefused         = "refused"
	// tagStateUnread is a read that failed — a 5xx, a dropped connection, a
	// token whose scope does not cover tags, a metric this caller cannot see,
	// an instance with no tag route. "I could not look" is not "nothing to do",
	// so the tree verdict counts it as unchecked.
	tagStateUnread = "unread"
)

// olderServerTagReason is what a 404 or 405 from the tag route means: the
// organization's Ronja does not serve it yet — a CLI updated ahead of its
// organization's deploy, or an instance mid-rollout. Never recorded: the
// deploy that fixes it changes nothing in the folder, so a recorded refusal
// would outlive it on every colleague's push.
const olderServerTagReason = "this organization's Ronja doesn't have tag support yet — push again after it's updated"

// pipelineMetricTagResult is what a push did about one metric file's tags. Nil
// on a file with no `tags` key, which does not manage them.
type pipelineMetricTagResult struct {
	Outcome string `json:"outcome"`
	Reason  string `json:"reason,omitempty"`
	// Added and Removed are tag NAMES, as the server spells them now.
	Added   []string `json:"added,omitempty"`
	Removed []string `json:"removed,omitempty"`
	// Created are the added names that did not exist in the organization's
	// catalog before — a HINT, reported so a typo is seen the first time it
	// lands, and never read by anything else.
	Created []string `json:"created,omitempty"`
	// AlreadyGone are recorded tags this push meant to remove that were no
	// longer on the metric when the PATCH ran — somebody got there first.
	AlreadyGone []string `json:"unmatchedRemoves,omitempty"`
	// Renamed are this folder's tags an admin renamed in the web app, as
	// "Fin → Finance": kept, and worth updating the file for.
	Renamed []string `json:"renamed,omitempty"`
	// cause is the error behind a `failed` outcome when it was a failure to get
	// an answer (a 5xx, a timeout, a dropped connection) — a write that may
	// have landed, which `sync apply` reports as unknown rather than refused.
	cause error
}

// pipelineMetricTagStatus is `pipeline status`'s answer about one metric file's
// tags. Nil on a file with no `tags` key.
type pipelineMetricTagStatus struct {
	State   string   `json:"state"`
	Add     []string `json:"add,omitempty"`
	Remove  []string `json:"remove,omitempty"`
	Renamed []string `json:"renamed,omitempty"`
	Reason  string   `json:"reason,omitempty"`
}

// metricTagPlan is what a push would send to make the metric hold what the
// file asserts.
type metricTagPlan struct {
	// Adds are FILE names no live tag matches and no recorded id covers.
	Adds []string
	// RemoveIDs are RECORDED tags still on the metric whose recorded name and
	// current name are both absent from the file.
	RemoveIDs []string
	// removeNames maps each RemoveIDs entry to the name the metric shows now,
	// for the report.
	removeNames map[string]string
	// Renamed are recorded tags still on the metric under a different name
	// while the file still says the old one, as "Old → New".
	Renamed []string
}

func (p metricTagPlan) pending() bool { return len(p.Adds) > 0 || len(p.RemoveIDs) > 0 }

// removeNamesList is the plan's removals as current names, in plan order.
func (p metricTagPlan) removeNamesList() []string {
	out := make([]string, 0, len(p.RemoveIDs))
	for _, id := range p.RemoveIDs {
		out = append(out, p.removeNames[id])
	}
	return out
}

// planMetricTags is THE predicate: given what the file lists, what the metric
// holds now and what this folder recorded applying, what would a push change?
//
//   - A file name is SATISFIED by a live tag of the same folded name, or by a
//     recorded entry of the same folded name whose id is still on the metric
//     (the tag was renamed since). Otherwise it is an ADD — which is also how a
//     tag a colleague removed in the web app comes back.
//   - A recorded id is a REMOVE when it is still on the metric and NO recorded
//     entry for that id has either its recorded name or the name the tag has
//     now in the file. Asked of every entry for the id, not entry by entry: the
//     record can hold two entries for one id (a file that listed a renamed
//     tag's old AND new name), and dropping one of those names must not remove
//     a tag the other still asserts — in whichever order they were recorded.
//     A recorded id no longer on the metric needs nothing: it is gone, and if
//     the file still wants the name the add above puts it back.
//   - A live tag that is not recorded is never removed, whatever it is called.
//     With no record, nothing is removed at all.
func planMetricTags(fileTags []string, live []api.Tag, applied []wfdir.AppliedTag) metricTagPlan {
	liveByID := make(map[string]api.Tag, len(live))
	liveByName := make(map[string]api.Tag, len(live))
	for _, tag := range live {
		liveByID[tag.ID] = tag
		liveByName[metricfile.FoldTagName(tag.Name)] = tag
	}
	inFile := make(map[string]bool, len(fileTags))
	for _, name := range fileTags {
		inFile[metricfile.FoldTagName(name)] = true
	}

	plan := metricTagPlan{removeNames: map[string]string{}}
	for _, name := range fileTags {
		fold := metricfile.FoldTagName(name)
		if _, on := liveByName[fold]; on {
			continue
		}
		if coveringRecord(applied, liveByID, fold) != nil {
			continue
		}
		plan.Adds = append(plan.Adds, name)
	}
	// asserted is each recorded id still on the metric that the file names by
	// ANY of its entries — under the name it was recorded as, or (the author
	// updated the file after a rename, or the tag was renamed TO a name the
	// file lists) the name it has now.
	asserted := make(map[string]bool, len(applied))
	for _, entry := range applied {
		tag, on := liveByID[entry.ID]
		if !on {
			continue
		}
		recordedFold, currentFold := metricfile.FoldTagName(entry.Name), metricfile.FoldTagName(tag.Name)
		if inFile[recordedFold] || inFile[currentFold] {
			asserted[entry.ID] = true
		}
		if inFile[recordedFold] && currentFold != recordedFold {
			plan.Renamed = append(plan.Renamed, entry.Name+" → "+tag.Name)
		}
	}
	for _, entry := range applied {
		tag, on := liveByID[entry.ID]
		if !on || asserted[entry.ID] {
			continue
		}
		if _, planned := plan.removeNames[entry.ID]; planned {
			continue
		}
		plan.RemoveIDs = append(plan.RemoveIDs, entry.ID)
		plan.removeNames[entry.ID] = tag.Name
	}
	return plan
}

// coveringRecord is the recorded entry, if any, that satisfies a file name by
// id: recorded under that folded name, and still on the metric.
func coveringRecord(applied []wfdir.AppliedTag, liveByID map[string]api.Tag, fold string) *wfdir.AppliedTag {
	for i := range applied {
		if metricfile.FoldTagName(applied[i].Name) != fold {
			continue
		}
		if _, on := liveByID[applied[i].ID]; on {
			return &applied[i]
		}
	}
	return nil
}

// appliedAfter is the record a reconcile leaves: one entry per FILE name that
// the metric now holds, against the id that holds it.
//
// A name matched by a live tag directly records that tag, spelled as the server
// spells it. A name satisfied only through a renamed tag keeps its PRIOR entry —
// the old name — because that entry is what tells the next run the file's name
// is already satisfied; recording the new name there would make the next push
// re-mint the old one. A name the metric does not hold (a concurrent removal
// after the PATCH) is left out, and the next push adds it.
func appliedAfter(fileTags []string, live []api.Tag, prior []wfdir.AppliedTag) []wfdir.AppliedTag {
	liveByID := make(map[string]api.Tag, len(live))
	liveByName := make(map[string]api.Tag, len(live))
	for _, tag := range live {
		liveByID[tag.ID] = tag
		liveByName[metricfile.FoldTagName(tag.Name)] = tag
	}
	out := make([]wfdir.AppliedTag, 0, len(fileTags))
	for _, name := range fileTags {
		fold := metricfile.FoldTagName(name)
		if tag, on := liveByName[fold]; on {
			out = append(out, wfdir.AppliedTag{ID: tag.ID, Name: tag.Name})
			continue
		}
		if entry := coveringRecord(prior, liveByID, fold); entry != nil {
			out = append(out, wfdir.AppliedTag{ID: entry.ID, Name: entry.Name})
		}
	}
	return out
}

// tagErrorKind is what a tag route's error means for the record.
type tagErrorKind int

const (
	// tagErrorFailed says nothing lasting about the file: reported, never
	// recorded, and retried by the next push.
	tagErrorFailed tagErrorKind = iota
	// tagErrorRefused is the 20-tag cap: recorded, so it is said once and not
	// re-sent on every push.
	tagErrorRefused
	// tagErrorOverlap is the server's tag_overlap 400 — on a body this loop
	// planned from a read, a tag renamed between that read and the PATCH. Worth
	// one re-read and re-plan; never recorded.
	tagErrorOverlap
)

// classifyTagError turns a tag route's error into a reason and a kind. `verb`
// is the action the request needed ("read" for the GET, "write" for the
// PATCH): it names the scope denial when the server's message does not, and it
// tells the handler's two 403s apart.
//
// ONE THING IS RECORDED: the 400 coded `tag_limit`, the 20-tag cap — the only
// refusal that answers the same way for every caller. It is keyed on the code
// alone, never on the status: every other 400 (a race's "one or more tags not
// found", an older server's uncoded cap) could answer differently on the next
// push. It is not purely about the file — tags added in the web app count
// toward it, including a colleague's concurrent add that tipped it over under
// the server's lock — which is why --retry-tags exists.
//
// Everything else is `failed`, because it is a fact about the caller or the
// deploy, and recording it in a COMMITTED lock would stop every colleague's
// push from ever trying:
//
//   - a 403 — the token's scope (code insufficient_scope, or the middleware's
//     prose from an instance that predates the code), the handler's role gate,
//     or its visibility gate. The handler refuses both with a bare 403, and the
//     GET and the PATCH share the visibility gate, so a 403 on the READ is
//     visibility and one on the WRITE, after the read was answered, is the
//     role;
//   - a 404 or 405 — an instance that does not serve the route yet;
//   - any other 400, a 5xx, a dropped connection, a timeout, a 401.
func classifyTagError(err error, verb string) (reason string, kind tagErrorKind) {
	status, code := api.StatusOf(err), api.WireCodeOf(err)
	switch {
	case status == 400 && code == api.TagLimitCode:
		return fmt.Sprintf("the server refused the metric's tags: %v", err), tagErrorRefused
	case status == 400 && code == api.TagOverlapCode:
		return fmt.Sprintf("a tag was renamed in the web app while this push reconciled the metric's tags (%v) — push again", err), tagErrorOverlap
	case status == 403 && (code == api.InsufficientScopeCode || scopeDenied(err)):
		return fmt.Sprintf("this token's scope doesn't allow %s on tags — use a token with data:write", scopeDeniedAction(err, verb)), tagErrorFailed
	case status == 403 && verb == "read":
		return "you can't see this metric, so its tags can't be read or changed", tagErrorFailed
	case status == 403:
		return "your role can't change tags — it takes the user role or above", tagErrorFailed
	case status == 404 || status == 405:
		return olderServerTagReason, tagErrorFailed
	default:
		return fmt.Sprintf("the metric's tags could not be reconciled: %v", err), tagErrorFailed
	}
}

// scopeDeniedAction is the action a scope 403 names — `token scope "data" does
// not permit write` — echoed rather than assumed, or `fallback` when the
// message names none (the route-not-reachable form).
func scopeDeniedAction(err error, fallback string) string {
	if _, action, ok := strings.Cut(api.CodeOf(err), " does not permit "); ok && strings.TrimSpace(action) != "" {
		return strings.TrimSpace(action)
	}
	return fallback
}

// tagsNotAppliedError is a push whose problems were ONLY in metric files' tag
// halves — and, possibly, in its health checks. Typed on the checks precedent
// (checksNotAppliedError), so `sync apply` can publish the folder's SQL past
// it: tags are not drafted, so an unapplied tag change must not hold a table
// deploy hostage — least of all when the cause is an organization whose Ronja
// has no tag route yet. It is still an error: the command exits non-zero and
// says why, and the next push retries.
type tagsNotAppliedError struct {
	msg string
	// problems names each metric file whose tag half did not apply, and why.
	problems []string
	// cause is non-nil when a problem was a failure to get an answer.
	cause error
	// checks is the push's checks leg when that failed too, so `sync apply`
	// can fold it into the folder's report.
	checks *checksNotAppliedError
}

func (e *tagsNotAppliedError) Error() string { return e.msg }
func (e *tagsNotAppliedError) Unwrap() error { return e.cause }

// newTagsNotApplied reads the failed tag halves out of a push's metric results.
func newTagsNotApplied(msg string, metrics []pipelineMetricFileResult, checks *checksNotAppliedError) *tagsNotAppliedError {
	out := &tagsNotAppliedError{msg: msg, checks: checks}
	for _, m := range metrics {
		if m.Tags == nil || m.Tags.Outcome != tagOutcomeFailed {
			continue
		}
		if out.cause == nil {
			out.cause = m.Tags.cause
		}
		out.problems = append(out.problems, fmt.Sprintf("%s tags not applied (%s)", m.Path, m.Tags.Reason))
	}
	return out
}

// tagsNotAppliedAfter folds a push's tag leg into the folder's error after the
// publish. checksErr is what checksNotAppliedAfter returned for the checks
// legs — nil, or a *checksNotAppliedError whose message already names the
// published tables. nil tags leaves checksErr as it is.
func tagsNotAppliedAfter(detail string, published []string, tags *tagsNotAppliedError, checksErr error) error {
	if tags == nil {
		return checksErr
	}
	out := &tagsNotAppliedError{problems: tags.problems, cause: tags.cause}
	if checksErr != nil {
		// The checks message already carries the detail and the published
		// tables; the tag problems ride after it.
		if c, ok := checksErr.(*checksNotAppliedError); ok {
			out.checks = c
			if out.cause == nil {
				out.cause = c.cause
			}
		}
		out.msg = fmt.Sprintf("%s; and the tags of %d metric file(s) did not apply: %s", checksErr.Error(), len(tags.problems), strings.Join(tags.problems, "; "))
		return out
	}
	if len(published) > 0 {
		detail += " (" + strings.Join(published, ", ") + ")"
	}
	out.msg = fmt.Sprintf("%s, but the tags of %d metric file(s) did not apply: %s", detail, len(tags.problems), strings.Join(tags.problems, "; "))
	return out
}

// reconcileMetricTags is the push's tag half for one metric file, run once the
// metric is known to exist and to BE a metric: immediately after a create, or
// after the existing row's read and kind check — and before every recipe step
// after the draft read (the drift refusals, the write, the build, the
// up-to-date return), so none of those can skip it. A failed draft read, which
// comes first because resolving the draft is part of the metric's identity, is
// the one recipe step that still stops it.
//
// `row` is the live metric as just read — the ADOPTED row on a create refused
// as name_taken, which may be archived or hidden like any other — and nil only
// straight after a create that succeeded (a metric that did not exist a moment
// ago is neither).
func reconcileMetricTags(ctx context.Context, client *api.Client, live liveHashes,
	file pipelineMetricFile, metricID string, row *api.Table, opts pipelinePushOptions) *pipelineMetricTagResult {

	if file.File.Tags == nil || metricID == "" {
		return nil
	}
	fileTags := *file.File.Tags
	if row != nil && (row.Archived || row.Hidden) {
		fmt.Fprintf(os.Stderr, "  Note: %s — the metric is archived or hidden, so its tags were left alone (an admin can tag it in the web app).\n", file.Path)
		return &pipelineMetricTagResult{Outcome: tagOutcomeSkippedArchived,
			Reason: "the metric is archived or hidden"}
	}
	applied, refused := live.metricTags(file.Path, metricID)
	hash := metricfile.TagsFingerprint(fileTags)
	if refused != nil && refused.FileTagsHash == hash && !opts.RetryTags {
		// Said, and not re-sent: the file says what it said when the server
		// refused it, and the server has no reason to answer differently. This is
		// what keeps `sync apply` from looping on a permanent refusal.
		return &pipelineMetricTagResult{Outcome: tagOutcomeRefused,
			Reason: refused.Reason + " (recorded at an earlier push — edit the file's tags, or push --retry-tags, to try again)"}
	}
	report := func(err error, reason string, kind tagErrorKind) *pipelineMetricTagResult {
		if kind == tagErrorRefused {
			live.setMetricTagsRefused(file.Path, metricID, &wfdir.TagsRefusal{FileTagsHash: hash, Reason: reason})
			fmt.Fprintf(os.Stderr, "  Note: %s — tags NOT applied: %s\n", file.Path, reason)
			return &pipelineMetricTagResult{Outcome: tagOutcomeRefused, Reason: reason}
		}
		fmt.Fprintf(os.Stderr, "  Note: %s — tags NOT applied (not recorded — the next push tries again): %s\n", file.Path, reason)
		return &pipelineMetricTagResult{Outcome: tagOutcomeFailed, Reason: reason, cause: noAnswer(err)}
	}
	if len(fileTags) == 0 && len(applied) == 0 {
		// `[]` with nothing recorded: there is nothing this folder could remove
		// and nothing it asserts, so no request is worth making.
		live.setMetricTagsRefused(file.Path, metricID, nil)
		return &pipelineMetricTagResult{Outcome: tagOutcomeUnchanged}
	}
	// A Ctrl-C stops the tag half where it stands, quietly: the request it
	// would send is aborted anyway, and one "tags NOT applied" note per
	// remaining metric describes the interrupt rather than the metrics.
	interrupted := func() *pipelineMetricTagResult {
		return &pipelineMetricTagResult{Outcome: tagOutcomeFailed, Reason: interruptedMessage}
	}

	// READ, PLAN, PATCH — and on tag_overlap, once more from a fresh read. The
	// overlap is the server refusing an add that names a tag this PATCH removes
	// by id, which on a plan built from a read means an admin renamed the tag
	// in between: the re-read sees the new name, and the re-plan either keeps
	// the tag (the file now names it as it is called) or drops the add. ONE
	// re-plan: a race that repeats is `failed`, and the next push starts over.
	var plan metricTagPlan
	var result *api.PatchTagsResult
	for attempt := 0; ; attempt++ {
		if ctx.Err() != nil {
			return interrupted()
		}
		current, err := client.ListTableTags(ctx, metricID)
		if err != nil && ctx.Err() != nil {
			return interrupted()
		}
		if err != nil {
			reason, kind := classifyTagError(err, "read")
			return report(err, reason, kind)
		}
		plan = planMetricTags(fileTags, current, applied)
		if !plan.pending() {
			live.setMetricTagsApplied(file.Path, metricID, appliedAfter(fileTags, current, applied))
			live.setMetricTagsRefused(file.Path, metricID, nil)
			return &pipelineMetricTagResult{Outcome: tagOutcomeUnchanged, Renamed: plan.Renamed}
		}
		result, err = client.PatchTableTags(ctx, metricID, api.PatchTagsInput{Add: plan.Adds, RemoveIDs: plan.RemoveIDs})
		if err == nil {
			break
		}
		if ctx.Err() != nil {
			// Aborted mid-request: whether the merge landed is unknown, and the
			// record is left as it was, so the next push reads and re-plans.
			res := interrupted()
			res.cause = err
			return res
		}
		reason, kind := classifyTagError(err, "write")
		if kind == tagErrorOverlap {
			if attempt == 0 {
				continue
			}
			kind = tagErrorFailed
		}
		return report(err, reason, kind)
	}
	live.setMetricTagsApplied(file.Path, metricID, appliedAfter(fileTags, result.Tags, applied))
	live.setMetricTagsRefused(file.Path, metricID, nil)
	out := &pipelineMetricTagResult{
		Outcome: tagOutcomeApplied,
		Added:   result.Added,
		Removed: result.Removed,
		Created: result.Created,
		Renamed: plan.Renamed,
	}
	// Matched BY VALUE against what was sent: the server echoes each unmatched
	// id trimmed, and the ids this client sends are the server's own, untrimmed
	// only if the server ever issued one with padding.
	for _, id := range result.UnmatchedRemoves {
		name := plan.removeNames[id]
		if name == "" {
			name = id
		}
		out.AlreadyGone = append(out.AlreadyGone, name)
	}
	if len(out.Added) == 0 && len(out.Removed) == 0 {
		// Everything the plan asked for was already true by the time it landed —
		// a concurrent edit got there first. Nothing changed here.
		out.Outcome = tagOutcomeUnchanged
	}
	return out
}

// metricTagStatus is `pipeline status`'s tag half for one metric file, on the
// same predicate a push runs and without ever writing: a refusal is not
// recorded here, because status promises to change nothing.
//
// `row` is the live metric (nil for a file with no metric yet).
func metricTagStatus(ctx context.Context, client *api.Client, live liveHashes,
	file pipelineMetricFile, row *api.Table) (*pipelineMetricTagStatus, bool) {

	if file.File.Tags == nil {
		return nil, false
	}
	fileTags := *file.File.Tags
	if row == nil {
		// A push creates the metric and then puts every listed name on it.
		if len(fileTags) == 0 {
			return &pipelineMetricTagStatus{State: tagStateInPlace}, false
		}
		return &pipelineMetricTagStatus{State: tagStatePending, Add: fileTags}, true
	}
	if row.Archived || row.Hidden {
		return &pipelineMetricTagStatus{State: tagStateSkippedArchived,
			Reason: "the metric is archived or hidden"}, false
	}
	metricID := row.IdentityID()
	applied, refused := live.metricTags(file.Path, metricID)
	if refused != nil && refused.FileTagsHash == metricfile.TagsFingerprint(fileTags) {
		return &pipelineMetricTagStatus{State: tagStateRefused, Reason: refused.Reason}, false
	}
	if len(fileTags) == 0 && len(applied) == 0 {
		return &pipelineMetricTagStatus{State: tagStateInPlace}, false
	}
	if ctx.Err() != nil {
		return &pipelineMetricTagStatus{State: tagStateUnread, Reason: interruptedMessage}, false
	}
	current, err := client.ListTableTags(ctx, metricID)
	if err != nil {
		// The push's own classification, so the two keep one predicate. A read
		// is never refused — the cap answers only a write — so every failed
		// read is `unread`, which the tree verdict counts as unchecked.
		reason, _ := classifyTagError(err, "read")
		return &pipelineMetricTagStatus{State: tagStateUnread, Reason: reason}, false
	}
	plan := planMetricTags(fileTags, current, applied)
	if !plan.pending() {
		return &pipelineMetricTagStatus{State: tagStateInPlace, Renamed: plan.Renamed}, false
	}
	return &pipelineMetricTagStatus{State: tagStatePending, Add: plan.Adds,
		Remove: plan.removeNamesList(), Renamed: plan.Renamed}, true
}

// describeTagChange renders adds and removes as `+Sales −Operations`.
func describeTagChange(added, removed []string) string {
	parts := make([]string, 0, len(added)+len(removed))
	for _, name := range added {
		parts = append(parts, "+"+name)
	}
	for _, name := range removed {
		parts = append(parts, "−"+name)
	}
	return strings.Join(parts, " ")
}

// printMetricTagResult renders a push's tag outcome under its metric's block.
func printMetricTagResult(out *os.File, tags *pipelineMetricTagResult) {
	if tags == nil {
		return
	}
	switch tags.Outcome {
	case tagOutcomeApplied:
		fmt.Fprintf(out, "    Tags:   %s (on the live metric — tags are not drafted, so there is nothing to publish for them)\n",
			describeTagChange(tags.Added, tags.Removed))
	case tagOutcomeSkippedArchived:
		fmt.Fprintf(out, "    Tags:   not reconciled — %s (an admin can tag it in the web app)\n", tags.Reason)
	case tagOutcomeRefused:
		fmt.Fprintf(out, "    Tags:   NOT applied — %s\n", tags.Reason)
	case tagOutcomeFailed:
		fmt.Fprintf(out, "    Tags:   NOT applied — %s (not recorded: the next push tries again)\n", tags.Reason)
	}
	for _, name := range tags.Created {
		fmt.Fprintf(out, "    Note:   created tag %q — it did not exist in this organization, so check the spelling\n", name)
	}
	if len(tags.AlreadyGone) > 0 {
		fmt.Fprintf(out, "    Note:   already gone from the metric: %s\n", strings.Join(tags.AlreadyGone, ", "))
	}
	for _, rename := range tags.Renamed {
		fmt.Fprintf(out, "    Note:   renamed in the web app: %s — update the file\n", rename)
	}
}

// printMetricTagStatus renders status's tag answer under its metric's block.
func printMetricTagStatus(out *os.File, tags *pipelineMetricTagStatus) {
	if tags == nil {
		return
	}
	switch tags.State {
	case tagStateInPlace:
		fmt.Fprintf(out, "      tags:   the metric holds every tag this file lists\n")
	case tagStatePending:
		fmt.Fprintf(out, "      tags:   a push would change the metric's tags: %s\n", describeTagChange(tags.Add, tags.Remove))
	case tagStateSkippedArchived:
		fmt.Fprintf(out, "      tags:   not reconciled — %s\n", tags.Reason)
	case tagStateRefused:
		fmt.Fprintf(out, "      tags:   refused: %s — edit the file's tags, or push --retry-tags, to try again\n", tags.Reason)
	case tagStateUnread:
		fmt.Fprintf(out, "      tags:   not checked (%s)\n", tags.Reason)
	}
	for _, rename := range tags.Renamed {
		fmt.Fprintf(out, "      tags:   renamed in the web app: %s — update the file\n", rename)
	}
}
