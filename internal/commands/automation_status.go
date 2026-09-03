package commands

import (
	"context"
	"errors"
	"fmt"
	"os"
	"sort"
	"strings"
	"time"

	"github.com/ronjatech/ronja-cli/internal/api"
	"github.com/ronjatech/ronja-cli/internal/config"
	"github.com/ronjatech/ronja-cli/internal/wfdir"
	"github.com/spf13/cobra"
)

// `ronja automation status` answers the two questions you have before pushing:
// what would a push change, and has anything moved underneath me since I last
// agreed with it.
//
// It mutates nothing — there is no draft to check out here, and nothing local to
// record — so it is safe to run at any point, including against a feature you
// only have read access to.
func newAutomationStatusCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "status",
		Short: "Show what a push would change, and what moved on the server",
		Long: `Show what a push would change, and what moved on the server.

For the instance this folder is bound to:

  NEW       .json files with no automation behind them — a push would create them
  CHANGES   the declared fields each bound automation's row does not already hold
  DRIFT     automations that changed on the server since this folder last agreed
            with them; a push refuses those, because there is no version and no
            compare-and-set to fall back on
  ORPHANS   automations this folder is bound to whose file is gone

Unlike the other loops there is no local baseline: a read returns the whole live
configuration, so this compares the COMMITTED files against the server directly.
A fresh clone therefore answers exactly what your own machine does.

Read-only. Run it from anywhere inside the folder. With --json, one object
carrying all of the above.

The exit code answers one question — has anything moved on the server? — which
makes this a CI gate that needs no parsing. Non-zero for drift, and equally for
an automation whose state could not be READ: an unreachable instance, an expired
token, a binding that no longer resolves, or a feature holding more automations
than one page carries. "I could not look" is not the same answer as "nothing
moved".`,
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			// Deliberately NOT resolveInstance: a signed-out caller can still be
			// told what its files declare, and the remote half degrades to "not
			// checked" with a reason. Same trade every other `status` makes.
			resolved, err := config.Resolve(flagURL, flagProfile)
			if err != nil {
				return err
			}
			root, err := folderRootHere(wfdir.AutomationKind)
			if err != nil {
				return err
			}
			report, err := automationStatusReportAt(cmd.Context(), root, resolved)
			if err != nil {
				return err
			}
			if flagJSON {
				if err := emitJSON(report); err != nil {
					return err
				}
			} else {
				printAutomationStatus(report)
			}
			return automationStatusVerdict(report)
		},
	}
	return cmd
}

// automationStatusReportAt computes the report for ONE automation folder at an
// explicit root, and prints nothing.
//
// Hoisted out of the RunE closure so `ronja sync` can ask the same question of
// every folder in a tree, exactly as pipelineStatusReportAt is: computing the
// answer and deciding what to do with it are separate steps, and a second copy
// of "is this folder clean" would drift from the one people run.
func automationStatusReportAt(ctx context.Context, root string, resolved *config.Resolved) (*automationStatusReport, error) {
	f, err := openFolderForStatusAt(ctx, root, resolved, wfdir.AutomationKind)
	if err != nil {
		return nil, err
	}
	// Reported, never refused — see folder.noteAliases.
	f.noteAliases()

	report := &automationStatusReport{
		Root:        f.Root,
		URL:         resolved.URL,
		Title:       f.Manifest.Title,
		Stack:       f.declaredStack(),
		Bound:       f.Bound,
		Ambiguous:   f.BindingErr != nil,
		FeatureID:   f.Binding.FeatureID,
		Automations: len(f.Binding.Automations),
		WillCreate:  []string{},
	}

	local, enumeration, err := readAutomationFiles(f.Root)
	if err != nil {
		return nil, err
	}
	report.Files = len(local)
	report.Skipped = enumeration.Skipped
	parsed, problems := parseAutomationFolder(local)
	for _, path := range sortedKeys(problems) {
		report.Problems = append(report.Problems,
			automationFileProblem{Path: path, Problem: problems[path].Error()})
	}
	for _, path := range sortedPaths(local) {
		if f.Binding.Automations[path] == "" {
			report.WillCreate = append(report.WillCreate, path)
		}
	}
	report.Orphans = automationOrphans(f.Binding, local)

	report.Remote = automationRemoteStatus(ctx, resolved, f, parsed, local)
	return report, nil
}

