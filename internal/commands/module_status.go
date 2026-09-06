package commands

import (
	"context"
	"fmt"
	"os"
	"strings"

	"github.com/ronjatech/ronja-cli/internal/api"
	"github.com/ronjatech/ronja-cli/internal/config"
	"github.com/ronjatech/ronja-cli/internal/wfdir"
	"github.com/spf13/cobra"
)

// `ronja module status` answers the three questions you have before pushing:
// what have I changed, what is the module's state over there, and has anything
// moved underneath me since I last synced.
//
// It mutates nothing — not even a checkout — so it is safe to run at any point,
// including against a module you only have read access to.
func newModuleStatusCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "status",
		Short: "Show local changes, remote state and drift",
		Long: `Show local changes, remote state and drift.

Three things, for the instance this folder is bound to:

  LOCAL    .py files added, modified or deleted since the last sync
  REMOTE   the module's lifecycle, and whether a draft is open
  DRIFT    files changed on the server since the last sync

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
			f, err := openFolderForStatus(cmd.Context(), resolved, wfdir.ModuleKind)
			if err != nil {
				return err
			}

			report := &moduleStatusReport{
				Root:      f.Root,
				URL:       resolved.URL,
				Name:      f.Manifest.Name,
				Title:     f.Manifest.Title,
				Stack:     f.declaredStack(),
				Bound:     f.Bound,
				Ambiguous: f.BindingErr != nil,
				ModuleID:  f.Binding.ModuleID,
				FeatureID: f.Binding.FeatureID,
			}

			enumeration, err := wfdir.Enumerate(f.Root, wfdir.ModuleKind)
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
			// The package's own file, reported as a PROBLEM rather than as a
			// missing local file: a folder without it cannot be pushed at all
			// (the reconcile would try to delete the server's seed, which the
			// server refuses), and "status is clean but push refuses" is the shape
			// this line exists to prevent.
			if _, ok := enumeration.Files[moduleInitFile]; !ok {
				report.LocalProblem = fmt.Sprintf(
					"%s is missing — it is what makes this folder an importable package, and a push is refused without one",
					moduleInitFile)
			}

			// Nothing refuses a `dependencies` block on a module manifest at
			// load, and nothing ever fills one, so an author who committed one
			// gets silence from every command. Say so here, where they are
			// already looking.
			report.InertDependencies = f.Manifest.AliasNames()

			report.Remote = moduleRemoteStatus(cmd.Context(), resolved, f)

			if flagJSON {
				return emitJSON(report)
			}
			printModuleStatus(report)
			return nil
		},
	}
	return cmd
}

// moduleStatusReport is the --json shape, and the same struct the human
// renderer reads — so the two can never describe different things.
//
// It is a separate type from statusReport rather than a reuse of it, and
// deliberately so: that struct's keys name workflow concepts (entrypoint,
// parameters, reportingTimezone, runtime) that a module has none of, and a
// document carrying them all as `null` would invite a script to branch on
// something that can never be set.
type moduleStatusReport struct {
	Root  string `json:"root"`
	URL   string `json:"url"`
	Name  string `json:"name,omitempty"`
	Title string `json:"title,omitempty"`
	// Stack is the NAME of the environment this report is about, empty for a
	// folder still using the unnamed legacy "instances" shape.
	Stack string `json:"stack,omitempty"`
	Bound bool   `json:"bound"`
	// Ambiguous reports a folder that IS bound here, to more than one
	// organization, with no way to tell which applies. Distinct from Bound so a
	// caller does not read "not bound" and conclude a push would create a new
	// module.
	Ambiguous bool   `json:"ambiguous,omitempty"`
	ModuleID  string `json:"moduleID,omitempty"`
	FeatureID string `json:"featureID,omitempty"`
	// LocalProblem names something about the FOLDER that will stop a push, as
	// opposed to something about the sync. Today there is exactly one: no
	// __init__.py.
	LocalProblem string `json:"localProblem,omitempty"`
	// InertDependencies names the aliases a committed `dependencies` block in
	// this folder declares, which in a module folder can never resolve.
	//
	// A NOTE, not a LocalProblem, and the distinction is the field's whole
	// point: this does not stop a push, and filing it under the key that means
	// "your push will be refused" would send the reader to fix something that is
	// not blocking them. It is reported at all because the block is SILENT
	// otherwise — nothing refuses it at load, and an author who wrote it is
	// waiting for a binding that is never coming.
	//
	// Reported instead of the sibling folder.noteAliases() the other three
	// status commands call: that pre-flight compares declared names against the
	// markers in the folder's files, and its finding for a module folder would
	// be "no file in this folder uses it" — true, and read as "add the marker",
	// which is exactly the thing a module may not do.
	InertDependencies []string            `json:"inertDependencies,omitempty"`
	Local             wfdir.Diff          `json:"local"`
	Skipped           []wfdir.Skipped     `json:"skipped,omitempty"`
	Baseline          *baselineReport     `json:"baseline,omitempty"`
	Remote            *moduleRemoteReport `json:"remote"`
}

// moduleRemoteReport is shaped so "we did not look" and "we looked and it is
// fine" are different states. Problem is the one field a caller should branch
// on: it is set whenever the binding cannot be used as it stands.
type moduleRemoteReport struct {
	Checked bool `json:"checked"`
	// NotCheckedReason is why the remote half was not looked at. Deliberately
	// NOT called "skipped": the report's Skipped already means "a local file
	// that will never be synced", and one JSON document using one word for two
	// unrelated things is a trap for anything parsing it.
	NotCheckedReason string `json:"notCheckedReason,omitempty"`
	Problem          string `json:"problem,omitempty"`
	Lifecycle        string `json:"lifecycle,omitempty"`
	// Draft describes the module's open draft, when there is one. ⚠️ A module
	// has ONE draft, not one per author, so this may be a colleague's — which is
	// why drafterUserID is reported rather than assumed.
	Draft *moduleDraftReport `json:"draft,omitempty"`
	// ComparedAgainst names the row DRIFT was measured against — the draft when
	// there is one (it is what a push would write to), otherwise the live row.
	ComparedAgainst *comparedReport `json:"comparedAgainst,omitempty"`
	Drift           *wfdir.Diff     `json:"drift,omitempty"`
	// DriftNotes explain a comparison that is technically valid but not
	// apples-to-apples. A LIST because these accumulate independently.
	DriftNotes []string `json:"driftNotes,omitempty"`
	// Name and Title are set only when the folder DECLARES the field and the
	// row's differs — i.e. only when a push would change something.
	Name  *metadataFieldReport `json:"name,omitempty"`
	Title *metadataFieldReport `json:"title,omitempty"`
}

// metadataFieldReport is one metadata field on each side — enough to see what a
// push would do.
type metadataFieldReport struct {
	Local  string `json:"local"`
	Remote string `json:"remote"`
}

type moduleDraftReport struct {
	ID                   string `json:"id"`
	DrafterUserID        string `json:"drafterUserID,omitempty"`
	SubmittedForReviewAt string `json:"submittedForReviewAt,omitempty"`
	UpdatedAt            string `json:"updatedAt,omitempty"`
}

func (r *moduleRemoteReport) note(format string, args ...any) {
	r.DriftNotes = append(r.DriftNotes, fmt.Sprintf(format, args...))
}

// moduleRemoteStatus gathers everything server-side, degrading rather than
// aborting: the local half of a status is worth having on a plane.
func moduleRemoteStatus(ctx context.Context, resolved *config.Resolved, f *folder) *moduleRemoteReport {
	out := &moduleRemoteReport{}
	if reason := f.orgNotCheckedReason(); reason != "" {
		out.NotCheckedReason = reason
		return out
	}
	if f.BindingErr != nil {
		out.NotCheckedReason = fmt.Sprintf("%s on %s — sign in (`ronja login`), or pass --profile to say which one",
			f.BindingErr, resolved.URL)
		return out
	}
	if !f.Bound {
		out.NotCheckedReason = fmt.Sprintf("this folder is not bound to %s yet — the first push will create the binding", resolved.URL)
		return out
	}
	if resolved.Token == "" {
		out.NotCheckedReason = fmt.Sprintf("not signed in to %s — run `ronja login --url %s`", resolved.URL, resolved.URL)
		return out
	}
	if f.Binding.ModuleID == "" {
		out.NotCheckedReason = "no module exists on this instance yet — the first push will create it"
		return out
	}

	client := api.New(resolved.URL, resolved.Token)
	mod, err := client.GetModule(ctx, f.Binding.ModuleID)
	if err != nil {
		if api.StatusOf(err) == 404 {
			out.Checked = true
			// ⚠️ NOT reported as a broken binding, unlike the workflow status's
			// 404 leg, because here it is ambiguous. GET /module/:id resolves
			// live modules AND the caller's own draft or proposal, so a 404 is
			// either a module that is gone or somebody ELSE's in-flight row —
			// which answers 404 rather than 403 so that it discloses nothing.
			// Calling that "binding broken" would tell half the readers their
			// folder is fine and the other half it is not, with no way to tell.
			out.Problem = fmt.Sprintf("module %s cannot be read on %s — either it no longer exists (or you lost access to it), or it is an unpublished draft or proposal belonging to somebody else, which this route will not resolve for you",
				f.Binding.ModuleID, resolved.URL)
			return out
		}
		out.NotCheckedReason = fmt.Sprintf("could not read module %s: %v", f.Binding.ModuleID, err)
		return out
	}
	out.Checked = true
	out.Lifecycle = mod.Lifecycle
	if err := refuseUnclonableModule(mod); err != nil {
		out.Problem = "binding broken: " + err.Error()
		return out
	}

	// The row a push would target: the open draft if there is one, else the live
	// module (which push would check out first).
	target := mod
	if mod.Lifecycle == api.LifecycleLive {
		draft, err := client.GetModuleDraft(ctx, mod.ID)
		if err != nil {
			// Load-bearing: everything below otherwise measures drift against the
			// LIVE row while a draft a push would actually write to exists.
			out.note("could not check for an open draft (%v) — drift below is against the live version", err)
		} else if draft != nil {
			target = draft
		}
	}
	if target.ID != mod.ID || moduleIsUnpublished(target) {
		out.Draft = &moduleDraftReport{
			ID:                   target.ID,
			DrafterUserID:        target.DrafterUserID,
			UpdatedAt:            formatTime(&target.UpdatedAt),
			SubmittedForReviewAt: formatTime(target.SubmittedForReviewAt),
		}
	}

	files, err := client.ListModuleFiles(ctx, target.ID)
	if err != nil {
		out.note("could not read remote files: %v", err)
		return out
	}
	out.ComparedAgainst = &comparedReport{ID: target.ID, Lifecycle: target.Lifecycle}
	baseline := f.State.For(f.Key)
	drift := wfdir.DiffHashes(hashModuleFiles(files), baseline.Hashes())
	out.Drift = &drift

	// The metadata a push would change. Compared against the LIVE row, because
	// that is the row PUT /module/:id writes — a draft carries no claim on the
	// package name — so reporting the draft's would describe a change this loop
	// does not make.
	if f.Manifest.Name != "" && f.Manifest.Name != mod.Name {
		out.Name = &metadataFieldReport{Local: f.Manifest.Name, Remote: mod.Name}
	}
	if f.Manifest.Title != "" && f.Manifest.Title != mod.Title {
		out.Title = &metadataFieldReport{Local: f.Manifest.Title, Remote: mod.Title}
	}

	switch {
	case baseline == nil:
		out.note("no local baseline — every remote file reads as new (this folder was never synced from here)")
	case baseline.SourceID != target.ID:
		out.note("baseline came from %s (%s); comparing against %s (%s)",
			baseline.SourceID, baseline.SourceLifecycle, target.ID, target.Lifecycle)
	}
	return out
}

func printModuleStatus(r *moduleStatusReport) {
	out := os.Stdout
	fmt.Fprintf(out, "  %s\n", r.Title)
	fmt.Fprintf(out, "  %s\n\n", r.Root)

	fmt.Fprintf(out, "  Instance: %s\n", r.URL)
	if r.Stack != "" {
		fmt.Fprintf(out, "  Stack:    %s\n", r.Stack)
	}
	switch {
	case r.Bound && r.ModuleID != "":
		fmt.Fprintf(out, "  Module:   %s\n", r.ModuleID)
	case r.Bound:
		fmt.Fprintf(out, "  Module:   not created yet (first push will create it)\n")
	case r.Ambiguous:
		// "not bound" would flatly contradict the Remote line below, which is
		// about to say the folder is bound to several organizations here.
		fmt.Fprintf(out, "  Module:   bound here, but to several organizations — pick one with --profile\n")
	default:
		fmt.Fprintf(out, "  Module:   not bound to this instance\n")
	}
	if r.FeatureID != "" {
		fmt.Fprintf(out, "  Feature:  %s\n", r.FeatureID)
	}
	if r.Name != "" {
		fmt.Fprintf(out, "  Package:  %s\n", r.Name)
	}

	fmt.Fprintf(out, "\n  Local changes\n")
	if !r.Local.Dirty() {
		fmt.Fprintf(out, "    clean (%d file(s) match the last sync)\n", r.Local.Unchanged)
	} else {
		printPathList(out, "new", r.Local.Added)
		printPathList(out, "modified", r.Local.Modified)
		printPathList(out, "deleted", r.Local.Deleted)
		fmt.Fprintf(out, "    %d unchanged\n", r.Local.Unchanged)
	}
	if r.LocalProblem != "" {
		fmt.Fprintf(out, "    problem  %s\n", r.LocalProblem)
	}
	if len(r.InertDependencies) > 0 {
		fmt.Fprintf(out, "    note     %s declares the dependency name(s) %s, which will never resolve here — %s\n",
			wfdir.ManifestName, strings.Join(r.InertDependencies, ", "), moduleDependenciesInert)
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
		if d := r.Remote.Draft; d != nil {
			fmt.Fprintf(out, "    draft: %s\n", d.ID)
			if d.DrafterUserID != "" {
				// Named because a module has ONE draft: whose it is decides
				// whether `module discard` is tidying up or destroying somebody's
				// afternoon.
				fmt.Fprintf(out, "    drafter: %s\n", d.DrafterUserID)
			}
			if d.SubmittedForReviewAt != "" {
				fmt.Fprintf(out, "    submitted for review: %s (an admin commits it)\n", d.SubmittedForReviewAt)
			}
		} else if r.Remote.Lifecycle == api.LifecycleLive {
			fmt.Fprintf(out, "    draft: none (a push would open one)\n")
		}
	}

	if r.Remote.Drift != nil {
		fmt.Fprintf(out, "\n  Drift since last sync")
		if r.Remote.ComparedAgainst != nil {
			fmt.Fprintf(out, " (vs %s)", r.Remote.ComparedAgainst.ID)
		}
		fmt.Fprintln(out)
		if !r.Remote.Drift.Dirty() && r.Remote.Name == nil && r.Remote.Title == nil {
			fmt.Fprintf(out, "    none\n")
		} else {
			printPathList(out, "new", r.Remote.Drift.Added)
			printPathList(out, "modified", r.Remote.Drift.Modified)
			printPathList(out, "deleted", r.Remote.Drift.Deleted)
			// In the same list as the files: the author wants one answer to "what
			// would a push change", not a files answer and a metadata answer.
			if n := r.Remote.Name; n != nil {
				fmt.Fprintf(out, "    %-9s package name: %s here, %s there\n", "changed", n.Local, n.Remote)
			}
			if t := r.Remote.Title; t != nil {
				fmt.Fprintf(out, "    %-9s title: %s here, %s there\n", "changed", t.Local, t.Remote)
			}
		}
	}
	for _, note := range r.Remote.DriftNotes {
		fmt.Fprintf(out, "    note: %s\n", note)
	}
}
