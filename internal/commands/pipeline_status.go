package commands

import (
	"context"
	"fmt"
	"os"
	"sort"
	"strings"
	"sync"

	"github.com/ronjatech/ronja-cli/internal/api"
	"github.com/ronjatech/ronja-cli/internal/config"
	"github.com/ronjatech/ronja-cli/internal/wfdir"
	"github.com/spf13/cobra"
)

// `ronja pipeline status` answers the three questions you have before pushing:
// what have I changed, what state are the tables in over there, and has anything
// moved underneath me since I last synced.
//
// It mutates nothing — not even a checkout — so it is safe to run at any point,
// including against a feature you only have read access to.
func newPipelineStatusCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "status",
		Short: "Show local changes, table health and drift",
		Long: `Show local changes, table health and drift.

Four things, for the instance this folder is bound to:

  LOCAL    .sql files added, modified or deleted since the last sync
  NEW      files that are not bound to a table yet — a push would create them
  HEALTH   each bound table's build state, and whether it has a failure recorded
  DRIFT    tables whose SQL changed on the server since the last sync — chat and
           the web builder edit the same rows, so this is a real collision

When you have a draft open, drift is reported for BOTH rows: your draft (the one
a push writes to) and the live table underneath it, which a colleague can commit
to while your draft sits there.

Read-only: no draft is created and nothing is written. Run it from anywhere
inside the folder.

With --json, one object carrying all of the above.

The exit code answers one question — has anything moved on the server? — which
makes this a CI gate that needs no parsing. Non-zero for drift, and equally for a
table whose state could not be READ: an unreachable instance, an expired token, a
binding that no longer resolves. "I could not look" is not the same answer as
"nothing moved", and a gate that conflated them would go green on a revoked
credential.

Zero for everything local, and for a folder with nothing to compare against yet:
a file you have only changed on disk, a file with no table behind it, and a fresh
clone whose .ronja/ baseline was correctly never committed. Run a push to record
one.`,
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			// Deliberately NOT resolveInstance: a signed-out caller can still be
			// told what changed locally, which is most of the value and costs
			// nothing. The remote half degrades to "not checked" with a reason.
			resolved, err := config.Resolve(flagURL, flagProfile)
			if err != nil {
				return err
			}
			f, err := openFolder(cmd.Context(), resolved, wfdir.PipelineKind)
			if err != nil {
				return err
			}

			report := &pipelineStatusReport{
				Root:      f.Root,
				URL:       resolved.URL,
				Title:     f.Manifest.Title,
				Bound:     f.Bound,
				Ambiguous: f.BindingErr != nil,
				FeatureID: f.Binding.FeatureID,
				Tables:    len(f.Binding.Tables),
			}

			enumeration, err := enumerateFolder(f.Root, wfdir.PipelineKind)
			if err != nil {
				return err
			}
			baseline := f.State.For(f.Key)
			// In memory only — status writes nothing. It exists so that removing a
			// file's entry from ronja.json makes the phantom disappear from THIS
			// report rather than only from the one after the next push.
			prunePhantoms(baseline, f.Binding, enumeration.Files)
			report.Local = wfdir.DiffHashes(enumeration.Files, baseline.Hashes())
			report.Skipped = enumeration.Skipped
			report.WillCreate = []string{}
			for path := range enumeration.Files {
				if f.Binding.Tables[path] == "" {
					report.WillCreate = append(report.WillCreate, path)
				}
			}
			sort.Strings(report.WillCreate)

			report.Remote = pipelineRemoteStatus(cmd.Context(), resolved, f)

			// The report is emitted FIRST and the verdict decides only the exit
			// code: a caller reading --json gets its payload whatever the answer,
			// exactly as it does from a push that failed.
			if flagJSON {
				if err := emitJSON(report); err != nil {
					return err
				}
			} else {
				printPipelineStatus(report)
			}
			return pipelineStatusVerdict(report)
		},
	}
	return cmd
}

