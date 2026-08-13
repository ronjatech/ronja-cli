package commands

import (
	"context"
	"fmt"
	"os"
	"time"

	"github.com/ronjatech/ronja-cli/internal/api"
	"github.com/ronjatech/ronja-cli/internal/config"
	"github.com/ronjatech/ronja-cli/internal/wfdir"
	"github.com/spf13/cobra"
)

// `ronja wf status` answers the three questions you have before pushing: what
// have I changed, what is the workflow's state over there, and has anything
// moved underneath me since I last synced.
//
// It mutates nothing — not even a checkout — so it is safe to run at any point,
// including against a workflow you only have read access to.
func newWorkflowStatusCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "status",
		Short: "Show local changes, remote state and drift",
		Long: `Show local changes, remote state and drift.

Three things, for the instance this folder is bound to:

  LOCAL    files added, modified or deleted since the last sync
  REMOTE   the workflow's lifecycle, and whether you have an open draft
  DRIFT    files changed on the server since the last sync — the web builder
           edits the same per-user draft, so this is a real collision, not a
           theoretical one

Read-only: no draft is created and nothing is written. Run it from anywhere
inside the folder.

With --json, one object carrying all of the above.`,
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			// Deliberately NOT resolveInstance: a signed-out caller can still be
			// told what changed locally, which is most of the value and costs
			// nothing. The remote half degrades to "not checked" with a reason.
			resolved, err := config.Resolve(flagURL, flagProfile)
			if err != nil {
				return err
			}
			f, err := openFolder(cmd.Context(), resolved, wfdir.WorkflowKind)
			if err != nil {
				return err
			}

			report := &statusReport{
				Root:              f.Root,
				URL:               resolved.URL,
				Title:             f.Manifest.Title,
				Entrypoint:        f.Manifest.Entrypoint,
				ManagesParameters: f.Manifest.ManagesParameters(),
				Parameters:        describeParameters(f.Manifest.DeclaredParameters()),
				Bound:             f.Bound,
				Ambiguous:         f.BindingErr != nil,
				WorkflowID:        f.Binding.WorkflowID,
				FeatureID:         f.Binding.FeatureID,
			}

			enumeration, err := wfdir.Enumerate(f.Root, wfdir.WorkflowKind)
			if err != nil {
				return err
			}
			baseline := f.State.For(f.Key)
			report.Local = wfdir.DiffHashes(enumeration.Files, baseline.Hashes())
			report.Skipped = enumeration.Skipped
			if baseline != nil {
				report.Baseline = &baselineReport{
					SourceID:        baseline.SourceID,
					SourceLifecycle: baseline.SourceLifecycle,
					UpdatedAt:       baseline.BaselineUpdatedAt,
				}
			}

			report.Remote, report.WorkflowURL = remoteStatus(cmd.Context(), resolved, f)

			if flagJSON {
				return emitJSON(report)
			}
			printStatus(report)
			return nil
		},
	}
	return cmd
}

// statusReport is the --json shape, and the same struct the human renderer
// reads — so the two can never describe different things.
type statusReport struct {
	Root       string `json:"root"`
	URL        string `json:"url"`
	Title      string `json:"title"`
	Entrypoint string `json:"entrypoint"`
	// ManagesParameters distinguishes a folder that declares none from one that
	// does not manage them at all — the same distinction the manifest's pointer
	// carries, and a caller branching on Parameters alone would lose it.
	ManagesParameters bool   `json:"managesParameters"`
	Parameters        string `json:"parameters,omitempty"`
	Bound             bool   `json:"bound"`
	// Ambiguous reports a folder that IS bound here, to more than one
	// organization, with no way to tell which applies — the signed-out case.
	// Distinct from Bound so a caller does not read "not bound" and conclude a
	// push would create a new workflow.
	Ambiguous  bool   `json:"ambiguous,omitempty"`
	WorkflowID string `json:"workflowID,omitempty"`
	FeatureID  string `json:"featureID,omitempty"`
	// WorkflowURL is where a human looks at the workflow, as the SERVER reported
	// it on the row — never templated from the instance URL, which is the API
	// origin (see printResourceURL).
	//
	// A fact the REMOTE half establishes, so it is empty whenever that half was
	// not reached at all: signed out, unbound, or unreachable. Guessing there
	// would mean guessing at both the origin and the row.
	//
	// Rendered by the human report ONLY, like the same link on push and publish:
	// this struct's `url` key already means the INSTANCE origin, and a second
	// url under another name is a shape change to something scripts parse. A
	// caller that wants the page reads it off the API response the CLI got it
	// from (`ronja api /api/v2/workflow/<id> --jq .url`).
	WorkflowURL string          `json:"-"`
	Local       wfdir.Diff      `json:"local"`
	Skipped     []wfdir.Skipped `json:"skipped,omitempty"`
	Baseline    *baselineReport `json:"baseline,omitempty"`
	Remote      *remoteReport   `json:"remote"`
}

