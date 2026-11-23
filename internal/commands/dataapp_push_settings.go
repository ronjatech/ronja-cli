package commands

import (
	"fmt"

	"github.com/ronjatech/ronja-cli/internal/api"
	"github.com/ronjatech/ronja-cli/internal/wfdir"
)

// The SETTINGS a data app's push patches onto the row — the description and
// server-side data only — split out of dataapp_push.go as
// workflow_push_metadata.go is out of workflow_push.go: the patch, how a card
// names it, the drift guard's view of it, and that view less what a change
// list already declared.

// appSettingsPatch is the difference between the settings the manifest
// declares and the row's: empty when they agree or the folder manages none.
func appSettingsPatch(m *wfdir.Manifest, row *api.DataApp) api.DataAppPatch {
	var patch api.DataAppPatch
	if m.Description != nil && *m.Description != row.Description {
		description := *m.Description
		patch.Description = &description
	}
	if m.ServerDataOnly != nil && *m.ServerDataOnly != row.ServerDataOnly {
		on := *m.ServerDataOnly
		patch.ServerDataOnly = &on
	}
	return patch
}

// appSettingsChanges names each setting patch changes on row, as a card and a
// push report show it.
func appSettingsChanges(patch api.DataAppPatch, row *api.DataApp) []string {
	var out []string
	if patch.Description != nil {
		out = append(out, "description")
	}
	if patch.ServerDataOnly != nil {
		out = append(out, fmt.Sprintf("server-side data only: %s → %s", onOff(row.ServerDataOnly), onOff(*patch.ServerDataOnly)))
	}
	return out
}

// appSettingsDriftOf is metadataDriftOf for a data app's settings: each one
// the push would overwrite that moved on the server since the last sync.
func appSettingsDriftOf(baseline *wfdir.InstanceState, row *api.DataApp, patch api.DataAppPatch) []metadataDrift {
	if baseline == nil {
		return nil
	}
	var out []metadataDrift
	if patch.Description != nil && baseline.Description != nil && row.Description != *baseline.Description {
		out = append(out, metadataDrift{Field: "description", Baseline: *baseline.Description, Remote: row.Description})
	}
	if patch.ServerDataOnly != nil && baseline.ServerDataOnly != nil && row.ServerDataOnly != *baseline.ServerDataOnly {
		out = append(out, metadataDrift{Field: "server-side data only", Baseline: onOff(*baseline.ServerDataOnly), Remote: onOff(row.ServerDataOnly)})
	}
	return out
}

// withoutProposedAppSettings is patch less every setting the row already holds
// as the change list m (nil for none) declared it; see withoutProposedFields.
func withoutProposedAppSettings(patch api.DataAppPatch, row *api.DataApp, m *wfdir.Manifest) api.DataAppPatch {
	if m == nil {
		return patch
	}
	held := appSettingsPatch(m, row)
	if m.Description != nil && held.Description == nil {
		patch.Description = nil
	}
	if m.ServerDataOnly != nil && held.ServerDataOnly == nil {
		patch.ServerDataOnly = nil
	}
	return patch
}
