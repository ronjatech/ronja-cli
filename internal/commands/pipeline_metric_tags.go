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

// The metric loop's tag half. Tags are NOT drafted: one PATCH on the live
// metric, a pass independent of the recipe in both directions. The rules (the
// folder asserts its names, following recorded tag ids) are in cli/README.md →
// "Tags — `"tags"` in the metric file"; push and status share planMetricTags.
//
// ⚠️ Never key logic on the response's `created`, a hint for the typo note; the
// record is built from the response's final tag list.

// Per-metric tag outcomes, the --json `tags.outcome` values.
const (
	tagOutcomeApplied         = "applied"
	tagOutcomeUnchanged       = "unchanged"
	tagOutcomeSkippedArchived = "skipped_archived"
	// tagOutcomeRefused is the 20-tag cap (`tag_limit`), the ONLY refusal
	// recorded, since it answers the same for every caller; never a non-zero exit.
	tagOutcomeRefused = "refused"
	// tagOutcomeFailed says nothing lasting about the FILE, so it is never
	// recorded (a colleague's push may succeed); it makes the push exit non-zero
	// so neither CI nor `sync apply` reads the tag change as deployed.
	tagOutcomeFailed = "failed"
)

// Status tag states, the --json `tags.state` values of `pipeline status`.
const (
	tagStateInPlace         = "in_place"
	tagStatePending         = "pending"
	tagStateSkippedArchived = "skipped_archived"
	tagStateRefused         = "refused"
	// tagStateUnread is a failed read: "could not look" is not "nothing to
	// do", so the tree verdict counts it as unchecked.
	tagStateUnread = "unread"
)

// olderServerTagReason answers a 404 or 405 from the tag route. Never
// recorded: the deploy that fixes it changes nothing in the folder.
const olderServerTagReason = "this organization's Ronja doesn't have tag support yet — push again after it's updated"

// pipelineMetricTagResult is nil on a file with no `tags` key.
type pipelineMetricTagResult struct {
	Outcome string `json:"outcome"`
	Reason  string `json:"reason,omitempty"`
	// Added and Removed are tag NAMES, as the server spells them now.
	Added   []string `json:"added,omitempty"`
	Removed []string `json:"removed,omitempty"`
	// Created is a HINT for the typo note, never read by anything else.
	Created     []string `json:"created,omitempty"`
	AlreadyGone []string `json:"unmatchedRemoves,omitempty"`
	Renamed     []string `json:"renamed,omitempty"`
	// cause is set when a `failed` write got no answer and may have landed,
	// which `sync apply` reports as unknown rather than refused.
	cause error
}

type pipelineMetricTagStatus struct {
	State   string   `json:"state"`
	Add     []string `json:"add,omitempty"`
	Remove  []string `json:"remove,omitempty"`
	Renamed []string `json:"renamed,omitempty"`
	Reason  string   `json:"reason,omitempty"`
}

type metricTagPlan struct {
	// Adds are FILE names no live tag matches and no recorded id covers.
	Adds []string
	// RemoveIDs are RECORDED tags still on the metric whose recorded name and
	// current name are both absent from the file.
	RemoveIDs   []string
	removeNames map[string]string
	Renamed     []string
}

func (p metricTagPlan) pending() bool { return len(p.Adds) > 0 || len(p.RemoveIDs) > 0 }

func (p metricTagPlan) removeNamesList() []string {
	out := make([]string, 0, len(p.RemoveIDs))
	for _, id := range p.RemoveIDs {
		out = append(out, p.removeNames[id])
	}
	return out
}

// planMetricTags is the one predicate push and status share. A recorded id is
// removed only when NO recorded entry for it is named in the file, by recorded
// or current name — asked across every entry, since one id can be recorded
// twice (a renamed tag's old and new name). An unrecorded tag is never removed.
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

// appliedAfter keeps a name satisfied only through a renamed tag under its
// PRIOR (old) entry: recording the new name would make the next push re-mint
// the old one.
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

type tagErrorKind int

