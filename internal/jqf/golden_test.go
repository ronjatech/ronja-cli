package jqf

import (
	"context"
	"encoding/json"
	"os"
	"strings"
	"testing"
)

// goldenPath is this package's semantics file: the cases where a hand-rolled
// lookalike gets jq wrong, run as assertions.
//
// ⚠️ IT MUST LIVE INSIDE THIS MODULE, not at the repo root. It was at the root
// once, back when a second copy of this package served the backend, and that was
// wrong in a way only CI could show: the test workflows use sparse-checkout
// limited to their own module directory, so a root fixture is simply ABSENT
// there — and cli/ is synced verbatim to the public ronja-cli mirror, where a
// path climbing out of the module does not exist at all. It passed locally and
// failed in both places. testdata/ beside the test is also just where Go
// expects it.
const goldenPath = "testdata/jq_golden.json"

type goldenCase struct {
	Name    string   `json:"name"`
	Expr    string   `json:"expr"`
	Input   string   `json:"input"`
	Outputs []string `json:"outputs"`
	Error   string   `json:"error"`
}

type goldenFile struct {
	Cases []goldenCase `json:"cases"`
}

// TestGoldenSemantics is this package's regression suite for jq semantics
// itself: `//`, `?`, an empty stream, and where a decode failure gets reported.
// It was written when a second copy of this package served the backend and CI
// diffed the two fixtures; that copy is gone, and the cases are worth keeping
// on their own, because each one is a place a hand-rolled lookalike — or a gojq
// upgrade — quietly gets jq wrong.
func TestGoldenSemantics(t *testing.T) {
	raw, err := os.ReadFile(goldenPath)
	if err != nil {
		t.Fatalf("read golden: %v (expected %s beside this test)", err, goldenPath)
	}
	var gf goldenFile
	if err := json.Unmarshal(raw, &gf); err != nil {
		t.Fatalf("parse shared golden: %v", err)
	}
	if len(gf.Cases) == 0 {
		t.Fatal("the shared golden is empty — that is a broken test, not a passing one")
	}

	for _, tc := range gf.Cases {
		t.Run(tc.Name, func(t *testing.T) {
			f, err := Compile(tc.Expr)
			if tc.Error == "compile" {
				if err == nil {
					t.Fatalf("expected a COMPILE error for %q", tc.Expr)
				}
				// The expression must be quoted: a bare parser position names a
				// place in a string the reader cannot see.
				if !strings.Contains(err.Error(), tc.Expr) {
					t.Errorf("compile error should quote the expression, got: %v", err)
				}
				return
			}
			if err != nil {
				t.Fatalf("compile %q: %v", tc.Expr, err)
			}

			got, err := f.RunBytes(context.Background(), []byte(tc.Input))
			switch tc.Error {
			case "decode":
				if err == nil {
					t.Fatal("expected a DECODE error")
				}
				// Decode and jq failures have opposite fixes, so the message
				// must not blame the expression.
				if !strings.Contains(err.Error(), "not JSON") {
					t.Errorf("decode failure must say the body is not JSON, got: %v", err)
				}
				return
			case "runtime":
				if err == nil {
					t.Fatalf("expected a RUNTIME error, got outputs %v", got)
				}
				return
			}
			if err != nil {
				t.Fatalf("run %q: %v", tc.Expr, err)
			}

			if len(got) != len(tc.Outputs) {
				t.Fatalf("got %d outputs %v, want %d %v", len(got), got, len(tc.Outputs), tc.Outputs)
			}
			for i, want := range tc.Outputs {
				encoded, err := json.Marshal(got[i])
				if err != nil {
					t.Fatalf("marshal output %d: %v", i, err)
				}
				if string(encoded) != want {
					t.Errorf("output %d = %s, want %s", i, encoded, want)
				}
			}
		})
	}
}