// automationStatusReport is the --json shape, and the same struct the human
// renderer reads — so the two can never describe different things.
type automationStatusReport struct {
	Root  string `json:"root"`
	URL   string `json:"url"`
	Title string `json:"title"`
	// Stack is the NAME of the environment this report is about, empty for a
	// folder still using the unnamed legacy "instances" shape.
	Stack string `json:"stack,omitempty"`
	Bound bool   `json:"bound"`
	// Ambiguous reports a folder that IS bound here, to more than one
	// organization, with no way to tell which applies — the signed-out case.
	Ambiguous bool   `json:"ambiguous,omitempty"`
	FeatureID string `json:"featureID,omitempty"`
	// Automations is how many files this folder is BOUND to; Files is how many
	// .json files it holds. They differ by WillCreate and by Orphans.
	Automations int             `json:"automations"`
	Files       int             `json:"files"`
	Skipped     []wfdir.Skipped `json:"skipped,omitempty"`
	// Problems is one entry per file that will not parse as a declaration.
	// Reported rather than fatal here: `status` is the command you run when you
	// are already suspicious, and the rest of the report is still worth having.
	Problems   []automationFileProblem `json:"problems,omitempty"`
	WillCreate []string                `json:"willCreate"`
	// Orphans are paths this folder's lock still binds and the folder no longer
	// holds. A push REFUSES on these unless --prune is given, because a bad
	// rebase must not silently stop a live automation.
	Orphans []string                `json:"orphans,omitempty"`
	Remote  *automationRemoteReport `json:"remote"`
}

type automationFileProblem struct {
	Path    string `json:"path"`
	Problem string `json:"problem"`
}

// automationRemoteReport is shaped so "we did not look" and "we looked and it is
// fine" are different states, the same way the other loops' are.
type automationRemoteReport struct {
	Checked bool `json:"checked"`
	// NotCheckedReason is why the remote half was not looked at. Not called
	// "skipped": Skipped above already means "a local file that will never be
	// synced", and one document using one word for two things is a trap for
	// anything parsing it.
	NotCheckedReason string `json:"notCheckedReason,omitempty"`
	// Problem is a folder-level failure — the listing itself could not be
	// trusted. The TRUNCATION refusal lands here, and it must: a listing that
	// saw a prefix of the feature cannot tell which rows are missing, and
	// reading it as "no automations" is exactly what produces a duplicate.
	Problem     string                `json:"problem,omitempty"`
	Automations []automationRowReport `json:"automations,omitempty"`
	// Unmanaged names the feature's automations no file in this folder describes
	// — made in the web app, or by a colleague's folder. Advisory only: this
	// loop never touches a row it did not create.
	Unmanaged []string `json:"unmanaged,omitempty"`
	Notes     []string `json:"notes,omitempty"`
}

func (r *automationRemoteReport) note(format string, args ...any) {
	r.Notes = append(r.Notes, fmt.Sprintf(format, args...))
}

// automationRowReport is one bound file's remote state.
type automationRowReport struct {
	Path         string `json:"path"`
	AutomationID string `json:"automationID"`
	Name         string `json:"name,omitempty"`
	TriggerKind  string `json:"triggerKind,omitempty"`
	Enabled      bool   `json:"enabled"`
	// DisabledReason is why a disabled row is disabled, and it is the field the
	// re-enable refusal quotes: "user" means a human paused it.
	DisabledReason string `json:"disabledReason,omitempty"`
	// Problem is set when this binding cannot be used as it stands — the row is
	// not in the feature any more, or the file declares something unpatchable.
	Problem string `json:"problem,omitempty"`
	// Drift is one of the drift* constants, computed from the row's updatedAt
	// against the anchor in ronja.lock.json. It is this loop's ONLY drift
	// signal: scheduled_jobs has no version history and no compare-and-set.
	Drift string `json:"drift,omitempty"`
	// Changes names the declared fields a push would write. Empty means the row
	// already says what the file says.
	Changes []string `json:"changes,omitempty"`
	// Orphan marks a row whose FILE is gone from the folder.
	//
	// A field rather than a Problem, because it is neither: the row was read
	// perfectly and there is nothing wrong with it — what is missing is the file.
	// Reported as a problem it would score as "could not be checked", which sends
	// a reader looking for an instance that would not answer, and would hide the
	// orphan sentence the verdict has for exactly this case.
	Orphan bool `json:"orphan,omitempty"`
}

