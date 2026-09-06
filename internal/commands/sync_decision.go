package commands

import (
	"fmt"
	"strings"

	"github.com/ronjatech/ronja-cli/internal/wfdir"
)

// The local decision every push loop makes before it touches the network, in
// one place, so a tree-wide `sync apply` and its `--dry-run` cannot disagree
// about it.
//
// `workflow_push.go`'s inspectTarget is the precedent AND THE BOUNDARY: "Every
// refusal a push makes about the row it was pointed at lives here, so they all
// happen before the first write." This file generalises the first half of that
// sentence — the refusals a push makes with NO REQUEST AT ALL — and stops
// exactly where inspectTarget's own request begins.
//
// ⚠️ WHAT IS DELIBERATELY NOT HERE, because it is not knowable locally and an
// earlier revision of the plan assumed it was:
//
//   - the 403 fork in pushOneTable, which is only knowable from CreateTable's
//     response;
//   - the table-code CAS 409, only knowable from UpdateTableCode;
//   - the build verdict, only knowable after SyncTable;
//   - a table's wire form at all, because newPipelineCodec reads LIVE: a
//     sibling's table id only exists after that sibling's own create, which
//     happens INSIDE the loop, so no pre-pass can resolve file B before file A
//     has run;
//   - whether the remote has drifted, which is a read whose answer can move
//     between a dry-run and the apply it previews.
//
// A decision function that reached for any of those would be a dry-run that
// promises what the SERVER will answer, rather than what the CLI will ATTEMPT.
//
// ⚠️ AND ONE LIMIT OF THIS FILE'S OWN, stated because the first version of it
// overstated the boundary. "The refusals a push makes with no request" is TWO
// things, not one: create-vs-update, which the applyDecision* functions answer,
// and the per-kind local GUARDS the loops run before their first byte — an
// orphaned automation file, a dependency cycle, a positional ref, a manifest
// naming an entrypoint the folder does not hold, a folder bound to automations
// with no feature to compare them in. decideApplyPushPreflight covers the
// second, by calling the loops' own functions; what it still does not cover, and
// why, is on that function.

// Three never-green reasons beside sync_walk.go's and sync_edges.go's, in the
// same vocabulary and under the same contract: these strings are the machine
// half of a report, and a second set that drifted from the tree commands' is
// precisely the failure this file exists to prevent.
const (
	// syncReasonManifestRewrite — acting on this folder would REWRITE its
	// committed ronja.json as a side effect, which a tree-wide command must not
	// do across thirty folders nobody was watching.
	//
	// ⚠️ It is deliberately NOT called `legacy_folder`, and the reason is worth
	// stating because "skip legacy folders" is how the rule was first written.
	// The plain legacy shape — an `instances[]` folder naming no stacks — is
	// ALREADY never-green under syncReasonUnnamedStack, whose own doc says so
	// ("the legacy instances[] shape"). Reusing `sync status`'s vocabulary
	// covered it for free. What was left over is not a shape but an ACT, and
	// there are two of them:
	//
	//   - adoptStack, which writes a stack entry for a name the manifest does
	//     not declare. Reachable here only through the stranded-lock leg: a lock
	//     recording ids under a name no stack declares, which a push ADOPTS by
	//     writing the declaration.
	//   - Manifest.Record, which DROPS a superseded `instances[]` entry for the
	//     place the stack now names — the second half of the same migration.
	//     ⚠️ ONLY for a NAMED selection: Record's `!sel.Named()` branch returns
	//     before the drop, so a legacy folder pushed with no --stack migrates
	//     nothing and is not refused here. See decideApplyFolder.
	//
	// Both are correct for a person standing in the folder and watching it
	// happen, which is where they stay.
	syncReasonManifestRewrite = "manifest_rewrite"
	// syncReasonNoDriftAnchor — the creation unit is bound to a live row and the
	// folder has recorded no anchor for it, so every guard that would refuse an
	// overwrite is disarmed. See applyActionUnanchored, which is the decision it
	// belongs to.
	syncReasonNoDriftAnchor = "no_drift_anchor"
	// syncReasonUnresolvedBind — the alias pre-flight refused: a declared
	// dependency this stack binds nothing to, a bind that answers nothing, a
	// source writing an id an alias already owns. Entirely local, which is why
	// it is a decision and not an edge.
	syncReasonUnresolvedBind = "unresolved_bind"
	// syncReasonCyclicDependency — this folder's files read from each other in a
	// loop, so there is no order to push them in. topoOrder's own refusal, which
	// fires over the WHOLE folder rather than the pushed subset.
	syncReasonCyclicDependency = "cyclic_dependency"
	// syncReasonOrphanedFile — a file bound to a live row is gone from the
	// folder. The push refuses rather than deleting, and apply never passes
	// --prune, so for a tree command it is simply never-green.
	syncReasonOrphanedFile = "orphaned_file"
	// syncReasonMissingEntrypoint — the manifest names an entrypoint the folder
	// does not hold, which is what a renamed .py (or App.tsx) with an unedited
	// ronja.json looks like. A WHOLE-FOLDER refusal both draft kinds make before
	// their first byte, and one no create-vs-update answer can reach: the folder
	// is bound, so its only decision is `update`.
	syncReasonMissingEntrypoint = "missing_entrypoint"
	// syncReasonNoFeature is sync_edges.go's, reused verbatim: "this folder
	// names no feature" is the same fact whether a validate needs one or a
	// create does.
	//
	// syncReasonPositionalRef is sync_edges.go's too, for the same reason: `sync
	// check` reports a positional ref as an unusable edge and a push refuses to
	// send one, and they are the same fact about the same line of SQL.
)