// pipelineStatusReport is the --json shape, and the same struct the human
// renderer reads — so the two can never describe different things.
type pipelineStatusReport struct {
	Root  string `json:"root"`
	URL   string `json:"url"`
	Title string `json:"title"`
	Bound bool   `json:"bound"`
	// Ambiguous reports a folder that IS bound here, to more than one
	// organization, with no way to tell which applies — the signed-out case.
	// Distinct from Bound so a caller does not read "not bound" and conclude a
	// push would create everything again.
	Ambiguous bool   `json:"ambiguous,omitempty"`
	FeatureID string `json:"featureID,omitempty"`
	// Tables is how many files this folder is BOUND to, which is not the same as
	// how many .sql files it holds — see WillCreate.
	Tables  int             `json:"tables"`
	Local   wfdir.Diff      `json:"local"`
	Skipped []wfdir.Skipped `json:"skipped,omitempty"`
	// WillCreate lists local .sql files with no table behind them yet.
	WillCreate []string              `json:"willCreate"`
	Remote     *pipelineRemoteReport `json:"remote"`
}

// pipelineRemoteReport is shaped so "we did not look" and "we looked and it is
// fine" are different states, the same way the workflow loop's is.
type pipelineRemoteReport struct {
	Checked bool `json:"checked"`
	// NotCheckedReason is why the remote half was not looked at. Deliberately
	// NOT called "skipped": pipelineStatusReport.Skipped already means "a local
	// file that will never be synced", and one JSON document using one word for
	// two unrelated things is a trap for anything parsing it.
	NotCheckedReason string `json:"notCheckedReason,omitempty"`
	// Problem is a folder-level failure — the list read itself did not work.
	Problem string                `json:"problem,omitempty"`
	Tables  []pipelineTableReport `json:"tables,omitempty"`
	Notes   []string              `json:"notes,omitempty"`
}

func (r *pipelineRemoteReport) note(format string, args ...any) {
	r.Notes = append(r.Notes, fmt.Sprintf(format, args...))
}

// Per-table drift verdicts. A vocabulary rather than a bool, because "we could
// not tell" is a third answer and reporting it as "no drift" is the one mistake
// that gets someone's work overwritten.
const (
	driftNone       = "none"
	driftChanged    = "changed"
	driftNoBaseline = "no_baseline"
	driftUnreadable = "unreadable"
)

// reasonNothingBound is the one NotCheckedReason that is not a failure to look.
// The folder is bound here, it names a feature, and that feature simply holds
// nothing this folder has pushed yet — a conclusion, where every other reason in
// that field is the remote half being unreachable, unreadable or ambiguous.
//
// A const rather than a literal because the exit code turns on the difference,
// and a gate that read one of these as the other would go green on an instance
// it never reached.
const reasonNothingBound = "no tables exist on this instance yet — the first push will create them"

// pipelineTableReport is one bound file's remote state.
type pipelineTableReport struct {
	Path    string `json:"path"`
	TableID string `json:"tableID"`
	Name    string `json:"name,omitempty"`
	// Status is the LIST's build status, and HasError its cheap "something is
	// recorded against this row" flag. Both come free with the one list call.
	Status   string `json:"status,omitempty"`
	HasError bool   `json:"hasError,omitempty"`
	// Problem is set when this binding cannot be used as it stands — the id is
	// not in the feature any more, or its SQL could not be read.
	Problem string `json:"problem,omitempty"`
	// Drift is one of the drift* constants above, for the row a push would WRITE
	// TO: your draft when you have one, else the live table.
	Drift string `json:"drift,omitempty"`
	// ComparedAgainst names the row Drift was measured against.
	ComparedAgainst string `json:"comparedAgainst,omitempty"`
	// LiveDrift is the OTHER leg — the live table against the fingerprint taken
	// from it — and is present only when a draft exists, because that is the only
	// time it says something Drift does not.
	//
	// It exists because push refuses on either leg and then says "run status to
	// see the detail": a status that could only ever report the draft answered a
	// question the caller had not asked, and the live leg (a colleague committed
	// while your draft sat open) was invisible on the one surface built to show
	// it.
	LiveDrift string               `json:"liveDrift,omitempty"`
	Draft     *pipelineDraftReport `json:"draft,omitempty"`
	// URL is the table's frontend page as the SERVER stamped it, rendered by the
	// human report only — see pushResult.URL for why links stay out of --json.
	URL string `json:"-"`
}

