package commands

import (
	"strings"
	"testing"

	"github.com/ronjatech/ronja-cli/internal/api"
)

// `ronja app publish` told you WHETHER your draft went live and nothing about
// who could then use it. For a system of record carrying a managed database's
// write role, those are not the same question — the second is the one that
// matters, and it was unanswered on every path.
//
// The sentence is composed server-side (rdataapp.DescribeAudience) and printed
// verbatim, so these pin the PRINTING, not the wording: that both outcomes show
// it, and that its absence degrades silently rather than printing a stub.

func writeRoleAudience() *api.DataAppAudience {
	return &api.DataAppAudience{
		Reach:       "organization",
		FeatureName: "Quality",
		Reads:       []string{`table "ncr"`},
		Writes:      []string{`secret "ncr_db write role"`},
		Sentence: `Everyone in your organization can open "NCR register" (its feature "Quality"). ` +
			`Anyone who opens it can read table "ncr" and WRITE via secret "ncr_db write role".`,
	}
}

func publishReportOutput(t *testing.T, r *appPublishResult) string {
	t.Helper()
	f, done := captureStdout(t)
	printAppPublishReport(r)
	return done(f)
}

func TestPrintAppPublishReport_PublishedPrintsTheAudience(t *testing.T) {
	out := publishReportOutput(t, &appPublishResult{
		Outcome:   outcomePublished,
		DataAppID: "data_app-1",
		DraftID:   "data_app-1",
		Detail:    `committed to "NCR register"`,
		Audience:  writeRoleAudience(),
	})
	if !strings.Contains(out, "Published.") {
		t.Fatalf("outcome line missing:\n%s", out)
	}
	if !strings.Contains(out, "can open") {
		t.Fatalf("audience sentence missing:\n%s", out)
	}
	if !strings.Contains(out, "ncr_db write role") {
		t.Fatalf("the write secret must be named:\n%s", out)
	}
}

// The review path is the ncr/1 shape — a non-admin editing an org-shared app —
// so it is the path that most needs the line, and the one that had no body to
// read it from until request-review started answering the commit shape.
func TestPrintAppPublishReport_SubmittedForReviewPrintsTheAudience(t *testing.T) {
	out := publishReportOutput(t, &appPublishResult{
		Outcome:   outcomeSubmittedForReview,
		DataAppID: "data_app-1",
		DraftID:   "data_app-2",
		Detail:    `"NCR register" is in a shared feature, so an admin commits changes to it`,
		Audience:  writeRoleAudience(),
	})
	if !strings.Contains(out, "Submitted for review") {
		t.Fatalf("outcome line missing:\n%s", out)
	}
	if !strings.Contains(out, "can open") {
		t.Fatalf("audience sentence missing on the review path:\n%s", out)
	}
}

// An older instance sends no audience. The report must be exactly what it was
// before rather than an empty bullet.
func TestPrintAppPublishReport_NoAudiencePrintsNothingExtra(t *testing.T) {
	out := publishReportOutput(t, &appPublishResult{
		Outcome:   outcomePublished,
		DataAppID: "data_app-1",
		DraftID:   "data_app-1",
		Detail:    `committed to "NCR register"`,
	})
	for _, line := range strings.Split(out, "\n") {
		if strings.TrimSpace(line) == "" {
			continue
		}
		if !strings.Contains(line, "Published") && !strings.Contains(line, "committed to") &&
			!strings.Contains(line, "Data app:") && !strings.Contains(line, "Draft:") {
			t.Fatalf("unexpected line with no audience: %q\n%s", line, out)
		}
	}
}

// A summary that arrived with no sentence (server-side hydration produced the
// structure but nothing to say) must not print a blank line either.
func TestPrintAppPublishReport_EmptySentencePrintsNothing(t *testing.T) {
	out := publishReportOutput(t, &appPublishResult{
		Outcome:   outcomePublished,
		DataAppID: "data_app-1",
		DraftID:   "data_app-1",
		Audience:  &api.DataAppAudience{Reach: "author"},
	})
	if strings.Contains(out, "author") {
		t.Fatalf("the machine-readable fields must not leak into the report:\n%s", out)
	}
}
