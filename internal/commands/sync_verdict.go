package commands

import (
	"fmt"
	"sort"
	"strings"

	"github.com/ronjatech/ronja-cli/internal/wfdir"
)

// The verdict vocabulary a tree command aggregates over.
//
// Three values, not a bool, for the reason pipeline_status.go's drift constants
// give: "we could not tell" is a third answer, and reporting it as "no drift" is
// the one mistake that gets someone's work overwritten. The tree contract — the
// syncExit* constants in sync.go, applied by each report's exitError — turns
// each of these into its own exit code, so a CI job can act
// on "something drifted" (push, or pull the change back) differently from "I
// could not verify this" (fix the credential, the binding, or the folder).
const (
	verdictClean   = "clean"
	verdictDrifted = "drifted"
	verdictUnknown = "unknown"
	// verdictBroken is `sync check`'s middle answer, where `sync status`'s is
	// verdictDrifted. It scores the same exit code and sits at the same rank —
	// only the WORD differs, because "drifted" is a claim about content moving
	// on the server and an unresolvable reference is not that. A gate that
	// printed "drifted" at somebody whose alias lost its bind would send them
	// looking at a diff that does not exist.
	verdictBroken = "broken"
	// verdictApplied and verdictRefused are `sync apply`'s own two words, where
	// `sync status` has clean/drifted and `sync check` has clean/broken. Here
	// rather than beside the command for the same reason verdictBroken is: this
	// block is the tree-command vocabulary, and verdictRank below reads all of
	// them — a constant defined in the command file made the ranking depend on
	// another file's const block, which is the drift this one exists to prevent.
	verdictApplied = "applied"
	verdictRefused = "refused"
)

// folderVerdict is one folder's answer plus the sentence that explains it.
//
// Detail is prose for a human and is deliberately not parsed by anything: the
// contract is Verdict, which is one of the three words above.
type folderVerdict struct {
	Verdict string
	Detail  string
}

func cleanVerdict() folderVerdict { return folderVerdict{Verdict: verdictClean} }

func driftedVerdict(format string, args ...any) folderVerdict {
	return folderVerdict{Verdict: verdictDrifted, Detail: fmt.Sprintf(format, args...)}
}

func unknownVerdict(format string, args ...any) folderVerdict {
	return folderVerdict{Verdict: verdictUnknown, Detail: fmt.Sprintf(format, args...)}
}

// ⚠️ Used ONLY by `ronja sync`, and deliberately not wired into the three
// per-folder status commands.
//
// `ronja pipeline status` keeps pipelineStatusVerdict, and `ronja wf status` /
// `ronja app status` keep their unconditional `return nil` — both of which
// exit ZERO for a folder that is bound and has nothing to compare against,
// where this helper says `unknown` (see the no-baseline clause below). Applying
// this to them would change the exit code of three shipped commands, which is a
// scripting-visible break gated on a decision nobody has taken yet. Computing
// the verdict and CHANGING the exit code are two separate steps, and only the
// first has been approved.

// pushDelta is everything a folder's report says a PUSH WOULD CHANGE, split by
// which side of the wire the change is on.
//
// ⚠️ The split is the fix for a false green that ran the whole width of this
// command. The tree verdict used to read exactly ONE leg — the server's copy
// against the local baseline — so a folder whose files had been EDITED and never
// pushed came back `clean`, exit 0. The report already computed that (`Local`);
// nothing read it. Every per-folder `status` is right to leave it out of its own
// drift verdict — "a file the author edited locally is exactly what a push is
// for" (pipeline_status.go) — but that is the semantic of an interactive
// one-folder command. A repository gate asks a different question, "would a push
// from this tree change the organization", and a local edit is a yes.
//
// The three lists stay APART because they are different facts and a reader acts
// differently on each: Local is "you hold work that was never deployed" (push
// it), Remote is "the organization moved under you" (look before you push), Meta
// is "your declarations no longer match the row" — which includes the case that
// is not even pushable, a runtime-1 folder against a runtime-2 row.
type pushDelta struct {
	// Local is what this folder holds that the organization does not.
	Local []string
	// Remote is what moved on the server since the last sync.
	Remote []string
	// Meta is the declarations a push would change: parameters, the reporting
	// timezone, the runtime generation, a data app's access grants.
	Meta []string
	// Unchecked is everything that could not be compared at all, and it wins —
	// see verdict.
	Unchecked []string
}

