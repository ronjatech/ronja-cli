package commands

import (
	"strings"
	"testing"

	"github.com/ronjatech/ronja-cli/internal/api"
)

// The audience sentence is composed server-side (rdataapp's DescribeAudience)
// and printed verbatim, so these pin the printing, not the wording.

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

// The review path (a non-admin editing an org-shared app) is the one that most
// needs the line.
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

// An older instance sends no audience; no empty bullet may appear.
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
