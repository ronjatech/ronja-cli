package jqf

import (
	"strings"
	"testing"
)

// What is worth testing here is the thin layer around gojq, not gojq: the
// stream-to-slice collection, the raw-string rendering, and the two truth rules
// a poll loop depends on.

func TestCompileRejectsBadExpression(t *testing.T) {
	_, err := Compile(".items[")
	if err == nil {
		t.Fatal("expected a parse error")
	}
	// The expression has to be in the message: a caller sees only their own
	// one-line flag, and a bare position offset locates nothing.
	if !strings.Contains(err.Error(), ".items[") {
		t.Fatalf("error does not quote the expression: %v", err)
	}
}

func TestRunCollectsEveryOutput(t *testing.T) {
	f, err := Compile(".items[].id")
	if err != nil {
		t.Fatalf("compile: %v", err)
	}
	got, err := f.RunBytes([]byte(`{"items":[{"id":"a"},{"id":"b"}]}`))
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
	got, err := f.RunBytes([]byte(`{"a":1}`))
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
	if _, err := f.RunBytes([]byte(`{"a":1}`)); err == nil {
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
	_, err = f.RunBytes([]byte("<html>gateway timeout</html>"))
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