type baselineReport struct {
	SourceID        string `json:"sourceID"`
	SourceLifecycle string `json:"sourceLifecycle,omitempty"`
	UpdatedAt       string `json:"updatedAt,omitempty"`
}

// remoteReport is deliberately shaped so "we did not look" and "we looked and
// it is fine" are different states. Problem is the one field a caller should
// branch on: it is set whenever the binding cannot be used as it stands.
type remoteReport struct {
	Checked bool `json:"checked"`
	// NotCheckedReason is why the remote half was not looked at. Deliberately
	// NOT called "skipped": statusReport.Skipped already means "a local file
	// that will never be synced", and one JSON document using one word for two
	// unrelated things is a trap for anything parsing it.
	NotCheckedReason string `json:"notCheckedReason,omitempty"`
	Problem          string `json:"problem,omitempty"`
	Lifecycle        string `json:"lifecycle,omitempty"`
	// Draft describes the caller's own open draft, when they have one.
	Draft *draftReport `json:"draft,omitempty"`
	// ComparedAgainst names the row DRIFT was measured against — the draft when
	// there is one (it is what a push would write to), otherwise the live row.
	ComparedAgainst *comparedReport `json:"comparedAgainst,omitempty"`
	Drift           *wfdir.Diff     `json:"drift,omitempty"`
	// DriftNotes explain a comparison that is technically valid but not
	// apples-to-apples, e.g. a baseline taken from live and a draft opened since.
	//
	// A LIST because these accumulate independently and the two that matter most
	// arrive together: "could not check for your open draft" followed by
	// "comparing against the live row". A single field let the second overwrite
	// the first, so drift measured against live — while the caller may well have
	// a draft — reported itself as an ordinary baseline mismatch.
	DriftNotes []string `json:"driftNotes,omitempty"`
	// Parameters is set only when the folder MANAGES parameters and its
	// declaration differs from the row's — i.e. only when a push would change
	// something. Absent is the ordinary "they agree, or the folder does not
	// manage them" case.
	Parameters *parameterReport `json:"parameters,omitempty"`
}

// parameterReport is the declaration on each side, by name — enough to see what
// a push would do without reprinting the whole set.
type parameterReport struct {
	Local  string `json:"local"`
	Remote string `json:"remote"`
}

// note appends one drift caveat. Every caller uses this rather than assigning,
// which is the whole point.
func (r *remoteReport) note(format string, args ...any) {
	r.DriftNotes = append(r.DriftNotes, fmt.Sprintf(format, args...))
}

type draftReport struct {
	ID                   string `json:"id"`
	SubmittedForReviewAt string `json:"submittedForReviewAt,omitempty"`
	UpdatedAt            string `json:"updatedAt,omitempty"`
}

type comparedReport struct {
	ID        string `json:"id"`
	Lifecycle string `json:"lifecycle"`
}