type pipelineDraftReport struct {
	ID string `json:"id"`
	// Verdict is the draft's own build verdict, which is the single most useful
	// thing here: a draft that failed to build cannot be published, and nothing
	// else in this report would say so.
	Verdict   string `json:"verdict,omitempty"`
	UpdatedAt string `json:"updatedAt,omitempty"`
}

// pipelineStatusConcurrency bounds the per-table reads.
//
// A folder of thirty tables is an ordinary size, and issuing sixty requests one
// after another makes `status` take long enough that people stop running it.
// Bounded rather than unbounded because the other failure mode is a folder of
// two hundred tables opening two hundred connections at once.
const pipelineStatusConcurrency = 8

// pipelineRemoteStatus gathers everything server-side in TWO TIERS, degrading
// rather than aborting.
//
// Tier one is a SINGLE list of the feature's tables, which carries every bound
// row's kind, status, shadow markers and hasError flag. That is the whole health
// leg, for any number of tables, at the cost of one request — and it is what a
// naive implementation would have paid a GET per table for.
//
// Tier two is the code, which the list deliberately omits, so drift costs a read
// per table: the caller's own draft, then the live row, and — when a draft is
// open — that draft by its own id, which is the only way to get its build
// verdict. Two requests per bound table, three where there is a draft to judge,
// run concurrently.
//
// Drift is remote-vs-BASELINE, never remote-vs-local: the question is whether
// anything moved underneath us, and a file the author edited locally is exactly
// what a push is for.
func pipelineRemoteStatus(ctx context.Context, resolved *config.Resolved, f *folder) *pipelineRemoteReport {
	out := &pipelineRemoteReport{}
	// Bound to several organizations here, and signed out, so which binding
	// applies is genuinely unknown. Reported rather than guessed: picking one
	// would show another organization's tables as if they were this folder's.
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
		out.NotCheckedReason = fmt.Sprintf("this folder names no feature — add \"featureID\" to the %s entry in %s",
			resolved.URL, wfdir.ManifestName)
		return out
	}
	if len(f.Binding.Tables) == 0 {
		out.NotCheckedReason = reasonNothingBound
		return out
	}

	client := newClient(resolved.URL, resolved.Token)
	items, err := client.ListFeatureTables(ctx, f.Binding.FeatureID)
	if err != nil {
		if api.StatusOf(err) == 404 {
			out.Checked = true
			out.Problem = fmt.Sprintf("binding broken: feature %s no longer exists on %s (or you lost access to it)",
				f.Binding.FeatureID, resolved.URL)
			return out
		}
		out.NotCheckedReason = fmt.Sprintf("could not list the tables of %s: %v", f.Binding.FeatureID, err)
		return out
	}
	out.Checked = true

	byID := make(map[string]*api.TableListItem, len(items))
	for _, item := range items {
		if item != nil {
			byID[item.ID] = item
		}
	}

	paths := make([]string, 0, len(f.Binding.Tables))
	for path := range f.Binding.Tables {
		paths = append(paths, path)
	}
	sort.Strings(paths)

	baseline := f.State.For(f.Key)
	known := baseline.Hashes()
	reports := make([]pipelineTableReport, len(paths))
	var needCode []int

	for i, path := range paths {
		id := f.Binding.Tables[path]
		report := pipelineTableReport{Path: path, TableID: id}
		item, present := byID[id]
		if !present {
			// A bound id the feature no longer holds. Reported per table rather
			// than aborting the whole block: the other twenty-nine bindings are
			// still worth answering, and this one is the answer.
			report.Problem = "binding broken: this table is not in the feature any more (deleted, moved, or you lost access to it)"
			reports[i] = report
			continue
		}
		report.Name = item.Name
		report.Status = item.Status
		report.HasError = item.HasError
		needCode = append(needCode, i)
		reports[i] = report
	}

	// Tier two, concurrently. Each worker writes only its own slot, so no lock is
	// needed for the results themselves.
	work := make(chan int)
	var wg sync.WaitGroup
	workers := pipelineStatusConcurrency
	if len(needCode) < workers {
		workers = len(needCode)
	}
	for w := 0; w < workers; w++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for i := range work {
				fillTableDrift(ctx, client, &reports[i], baseline.TableStateFor(reports[i].Path))
			}
		}()
	}
	for _, i := range needCode {
		work <- i
	}
	close(work)
	wg.Wait()

	out.Tables = reports
	if baseline == nil {
		out.note("no local baseline — every table reads as unknown (this folder was never synced from here)")
	}
	for _, path := range paths {
		// `known` is the BASELINE's hashes, not the folder's contents: a missing
		// entry means this file was never synced from here, which is why its drift
		// cannot be judged. Nothing here says anything about what is on disk.
		if _, hasBaseline := known[path]; !hasBaseline && baseline != nil {
			out.note("%s is bound to a table but has no baseline — its drift cannot be judged until the next push", path)
		}
	}
	return out
}

