package commands

import (
	"fmt"
	"sort"
	"strconv"
	"strings"

	"github.com/ronjatech/ronja-cli/internal/api"
)

// Parameter handling for `ronja wf test`: --param key=value flags checked
// against what the workflow declares.

// parseParams mirrors rworkflow.CoerceWorkflowParameterValues so a typo costs no
// round trip and a "number" does not travel as text. The server's pass is the
// authority and strictly wider: keep this the SUBSET, never accepting something
// the server refuses.
func parseParams(specs []string, declared []api.WorkflowParameter) (map[string]any, error) {
	byName := make(map[string]api.WorkflowParameter, len(declared))
	for _, p := range declared {
		byName[p.Name] = p
	}

	values := map[string]any{}
	for _, spec := range specs {
		// First = only: a value may itself contain one.
		name, raw, ok := strings.Cut(spec, "=")
		if !ok || name == "" {
			return nil, fmt.Errorf("--param takes key=value, got %q", spec)
		}
		if _, dup := values[name]; dup {
			return nil, fmt.Errorf("--param %s was given more than once", name)
		}
		p, known := byName[name]
		if !known {
			return nil, fmt.Errorf("workflow parameter %q is not declared by this workflow%s", name, declaredNames(declared))
		}
		value, err := coerceParam(p, raw)
		if err != nil {
			return nil, err
		}
		values[name] = value
	}

	// A defaulted parameter is not missing: the server applies it at dispatch
	// (rworkflow.ApplyParameterDefaults).
	var missing, blank []string
	for _, p := range declared {
		if !p.Required || p.DefaultValue != nil {
			continue
		}
		// A blank required value is refused as the server does, except a select with
		// static options, where coerceParam has already required "" to be a declared
		// option. Options on other types exempt nothing, since only a select checks them.
		v, ok := values[p.Name]
		if !ok {
			missing = append(missing, p.Name)
		} else if s, isStr := v.(string); isStr && !(p.Type == "select" && len(p.Options) > 0) && strings.TrimSpace(s) == "" {
			blank = append(blank, p.Name)
		}
	}
	if len(missing) > 0 {
		sort.Strings(missing)
		return nil, fmt.Errorf("this workflow requires %s %s, which has no value and no default — pass --param %s=<value>",
			plural(len(missing), "parameter"), strings.Join(quoteAll(missing), ", "), missing[0])
	}
	if len(blank) > 0 {
		sort.Strings(blank)
		return nil, fmt.Errorf("this workflow requires %s %s, which was passed empty and has no default — pass --param %s=<value>",
			plural(len(blank), "parameter"), strings.Join(quoteAll(blank), ", "), blank[0])
	}
	if len(values) == 0 {
		return nil, nil
	}
	return values, nil
}

// coerceParam passes an unknown or empty type through as a string: the server
// treats it as unconstrained, and refusing a newer kind would block work.
func coerceParam(p api.WorkflowParameter, raw string) (any, error) {
	switch p.Type {
	case "number":
		// Integers first: a float64 would quietly round an id or a nanosecond epoch.
		if i, err := strconv.ParseInt(raw, 10, 64); err == nil {
			return i, nil
		}
		n, err := strconv.ParseFloat(raw, 64)
		if err != nil {
			return nil, fmt.Errorf("workflow parameter %q expects a number, got %q", p.Name, raw)
		}
		return n, nil
	case "select":
		// Only static options: the server does not check an OptionsQuery select either.
		if len(p.Options) > 0 && !contains(p.Options, raw) {
			return nil, fmt.Errorf("workflow parameter %q value %q is not one of its options: %s",
				p.Name, raw, strings.Join(p.Options, ", "))
		}
		return raw, nil
	default:
		// string, date, and anything added later; the server takes dates as strings.
		return raw, nil
	}
}

func contains(haystack []string, needle string) bool {
	for _, s := range haystack {
		if s == needle {
			return true
		}
	}
	return false
}

func quoteAll(names []string) []string {
	out := make([]string, len(names))
	for i, name := range names {
		out[i] = strconv.Quote(name)
	}
	return out
}

func declaredNames(declared []api.WorkflowParameter) string {
	if len(declared) == 0 {
		return " (it declares no parameters at all)"
	}
	names := make([]string, 0, len(declared))
	for _, p := range declared {
		names = append(names, p.Name)
	}
	sort.Strings(names)
	return " — it declares: " + strings.Join(names, ", ")
}