// verdict folds one folder's delta into its answer.
//
// UNKNOWN DOMINATES, for the reason it dominates everywhere else here: the
// strongest TRUE statement about a folder where one thing changed and another
// could not be read is "I could not verify everything". Reporting it as a change
// claims the unverifiable half was fine, and the reading a CI operator takes
// from that is "push" — which overwrites the half nobody read.
func (d pushDelta) verdict() folderVerdict {
	if len(d.Unchecked) > 0 {
		sort.Strings(d.Unchecked)
		detail := fmt.Sprintf("%d %s could not be checked (%s)",
			len(d.Unchecked), plural(len(d.Unchecked), "thing"), strings.Join(d.Unchecked, ", "))
		if changed := d.changes(); changed != "" {
			return unknownVerdict("%s, and %s", detail, changed)
		}
		return unknownVerdict("%s — this is not the same answer as nothing having changed", detail)
	}
	if changed := d.changes(); changed != "" {
		return driftedVerdict("%s", changed)
	}
	return cleanVerdict()
}

// changes renders the three change lists as one sentence, or "" when there are
// none. Each half names itself, because "3 things differ" sends a reader to
// diff the wrong side.
func (d pushDelta) changes() string {
	var parts []string
	if len(d.Local) > 0 {
		sort.Strings(d.Local)
		parts = append(parts, fmt.Sprintf("%d %s here %s never been deployed (%s)",
			len(d.Local), plural(len(d.Local), "change"), agree(len(d.Local), "has", "have"),
			strings.Join(d.Local, ", ")))
	}
	if len(d.Remote) > 0 {
		sort.Strings(d.Remote)
		parts = append(parts, fmt.Sprintf("%d %s changed on the server since the last sync (%s)",
			len(d.Remote), plural(len(d.Remote), "thing"), strings.Join(d.Remote, ", ")))
	}
	if len(d.Meta) > 0 {
		sort.Strings(d.Meta)
		parts = append(parts, fmt.Sprintf("a push would change %s", strings.Join(d.Meta, ", ")))
	}
	return strings.Join(parts, "; ")
}

// localChanges is the half of a wfdir.Diff that is SAFE to read without knowing
// whether a local baseline exists — which is the trap this whole leg has to step
// around.
//
// ⚠️ `Added` is EXCLUDED, and that is not conservatism for its own sake.
// DiffHashes against an ABSENT baseline reports every local file as added (see
// its loop: a missing baseline entry is `Added`, and Modified/Deleted can only
// arise from an entry that exists). A folder freshly cloned from git has no
// .ronja/ at all, so reading `Added` there would report a perfectly in-step
// repository as holding undeployed work — a confident wrong answer, in CI, on
// the run that matters most. Modified and Deleted are structurally impossible
// without a baseline, so they need no gate.
//
// The callers that CAN see a baseline add `Added` back themselves; the pipeline
// one uses WillCreate instead, which answers the same question without one.
func localChanges(d wfdir.Diff) []string {
	out := make([]string, 0, len(d.Modified)+len(d.Deleted))
	out = append(out, d.Modified...)
	out = append(out, d.Deleted...)
	return out
}

