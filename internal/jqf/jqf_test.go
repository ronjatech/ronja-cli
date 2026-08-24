package jqf

import (
	"context"
	"errors"
	"fmt"
	"runtime"
	"runtime/metrics"
	"strings"
	"testing"
	"time"
)

// ⚠️ NOTHING IN THIS FILE CALLS t.Parallel(), AND THAT IS DELIBERATE. The memory
// budget is metered off a PROCESS-WIDE allocation counter (see heapAllocated),
// so a test that deliberately allocates a quarter of a gigabyte inflates the
// reading of every filter running beside it. With t.Parallel() the runaway
// tests spent the honest tests' budget and refused them — which is the
// production over-estimate property, reproduced as a flake.

// What is worth testing here is the thin layer around gojq, not gojq: the
// stream-to-slice collection, the raw-string rendering, and the two truth rules
// a poll loop depends on.

func TestCompileRejectsBadExpression(t *testing.T) {
	_, err := Compile(".result[")
	if err == nil {
		t.Fatal("expected a parse error")
	}
	// The expression has to be in the message: a caller sees only their own
	// one-line flag, and a bare position offset locates nothing.
	if !strings.Contains(err.Error(), ".result[") {
		t.Fatalf("error does not quote the expression: %v", err)
	}
}

func TestRunCollectsEveryOutput(t *testing.T) {
	f, err := Compile(".result[].id")
	if err != nil {
		t.Fatalf("compile: %v", err)
	}
	got, err := f.RunBytes(context.Background(), []byte(`{"result":[{"id":"a"},{"id":"b"}]}`))
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	if len(got) != 2 || got[0] != "a" || got[1] != "b" {
		t.Fatalf("got %#v, want [a b]", got)
	}
}

// An expression that matches nothing is not an error — `empty` and a missing
// key are ordinary jq results — but it must come back as zero outputs rather
// than as a null, because Holds distinguishes the two.
func TestRunEmptyIsNotAnError(t *testing.T) {
	f, err := Compile("empty")
	if err != nil {
		t.Fatalf("compile: %v", err)
	}
	got, err := f.RunBytes(context.Background(), []byte(`{"a":1}`))
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	if len(got) != 0 {
		t.Fatalf("got %#v, want no outputs", got)
	}
}

func TestRunReportsRuntimeError(t *testing.T) {
	f, err := Compile(".[0]")
	if err != nil {
		t.Fatalf("compile: %v", err)
	}
	if _, err := f.RunBytes(context.Background(), []byte(`{"a":1}`)); err == nil {
		t.Fatal("expected a runtime error indexing an object with a number")
	}
}

// A non-JSON body must fail as a decode problem, not as a jq problem: the fix
// is entirely different, and blaming the expression sends the caller to rewrite
// something that was never wrong.
func TestRunBytesReportsDecodeFailureSeparately(t *testing.T) {
	f, err := Compile(".")
	if err != nil {
		t.Fatalf("compile: %v", err)
	}
	_, err = f.RunBytes(context.Background(), []byte("<html>gateway timeout</html>"))
	if err == nil {
		t.Fatal("expected a decode error")
	}
	if !strings.Contains(err.Error(), "not JSON") {
		t.Fatalf("error should name the decode failure, got: %v", err)
	}
}

func TestRenderRawUnquotesStringsOnly(t *testing.T) {
	var sb strings.Builder
	if err := Render(&sb, []any{"wf-123", 7.0, map[string]any{"a": 1.0}}, true); err != nil {
		t.Fatalf("render: %v", err)
	}
	want := "wf-123\n7\n{\"a\":1}\n"
	if sb.String() != want {
		t.Fatalf("got %q, want %q", sb.String(), want)
	}
}

func TestRenderWithoutRawQuotesStrings(t *testing.T) {
	var sb strings.Builder
	if err := Render(&sb, []any{"wf-123"}, false); err != nil {
		t.Fatalf("render: %v", err)
	}
	if sb.String() != "\"wf-123\"\n" {
		t.Fatalf("got %q", sb.String())
	}
}

