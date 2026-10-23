package commands

import (
	"context"
	"encoding/json"
	"fmt"
	"maps"
	"reflect"
	"strings"

	"github.com/ronjatech/ronja-cli/internal/api"
	"github.com/ronjatech/ronja-cli/internal/markers"
)

// `ronja automation pull`: each automation this folder manages on the stack,
// read live and written back into its curated `.json`.
//
// THREE-STATE IS KEPT. A key the file leaves out is a field the folder does
// not manage, and a pull does not start managing it: only the keys the file
// already declares take the live values (an `enabled` the file never declared
// stays undeclared, however the row is set). A claimed automation whose file is
// gone from the folder is written whole, the way `ronja import feature` writes
// one. A file the server holds no claim for has never been deployed from here,
// and is left exactly as it is.
//
// ALIASES ARE KEPT FIELD-WISE. A field whose spelling already resolves to the
// live id keeps that spelling; one that does not takes the alias this folder's
// codec names for the live id, "@feature" for the stack's own feature, and the
// literal id otherwise — which is reported, never turned into a new alias.
func pullAutomation(ctx context.Context, client *api.Client, f *folder) (*pullPlan, error) {
	local, _, err := readAutomationFiles(f.Root)
	if err != nil {
		return nil, err
	}
	parsed, problems := parseAutomationFolder(local)
	if len(problems) > 0 {
		var lines []string
		for _, path := range sortedKeys(problems) {
			lines = append(lines, fmt.Sprintf("%s: %v", path, problems[path]))
		}
		return nil, fmt.Errorf("these files are not valid automation declarations, so nothing was pulled — fix them first:\n    %s",
			strings.Join(lines, "\n    "))
	}

	plan := &pullPlan{files: map[string]*string{}}
	for _, address := range sortedKeys(f.managed.claims) {
		claim := f.managed.claims[address]
		row, err := client.GetAutomation(ctx, claim.ResourceID)
		if err != nil {
			if api.StatusOf(err) == 404 {
				return nil, &managedRefusal{Reason: syncReasonLost, Detail: fmt.Sprintf(
					"automation %s, which this folder manages at %s on stack %q, cannot be read (deleted, or out of your reach) — there is nothing live to pull. Run `ronja sync apply --stack %s` to see the claim's state and its remedy",
					claim.ResourceID, address, f.Stack, f.Stack)}
			}
			return nil, fmt.Errorf("read automation %s: %w", claim.ResourceID, err)
		}
		have := parsed[address]
		if have != nil && automationFileHolds(f, have, row) {
			// The file already declares exactly what the row holds: its bytes
			// are the author's, and stay.
			continue
		}
		file, literals := pulledAutomationFile(f, address, have, row)
		text, err := renderPulledAutomation(f, address, file, row)
		if err != nil {
			return nil, err
		}
		if before, ok := local[address]; ok && before == text {
			continue
		}
		plan.files[address] = &text
		plan.literals = append(plan.literals, literals...)
	}
	plan.gate = func(ctx context.Context) error {
		planned := maps.Clone(local)
		for path, content := range plan.files {
			if content == nil {
				delete(planned, path)
				continue
			}
			planned[path] = *content
		}
		if files := automationGateFiles(f, planned); files != nil {
			return gateAutomationReferences(ctx, client, f, files)
		}
		return nil
	}
	plan.anchor = func(ctx context.Context, f *folder) ([]string, error) {
		res, err := runAutomationPush(ctx, f, automationPushOptions{Pull: true})
		if err != nil {
			return nil, err
		}
		var pending []string
		for _, file := range res.Files {
			switch file.Outcome {
			case automationOutcomeUnchanged:
			case automationOutcomePending:
				pending = append(pending, fmt.Sprintf("%s — %s", file.Path, file.Error))
			default:
				return nil, fmt.Errorf("%s: the push in pull mode answered %q", file.Path, file.Outcome)
			}
		}
		return pending, nil
	}
	return plan, nil
}

// automationFileHolds reports that a file, resolved through the folder's
// codec, declares nothing the row does not already hold.
func automationFileHolds(f *folder, file *automationFile, row *api.Automation) bool {
	resolved := copyAutomationFile(file)
	if err := resolveAutomationRefs(resolved, f.Codec, f.Stack, f.Binding.FeatureID); err != nil {
		return false
	}
	changed, _ := automationDelta(resolved, row)
	return len(changed) == 0
}

