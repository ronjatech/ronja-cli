package commands

import (
	"fmt"
	"slices"
	"sort"
	"strings"

	"github.com/ronjatech/ronja-cli/internal/api"
	"github.com/ronjatech/ronja-cli/internal/markers"
)

// `ronja import feature --only <id>...` is the import restricted to the named
// rows and what they need inside their feature. It runs the whole import's
// planner and portability pass: a row of the feature left out of the set is
// classified as a row of another feature is, a declared dependency.

// onlyFeature reads the feature the named rows are in. They must all be in
// one, since a tree imports one feature.
func (imp *featureImport) onlyFeature(ids []string) (*api.Feature, error) {
	byFeature := map[string][]string{}
	for _, id := range ids {
		featureID, err := imp.rowFeatureOf(id)
		if err != nil {
			return nil, err
		}
		byFeature[featureID] = append(byFeature[featureID], id)
	}
	if len(byFeature) > 1 {
		var parts []string
		for _, featureID := range sortedKeys(byFeature) {
			parts = append(parts, fmt.Sprintf("%s in %s", strings.Join(byFeature[featureID], ", "), featureID))
		}
		return nil, fmt.Errorf("--only names rows of %d features (%s) — a tree imports one feature, so import each feature's rows on their own", len(byFeature), strings.Join(parts, "; "))
	}
	for featureID := range byFeature {
		return imp.resolveFeature(featureID)
	}
	return nil, fmt.Errorf("--only names no rows")
}

// rowFeatureOf is the feature one --only row is in, read by its own route.
func (imp *featureImport) rowFeatureOf(id string) (string, error) {
	var featureID string
	var err error
	switch {
	case markers.IsResourceID(markers.KindTable, id):
		var t *api.Table
		if t, err = imp.client.GetTable(imp.ctx, id); err == nil {
			featureID = t.FeatureID
		}
	case markers.IsResourceID(markers.KindWorkflow, id):
		var w *api.Workflow
		if w, err = imp.client.GetWorkflow(imp.ctx, id); err == nil {
			featureID = w.FeatureID
		}
	case markers.IsResourceID(markers.KindDataApp, id):
		var a *api.DataApp
		if a, err = imp.client.GetDataApp(imp.ctx, id); err == nil {
			featureID = a.FeatureID
		}
	case markers.IsResourceID(markers.KindModule, id):
		var m *api.Module
		if m, err = imp.client.GetModule(imp.ctx, id); err == nil {
			featureID = m.FeatureID
		}
	default:
		var a *api.Automation
		if a, err = imp.client.GetAutomation(imp.ctx, id); err == nil {
			featureID = a.FeatureID
		}
	}
	if err != nil {
		return "", fmt.Errorf("--only %s: read it to find its feature: %w — pass the id of a table, workflow, data app, module or automation you can read", id, err)
	}
	if featureID == "" {
		return "", fmt.Errorf("--only %s is in no feature, so there is no feature to import it from", id)
	}
	return featureID, nil
}

// onlyClosure is the --only rows and everything they need inside the
// feature, transitively: every row a member references (an app's workflows
// and tables, a workflow's modules and tables, a derived table's inputs), the
// workflow that writes a member output table, every other workflow that writes
// a member, by marker or by id, and every automation whose action runs a
// member. writers lists the workflows carried only because they write one.
func onlyClosure(resources []api.ReferenceResource, seeds []string, featureName string) (in map[string]bool, writers []importWriterCarried, err error) {
	byID := map[string]api.ReferenceResource{}
	for _, res := range resources {
		byID[res.ID] = res
	}
	in = map[string]bool{}
	var queue []string
	add := func(id string) bool {
		if _, listed := byID[id]; listed && !in[id] {
			in[id] = true
			queue = append(queue, id)
			return true
		}
		return false
	}
	for _, id := range seeds {
		if _, listed := byID[id]; !listed {
			return nil, nil, fmt.Errorf("--only %s is not among the live rows of %s this credential can read, so it cannot be imported", id, featureName)
		}
		add(id)
	}
	for len(queue) > 0 {
		for len(queue) > 0 {
			res := byID[queue[0]]
			queue = queue[1:]
			for _, ref := range res.References {
				add(ref.TargetID)
			}
			add(res.OwnerWorkflowID)
		}
		for _, res := range resources {
			if in[res.ID] {
				continue
			}
			switch res.Kind {
			case api.RefKindAutomation:
				for _, ref := range res.References {
					if ref.Position == api.RefPosAutomationAction && in[ref.TargetID] {
						add(res.ID)
						break
					}
				}
			case api.RefKindWorkflow:
				// A table an app reads may be filled by a workflow that writes
				// it by id and owns nothing, and leaving that workflow out
				// would deploy a snapshot nothing refreshes.
				var writes []string
				for _, ref := range res.References {
					if ref.Writes() && in[ref.TargetID] && !slices.Contains(writes, ref.TargetID) {
						writes = append(writes, ref.TargetID)
					}
				}
				if len(writes) > 0 && add(res.ID) {
					writers = append(writers, importWriterCarried{ID: res.ID, Name: res.Name, Writes: writes})
				}
			}
		}
	}
	return in, writers, nil
}

// importWriterCarried is a workflow --only carried because it writes a row
// the import needs.
type importWriterCarried struct {
	ID     string   `json:"id"`
	Name   string   `json:"name"`
	Writes []string `json:"writes"`
}

// importAlsoFires is an imported automation that keeps running in the
// feature it came from after another stack deploys its copy.
type importAlsoFires struct {
	Folder string `json:"folder"`
	Path   string `json:"path"`
	ID     string `json:"id"`
	Name   string `json:"name"`
}

// alsoFires lists the automations an --only import carries.
func (imp *featureImport) alsoFires() []importAlsoFires {
	out := []importAlsoFires{}
	if len(imp.only) == 0 {
		return out
	}
	for _, f := range imp.folders {
		if f.withdrawn {
			continue
		}
		for _, row := range f.rows {
			if row.res.Kind == api.RefKindAutomation {
				out = append(out, importAlsoFires{Folder: f.Rel, Path: row.address, ID: row.res.ID, Name: row.res.Name})
			}
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Folder+out[i].Path < out[j].Folder+out[j].Path })
	return out
}