// verdictOfPipelineStatus is folderVerdict for a pipeline folder.
//
// It follows pipelineStatusVerdict's discipline — anything not provably clean
// is non-green, and a NotCheckedReason that is not reasonNothingBound means the
// remote half was never reached — with the corrections marked below.
func verdictOfPipelineStatus(r *pipelineStatusReport, localFiles int, undeployed []string) folderVerdict {
	if r == nil || r.Remote == nil {
		return unknownVerdict("no remote state was computed for this folder")
	}
	if reason := r.Remote.NotCheckedReason; reason != "" {
		if reason == reasonNothingBound {
			return neverDeployedVerdict(localFiles, "its tables")
		}
		return unknownVerdict("the remote state was not checked, so nothing here rules drift out: %s", reason)
	}
	if r.Remote.Problem != "" {
		return unknownVerdict("%s", r.Remote.Problem)
	}

	var delta pushDelta
	for _, t := range r.Remote.Tables {
		// ⚠️ The two legs are read INDEPENDENTLY, and a table can land in both
		// lists. This used to be one switch whose first arm was `changed`, so a
		// table whose draft had drifted and whose LIVE row could not be read
		// scored as drift — a silent exception to "unknown dominates", inside the
		// one function that exists to enforce it.
		switch {
		case t.Problem != "", t.Drift == "", t.Drift == driftUnreadable, t.LiveDrift == driftUnreadable:
			delta.Unchecked = append(delta.Unchecked, t.Path)
		// ⚠️ THE FRESH-CLONE CORRECTION. pipelineStatusVerdict treats
		// driftNoBaseline as green — "not compared yet" rather than "could not be
		// compared" — and for a single folder run by its author that is right: a
		// first checkout must be able to pass its own gate. It is wrong for a tree
		// gate, and wrong in exactly the case this command exists for.
		case t.Drift == driftNoBaseline, t.LiveDrift == driftNoBaseline:
			delta.Unchecked = append(delta.Unchecked, t.Path)
		}
		if t.Drift == driftChanged || t.LiveDrift == driftChanged || t.DocsDrift == driftChanged {
			delta.Remote = append(delta.Remote, t.Path)
		}
	}

	// The DOCS SIDECARS — the tables this folder documents but does not build.
	// They belong in this gate for the reason the whole command exists: `sync
	// status` asks whether a push from anywhere beneath a directory would change
	// the organization, and a sidecar the row does not agree with is exactly such
	// a change. It is a LOCAL one — the file says something the organization does
	// not yet hold — which is the same shape as an edited .sql file.
	//
	// The same two corrections as above: unknown dominates, and driftNoBaseline
	// is unknown here even though the single-folder command reads it as green.
	var docsPending []string
	for _, d := range r.Remote.Docs {
		switch {
		case d.Problem != "", d.Drift == "", d.Drift == driftUnreadable, d.Drift == driftNoBaseline:
			delta.Unchecked = append(delta.Unchecked, d.Path)
		case d.Drift == driftChanged:
			delta.Remote = append(delta.Remote, d.Path)
		}
		if d.Pending {
			docsPending = append(docsPending, d.Path)
		}
	}

	// The LOCAL half, which nothing read before this. Three sources, deduped,
	// because on a machine that HAS a baseline the same file legitimately shows
	// up in two of them:
	//
	//   - localChanges: edited or deleted since the last sync here.
	//   - WillCreate: a .sql file with no table behind it — a push would create
	//     one, and unlike Diff.Added it says so without needing a baseline.
	//   - undeployed: the committed SQL differs from what the COMMITTED lock says
	//     was last deployed. The only one of the three that survives a fresh
	//     clone, and the reason the lock carries a live fingerprint at all.
	//
	// ⚠️ `undeployed` also fires for a file pushed to the author's own DRAFT and
	// not yet published, on a machine where `Local` is clean because the push
	// updated the baseline. That is not a false positive: the live table does not
	// hold it, and this command's question is whether the ORGANIZATION holds what
	// the folder committed. Gating it on the absence of a baseline would make the
	// answer depend on whose machine it ran on, which is the exact property the
	// lock file exists to remove.
	delta.Local = dedupe(localChanges(r.Local), r.WillCreate, undeployed, docsPending)
	return delta.verdict()
}