// copyAutomationFile is a deep copy through the file's own JSON shape, so a
// resolve cannot write through into the original.
func copyAutomationFile(in *automationFile) *automationFile {
	body, _ := json.Marshal(in)
	var out automationFile
	_ = json.Unmarshal(body, &out)
	return &out
}

// pulledAutomationFile is the file the live row makes: every key `have`
// declares (all of them, for a file not in the folder), valued from the row.
func pulledAutomationFile(f *folder, address string, have *automationFile, row *api.Automation) (*automationFile, []pullLiteral) {
	full := have == nil
	declares := func(present bool) bool { return full || present }
	var literals []pullLiteral
	// spell keeps the file's own spelling of a reference when it still names
	// the live id, and otherwise takes the folder's alias for it, "@feature",
	// or the literal id — reported.
	spell := func(where, kind, current, id string) string {
		if id == "" {
			return id
		}
		if current != "" {
			if resolved, ok := resolveAutomationSpelling(f, kind, current); ok && resolved == id {
				return current
			}
		}
		if kind == markers.KindFeature && id == f.Binding.FeatureID {
			return featureSelfRef
		}
		if alias, ok := f.Codec.toAlias[aliasKey{kind: kind, name: id}]; ok {
			return alias
		}
		if current != id {
			literals = append(literals, pullLiteral{Path: address, Field: where, Kind: kind, ID: id})
		}
		return id
	}
	str := func(s string) *string { return &s }
	list := func(in []string) *[]string {
		out := append([]string{}, in...)
		return &out
	}
	if have == nil {
		have = &automationFile{}
	}
	out := &automationFile{}

	if have.Name != nil || (full && automationNameFor(address) != row.Name) {
		out.Name = str(row.Name)
	}
	if declares(have.Description != nil) && (!full || row.Description != "") {
		out.Description = str(row.Description)
	}
	if declares(have.TriggerKind != nil) && (!full || row.TriggerKind != "") {
		out.TriggerKind = str(row.TriggerKind)
	}
	if declares(have.CronExpr != nil) && (!full || row.CronExpr != "") {
		out.CronExpr = str(row.CronExpr)
	}
	if declares(have.Timezone != nil) && (!full || row.Timezone != "") {
		out.Timezone = str(row.Timezone)
	}
	if declares(have.ReportingTimezone != nil) && (!full || row.ReportingTimezone != "") {
		out.ReportingTimezone = str(row.ReportingTimezone)
	}
	if declares(have.Enabled != nil) {
		enabled := row.Enabled
		out.Enabled = &enabled
	}
	if declares(have.Model != nil) && (!full || row.Model != "") {
		out.Model = str(row.Model)
	}
	if declares(have.ApprovedTools != nil) && (!full || len(row.ApprovedTools) > 0) {
		out.ApprovedTools = list(row.ApprovedTools)
	}
	if declares(have.ApprovedToolGroups != nil) && (!full || len(row.ApprovedToolGroups) > 0) {
		out.ApprovedToolGroups = list(row.ApprovedToolGroups)
	}
	if declares(have.EmailAllowedFromDomains != nil) && (!full || len(row.EmailAllowedFromDomains) > 0) {
		out.EmailAllowedFromDomains = list(row.EmailAllowedFromDomains)
	}
	if declares(have.EmailAllowedFromAddresses != nil) && (!full || len(row.EmailAllowedFromAddrs) > 0) {
		out.EmailAllowedFromAddresses = list(row.EmailAllowedFromAddrs)
	}
	if declares(have.WatchedTableIds != nil) && (!full || len(row.WatchedTableIDs) > 0) {
		ids := make([]string, 0, len(row.WatchedTableIDs))
		for i, id := range row.WatchedTableIDs {
			current := ""
			if have.WatchedTableIds != nil && i < len(*have.WatchedTableIds) {
				current = (*have.WatchedTableIds)[i]
			}
			ids = append(ids, spell(fmt.Sprintf("watchedTableIds[%d]", i), markers.KindTable, current, id))
		}
		out.WatchedTableIds = &ids
	}
	if declares(have.EventName != nil) && (!full || row.EventName != "") {
		out.EventName = str(row.EventName)
	}
	if declares(have.MailboxID != nil) && (!full || row.MailboxID != "") {
		current := ""
		if have.MailboxID != nil {
			current = *have.MailboxID
		}
		out.MailboxID = str(spell("mailboxID", markers.KindMailbox, current, row.MailboxID))
	}
	if declares(have.MailboxFilter != nil) && (!full || !reflect.DeepEqual(row.MailboxFilter, api.AutomationMailboxFilter{})) {
		filter := row.MailboxFilter
		out.MailboxFilter = &filter
	}
	if declares(have.RateLimitPerMinute != nil) && row.RateLimitPerMinute != nil {
		v := *row.RateLimitPerMinute
		out.RateLimitPerMinute = &v
	}
	if declares(have.RateLimitPerDay != nil) && row.RateLimitPerDay != nil {
		v := *row.RateLimitPerDay
		out.RateLimitPerDay = &v
	}
	if row.Action != nil && declares(have.Action != nil) {
		action := &automationActionFile{Kind: row.Action.Kind}
		cfg := row.Action.Config
		var hc automationActionConfigFile
		if have.Action != nil {
			hc = have.Action.Config
		}
		switch row.Action.Kind {
		case api.ActionKindWorkflow:
			if declares(hc.WorkflowID != nil) {
				current := ""
				if hc.WorkflowID != nil {
					current = *hc.WorkflowID
				}
				action.Config.WorkflowID = str(spell("action.config.workflowID", markers.KindWorkflow, current, cfg.WorkflowID))
			}
			if declares(hc.ParameterValues != nil) && (!full || len(cfg.ParameterValues) > 0) {
				values := cfg.ParameterValues
				if values == nil {
					values = map[string]any{}
				}
				action.Config.ParameterValues = &values
			}
		case api.ActionKindSavedAgent:
			if declares(hc.AgentID != nil) {
				current := ""
				if hc.AgentID != nil {
					current = *hc.AgentID
				}
				action.Config.AgentID = str(spell("action.config.agentID", markers.KindAgent, current, cfg.AgentID))
			}
			if declares(hc.Prompt != nil) && (!full || cfg.Prompt != "") {
				action.Config.Prompt = str(cfg.Prompt)
			}
		case api.ActionKindAgent:
			if declares(have.Prompt != nil) && (!full || row.Prompt != "") {
				out.Prompt = str(row.Prompt)
			}
			if declares(have.ReferenceBased != nil) && (!full || row.ReferenceBased) {
				rb := row.ReferenceBased
				out.ReferenceBased = &rb
			}
			if declares(have.References != nil) && (!full || len(cfg.References) > 0) {
				refs := make([]api.AutomationReference, 0, len(cfg.References))
				for i, r := range cfg.References {
					current := ""
					if have.References != nil && i < len(*have.References) && (*have.References)[i].Kind == r.Kind {
						current = (*have.References)[i].ResourceID
					}
					refs = append(refs, api.AutomationReference{Kind: r.Kind,
						ResourceID: spell(fmt.Sprintf("references[%d].resourceID", i), automationReferenceKinds[r.Kind], current, r.ResourceID)})
				}
				out.References = &refs
			}
		}
		out.Action = action
	}
	return out, literals
}