// applyAction is what a push would DO to one creation unit, decided from the
// manifest, the lock and the files on disk with no request.
//
// ⚠️ There is deliberately no `no-op`. It is local for a PIPELINE file (the
// committed SQL against the lock's fingerprint) and NOT local for a workflow or
// a data app, whose files are compared against a listing of the caller's own
// draft. A uniform `no-op` would be a promise only one kind could keep, and the
// one thing worse than a dry-run that says less than it knows is one that says
// more.
type applyAction string

const (
	// applyActionCreate — nothing is bound here, so a push MAKES a row. This is
	// the action a create gate counts, and it is counted per creation unit: per
	// FILE for a pipeline and an automation, per FOLDER for a workflow and a
	// data app.
	applyActionCreate applyAction = "create"
	// applyActionUpdate — a row is bound and a push writes to it.
	applyActionUpdate applyAction = "update"
	// applyActionUnanchored — a row is bound, and the folder has no anchor to
	// compare it against.
	//
	// ⚠️ ITS OWN ACTION AND NOT AN UPDATE, which is the whole reason it exists.
	// An empty LockAutomation.UpdatedAt disarms BOTH automation guards at once —
	// automationDrift answers driftNoBaseline rather than driftChanged, and
	// automationReEnableGuard returns nil on `seen == ""` — while AutomationID is
	// still there, so the unit looks bound and sails through a create gate as an
	// ordinary update. It is not one: scheduled_jobs has no version history and
	// PUT's list fields REPLACE, so the write is an unguarded overwrite, and it
	// flips `enabled` false→true on every automation somebody paused during an
	// incident.
	//
	// Only the automation kind produces it, and that is a statement about the
	// GUARDS rather than about the field. A pipeline with no committed
	// LiveSHA256 still has the local baseline leg and the draft-commit CAS; a
	// workflow or data app with no HeadVersionID still has per-file
	// preconditions built from the listing it just read. An automation with no
	// anchor has NOTHING, which is why it is the one that gets its own answer.
	applyActionUnanchored applyAction = "unanchored"
	// applyActionRefuse — a push cannot proceed on this unit, and Reason says
	// why in the tree commands' vocabulary.
	applyActionRefuse applyAction = "refuse"
)

// applyDecision is what a push would attempt for ONE CREATION UNIT.
//
// The unit is the grain the LOCK discriminates at, never the folder: Binding
// carries WorkflowID/DataAppID per folder, but Tables and Automations are
// map[path]… per FILE, and pushOneTable creates whenever Tables[path] is empty.
// A folder-grain answer would call a pipeline folder with six bound .sql files
// and one new one "bound", and a caller acting on that would create a live table
// in a live organization from a run that promised to create nothing.
type applyDecision struct {
	// Path is the folder-relative file this decision is about, and is EMPTY for
	// a workflow or a data app, whose creation unit is the folder itself.
	Path string
	// ID is the row already bound to this unit, empty for a create.
	ID string
	// Action is the answer.
	Action applyAction
	// Reason is a sync* constant, set only for a refusal and for an unanchored
	// unit. Machine contract; Detail is the prose beside it.
	Reason string
	Detail string
	// noRow records that this unit has nothing behind it, INDEPENDENTLY of
	// whether the create was then refused for want of a feature. Action alone
	// cannot say so — a refusal overwrites it — and "would this make a row" is
	// the question the create gate asks. See WouldCreate.
	noRow bool

	// err is the refusal AS THE LOOP RETURNS IT, wrapping included. It is not
	// serialized and it is not Detail's source of truth by accident: featureIDFor
	// wraps errNoFeature, `sync check` reads that wrapping with errors.Is, and a
	// decision that handed back a re-made errors.New would break it silently.
	err error
}

