package commands

import (
	"cmp"
	"context"
	"fmt"
	"strings"

	"github.com/ronjatech/ronja-cli/internal/api"
	"github.com/ronjatech/ronja-cli/internal/tabledocs"
	"github.com/ronjatech/ronja-cli/internal/wfdir"
)

// A dry run's comparison for the two kinds whose units are FILES: a pipeline
// folder's tables and metrics — with its health-check and docs files listed
// beside them — and an automation folder's automations.
//
// Each unit is asked the question its own push asks before its first write,
// through the push's own function — tableHeldLive, metricHeld,
// docsUnchangedSinceRecorded and docsHeldExactly, checksPlanWritesNothing,
// automationHeldLive — so a unit the apply would send nothing for is listed
// `unchanged`, and the preview cannot drift from the apply. Reads only, and
// nothing is recorded: the lock and the baseline are left exactly as they were.
//
// A unit stays `update` whenever the answer is not a clean "nothing": a read
// that failed, a draft that is open (the publish may commit it), a file that
// does not parse. Its Detail says which, so `update` is never a guess dressed
// as a comparison.

// comparePipelineUnits makes the comparison for a pipeline folder's units, and
// adds a unit for each docs file and checks file the push also visits.
func comparePipelineUnits(ctx context.Context, plan *plannedFolder) {
	line, f := &plan.line, plan.f
	local, _, err := readPipelineFiles(f.Root)
	if err != nil {
		return
	}
	client := newClient(f.Resolved.URL, f.Resolved.Token)
	codec := newPipelineCodec(f, local)
	// In memory only: pipelineBaseline fills in an empty baseline on the
	// folder, which a dry run never saves.
	inst := pipelineBaseline(f)
	store := f.live(inst)
	// The files a push visits: the rest it skips without a request, so they
	// are unchanged on the push's own terms (see runPipelinePush).
	changed := map[string]bool{}
	for _, path := range pipelineChangedTargets(f, local, inst.Hashes()) {
		changed[path] = true
	}
	metrics := map[string]pipelineMetricFile{}
	if files, err := readMetricFiles(f.Root); err == nil {
		for _, file := range resolveMetricFiles(f, codec, store, files) {
			metrics[file.Path] = file
		}
	}
	for i := range line.Units {
		u := &line.Units[i]
		if u.Action != string(applyActionUpdate) {
			continue
		}
		prior := u.Detail
		if content, ok := local[u.Path]; ok {
			compareTableUnit(ctx, client, codec, store, u, content, changed[u.Path] || inst.Tables[u.Path].DraftID != "")
		} else if file, ok := metrics[u.Path]; ok {
			compareMetricUnit(ctx, client, store, u, file)
		}
		keepPriorDetail(u, prior)
	}
	line.Units = append(line.Units, docsUnits(ctx, client, f, store)...)
	line.Units = append(line.Units, checksUnits(ctx, client, f, store, local)...)
}

// compareTableUnit answers one .sql file. visited is whether the push would
// look at it at all: a file unchanged since this checkout last pushed it, with
// no staged draft, is skipped without a request.
func compareTableUnit(ctx context.Context, client *api.Client, codec pipelineCodec, store liveHashes,
	u *syncApplyUnitReport, content string, visited bool) {
	if !visited {
		u.Action, u.Detail = string(applyActionUnchanged), "unchanged since this checkout last pushed it, so apply does not send it"
		return
	}
	draft, err := client.GetTableDraft(ctx, u.ID)
	if err != nil {
		u.Detail = fmt.Sprintf("not compared: your draft of %s could not be read (%v)", u.ID, err)
		return
	}
	if draft != nil {
		u.Detail = fmt.Sprintf("a draft of %s is open, and apply pushes into it and publishes it", u.ID)
		return
	}
	live, err := client.GetTable(ctx, u.ID)
	if err != nil {
		u.Detail = fmt.Sprintf("not compared: %s could not be read (%v)", u.ID, err)
		return
	}
	if tableHeldLive(codec, store, u.Path, content, live, nil) {
		u.Action, u.Detail = string(applyActionUnchanged), "the live table already holds this file"
		return
	}
	u.Detail = "the live table differs from this file"
	if docs, _ := tabledocs.ParseHeader(content); !docs.Empty() && docsLanded(docs, live) && !docsHeldExactly(docs, live) {
		u.Detail += unmeasuredColumnsNote
	}
}

