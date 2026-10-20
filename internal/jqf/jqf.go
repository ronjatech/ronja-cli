// Package jqf is the CLI's jq filter: compile an expression once, apply it to a
// decoded JSON value, render the results. It is real jq (gojq), not a path
// extractor, because a lookalike that disagrees with jq on `//`, `?` or `[]` in
// a poll condition is worse than no --jq flag. See cli/README.md.
package jqf

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"runtime/metrics"

	"github.com/itchyny/gojq"
)

// Filter is one compiled expression, reusable across inputs (a poll loop applies
// it to every response).
type Filter struct {
	code *gojq.Code
	expr string
}

// Compile parses and compiles a jq expression. Both failures quote the
// expression, since a bare gojq parse error names a position in a string the
// caller cannot see.
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

// ⚠️ A filter produces every value it matched, or it fails: there is no output
// cap, because a truncated jq stream exits zero with a wrong prefix and has
// nowhere to say so. An enormous answer trips the memory budget (the collected
// slice's allocations count), an endless one the step budget, a slow one the
// caller's deadline; the commands collect before rendering, so a filter that
// fails partway prints nothing.

// Both budgets scale with the size of the input document, because cost tracks
// elements, not a flat count:
//
//	steps = max(MinSteps, inputBytes*StepsPerInputByte)
//	alloc = max(MinAllocBudget, inputBytes*AllocPerInputByte)
//
// ⚠️ The step budget is only safe this high because the memory budget exists: a
// step is not a byte (one `join` instruction can allocate gigabytes), so steps
// only stop a program that spins without allocating (`def f: f; f`).
const (
	// MinSteps is the step floor for a small document; it stops `def f: f; f`
	// in about 15 ms.
	MinSteps = 4_000_000

	// StepsPerInputByte is ~4.6x the densest honest input measured:
	// `[.[] | select(...)]` over a 5 MiB array of bare numbers costs 14 steps
	// per input byte.
	StepsPerInputByte = 64

	// MinAllocBudget is the floor that stops a hostile expression, which is
	// handed a small document and manufactures a large one. It is a quarter
	// gigabyte rather than tighter because the counter is process-wide (see
	// heapAllocated), so the floor must leave room for other allocation.
	MinAllocBudget = 256 << 20

	// AllocPerInputByte is ~6x the heaviest honest filter measured (1.4 to 20
	// cumulative bytes per input byte over a 5 MiB body). It does refuse some
	// honest filters over huge thin bodies (`[.[] | tostring]` over 2.6 million
	// numbers is 153), where the remedy is a narrower request.
	AllocPerInputByte = 128
)

type limits struct {
	steps int
	alloc uint64
}

func limitsFor(inputBytes int) limits {
	if inputBytes < 0 {
		inputBytes = 0
	}
	lim := limits{steps: MinSteps, alloc: MinAllocBudget}
	if s := inputBytes * StepsPerInputByte; s > lim.steps {
		lim.steps = s
	}
	if a := uint64(inputBytes) * AllocPerInputByte; a > lim.alloc {
		lim.alloc = a
	}
	return lim
}

// largeInputBytes is where a budget error's remedy flips: past it the response
// is the likely cause (narrow the request), below it the expression is. Naming
// the wrong one sends the caller rewriting a filter that was already right.
const largeInputBytes = 256 << 10

// ErrWorkBudget reports that a filter exceeded its step budget: work that does
// not terminate in any useful time.
var ErrWorkBudget = errors.New("jq work budget exceeded")

// ErrMemoryBudget reports that a filter exceeded its allocation budget, usually
// an honest filter over a response that should have been paged.
var ErrMemoryBudget = errors.New("jq memory budget exceeded")

// ⚠️ heapAllocated is process-wide and cumulative (Go has no per-goroutine
// counter), so concurrent work and garbage inflate it: a filter is only ever
// refused earlier than its own allocation justifies, never later. It uses
// runtime/metrics because ReadMemStats stops the world (~30 µs against ~173 ns).
func heapAllocated(sample []metrics.Sample) uint64 {
	metrics.Read(sample)
	return sample[0].Value.Uint64()
}