// fillTableDrift reads one bound table's SQL and compares it against the
// baseline fingerprint TAKEN FROM THAT ROW.
//
// BOTH legs of push's drift guard, not one, and that is the point of the extra
// read: push refuses when EITHER the live table or your own draft has moved, and
// then tells the caller to run this command for the detail. A status that
// resolved the draft and reported only that could not show the live leg at all —
// a colleague committing while your draft sat open — so the sentence push prints
// was false exactly when it mattered.
//
// Which fingerprint follows from which row, and the pairing is the whole
// invariant (see wfdir.TableState): the live table against LiveSHA256, the draft
// against DraftSHA256 — and only when the recorded draft id IS the open one,
// since a fingerprint from a draft that has since been committed describes a row
// that no longer exists. A draft with no usable fingerprint falls back to the
// live SQL: a draft nobody has edited is byte-identical to the parent it forked
// from, so that comparison still catches somebody else's edit sitting in your
// own draft.
//
// An empty fingerprint is reported as its own answer (driftNoBaseline) rather
// than as drift.
func fillTableDrift(ctx context.Context, client *api.Client, report *pipelineTableReport, state wfdir.TableState) {
	draft, err := client.GetTableDraft(ctx, report.TableID)
	if err != nil {
		report.Drift = driftUnreadable
		report.Problem = fmt.Sprintf("could not check for your open draft: %v", err)
		return
	}

	// The LIVE row is read whether or not a draft exists: it is leg (a), and it
	// is also the fallback the draft leg compares against.
	live, err := client.GetTable(ctx, report.TableID)
	if err != nil {
		report.Drift = driftUnreadable
		report.Problem = fmt.Sprintf("could not read %s: %v", report.TableID, err)
		return
	}
	liveCode, liveUnresolved := live.CanonicalCode()
	liveDrift, liveProblem := compareDrift(liveCode, liveUnresolved, state.LiveSHA256)

	if draft == nil {
		report.URL = live.URL
		report.ComparedAgainst = live.ID
		report.Drift = liveDrift
		report.Problem = liveProblem
		return
	}

	// The draft is read by its OWN id rather than taken from the /draft route,
	// which projects a plain table view with no build verdict on it.
	staged, err := client.GetTable(ctx, draft.ID)
	if err != nil {
		report.LiveDrift = liveDrift
		report.Drift = driftUnreadable
		report.Problem = fmt.Sprintf("could not read your draft %s: %v", draft.ID, err)
		return
	}
	report.Draft = &pipelineDraftReport{
		ID:        draft.ID,
		Verdict:   staged.BuildVerdict,
		UpdatedAt: formatTime(&draft.UpdatedAt),
	}
	report.ComparedAgainst = staged.ID
	report.LiveDrift = liveDrift

	against := state.DraftSHA256
	if state.DraftID != draft.ID {
		against = ""
	}
	if against == "" && !liveUnresolved {
		against = wfdir.HashString(liveCode)
	}
	draftCode, draftUnresolved := staged.CanonicalCode()
	report.Drift, report.Problem = compareDrift(draftCode, draftUnresolved, against)
	if report.Problem == "" && liveProblem != "" {
		report.Problem = liveProblem
	}
}

