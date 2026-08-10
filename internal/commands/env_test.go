package commands

import (
	"os/exec"
	"strings"
	"testing"
)

// The output of `ronja env` is fed to `eval`, so a value that does not survive
// shell parsing is worse than a broken command — it is a credential silently
// mangled, or a metacharacter executing.
func TestShellQuote(t *testing.T) {
	tests := []struct {
		name string
		in   string
	}{
		{"typical token", "OVqehzLHL1ihsyRI8q1spYxw9L7MiAzXoqIZDp1jZ_ptNWPMGHa"},
		{"url", "https://app.ronja.tech"},
		{"url with port", "http://localhost:8082"},
		{"embedded single quote", "abc'def"},
		{"quote at the end", "abc'"},
		{"double quote", `abc"def`},
		{"dollar sign", "abc$USER"},
		{"backtick", "abc`whoami`"},
		{"semicolon", "abc;rm -rf /"},
		{"space", "abc def"},
		{"backslash", `abc\def`},
		{"newline", "abc\ndef"},
		{"empty", ""},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			quoted := shellQuote(tt.in)

			// Round-trip through a real shell: whatever we emit must come back
			// out byte-identical, with no expansion or word splitting.
			out, err := exec.Command("sh", "-c", "printf %s "+quoted).Output()
			if err != nil {
				t.Fatalf("shell rejected %s: %v", quoted, err)
			}
			if string(out) != tt.in {
				t.Errorf("round trip: quoted %q as %s, shell produced %q",
					tt.in, quoted, string(out))
			}
		})
	}
}

// The emitted lines must be valid `export` statements that eval can consume.
func TestShellQuoteProducesEvaluableExport(t *testing.T) {
	const token = "tok'en$with`nasty;chars"
	line := "export RONJA_TOKEN=" + shellQuote(token)

	out, err := exec.Command("sh", "-c", line+"; printf %s \"$RONJA_TOKEN\"").Output()
	if err != nil {
		t.Fatalf("eval of %q failed: %v", line, err)
	}
	if string(out) != token {
		t.Errorf("after eval, RONJA_TOKEN = %q, want %q", string(out), token)
	}
	// Guard against the value being split across lines, which would break the
	// two-line contract the command emits.
	if strings.Count(line, "\n") != 0 {
		t.Errorf("export line contains a newline: %q", line)
	}
}