// remoteStatus gathers everything server-side, degrading rather than aborting.
//
// Same principle as `ronja context`'s optional /llms.txt fetch: a failed
// optional read prints a note and the command still delivers what it can. The
// local half of a status is worth having on a plane.
//
// The second return is the workflow's frontend page, which belongs to the
// top-level report rather than to this block — but only the remote read can
// produce it, since the link is the SERVER's to give (see printResourceURL).
// Returned alongside instead of added to remoteReport so the --json shape stays
// exactly as callers already find it; "" whenever the remote half was not
// reached, which prints as nothing at all.
func remoteStatus(ctx context.Context, resolved *config.Resolved, f *folder) (*remoteReport, string) {
	out := &remoteReport{}
	// Bound to several organizations here, and signed out, so which binding
	// applies is genuinely unknown. Reported rather than guessed: picking one
	// would show another organization's workflow as if it were this folder's.
	if f.BindingErr != nil {
		out.NotCheckedReason = fmt.Sprintf("%s on %s — sign in (`ronja login`), or pass --profile to say which one",
			f.BindingErr, resolved.URL)
		return out, ""
	}
	if !f.Bound {
		out.NotCheckedReason = fmt.Sprintf("this folder is not bound to %s yet — the first push will create the binding", resolved.URL)
		return out, ""
	}
	if resolved.Token == "" {
		out.NotCheckedReason = fmt.Sprintf("not signed in to %s — run `ronja login --url %s`", resolved.URL, resolved.URL)
		return out, ""
	}
	if f.Binding.WorkflowID == "" {
		out.NotCheckedReason = "no workflow exists on this instance yet — the first push will create it"
		return out, ""
	}

	client := api.New(resolved.URL, resolved.Token)
	wf, err := client.GetWorkflow(ctx, f.Binding.WorkflowID)
	if err != nil {
		if api.StatusOf(err) == 404 {
			out.Checked = true
			out.Problem = fmt.Sprintf("binding broken: workflow %s no longer exists on %s (or you lost access to it)",
				f.Binding.WorkflowID, resolved.URL)
			return out, ""
		}
		out.NotCheckedReason = fmt.Sprintf("could not read workflow %s: %v", f.Binding.WorkflowID, err)
		return out, ""
	}
	out.Checked = true
	out.Lifecycle = wf.Lifecycle
	// The WORKFLOW's page, not the draft's: this is the row the binding names
	// and the one a reader means by "the workflow", whoever has a draft open.
	pageURL := wf.URL
	if err := refuseUnclonable(wf); err != nil {
		// Reported WITH the link: an archived or proposed row is exactly the
		// case whose fix is "go and look at it in the web UI", and the link is
		// still the server's own — a VERSION row, which has no page, comes back
		// with no url at all rather than one this could have invented.
		out.Problem = "binding broken: " + err.Error()
		return out, pageURL
	}

	// The row a push would target: your draft if you have one, else the live
	// workflow (which push would check out first).
	target := wf
	if wf.Lifecycle == api.LifecycleLive {
		draft, err := client.GetWorkflowDraft(ctx, wf.ID)
		if err != nil {
			// Load-bearing, and it used to be lost: everything below then
			// measures drift against the LIVE row while the caller may have a
			// draft that a push would actually write to.
			out.note("could not check for your open draft (%v) — drift below is against the live version", err)
		} else if draft != nil {
			target = draft
		}
	}
	if target.Lifecycle == api.LifecycleDraft {
		out.Draft = &draftReport{ID: target.ID, UpdatedAt: formatTime(&target.UpdatedAt)}
		out.Draft.SubmittedForReviewAt = formatTime(target.SubmittedForReviewAt)
	}

	files, err := client.ListWorkflowFiles(ctx, target.ID)
	if err != nil {
		out.note("could not read remote files: %v", err)
		return out, pageURL
	}
	out.ComparedAgainst = &comparedReport{ID: target.ID, Lifecycle: target.Lifecycle}
	baseline := f.State.For(f.Key)
	drift := wfdir.DiffHashes(hashFiles(files), baseline.Hashes())
	out.Drift = &drift
	// Reported only for a folder that manages parameters: for one that does not,
	// the row's declaration is not the folder's business and flagging a
	// difference would be advising a change push deliberately will not make.
	if f.Manifest.ManagesParameters() && !sameParameters(f.Manifest.DeclaredParameters(), target.Parameters) {
		out.Parameters = &parameterReport{
			Local:  describeParameters(f.Manifest.DeclaredParameters()),
			Remote: describeParameters(target.Parameters),
		}
	}
	switch {
	case baseline == nil:
		out.note("no local baseline — every remote file reads as new (this folder was never synced from here)")
	case baseline.SourceID != target.ID:
		out.note("baseline came from %s (%s); comparing against %s (%s)",
			baseline.SourceID, baseline.SourceLifecycle, target.ID, target.Lifecycle)
	}
	return out, pageURL
}

// formatTime renders an optional timestamp, or "" when absent.
func formatTime(t *time.Time) string {
	if t == nil || t.IsZero() {
		return ""
	}
	return t.UTC().Format(time.RFC3339)
}

