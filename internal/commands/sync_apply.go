package commands

import (
	"context"
	"fmt"
	"maps"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/ronjatech/ronja-cli/internal/api"
	"github.com/ronjatech/ronja-cli/internal/config"
	"github.com/ronjatech/ronja-cli/internal/wfdir"
	"github.com/spf13/cobra"
)

// flagSyncDryRun prints what apply would attempt and touches nothing.
var flagSyncDryRun bool

// flagSyncAllowCreate opts into the one refusal that is about a row not existing
// yet. See syncApplyGate, and syncApplyTree for where the count is named.
var flagSyncAllowCreate bool

// `ronja sync apply` is the deploy verb for a REPOSITORY: it runs each folder's
// own push loop and then its own publish, in path order, and reports one answer
// for the tree.
//
// ⚠️ IT IS NOT A NEW WRITER. Every byte it sends is sent by a loop that already
// exists, under that loop's own guards, and the local half of every refusal it
// makes comes from sync_decision.go — the same values `--dry-run` prints. A
// second copy of "may this folder be written to" is exactly how a tree command
// and the per-folder command it orchestrates start disagreeing.
//
// What it ADDS is refusal, and the two it adds are the design rather than a
// limitation. See the comments on syncApplyGate.
func newSyncApplyCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "apply",
		Short: "Deploy every folder beneath a directory",
		Long: `Deploy every folder beneath a directory.

Finds every ronja.json under --dir and, for each one, runs that folder's own
push AND its publish against the stack you name — so what comes out is LIVE,
not a repository of staged drafts.

Folders are deployed in PATH ORDER, which the report names. That is for
determinism and diffable CI logs and nothing else: a derived table's draft
always builds against its inputs' LIVE data, and a commit cascades server-side,
so a folder deployed "too early" self-heals on the next run rather than landing
something wrong.

UPDATE-ONLY, and that is the safety property this command is built around. It
refuses anything that does not already exist:

  create           this file (or this folder) has no row behind it yet, so a
                   push would MAKE one. There is no delete verb to undo that,
                   and a CI runner that discards its working tree loses the ids
                   and creates again on the next run
  unanchored       an automation this folder is bound to that it has never
                   recorded agreeing with. Both of its guards are then off: the
                   write would overwrite whatever moved on the server, and would
                   turn the automation back on if somebody had paused it
  manifest_rewrite acting here would REWRITE a committed ronja.json. Migration
                   is a per-folder act somebody watches, never a side effect of
                   deploying thirty folders

Each of those is refused PER CREATION UNIT — per file for a pipeline and an
automation, per folder for a workflow and a data app — because that is the grain
the lock discriminates at. Push the folder on its own to resolve one.

--allow-create opts into the first of them, and only the first: it names how many
ROWS it is about to make, in which organization AND in which features, before it
makes any of them. Rows rather than folders, because they are different numbers —
a merge that adds one file to each of thirty folders is thirty rows — and volume
is the failure mode. The features because every create resolves its destination
from a committed ronja.json, so a merge can redirect one without changing
anything else the line would print. The other two refusals stand whatever it
says.

There is no --force and no --overwrite-remote. Where apply refuses on drift, the
escape is the folder's own command (` + "`ronja pipeline publish --overwrite-remote`" + `
and friends), run by somebody who is looking at that one folder.

It never deletes. A file removed from a folder leaves its row alone, exactly as
the per-folder push does.

Publishing passes --no-request-review, so a credential that may not commit FAILS
instead of filing a review request in every folder — and a draft that did end up
submitted for review is reported as NOT deployed.

A folder that refuses does not stop the run: every folder is attempted, every
one is reported, and the exit code is the tree's answer:

  0   every folder was deployed (or had nothing to deploy)
  1   checked, and something was refused or did not land
  2   could not tell — a folder was skipped, a manifest would not load, the
      credential is dead, the walk found no folders at all, or a write went out
      and the instance never answered (which is not the same as a refusal: a
      commit that timed out may well have landed, so look before re-running)

--dry-run prints, per creation unit, what apply would ATTEMPT, off the same
decision apply then executes. It deliberately does NOT promise a clean remote:
that is a read whose answer can move between the dry-run and the apply, and
promising it is what makes a dry-run a liar.`,
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			// resolveInstance, NOT config.Resolve: unlike `status` and `check`,
			// this command ACTS. There is no useful degraded answer for a dead
			// credential here — every folder would be reported unknown — and the
			// dry-run needs the same organization the apply would use, or it
			// would preview a stack selection the apply does not make.
			resolved, err := resolveInstance()
			if err != nil {
				return withExitCode(syncExitUnknown, err)
			}
			report, err := syncApplyTree(cmd.Context(), flagSyncDir, resolved)
			if err != nil {
				return err
			}
			if flagJSON {
				if err := emitJSON(report); err != nil {
					return err
				}
			} else {
				printSyncApply(report)
			}
			return report.exitError()
		},
	}
	cmd.Flags().BoolVar(&flagSyncDryRun, "dry-run", false,
		"print what apply would attempt, and write nothing")
	cmd.Flags().BoolVar(&flagSyncAllowCreate, "allow-create", false,
		"create the rows that do not exist yet, after naming how many")
	return cmd
}

// syncApplyReport is the --json shape, and the same struct the human renderer
// reads — so the two can never describe different things.
type syncApplyReport struct {
	Dir   string `json:"dir"`
	URL   string `json:"url"`
	Stack string `json:"stack,omitempty"`
	// DryRun reports that nothing was written. It is a field rather than an
	// absence because every other value in this report reads identically in
	// both modes, and a reader who cannot tell them apart would take a preview
	// for a deploy.
	DryRun bool `json:"dryRun"`
	// AllowCreate reports that the create refusal was opted out of. In the report
	// because a run that made thirty rows must not look identical to one that made
	// none — the flag is the whole difference between them.
	AllowCreate bool `json:"allowCreate,omitempty"`
	// Order names the order the folders were deployed in, because a CI log that
	// does not say so leaves the reader guessing whether a failure was an
	// ordering problem. Always "path" today; see the command's own help.
	Order string `json:"order"`
	// Verdict is the tree's answer — applied, refused or unknown — carrying the
	// same word as the exit code, because exit codes do not survive a wrapper
	// script.
	Verdict string                  `json:"verdict"`
	Folders []syncApplyFolderReport `json:"folders"`
	Summary syncApplySummary        `json:"summary"`
}

