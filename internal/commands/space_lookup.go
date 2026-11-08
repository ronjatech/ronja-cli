package commands

import (
	"context"

	"github.com/ronjatech/ronja-cli/internal/api"
	"github.com/ronjatech/ronja-cli/internal/config"
	"github.com/ronjatech/ronja-cli/internal/markers"
)

// spaceLookup answers which Space a row is in, asking once per row and once
// per feature.
type spaceLookup struct {
	ctx      context.Context
	client   *api.Client
	features map[string]featureAnswer
	spaces   map[string]spaceAnswer
	rows     map[string]rowAnswer
}

type spaceAnswer struct {
	space *api.Space
	err   error
}

type featureAnswer struct {
	feature *api.Feature
	err     error
}

type rowAnswer struct {
	featureID, name string
	// copiedFrom is a table's CopiedFromModelID.
	copiedFrom string
	err        error
}

func newSpaceLookup(ctx context.Context, resolved *config.Resolved) *spaceLookup {
	return newSpaceLookupOn(ctx, newClient(resolved.URL, resolved.Token))
}

func newSpaceLookupOn(ctx context.Context, client *api.Client) *spaceLookup {
	return &spaceLookup{ctx: ctx, client: client,
		features: map[string]featureAnswer{}, spaces: map[string]spaceAnswer{}, rows: map[string]rowAnswer{}}
}

func (l *spaceLookup) feature(id string) (*api.Feature, error) {
	got, asked := l.features[id]
	if !asked {
		got.feature, got.err = l.client.GetFeature(l.ctx, id)
		l.features[id] = got
	}
	return got.feature, got.err
}

func (l *spaceLookup) space(id string) (*api.Space, error) {
	got, asked := l.spaces[id]
	if !asked {
		got.space, got.err = l.client.GetSpace(l.ctx, id)
		l.spaces[id] = got
	}
	return got.space, got.err
}

// spaceName is the Space's current name, or "" when it cannot be read.
func (l *spaceLookup) spaceName(id string) string {
	if space, err := l.space(id); err == nil {
		return space.Name
	}
	return ""
}

// describeSpace names a Space for a message: its name when it can be read.
func (l *spaceLookup) describeSpace(id string) string {
	if id == "" {
		return "no Space"
	}
	if name := l.spaceName(id); name != "" {
		return "Space " + name
	}
	return "Space " + id
}

// rowFeature reads the feature a row of a dependency kind belongs to. ok is
// false for a kind with no read that names one.
func (l *spaceLookup) rowFeature(kind, id string) (featureID, name string, ok bool, err error) {
	if kind == markers.KindFeature {
		return id, "", true, nil
	}
	key := rowKey(kind, id)
	got, asked := l.rows[key]
	if asked {
		return got.featureID, got.name, true, got.err
	}
	switch kind {
	case markers.KindTable:
		t, e := l.client.GetTable(l.ctx, id)
		if e == nil {
			got.featureID, got.name, got.copiedFrom = t.FeatureID, t.Name, t.CopiedFromModelID
		}
		got.err = e
	case markers.KindWorkflow:
		w, e := l.client.GetWorkflow(l.ctx, id)
		if e == nil {
			got.featureID, got.name = w.FeatureID, w.Title
		}
		got.err = e
	case markers.KindAgent:
		a, e := l.client.GetAgent(l.ctx, id)
		if e == nil {
			got.featureID, got.name = a.FeatureID, a.Name
		}
		got.err = e
	case markers.KindSecret:
		s, e := l.client.GetSecret(l.ctx, id)
		if e == nil {
			got.featureID, got.name = s.FeatureID, s.Name
		}
		got.err = e
	case markers.KindCodex:
		c, e := l.client.GetCodex(l.ctx, id)
		if e == nil {
			got.featureID, got.name = c.FeatureID, c.Name
		}
		got.err = e
	case markers.KindDataApp:
		a, e := l.client.GetDataApp(l.ctx, id)
		if e == nil {
			got.featureID, got.name = a.FeatureID, a.Name
		}
		got.err = e
	case markers.KindModule:
		m, e := l.client.GetModule(l.ctx, id)
		if e == nil {
			got.featureID, got.name = m.FeatureID, m.Name
		}
		got.err = e
	default:
		return "", "", false, nil
	}
	l.rows[key] = got
	return got.featureID, got.name, true, got.err
}

func rowKey(kind, id string) string { return kind + "\x00" + id }

// copiedFrom is the table a table is a snapshot copy of, "" for one that is
// not or cannot be read.
func (l *spaceLookup) copiedFrom(id string) string {
	if _, _, _, err := l.rowFeature(markers.KindTable, id); err != nil {
		return ""
	}
	return l.rows[rowKey(markers.KindTable, id)].copiedFrom
}

// rowSpace is the Space a row is in: its feature's. featureID is "" for a row
// in no feature, which is in no Space.
func (l *spaceLookup) rowSpace(kind, id string) (spaceID, featureID, name string, ok bool, err error) {
	featureID, name, ok, err = l.rowFeature(kind, id)
	if !ok || err != nil || featureID == "" {
		return "", featureID, name, ok, err
	}
	feature, err := l.feature(featureID)
	if err != nil {
		return "", featureID, name, true, err
	}
	return feature.SpaceID, featureID, name, true, nil
}
