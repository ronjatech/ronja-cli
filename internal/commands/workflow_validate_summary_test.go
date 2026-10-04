package commands

import (
	"bytes"
	"strings"
	"testing"

	"github.com/ronjatech/ronja-cli/internal/api"
)

// TestValidateSummaryCountsByTier pins the headline against the server's own
// severity, not against "every finding that is not an error". Two lies it must
// never tell: that an `info` note is a warning, and "Clean" over a warning — a
// probe builder read "Clean, with 1 warning" and moved on, and the warning was
// the only thing standing between it and a workflow that ran `done` having done
// nothing. A tier this build has never heard of counts as a warning, so the CLI
// never says "Clean" about something it does not understand.
func TestValidateSummaryCountsByTier(t *testing.T) {
	finding := func(sev, code string) api.ValidateFinding {
		return api.ValidateFinding{Severity: sev, Code: code, Path: "main.py", Message: "m"}
	}
	for _, tc := range []struct {
		name     string
		findings []api.ValidateFinding
		want     string
		never    []string
	}{
		{
			name:     "nothing",
			findings: nil,
			want:     "Clean.",
		},
		{
			name:     "info only",
			findings: []api.ValidateFinding{finding("info", "durable_trailing_call")},
			want:     "Clean, with 1 note — a save would succeed",
			never:    []string{"warning"},
		},
		{
			name:     "warning only",
			findings: []api.ValidateFinding{finding("warning", "entrypoint_main_guard")},
			want:     "No errors, 1 warning — a save would succeed; read them above",
			never:    []string{"Clean"},
		},
		{
			name: "warning and info",
			findings: []api.ValidateFinding{
				finding("warning", "entrypoint_main_guard"),
				finding("info", "durable_trailing_call"),
				finding("info", "durable_nondeterminism"),
			},
			want:  "No errors, 1 warning, 2 notes — a save would succeed; read them above",
			never: []string{"Clean"},
		},
		{
			name: "mixed with an error",
			findings: []api.ValidateFinding{
				finding("error", "unresolved_ref"),
				finding("warning", "secret_dropped"),
				finding("info", "durable_trailing_call"),
			},
			want:  "1 error, 1 warning, 1 note — a save would be refused",
			never: []string{"Clean"},
		},
		{
			name:     "unknown tier",
			findings: []api.ValidateFinding{finding("critical", "some_new_code")},
			want:     "No errors, 1 warning — a save would succeed; read them above",
			never:    []string{"Clean"},
		},
		{
			name:     "empty tier",
			findings: []api.ValidateFinding{finding("", "some_new_code")},
			want:     "No errors, 1 warning — a save would succeed; read them above",
			never:    []string{"Clean"},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var buf bytes.Buffer
			printValidateReport(&buf, &api.ValidateResult{Findings: tc.findings}, 0)
			out := buf.String()
			if !strings.Contains(out, tc.want) {
				t.Errorf("summary lacks %q:\n%s", tc.want, out)
			}
			// Judged on the SUMMARY lines only — the per-finding lines above
			// print the server's own label, which is a different claim.
			summary := out[strings.LastIndex(out, "\n\n")+1:]
			if len(tc.findings) == 0 {
				summary = out
			}
			for _, bad := range tc.never {
				if strings.Contains(summary, bad) {
					t.Errorf("summary says %q:\n%s", bad, summary)
				}
			}
		})
	}
}

// TestCountErrorsCountsByTier pins the refusal line — what `wf validate` and
// `wf push` exit with — to the same three buckets.
func TestCountErrorsCountsByTier(t *testing.T) {
	got := countErrors(&api.ValidateResult{Findings: []api.ValidateFinding{
		{Severity: "error"}, {Severity: "error"}, {Severity: "warning"}, {Severity: "info"},
	}})
	if want := "2 errors, 1 warning and 1 note — nothing was saved"; got != want {
		t.Errorf("countErrors = %q, want %q", got, want)
	}
	got = countErrors(&api.ValidateResult{Findings: []api.ValidateFinding{
		{Severity: "error"}, {Severity: "info"},
	}})
	if want := "1 error and 1 note — nothing was saved"; got != want {
		t.Errorf("countErrors = %q, want %q", got, want)
	}
}

// TestPushFindingLinesLabelByTier pins push's stderr lines to the same
// classification: an `info` advisory is a Note, and everything else a push
// prints (warnings, and any tier this build does not know) is a Warning.
func TestPushFindingLinesLabelByTier(t *testing.T) {
	var buf bytes.Buffer
	printPushFindingLines(&buf, []api.ValidateFinding{
		{Severity: "info", Path: "main.py", Message: "a note"},
		{Severity: "warning", Path: "main.py", Message: "a warning"},
		{Severity: "", Path: "main.py", Message: "no tier"},
	})
	out := buf.String()
	for _, want := range []string{
		"  Note: main.py — a note\n",
		"  Warning: main.py — a warning\n",
		"  Warning: main.py — no tier\n",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("push lines lack %q:\n%s", want, out)
		}
	}
	if strings.Contains(out, "Warning: main.py — a note") {
		t.Errorf("an info finding printed as a Warning:\n%s", out)
	}
}