// syncApplyFolderReport is one folder's line.
type syncApplyFolderReport struct {
	Path string `json:"path"`
	Root string `json:"root"`
	Kind string `json:"kind,omitempty"`
	// Title is the manifest's, absent for a folder ruled out before it loaded.
	Title   string `json:"title,omitempty"`
	Verdict string `json:"verdict"`
	// Reason is a sync* constant and is the machine contract; Detail is prose
	// beside it and may be reworded. Same split as `sync status`'s.
	Reason string `json:"reason,omitempty"`
	Detail string `json:"detail,omitempty"`
	// Units is what a push would do to each creation unit, EMPTY rather than
	// null for a folder that never got that far.
	Units []syncApplyUnitReport `json:"units"`
	// Notes are true, worth printing and not a refusal: the alias pre-flight's
	// warnings, which every push loop prints and which a tree run would
	// otherwise swallow.
	Notes []string `json:"notes,omitempty"`
}

// syncApplyUnitReport is one creation unit's decision — the value apply acts on
// and the value --dry-run prints, never two derivations of it.
type syncApplyUnitReport struct {
	// Path is the folder-relative file, EMPTY for a workflow or a data app,
	// whose creation unit is the folder itself.
	Path string `json:"path,omitempty"`
	// ID is the row already bound to this unit, empty for a create.
	ID string `json:"id,omitempty"`
	// Action is applyAction: create, update, unanchored or refuse.
	//
	// ⚠️ There is deliberately no `no-op`. It is knowable locally for a pipeline
	// file and NOT for a workflow or a data app, whose files are compared
	// against a listing of the caller's own draft — so a unit apply may find
	// unchanged is reported as `update`, which is what apply will ATTEMPT. A
	// uniform `no-op` would be a promise only one kind could keep.
	Action string `json:"action"`
	Reason string `json:"reason,omitempty"`
	Detail string `json:"detail,omitempty"`
}

type syncApplySummary struct {
	Folders int `json:"folders"`
	Applied int `json:"applied"`
	Refused int `json:"refused"`
	// Unknown counts every folder that could not be vouched for either way.
	Unknown int `json:"unknown"`
	// Creates is how many creation units in this tree have no row behind them.
	// Counted in FILES, never folders: Tables and Automations are per-file maps,
	// so "thirty folders" and "thirty rows" are different numbers and the failure
	// mode here is volume.
	//
	// ⚠️ A FLOOR, not a census. It is summed over the units each folder reported,
	// and a folder refused BEFORE its decisions were computed — an unreadable
	// folder, one whose manifest would be rewritten, one whose binds do not
	// resolve, one that is not bound on this stack — reports none, so whatever it
	// would have created is not in here. That is not a gap to close by guessing:
	// a folder we could not open has no knowable unit count, and a number that
	// invented one would be worse than a number that is short. The refusals are
	// listed individually above it, which is where that folder is accounted for.
	Creates int `json:"creates,omitempty"`
	// Created is how many of those apply actually went ahead with — the number
	// --allow-create names on stderr before the first write, and zero without it.
	//
	// A SECOND number rather than a reinterpretation of the first, because the
	// two really do differ: a folder held back by an unanchored anchor still has
	// create units, and they are counted above and not here. Collapsed into one
	// field, a run that made four rows and refused a fifth would have to claim
	// either four creation units or five created rows, and both are false.
	Created int `json:"created,omitempty"`
}

// The three verdicts a folder gets from apply.
//
// `applied` and `refused` are apply's spellings of `sync status`'s clean and
// drifted, on the precedent `sync check` already set with `broken`: one rank,
// one exit code, a word that is true about what THIS command did. "Drifted" is
// a claim about content moving on the server and says nothing about a deploy.
const ()

// exitError turns the tree verdict into the process exit code, on the same
// three-valued contract as `sync status`.
func (r *syncApplyReport) exitError() error {
	// Agreed with the REFUSED count, which is the subject of the sentence: "1 of
	// 30 folders was not deployed", not "were".
	verb := agree(r.Summary.Refused, "was not deployed", "were not deployed")
	if r.DryRun {
		verb = "would be refused"
	}
	switch r.Verdict {
	case verdictRefused:
		return withExitCode(syncExitDrifted, fmt.Errorf("%d of %d %s %s — see above",
			r.Summary.Refused, r.Summary.Folders, plural(r.Summary.Folders, "folder"), verb))
	case verdictUnknown:
		if r.Summary.Folders == 0 {
			return withExitCode(syncExitUnknown, fmt.Errorf("no %s found under %s — nothing was deployed, which is not the same answer as nothing needing to be",
				wfdir.ManifestName, r.Dir))
		}
		return withExitCode(syncExitUnknown, fmt.Errorf("%d of %d %s could not be deployed or ruled out — see above; this is not the same answer as everything being live",
			r.Summary.Unknown, r.Summary.Folders, plural(r.Summary.Folders, "folder")))
	}
	return nil
}