// Creates reports a unit with no row behind it — the thing a create gate counts.
func (d applyDecision) Creates() bool { return d.Action == applyActionCreate }

// WouldCreate reports a unit with no row behind it whether or not the create was
// then refused, which is what lets the create gate name a file BEFORE sending its
// author off to add a featureID for a row apply would refuse anyway.
func (d applyDecision) WouldCreate() bool { return d.noRow }

// Refused reports a unit a push cannot proceed on.
func (d applyDecision) Refused() bool { return d.Action == applyActionRefuse }

// Err is the refusal as an error, or nil. See applyDecision.err.
func (d applyDecision) Err() error { return d.err }

// refusedDecision builds the refusal, keeping Detail and err the same sentence.
func refusedDecision(base applyDecision, reason string, err error) applyDecision {
	base.Action, base.Reason, base.Detail, base.err = applyActionRefuse, reason, err.Error(), err
	return base
}

// decideApplyFolder answers whether a folder can be acted on AT ALL for the
// stack named, before any creation unit inside it is looked at.
//
// It is resolveStackForFolder plus the two manifest rewrites, and it is
// resolveStackForFolder rather than a copy for the reason that function already
// gives: it mirrors selectNamed's acceptance rules, so a folder this says yes to
// is one selectNamed will also accept. An empty reason means "go on".
//
// ⚠️ These are refusals no single-folder push makes, and that asymmetry is real
// rather than an omission. `ronja pipeline push` reaches a stack mismatch as a
// HARD ERROR out of openFolder and a manifest rewrite as a silent adoptStack;
// neither is a decision it takes. A tree command must not abort the whole run
// for one folder, and must not edit thirty committed manifests as a side effect,
// so it needs the answers as data — which is what this returns.
func decideApplyFolder(m *wfdir.Manifest, lock *wfdir.Lock, key wfdir.InstanceKey, want string) (reason, detail string) {
	if reason, detail := resolveStackForFolder(m, lock, key, want); reason != "" {
		return reason, detail
	}
	if _, declared := m.Stacks[want]; want != "" && !declared {
		// resolveStackForFolder said yes and the manifest declares no such stack,
		// which leaves exactly one way to have got here: the LOCK records the
		// name. selectNamed adopts that, and adoptStack then writes the
		// declaration into the committed file.
		return syncReasonManifestRewrite, fmt.Sprintf("%s records stack %q and %s declares no such stack, so acting here would WRITE that declaration into a committed file. Push this folder on its own, where the repair is something a person watched happen",
			wfdir.LockName, want, wfdir.ManifestName)
	}
	// ⚠️ ONLY FOR A NAMED SELECTION, and the narrowing is the whole of the rule
	// rather than a caution. Manifest.Record takes its `!sel.Named()` branch for
	// an unnamed one — SetBinding in place, returning BEFORE the instances[] drop
	// — so a legacy folder pushed with no --stack migrates nothing. Refusing it
	// here locked those folders out of apply altogether: with no --stack they were
	// refused for an act that cannot happen, and with one they are already
	// never-green as unnamed_stack, so there was no route to green in either
	// direction. Reaching this line at all means m.Stacks[want] is declared (the
	// undeclared case returned above), which is exactly the condition under which
	// Record really does drop the entry.
	if _, declared := m.Stacks[want]; want != "" && declared && key.Known() {
		for _, inst := range m.Instances {
			if wfdir.Matches(inst.Key(), key) {
				// A legacy entry for the very place the stack names. Recording a
				// binding DROPS it (Manifest.Record), which is the other half of
				// the migration and the other committed-file edit.
				return syncReasonManifestRewrite, fmt.Sprintf("this folder declares stack %q and still keeps a binding for %s in \"instances\" in %s beside it, and a push under that stack MIGRATES the entry — rewriting a committed file. Push this folder on its own, where the rewrite is something a person watched happen",
					want, describeKeyForApply(key), wfdir.ManifestName)
			}
		}
	}
	return "", ""
}

