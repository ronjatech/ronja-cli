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
// called --jq. A caller who writes `.result[] | select(.status=="ready") | .id`
// — the expression jq documentation teaches, and the one `gh api --jq` accepts
// — must get jq's answer or a clear parse error, never a subtly different one
// from a lookalike that implements the easy half of the syntax. Being wrong
// about `//`, `?` or `[]` in a poll condition is worse than not having the flag.
//
// The cost is contained: cli/ is its own Go module, so nothing here reaches the
// backend build.
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

// ⚠️ THERE IS NO CAP ON HOW MANY VALUES A FILTER MAY PRODUCE, and its absence is
// a decision rather than an omission. The rule callers get instead is:
//
//	A FILTER PRODUCES EVERY VALUE IT MATCHED, OR IT FAILS.
//
// There is never a prefix, so there is never anything to signal.
//
// An earlier form of this package capped the stream at 10,000 values, and the
// CLI printed the first 10,000 with a note on stderr and exited zero. That is
// silent data loss: `ids=$(ronja api ... --jq '.result[].id' -r 2>/dev/null)`
// discards the only warning there was and captures a wrong PREFIX with a
// successful exit. Every other truncation this CLI reports — `ronja query`'s
// `truncated:true` — is IN the data, where a caller can act on it, and a jq
// stream has nowhere to put such a field. A flag was tried (--jq-strict) and
// only moved the problem: the DEFAULT still lost data, and turning it on made
// `--jq` over a large listing print nothing at all for an answer that had been
// printable in full one release earlier.
//
// ⚠️ THE CAP NEVER BOUNDED COST, which is why removing it costs nothing. Past
// it the loop kept pulling and discarding — it had to, because a jq program can
// fail LATE and returning at the cap converted a failing filter into a
// successful-looking prefix. The only thing it bounded was the length of the
// collected slice, and THAT is bounded by the memory budget below: the slice's
// own allocations are counted, because heapAllocated is process-wide and
// cumulative. So a stream whose answer is genuinely enormous trips
// ErrMemoryBudget, one that is merely endless trips the step budget, and one
// that is slow trips the caller's deadline. All three fail loudly and name a
// remedy, which is exactly what a truncation cannot do.
//
// The other half of the rule lives in the commands: results are collected
// before anything is rendered, so a filter that fails partway prints NOTHING
// rather than a prefix plus a non-zero exit.