// budgetContext meters gojq through the only hook it gives: the VM polls
// ctx.Done() once per instruction, so counting polls counts instructions, and a
// closed channel stops the program down the cancellation path. A custom
// function cannot shadow a builtin (gojq resolves customFuncs last), so a
// per-value hook like WithIterFunction("_range") silently never fires.
//
// ⚠️ The per-step poll is a gojq implementation detail, not a contract; the
// runaway-program tests are what fail if a future gojq caches the channel.
type budgetContext struct {
	context.Context
	lim    limits
	steps  int
	closed chan struct{}

	// sample is allocated once so the poll path does not allocate.
	sample     []metrics.Sample
	base       uint64
	used       uint64
	over       bool
	nextSample int
	lastStep   int
	lastUsed   uint64
}

// The allocation-sampling interval adapts: after each sample, sample again at
// half the steps that would spend the rest of the budget at the observed rate.
// A fixed fine interval would dominate an honest run (a read costs ~173 ns); a
// fixed coarse one would let a fast allocator overshoot by gigabytes.
const (
	sampleEveryMin     = 16
	sampleEveryMax     = 8192
	sampleEveryInitial = 64
)

func newBudgetContext(parent context.Context, lim limits) *budgetContext {
	closed := make(chan struct{})
	close(closed)
	b := &budgetContext{
		Context:    parent,
		lim:        lim,
		closed:     closed,
		sample:     []metrics.Sample{{Name: "/gc/heap/allocs:bytes"}},
		nextSample: sampleEveryInitial,
	}
	b.base = heapAllocated(b.sample)
	return b
}

func (b *budgetContext) overSteps() bool { return b.steps > b.lim.steps }

func (b *budgetContext) exceeded() bool { return b.over || b.overSteps() }

func (b *budgetContext) checkAlloc() {
	if b.over {
		return
	}
	now := heapAllocated(b.sample)
	if now < b.base { // counters do not go backwards, but do not trust arithmetic on it
		return
	}
	b.used = now - b.base
	if b.used > b.lim.alloc {
		b.over = true
		return
	}
	steps, used := b.steps-b.lastStep, b.used-b.lastUsed
	b.lastStep, b.lastUsed = b.steps, b.used
	next := sampleEveryMax
	if steps > 0 && used > 0 {
		perStep := float64(used) / float64(steps)
		if n := int(float64(b.lim.alloc-b.used) / perStep / 2); n < next {
			next = n
		}
	}
	if next < sampleEveryMin {
		next = sampleEveryMin
	}
	b.nextSample = b.steps + next
}

func (b *budgetContext) Done() <-chan struct{} {
	b.steps++
	if b.overSteps() {
		return b.closed
	}
	if b.steps >= b.nextSample {
		b.checkAlloc()
	}
	if b.over {
		return b.closed
	}
	return b.Context.Done()
}

func (b *budgetContext) Err() error {
	if b.over {
		return ErrMemoryBudget
	}
	if b.overSteps() {
		return ErrWorkBudget
	}
	return b.Context.Err()
}

// Run applies the filter and collects every output; no outputs (`empty`, a
// select matching nothing) is an answer, not an error. ctx is what stops a
// merely slow program, and no bound truncates: each returns an error naming its
// remedy.
func (f *Filter) Run(ctx context.Context, input any) ([]any, error) {
	return f.run(ctx, input, approxSize(input))
}

// approxSize estimates a decoded value's JSON-encoded size, the measure the
// budgets are calibrated against, so Run gets the same input-scaled budget as
// RunBytes rather than the small-document floor.
func approxSize(v any) int {
	switch t := v.(type) {
	case nil:
		return 4
	case bool:
		return 5
	case string:
		return len(t) + 2
	case []any:
		n := 2
		for _, e := range t {
			n += approxSize(e) + 1
		}
		return n
	case map[string]any:
		n := 2
		for k, e := range t {
			n += len(k) + 4 + approxSize(e)
		}
		return n
	default:
		// Numbers, and anything a custom decoder produced.
		return 8
	}
}