// syncApplyTree walks the tree and deploys every folder it can.
//
// The shape mirrors syncStatusTree and syncCheckTree exactly — one walk, one
// organization lookup before the loop, one stack resolution per folder, all
// serial and in path order — so the three tree commands cannot disagree about
// which folders a repository contains or which of them a --stack applies to.
func syncApplyTree(ctx context.Context, dir string, resolved *config.Resolved) (*syncApplyReport, error) {
	// A bad --stack is a wrong ARGUMENT, not something to degrade around. Same
	// rule, and same reasoning, as syncStatusTree's.
	if flagStack != "" {
		if err := wfdir.ValidStackName(flagStack); err != nil {
			return nil, err
		}
	}
	found, err := discoverFolders(dir)
	if err != nil {
		return nil, err
	}
	base, err := filepath.Abs(dir)
	if err != nil {
		return nil, fmt.Errorf("resolve %s: %w", dir, err)
	}

	// Resolved ONCE, before the loop — see syncStatusTree, where the reason is
	// that a lazy lookup would leave folder one compared on URL alone and folder
	// two compared on both. ⚠️ Unlike there it is a REFUSAL here: an acting
	// command whose organization is unknown is openFolder's own hard error, and
	// a tree of thirty writes is the last place to guess at one.
	if resolved.TenantID == "" {
		if err := ensureTenant(ctx, resolved); err != nil {
			return nil, withExitCode(syncExitUnknown, err)
		}
	}
	key := wfdir.InstanceKey{URL: resolved.URL, TenantID: resolved.TenantID}

	report := &syncApplyReport{
		Dir:         base,
		URL:         resolved.URL,
		Stack:       flagStack,
		DryRun:      flagSyncDryRun,
		AllowCreate: flagSyncAllowCreate,
		Order:       "path",
		Folders:     []syncApplyFolderReport{},
	}

	// PLAN EVERY FOLDER FIRST, then act. The split is what --allow-create needs:
	// its promise is a NUMBER for the whole tree ("this will create 30 rows"),
	// and a number announced folder by folder is thirty small numbers nobody adds
	// up — which is precisely the volume failure the flag has to make visible. It
	// costs nothing: every decision here is local, and the organization was
	// resolved once above, so the pre-pass makes no request at all.
	planned := make([]plannedFolder, 0, len(found))
	creating := 0
	interrupted := false
	for _, folder := range found {
		if ctx.Err() != nil {
			// ⚠️ THE REPORT IS FINISHED RATHER THAN DISCARDED. Nothing has been
			// written yet at this point, but the rule is the same one the execute
			// loop needs and the per-folder loops already keep: an interrupt is an
			// answer about every folder, and returning no report at all leaves the
			// operator of a half-applied tree with nothing to read — least of all
			// under --json, where the error goes to stderr and the document a job
			// parses never arrives.
			interrupted = true
			break
		}
		plan := planOneFolder(ctx, folder, resolved, key)
		// Counted only for the folders that will actually RUN. A folder held back
		// by an unanchored anchor or an unresolved bind creates nothing whatever
		// the flag says, and including it would overstate the promise.
		if plan.f != nil {
			creating += plan.creates
		}
		planned = append(planned, plan)
	}
	// The folders the walk found and the plan never reached. They are `unknown`
	// for the same reason a folder ruled out before it loaded is: nothing here
	// answers for them either way.
	for i := len(planned); i < len(found); i++ {
		planned = append(planned, plannedFolder{line: syncApplyFolderReport{
			Path: found[i].Rel, Root: found[i].Root, Kind: found[i].Kind.Name,
			Units: []syncApplyUnitReport{},
		}})
	}
	// NAMED BEFORE THE FIRST WRITE, on stderr so it survives --json. Rows rather
	// than folders, because they are different numbers: Tables and Automations
	// are per-file maps, so thirty folders on a merge that added one file each is
	// thirty rows, and thirty is the number worth reading before it happens.
	// Guarded on the FLAG as well as the count, though the count cannot be
	// non-zero without it while the gate stands: this line names --allow-create,
	// and a message that named a flag nobody passed would be the confusing kind.
	// Not after an interrupt: nothing is going to be created, and a promise about
	// writes that will not happen is the one line here nobody should read twice.
	if !interrupted && flagSyncAllowCreate && creating > 0 {
		verb := "will create"
		if flagSyncDryRun {
			verb = "would create"
		}
		// THE DESTINATION IS PART OF THE PROMISE. Every create resolves its
		// feature from a committed, hand-editable ronja.json, and all four
		// decisions gate on nothing but "featureID is not empty" — so in the CI
		// setting this flag is for, a commit that edits one folder's featureID (or
		// adds a folder naming another feature) silently redirects creates into any
		// feature the runner's credential can reach, and a line naming only the
		// count and the organization cannot tell that run from the intended one.
		// This is the informed-consent property the flag exists to provide, not a
		// hole somebody is exploiting.
		fmt.Fprintf(os.Stderr, "  --allow-create: this %s %d %s in %s.\n",
			verb, creating, plural(creating, "row"), describeKeyForApply(key))
		if into := describeCreateDestinations(planned); into != "" {
			fmt.Fprintf(os.Stderr, "                  %s\n", into)
		}
	}

	verdicts := make([]string, 0, len(found))
	created := 0
	for i := range planned {
		if ctx.Err() != nil {
			// ⚠️ FINISH THE REPORT. A tree interrupted 20 folders into 30 is
			// precisely the state that needs one — some of it is live and some of
			// it is not — and returning an error instead prints nothing at all,
			// --json included. The rest are `unknown` because they were never
			// attempted, which is a different answer from refused.
			markApplyInterrupted(planned[i:])
		} else {
			runPlannedFolder(ctx, &planned[i])
			created += planned[i].created
		}
		report.Folders = append(report.Folders, planned[i].line)
		verdicts = append(verdicts, planned[i].line.Verdict)
	}

	report.Summary = summarizeApply(report.Folders)
	// WHAT LANDED, not what was planned. A folder that cleared the gate and then
	// failed mid-push still holds its create units in Creates, and reporting them
	// here would print a plan as history — "4 rows created" about a run that made
	// none. A dry-run has no history to report, so there it is the plan, and the
	// renderer says "would be created" for exactly that reason.
	report.Summary.Created = created
	if flagSyncDryRun {
		report.Summary.Created = creating
	}
	report.Verdict = worstApplyVerdict(verdicts)
	// An EMPTY WALK is unknown, never green — see syncStatusTree. A renamed
	// directory must not report a deploy that never touched anything.
	if len(found) == 0 {
		report.Verdict = verdictUnknown
	}
	return report, nil
}

// describeCreateDestinations names the FEATURES the creates in this tree would
// land in, off the same decisions apply is about to execute.
//
// Grouped and deduped rather than one line per row, because the number the line
// above names is the volume failure mode and thirty lines is how you hide it:
// the ordinary single-feature run stays one short clause, and a run that would
// scatter rows across several features says so in one place. Sorted by id, so
// two runs of the same tree produce the same bytes in a CI log.
func describeCreateDestinations(planned []plannedFolder) string {
	rows := map[string]int{}
	for _, plan := range planned {
		if plan.f == nil {
			continue
		}
		for _, d := range plan.decisions {
			// Creates(), not WouldCreate(): a unit refused for want of a feature has
			// no destination to name, and it is the reason it has none.
			if d.Creates() {
				rows[plan.f.Binding.FeatureID]++
			}
		}
	}
	features := make([]string, 0, len(rows))
	for id := range rows {
		features = append(features, id)
	}
	sort.Strings(features)
	if len(features) == 0 {
		// Unreachable beside the count the caller guards on — the two are summed
		// from the same predicate — and empty rather than a sentence about no
		// features, so an accident here prints nothing instead of nonsense.
		return ""
	}
	if len(features) == 1 {
		return "Into feature " + features[0] + "."
	}
	parts := make([]string, 0, len(features))
	for _, id := range features {
		parts = append(parts, fmt.Sprintf("%s (%d %s)", id, rows[id], plural(rows[id], "row")))
	}
	return "Into features " + strings.Join(parts, ", ") + "."
}

// plannedFolder is one folder's answer BEFORE anything is written: the line as
// far as it can be filled in locally, and — only when the folder is cleared to
// run — the opened folder to run it on.
//
// The plan/execute split exists for --allow-create, whose promise is a number
// for the WHOLE TREE. See syncApplyTree.
type plannedFolder struct {
	line syncApplyFolderReport
	// f is non-nil ONLY when every local gate cleared. A nil f is the whole of
	// "this folder is not going to be written to", so there is one place to read
	// that from rather than a verdict a later edit could set two ways.
	f *folder
	// creates is how many of this folder's creation units have no row behind
	// them — the number --allow-create names, in FILES.
	creates int
	// created is how many rows the run actually made here, filled in by the
	// execute pass from the loops' own per-unit `created` flags. Zero until then,
	// which is the whole difference between the plan and the history.
	created   int
	decisions []applyDecision
}