// The two budgets below are derived from the SIZE OF THE DOCUMENT the filter is
// handed, and that derivation is the whole point.
//
// An earlier version bounded ONE thing — gojq VM instructions — with ONE flat
// number, and was wrong in BOTH directions at once, which is the tell that the
// quantity being counted was not the quantity that hurts.
//
//   - TOO LOOSE, because a step is not a byte. Measured in-tree:
//     `[range(2000) | "x"*1000000]` is ~18,000 instructions — half a percent of
//     a four-million-step budget — and 1.9 GB of heap; and
//     `("a"*100000) as $s | [range(100000) | $s] | join("")` ran for 19.8 s
//     under a TEN-SECOND deadline, because `join` over a 100,000-element array
//     of 100 KB strings is ONE instruction, allocating ~49 GB cumulative and
//     ~10 GB live. In a shared multi-tenant API pod that is `fatal error: out
//     of memory`, which gin.Recovery cannot catch, and a per-turn CALL cap
//     multiplies such an expression rather than bounding it, because a model's
//     tool batch is dispatched concurrently. (That reading is what settled the
//     question for the agent: callAPI no longer takes a jq filter at all — see
//     the residual note on run() below. The numbers still bound THIS process.)
//   - TOO TIGHT, because cost tracks ELEMENTS, and elements-per-5-MiB scales
//     inversely with element size. The ordinary projection an operator writes to
//     trim a listing — `[.[] | select(.id != null) | {id, name}]` — costs
//     790,000 steps over 21,000 fat rows and 7,200,000 over 194,000 thin ones.
//     Four million REFUSED the second one in 44 ms and told the model its
//     expression was generating data.
//
// So there are two budgets, both scaled to the input, and the MEMORY one is the
// bound that actually holds:
//
//	steps = max(MinSteps, inputBytes*StepsPerInputByte)
//	alloc = max(MinAllocBudget, inputBytes*AllocPerInputByte)
//
// ⚠️ RAISING THE STEP BUDGET IS ONLY SAFE BECAUSE THE MEMORY BUDGET EXISTS.
// They are not two dials on one knob: the step budget's remaining job is the
// program that spins without allocating (`def f: f; f`), and the memory budget
// is what answers every shape a step count cannot see. Do not tune one without
// re-reading why the other is there.
const (
	// MinSteps is the floor for a small document, and it is what stops
	// `def f: f; f` — an expression whose cost has nothing to do with its
	// input — in about 15 ms.
	MinSteps = 4_000_000

	// StepsPerInputByte is calibrated on the DENSEST honest input measured: a
	// 5 MiB array whose elements are bare numbers, where `[.[] | select(...)]`
	// costs 14 steps per input byte (73.4M steps, ~2.6M elements at ~28 steps
	// each). Sixty-four is ~4.6x that, so no honest filter comes near it.
	StepsPerInputByte = 64

	// MinAllocBudget is the floor, and it is the number that answers a hostile
	// expression, because those ignore their input: they are handed a small
	// document and manufacture a large one. `[range(2000) | "x"*1000000]` trips
	// this after ~256 MiB instead of 1.9 GB, and the `join`-over-a-manufactured-
	// array shape after ~256 MiB instead of ~49 GB.
	//
	// A quarter of a gigabyte rather than something tighter because the counter
	// is process-wide (see heapAllocated): the floor has to leave room for
	// whatever else the process allocated during the filter's window, or a busy
	// pod refuses honest filters.
	MinAllocBudget = 256 << 20

	// AllocPerInputByte is calibrated the same way: the honest filters measured
	// over a 5 MiB body allocate 1.4 (fat rows) to 20 (thin rows) bytes per
	// input byte, cumulative, garbage included. A hundred and twenty-eight is
	// ~6x the realistic worst, which admits every ordinary listing and refuses
	// the shapes that manufacture data.
	//
	// ⚠️ It does NOT admit every conceivable filter over every conceivable
	// body: `[.[] | tostring]` over 2.6 MILLION bare numbers allocates 153
	// bytes per input byte and is refused. That is the right answer — a
	// response with 2.6 million rows in it needs a narrower REQUEST, which is
	// what the refusal says — but it is a real behaviour change and belongs in
	// the comment rather than in a support ticket.
	AllocPerInputByte = 128
)

// limits is one filter run's ceiling, derived from its input.
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

// largeInputBytes is where the refusal message changes its LEAD, and that is
// the whole reason the number exists.
//
// The two ways to trip a budget have opposite fixes and the old message named
// only one of them: it said the cause was "an expression that GENERATES values
// (range, recurse, repeat, or a def with no base case)". For a correct filter
// over an enormous listing that is simply false, so the model rewrote an
// expression that was already right — and each rewrite is a NEW call, so the
// repeat-guard never fires and the loop is unbounded. Past this size the
// response is the likely cause and the remedy is to narrow the REQUEST; below
// it, the input is too small to explain the cost and the expression is.
const largeInputBytes = 256 << 10

// ErrWorkBudget reports that a filter exceeded its step budget.
//
// A distinct sentinel because it has a distinct fix. A deadline says "slow"; a
// decode failure says "not JSON"; this one says the program is doing work that
// does not terminate in any useful time, and no amount of waiting changes that.
var ErrWorkBudget = errors.New("jq work budget exceeded")

// ErrMemoryBudget reports that a filter exceeded its allocation budget.
//
// Separate from ErrWorkBudget because the two are separate failures with
// separate remedies, and because callers meter them separately: a step trip is
// nearly always a runaway program, whereas a memory trip is usually an honest
// filter over a response that should have been paged.
var ErrMemoryBudget = errors.New("jq memory budget exceeded")

// heapAllocated reads the process's cumulative heap allocation.
//
// ⚠️ IT IS PROCESS-WIDE, NOT PER-GOROUTINE, and Go exposes nothing finer —
// there is no per-goroutine allocation counter at any supported API. Two
// consequences, both in the safe direction for a BOUND: concurrent work in the
// same process inflates the reading, so a filter can only ever be refused
// EARLIER than its own allocation alone would justify, never later; and the
// counter is CUMULATIVE, so garbage counts too. Both are why the budget is
// calibrated with ~6x headroom over the heaviest honest filter rather than
// snugly.
//
// runtime/metrics rather than runtime.ReadMemStats, and the difference decides
// whether this is affordable at all: measured on this metric, metrics.Read
// costs ~173 ns and ReadMemStats ~30 µs — 175x — because ReadMemStats stops the
// world. A per-step ReadMemStats would cost more than the filter.
func heapAllocated(sample []metrics.Sample) uint64 {
	metrics.Read(sample)
	return sample[0].Value.Uint64()
}