func printStatus(r *statusReport) {
	out := os.Stdout
	fmt.Fprintf(out, "  %s\n", r.Title)
	fmt.Fprintf(out, "  %s\n\n", r.Root)

	fmt.Fprintf(out, "  Instance:   %s\n", r.URL)
	if r.Bound && r.WorkflowID != "" {
		fmt.Fprintf(out, "  Workflow:   %s\n", r.WorkflowID)
	} else if r.Bound {
		fmt.Fprintf(out, "  Workflow:   not created yet (first push will create it)\n")
	} else if r.Ambiguous {
		// "not bound" would flatly contradict the Remote line below, which is
		// about to say the folder is bound to several organizations here.
		fmt.Fprintf(out, "  Workflow:   bound here, but to several organizations — pick one with --profile\n")
	} else {
		fmt.Fprintf(out, "  Workflow:   not bound to this instance\n")
	}
	if r.FeatureID != "" {
		fmt.Fprintf(out, "  Feature:    %s\n", r.FeatureID)
	}
	fmt.Fprintf(out, "  Entrypoint: %s\n", r.Entrypoint)
	// Only for a folder that manages them: printing "Parameters: none" at a
	// folder that has simply never declared any would read as a fact about the
	// workflow rather than about the folder.
	if r.ManagesParameters {
		fmt.Fprintf(out, "  Parameters: %s\n", r.Parameters)
	}
	printResourceURL(out, statusKeyWidth, r.WorkflowURL)

	fmt.Fprintf(out, "\n  Local changes\n")
	if !r.Local.Dirty() {
		fmt.Fprintf(out, "    clean (%d file(s) match the last sync)\n", r.Local.Unchanged)
	} else {
		printPathList(out, "new", r.Local.Added)
		printPathList(out, "modified", r.Local.Modified)
		printPathList(out, "deleted", r.Local.Deleted)
		fmt.Fprintf(out, "    %d unchanged\n", r.Local.Unchanged)
	}
	for _, s := range r.Skipped {
		fmt.Fprintf(out, "    skipped  %s — %s\n", s.Path, s.Reason)
	}

	fmt.Fprintf(out, "\n  Remote\n")
	switch {
	case r.Remote.NotCheckedReason != "":
		fmt.Fprintf(out, "    not checked: %s\n", r.Remote.NotCheckedReason)
	case r.Remote.Problem != "":
		fmt.Fprintf(out, "    %s\n", r.Remote.Problem)
	default:
		fmt.Fprintf(out, "    lifecycle: %s\n", api.DescribeLifecycle(r.Remote.Lifecycle))
		if r.Remote.Draft != nil {
			fmt.Fprintf(out, "    your draft: %s\n", r.Remote.Draft.ID)
			if r.Remote.Draft.SubmittedForReviewAt != "" {
				fmt.Fprintf(out, "    submitted for review: %s (an admin commits it)\n",
					r.Remote.Draft.SubmittedForReviewAt)
			}
		} else if r.Remote.Lifecycle == api.LifecycleLive {
			fmt.Fprintf(out, "    your draft: none (a push would open one)\n")
		}
	}

	if r.Remote.Drift != nil {
		fmt.Fprintf(out, "\n  Drift since last sync")
		if r.Remote.ComparedAgainst != nil {
			fmt.Fprintf(out, " (vs %s)", r.Remote.ComparedAgainst.ID)
		}
		fmt.Fprintln(out)
		if !r.Remote.Drift.Dirty() && r.Remote.Parameters == nil {
			fmt.Fprintf(out, "    none\n")
		} else {
			printPathList(out, "new", r.Remote.Drift.Added)
			printPathList(out, "modified", r.Remote.Drift.Modified)
			printPathList(out, "deleted", r.Remote.Drift.Deleted)
			// In the same list as the files: the author wants one answer to
			// "what would a push change", not a files answer and a separate
			// metadata answer.
			if p := r.Remote.Parameters; p != nil {
				fmt.Fprintf(out, "    %-9s parameters: %s here, %s there\n", "changed", p.Local, p.Remote)
			}
		}
	}
	for _, note := range r.Remote.DriftNotes {
		fmt.Fprintf(out, "    note: %s\n", note)
	}
}

func printPathList(out *os.File, label string, paths []string) {
	for _, p := range paths {
		fmt.Fprintf(out, "    %-9s %s\n", label, p)
	}
}