// verdictOfAutomationStatus is folderVerdict for an automation folder.
//
// It follows automationStatusVerdict's discipline with the same two corrections
// the pipeline one carries: a folder that has never been deployed is NOT clean
// when it holds files, and "not compared yet" is unknown for a tree gate even
// though it is green for the author running the single-folder command.
func verdictOfAutomationStatus(r *automationStatusReport, localFiles int) folderVerdict {
	if r == nil || r.Remote == nil {
		return unknownVerdict("no remote state was computed for this folder")
	}
	if reason := r.Remote.NotCheckedReason; reason != "" {
		if reason == reasonNoAutomationsYet {
			return neverDeployedVerdict(localFiles, "its automations")
		}
		return unknownVerdict("the remote state was not checked, so nothing here rules drift out: %s", reason)
	}
	if r.Remote.Problem != "" {
		return unknownVerdict("%s", r.Remote.Problem)
	}

	var delta pushDelta
	for _, a := range r.Remote.Automations {
		if a.Orphan {
			// Owned by the orphan clause at the bottom, which is the only place
			// that says what a push will do about it.
			continue
		}
		switch {
		case a.Problem != "", a.Drift == "", a.Drift == driftUnreadable:
			delta.Unchecked = append(delta.Unchecked, a.Path)
		// ⚠️ THE FRESH-CLONE CORRECTION, in the one place it differs from the
		// pipeline's. There is no local baseline here, so driftNoBaseline means
		// only one thing: this folder is bound to the row and has never recorded
		// WHEN it agreed with it — which is precisely the state in which a push
		// would overwrite whatever moved without knowing what it was.
		case a.Drift == driftNoBaseline:
			delta.Unchecked = append(delta.Unchecked, a.Path)
		}
		if a.Drift == driftChanged {
			delta.Remote = append(delta.Remote, a.Path)
		}
		if len(a.Changes) > 0 {
			delta.Local = append(delta.Local, a.Path)
		}
	}
	// A file with no automation behind it is undeployed work, and it needs no
	// baseline to say so — the pipeline arm's WillCreate, by the same argument.
	delta.Local = dedupe(delta.Local, r.WillCreate)
	// A file that will not parse is a file nothing can be said about.
	for _, problem := range r.Problems {
		delta.Unchecked = append(delta.Unchecked, problem.Path)
	}

	verdict := delta.verdict()
	if len(r.Orphans) == 0 {
		return verdict
	}
	// ⚠️ Orphans are folded in AFTERWARDS rather than into one of pushDelta's
	// lists, because none of the three says what an orphan is: it is neither
	// undeployed work here nor a change over there, and a push does not apply it
	// — a push REFUSES on it. Slotting it into `Local` would have the report say
	// "changes here have never been deployed" about a file that is gone.
	orphans := fmt.Sprintf("%d %s bound to an automation and %s gone from this folder (%s), which a push refuses on",
		len(r.Orphans), plural(len(r.Orphans), "file"), agree(len(r.Orphans), "is", "are"),
		strings.Join(r.Orphans, ", "))
	if verdict.Verdict == verdictClean {
		return driftedVerdict("%s", orphans)
	}
	verdict.Detail = orphans + "; " + verdict.Detail
	return verdict
}

// dedupe merges path lists, keeping each path once. The lists overlap by design
// — see the note at its caller — and a folder reporting "3 changes" for one
// edited file is a report nobody trusts twice.
func dedupe(lists ...[]string) []string {
	seen := map[string]bool{}
	var out []string
	for _, list := range lists {
		for _, path := range list {
			if seen[path] {
				continue
			}
			seen[path] = true
			out = append(out, path)
		}
	}
	return out
}

// verdictOfWorkflowStatus is folderVerdict for a workflow folder.
//
// `ronja wf status` has no verdict of its own (see the note above), so unlike
// the pipeline one this is not a corrected copy of anything — it is the same
// discipline applied to a report shape that never had one.
func verdictOfWorkflowStatus(r *statusReport, localFiles int) folderVerdict {
	if r == nil {
		return unknownVerdict("no status was computed for this folder")
	}
	return verdictOfFileDrift(fileDriftInput{
		Remote: r.Remote, Baseline: r.Baseline, NothingYet: reasonNoWorkflowYet,
		LocalFiles: localFiles, Creates: "the workflow", Local: r.Local,
		Meta: workflowMetaChanges(r.Remote),
	})
}

// verdictOfAppStatus is folderVerdict for a data-app folder.
func verdictOfAppStatus(r *appStatusReport, localFiles int) folderVerdict {
	if r == nil {
		return unknownVerdict("no status was computed for this folder")
	}
	if r.Remote == nil {
		return unknownVerdict("no remote state was computed for this folder")
	}
	// The two loops' remote reports are separate types carrying the same five
	// fields, so the shared half is fed by hand rather than by an interface: an
	// interface over two --json shapes would be a third contract to keep in step
	// with both, and these are five field reads.
	return verdictOfFileDrift(fileDriftInput{
		Remote: &remoteReport{
			Checked:          r.Remote.Checked,
			NotCheckedReason: r.Remote.NotCheckedReason,
			Problem:          r.Remote.Problem,
			Drift:            r.Remote.Drift,
		},
		Baseline: r.Baseline, NothingYet: reasonNoAppYet,
		LocalFiles: localFiles, Creates: "the data app", Local: r.Local,
		Meta: appMetaChanges(r.Remote),
	})
}