// budgetContext meters gojq's work, and it is the only hook gojq gives us.
//
// ⚠️ THE OBVIOUS IMPLEMENTATION DOES NOT WORK, and it was tried first. Counting
// values as the generators emit them — gojq.WithIterFunction("_range", ...) —
// compiles without complaint and is then never called, because
// compiler.compileFunc resolves builtin definitions and internalFuncs BEFORE
// customFuncs. A custom function cannot shadow a builtin in gojq, so there is no
// per-value hook to hang accounting on. The failure is silent: the filter simply
// runs unbounded and the counter reads zero.
//
// What gojq does expose is its execution loop, which polls ctx.Done() once per
// VM instruction. A context that counts those polls is therefore an exact
// instruction counter, and handing back an already-closed channel past the
// budget stops the program down the path gojq already handles — the same one a
// real cancellation takes, so nothing downstream needs to learn a new shape.
// The same hook is where the memory sampling happens, for exactly the same
// reason: it is the only place inside a running program we are given.
//
// ⚠️ That per-step poll is an implementation detail of gojq, not a documented
// contract. It is pinned by tests that run genuinely runaway programs and
// require them to be REFUSED: if a future gojq caches the channel, both budgets
// stop applying silently and those tests are the thing that says so.
type budgetContext struct {
	context.Context
	lim    limits
	steps  int
	closed chan struct{}

	// Memory sampling state. sample is allocated once so the poll path does
	// not allocate; base is the reading at the start of this run.
	sample     []metrics.Sample
	base       uint64
	used       uint64
	over       bool
	nextSample int
	lastStep   int
	lastUsed   uint64
}

// sampleEvery bounds how often the poll path reads the allocation counter, and
// the interval is ADAPTIVE because a fixed one cannot be both cheap and timely.
//
// A sample costs ~173 ns against a step of a few ns, so a fixed fine interval
// would dominate an honest 300-million-step run; a fixed coarse one would let
// `[range(N) | "x"*100000000]` allocate gigabytes between two readings. So
// after each sample the interval is re-derived from the rate just observed:
// aim to sample again after half the steps it would take, at that rate, to
// spend what is LEFT of the budget. An honest filter (~16 bytes/step) lands on
// the ceiling and pays ~0.02 ns/step; one allocating 100 KB/step lands on the
// floor and overshoots by at most a factor of two.
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

// checkAlloc reads the counter and re-derives the sampling interval.
//
// Also called from run after every top-level value, which is what catches
// the shape no step-based sampling can: ONE enormous value. `"a" * 200000000`
// is eight instructions and 190 MiB, so the run can finish between two
// step-samples — but it cannot finish between two OUTPUTS, and an output is
// where the caller would otherwise take delivery of it.
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

// Run applies the filter and collects EVERY output.
//
// A jq program is a stream: `.result[]` yields one output per element and
// `empty` yields none. Both are ordinary results, not errors — a filter that
// matches nothing is an answer ("none of them"), and turning it into a failure
// would send the caller off fixing an expression that was correct.
//
// Three bounds apply, and they cover different failures. The memory budget stops
// a program that manufactures data rather than selecting it — it is the one that
// holds, and it is also what bounds a genuinely enormous ANSWER, since the
// collected slice allocates like everything else. The step budget stops a
// program that spins without allocating. And ctx stops one that is merely slow.
// None of them truncates: each returns an error naming its remedy.
//
// ⚠️ ctx IS LOAD-BEARING, not a convention — but it is not the ONLY thing
// standing between a runaway expression and the process, which is what an
// earlier version of this comment assumed.
func (f *Filter) Run(ctx context.Context, input any) ([]any, error) {
	return f.run(ctx, input, approxSize(input))
}

// approxSize estimates the JSON footprint of an already-decoded value, so that
// a caller holding one gets the same input-scaled budget as a caller holding
// bytes.
//
// Without it, Run over a decoded 5 MiB listing would be given the SMALL-document
// floor and would refuse an honest filter — the Half-B failure this change
// exists to fix, reintroduced through the one entry point that does not carry a
// byte count. Only Run pays the walk; every production caller arrives through
// RunBytes, which knows the real number.
//
// An estimate, not a measurement: the numbers are the JSON encoding's shape,
// not Go's in-memory footprint, because it is the JSON size the budgets were
// calibrated against. Recursion is bounded by encoding/json's own nesting limit
// — nothing reaches this function that json.Unmarshal did not just build.
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