// reasonNoAutomationsYet is the one NotCheckedReason that is not a failure to
// look: the folder is bound here, it names a feature, and nothing it describes
// has been pushed yet.
//
// A const rather than a literal because the exit code turns on the difference,
// and a gate that read one of these as the other would go green on an instance
// it never reached.
const reasonNoAutomationsYet = "no automations exist on this instance yet — the first push will create them"

// automationRemoteStatus reads the feature's automations ONCE and answers every
// per-file question from that listing.
//
// One request, not one per file, and unlike the pipeline loop there is no second
// tier: LIST is the route that carries the action, so a single call returns each
// row's whole configuration — schedule, action and reference set together.
//
// ⚠️ A TRUNCATED LISTING IS A FOLDER-LEVEL PROBLEM, never a short answer. See
// api.ErrAutomationListTruncated: the page token encodes an offset, so paging
// while rows are created or deleted skips one — and a row this loop cannot see
// reads as deleted, which is how the next push creates a duplicate beside a live
// automation. "I could not tell" is the only honest verdict there.
func automationRemoteStatus(ctx context.Context, resolved *config.Resolved, f *folder,
	parsed map[string]*automationFile, onDisk map[string]string) *automationRemoteReport {
	out := &automationRemoteReport{}
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
	if f.Binding.FeatureID == "" {
		out.NotCheckedReason = automationFeatureAdvice(f)
		return out
	}
	if len(f.Binding.Automations) == 0 {
		out.NotCheckedReason = reasonNoAutomationsYet
		return out
	}

	client := newClient(resolved.URL, resolved.Token)
	rows, err := client.ListAutomations(ctx, f.Binding.FeatureID)
	var truncated *api.ErrAutomationListTruncated
	switch {
	case errors.As(err, &truncated):
		out.Checked = true
		out.Problem = fmt.Sprintf("this feature's automations could not be listed completely: %v", truncated)
		return out
	case err != nil && api.StatusOf(err) == 404:
		out.Checked = true
		out.Problem = fmt.Sprintf("binding broken: feature %s no longer exists on %s (or you lost access to it)",
			f.Binding.FeatureID, resolved.URL)
		return out
	case err != nil:
		out.NotCheckedReason = fmt.Sprintf("could not list the automations of %s: %v", f.Binding.FeatureID, err)
		return out
	}
	out.Checked = true

	byID := automationRowsByID(rows)
	described := map[string]bool{}
	paths := make([]string, 0, len(f.Binding.Automations))
	for path := range f.Binding.Automations {
		paths = append(paths, path)
	}
	sort.Strings(paths)

	for _, path := range paths {
		id := f.Binding.Automations[path]
		described[id] = true
		report := automationRowReport{Path: path, AutomationID: id}
		row, present := byID[id]
		if !present {
			// A bound id the feature no longer holds. Per file rather than
			// aborting: the other bindings are still worth answering, and this
			// one is the answer.
			report.Problem = "binding broken: this automation is not in the feature any more (deleted, moved, or you lost access to it)"
			out.Automations = append(out.Automations, report)
			continue
		}
		report.Name, report.TriggerKind = row.Name, row.TriggerKind
		report.Enabled, report.DisabledReason = row.Enabled, row.DisabledReason
		_, seen := f.Lock.AutomationSeen(f.Stack, path)
		report.Drift = automationDrift(seen, row)

		if _, present := onDisk[path]; !present {
			// The FILE is gone, and the row is fine — so this is not a problem
			// with the automation and must not be reported as one. The folder's
			// Orphans list and the verdict's own sentence own this case.
			report.Orphan = true
			out.Automations = append(out.Automations, report)
			continue
		}
		file := parsed[path]
		if file == nil {
			// On disk and would not parse. There is no declaration to compare,
			// and report.Problems carries the parse refusal itself.
			report.Problem = "this file could not be read as a declaration, so nothing here says what a push would change"
			out.Automations = append(out.Automations, report)
			continue
		}
		// The comparison is made on the RESOLVED declaration — the ids a push
		// would actually send. Comparing the alias form against the row would
		// report every aliased reference as a change on every run, which is the
		// permanent-drift failure the codec exists to prevent.
		resolvedFile := *file
		if err := resolveAutomationRefs(&resolvedFile, f.Codec, f.Stack); err != nil {
			report.Problem = fmt.Sprintf("this file's references do not resolve, so nothing here says what a push would change:\n    %v", err)
			out.Automations = append(out.Automations, report)
			continue
		}
		if err := refuseUnpatchableAutomation(&resolvedFile, row); err != nil {
			report.Problem = fmt.Sprintf("a push would refuse this file: %v", err)
			out.Automations = append(out.Automations, report)
			continue
		}
		report.Changes, _ = automationDelta(automationNameFor(path), &resolvedFile, row)
		out.Automations = append(out.Automations, report)
	}

	for _, row := range rows {
		if row != nil && !described[row.ID] {
			out.Unmanaged = append(out.Unmanaged, fmt.Sprintf("%s (%s)", row.Name, row.ID))
		}
	}
	sort.Strings(out.Unmanaged)
	if len(out.Unmanaged) > 0 {
		out.note("%d automation(s) in this feature are not described by this folder; nothing here touches them", len(out.Unmanaged))
	}
	return out
}

