package commands

import (
	"encoding/json"
	"fmt"
	"os"
	"reflect"
	"strings"

	"github.com/ronjatech/ronja-cli/internal/api"
	"github.com/ronjatech/ronja-cli/internal/markers"
	"github.com/ronjatech/ronja-cli/internal/wfdir"
)

// The first automation import there has been: each automation of the feature
// becomes the automation loop's own curated `.json` file — exactly what `ronja
// automation init` and a hand-written file are — so `automation status`,
// `automation push` and `ronja sync` treat it like any other, with its
// `updatedAt` anchor recorded in the lock so the drift guard is armed from the
// first run.
//
// The file declares every field the row holds a value for, written the way the
// loop compares them (so the push's delta against the row is empty), and every
// id it names in alias form: a workflow or app imported into another folder by
// address, the automation's own feature as "@feature", and everything else as a
// declared data source bound in the importing stack. Then it is checked the way
// a push would check it — parsed, resolved through the folder's codec, and
// compared with the row — and a file that would not round-trip refuses the
// import before anything is written.

// automationFileFor renders one row as its file.
func (imp *featureImport) automationFileFor(f *importFolder, address string, row *api.Automation) (string, error) {
	alias := func(depKind, id string) string {
		if id == "" {
			return id
		}
		if depKind == markers.KindFeature && id == imp.feature.ID {
			return featureSelfRef
		}
		if a, ok := f.aliasOf[targetKey(depKind, id)]; ok {
			return a
		}
		return id
	}
	str := func(s string) *string {
		if s == "" {
			return nil
		}
		return &s
	}
	list := func(in []string, depKind string) *[]string {
		if len(in) == 0 {
			return nil
		}
		out := make([]string, 0, len(in))
		for _, v := range in {
			if depKind != "" {
				v = alias(depKind, v)
			}
			out = append(out, v)
		}
		return &out
	}

	file := automationFile{
		Description:               str(row.Description),
		TriggerKind:               str(row.TriggerKind),
		CronExpr:                  str(row.CronExpr),
		Timezone:                  str(row.Timezone),
		ReportingTimezone:         str(row.ReportingTimezone),
		Model:                     str(row.Model),
		ApprovedTools:             list(row.ApprovedTools, ""),
		ApprovedToolGroups:        list(row.ApprovedToolGroups, ""),
		EmailAllowedFromDomains:   list(row.EmailAllowedFromDomains, ""),
		EmailAllowedFromAddresses: list(row.EmailAllowedFromAddrs, ""),
		WatchedTableIds:           list(row.WatchedTableIDs, markers.KindTable),
		EventName:                 str(row.EventName),
		RateLimitPerMinute:        row.RateLimitPerMinute,
		RateLimitPerDay:           row.RateLimitPerDay,
	}
	enabled := row.Enabled
	file.Enabled = &enabled
	if automationNameFor(address) != row.Name {
		file.Name = str(row.Name)
	}
	if row.MailboxID != "" {
		file.MailboxID = str(alias(markers.KindMailbox, row.MailboxID))
	}
	if !reflect.DeepEqual(row.MailboxFilter, api.AutomationMailboxFilter{}) {
		filter := row.MailboxFilter
		file.MailboxFilter = &filter
	}
	if row.Action != nil {
		action := &automationActionFile{Kind: row.Action.Kind}
		cfg := row.Action.Config
		switch row.Action.Kind {
		case api.ActionKindWorkflow:
			action.Config.WorkflowID = str(alias(markers.KindWorkflow, cfg.WorkflowID))
			if len(cfg.ParameterValues) > 0 {
				values := cfg.ParameterValues
				action.Config.ParameterValues = &values
			}
		case api.ActionKindSavedAgent:
			action.Config.AgentID = str(alias(markers.KindAgent, cfg.AgentID))
			action.Config.Prompt = str(cfg.Prompt)
		case api.ActionKindAgent:
			file.Prompt = str(row.Prompt)
			if row.ReferenceBased {
				referenceBased := true
				file.ReferenceBased = &referenceBased
			}
			if len(cfg.References) > 0 {
				refs := make([]api.AutomationReference, 0, len(cfg.References))
				for _, r := range cfg.References {
					refs = append(refs, api.AutomationReference{Kind: r.Kind, ResourceID: alias(automationReferenceKinds[r.Kind], r.ResourceID)})
				}
				file.References = &refs
			}
		}
		file.Action = action
	}

	body, err := json.MarshalIndent(file, "", "  ")
	if err != nil {
		return "", err
	}
	text := string(body) + "\n"

	// The round trip, checked as a push checks it.
	parsed, err := parseAutomationFile(address, text)
	if err != nil {
		return "", fmt.Errorf("automation %s (%s) cannot be written as a file the automation loop accepts: %w", row.ID, row.Name, err)
	}
	resolved := *parsed
	if parsed.References != nil {
		refs := append([]api.AutomationReference{}, *parsed.References...)
		resolved.References = &refs
	}
	if parsed.WatchedTableIds != nil {
		ids := append([]string{}, *parsed.WatchedTableIds...)
		resolved.WatchedTableIds = &ids
	}
	if parsed.Action != nil {
		action := *parsed.Action
		resolved.Action = &action
	}
	if err := resolveAutomationRefs(&resolved, f.codec(), imp.stack, imp.feature.ID); err != nil {
		return "", fmt.Errorf("automation %s (%s): %w", row.ID, row.Name, err)
	}
	if err := refuseUnpatchableAutomation(&resolved, row); err != nil {
		return "", fmt.Errorf("automation %s (%s): %w", row.ID, row.Name, err)
	}
	if changed, _ := automationDelta(&resolved, row); len(changed) > 0 {
		return "", fmt.Errorf("automation %s (%s) would not read back as written — %s differ — so it was not imported; this is a bug in ronja", row.ID, row.Name, strings.Join(changed, ", "))
	}
	return text, nil
}

// writeAutomations lays the automation folder down: the files, ronja.json with
// the stack and declarations, and the lock with each row's id, its
// `updatedAt` anchor and — for a file that declares one — its applied name.
func (imp *featureImport) writeAutomations(f *importFolder, root string, hooks cloneHooks) error {
	if err := os.MkdirAll(root, 0o755); err != nil {
		return fmt.Errorf("create %s: %w", root, err)
	}
	for _, address := range sortedKeys(f.automationBodies) {
		if err := wfdir.WriteFile(root, address, f.automationBodies[address]); err != nil {
			return fmt.Errorf("write %s: %w", address, err)
		}
	}
	key := wfdir.InstanceKey{URL: imp.resolved.URL, TenantID: imp.resolved.TenantID}
	manifest := &wfdir.Manifest{Kind: wfdir.KindAutomation, Title: imp.feature.Name}
	binding := wfdir.Binding{FeatureID: imp.feature.ID, Automations: map[string]string{}}
	for address, row := range f.automations {
		binding.Automations[address] = row.ID
	}
	lock := &wfdir.Lock{}
	if err := recordFirstBindingInto(manifest, lock, key, binding); err != nil {
		return err
	}
	for _, address := range sortedKeys(f.automations) {
		row := f.automations[address]
		lock.SetAutomationSeen(imp.stack, address, row.ID, row.Stamp())
		if automationNameFor(address) != row.Name {
			lock.SetAutomationName(imp.stack, address, row.ID, row.Name)
		}
	}
	hooks.finishManifest(manifest)
	return wfdir.SaveFolder(root, manifest, lock)
}