// workflowMetaChanges names the declarations a push would change on the row.
//
// Each of these fields is set by the status path ONLY when the folder manages
// that declaration AND it differs from the row's — so their presence already
// means "a push would change something", and this reads nothing else.
//
// ⚠️ They are BASELINE-INDEPENDENT: each compares the COMMITTED manifest against
// the SERVER's row, with .ronja/ nowhere in it, so unlike Diff.Added they are
// safe to read on a fresh clone. That is what makes the runtime one worth having
// most: a runtime-1 folder against a runtime-2 row is a push the server REFUSES,
// and reporting that repository as healthy is worse than reporting drift.
func workflowMetaChanges(r *remoteReport) []string {
	if r == nil {
		return nil
	}
	var out []string
	if r.Parameters != nil {
		out = append(out, "the declared parameters")
	}
	if r.ReportingTimezone != nil {
		out = append(out, "the reporting timezone")
	}
	if r.Runtime != nil {
		if r.Runtime.Refused {
			out = append(out, fmt.Sprintf("the runtime generation (this folder declares %d and the row is on %d, which a push REFUSES rather than lowers)",
				r.Runtime.Local, r.Runtime.Remote))
		} else {
			out = append(out, fmt.Sprintf("the runtime generation (%d to %d)", r.Runtime.Remote, r.Runtime.Local))
		}
	}
	return out
}

// appMetaChanges is workflowMetaChanges for a data app: the access grants, which
// are the app's PRIVILEGES and the one declaration where "the folder and the row
// disagree" is worth a gate all by itself.
func appMetaChanges(r *appRemoteReport) []string {
	if r == nil {
		return nil
	}
	var out []string
	for _, change := range r.AccessChanges {
		out = append(out, fmt.Sprintf("%s (%d added, %d removed)", change.Label, len(change.Added), len(change.Removed)))
	}
	return out
}

// fileDriftInput is what the shared workflow/data-app verdict reads. A struct
// rather than eight positional parameters, half of which are strings.
type fileDriftInput struct {
	Remote   *remoteReport
	Baseline *baselineReport
	// NothingYet is the loop's own "bound here, and the row does not exist yet"
	// reason — the one NotCheckedReason that is a conclusion rather than a
	// failure to look. Creates names what a first push would make.
	NothingYet string
	Creates    string
	LocalFiles int
	Local      wfdir.Diff
	Meta       []string
}