func TestTruthyFollowsJqRules(t *testing.T) {
	// The surprising half: 0, "" and [] are all TRUE in jq. A condition built
	// on the C intuition would fire on the wrong response.
	for _, v := range []any{true, 0.0, "", []any{}, map[string]any{}} {
		if !Truthy(v) {
			t.Errorf("Truthy(%#v) = false, want true", v)
		}
	}
	for _, v := range []any{nil, false} {
		if Truthy(v) {
			t.Errorf("Truthy(%#v) = true, want false", v)
		}
	}
}

// The rule a poll loop's correctness rests on: no outputs is NOT satisfied.
// Vacuous truth here would end the wait the moment an endpoint answered with an
// unexpected shape.
func TestHoldsNeedsAtLeastOneTruthyOutput(t *testing.T) {
	cases := []struct {
		name string
		in   []any
		want bool
	}{
		{"no outputs", nil, false},
		{"one true", []any{true}, true},
		{"one false", []any{false}, false},
		{"all true", []any{true, "x"}, true},
		{"one false among true", []any{true, false}, false},
		{"null", []any{nil}, false},
	}
	for _, tc := range cases {
		if got := Holds(tc.in); got != tc.want {
			t.Errorf("%s: Holds(%#v) = %v, want %v", tc.name, tc.in, got, tc.want)
		}
	}
}