// markApplyInterrupted finishes the lines of folders the run never got to.
//
// A folder already answered LOCALLY keeps its answer: a refusal decided before
// any request is true whether or not the run reached it, and overwriting it with
// `interrupted` would lose the one thing that folder is certain about.
func markApplyInterrupted(rest []plannedFolder) {
	for i := range rest {
		if rest[i].line.Verdict != "" {
			continue
		}
		rest[i].line.Verdict, rest[i].line.Reason = verdictUnknown, syncReasonInterrupted
		// NOT composed with interruptedMessage. Its second half — "anything
		// already sent is still running on the server" — is true of the folder the
		// Ctrl-C landed in and false of every folder after it, which is exactly
		// the set this marks. Reusing the constant here said both things at once.
		rest[i].line.Detail = "not attempted — the run was interrupted before this folder was reached"
	}
}

// planOneFolder answers everything about one folder that needs no request:
// whether it may be acted on at all, and what a push would do to each of its
// creation units.
func planOneFolder(ctx context.Context, folder discoveredFolder,
	resolved *config.Resolved, key wfdir.InstanceKey) plannedFolder {

	plan := plannedFolder{line: syncApplyFolderReport{
		Path: folder.Rel, Root: folder.Root, Kind: folder.Kind.Name,
		Units: []syncApplyUnitReport{},
	}}
	line := &plan.line
	unknown := func(reason, format string, args ...any) plannedFolder {
		line.Verdict, line.Reason, line.Detail = verdictUnknown, reason, fmt.Sprintf(format, args...)
		return plan
	}
	refuse := func(reason, format string, args ...any) plannedFolder {
		line.Verdict, line.Reason, line.Detail = verdictRefused, reason, fmt.Sprintf(format, args...)
		return plan
	}

	if folder.notChecked() {
		return unknown(folder.Reason, "%s", folder.Detail)
	}
	line.Title = folder.Manifest.Title

	// The lock is read here rather than in the walk because only the stack
	// resolution needs it, and a folder ruled out earlier never pays for it.
	lock, lockErr := wfdir.LoadLock(folder.Root)
	if lockErr != nil {
		return unknown(syncReasonUnreadable, "%s", lockErr.Error())
	}
	// decideApplyFolder is resolveStackForFolder — the tree commands' own stack
	// rule, so apply skips exactly the folders `sync status` skips — PLUS the two
	// acts that rewrite a committed ronja.json.
	if reason, detail := decideApplyFolder(folder.Manifest, lock, key, flagStack); reason != "" {
		// ⚠️ A manifest rewrite is REFUSED, not `unknown`. Nothing about this
		// folder is unreadable: it is perfectly legible and apply is declining to
		// migrate it, which is a definite answer and belongs at exit 1 with the
		// other definite ones. The stack reasons are the opposite — nothing here
		// answers for the stack asked about — and stay `unknown`, which is what
		// keeps a --stack typo from reporting a repository deployed.
		if reason == syncReasonManifestRewrite {
			return refuse(reason, "%s", detail)
		}
		return unknown(reason, "%s", detail)
	}

	// ⚠️ openFolderForStatusAt, NEVER openFolder, and this is load-bearing rather
	// than borrowed from `sync check`. The acting opener calls adoptStack, which
	// WRITES ronja.json — the very act decideApplyFolder just refused. Opening
	// through it would perform the migration and then report that apply had
	// declined to.
	f, err := openFolderForStatusAt(ctx, folder.Root, resolved, folder.Kind)
	if err != nil {
		return unknown(syncReasonFolderRefused, "%s", err.Error())
	}
	// The three refusals openFolder makes that openFolderForStatusAt does not,
	// as REPORTS rather than as a whole-run abort. A tree legitimately holds
	// folders belonging to several organizations, and one of them being
	// unreadable is not a reason to stop deploying the rest.
	if f.OrgErr != nil {
		return unknown(syncReasonLookupFailed, "%s", f.OrgErr.Error())
	}
	if f.BindingErr != nil {
		return unknown(syncReasonFolderRefused, "%s", f.BindingErr.Error())
	}
	// The NO-FLAG half of the one-stack-many-folders rule, and slice 2's
	// selection rule verbatim: an unambiguous (url, organization) match selects,
	// and otherwise --stack is required. Without it a folder belonging to another
	// organization would fall through to the create path and be reported as a
	// refusal about creates — a true statement about the wrong problem.
	if flagStack == "" && !f.Bound {
		return unknown(syncReasonNotBoundHere, "%s", notBoundHereDetail(f))
	}

	files, _, readErr := readLocalFiles(f.Root, f.Kind)
	if readErr != nil {
		return unknown(syncReasonUnreadable, "%s", readErr.Error())
	}
	// The alias pre-flight, exactly as each push loop runs it, with its WARNINGS
	// kept: they are not a refusal, every loop prints them, and a tree run that
	// dropped them would be the one place a dead binding goes unmentioned.
	aliases, bindReason, bindDetail := decideApplyBind(f, files)
	line.Notes = append(line.Notes, aliases.Warnings...)
	if bindReason != "" {
		return refuse(bindReason, "%s", bindDetail)
	}

	plan.decisions = applyDecisionsFor(f, sortedPaths(files))
	for _, d := range plan.decisions {
		line.Units = append(line.Units, syncApplyUnitReport{
			Path: d.Path, ID: d.ID, Action: string(d.Action), Reason: d.Reason, Detail: d.Detail,
		})
		if d.Creates() {
			plan.creates++
		}
	}
	// The loops' own LOCAL guards, BEFORE apply's gate — the order they really run
	// in, and the reason they are here at all: each fires before the loop's first
	// byte, so a dry-run that skipped them reported `applied` for a folder the
	// apply then exited 1 on. See decideApplyPushPreflight, including what it
	// still does not cover.
	if reason, detail := decideApplyPushPreflight(f, files); reason != "" {
		return refuse(reason, "%s", detail)
	}
	if reason, detail := syncApplyGate(f, plan.decisions, flagSyncAllowCreate); reason != "" {
		return refuse(reason, "%s", detail)
	}
	// Cleared. A non-nil f from here on is what the execute pass reads.
	plan.f = f
	return plan
}

