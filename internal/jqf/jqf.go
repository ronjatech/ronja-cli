// Package jqf is the CLI's jq filter: compile an expression once, apply it to a
// decoded JSON value, render the results.
//
// It exists because the CLI's two output-producing commands both hit the same
// wall. `ronja api` streams a response to stdout and stops there, so pulling one
// field out of it — the id of a thing just created, the status of a run — meant
// piping into a `python -c "import sys,json; ..."` incantation. That is the
// glue this CLI exists to remove: a caller who has to reach for another
// interpreter to read the answer has not been given a usable answer.
//
// gojq is a real dependency in a module that has kept to two, and the
// alternative was seriously considered: a hand-rolled `.a.b.c` path extractor
// is about fifty lines and needs nothing. It was rejected because the flag is
// called --jq. A caller who writes `.items[] | select(.status=="ready") | .id`
// — the expression jq documentation teaches, and the one `gh api --jq` accepts
// — must get jq's answer or a clear parse error, never a subtly different one
// from a lookalike that implements the easy half of the syntax. Being wrong
// about `//`, `?` or `[]` in a poll condition is worse than not having the flag.
//
// The cost is contained: cli/ is its own Go module, so nothing here reaches the
// backend build.
package jqf

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"

	"github.com/itchyny/gojq"
)

// Filter is one compiled expression, reusable across inputs. Compiling is the
// expensive half and a poll loop applies the same expression to every response,
// so it happens once.
type Filter struct {
	code *gojq.Code
	expr string
}

// Compile parses and compiles a jq expression.
//
// Both failures are reported with the expression quoted. A jq parse error on
// its own ("unexpected token \"]\"") names a position in a string the caller
// cannot see from the message, which for a one-line shell flag is most of what
// they need to fix it.
func Compile(expr string) (*Filter, error) {
	query, err := gojq.Parse(expr)
	if err != nil {
		return nil, fmt.Errorf("bad jq expression %q: %w", expr, err)
	}
	code, err := gojq.Compile(query)
	if err != nil {
		return nil, fmt.Errorf("bad jq expression %q: %w", expr, err)
	}
	return &Filter{code: code, expr: expr}, nil
}

// Expr is the expression as written, for error messages upstream.
func (f *Filter) Expr() string { return f.expr }

// Run applies the filter and collects every output.
//
// A jq program is a stream: `.items[]` yields one output per element, `empty`
// yields none, and both are ordinary results rather than errors. The outputs
// are collected rather than streamed because every caller here either renders
// all of them or tests all of them, and a partially-consumed iterator that then
// errors is a shape neither one wants to reason about.
func (f *Filter) Run(input any) ([]any, error) {
	iter := f.code.Run(input)
	var out []any
	for {
		v, ok := iter.Next()
		if !ok {
			return out, nil
		}
		if err, isErr := v.(error); isErr {
			// A halt with status 0 is `halt`/`empty`-like: the program chose to
			// stop, and what it produced before that is the answer. Any other
			// halt, and every ordinary runtime error ("cannot index array with
			// string"), is a failure.
			var halt *gojq.HaltError
			if errors.As(err, &halt) && halt.ExitCode() == 0 {
				return out, nil
			}
			return nil, fmt.Errorf("jq %q: %w", f.expr, err)
		}
		out = append(out, v)
	}
}

// RunBytes decodes a JSON document and applies the filter to it.
//
// The decode failure is reported as a decode failure rather than as a jq one,
// because the two have completely different fixes: a filter that will not run
// is the caller's expression, while a body that will not parse is an endpoint
// that did not answer with JSON (an HTML error page from a proxy is the usual
// one) and no expression will rescue it.
func (f *Filter) RunBytes(body []byte) ([]any, error) {
	input, err := Decode(body)
	if err != nil {
		return nil, err
	}
	return f.Run(input)
}

// Decode parses a JSON document into the shape gojq consumes.
//
// Plain encoding/json, so numbers arrive as float64. That is gojq's own
// documented input contract, and the precision it costs does not bite here:
// Ronja's identifiers are strings ("wf-...", "table-..."), and the numbers that
// reach a filter are counts, sizes and timestamps, all far inside float64's
// exact-integer range.
func Decode(body []byte) (any, error) {
	var input any
	if err := json.Unmarshal(body, &input); err != nil {
		return nil, fmt.Errorf("the response is not JSON, so there is nothing to filter: %w", err)
	}
	return input, nil
}

// Render writes each result on its own line.
//
// raw unquotes STRING results, which is the whole point of the mode: the common
// use is `$(ronja api ... --jq '.id' -r)`, and a quoted "wf-123" substituted
// into the next command is a path that does not exist. Only strings are
// affected — a raw number or object still renders as JSON, because there is no
// other form for them and jq does the same.
func Render(w io.Writer, results []any, raw bool) error {
	for _, r := range results {
		if raw {
			if s, isString := r.(string); isString {
				if _, err := fmt.Fprintln(w, s); err != nil {
					return err
				}
				continue
			}
		}
		// Compact, not indented: filtered output is nearly always consumed by
		// something, and one result per line is what makes it consumable.
		encoded, err := json.Marshal(r)
		if err != nil {
			return fmt.Errorf("render jq result: %w", err)
		}
		if _, err := fmt.Fprintln(w, string(encoded)); err != nil {
			return err
		}
	}
	return nil
}

// Truthy applies jq's own rule: false and null are false, EVERYTHING else is
// true — including 0, "" and [].
//
// Worth stating because it is the rule people expect to be wrong about. An
// empty array is truthy in jq, so `--wait-until '.errors'` on a response
// carrying `"errors": []` is satisfied immediately. Conditions want an explicit
// comparison (`.errors | length > 0`), which is what the flag's help says.
func Truthy(v any) bool {
	switch t := v.(type) {
	case nil:
		return false
	case bool:
		return t
	default:
		return true
	}
}

// Holds reports whether a set of filter outputs satisfies a condition.
//
// Two rules, and the second is the one that matters: the condition needs at
// least one output, and every output must be truthy. "All of none" is
// vacuously true in logic and catastrophically wrong here — `.status ==
// "ready"` against a response with no `status` at all yields no outputs, and
// treating that as satisfied would end a poll loop the instant the endpoint
// answered with a shape nobody expected. A filter that matches nothing has not
// established anything.
func Holds(results []any) bool {
	if len(results) == 0 {
		return false
	}
	for _, r := range results {
		if !Truthy(r) {
			return false
		}
	}
	return true
}
