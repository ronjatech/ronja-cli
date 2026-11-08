package commands

import (
	"context"
	"fmt"
	"os"

	"github.com/ronjatech/ronja-cli/internal/api"
	"github.com/ronjatech/ronja-cli/internal/markers"
	"github.com/ronjatech/ronja-cli/internal/wfdir"
)

// bindPicker is `ronja bind` on a stack that records a Space, or that this
// run declares on a feature in one (the default Space included). The search
// covers the whole organization, so every candidate is placed first:
//
//   - a READ dependency is proposed only when exactly one row of that name is
//     in the stack's Space;
//   - a WRITE or RUN dependency only when exactly one is in the stack's own
//     feature, the one place the write gate treats as home;
//   - a row in no feature is in no Space and is never proposed.
//
// Everything else is reported unbound, for a person to bind by hand. Any
// other stack has a picker with no Space: a read is proposed on one match
// across the organization, and a write or run target never.
type bindPicker struct {
	spaceID, spaceName string
	featureID          string
	// records is set when the plan's write records the Space on the stack.
	records bool
	lookup  *spaceLookup
}

// bindPickerOf is the picker for the selected stack. space is the Space of
// the feature a declaring run names (declaredStackSpace): the picker proposes
// from it, default Space included, and records it when it is outside the
// default Space. Nil leaves the stack's own recorded Space, if any.
func bindPickerOf(f *folder, l *spaceLookup, featureID string, space *api.Space) bindPicker {
	stack := f.Manifest.Stacks[f.Stack]
	p := bindPicker{spaceID: stack.SpaceID, spaceName: stack.Space, featureID: stack.FeatureID, lookup: l}
	if space != nil {
		p.spaceID, p.spaceName, p.featureID = space.ID, space.Name, featureID
		p.records = stack.SpaceID == "" && recordedSpace(space) != nil
	}
	if p.spaceID != "" && p.spaceName == "" {
		p.spaceName = l.spaceName(p.spaceID)
	}
	return p
}

// declaredStackSpace is the Space of the feature a declaring run names: for a
// stack it declares, which refuses when the Space cannot be read; and for a
// stack that already names the feature and records no Space, which gains it
// when it is outside the default Space (target.declares and spaceRecorded say
// so). Nil when the run declares nothing.
func declaredStackSpace(f *folder, l *spaceLookup, featureID string, target *bindTarget) (space *api.Space, spaceRecorded string, err error) {
	if featureID == "" || f.Stack == "" {
		return nil, "", nil
	}
	if target.declares != "" {
		space, err := featureSpace(l, featureID)
		if err != nil {
			return nil, "", declareSpaceFailed(featureID, f.Stack, err)
		}
		return space, "", nil
	}
	stack, declared := f.Manifest.Stacks[f.Stack]
	if !declared || stack.FeatureID != featureID || stack.SpaceID != "" {
		return nil, "", nil
	}
	space, err = featureSpace(l, featureID)
	if err != nil {
		fmt.Fprintf(os.Stderr, "  Note: could not read which Space feature %s is in (%v), so stack %q's Space is not recorded; run this again once the instance answers.\n", featureID, err, f.Stack)
		return nil, "", nil
	}
	if space = recordedSpace(space); space == nil {
		return nil, "", nil
	}
	target.declares = fmt.Sprintf("stack %q's Space (%s)", f.Stack, space.Name)
	return space, space.Name, nil
}

func (p bindPicker) propose(ctx context.Context, client *api.Client, f *folder, alias string, dep wfdir.Dependency) (*bindProposal, *bindUnresolved, error) {
	if proposal := p.copySource(ctx, client, f, alias, dep); proposal != nil {
		return proposal, nil, nil
	}
	scope := &bindScope{picker: p, kind: dep.Kind, inFeature: dep.Write || dep.Run}
	proposal, unresolved, err := proposeBind(ctx, client, alias, dep.Kind, scope)
	if unresolved == nil {
		return proposal, unresolved, err
	}
	copyable := unresolved.Reason == bindReasonNoMatch && dep.Kind == markers.KindTable && otherStackBinds(f, alias)
	if scope.inFeature {
		reason, verb := bindReasonWriteTarget, "writes"
		if !dep.Write {
			reason, verb = bindReasonRunTarget, "runs"
		}
		unresolved.Reason = reason
		unresolved.Detail += fmt.Sprintf(" — this folder %s it, so only a row in this stack's own feature is proposed; set it by hand in %s under this stack's \"bind\", to a row in Space %s",
			verb, wfdir.ManifestName, p.spaceName)
		if copyable && dep.Write {
			unresolved.Copy = fmt.Sprintf("ronja copy %s --no-rows --to-stack %s", alias, f.Stack)
			unresolved.Detail += fmt.Sprintf(", or create it empty there: `%s`", unresolved.Copy)
		}
	} else if copyable {
		unresolved.Copy = fmt.Sprintf("ronja copy %s --to-stack %s", alias, f.Stack)
		unresolved.Detail += fmt.Sprintf(", or copy it in: `%s`", unresolved.Copy)
	}
	return proposal, unresolved, err
}