// runPlannedFolder executes one planned folder, or finishes its line as the
// preview it already is.
func runPlannedFolder(ctx context.Context, plan *plannedFolder) {
	line := &plan.line
	refuse := func(reason, format string, args ...any) {
		line.Verdict, line.Reason, line.Detail = verdictRefused, reason, fmt.Sprintf(format, args...)
	}
	unknown := func(reason, format string, args ...any) {
		line.Verdict, line.Reason, line.Detail = verdictUnknown, reason, fmt.Sprintf(format, args...)
	}
	if plan.f == nil {
		return
	}
	if flagSyncDryRun {
		line.Verdict = verdictApplied
		line.Detail = describeAttempt(plan.f.Kind, plan.decisions)
		return
	}
	detail, created, reviewed, err := applyFolder(ctx, plan.f)
	// Counted whatever the verdict: a push that made two rows and then failed on
	// the third made two rows, and a run that reported none would be the report
	// contradicting the organization.
	plan.created = created
	switch {
	case err != nil && ctx.Err() != nil:
		// ⚠️ THE INTERRUPT IS ASKED ABOUT FIRST, because it is the case where the
		// loop's own error is least informative: a Ctrl-C mid-folder aborts
		// whatever request was in flight, and the sentence that comes back
		// describes the abort rather than the folder. Whether the write landed is
		// genuinely unknown, and the folders after this one already say
		// `interrupted` — see markApplyInterrupted.
		unknown(syncReasonInterrupted, "%s", err.Error())
	case err != nil && (api.Unanswered(err) || api.IsTimeout(err)):
		// THE INSTANCE DID NOT ANSWER: a connection that never landed, a 429, a
		// 5xx — or our own deadline, which is worse, because a write that timed
		// out may perfectly well have COMMITTED. `refused`/not_deployed is a claim
		// nothing here can make, and a CI log that reads "not deployed" beside a
		// folder that may be deployed is the one answer that sends somebody to
		// re-run a write by hand. Exit 2, on this command's own three-valued
		// contract and on the mapping every sibling tree command already uses for
		// a read the instance did not answer (syncReasonLookupFailed).
		//
		// Best-effort by nature: a loop that reports its stop as prose rather than
		// as a wrapped error is indistinguishable from a definite refusal here, so
		// this narrows the lie rather than closing it.
		unknown(syncReasonUnanswered, "%s", err.Error())
	case err != nil:
		// A push or publish that did not land is a DEFINITE negative: it was
		// attempted and the server answered. Exit 1, beside the local refusals,
		// and never `unknown` — a CI job reading "could not tell" goes looking for
		// a credential problem it does not have.
		refuse(syncReasonNotDeployed, "%s", err.Error())
	case reviewed:
		// ⚠️ A REVIEW REQUEST IS NOT A DEPLOY, and it has to be said here rather
		// than trusted to --no-request-review. runPipelinePublish counts only
		// `refused` and `conflict` as failed, so `submitted_for_review` returns a
		// nil error — and an apply that read only the error would report thirty
		// filed review requests as thirty successful deploys.
		refuse(syncReasonSubmittedForReview, "%s", detail)
	default:
		line.Verdict, line.Detail = verdictApplied, detail
	}
}

// Two reasons of apply's own, in the tree commands' vocabulary and under the
// same contract: these strings are the machine half of a report.
const (
	// syncReasonNotDeployed — the folder was attempted and the change is not
	// live. The prose beside it is the loop's own message.
	syncReasonNotDeployed = "not_deployed"
	// syncReasonSubmittedForReview — a draft ended up in an admin's inbox rather
	// than on the row. Its own reason, and not folded into not_deployed, because
	// the two need opposite actions: one is somebody's to fix, the other is
	// somebody's to approve.
	syncReasonSubmittedForReview = "submitted_for_review"
	// syncReasonWouldCreate — a creation unit with no row behind it. See
	// syncApplyGate.
	syncReasonWouldCreate = "would_create"
	// syncReasonInterrupted — the run was cancelled before this folder was
	// attempted, or while it was being written to. `unknown` rather than refused:
	// nothing was decided about it, and a job that read a refusal would go
	// looking for a problem in the repository.
	syncReasonInterrupted = "interrupted"
	// syncReasonUnanswered — the folder WAS attempted and the instance did not
	// answer: a connection that never landed, a 429, a 5xx, or our own deadline.
	// Its own reason and not folded into not_deployed, because the two say
	// opposite things about the organization — one is a write the server refused,
	// the other a write that may well have committed — and only one of them can
	// honestly be re-run without looking first.
	syncReasonUnanswered = "unanswered"
)

// syncApplyGate is the whole of what `sync apply` ADDS to the loops it runs: the
// three answers it refuses to act on. An empty reason means "go ahead".
//
// ⚠️ EVERY ONE OF THEM IS PER CREATION UNIT, which is the grain the LOCK
// discriminates at rather than the folder. Binding carries WorkflowID/DataAppID
// per folder, but Tables and Automations are map[path]… per FILE. A folder-grain
// gate would call a pipeline folder with six bound .sql files and one new one
// "bound", and runPipelinePush would then CreateTable the seventh — a live empty
// table in a live organization, out of a command that promised to create
// nothing, repeated across every folder a merge added a file to.
//
// allowCreate opts INTO the create half and nothing else. The other two stay
// refused whatever it says: neither is about a row that does not exist, and a
// single flag that quietly relaxed all three would be the worst kind — one
// nobody could reason about from its name.
func syncApplyGate(f *folder, decisions []applyDecision, allowCreate bool) (reason, detail string) {
	// ⚠️ WITHOUT THE FLAG, `would_create` COMES FIRST — and it is deliberately
	// WouldCreate rather than Creates, so a unit refused for want of a feature is
	// still counted. Apply is not going to run the loop at all here, so the
	// loop's own refusal is not one the reader "would have hit anyway": reporting
	// no_feature sends them off to add a featureID for a row apply refuses on the
	// next run regardless. With the flag the order is the other way round, because
	// then the loop really is about to run and its refusal is the live one.
	if !allowCreate {
		var creates []string
		for _, d := range decisions {
			if d.WouldCreate() {
				creates = append(creates, applyUnitName(f, d))
			}
		}
		if len(creates) > 0 {
			return wouldCreateRefusal(f, creates)
		}
	}
	for _, d := range decisions {
		if d.Refused() {
			return d.Reason, d.Detail
		}
	}
	for _, d := range decisions {
		if d.Action == applyActionUnanchored {
			return d.Reason, d.Detail
		}
	}
	return "", ""
}