// resolveAutomationSpelling resolves one field spelling as a push would.
func resolveAutomationSpelling(f *folder, kind, spelling string) (string, bool) {
	switch {
	case kind == markers.KindFeature && spelling == featureSelfRef:
		return f.Binding.FeatureID, f.Binding.FeatureID != ""
	case markers.IsResourceID(kind, spelling):
		return spelling, true
	}
	return f.Codec.resolve(kind, spelling)
}

// renderPulledAutomation lays the file out and checks it the way a push
// checks it — parsed, resolved through the folder's codec and compared with
// the row — so a pull that would not read back is refused before anything is
// written.
func renderPulledAutomation(f *folder, address string, file *automationFile, row *api.Automation) (string, error) {
	body, err := json.MarshalIndent(file, "", "  ")
	if err != nil {
		return "", err
	}
	text := string(body) + "\n"
	parsed, err := parseAutomationFile(address, text)
	if err != nil {
		return "", pullInexpressible(f, fmt.Sprintf("%s would not be a file the automation loop accepts: %v", address, err))
	}
	resolved := copyAutomationFile(parsed)
	if err := resolveAutomationRefs(resolved, f.Codec, f.Stack, f.Binding.FeatureID); err != nil {
		return "", pullInexpressible(f, fmt.Sprintf("%s: %v", address, err))
	}
	if err := refuseUnpatchableAutomation(resolved, row); err != nil {
		return "", pullInexpressible(f, fmt.Sprintf("%s: %v", address, err))
	}
	if changed, _ := automationDelta(resolved, row); len(changed) > 0 {
		return "", pullInexpressible(f, fmt.Sprintf("%s would still differ from automation %s in %s", address, row.ID, strings.Join(changed, ", ")))
	}
	return text, nil
}