// Cancellation, which is not expressible as a golden CASE — the failure it
// pins is a HANG, and a case file can only describe outputs.
//
// `ronja api --jq 'def f: f; f'` would otherwise wedge a user's terminal with
// no way out but Ctrl-C, and --wait-until re-runs its filter on every poll, so
// the same expression would wedge it once per attempt.
func TestRunIsCancellable(t *testing.T) {
	f, err := Compile("def f: f; f")
	if err != nil {
		t.Fatalf("compile: %v", err)
	}
	// ⚠️ ALREADY CANCELLED before Run is called. The step budget now stops this
	// program in about 15 ms, so racing a 200 ms deadline against the work
	// budget would pass while proving nothing about ctx — the budget would win
	// every time. Cancelling up front leaves the caller's context as the only
	// thing that can end the run, which is the property this test is named for.
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	done := make(chan error, 1)
	go func() {
		_, runErr := f.Run(ctx, map[string]any{})
		done <- runErr
	}()

	select {
	case err := <-done:
		if err == nil {
			t.Fatal("a non-terminating filter returned no error; cancellation is not wired up")
		}
		if !strings.Contains(err.Error(), "did not finish") {
			t.Errorf("want a timeout-shaped error, got: %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("Run did not return after its context was cancelled — it is using Code.Run, not RunWithContext")
	}
}

// TestRunReturnsEveryValueRatherThanAPrefix is the rule this package sells, and
// the regression it pins was shipped.
//
// An earlier form capped the stream at 10,000 values and had the CLI print the
// first 10,000 with a note on stderr and a ZERO exit. `ids=$(ronja api ... --jq
// '.result[].id' -r 2>/dev/null)` therefore captured a silently wrong prefix,
// and the flag added to opt out of that (--jq-strict, implied by
// --fail-on-error) printed nothing at all instead. Neither is an answer, so the
// cap is gone: a filter produces every value it matched, or it fails.
//
// Forty thousand is deliberately four times the old cap — a test at the old
// boundary would pass on an off-by-one that reintroduced it.
func TestRunReturnsEveryValueRatherThanAPrefix(t *testing.T) {
	const want = 40000

	var b strings.Builder
	b.WriteString(`{"result":[`)
	for i := 0; i < want; i++ {
		if i > 0 {
			b.WriteByte(',')
		}
		fmt.Fprintf(&b, `{"id":"wf-%d"}`, i)
	}
	b.WriteString(`]}`)

	f, err := Compile(".result[].id")
	if err != nil {
		t.Fatalf("compile: %v", err)
	}
	out, err := f.RunBytes(context.Background(), []byte(b.String()))
	if err != nil {
		t.Fatalf("a large but honest listing must not be refused: %v", err)
	}
	if len(out) != want {
		t.Fatalf("got %d values, want all %d — a prefix is not an answer", len(out), want)
	}
	if out[0] != "wf-0" || out[want-1] != fmt.Sprintf("wf-%d", want-1) {
		t.Errorf("first/last values are %v/%v, want the whole stream in order", out[0], out[want-1])
	}
}

// TestRunRefusesAnEnormousAnswerRatherThanTruncatingIt is the other half of that
// rule, and it is what makes removing the cap affordable.
//
// The cap never bounded COST — past it the loop kept pulling and discarding —
// so the only thing it bounded was the length of the collected slice. That is
// bounded by the MEMORY budget instead, because the slice allocates like
// everything else and heapAllocated is process-wide and cumulative. So an answer
// that is genuinely too large to hold comes back as a refusal naming a remedy,
// which is a thing a script can act on, rather than as a prefix that is not.
func TestRunRefusesAnEnormousAnswerRatherThanTruncatingIt(t *testing.T) {
	// 100,000 values of 100 KB each: no single value is large, and the array is
	// never built — it is the COLLECTED stream that cannot fit.
	f, err := Compile(`range(100000) | "x"*100000`)
	if err != nil {
		t.Fatalf("compile: %v", err)
	}

	done := make(chan error, 1)
	go func() {
		_, runErr := f.Run(context.Background(), map[string]any{})
		done <- runErr
	}()
	select {
	case err := <-done:
		if !errors.Is(err, ErrMemoryBudget) {
			t.Fatalf("want ErrMemoryBudget, got: %v", err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("an unholdable answer was never refused — nothing bounds the collected stream")
	}
}

// allocatedDuring reports how much the process allocated while fn ran, and it
// is the assertion that matters below.
//
// ⚠️ ASSERTING ONLY "it returned an error" IS NOT ENOUGH, and that is exactly
// how the first version of these bounds shipped wrong: every runaway shape here
// DID come back with an error, and some came back with it after allocating
// gigabytes. The number is the test.
func allocatedDuring(fn func()) uint64 {
	sample := []metrics.Sample{{Name: "/gc/heap/allocs:bytes"}}
	runtime.GC()
	metrics.Read(sample)
	before := sample[0].Value.Uint64()
	fn()
	metrics.Read(sample)
	return sample[0].Value.Uint64() - before
}

// TestRunRefusesRunawayGeneratorsWithoutADeadline is the half a cancellable
// context does NOT cover.
//
// Every expression here produces at most ONE top-level output, so the collected
// stream is not what bounds them. Two budgets share the work and the SPLIT IS
// THE POINT, which is
// why each case names the sentinel it must trip: the step budget answers a
// program that spins, the memory budget answers one that manufactures data, and
// a step count cannot see the second — `[range(2000) | "x"*1000000]` is half a
// percent of a four-million-step budget and 1.9 GB of heap. The context is
// Background, with no deadline, because that is the point: a caller who forgets
// one is still bounded.
func TestRunRefusesRunawayGeneratorsWithoutADeadline(t *testing.T) {
	for _, tc := range []struct {
		expr     string
		want     error
		maxAlloc uint64
	}{
		{"def f: f; f", ErrWorkBudget, 64 << 20},
		{"[range(20000000)] | length", ErrWorkBudget, 512 << 20},
		{"[recurse(.+1)] | length", ErrWorkBudget, 512 << 20},
		{`[range(2000) | "x"*1000000]`, ErrMemoryBudget, 512 << 20},
		{`[range(20000) | "y"*100000]`, ErrMemoryBudget, 512 << 20},
	} {
		t.Run(tc.expr, func(t *testing.T) {
			f, err := Compile(tc.expr)
			if err != nil {
				t.Fatalf("compile: %v", err)
			}

			done := make(chan error, 1)
			alloc := allocatedDuring(func() {
				go func() {
					// A number, so `recurse(.+1)` has something to add to.
					_, runErr := f.Run(context.Background(), 0.0)
					done <- runErr
				}()
				select {
				case err := <-done:
					if !errors.Is(err, tc.want) {
						t.Errorf("want %v, got: %v", tc.want, err)
					}
				case <-time.After(5 * time.Second):
					t.Errorf("%q was never stopped; the budgets are not applying", tc.expr)
				}
			})
			if alloc > tc.maxAlloc {
				t.Errorf("allocated %d MiB refusing %q, want under %d MiB — the budget is not bounding memory",
					alloc>>20, tc.expr, tc.maxAlloc>>20)
			}
		})
	}
}

// TestRunAllowsADenseHonestListing is the other edge, and it is the case a flat
// step budget got wrong.
//
// Cost tracks ELEMENTS, so the same 5 MiB body costs nine times as much when
// its rows are thin. Measured, `[.[] | select(.id != null) | {id, name}]` costs
// 7,200,000 steps over 194,000 thin rows against 790,000 over 21,000 fat ones,
// and a flat four million refused the first in 44 ms.
func TestRunAllowsADenseHonestListing(t *testing.T) {
	var b strings.Builder
	b.WriteByte('[')
	const row = `{"id":"a1b2c3","name":"n"}`
	for i := 0; b.Len() < 5<<20; i++ {
		if i > 0 {
			b.WriteByte(',')
		}
		b.WriteString(row)
	}
	b.WriteByte(']')

	f, err := Compile(`[.[] | select(.id != null) | {id, name}] | length`)
	if err != nil {
		t.Fatalf("compile: %v", err)
	}
	got, err := f.RunBytes(context.Background(), []byte(b.String()))
	if err != nil {
		t.Fatalf("an honest filter over a dense 5 MiB body was refused: %v", err)
	}
	if len(got) != 1 {
		t.Fatalf("got %d outputs, want 1", len(got))
	}
}

// TestRunSurfacesAnErrorRaisedLateInTheStream is a REGRESSION test, and the
// regression it pins was shipped.
//
// A jq program is a STREAM and a stream can fail LATE, so nothing may return on
// the strength of the values it has already collected. The capped form used to
// return the moment it held 10,000: measured on 20,000 items with a type error
// at index 15,000, `ronja api --jq` went from `"cannot add"` and EXIT 1 on main
// to ten thousand values, a nil error and EXIT 0 on the branch. `halt_error`
// behaved identically. A command that prints a prefix of a stream that was going
// to fail, and exits successfully, is worse than one that prints nothing.
func TestRunSurfacesAnErrorRaisedLateInTheStream(t *testing.T) {
	const items = 20000
	const failAt = 15000

	var b strings.Builder
	b.WriteByte('[')
	for i := 0; i < items; i++ {
		if i > 0 {
			b.WriteByte(',')
		}
		fmt.Fprintf(&b, "%d", i)
	}
	b.WriteByte(']')
	body := []byte(b.String())

	for _, tc := range []struct{ name, expr, want string }{
		{
			"a type error late in the stream",
			fmt.Sprintf(`.[] | if . == %d then . + "boom" else . end`, failAt),
			"cannot add",
		},
		{
			"halt_error late in the stream",
			fmt.Sprintf(`.[] | if . == %d then halt_error("stop") else . end`, failAt),
			"stop",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f, err := Compile(tc.expr)
			if err != nil {
				t.Fatalf("compile: %v", err)
			}
			out, err := f.RunBytes(context.Background(), body)
			if err == nil {
				t.Fatalf("a late error was swallowed: %d values came back clean", len(out))
			}
			if !strings.Contains(err.Error(), tc.want) {
				t.Errorf("want the stream's own error (%q), got: %v", tc.want, err)
			}
			// Nothing partial escapes: the caller renders what it is given, so
			// a prefix returned beside an error would be printed beside one.
			if out != nil {
				t.Errorf("a failed run returned %d values; it must return none", len(out))
			}
		})
	}
}