const (
	tagErrorFailed tagErrorKind = iota
	tagErrorRefused
	// tagErrorOverlap means a tag was renamed between the read and the PATCH:
	// worth one re-read and re-plan, never recorded.
	tagErrorOverlap
)

// classifyTagError records ONLY the 400 coded `tag_limit`, keyed on the code
// alone: anything else is a fact about the caller or the deploy, and recording
// it in a COMMITTED lock would stop every colleague's push from trying. The
// handler's role and visibility gates both answer a bare 403, and GET and PATCH
// share the visibility gate, so a 403 on "read" is visibility and on "write" is
// the role.
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

// scopeDeniedAction echoes the action a scope 403 names (`token scope "data"
// does not permit write`), or fallback when the message names none.
func scopeDeniedAction(err error, fallback string) string {
	if _, action, ok := strings.Cut(api.CodeOf(err), " does not permit "); ok && strings.TrimSpace(action) != "" {
		return strings.TrimSpace(action)
	}
	return fallback
}

// tagsNotAppliedError is a push whose problems were ONLY in tag halves (and
// possibly health checks). Typed like checksNotAppliedError so `sync apply`
// publishes the folder's SQL past it: undrafted tags must not hold a table
// deploy hostage. Still an error: the command exits non-zero.
type tagsNotAppliedError struct {
	msg      string
	problems []string
	cause    error
	checks   *checksNotAppliedError
}

func (e *tagsNotAppliedError) Error() string { return e.msg }
func (e *tagsNotAppliedError) Unwrap() error { return e.cause }

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

// tagsNotAppliedAfter takes checksErr from checksNotAppliedAfter: nil, or a
// *checksNotAppliedError whose message already names the published tables.
func tagsNotAppliedAfter(detail string, published []string, tags *tagsNotAppliedError, checksErr error) error {
	if tags == nil {
		return checksErr
	}
	out := &tagsNotAppliedError{problems: tags.problems, cause: tags.cause}
	if checksErr != nil {
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

// reconcileMetricTags must run once the row is known to BE a metric and before
// every recipe step after the draft read (drift refusals, write, build, the
// up-to-date return), so none of those can skip it. `row` is nil only straight
// after a successful create; an ADOPTED name_taken row may be archived or hidden.
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
		// Not re-sent while the file is unchanged: what keeps `sync apply`
		// from looping on a permanent refusal.
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
		live.setMetricTagsRefused(file.Path, metricID, nil)
		return &pipelineMetricTagResult{Outcome: tagOutcomeUnchanged}
	}
	// A Ctrl-C stops quietly: a "tags NOT applied" note per remaining metric
	// would describe the interrupt, not the metrics.
	interrupted := func() *pipelineMetricTagResult {
		return &pipelineMetricTagResult{Outcome: tagOutcomeFailed, Reason: interruptedMessage}
	}

	// On tag_overlap (an add naming a tag this PATCH removes by id, so a rename
	// raced the read), re-read and re-plan ONCE; a repeat is `failed`.
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
			// Aborted mid-request: whether it landed is unknown, so the record is
			// left as it was and the next push re-plans.
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
	// The server echoes each unmatched id trimmed; the ids sent are its own.
	for _, id := range result.UnmatchedRemoves {
		name := plan.removeNames[id]
		if name == "" {
			name = id
		}
		out.AlreadyGone = append(out.AlreadyGone, name)
	}
	if len(out.Added) == 0 && len(out.Removed) == 0 {
		// A concurrent edit got there first.
		out.Outcome = tagOutcomeUnchanged
	}
	return out
}

// metricTagStatus never writes, not even a refusal: status changes nothing.
// `row` is nil for a file with no metric yet.
func metricTagStatus(ctx context.Context, client *api.Client, live liveHashes,
	file pipelineMetricFile, row *api.Table) (*pipelineMetricTagStatus, bool) {

	if file.File.Tags == nil {
		return nil, false
	}
	fileTags := *file.File.Tags
	if row == nil {
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
		// The cap answers only a write, so every failed read is `unread`.
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