func (f *Filter) budgetError(b *budgetContext, inputBytes int) error {
	sentinel, spent := ErrWorkBudget, fmt.Sprintf("more than %d VM instructions", b.lim.steps)
	if b.over {
		sentinel, spent = ErrMemoryBudget, fmt.Sprintf("more than %d MiB of memory", b.lim.alloc>>20)
	}
	if inputBytes >= largeInputBytes {
		return fmt.Errorf(
			"jq %q used %s over a %d-byte response and was stopped — the RESPONSE is too big for this filter, the expression is not wrong. Narrow the REQUEST (most listings take ?limit=, and a smaller page filtered beats a large one refused); rewriting the filter will be refused again: %w",
			f.expr, spent, inputBytes, sentinel)
	}
	return fmt.Errorf(
		"jq %q used %s and was stopped — over an input of only %d bytes, the cost is coming from the EXPRESSION rather than the data. An expression that GENERATES values (range, recurse, repeat, a string repeated with *, or a def with no base case) allocates without bound; select from the response you were given rather than producing a new one: %w",
		f.expr, spent, inputBytes, sentinel)
}

// ⚠️ A stream can fail late (a type error at item 15,000, `halt_error`), so run
// never returns early on what it has collected; the budgets and ctx bound the
// tail.
//
// ⚠️ Residual, not fixed: one builtin call is one uninterruptible instruction,
// so `("a"*100000) as $s | [range(100000) | $s] | join("")` allocates ~49 GB
// inside one `join` before the output-boundary read refuses it. Tolerable only
// because the operator writes the expression and burns their own process; it is
// why the agent's callAPI takes no response filter.
func (f *Filter) run(ctx context.Context, input any, inputBytes int) ([]any, error) {
	budget := newBudgetContext(ctx, limitsFor(inputBytes))
	iter := f.code.RunWithContext(budget, input)
	var out []any
	for {
		v, ok := iter.Next()
		if !ok {
			return out, nil
		}
		if err, isErr := v.(error); isErr {
			// A halt with exit code 0 is a deliberate stop: what it produced is
			// the answer.
			var halt *gojq.HaltError
			if errors.As(err, &halt) && halt.ExitCode() == 0 {
				return out, nil
			}
			// Budget and deadline trips arrive as ordinary iteration errors and
			// must not read as a bad expression.
			if budget.exceeded() {
				return nil, f.budgetError(budget, inputBytes)
			}
			if ctxErr := ctx.Err(); ctxErr != nil {
				return nil, fmt.Errorf("jq %q did not finish: %w", f.expr, ctxErr)
			}
			return nil, fmt.Errorf("jq %q: %w", f.expr, err)
		}
		// Re-read at every output: one enormous value (`"a" * 200000000` is
		// eight instructions, 190 MiB) fits between two step-samples.
		budget.checkAlloc()
		if budget.over {
			return nil, f.budgetError(budget, inputBytes)
		}
		out = append(out, v)
	}
}

// RunBytes decodes a JSON document and applies the filter to it. A body that
// does not parse is reported as not JSON (usually a proxy's HTML error page),
// not as a jq failure, since no expression rescues it.
func (f *Filter) RunBytes(ctx context.Context, body []byte) ([]any, error) {
	input, err := Decode(body)
	if err != nil {
		return nil, err
	}
	return f.run(ctx, input, len(body))
}

// Decode parses a JSON document into the shape gojq consumes. Numbers arrive as
// float64, gojq's input contract; Ronja's identifiers are strings and its
// numbers sit well inside float64's exact-integer range.
func Decode(body []byte) (any, error) {
	var input any
	if err := json.Unmarshal(body, &input); err != nil {
		return nil, fmt.Errorf("the response is not JSON, so there is nothing to filter: %w", err)
	}
	return input, nil
}

// Render writes each result on its own line as compact JSON. raw unquotes
// string results only, so `$(ronja api ... --jq '.id' -r)` substitutes wf-123
// rather than a quoted path; other values still render as JSON, as in jq.
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

// Truthy applies jq's rule: only false and null are false, so 0, "" and [] are
// true (`--wait-until '.errors'` on `"errors": []` holds at once).
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

// Holds reports whether filter outputs satisfy a condition: at least one
// output, and every output truthy. No outputs is not satisfied, or a poll would
// end the instant an endpoint answered with an unexpected shape.
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