// automationDrift answers whether one row has moved since this folder last
// agreed with it, from the anchor the lock recorded.
//
// ⚠️ THE COMPARISON IS BETWEEN INSTANTS, NEVER BETWEEN STRINGS. The anchor is
// written with time.RFC3339Nano, which TRIMS trailing zeros — so a fraction-less
// stamp sorts after a fractional one and `Z` sorts above `.`, and a string
// comparison would report drift on some rows and not others depending on what
// the clock happened to land on. Intermittently wrong is the worst way to be
// wrong for a guard nobody re-runs.
//
// An EMPTY anchor is driftNoBaseline — "no answer", never "agreed" — which
// disarms both this refusal and the re-enable one, exactly as an absent
// LiveSHA256 disarms a pipeline's.
func automationDrift(seen string, row *api.Automation) string {
	switch {
	case seen == "":
		return driftNoBaseline
	case row == nil:
		return driftUnreadable
	}
	anchor, err := time.Parse(time.RFC3339Nano, seen)
	if err != nil {
		return driftUnreadable
	}
	if row.UpdatedAt.Equal(anchor) {
		return driftNone
	}
	return driftChanged
}

func printAutomationStatus(r *automationStatusReport) {
	out := os.Stdout
	fmt.Fprintf(out, "  %s\n", r.Title)
	fmt.Fprintf(out, "  %s\n\n", r.Root)

	fmt.Fprintf(out, "  Instance:    %s\n", r.URL)
	if r.Stack != "" {
		fmt.Fprintf(out, "  Stack:       %s\n", r.Stack)
	}
	switch {
	case r.Bound && r.FeatureID != "":
		fmt.Fprintf(out, "  Feature:     %s\n", r.FeatureID)
	case r.Ambiguous:
		fmt.Fprintf(out, "  Feature:     bound here, but to several organizations — pick one with --profile\n")
	case r.Bound:
		fmt.Fprintf(out, "  Feature:     none named — a push refuses until one is\n")
	default:
		fmt.Fprintf(out, "  Feature:     not bound to this instance\n")
	}
	fmt.Fprintf(out, "  Automations: %d bound, %d file(s) here\n", r.Automations, r.Files)

	fmt.Fprintf(out, "\n  Local\n")
	for _, path := range r.WillCreate {
		fmt.Fprintf(out, "    creates  %s\n", path)
	}
	for _, path := range r.Orphans {
		fmt.Fprintf(out, "    orphan   %s — bound to an automation and gone from this folder; a push refuses until it is back or --prune is given\n", path)
	}
	for _, p := range r.Problems {
		fmt.Fprintf(out, "    refused  %s — %s\n", p.Path, p.Problem)
	}
	for _, s := range r.Skipped {
		fmt.Fprintf(out, "    skipped  %s — %s\n", s.Path, s.Reason)
	}
	if len(r.WillCreate) == 0 && len(r.Orphans) == 0 && len(r.Problems) == 0 && len(r.Skipped) == 0 {
		fmt.Fprintf(out, "    every file is bound to an automation\n")
	}

	fmt.Fprintf(out, "\n  Remote\n")
	switch {
	case r.Remote.NotCheckedReason != "":
		fmt.Fprintf(out, "    not checked: %s\n", r.Remote.NotCheckedReason)
	case r.Remote.Problem != "":
		fmt.Fprintf(out, "    %s\n", r.Remote.Problem)
	default:
		for _, a := range r.Remote.Automations {
			printAutomationRowStatus(out, a)
		}
	}
	for _, note := range r.Remote.Notes {
		fmt.Fprintf(out, "    note: %s\n", note)
	}
	for _, name := range r.Remote.Unmanaged {
		fmt.Fprintf(out, "      not in this folder: %s\n", name)
	}
}