// describeKeyForApply renders an (instance, organization) for a refusal, in the
// one shape the tree reports use. wfdir has its own unexported spelling of this
// and no exported one; a second sentence is cheaper than an export that would
// then have two callers with two audiences.
func describeKeyForApply(key wfdir.InstanceKey) string {
	if key.TenantID == "" {
		return key.URL
	}
	return key.TenantID + " on " + key.URL
}

// decideApplyBind is the alias pre-flight, as a decision.
//
// The report is returned beside the reason because its WARNINGS are not a
// refusal and every push loop prints them; a helper that folded them away would
// make adopting it a silent loss of output.
//
// The arguments are the uniform ones `sync check` already passes: folderStems
// and folderFieldRefs are kind-aware and answer nil for the two kinds whose
// loops pass nil by hand, so this is the same call all four loops were already
// making.
func decideApplyBind(f *folder, files map[string]string) (report aliasReport, reason, detail string) {
	report = checkAliases(f.Manifest, f.selection(), f.Codec, files,
		folderStems(f.Kind, files), folderFieldRefs(f.Kind, f.Root, files))
	if err := report.err(); err != nil {
		return report, syncReasonUnresolvedBind, err.Error()
	}
	return report, "", ""
}

// decideApplyPushPreflight is the rest of what a push refuses with NO REQUEST:
// the per-kind local guards that live inside the loops rather than in a
// decision, and that a create-vs-update answer therefore does not cover.
//
// ⚠️ IT EXISTS BECAUSE A DRY-RUN THAT MISSES THEM IS A LIAR. Each of these fires
// before the loop's first byte, so a preview that reported the folder `applied`
// and an apply that exited 1 on it disagreed about something entirely local —
// the one class of disagreement §4.1 of the plan promises cannot happen.
//
// It calls the loops' OWN functions, on the loops' own inputs, and returns their
// own sentences: a second implementation of "is there a cycle here" is worth
// less than no check at all, because it would be wrong in its own way.
//
// ⚠️ EVERY KIND HAS A CASE, including the two whose creation unit is the folder
// itself, and the switch is exhaustive on purpose: a kind with no arm reads as a
// kind with nothing to check, which is how the two draft kinds' entrypoint guard
// went missing here for a whole slice. A workflow folder whose .py was renamed
// without its manifest is BOUND, so its only decision is `update` — the gate
// cleared it, the dry-run reported it deployable, and the apply then exited 1 on
// it, possibly twenty folders into a half-deployed tree.
//
// ⚠️ WHAT IS STILL OUTSIDE IT, deliberately: every per-file VALIDITY refusal —
// an unparseable automation declaration, a file over the size cap, a reference
// that resolves to nothing. Those are local too, and they are left where they
// are because reproducing them here means reproducing four kinds' parse steps
// and their diagnostics; a folder holding one is reported `applied` by the
// dry-run and refused by the apply. That is the honest edge of this claim.
func decideApplyPushPreflight(f *folder, files map[string]string) (reason, detail string) {
	switch f.Kind.Name {
	case wfdir.KindPipeline:
		// The order is runPipelinePush's: the cycle over the WHOLE folder, then
		// the positional refs over what is actually being SENT — a positional ref
		// in a file this run would not push cannot mis-resolve anything, and
		// refusing on it would let one stale file block the rest of the folder.
		targets := pipelineChangedTargets(f, files, f.State.For(f.Key).Hashes())
		if len(targets) == 0 {
			return "", ""
		}
		codec := newPipelineCodec(f, files)
		ordered, err := topoOrder(codec, files, targets)
		if err != nil {
			return syncReasonCyclicDependency, err.Error()
		}
		sending := make(map[string]string, len(ordered))
		for _, path := range ordered {
			sending[path] = files[path]
		}
		if err := refusePositionalRefs(sending); err != nil {
			return syncReasonPositionalRef, err.Error()
		}
	case wfdir.KindAutomation:
		// A file that left the folder is not an automation a push deletes, and
		// apply never passes --prune, so this is always the refusal.
		if orphans := automationOrphans(f.Binding, files); len(orphans) > 0 {
			return syncReasonOrphanedFile, automationOrphanError(orphans).Error()
		}
		// Step 5, which is NOT step 4's create refusal: a folder that creates
		// nothing can be bound and featureless at once, and the loop refuses it
		// before its listing rather than reading every row as gone.
		if err := refuseBoundAutomationsWithNoFeature(f); err != nil {
			return syncReasonNoFeature, err.Error()
		}
	case wfdir.KindDataApp:
		if err := refuseMissingAppEntrypoint(f, files); err != nil {
			return syncReasonMissingEntrypoint, err.Error()
		}
	default:
		// The workflow kind. Its own whole-folder guard, and the one a folder-grain
		// decision can never reach: a bound folder whose .py was renamed without
		// its manifest has exactly one decision — `update` — so the gate cleared it
		// and the dry-run called it deployable, twenty folders before the apply
		// exited 1 on it.
		if err := refuseMissingEntrypoint(f, files); err != nil {
			return syncReasonMissingEntrypoint, err.Error()
		}
	}
	return "", ""
}