// compareDrift is one row's verdict: its canonical SQL against the fingerprint
// taken from that same row.
//
// A row that cannot be canonicalized is driftUnreadable with a reason, never
// agreement — the stored SQL and the stored input list disagree with each other,
// and answering "no drift" there is the silent case the guard exists for.
func compareDrift(code string, unresolved bool, baselineHash string) (string, string) {
	switch {
	case unresolved:
		return driftUnreadable, "this table's stored SQL carries a positional ref that cannot be resolved, so its content cannot be compared — open it in the web builder and replace the ref with the table's id"
	case baselineHash == "":
		return driftNoBaseline, ""
	case wfdir.HashString(code) == baselineHash:
		return driftNone, ""
	default:
		return driftChanged, ""
	}
}

func printPipelineStatus(r *pipelineStatusReport) {
	out := os.Stdout
	fmt.Fprintf(out, "  %s\n", r.Title)
	fmt.Fprintf(out, "  %s\n\n", r.Root)

	fmt.Fprintf(out, "  Instance:   %s\n", r.URL)
	switch {
	case r.Bound && r.FeatureID != "":
		fmt.Fprintf(out, "  Feature:    %s\n", r.FeatureID)
	case r.Ambiguous:
		// "not bound" would flatly contradict the Remote line below, which is
		// about to say the folder is bound to several organizations here.
		fmt.Fprintf(out, "  Feature:    bound here, but to several organizations — pick one with --profile\n")
	case r.Bound:
		fmt.Fprintf(out, "  Feature:    none named — a push refuses until one is\n")
	default:
		fmt.Fprintf(out, "  Feature:    not bound to this instance\n")
	}
	fmt.Fprintf(out, "  Tables:     %d bound\n", r.Tables)

	fmt.Fprintf(out, "\n  Local changes\n")
	if !r.Local.Dirty() {
		fmt.Fprintf(out, "    clean (%d file(s) match the last sync)\n", r.Local.Unchanged)
	} else {
		printPathList(out, "new", r.Local.Added)
		printPathList(out, "modified", r.Local.Modified)
		printPathList(out, "deleted", r.Local.Deleted)
		fmt.Fprintf(out, "    %d unchanged\n", r.Local.Unchanged)
	}
	for _, path := range r.WillCreate {
		fmt.Fprintf(out, "    %-9s %s\n", "creates", path)
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
		for _, t := range r.Remote.Tables {
			printPipelineTableStatus(out, t)
		}
	}
	for _, note := range r.Remote.Notes {
		fmt.Fprintf(out, "    note: %s\n", note)
	}
}