// verdictOfFileDrift is the shared body of the workflow and data-app verdicts:
// both compare a set of REMOTE file hashes against the local baseline, both
// report the answer as one wfdir.Diff, and both have a LOCAL diff beside it that
// the tree verdict has to read too (see pushDelta).
func verdictOfFileDrift(in fileDriftInput) folderVerdict {
	remote := in.Remote
	if remote == nil {
		return unknownVerdict("no remote state was computed for this folder")
	}
	if reason := remote.NotCheckedReason; reason != "" {
		if reason == in.NothingYet {
			return neverDeployedVerdict(in.LocalFiles, in.Creates)
		}
		return unknownVerdict("the remote state was not checked, so nothing here rules drift out: %s", reason)
	}
	if remote.Problem != "" {
		return unknownVerdict("%s", remote.Problem)
	}
	if !remote.Checked {
		return unknownVerdict("the remote state was not checked, so nothing here rules drift out")
	}
	// THE SAME CORRECTION as the pipeline one, and it bites harder on these two
	// loops: DiffHashes against an absent baseline reports every remote file as
	// ADDED, so a folder freshly cloned from git does not read clean here — it
	// reads as wholesale drift, which is a confident answer to a question that
	// was never asked. Checked before the diff so the honest answer wins, and
	// before the LOCAL leg so that leg never sees an absent baseline either.
	if in.Baseline == nil {
		return unknownVerdict("no local baseline for this stack, so nothing could be compared — the drift below is every remote file reading as new, not a real comparison")
	}
	// Reached when the remote row was read and its files were not: remoteStatus
	// records a note and leaves Drift nil rather than claiming no difference.
	if remote.Drift == nil {
		return unknownVerdict("the remote files could not be read, so nothing here rules drift out")
	}

	var delta pushDelta
	// ⚠️ The row a push would WRITE TO could not be established — the draft
	// lookup failed and everything below was measured against the LIVE row. A
	// clean comparison there is a true statement about a row nobody is going to
	// write to, so it is "could not tell", not "clean". Nothing read this before,
	// and a workflow with an unreadable draft reported a healthy folder.
	if remote.TargetUnknown {
		delta.Unchecked = append(delta.Unchecked, "your own open draft (it could not be read, so the comparison below is against the live row a push would not write to)")
	}
	delta.Remote = append(delta.Remote, remote.Drift.Added...)
	delta.Remote = append(delta.Remote, remote.Drift.Modified...)
	delta.Remote = append(delta.Remote, remote.Drift.Deleted...)
	// The baseline is known to exist by here, so Added is meaningful: these are
	// files this folder holds and the last sync did not.
	delta.Local = append(localChanges(in.Local), in.Local.Added...)
	delta.Meta = in.Meta
	return delta.verdict()
}

// neverDeployedVerdict answers the one NotCheckedReason per loop that is a
// CONCLUSION rather than a failure to look: the folder is bound here, it names a
// feature, and the organization simply holds nothing this folder has pushed yet
// (reasonNothingBound / reasonNoWorkflowYet / reasonNoAppYet).
//
// ⚠️ THAT REASON WAS READ AS CLEAN, AND IT IS A FALSE GREEN. A folder bound to
// this very organization, holding a table's whole SQL or a whole workflow, that
// has never been pushed, reported `clean` and exited ZERO — on all three kinds.
// A CI gate built on `ronja sync status` passed while nothing was deployed,
// which is precisely the failure the command exists to prevent.
//
// The reason is only honest when there is nothing to deploy. So the answer turns
// on what the folder's OWN loop would push — wfdir.Enumerate under the kind's
// rules, so a pipeline counts its .sql files and not a stray .md:
//
//	no syncable files  — clean. An empty folder genuinely has nothing to deploy,
//	                     which is what makes `wf init` + `sync status` sane.
//	one or more        — DRIFTED, exit 1. Not `unknown`: nothing is ambiguous.
//	                     We know the files are here and we know nothing is over
//	                     there.
//
// The wording deliberately does not reuse the drift sentence ("changed on the
// server since the last sync"), which claims a comparison that never happened.
func neverDeployedVerdict(localFiles int, creates string) folderVerdict {
	if localFiles == 0 {
		return cleanVerdict()
	}
	return driftedVerdict("this folder has never been deployed here — it holds %d syncable %s and nothing of it is on the server, so a first push would create %s",
		localFiles, plural(localFiles, "file"), creates)
}

// verdictRank is the ONE precedence the tree commands fold by, and there is
// deliberately only one: unknown beats the middle answer beats clean.
//
// The middle answer is spelled `drifted` by `sync status`, `broken` by
// `sync check` and `refused` by `sync apply` — three words for one rank, because
// the three commands report different things about a folder and no word is right
// for the others. They never appear in one report: a tree command runs one
// computation over every folder, so a list of verdicts is all one vocabulary.
//
// `applied` takes the green rank by falling through, exactly as `clean` does.
func verdictRank(verdict string) int {
	switch verdict {
	case verdictUnknown:
		return 2
	case verdictDrifted, verdictBroken, verdictRefused:
		return 1
	}
	return 0
}

// worstVerdict folds per-folder answers into the tree's, by the same precedence
// pushDelta.verdict and checkVerdictOf use WITHIN one folder: unknown, then the
// middle answer, then clean.
func worstVerdict(verdicts []string) string {
	worst := verdictClean
	for _, v := range verdicts {
		if verdictRank(v) > verdictRank(worst) {
			worst = v
		}
	}
	return worst
}
