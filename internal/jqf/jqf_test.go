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

// ⚠️ Nothing in this file calls t.Parallel(), deliberately: the memory budget
// reads a process-wide allocation counter (see heapAllocated), so a runaway test
// would spend a parallel honest test's budget and refuse it.

func TestCompileRejectsBadExpression(t *testing.T) {
	_, err := Compile(".result[")
	if err == nil {
		t.Fatal("expected a parse error")
	}
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

// Zero outputs, not a null: Holds distinguishes the two.
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

// Not a golden case: the failure it pins is a hang, which a case file cannot
// describe.
func TestRunIsCancellable(t *testing.T) {
	f, err := Compile("def f: f; f")
	if err != nil {
		t.Fatalf("compile: %v", err)
	}
	// ⚠️ Cancelled before Run: the step budget stops this program in ~15 ms, so
	// a racing deadline would pass without proving anything about ctx.
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

// Forty thousand is four times the 10,000-value cap this package must never
// regain; a test at that boundary would pass on an off-by-one.
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

// The memory budget, not a cap, bounds the collected stream.
func TestRunRefusesAnEnormousAnswerRatherThanTruncatingIt(t *testing.T) {
	// No single value is large and no array is built: only the collected
	// stream cannot fit.
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

// ⚠️ allocatedDuring is the assertion that matters below: a runaway shape can
// return the right error after allocating gigabytes.
func allocatedDuring(fn func()) uint64 {
	sample := []metrics.Sample{{Name: "/gc/heap/allocs:bytes"}}
	runtime.GC()
	metrics.Read(sample)
	before := sample[0].Value.Uint64()
	fn()
	metrics.Read(sample)
	return sample[0].Value.Uint64() - before
}

// Each case names the sentinel it must trip because a step count cannot see a
// program that manufactures data (`[range(2000) | "x"*1000000]` is ~18,000
// steps and 1.9 GB). No deadline: a caller who forgets one is still
// bounded.
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

// Cost tracks elements: this thin-row 5 MiB body costs ~7.2M steps, past the
// MinSteps floor, so it passes only because the budget scales with input size.
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

// The failure sits past 10,000 values, where a capped run would have returned
// a clean prefix.
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
			// The caller renders what it is given, so a prefix returned beside
			// an error would be printed.
			if out != nil {
				t.Errorf("a failed run returned %d values; it must return none", len(out))
			}
		})
	}
}