// applyDecisionsFor yields one decision per creation unit in one folder.
//
// paths are the folder-relative files a push is acting on, in the order it will
// act on them; they are IGNORED for a workflow and a data app, which have one
// creation unit each however many files they hold.
func applyDecisionsFor(f *folder, paths []string) []applyDecision {
	switch f.Kind.Name {
	case wfdir.KindPipeline:
		out := make([]applyDecision, 0, len(paths))
		for _, path := range paths {
			out = append(out, pipelineApplyDecision(f, path))
		}
		return out
	case wfdir.KindAutomation:
		out := make([]applyDecision, 0, len(paths))
		for _, path := range paths {
			out = append(out, automationApplyDecision(f, path))
		}
		return out
	case wfdir.KindDataApp:
		return []applyDecision{appApplyDecision(f)}
	default:
		return []applyDecision{workflowApplyDecision(f)}
	}
}

// pipelineApplyDecision is one .sql file's answer.
func pipelineApplyDecision(f *folder, path string) applyDecision {
	d := applyDecision{Path: path, ID: f.Binding.Tables[path]}
	if d.ID != "" {
		d.Action = applyActionUpdate
		return d
	}
	d.Action, d.noRow = applyActionCreate, true
	if f.Binding.FeatureID == "" {
		return refusedDecision(d, syncReasonNoFeature, pipelineNoFeatureError(f, path))
	}
	return d
}

// pipelineNoFeatureError is the refusal runPipelinePush's step 4 gives, in the
// one place both it and a dry-run read it from.
//
// The ORGANIZATION is part of the answer whenever this folder has NO entry here
// and names another organization instead — the same reason featureIDFor says so
// for a workflow. "Add featureID to the <url> entry" sends the reader to a line
// that already has one, which is what they see and why they stop believing the
// message. A folder cloned from another organization is the ordinary way to
// arrive here.
//
// A folder that IS bound here and merely left "featureID" out gets the plain
// wording: it has a line to add the field to, and the cross-organization
// sentence would be wrong advice one case over.
func pipelineNoFeatureError(f *folder, path string) error {
	if others := f.otherOrganizationsOn(); !f.Bound && len(others) > 0 {
		return fmt.Errorf("%s has no table yet, and this folder names no feature for %s in %s — it is bound to %s instead, and a table's ids belong to the organization that holds them, so nothing recorded there can be pushed under this credential.\n  %s",
			path, describeTarget(f.Resolved), wfdir.ManifestPath(f.Root),
			f.describeOrganizationIDs(others), f.featureAdviceForAnotherOrganization())
	}
	return fmt.Errorf("%s has no table yet and this folder names no feature, so there is nowhere to create one.\n  %s, or start from `ronja pipeline init --feature <id>` or `ronja pipeline clone <feature-id>`",
		path, f.featureAdvice())
}