// wouldCreateRefusal is the sentence, apart from the gate that decides to say it.
func wouldCreateRefusal(f *folder, creates []string) (reason, detail string) {
	// The escape is NAMED, in both directions: the per-folder command for the
	// ordinary case, and --allow-create for somebody standing up an environment
	// deliberately. A hatch nobody can find is not a hatch, and the reader here is
	// looking at an exit code rather than at this command's help.
	return syncReasonWouldCreate, fmt.Sprintf("%d %s here %s no row in %s yet, so a push would CREATE %s (%s). `ronja sync apply` never creates by default: there is no delete verb to undo one, and a runner that discards its checkout loses the ids and creates again next run.\n  Deploy this folder on its own (`%s push`) once and apply keeps it up to date afterwards, or re-run with --allow-create, which names the number of rows it is about to make first",
		len(creates), plural(len(creates), "thing"), agree(len(creates), "has", "have"),
		describeKeyForApply(f.Key), agree(len(creates), "it", "them"),
		strings.Join(creates, ", "), f.Kind.Command)
}

// applyUnitName is what one creation unit is CALLED in a refusal: the file for
// the two per-file kinds, and the folder itself for the two that have one unit.
func applyUnitName(f *folder, d applyDecision) string {
	if d.Path != "" {
		return d.Path
	}
	return "the " + f.Kind.Name
}

// describeAttempt is the dry-run's one-line summary of a folder, built from the
// SAME decisions apply executes.
//
// ⚠️ IT COUNTS CREATES AS WELL AS UPDATES. A folder is only reached here once
// every gate cleared, so a create present here is one --allow-create let through
// and apply is about to make — and counting updates alone told the reader of an
// all-create folder that "a push would write nothing" immediately before it
// wrote rows. Under --json this sentence is the folder line's only prose.
//
// It says nothing about what the server holds — see the command's help for why
// "a drifted remote" is deliberately not on the list.
func describeAttempt(kind wfdir.Kind, decisions []applyDecision) string {
	updates, creates := 0, 0
	for _, d := range decisions {
		switch d.Action {
		case applyActionUpdate:
			updates++
		case applyActionCreate:
			creates++
		}
	}
	if updates+creates == 0 {
		return "there is nothing here to write"
	}
	// The automation kind has NO PUBLISH — its loop writes production directly —
	// so promising one here would describe a review step that does not exist.
	verb := "push and publish"
	if kind.Name == wfdir.KindAutomation {
		verb = "write"
	}
	if updates == 0 {
		return fmt.Sprintf("would create %d new %s (--allow-create)", creates, plural(creates, "row"))
	}
	out := fmt.Sprintf("would %s %d bound %s (whether each has changed is the server's answer, not this one)",
		verb, updates, plural(updates, "thing"))
	if creates > 0 {
		out += fmt.Sprintf(", and create %d new %s (--allow-create)", creates, plural(creates, "row"))
	}
	return out
}

// applyFolder runs one folder's own push and then its own publish.
//
// ⚠️ PUSH ALONE IS NOT A DEPLOY. Three of the four kinds stage a DRAFT and every
// push report ends `Next: ronja … publish`; an apply that stopped there would
// leave a repository of unpublished drafts and print success.
//
// ⚠️ AND THE FOURTH HAS NO DRAFT AT ALL. The automation loop writes production
// directly, so "apply publishes" must not be read as "everything goes through a
// reviewable draft" — for automations there is nothing to review.
//
// `reviewed` reports that a draft was submitted for admin review rather than
// committed. It is returned SEPARATELY from err because the loops return nil for
// it, deliberately (it is a real outcome for a person standing in the folder),
// and apply is the caller for which it is a failure to deploy.
//
// ⚠️ AN UP-TO-DATE PUSH IS NOT AN ANSWER ABOUT THE PUBLISH, and reading it as
// one was a silent non-deploy. "Nothing to push" has two causes, and only one of
// them means the folder is live:
//
//   - the draft this push writes to is a copy of live and always was. Committing
//     it mints a version for nothing, so an apply of an unchanged repository
//     would grow a version per workflow per run;
//   - an EARLIER run pushed, and its publish then failed — a 500, a conflict, a
//     Ctrl-C in between. There is nothing left to push, so the push is up to
//     date, while the change sits in an uncommitted draft and the live row is
//     stale. Reported `applied`, that is sticky: every later run takes the same
//     short-circuit, exits 0, and never mentions it. A developer's own local
//     `ronja wf push` before CI runs apply reproduces it with no failure at all.
//
// So the publish is skipped only when this folder has NOTHING STAGED — which is
// asked of the record that knows, per kind, and never of the push's verdict:
//
//   - pipeline: the files the baseline records a draft id for (pipelineStagedDrafts),
//     the same set the publish itself would target;
//   - workflow / data app: a draft that PRE-EXISTED this push and holds something
//     the live row does not (draftAwaitsPublish). "Is a draft open" cannot answer
//     it — resolvePushTarget checks one out on the way past — and "did it
//     pre-exist" cannot answer it alone either, because the draft an earlier
//     up-to-date apply left behind pre-exists the next one and is identical to
//     live;
//   - automation: nothing at all. That loop writes production directly, so there
//     is no publish here to skip.
func applyFolder(ctx context.Context, f *folder) (detail string, created int, reviewed bool, err error) {
	switch f.Kind.Name {
	case wfdir.KindPipeline:
		push, pushErr := runPipelinePush(ctx, f, nil, pipelinePushOptions{})
		// COUNTED EVEN ON THE ERROR PATH. A push that made two tables and then
		// failed on the third made two tables, and a report that said none would
		// send the reader looking for rows that are already there.
		created = countPipelineCreates(push)
		if pushErr != nil {
			return "", created, false, pushErr
		}
		// Nothing to push AND nothing staged. With a draft recorded the publish
		// still has work — an earlier run's, whose commit never landed — and
		// running it costs nothing when there is none: runPipelinePublish's own
		// no-argument selection is this same list, and an empty one returns
		// `Nothing` before it makes a single request.
		if push.UpToDate && len(pipelineStagedDrafts(pipelineBaseline(f))) == 0 {
			return "nothing to push", created, false, nil
		}
		pushed := countPipelinePushed(push)
		published, pubErr := runPipelinePublish(ctx, f, nil, true, false)
		if pubErr != nil {
			return "", created, false, pubErr
		}
		if published.Nothing {
			return fmt.Sprintf("pushed %d %s; no draft was staged to publish",
				pushed, plural(pushed, "file")), created, false, nil
		}
		// ⚠️ UNREACHABLE TODAY, and kept for symmetry with the data-app branch
		// below, which is not. runPipelinePublish reaches outcomeSubmittedForReview
		// only through publishRouting, which apply disarms with --no-request-review,
		// and its commit has no `proposed` answer to fall into. Mutating this scan
		// out leaves the suite green — the honest statement about it — but the two
		// draft kinds must not answer a review request differently, and the cost of
		// the loop is one comparison per file.
		for _, file := range published.Files {
			if file.Outcome == outcomeSubmittedForReview {
				return fmt.Sprintf("%s was submitted for admin review rather than committed, so this folder is NOT live", file.Path), created, true, nil
			}
		}
		// COUNTED BY OUTCOME, not by the length of the list. published.Files holds
		// one entry per candidate, and reporting them all as published is a claim
		// about rows this run may not have moved.
		live := 0
		for _, file := range published.Files {
			if file.Outcome == outcomePublished {
				live++
			}
		}
		return fmt.Sprintf("pushed and published %d %s", live, plural(live, "table")), created, false, nil

	case wfdir.KindAutomation:
		// No draft, no publish: this loop writes production directly.
		push, pushErr := runAutomationPush(ctx, f, automationPushOptions{})
		created, written := countAutomationWrites(push)
		if pushErr != nil {
			return "", created, false, pushErr
		}
		if push.UpToDate {
			return "nothing to push", created, false, nil
		}
		// WRITTEN, not enumerated. push.Files carries one entry per file including
		// the `unchanged` ones, and "wrote 9 automations" about a run that wrote
		// one is the report over-claiming on the line an operator reads first.
		return fmt.Sprintf("wrote %d %s", written, plural(written, "automation")), created, false, nil

	case wfdir.KindDataApp:
		push, pushErr := runAppPush(ctx, f, appPushOptions{Validate: true})
		if push != nil && push.Created {
			created = 1
		}
		if pushErr != nil {
			return "", created, false, pushErr
		}
		if push.UpToDate {
			staged, err := appDraftAwaitsPublish(ctx, f, push)
			if err != nil {
				return "", created, false, err
			}
			if !staged {
				return "nothing to push", created, false, nil
			}
		}
		published, pubErr := runAppPublish(ctx, f, true)
		if pubErr != nil {
			return "", created, false, pubErr
		}
		// ⚠️ REACHABLE, and NOT merely defensive. --no-request-review closes the
		// publishRouting leg, but runAppPublish sets outcomeSubmittedForReview a
		// SECOND way: a FIRST publish into a shared feature is committed and the
		// server answers `proposed`, which arrives with a nil commitErr and past no
		// noRequestReview guard at all (dataapp_publish.go). Under --allow-create
		// that is exactly the run apply makes, and without this scan it would report
		// `applied` for an app that only became somebody's proposal.
		if published.Outcome == outcomeSubmittedForReview {
			return "the draft was submitted for admin review rather than committed, so this folder is NOT live", created, true, nil
		}
		return published.Detail, created, false, nil

	default:
		push, pushErr := runPush(ctx, f, pushOptions{Validate: true})
		if push != nil && push.Created {
			created = 1
		}
		if pushErr != nil {
			return "", created, false, pushErr
		}
		if push.UpToDate {
			staged, err := workflowDraftAwaitsPublish(ctx, f, push)
			if err != nil {
				return "", created, false, err
			}
			if !staged {
				return "nothing to push", created, false, nil
			}
		}
		published, pubErr := runPublish(ctx, f, publishOptions{NoRequestReview: true})
		if pubErr != nil {
			return "", created, false, pubErr
		}
		if published.Outcome == outcomeSubmittedForReview {
			return "the draft was submitted for admin review rather than committed, so this folder is NOT live", created, true, nil
		}
		return published.Detail, created, false, nil
	}
}