// unmeasuredColumnsNote is why a documented file can never come out unchanged:
// prose for a column the table does not have yet is stored and never reported
// back, so nothing can show it landed.
const unmeasuredColumnsNote = " — it documents a column the table has not measured yet, and apply sends that prose again on every run until the table has the column"

// compareMetricUnit answers one metric file.
func compareMetricUnit(ctx context.Context, client *api.Client, store liveHashes, u *syncApplyUnitReport, file pipelineMetricFile) {
	if file.Problem != "" {
		return
	}
	draft, err := client.GetTableDraft(ctx, u.ID)
	if err != nil {
		u.Detail = fmt.Sprintf("not compared: your draft of %s could not be read (%v)", u.ID, err)
		return
	}
	if draft != nil {
		u.Detail = fmt.Sprintf("a draft of %s is open, and apply's publish commits it", u.ID)
		return
	}
	liveRow, err := client.GetTable(ctx, u.ID)
	if err != nil || liveRow.Kind != api.TableKindMetric {
		u.Detail = fmt.Sprintf("not compared: %s could not be read as a metric", u.ID)
		return
	}
	recordedID, liveBase, declared := store.metricSeen(file.Path)
	if recordedID != file.MetricID {
		liveBase, declared = "", ""
	}
	if metricDefinitionMoved(liveBase, liveRow, file) {
		refuseUnit(u, syncReasonNotDeployed, "the live definition changed since this folder last applied it, and apply refuses to overwrite it")
		return
	}
	tags, tagsPending := metricTagStatus(ctx, client, store, file, liveRow)
	if tags != nil && tags.State == tagStateUnread {
		u.Detail = "not compared: its tags could not be read (" + tags.Reason + ")"
		return
	}
	if metricHeld(file, declared, metricNeedsRename(file, liveRow, nil), nil, liveRow, true) && !tagsPending {
		u.Action, u.Detail = string(applyActionUnchanged), "the live metric already holds this file"
		return
	}
	u.Detail = "the live metric differs from this file"
}

// docsUnits is one unit per docs file (tables/<alias>.json), answered on
// pushOneTableDocs' terms.
func docsUnits(ctx context.Context, client *api.Client, f *folder, store liveHashes) []syncApplyUnitReport {
	files, err := readTableDocsSidecars(f.Root)
	if err != nil {
		return nil
	}
	var out []syncApplyUnitReport
	for _, file := range resolveTableDocsSidecars(f, files) {
		u := syncApplyUnitReport{Path: file.Path, ID: file.TableID, Action: string(applyActionUpdate)}
		switch {
		case file.Problem != "":
			refuseUnit(&u, syncReasonNotDeployed, file.Problem)
		case file.Docs.Empty():
			u.Action, u.Detail = string(applyActionUnchanged), "declares nothing, so nothing is sent"
		default:
			row, err := client.GetTable(ctx, file.TableID)
			switch {
			case err != nil:
				u.Detail = fmt.Sprintf("not compared: %s could not be read (%v)", file.TableID, err)
			case docsUnchangedSinceRecorded(store, file, row) || docsHeldExactly(file.Docs, row):
				u.Action, u.Detail = string(applyActionUnchanged), "the table already says what this file says"
			case docsLanded(file.Docs, row):
				u.Detail = "the table's documentation differs from this file" + unmeasuredColumnsNote
			default:
				u.Detail = "the table's documentation differs from this file"
			}
		}
		out = append(out, u)
	}
	return out
}

