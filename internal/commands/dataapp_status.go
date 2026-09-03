package commands

import (
	"context"
	"fmt"
	"os"

	"github.com/ronjatech/ronja-cli/internal/api"
	"github.com/ronjatech/ronja-cli/internal/config"
	"github.com/ronjatech/ronja-cli/internal/wfdir"
	"github.com/spf13/cobra"
)

// `ronja app status` answers the three questions you have before pushing: what
// have I changed, what is the app's state over there, and has anything moved
// underneath me since I last synced.
//
// It mutates nothing — not even a checkout — so it is safe to run at any point,
// including against an app you only have read access to.
func newDataAppStatusCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "status",
		Short: "Show local changes, remote state and drift",
		Long: `Show local changes, remote state and drift.

Four things, for the instance this folder is bound to:

  LOCAL    files added, modified or deleted since the last sync
  REMOTE   the app's lifecycle, whether you have an open draft, and whether
           that draft currently compiles
  ACCESS   what ronja.json grants the app, and how it differs from the server
  DRIFT    files changed on the server since the last sync — the web builder
           edits the same per-user draft, so this is a real collision

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
			root, err := folderRootHere(wfdir.DataAppKind)
			if err != nil {
				return err
			}
			report, err := dataAppStatusReportAt(cmd.Context(), root, resolved)
			if err != nil {
				return err
			}

			if flagJSON {
				return emitJSON(report)
			}
			printAppStatus(report)
			// No verdict, deliberately, and unchanged — see the same note on
			// `ronja wf status`.
			return nil
		},
	}
	return cmd
}

// dataAppStatusReportAt computes the report for ONE data-app folder at an
// explicit root, and prints nothing. See pipelineStatusReportAt for why the
// three status computations were hoisted out of their RunE closures.
func dataAppStatusReportAt(ctx context.Context, root string, resolved *config.Resolved) (*appStatusReport, error) {
	f, err := openFolderForStatusAt(ctx, root, resolved, wfdir.DataAppKind)
	if err != nil {
		return nil, err
	}

	// Reported, never refused — see folder.noteAliases.
	f.noteAliases()

	report := &appStatusReport{
		Root:          f.Root,
		URL:           resolved.URL,
		Title:         f.Manifest.Title,
		Entrypoint:    f.Manifest.Entrypoint,
		ManagesAccess: f.Manifest.ManagesAccess(),
		Access:        describeAccess(f.declaredAccess()),
		Stack:         f.declaredStack(),
		Bound:         f.Bound,
		Ambiguous:     f.BindingErr != nil,
		DataAppID:     f.Binding.DataAppID,
		FeatureID:     f.Binding.FeatureID,
	}

	enumeration, err := wfdir.Enumerate(f.Root, wfdir.DataAppKind)
	if err != nil {
		return nil, err
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

	report.Remote, report.AppURL = appRemoteStatus(ctx, resolved, f)
	return report, nil
}

// appStatusReport is the --json shape, and the same struct the human renderer
// reads — so the two can never describe different things.
type appStatusReport struct {
	Root       string `json:"root"`
	URL        string `json:"url"`
	Title      string `json:"title"`
	Entrypoint string `json:"entrypoint"`
	// ManagesAccess distinguishes a folder that grants nothing from one that does
	// not manage the allowlists at all — the same distinction the manifest's
	// pointer carries, and a caller branching on Access alone would lose it.
	ManagesAccess bool   `json:"managesAccess"`
	Access        string `json:"access,omitempty"`
	// Stack is the NAME of the environment this report is about, empty for a
	// folder still using the unnamed legacy "instances" shape. Reported because
	// a folder can name several and the answer to "which one am I looking at"
	// must not be inferred from the organization id.
	Stack string `json:"stack,omitempty"`
	Bound bool   `json:"bound"`
	// Ambiguous reports a folder that IS bound here, to more than one
	// organization, with no way to tell which applies — the signed-out case.
	Ambiguous bool   `json:"ambiguous,omitempty"`
	DataAppID string `json:"dataAppID,omitempty"`
	FeatureID string `json:"featureID,omitempty"`
	// AppURL is where a human looks at the app, as the SERVER reported it on
	// the row — never templated from the instance URL, which is the API origin
	// (see printResourceURL).
	//
	// That makes it a fact the REMOTE half establishes: it is empty until the
	// app exists, and also whenever the remote half was not checked at all —
	// signed out, unbound, or unreachable. Reporting a link in those cases would
	// mean guessing at both the origin and the row, so the local-only half of a
	// status is now silent about the URL rather than confidently wrong.
	AppURL   string           `json:"appURL,omitempty"`
	Local    wfdir.Diff       `json:"local"`
	Skipped  []wfdir.Skipped  `json:"skipped,omitempty"`
	Baseline *baselineReport  `json:"baseline,omitempty"`
	Remote   *appRemoteReport `json:"remote"`
}

// appRemoteReport is deliberately shaped so "we did not look" and "we looked and
// it is fine" are different states. Problem is the one field a caller should
// branch on: it is set whenever the binding cannot be used as it stands.
type appRemoteReport struct {
	Checked bool `json:"checked"`
	// NotCheckedReason is why the remote half was not looked at. Deliberately
	// NOT called "skipped": appStatusReport.Skipped already means "a local file
	// that will never be synced", and one JSON document using one word for two
	// unrelated things is a trap for anything parsing it.
	NotCheckedReason string       `json:"notCheckedReason,omitempty"`
	Problem          string       `json:"problem,omitempty"`
	Lifecycle        string       `json:"lifecycle,omitempty"`
	Draft            *draftReport `json:"draft,omitempty"`
	// Validated reports whether the row DRIFT was measured against currently
	// compiles — i.e. whether `app publish` could commit it as it stands. Only
	// meaningful for a draft, so nil for a folder with none.
	Validated *bool `json:"validated,omitempty"`
	// ComparedAgainst names the row DRIFT was measured against — the draft when
	// there is one (it is what a push would write to), otherwise the live row.
	ComparedAgainst *comparedReport `json:"comparedAgainst,omitempty"`
	Drift           *wfdir.Diff     `json:"drift,omitempty"`
	DriftNotes      []string        `json:"driftNotes,omitempty"`
	// AccessChanges is set only when the folder MANAGES the allowlists and its
	// declaration differs from the row's — i.e. only when a push would change
	// something. Listed per id rather than counted, because these are grants.
	AccessChanges []accessChange `json:"accessChanges,omitempty"`
	// RemoteAccess is what the server currently grants, for context alongside
	// the changes.
	RemoteAccess string `json:"remoteAccess,omitempty"`
}

func (r *appRemoteReport) note(format string, args ...any) {
	r.DriftNotes = append(r.DriftNotes, fmt.Sprintf(format, args...))
}

// reasonNoAppYet is this loop's reasonNothingBound — see reasonNoWorkflowYet
// for why it is a const. The string is unchanged from the literal it replaced.
const reasonNoAppYet = "no data app exists on this instance yet — the first push will create it"

// appRemoteStatus gathers everything server-side, degrading rather than
// aborting. The local half of a status is worth having on a plane.
//
// The second return is the app's frontend page, which belongs to the top-level
// report rather than to this block — but only the remote read can produce it,
// since the link is the SERVER's to give (see printResourceURL). Returned
// alongside instead of added to appRemoteReport so the --json shape stays where
// callers already find it; "" whenever the remote half was not reached, which
// prints as nothing at all.
func appRemoteStatus(ctx context.Context, resolved *config.Resolved, f *folder) (*appRemoteReport, string) {
	out := &appRemoteReport{}
	// The organization lookup failed, so this folder's binding cannot be trusted
	// to be the right one. Reported ahead of the ambiguity below because it is
	// the CAUSE of it whenever both are set.
	if reason := f.orgNotCheckedReason(); reason != "" {
		out.NotCheckedReason = reason
		return out, ""
	}
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
	if f.Binding.DataAppID == "" {
		out.NotCheckedReason = reasonNoAppYet
		return out, ""
	}

	client := api.New(resolved.URL, resolved.Token)
	target, err := inspectAppTarget(ctx, client, f)
	if err != nil {
		// inspectAppTarget folds two different failures together, and status wants
		// them apart: a binding that names a row we can no longer use is a PROBLEM
		// the user must act on, while a transport failure is simply "not checked".
		if api.StatusOf(err) == 0 && target.App == nil {
			out.Checked = true
			out.Problem = err.Error()
			return out, ""
		}
		out.NotCheckedReason = fmt.Sprintf("could not read data app %s: %v", f.Binding.DataAppID, err)
		return out, ""
	}
	out.Checked = true
	// The APP's page, not the draft's: this is the row the binding names and the
	// one a reader means by "the app", whoever happens to have a draft open.
	pageURL := target.App.URL
	out.Lifecycle = target.App.Lifecycle
	out.RemoteAccess = describeAccess(target.Access())

	// The row a push would target: your draft if you have one, else the live app
	// (which push would fork first).
	row := target.FilesRow()
	if row.Lifecycle == api.LifecycleDraft {
		out.Draft = &draftReport{ID: row.ID, UpdatedAt: formatTime(&row.UpdatedAt)}
		out.Draft.SubmittedForReviewAt = formatTime(row.SubmittedForReviewAt)
		validated := row.IsValidated()
		out.Validated = &validated
	}

	// Addressed to the row we RESOLVED — the draft when one is open — because
	// GET :id/files answers with exactly the row it is named, and the drift
	// below has to be measured against the row the push wrote to.
	files, err := client.ListDataAppFiles(ctx, row.ID)
	if err != nil {
		out.note("could not read remote files: %v", err)
		return out, pageURL
	}
	out.ComparedAgainst = &comparedReport{ID: row.ID, Lifecycle: row.Lifecycle}
	baseline := f.State.For(f.Key)
	drift := wfdir.DiffHashes(hashAppFiles(f.Codec, files), baseline.Hashes())
	out.Drift = &drift
	// Reported only for a folder that manages the allowlists: for one that does
	// not, the row's grants are not the folder's business and flagging a
	// difference would be advising a change push deliberately will not make.
	if f.Manifest.ManagesAccess() {
		out.AccessChanges = diffAccess(target.Access(), f.declaredAccess())
	}
	switch {
	case baseline == nil:
		out.note("no local baseline — every remote file reads as new (this folder was never synced from here)")
	case baseline.SourceID != row.ID:
		out.note("baseline came from %s (%s); comparing against %s (%s)",
			baseline.SourceID, baseline.SourceLifecycle, row.ID, row.Lifecycle)
	}
	return out, pageURL
}

func printAppStatus(r *appStatusReport) {
	out := os.Stdout
	fmt.Fprintf(out, "  %s\n", r.Title)
	fmt.Fprintf(out, "  %s\n\n", r.Root)

	fmt.Fprintf(out, "  Instance:   %s\n", r.URL)
	if r.Stack != "" {
		// Printed next to the instance because it answers the same question a
		// step further in: which of this folder's environments is being reported.
		fmt.Fprintf(out, "  Stack:      %s\n", r.Stack)
	}
	if r.Bound && r.DataAppID != "" {
		fmt.Fprintf(out, "  Data app:   %s\n", r.DataAppID)
	} else if r.Bound {
		fmt.Fprintf(out, "  Data app:   not created yet (first push will create it)\n")
	} else if r.Ambiguous {
		fmt.Fprintf(out, "  Data app:   bound here, but to several organizations — pick one with --profile\n")
	} else {
		fmt.Fprintf(out, "  Data app:   not bound to this instance\n")
	}
	if r.FeatureID != "" {
		fmt.Fprintf(out, "  Feature:    %s\n", r.FeatureID)
	}
	fmt.Fprintf(out, "  Entrypoint: %s\n", r.Entrypoint)
	if r.ManagesAccess {
		fmt.Fprintf(out, "  Access:     %s\n", r.Access)
	}
	printResourceURL(out, statusKeyWidth, r.AppURL)

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
			if r.Remote.Validated != nil && !*r.Remote.Validated {
				// The single most useful line in this command: a draft that does not
				// compile cannot be published, and nothing else here would say so.
				//
				// But it is NOT "NO", because the wire cannot say that. All the row
				// carries is validated_at, which every file write clears in the same
				// transaction and only a successful compile re-stamps — so NULL is
				// "no clean build stands for these files", which is equally true
				// after a failed compile and after one that never ran (an outage, a
				// push with --no-validate, a builder edit). No compile status is
				// persisted anywhere, so "NO" was never a measured fact about the
				// author's code; this says what is known and names the command that
				// finds out the rest.
				fmt.Fprintf(out, "    compiles:   no clean build since the last change — `ronja app validate` to find out\n")
			} else if r.Remote.Validated != nil {
				fmt.Fprintf(out, "    compiles:   yes (ready to publish)\n")
			}
			if r.Remote.Draft.SubmittedForReviewAt != "" {
				fmt.Fprintf(out, "    submitted for review: %s (an admin commits it)\n",
					r.Remote.Draft.SubmittedForReviewAt)
			}
		} else if r.Remote.Lifecycle == api.LifecycleLive {
			fmt.Fprintf(out, "    your draft: none (a push would open one)\n")
		}
		if r.Remote.RemoteAccess != "" {
			fmt.Fprintf(out, "    access:     %s\n", r.Remote.RemoteAccess)
		}
	}

	if len(r.Remote.AccessChanges) > 0 {
		fmt.Fprintf(out, "\n  Access changes a push would make\n")
		printAccessChanges(out, r.Remote.AccessChanges)
	}

	if r.Remote.Drift != nil {
		fmt.Fprintf(out, "\n  Drift since last sync")
		if r.Remote.ComparedAgainst != nil {
			fmt.Fprintf(out, " (vs %s)", r.Remote.ComparedAgainst.ID)
		}
		fmt.Fprintln(out)
		if !r.Remote.Drift.Dirty() {
			fmt.Fprintf(out, "    none\n")
		} else {
			printPathList(out, "new", r.Remote.Drift.Added)
			printPathList(out, "modified", r.Remote.Drift.Modified)
			printPathList(out, "deleted", r.Remote.Drift.Deleted)
		}
	}
	for _, note := range r.Remote.DriftNotes {
		fmt.Fprintf(out, "    note: %s\n", note)
	}
}

// printAccessChanges lists every id being granted or revoked.
//
// In FULL, never counted. ronja.json is a committed file whose "access" block
// hands the app's viewers reach into the tables it names, and "3 tables differ"
// is not something a reviewer can act on.
func printAccessChanges(out *os.File, changes []accessChange) {
	for _, c := range changes {
		for _, id := range c.Added {
			fmt.Fprintf(out, "    grant    %-10s %s\n", c.Label, id)
		}
		for _, id := range c.Removed {
			fmt.Fprintf(out, "    revoke   %-10s %s\n", c.Label, id)
		}
	}
}