// workflowDraftAwaitsPublish and appDraftAwaitsPublish answer the ONE question
// applyFolder's up-to-date branch turns on: is there a draft here holding
// something the live row does not?
//
// Three legs, and each rules out a different way of getting it wrong:
//
//   - a draft THIS push checked out is a copy of live by construction, and the
//     push wrote nothing to it. Publishing it commits an identical draft and
//     mints an empty version, which is the regression the skip exists to
//     prevent — so this leg is the one that keeps an unchanged repository from
//     growing a version per folder per run. It is also what keeps the common
//     case free: no request is made at all;
//   - a PARENTLESS draft is not a copy of anything. It is a resource an earlier
//     run created and never published, so there is no live row to compare it
//     against and nothing to compare — it awaits publication by definition.
//     Skipping it left a workflow created under --allow-create unpublished for
//     ever, since every later run is up to date;
//   - anything else is a draft an earlier run (or the author's own `push`) left
//     open, and only its CONTENT says whether its publish ever landed. That
//     costs two listings, and only on a folder that has one lying about.
//
// The content comparison is here because resolvePushTarget checks a draft out
// before it knows whether it has anything to write, so an up-to-date apply
// leaves one behind that pre-exists the NEXT run. See BL-d513: fix that and this
// can gate on pre-existence alone.
//
// ⚠️ FILES ONLY, which is the honest edge of the claim. A push whose only change
// was METADATA — a title, an entrypoint, the declared calendar — and whose
// publish then failed leaves a draft whose files match live, and this answers
// no. That is the pre-existing behaviour for that folder rather than a new hole,
// and the remedy is the same one every refusal here names: publish the folder on
// its own.
func workflowDraftAwaitsPublish(ctx context.Context, f *folder, push *pushResult) (bool, error) {
	if push == nil || push.DraftID == "" || !push.DraftPreexisted {
		return false, nil
	}
	if push.DraftID == push.WorkflowID {
		return true, nil
	}
	client := newClient(f.Resolved.URL, f.Resolved.Token)
	draft, err := client.ListWorkflowFiles(ctx, push.DraftID)
	if err != nil {
		return false, fmt.Errorf("read the files of draft %s to find out whether it still needs publishing: %w", push.DraftID, err)
	}
	live, err := client.ListWorkflowFiles(ctx, push.WorkflowID)
	if err != nil {
		return false, fmt.Errorf("read the files of %s to find out whether its draft still needs publishing: %w", push.WorkflowID, err)
	}
	return !maps.Equal(contentByPath(f.Codec, draft), contentByPath(f.Codec, live)), nil
}

func appDraftAwaitsPublish(ctx context.Context, f *folder, push *appPushResult) (bool, error) {
	if push == nil || push.DraftID == "" || !push.DraftPreexisted {
		return false, nil
	}
	if push.DraftID == push.DataAppID {
		return true, nil
	}
	client := newClient(f.Resolved.URL, f.Resolved.Token)
	draft, err := client.ListDataAppFiles(ctx, push.DraftID)
	if err != nil {
		return false, fmt.Errorf("read the files of draft %s to find out whether it still needs publishing: %w", push.DraftID, err)
	}
	live, err := client.ListDataAppFiles(ctx, push.DataAppID)
	if err != nil {
		return false, fmt.Errorf("read the files of %s to find out whether its draft still needs publishing: %w", push.DataAppID, err)
	}
	return !maps.Equal(appContentByPath(f.Codec, draft), appContentByPath(f.Codec, live)), nil
}

