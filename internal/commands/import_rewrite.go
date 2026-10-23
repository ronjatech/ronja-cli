package commands

import (
	"bytes"
	"encoding/json"
	"fmt"

	"github.com/ronjatech/ronja-cli/internal/api"
	"github.com/ronjatech/ronja-cli/internal/markers"
	"github.com/ronjatech/ronja-cli/internal/wfdir"
)

// The REWRITE half of `ronja import feature`: the folder a plan decided, with
// every reference rewritten into the form the portability pass chose — a stem,
// an address or a declared alias — in its files, its metric recipes and its
// app access block. Split from import.go.

// rewrite puts each folder's OUTBOUND-ONLY positions, and a pipeline's SQL and
// recipes, into alias form; the both-ways positions are de-aliased by the
// codec as the shared writers lay the files down.
func (imp *featureImport) rewrite(f *importFolder) error {
	codec := f.codec()
	switch f.Kind.Name {
	case wfdir.KindPipeline:
		stems := map[string]string{}
		for _, s := range f.pipeline.sources {
			stems[s.TableID] = tableNameFor(s.Path)
		}
		disk := func(code string) string {
			return rewriteMarkers(code, func(o markers.Occurrence) string {
				if o.Family.Kind == markers.KindTable {
					if stem, ok := stems[o.Arg]; ok {
						return stem
					}
				}
				if alias, ok := codec.toAlias[aliasKey{kind: o.Family.Kind, name: o.Arg}]; ok {
					return alias
				}
				return o.Arg
			})
		}
		for i := range f.pipeline.sources {
			s := &f.pipeline.sources[i]
			s.Code = disk(s.Code)
			if s.LiveCode != "" {
				s.LiveCode = disk(s.LiveCode)
			}
		}
		for i := range f.pipeline.metrics {
			m := &f.pipeline.metrics[i]
			body, err := rewriteMetricSource(m.Body, func(id string) string {
				if stem, ok := stems[id]; ok {
					return stem
				}
				if alias, ok := codec.toAlias[aliasKey{kind: markers.KindTable, name: id}]; ok {
					return alias
				}
				return id
			})
			if err != nil {
				return fmt.Errorf("rewrite the metric file %s: %w", m.Path, err)
			}
			m.Body = body
		}
	case wfdir.KindDataApp:
		access := aliasedAccess(f.app.DataAppAccess, codec)
		f.access = &access
	case wfdir.KindAutomation:
		f.automationBodies = map[string]string{}
		for address, row := range f.automations {
			body, err := imp.automationFileFor(f, address, row)
			if err != nil {
				return err
			}
			f.automationBodies[address] = body
		}
	}
	return nil
}

// rewriteMetricSource rewrites a metric file's recipe.source through sub,
// leaving every other byte of the recipe as it was: only the source value is
// replaced inside the recipe — its keys keep their order — and the file is
// encoded exactly as renderMetricFile encodes it (pipeline clone's writer), so
// an imported metric file differs from a cloned one in that one value.
func rewriteMetricSource(body string, sub func(string) string) (string, error) {
	var file map[string]json.RawMessage
	if err := json.Unmarshal([]byte(body), &file); err != nil {
		return "", err
	}
	var recipe struct {
		Source string `json:"source"`
	}
	if err := json.Unmarshal(file["recipe"], &recipe); err != nil {
		return "", err
	}
	next := sub(recipe.Source)
	if next == recipe.Source {
		return body, nil
	}
	rewritten, err := replaceObjectString(file["recipe"], "source", next)
	if err != nil {
		return "", err
	}
	file["recipe"] = rewritten
	out, err := json.MarshalIndent(file, "", "  ")
	if err != nil {
		return "", err
	}
	return string(out) + "\n", nil
}

// replaceObjectString replaces the value of one top-level key of a JSON
// object with a string, every other byte left where it was.
func replaceObjectString(raw json.RawMessage, key, value string) (json.RawMessage, error) {
	dec := json.NewDecoder(bytes.NewReader(raw))
	if tok, err := dec.Token(); err != nil || tok != json.Delim('{') {
		return nil, fmt.Errorf("not a JSON object")
	}
	for dec.More() {
		tok, err := dec.Token()
		if err != nil {
			return nil, err
		}
		name, _ := tok.(string)
		start := dec.InputOffset()
		var skip json.RawMessage
		if err := dec.Decode(&skip); err != nil {
			return nil, err
		}
		if name != key {
			continue
		}
		end := dec.InputOffset()
		encoded, err := json.Marshal(value)
		if err != nil {
			return nil, err
		}
		out := append([]byte{}, raw[:start]...)
		out = append(out, ':')
		out = append(out, encoded...)
		return append(out, raw[end:]...), nil
	}
	return nil, fmt.Errorf("no %q key", key)
}

// aliasedAccess writes each allowlist entry the folder names by alias (an
// address or a data source) as that alias.
func aliasedAccess(in api.DataAppAccess, codec aliasCodec) api.DataAppAccess {
	out := in.Normalized()
	for _, dep := range accessDependencies(&out) {
		for i, id := range *dep.IDs {
			if alias, ok := codec.toAlias[aliasKey{kind: dep.Kind, name: id}]; ok {
				(*dep.IDs)[i] = alias
			}
		}
	}
	return out
}