// budgetError names the remedy that fits, and the fork is the point.
//
// The old message said the cause was always "an expression that GENERATES
// values (range, recurse, repeat, or a def with no base case)". For a correct
// filter over an enormous listing that is simply untrue, so the model rewrote an
// expression that was already right — and each rewrite is a NEW tool call, so
// the repeat-guard never fires and the loop has no end. Past largeInputBytes the
// response is the likely cause and the remedy is a narrower REQUEST; below it,
// the input is too small to explain the cost and the expression is.
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

// run is the whole engine: collect every output, or return the error that says
// why there is no answer.
//
// The input's size is a PARAMETER rather than something computed here because
// the two callers know it by different means, and the cheap one is the one
// production uses: RunBytes hands over len(body) for free, while Run has only a
// decoded value and has to walk it.
//
// ⚠️ A STREAM CAN FAIL LATE, which is why nothing here returns early on the
// strength of what it has already collected. Measured on 20,000 items with a
// type error at index 15,000, an earlier form that stopped at a 10,000-value cap
// went from `"cannot be added"` and exit 1 to ten thousand values, a nil error
// and exit ZERO — a real failure silently converted into a successful-looking
// prefix. `halt_error` behaved the same way. The tail is not unbounded work: the
// step budget, the memory budget and ctx all apply to it. A wrong exit code has
// no such bound.
//
// ⚠️ THE ALLOCATION IS RE-READ AT EVERY OUTPUT, not only on the step-sampling
// schedule, and that is what catches the one shape no step counter can see: a
// single enormous VALUE. `"a" * 200000000` is EIGHT instructions and 190 MiB, so
// it is produced entirely between two step-samples — but never between two
// outputs, and an output is where the caller would otherwise take delivery of it.
//
// ⚠️ THE RESIDUAL, STATED PLAINLY BECAUSE IT IS NOT FIXED: one BUILTIN call is
// one instruction, and neither budget can interrupt a Go function already
// running. gojq's `join` is `funcJoin`, a single Go function, so
// `("a"*100000) as $s | [range(100000) | $s] | join("")` builds its array for
// almost nothing (100,000 pointers to ONE string) and then allocates ~49 GB
// inside that one instruction — measured, over 12 s, and neither the sampler
// nor ctx is consulted once during it. It is CAUGHT (the output-boundary read
// above refuses it) but not PREVENTED. Nothing available in-process prevents it:
// gojq resolves customFuncs AFTER internalFuncs, so a size-checking `join`
// cannot shadow the builtin, and a watchdog cannot stop a running Go call. Every
// MULTI-instruction shape — generators, array and object accumulation, string
// repetition (which gojq itself caps at MaxInt32 bytes) — is bounded.
//
// It is tolerable HERE, and only here, because the expression is written by the
// operator running this CLI and the process it burns is their own. The same
// residual is why the agent's callAPI tool no longer takes a jq filter at all:
// on a shared multi-tenant pod, with the expression chosen by a model, an
// unbounded builtin is somebody else's outage.
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
			// A halt with status 0 is `halt`/`empty`-like: the program chose to
			// stop and what it produced is the answer. Anything else is a
			// failure.
			var halt *gojq.HaltError
			if errors.As(err, &halt) && halt.ExitCode() == 0 {
				return out, nil
			}
			// Both of the next two arrive here as an ordinary iteration error,
			// and both must be told apart from a bad expression: "bad jq
			// expression" would send the caller off rewriting something that
			// was merely expensive, or merely unlucky in its timing.
			if budget.exceeded() {
				return nil, f.budgetError(budget, inputBytes)
			}
			if ctxErr := ctx.Err(); ctxErr != nil {
				return nil, fmt.Errorf("jq %q did not finish: %w", f.expr, ctxErr)
			}
			return nil, fmt.Errorf("jq %q: %w", f.expr, err)
		}
		budget.checkAlloc()
		if budget.over {
			return nil, f.budgetError(budget, inputBytes)
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
func (f *Filter) RunBytes(ctx context.Context, body []byte) ([]any, error) {
	input, err := Decode(body)
	if err != nil {
		return nil, err
	}
	return f.run(ctx, input, len(body))
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