func printAutomationRowStatus(out *os.File, a automationRowReport) {
	fmt.Fprintf(out, "    %s\n", a.Path)
	if a.Orphan {
		fmt.Fprintf(out, "      row:     %s (%s) — this file is gone; a push refuses until it is back, or --prune deletes it\n",
			a.AutomationID, a.Name)
		return
	}
	if a.Problem != "" {
		fmt.Fprintf(out, "      %s\n", a.Problem)
		return
	}
	state := "enabled"
	if !a.Enabled {
		state = "disabled"
		if a.DisabledReason != "" {
			state += " (" + a.DisabledReason + ")"
		}
	}
	fmt.Fprintf(out, "      row:     %s — %s trigger, %s\n", a.AutomationID, a.TriggerKind, state)
	switch a.Drift {
	case driftChanged:
		fmt.Fprintf(out, "      drift:   this automation changed on the server since your last push — a push refuses; --force overwrites\n")
	case driftNoBaseline:
		fmt.Fprintf(out, "      drift:   nothing to compare against yet — the next push records an anchor\n")
	case driftUnreadable:
		fmt.Fprintf(out, "      drift:   not checked\n")
	}
	if len(a.Changes) > 0 {
		fmt.Fprintf(out, "      changes: %s\n", strings.Join(a.Changes, ", "))
	}
}

// automationStatusVerdict turns the report into an exit code, answering ONE
// question: has anything moved on the server under this folder?
//
// Same contract as `ronja pipeline status`, and "could not check" fails for the
// same reason: a status that exits zero is read as "nothing has moved", and a
// revoked token, an unreachable instance or a truncated listing says nothing of
// the kind.
//
// A LOCAL change is not the gate — a file the author edited is what a push is
// for — but an ORPHAN is, because a push cannot proceed past one.
func automationStatusVerdict(r *automationStatusReport) error {
	if r == nil || r.Remote == nil {
		return nil
	}
	if reason := r.Remote.NotCheckedReason; reason != "" {
		if reason == reasonNoAutomationsYet {
			return nil
		}
		return fmt.Errorf("the remote state was not checked, so nothing here rules drift out: %s", reason)
	}
	if r.Remote.Problem != "" {
		return fmt.Errorf("%s", r.Remote.Problem)
	}

	var drifted, unchecked []string
	for _, a := range r.Remote.Automations {
		switch {
		case a.Orphan:
			// Answered by the orphan clause below, which says what a push will
			// actually do about it.
		case a.Drift == driftChanged:
			drifted = append(drifted, a.Path)
		// driftNoBaseline is deliberately absent: it is "not compared yet", not
		// "could not be compared".
		case a.Problem != "", a.Drift == "", a.Drift == driftUnreadable:
			unchecked = append(unchecked, a.Path)
		}
	}
	switch {
	case len(drifted) > 0 && len(unchecked) > 0:
		return fmt.Errorf("%d %s drifted on the server (%s), and %d %s could not be checked (%s) — see above",
			len(drifted), plural(len(drifted), "automation"), strings.Join(drifted, ", "),
			len(unchecked), plural(len(unchecked), "automation"), strings.Join(unchecked, ", "))
	case len(drifted) > 0:
		return fmt.Errorf("%d %s changed on the server since your last push: %s — push --force overwrites, or pull the change back into the file",
			len(drifted), plural(len(drifted), "automation"), strings.Join(drifted, ", "))
	case len(unchecked) > 0:
		return fmt.Errorf("%d %s could not be checked for drift (%s) — see the reason above; this is not the same as no drift",
			len(unchecked), plural(len(unchecked), "automation"), strings.Join(unchecked, ", "))
	case len(r.Orphans) > 0:
		return fmt.Errorf("%d %s bound to an automation and %s gone from this folder (%s) — a push refuses until they are back, or `--prune` deletes the automations",
			len(r.Orphans), plural(len(r.Orphans), "file"), agree(len(r.Orphans), "is", "are"),
			strings.Join(r.Orphans, ", "))
	}
	return nil
}