// checksUnits is one unit per checks file (checks/<stem>.json) — and per one
// the folder no longer holds — answered on applyTableChecks' terms.
func checksUnits(ctx context.Context, client *api.Client, f *folder, store liveHashes, local map[string]string) []syncApplyUnitReport {
	files, err := readChecksFiles(f.Root, local)
	if err != nil {
		return nil
	}
	var out []syncApplyUnitReport
	for _, file := range withDeletedChecksFiles(files, store) {
		tableID := file.TableID
		if !file.Deleted {
			tableID = f.Binding.Tables[file.SQLPath]
		}
		u := syncApplyUnitReport{Path: file.Path, ID: tableID, Action: string(applyActionUpdate)}
		switch {
		case file.Deleted:
			u.Detail = "gone from this folder, so apply looks for the checks it created"
		case file.Problem != "":
			refuseUnit(&u, syncReasonChecksNotApplied, file.Problem)
		case tableID == "":
			u.Detail = fmt.Sprintf("%s has no table yet, so its checks wait for it", file.SQLPath)
		default:
			rows, err := client.ListTableChecks(ctx, tableID)
			if err != nil {
				u.Detail = fmt.Sprintf("not compared: the checks on %s could not be read (%v)", tableID, err)
				break
			}
			plan := planTableChecks(file.File.Checks, rows, managedChecksOf(store, file.Path, tableID), checksClaim{})
			if checksPlanWritesNothing(file.File.Checks, plan) {
				u.Action, u.Detail = string(applyActionUnchanged), "the table's checks already say what this file says"
			} else {
				u.Detail = "the table's checks differ from this file"
			}
		}
		out = append(out, u)
	}
	return out
}

// compareAutomationUnits makes the comparison for an automation folder's units,
// over the push's own one listing of the stack's feature.
func compareAutomationUnits(ctx context.Context, plan *plannedFolder) {
	line, f := &plan.line, plan.f
	files, _, err := readLocalFiles(f.Root, f.Kind)
	if err != nil {
		return
	}
	rows, listErr := automationRowsOf(ctx, newClient(f.Resolved.URL, f.Resolved.Token), f)
	for i := range line.Units {
		u := &line.Units[i]
		if u.Action != string(applyActionUpdate) {
			continue
		}
		prior := u.Detail
		if listErr != nil {
			u.Detail = fmt.Sprintf("not compared: the automations of %s could not be read (%v)", f.Binding.FeatureID, listErr)
			keepPriorDetail(u, prior)
			continue
		}
		switch held, refusal, ok := automationHeldLive(f, u.Path, files[u.Path], u.ID, rows); {
		case !ok:
			u.Detail = "not compared: the file could not be read as an automation"
		case refusal != "":
			refuseUnit(u, syncReasonNotDeployed, refusal)
			continue
		case held:
			u.Action, u.Detail = string(applyActionUnchanged), "the live automation already holds this file"
		default:
			u.Detail = "the live automation differs from this file"
		}
		keepPriorDetail(u, prior)
	}
}

// refuseUnit marks a unit the apply would refuse, on the push's own terms, so
// the dry run says so (refuseCompared) rather than calling it an update.
func refuseUnit(u *syncApplyUnitReport, reason, detail string) {
	u.Action, u.Reason, u.Detail = string(applyActionRefuse), reason, detail
}