// otherStackBinds reports whether another stack of the folder on this
// organization binds alias, so `ronja copy` has a source for it: a copy never
// crosses organizations.
func otherStackBinds(f *folder, alias string) bool {
	for _, name := range f.Manifest.StackNames() {
		st := f.Manifest.Stacks[name]
		if name != f.Stack && wfdir.Matches(st.Key(), f.Key) && st.Bind[alias] != "" {
			return true
		}
	}
	return false
}

// bindScope narrows a search's name matches to the rows one dependency of a
// Space stack may bind.
type bindScope struct {
	picker    bindPicker
	kind      string
	inFeature bool
}

// filter keeps the hits the dependency may bind. A candidate the caller
// cannot read is dropped, as one in no Space is; a read that failed otherwise
// is returned, since dropping it could leave a single wrong candidate.
func (s *bindScope) filter(hits []api.SearchHit) ([]api.SearchHit, error) {
	var out []api.SearchHit
	for _, hit := range hits {
		space, feature, _, ok, err := s.picker.lookup.rowSpace(s.kind, hit.ID)
		if err != nil && !unreachable(err) {
			return nil, fmt.Errorf("check which Space %s %s is in: %w — nothing was written", s.kind, hit.ID, err)
		}
		if !ok || err != nil || feature == "" {
			continue
		}
		if s.inFeature && feature == s.picker.featureID || !s.inFeature && space == s.picker.spaceID {
			out = append(out, hit)
		}
	}
	return out, nil
}

func (s *bindScope) noMatch(kind, alias string) string {
	if s.inFeature {
		return fmt.Sprintf("no %s in this stack's feature %s is called %q", kind, s.picker.featureID, alias)
	}
	return fmt.Sprintf("no %s in Space %s is called %q, even ignoring case and separators — bind another Space's row by hand in %s to read it live", kind, s.picker.spaceName, alias, wfdir.ManifestName)
}

// copySource proposes, for a table another stack of this folder binds to a
// snapshot copy, the table that copy was made from — by id, not by name — when
// it is in this stack's Space. A write target never is: the copy's source is
// another stack's row by construction.
func (p bindPicker) copySource(ctx context.Context, client *api.Client, f *folder, alias string, dep wfdir.Dependency) *bindProposal {
	if dep.Kind != markers.KindTable || dep.Write || dep.Run {
		return nil
	}
	for _, name := range f.Manifest.StackNames() {
		id := f.Manifest.Stacks[name].Bind[alias]
		if name == f.Stack || id == "" {
			continue
		}
		snapshot, err := client.GetTable(ctx, id)
		if err != nil || snapshot.CopiedFromModelID == "" {
			continue
		}
		space, feature, title, ok, err := p.lookup.rowSpace(markers.KindTable, snapshot.CopiedFromModelID)
		if ok && err == nil && feature != "" && space == p.spaceID {
			return &bindProposal{Alias: alias, Kind: dep.Kind, ID: snapshot.CopiedFromModelID, Title: title, Source: id}
		}
	}
	return nil
}

// refuseDeclaringOverAnotherFeaturesRow refuses to give a feature to a bound
// stack that names none (a legacy "instances" entry being named, or a stack
// written by hand) when the row it already deploys is in another feature, or
// cannot be read: the stack would then deploy that row as the new feature's.
func refuseDeclaringOverAnotherFeaturesRow(f *folder, l *spaceLookup, featureID string, target bindTarget) error {
	if target.declares == "" || featureID == "" || !f.Bound || f.Manifest.Stacks[f.Stack].FeatureID != "" {
		return nil
	}
	kind, id := f.boundRow()
	if id == "" {
		return nil
	}
	rowFeature, _, ok, err := l.rowFeature(kind, id)
	switch {
	case !ok:
		return nil
	case err != nil:
		return fmt.Errorf("stack %q already deploys the %s %s, and which feature that row is in could not be read (%v), so it cannot be declared on feature %s — %s was not changed; try again once the instance answers",
			f.Stack, kind, id, err, featureID, wfdir.ManifestName)
	case rowFeature == "":
		return fmt.Errorf("stack %q already deploys the %s %s, which is in no feature, so it cannot be declared on feature %s — %s was not changed",
			f.Stack, kind, id, featureID, wfdir.ManifestName)
	case rowFeature != featureID:
		return fmt.Errorf("stack %q already deploys the %s %s, which is in feature %s, so declaring it on feature %s would deploy that row as the other feature's. Declare it on the feature it is in (--feature %s), then declare a second stack beside it for %s — %s was not changed",
			f.Stack, kind, id, rowFeature, featureID, rowFeature, featureID, wfdir.ManifestName)
	}
	return nil
}
