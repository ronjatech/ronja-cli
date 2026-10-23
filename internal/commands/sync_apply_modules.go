package commands

import (
	"fmt"
	"strings"

	"github.com/ronjatech/ronja-cli/internal/wfdir"
)

// The shared_module WARNING of `sync apply`: an organization has one live copy
// of each module, so deploying any stack publishes it into the feature it
// lives in. Split from sync_apply.go.

// syncWarningSharedModule — a module folder this run deploys publishes the
// organization's ONE module of that name, which every feature's workflows
// import, and this stack's other folders deploy to another feature (or there
// are none). A Dev stack applying it changes what production imports.
const syncWarningSharedModule = "shared_module"

// sharedModuleWarnings names every module folder this run would push into a
// feature the run's other folders do not all deploy to. A module's package
// name is unique in the organization, so a tree deployed to a second feature
// binds its module folder to the feature that holds the one copy — and
// applying that stack publishes the module every feature's workflows import.
// Not refused: it is the only way the tree can share one module, and a
// consume-only stack is not something this build has. Said loudly instead.
func sharedModuleWarnings(planned []plannedFolder) []syncApplyWarning {
	others := map[string]bool{}
	for i := range planned {
		if f := planned[i].f; f != nil && f.Kind.Name != wfdir.KindModule && f.Binding.FeatureID != "" {
			others[f.Binding.FeatureID] = true
		}
	}
	var out []syncApplyWarning
	for i := range planned {
		f := planned[i].f
		if f == nil || f.Kind.Name != wfdir.KindModule {
			continue
		}
		feature := f.Binding.FeatureID
		if len(others) == 1 && others[feature] {
			continue
		}
		elsewhere := "no other folder of this run deploys to that feature"
		if len(others) > 0 {
			elsewhere = "this run's other folders deploy to " + strings.Join(sortedKeys(others), ", ")
		}
		name := f.Manifest.Name
		if name == "" {
			name = f.Manifest.Title
		}
		// The first line says what happens and what to do; the lines after it
		// say why (the renderer indents them).
		detail := fmt.Sprintf("This publishes the organization's single module %q into feature %s, so its change reaches production too — apply it only if you mean that change for every feature.\nEvery feature's workflows import that one copy, and %s, whichever stack you meant to deploy.\n%s",
			name, orUnknown(feature), elsewhere, modulePinNote)
		switch line := planned[i].line; {
		case moduleUnchangedIn(line):
			detail = fmt.Sprintf("Nothing in module %q changes in this run; once it does, applying this stack publishes it into feature %s, production included — change it only when you mean that change for every feature.\nThe live module already holds this folder. It is the organization's single copy, which every feature's workflows import, and %s.\n%s",
				name, orUnknown(feature), elsewhere, modulePinNote)
		case line.Verdict != verdictApplied:
			// Refused or unknown: whatever the run meant to publish, it did not.
			detail = fmt.Sprintf("This run did not publish module %q (the folder's line says why); once a run does, applying this stack publishes it into feature %s, production included.\nIt is the organization's single copy, which every feature's workflows import, and %s.\n%s",
				name, orUnknown(feature), elsewhere, modulePinNote)
		}
		out = append(out, syncApplyWarning{Reason: syncWarningSharedModule, Path: planned[i].line.Path, Detail: detail})
	}
	return out
}

// modulePinNote is when a published module reaches a workflow that imports it:
// a draft save pins the module's current version, and a workflow's publish
// freezes the pin (claude/primitives/MODULE.md, "Version pinning").
const modulePinNote = "A workflow runs the module version it was last published with: it picks a new version up when a draft of it is next saved, and runs it once that workflow is published again"

// moduleUnchangedIn reports that a module folder's line says the run publishes
// nothing to it: a dry run that found the live module holding the folder, or
// an apply whose push had nothing to push.
func moduleUnchangedIn(line syncApplyFolderReport) bool {
	if line.Verdict != verdictApplied {
		return false
	}
	if len(line.Units) == 1 && line.Units[0].Action == string(applyActionUnchanged) {
		return true
	}
	return line.Detail == applyNothingToPush
}