// refuseCompared turns a compared folder whose units include one the apply
// would refuse into the refusal the apply would report: the folder is
// refused, under the first such unit's reason. False when there is none.
//
// A checks file is the one refusal that holds nothing else back: apply
// publishes the tables and then reports checks_not_applied. When every refused
// unit is a checks file, the line says what publishes and that the checks
// would not apply, not that apply refuses the folder's files.
func refuseCompared(kind wfdir.Kind, line *syncApplyFolderReport) bool {
	var refused []string
	reason, checksOnly := "", true
	for _, u := range line.Units {
		if u.Action != string(applyActionRefuse) {
			continue
		}
		reason = cmp.Or(reason, u.Reason)
		checksOnly = checksOnly && u.Reason == syncReasonChecksNotApplied
		refused = append(refused, u.Path+": "+u.Detail)
	}
	if len(refused) == 0 {
		return false
	}
	line.Verdict, line.Reason = verdictRefused, reason
	if checksOnly {
		line.Detail = fmt.Sprintf("%s; then the checks would not apply — %s:\n%s",
			describeCompared(kind, line.Units, line.Claims), agree(len(refused), "apply cannot use this checks file", "apply cannot use these checks files"), strings.Join(refused, "\n"))
		return true
	}
	line.Detail = fmt.Sprintf("apply would refuse %s:\n%s", agree(len(refused), "this file", "these files"), strings.Join(refused, "\n"))
	return true
}

// keepPriorDetail keeps what the plan had already said about a unit — that
// apply claims it for this folder, say (markManagedUnits) — after the
// comparison's own sentence.
func keepPriorDetail(u *syncApplyUnitReport, prior string) {
	prior = strings.TrimPrefix(strings.TrimSpace(prior), "— ")
	if prior == "" || prior == u.Detail {
		return
	}
	if u.Detail == "" {
		u.Detail = prior
		return
	}
	u.Detail += " — " + prior
}

// describeCompared is a compared folder's one-line summary, from its units and
// the claims apply records for it (claims).
//
// A declared move or removal is not a file that differs from what is live, and
// a claim is not a write to any row: each is named for what it is, so "nothing
// to push or publish" stays true of the rows and the records are still said.
func describeCompared(kind wfdir.Kind, units []syncApplyUnitReport, claims int) string {
	changing, creates, moves, removes, files := 0, 0, 0, 0, 0
	for _, u := range units {
		switch applyAction(u.Action) {
		case applyActionRefuse:
			// Said apart by refuseCompared: a checks file apply cannot use
			// is not a file it pushes.
			continue
		case applyActionMove:
			moves++
			continue
		case applyActionRemove:
			removes++
			continue
		case applyActionUnchanged:
		case applyActionCreate:
			creates++
		default:
			changing++
		}
		files++
	}
	verb := "push and publish"
	if kind.Name == wfdir.KindAutomation {
		verb = "write"
	}
	var records []string
	if moves > 0 {
		records = append(records, fmt.Sprintf("records %d declared %s", moves, plural(moves, "move")))
	}
	if removes > 0 {
		records = append(records, fmt.Sprintf("stops managing %d declared %s, leaving %s as %s", removes, plural(removes, "removal"), agree(removes, "its row", "their rows"), agree(removes, "it is", "they are")))
	}
	records = append(records, claimsRecorded(claims)...)
	if changing+creates == 0 {
		nothing := "push or publish"
		if kind.Name == wfdir.KindAutomation {
			nothing = verb
		}
		out := fmt.Sprintf("nothing to %s: the live rows already hold this folder", nothing)
		if len(records) > 0 {
			out += "; apply " + joinAnd(records)
		}
		return out
	}
	var parts []string
	if changing > 0 {
		parts = append(parts, fmt.Sprintf("%s %d of its %d %s, which differ from what is live", verb, changing, files, plural(files, "file")))
	}
	if creates > 0 {
		parts = append(parts, fmt.Sprintf("create %d new %s (--allow-create)", creates, plural(creates, "row")))
	}
	out := "would " + joinAnd(parts)
	if len(records) > 0 {
		out += "; it also " + joinAnd(records)
	}
	return out
}

// claimsRecorded says what apply records for rows the lock names and the
// server holds no claim for (managedBackfill) — writes to Ronja's records,
// never to a row.
func claimsRecorded(claims int) []string {
	if claims == 0 {
		return nil
	}
	return []string{fmt.Sprintf("records %d %s in Ronja for %s this folder's lock names (no row is written)", claims, plural(claims, "claim"), agree(claims, "a row", "rows"))}
}
