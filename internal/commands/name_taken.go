package commands

import (
	"context"
	"fmt"
	"strings"

	"github.com/ronjatech/ronja-cli/internal/api"
	"github.com/ronjatech/ronja-cli/internal/wfdir"
)

// syncReasonNameTaken — `sync apply --allow-create` would create a workflow,
// data app, module or automation beside a live one of the same kind and name
// in the same feature. Machine contract: add, never rename.
const syncReasonNameTaken = "name_taken"

// nameTakenRefusal refuses a create under --allow-create that would twin a
// same-named row of its kind in the target feature: a second "Region Board"
// beside the first, which nothing afterwards tells apart. It reads the
// feature's reference inventory once, and only when the folder creates one of
// those kinds.
//
// Names match exactly, case included. That is the server's rule for modules
// (a case-sensitive unique index per feature); workflows, apps and automations
// have no server rule, so exact matching refuses only a true twin.
func nameTakenRefusal(ctx context.Context, f *folder, files map[string]string, decisions []applyDecision) (reason, detail string) {
	type wanted struct{ kind, name, path string }
	var creates []wanted
	switch f.Kind.Name {
	case wfdir.KindWorkflow, wfdir.KindDataApp, wfdir.KindModule:
		refKind := map[string]string{wfdir.KindWorkflow: api.RefKindWorkflow, wfdir.KindDataApp: api.RefKindDataApp, wfdir.KindModule: api.RefKindModule}[f.Kind.Name]
		// The field the create names the row by: a module's package name, which
		// the server keeps unique per feature; a workflow's or app's title.
		name := f.Manifest.Title
		if f.Kind.Name == wfdir.KindModule {
			name = f.Manifest.Name
		}
		for _, d := range decisions {
			if d.Path == "" && d.Creates() && strings.TrimSpace(name) != "" {
				creates = append(creates, wanted{kind: refKind, name: strings.TrimSpace(name)})
			}
		}
	case wfdir.KindAutomation:
		parsed, problems := parseAutomationFolder(files)
		if len(problems) > 0 {
			return "", ""
		}
		for _, d := range decisions {
			file := parsed[d.Path]
			if !d.Creates() || file == nil {
				continue
			}
			name := automationNameFor(d.Path)
			if file.Name != nil {
				name = *file.Name
			}
			creates = append(creates, wanted{kind: api.RefKindAutomation, name: strings.TrimSpace(name), path: d.Path})
		}
	}
	if len(creates) == 0 || f.Binding.FeatureID == "" {
		return "", ""
	}
	inv, err := f.client().FeatureReferences(ctx, f.Binding.FeatureID, 0)
	if err != nil {
		return syncReasonLookupFailed, fmt.Sprintf("read what feature %s already holds, to make sure a create does not twin a row of the same name (GET /feature/:id/references): %v — nothing was written", f.Binding.FeatureID, err)
	}
	featureName := orUnknown(inv.FeatureName)
	for _, want := range creates {
		for _, omitted := range inv.OmittedKinds {
			if omitted.Kind == want.kind {
				return syncReasonLookupFailed, fmt.Sprintf("this credential may not list the %s rows of %s (%s), so nothing can say whether a create would twin one of the same name. Run with a credential that may read them. Nothing was written",
					want.kind, featureName, orUnknown(omitted.Reason))
			}
		}
		for _, res := range inv.Resources {
			if res.Kind != want.kind || strings.TrimSpace(res.Name) != want.name {
				continue
			}
			what := map[string]string{api.RefKindWorkflow: "a workflow", api.RefKindDataApp: "an app", api.RefKindModule: "a module", api.RefKindAutomation: "an automation"}[want.kind]
			where := ""
			if want.path != "" {
				where = want.path + ": "
			}
			return syncReasonNameTaken, fmt.Sprintf("%s%s already has %s called %s (%s), and --allow-create would make a second one beside it. Adopt it into this folder instead: record %s for stack %q in %s and apply again, or rename this one. Nothing was written",
				where, featureName, what, want.name, res.ID, adoptKey(want.kind, want.path, res.ID), f.Stack, wfdir.LockName)
		}
	}
	return "", ""
}

// adoptKey names the lock entry that makes a folder's stack own an existing row.
func adoptKey(kind, path, id string) string {
	switch kind {
	case api.RefKindWorkflow:
		return fmt.Sprintf(`"workflowID": %q`, id)
	case api.RefKindDataApp:
		return fmt.Sprintf(`"dataAppID": %q`, id)
	case api.RefKindModule:
		return fmt.Sprintf(`"moduleID": %q`, id)
	}
	return fmt.Sprintf(`"automations": {%q: {"automationID": %q}}`, path, id)
}