func printPipelineTableStatus(out *os.File, t pipelineTableReport) {
	fmt.Fprintf(out, "    %s\n", t.Path)
	if t.Problem != "" {
		fmt.Fprintf(out, "      %s\n", t.Problem)
		return
	}
	fmt.Fprintf(out, "      table:  %s (%s)\n", t.TableID, t.Status)
	if t.HasError {
		// The list's flag says only that SOMETHING is recorded; it never says
		// what, and never whether the table is nonetheless serving good data.
		fmt.Fprintf(out, "      health: a build failure is recorded — `ronja api /api/v2/feature/model/%s --jq .lastBuildError` for the reason\n", t.TableID)
	}
	if t.Draft != nil {
		fmt.Fprintf(out, "      draft:  %s (%s)\n", t.Draft.ID, describeVerdict(t.Draft.Verdict))
	}
	switch t.Drift {
	case driftChanged:
		fmt.Fprintf(out, "      drift:  the SQL of %s changed since your last sync — a push would overwrite it\n", t.ComparedAgainst)
	case driftNoBaseline:
		fmt.Fprintf(out, "      drift:  nothing to compare against yet — the next push records a baseline\n")
	case driftUnreadable:
		fmt.Fprintf(out, "      drift:  not checked\n")
	}
	// The live leg, printed only when it is a second answer — i.e. when a draft
	// exists. A colleague's commit landing under an open draft is the case push
	// refuses on and this line is the only place it is visible.
	switch t.LiveDrift {
	case driftChanged:
		fmt.Fprintf(out, "      live:   the SQL of %s changed since your last sync — committing your draft would revert it\n", t.TableID)
	case driftNoBaseline:
		fmt.Fprintf(out, "      live:   nothing to compare against yet — the next push records a baseline\n")
	case driftUnreadable:
		fmt.Fprintf(out, "      live:   not checked\n")
	}
	printResourceURL(out, statusKeyWidth, t.URL)
}

// pipelineStatusVerdict turns the report into an exit code, answering ONE
// question: has anything moved on the server under this folder?
//
// The contract is `ronja db migrate status`'s — a CI gate that needs no parsing
// — and the reason it treats "could not check" as a failure is the same reason
// that command does: a status that exits zero is read as "nothing has moved",
// and a revoked token, an unreachable instance or a binding that no longer
// resolves says nothing of the kind. Exiting zero there would make the gate go
// green precisely when it stopped working.
//
// "Nothing to compare against YET" is the opposite case and exits zero. A folder
// cloned from git has no .ronja/ — it is correctly never committed — so every
// file reads as no-baseline, and a folder freshly bound to a feature has nothing
// on the server at all. Both are the normal way somebody joins a pipeline, which
// is why push's drift guard disarms for exactly the same state; failing here
// would mean a first checkout could not pass a gate until it had pushed.
//
// A LOCAL edit is not the gate either, and never has been: drift is measured
// remote-against-baseline, and a file the author changed is what a push is for.
func pipelineStatusVerdict(r *pipelineStatusReport) error {
	if r == nil || r.Remote == nil {
		return nil
	}
	if reason := r.Remote.NotCheckedReason; reason != "" {
		if reason == reasonNothingBound {
			return nil
		}
		return fmt.Errorf("the remote state was not checked, so nothing here rules drift out: %s", reason)
	}
	if r.Remote.Problem != "" {
		return fmt.Errorf("%s", r.Remote.Problem)
	}

	var drifted, unchecked []string
	for _, t := range r.Remote.Tables {
		switch {
		case t.Drift == driftChanged || t.LiveDrift == driftChanged:
			drifted = append(drifted, t.Path)
		// driftNoBaseline is deliberately absent: it is "not compared yet", not
		// "could not be compared", and the two rows above are what tell them
		// apart — an unreadable row is a row that was read and made no sense.
		case t.Problem != "", t.Drift == "", t.Drift == driftUnreadable,
			t.LiveDrift == driftUnreadable:
			unchecked = append(unchecked, t.Path)
		}
	}
	switch {
	case len(drifted) > 0 && len(unchecked) > 0:
		return fmt.Errorf("%d %s drifted on the server (%s), and %d %s could not be checked (%s) — see above",
			len(drifted), plural(len(drifted), "table"), strings.Join(drifted, ", "),
			len(unchecked), plural(len(unchecked), "table"), strings.Join(unchecked, ", "))
	case len(drifted) > 0:
		return fmt.Errorf("%d %s changed on the server since your last sync: %s — push --force overwrites, or pull the change back into your file",
			len(drifted), plural(len(drifted), "table"), strings.Join(drifted, ", "))
	case len(unchecked) > 0:
		return fmt.Errorf("%d %s could not be checked for drift (%s) — see the reason above; this is not the same as no drift",
			len(unchecked), plural(len(unchecked), "table"), strings.Join(unchecked, ", "))
	}
	return nil
}