// automationApplyDecision is one .json file's answer.
//
// The unanchored leg is the one thing here that is not create-vs-update, and it
// is the reason this function is not a copy of the pipeline's — see
// applyActionUnanchored.
func automationApplyDecision(f *folder, path string) applyDecision {
	d := applyDecision{Path: path, ID: f.Binding.Automations[path]}
	if d.ID == "" {
		d.Action, d.noRow = applyActionCreate, true
		if f.Binding.FeatureID == "" {
			return refusedDecision(d, syncReasonNoFeature, automationNoFeatureError(f, path))
		}
		return d
	}
	if _, seen := f.Lock.AutomationSeen(f.Stack, path); seen == "" {
		d.Action, d.Reason = applyActionUnanchored, syncReasonNoDriftAnchor
		// The recovery is deliberately a PER-FOLDER push: `ronja automation push`
		// records the anchor in the same write that lands the file, so a person
		// who wants this row written is one command away from having one. Nothing
		// records an anchor without writing, which is exactly why a tree-wide
		// apply cannot quietly arm it on the reader's behalf.
		d.Detail = fmt.Sprintf("this folder is bound to automation %s and has never recorded when it last agreed with it, so both guards are off: a write would overwrite whatever moved on the server, and would turn the row back on if somebody had paused it. Push this folder on its own — that write records the anchor",
			d.ID)
		return d
	}
	d.Action = applyActionUpdate
	return d
}

// automationNoFeatureError is runAutomationPush's step-4 refusal, in one
// place. Worded per kind rather than shared with the pipeline's because the two
// name different things ("no table yet" / "no automation yet") and both spell
// out the organization they are missing a feature for.
func automationNoFeatureError(f *folder, path string) error {
	if others := f.otherOrganizationsOn(); !f.Bound && len(others) > 0 {
		return fmt.Errorf("%s has no automation yet, and this folder names no feature for organization %s on %s in %s — the entries there name %s instead, and an automation's ids belong to the organization that holds them.\n  %s",
			path, f.Key.TenantID, f.Resolved.URL, wfdir.ManifestPath(f.Root),
			strings.Join(others, ", "), f.featureAdviceForAnotherOrganization())
	}
	return fmt.Errorf("%s has no automation yet and this folder names no feature, so there is nowhere to create one.\n  %s, or start from `ronja automation init --feature <id>`",
		path, f.featureAdvice())
}

// workflowApplyDecision is a workflow folder's answer. One creation unit, and
// the create fork is Binding.WorkflowID — the same value inspectTarget branches
// on before it makes its own request.
func workflowApplyDecision(f *folder) applyDecision {
	d := applyDecision{ID: f.Binding.WorkflowID}
	if d.ID != "" {
		d.Action = applyActionUpdate
		return d
	}
	d.Action, d.noRow = applyActionCreate, true
	if f.Binding.FeatureID == "" {
		// featureIDFor's own refusal, ASKED FOR rather than duplicated, so the
		// errNoFeature wrapping `sync check` reads with errors.Is survives and
		// there is one wording. It is only reachable locally for a create: a
		// BOUND folder with no featureID is answered by the row.
		return refusedDecision(d, syncReasonNoFeature, noFeatureError(f))
	}
	return d
}

// appApplyDecision is a data-app folder's answer.
func appApplyDecision(f *folder) applyDecision {
	d := applyDecision{ID: f.Binding.DataAppID}
	if d.ID != "" {
		d.Action = applyActionUpdate
		return d
	}
	d.Action, d.noRow = applyActionCreate, true
	if f.Binding.FeatureID == "" {
		return refusedDecision(d, syncReasonNoFeature, appNoFeatureError(f))
	}
	return d
}

// appNoFeatureError is resolveAppPushTarget's refusal, in one place.
//
// The ORGANIZATION is part of the answer whenever this folder has NO entry here
// and names another one instead — the same reason featureIDFor says so for a
// workflow, and the case `app push --profile <other>` lands in. "Add featureID
// to its entry" sends the reader to a line that already has one, in an entry
// belonging to a different organization.
func appNoFeatureError(f *folder) error {
	if others := f.otherOrganizationsOn(); !f.Bound && len(others) > 0 {
		return fmt.Errorf("this folder has no feature to create the data app in for %s — it is bound to %s instead, and a data app's ids belong to the organization that holds them, so nothing recorded in %s can be pushed under this credential.\n  %s",
			describeTarget(f.Resolved), f.describeOrganizationIDs(others),
			wfdir.ManifestPath(f.Root), f.featureAdviceForAnotherOrganization())
	}
	return fmt.Errorf("this folder has no feature to create the data app in — %s, or start again with `ronja app init --feature <id>`",
		f.featureAdvice())
}

// errFromDecisions is the first refusal in a list, as the error a push loop
// returns — nil when nothing refuses. The FIRST rather than all of them because
// that is what the loops did: step 4 of both file loops returns on the first
// file it cannot create.
func errFromDecisions(decisions []applyDecision) error {
	for _, d := range decisions {
		if d.Refused() {
			return d.Err()
		}
	}
	return nil
}
