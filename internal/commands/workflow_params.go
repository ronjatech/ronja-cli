package commands

import (
	"fmt"
	"sort"
	"strconv"
	"strings"

	"github.com/ronjatech/ronja-cli/internal/api"
)

// Parameter handling for `ronja wf test`: turning repeated --param key=value
// flags into the values a run POST carries, and refusing the ones the workflow
// never declared.
//
// Its own file because it is the one part of the command that is pure — no
// client, no folder, no terminal — and because the v2 run endpoint does not
// validate parameters at all, which makes these checks worth reading on their
// own rather than in the middle of a poll loop.

// parseParams turns repeated --param key=value flags into the run's parameter
// values, checked against what the workflow declares.
//
// This mirrors rscheduledjob.ValidateWorkflowParameterValues, which the v2 run
// endpoint does NOT apply — so the checks are the CLI's or they are nobody's.
// Coercion is the CLI-only part: a shell hands over strings, and a "number"
// parameter arriving as "12" would reach the script as text.
func parseParams(specs []string, declared []api.WorkflowParameter) (map[string]any, error) {
	byName := make(map[string]api.WorkflowParameter, len(declared))
	for _, p := range declared {
		byName[p.Name] = p
	}

	values := map[string]any{}
	for _, spec := range specs {
		// FIRST = only: a value may perfectly well contain one (a SQL fragment,
		// a base64 blob, a date range), and splitting on all of them would
		// corrupt it silently.
		name, raw, ok := strings.Cut(spec, "=")
		if !ok || name == "" {
			return nil, fmt.Errorf("--param takes key=value, got %q", spec)
		}
		if _, dup := values[name]; dup {
			// Last-wins would be a coin flip nobody can see in the output.
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

	// A defaulted parameter is not missing: the server falls back to its
	// default, which is the whole point of having one.
	var missing []string
	for _, p := range declared {
		if !p.Required || p.DefaultValue != nil {
			continue
		}
		if _, ok := values[p.Name]; !ok {
			missing = append(missing, p.Name)
		}
	}
	if len(missing) > 0 {
		sort.Strings(missing)
		return nil, fmt.Errorf("this workflow requires %s %s, which has no value and no default — pass --param %s=<value>",
			plural(len(missing), "parameter"), strings.Join(quoteAll(missing), ", "), missing[0])
	}
	if len(values) == 0 {
		return nil, nil
	}
	return values, nil
}

// coerceParam converts one command-line string to the type the parameter
// declares, or explains why it cannot.
//
// An unknown or empty type is passed through as a string rather than refused:
// the server treats it as unconstrained, and a CLI that rejected a parameter
// kind added after it shipped would be wrong in the direction that blocks work.
func coerceParam(p api.WorkflowParameter, raw string) (any, error) {
	switch p.Type {
	case "number":
		// Integers first, and not only for tidiness: a float64 holds 53 bits of
		// mantissa, so an id or an epoch-nanosecond timestamp passed as a
		// "number" would arrive at the script quietly rounded. Anything that is
		// not an integer falls through to the float, which is what "number"
		// mostly means.
		if i, err := strconv.ParseInt(raw, 10, 64); err == nil {
			return i, nil
		}
		n, err := strconv.ParseFloat(raw, 64)
		if err != nil {
			return nil, fmt.Errorf("workflow parameter %q expects a number, got %q", p.Name, raw)
		}
		return n, nil
	case "select":
		// Only when the options are declared statically. A select whose options
		// come from an OptionsQuery has no list to check against here — the
		// server does not check it either.
		if len(p.Options) > 0 && !contains(p.Options, raw) {
			return nil, fmt.Errorf("workflow parameter %q value %q is not one of its options: %s",
				p.Name, raw, strings.Join(p.Options, ", "))
		}
		return raw, nil
	default:
		// string, date, and anything added later. Dates cross the wire as
		// strings server-side, so there is nothing to parse — and parsing them
		// here would only invent a format the server never asked for.
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

// declaredNames renders the "here is what you could have said" tail of the
// unknown-parameter error.
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