// countPipelineCreates / countPipelinePushed read the loop's OWN per-file flags
// rather than the length of its list, which is what makes the numbers apply
// reports true about the organization rather than about the folder.
func countPipelineCreates(push *pipelinePushResult) int {
	if push == nil {
		return 0
	}
	n := 0
	for _, file := range push.Files {
		if file.Created {
			n++
		}
	}
	return n
}

func countPipelinePushed(push *pipelinePushResult) int {
	n := 0
	for _, file := range push.Files {
		if file.Outcome != pushOutcomeRefused {
			n++
		}
	}
	return n
}

// countAutomationWrites answers (created, written). `written` deliberately
// excludes the `unchanged` rows: they are in push.Files because the loop reports
// on every file, not because it wrote to them.
func countAutomationWrites(push *automationPushResult) (created, written int) {
	if push == nil {
		return 0, 0
	}
	for _, file := range push.Files {
		if file.Created {
			created++
		}
		if file.Outcome != automationOutcomeUnchanged {
			written++
		}
	}
	return created, written
}

func summarizeApply(folders []syncApplyFolderReport) syncApplySummary {
	summary := syncApplySummary{Folders: len(folders)}
	for _, f := range folders {
		switch f.Verdict {
		case verdictApplied:
			summary.Applied++
		case verdictRefused:
			summary.Refused++
		default:
			summary.Unknown++
		}
		for _, unit := range f.Units {
			if unit.Action == string(applyActionCreate) {
				// FILES, not folders. See syncApplySummary.Creates.
				summary.Creates++
			}
		}
	}
	return summary
}

// worstApplyVerdict folds per-folder answers into the tree's, by the ONE
// precedence every tree command uses: unknown, then the middle answer, then the
// green one. Seeded with apply's own green word rather than reusing
// worstVerdict, which would answer "clean" about a run that deployed.
func worstApplyVerdict(verdicts []string) string {
	worst := verdictApplied
	for _, v := range verdicts {
		if verdictRank(v) > verdictRank(worst) {
			worst = v
		}
	}
	return worst
}

// printSyncApply renders the human report.
//
// The folders are LISTED before their verdicts, and in the order they were
// deployed in, for the reason printSyncStatus gives plus one of apply's own: the
// order is part of what this command did, so it belongs in the output rather
// than only in the help.
func printSyncApply(r *syncApplyReport) {
	out := os.Stdout
	fmt.Fprintf(out, "  %s\n", r.Dir)
	fmt.Fprintf(out, "  Instance:   %s\n", r.URL)
	if r.Stack != "" {
		fmt.Fprintf(out, "  Stack:      %s\n", r.Stack)
	}
	if r.DryRun {
		fmt.Fprintf(out, "  DRY RUN — nothing was written.\n")
	}

	if len(r.Folders) == 0 {
		fmt.Fprintf(out, "\n  No %s found under this directory.\n", wfdir.ManifestName)
		return
	}

	fmt.Fprintf(out, "\n  %d %s, in %s order\n", len(r.Folders), plural(len(r.Folders), "folder"), r.Order)
	for _, f := range r.Folders {
		if f.Kind != "" {
			fmt.Fprintf(out, "    %s (%s)\n", f.Path, f.Kind)
			continue
		}
		fmt.Fprintf(out, "    %s\n", f.Path)
	}

	heading := "Deployed"
	if r.DryRun {
		heading = "Would deploy"
	}
	fmt.Fprintf(out, "\n  %s\n", heading)
	for _, f := range r.Folders {
		fmt.Fprintf(out, "    %-8s %s\n", f.Verdict, f.Path)
		if f.Detail != "" {
			fmt.Fprintf(out, "             %s\n", f.Detail)
		}
		// Only in a dry-run: after a real apply the per-unit decisions are the
		// INPUT to what happened, and printing them beside the outcome invites
		// reading them as it.
		if r.DryRun {
			for _, unit := range f.Units {
				fmt.Fprintf(out, "             %-10s %s\n", unit.Action, applyUnitLabel(unit, f.Kind))
			}
		}
		for _, note := range f.Notes {
			fmt.Fprintf(out, "             note       %s\n", note)
		}
	}

	fmt.Fprintf(out, "\n  %s — %d applied, %d refused, %d not deployable\n",
		r.Verdict, r.Summary.Applied, r.Summary.Refused, r.Summary.Unknown)
	// The create line says what actually HAPPENED to those units, and the two
	// answers are opposites: under --allow-create the rows were made, and printing
	// "which apply refuses" underneath a run that made thirty of them would be the
	// report contradicting itself on the line that matters most.
	if made := r.Summary.Created; made > 0 {
		verb := "created"
		if r.DryRun {
			verb = "would be created"
		}
		fmt.Fprintf(out, "  %d %s %s (--allow-create).\n", made, plural(made, "row"), verb)
	}
	if left := r.Summary.Creates - r.Summary.Created; left > 0 {
		// ⚠️ TWO DIFFERENT SENTENCES, because the same number means two different
		// things. Without the flag it is a refusal and the escape is the flag. WITH
		// the flag it is not — those rows sat in a folder held back for another
		// reason, or the run did not reach them — and telling somebody who just
		// passed --allow-create to re-run with --allow-create is the report
		// arguing with the command line it was given.
		if r.AllowCreate && r.DryRun {
			// A dry-run made nothing, so the past tense below would be describing
			// writes that never happened — two lines under one that correctly says
			// "would be created".
			fmt.Fprintf(out, "  %d %s in this tree would still NOT be created: %s folder is refused for another reason, or would not be reached. See above.\n",
				left, plural(left, "row"), agree(left, "its", "their"))
		} else if r.AllowCreate {
			fmt.Fprintf(out, "  %d %s in this tree %s NOT created: %s folder was refused for another reason, or did not run. See above.\n",
				left, plural(left, "row"), agree(left, "was", "were"), agree(left, "its", "their"))
		} else {
			fmt.Fprintf(out, "  %d %s would be CREATED, which apply refuses by default. Deploy those folders on their own once, or re-run with --allow-create.\n",
				left, plural(left, "row"))
		}
	}
}

// applyUnitLabel names one unit in the dry-run listing: the file for the two
// per-file kinds, the kind itself for the two with one unit each.
func applyUnitLabel(unit syncApplyUnitReport, kind string) string {
	if unit.Path != "" {
		return unit.Path
	}
	if kind != "" {
		return "the " + kind
	}
	return "this folder"
}
