package commands

import (
	"bytes"
	"context"
	"errors"
	"fmt"
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
		Long: `Deploy every folder beneath a directory: each folder's own push and its
publish, against the stack you name, so what comes out is LIVE.

  ronja sync apply --stack prod --dry-run   # what would change; writes nothing
  ronja sync apply --stack prod             # deploy it

It only updates rows that exist; --allow-create also makes the new ones, after
saying how many. --dir picks the tree (default: here).

ORDER. Folders deploy in dependency order, which the report names: modules;
pipelines, a folder whose table another reads by address before the one that
reads it; every new workflow another names by address, created before any
workflow's code is written; then the workflows' code, data apps and
automations — path order within each. Under --allow-create a folder may name
by address a row this same run creates: it resolves once that row exists,
which is what stands a new stack up in one apply. A loop of table reads
between pipeline folders is refused.

WRITE CHECK. In a folder with a folder id, every write is checked against the
stack's feature first. Code, an app's codex grant or an agent automation's
table or feature reference that would write outside that feature through
anything the stack does not bind as a write source is refused
writes_outside_target before it is written. A workflow or saved agent it would
RUN outside that feature — a {{ workflow }} or {{ agent }} marker, an app's
allowed workflows or agents, an automation's action or workflow reference — is
refused runs_outside_target unless a "run": true dependency binds it in that
stack. A module folder pushed into a feature the run's other folders do not
deploy to is warned about (shared_module): it publishes the organization's one
module.

UPDATE-ONLY is the safety property this command is built around. It refuses,
per creation unit (each .sql file, metrics/*.json and automation file; the
whole folder for a workflow, data app or module — the grain the lock records):

  create           no row behind it yet, so a push would MAKE one. There is no
                   delete verb to undo that, and a CI runner that discards its
                   working tree loses the ids and creates again next run
  unanchored       an automation this folder is bound to that it has never
                   recorded agreeing with. A write would overwrite whatever
                   moved on the server, and turn it back on if somebody paused it
  manifest_rewrite acting here would REWRITE a committed ronja.json. Migration
                   is a per-folder act somebody watches

Push the folder on its own to resolve one. --allow-create lifts only the first:
before making anything it names how many ROWS (not folders — a merge adding
one file to thirty folders is thirty rows), in which organization AND in which
features, since a commit can redirect a create by editing one ronja.json.

There is no --force and no --overwrite-remote. Where apply refuses on drift,
run the folder's own command (` + "`ronja pipeline publish --overwrite-remote`" + ` and
friends) while looking at that folder — or, in a folder that keeps its records
in Ronja, ` + "`ronja <kind> pull`" + `, which takes the server's version into the folder
to be committed, after which apply writes nothing.

RENAMES. It never deletes, and never infers a rename. Declare one in the
folder's ronja.json — "moved": [{"from": "orders.sql", "to":
"sales_orders.sql"}] keeps the same row under the new file, "removed":
["old.sql"] stops managing a row and leaves it in place — and each stack
applies it when it is applied. A bound file that is gone with neither
declaration is refused (orphaned_file), where the per-folder push only notes
it.

PUBLISHING passes --no-request-review, so a credential that may not commit
FAILS instead of filing a review request in every folder — and a draft that
did end up submitted for review is reported as NOT deployed.

EXIT CODE. A folder that refuses does not stop the run: every folder is
attempted and reported, and the exit code is the tree's answer:

  0   every folder was deployed (or had nothing to deploy)
  1   checked, and something was refused or did not land
  2   could not tell — a folder was skipped, a manifest would not load, the
      credential is dead, the walk found no folders at all, or a write went out
      and the instance never answered (a commit that timed out may well have
      landed, so look before re-running)

--DRY-RUN prints, per creation unit, what apply would ATTEMPT, off the same
decision apply then executes, and sends no write; declared moves and removals
are units too. It also makes apply's comparison with what is live, reading
only: a unit apply would send nothing for is "unchanged" — an adopted workflow,
data app or module whose live row holds the folder, and each of a pipeline
folder's tables, metrics, docs files and checks files, and each automation,
that its push would skip. A unit the apply would refuse is "refuse", and its
folder refused. A unit that could not be compared stays "update" and says why.
A folder whose lock names a row Ronja holds no claim for says how many claims
apply records (no row changes for them). Every answer is as of the dry run's
reads: the organization can move before the apply.`,
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
	// ordering problem. "dependency" (see sync_apply_order.go): modules, then
	// pipelines in build order, then every workflow created before any
	// workflow's code is written, then data apps, then automations — path
	// order within each.
	Order string `json:"order"`
	// Sequence is the folders' paths in the order they ran — Folders stays in
	// path order, so two runs of one tree diff line for line. A folder an
	// interrupt kept from running is not in it.
	Sequence []string `json:"sequence"`
	// Verdict is the tree's answer — applied, refused or unknown — carrying the
	// same word as the exit code, because exit codes do not survive a wrapper
	// script.
	Verdict string `json:"verdict"`
	// Warnings are about the RUN, not one folder's verdict: true, loud, and not
	// a refusal — today only shared_module (sharedModuleWarnings). Absent when
	// there are none.
	Warnings []syncApplyWarning      `json:"warnings,omitempty"`
	Folders  []syncApplyFolderReport `json:"folders"`
	Summary  syncApplySummary        `json:"summary"`
	// LocksRewritten are the ADOPTED folders whose ronja.lock.json this run
	// changed. They keep their records in Ronja, so the lock is a local cache
	// of them: a deploy never needs it, and a stale one is overruled by Ronja's
	// record — so a merge conflict in it is settled by taking either side and
	// running `sync apply`, which rewrites it (until then the dry run and
	// status read the side taken). It is still committed: a fresh checkout's
	// `sync apply --dry-run` and `sync status` compare against it (BL-e7b3).
	// Said once, so a reader of `git status` after a deploy is not left
	// guessing. Absent when there are none.
	LocksRewritten []string `json:"locksRewritten,omitempty"`
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
	// warnings and, for a data app, the write-time lint the push's last file
	// write returned (appPushResult.Warnings, one `file:line: message` each) —
	// which every push loop prints and which a tree run would otherwise
	// swallow.
	Notes []string `json:"notes,omitempty"`
	// Claims is how many rows this folder's lock names that the server holds
	// no claim for — the records apply writes in Ronja before anything else
	// (the lazy backfill), which change no row. In a dry run only.
	Claims int `json:"claims,omitempty"`
}

// syncApplyWarning is one run-level warning: a stable reason (machine
// contract, add never rename), the folder it is about and prose beside it.
type syncApplyWarning struct {
	Reason string `json:"reason"`
	Path   string `json:"path"`
	Detail string `json:"detail"`
}

// syncApplyUnitReport is one creation unit's decision — the value apply acts on
// and the value --dry-run prints, never two derivations of it.
type syncApplyUnitReport struct {
	// Path is the folder-relative file, EMPTY for a workflow, a data app or a
	// module, whose creation unit is the folder itself.
	Path string `json:"path,omitempty"`
	// ID is the row already bound to this unit, empty for a create.
	ID string `json:"id,omitempty"`
	// To is where a `move` unit's address goes; Path is where it was.
	To string `json:"to,omitempty"`
	// Action is applyAction: create, update, unanchored or refuse — or move or
	// remove, for the units a folder's address declarations re-key or stop
	// managing on this stack.
	//
	// `unchanged` appears only in a dry run (applyActionUnchanged), for a unit
	// apply would send nothing for: an adopted workflow, data app or module whose
	// live row already holds the folder, or a pipeline or automation file its
	// push would skip (sync_apply_dryrun.go). A dry run of a pipeline folder also
	// lists its docs and checks files as units, compared the same way. A unit
	// that could not be compared stays `update`, with Detail saying why.
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
	// Claims is the folders' Claims summed: in a dry run, the records in Ronja
	// the apply would write for rows it does not otherwise change.
	Claims int `json:"claims,omitempty"`
}

// exitError turns the tree verdict into the process exit code, on the same
// three-valued contract as `sync status`.
func (r *syncApplyReport) exitError() error {
	// Agreed with the REFUSED count, which is the subject of the sentence: "1 of
	// 30 folders was not fully deployed", not "were". FULLY, because a
	// checks_not_applied folder is refused with its tables published.
	verb := agree(r.Summary.Refused, "was not fully deployed", "were not fully deployed")
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
	// A folder in no git work tree may name by address only folders of this
	// tree (addressBoundary).
	ctx = withAddressWalkRoot(ctx, base)

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
		Order:       applyOrderDependency,
		Sequence:    []string{},
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
		planned = append(planned, planOneFolder(ctx, folder, resolved, key))
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
	// The deferred addresses, and the order: a folder waiting on a row this run
	// does not deploy is refused, and the folders run in dependency order (see
	// sync_apply_order.go), a pipeline read loop refused — after which the
	// deferrals are asked again, since a refused pipeline holds no rows.
	resolveDeferredAddresses(planned)
	modules, pipelines, workflows, apps, automations, rest := applySequence(planned)
	resolveDeferredAddresses(planned)
	noteUnmanagedFolders(planned)
	// Counted only for the folders that will actually RUN. A folder held back
	// by an unanchored anchor or an unresolved bind creates nothing whatever
	// the flag says, and including it would overstate the promise.
	for i := range planned {
		if planned[i].f != nil {
			creating += planned[i].creates
		}
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
	// IN-RUN RESOLUTION: every row a folder holds once it has run (or once pass
	// one has created it) answers the addresses later folders deferred.
	overlay := newAddressOverlay()
	runCtx := withAddressOverlay(ctx, overlay)
	run := func(i int) {
		plan := &planned[i]
		if ctx.Err() != nil {
			// ⚠️ FINISH THE REPORT. A tree interrupted 20 folders into 30 is
			// precisely the state that needs one — some of it is live and some of
			// it is not — and returning an error instead prints nothing at all,
			// --json included. The rest are `unknown` because they were never
			// attempted, which is a different answer from refused.
			if plan.created > 0 && plan.line.Verdict == "" {
				// Pass one made its row; its code was never written.
				plan.line.Verdict, plan.line.Reason = verdictUnknown, syncReasonInterrupted
				plan.line.Detail = "its row was created before any code was written, and the run was interrupted before its code was — the next apply finishes it"
			}
			markApplyInterrupted(planned[i : i+1])
			return
		}
		report.Sequence = append(report.Sequence, plan.line.Path)
		if plan.f != nil && len(plan.pending) > 0 && !flagSyncDryRun {
			// The rows it waited on exist now: resolve them in memory, and the
			// folder's own loop re-runs its pre-flight over the result.
			plan.f.resolveAddressDependencies(runCtx)
		}
		adopted := plan.f != nil && plan.f.adoptedHere()
		lockBefore, _ := os.ReadFile(wfdir.LockPath(plan.line.Root))
		runPlannedFolder(runCtx, plan)
		if !flagSyncDryRun {
			overlay.recordFolder(plan.f)
			if lockAfter, err := os.ReadFile(wfdir.LockPath(plan.line.Root)); adopted && err == nil && !bytes.Equal(lockBefore, lockAfter) {
				report.LocksRewritten = append(report.LocksRewritten, plan.line.Path)
			}
		}
	}
	for _, i := range modules {
		run(i)
	}
	for _, i := range pipelines {
		run(i)
	}
	// PASS ONE: every workflow another workflow names by address that does not
	// exist yet, created and claimed before any workflow's code is written.
	if !flagSyncDryRun {
		chosen := passOne(planned, workflows, overlay)
		for _, i := range workflows {
			if ctx.Err() != nil {
				break
			}
			if !chosen[i] {
				continue
			}
			createWorkflowFirst(runCtx, &planned[i])
			overlay.recordFolder(planned[i].f)
		}
	}
	// PASS TWO, then what names a workflow.
	for _, group := range [][]int{workflows, apps, automations, rest} {
		for _, i := range group {
			run(i)
		}
	}
	// After the run, so each warning can say whether its module changes in it.
	report.Warnings = sharedModuleWarnings(planned)
	for i := range planned {
		report.Folders = append(report.Folders, planned[i].line)
		verdicts = append(verdicts, planned[i].line.Verdict)
		// Pass one's creates and the push's, summed once per folder.
		created += planned[i].created
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
	// addresses is what the folder's `moved` / `removed` did to this stack, in
	// memory, at plan time — reported with the outcome.
	addresses *addressOutcome
	// pending are the address dependencies deferred to a row this run deploys
	// (deferrablePending); pendingDetail is the refusal they would otherwise
	// have been, for the folder resolveDeferredAddresses cannot keep.
	pending       []addressPending
	pendingDetail string
	// settled reports the folder's managed state was settled already — by
	// pass one, before its create.
	settled bool
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
		// An ADOPTED folder whose server state refuses it: a definite
		// answer about this folder, under its own reason.
		if r, ok := asManagedRefusal(err); ok {
			return refuse(r.Reason, "%s", r.Detail)
		}
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
	if err := f.requireAdoptedStackFeature(); err != nil {
		r, _ := asManagedRefusal(err)
		return refuse(r.Reason, "%s", r.Detail)
	}
	// The write gate's notes go on this folder's line (Notes), not to stderr
	// as a single-folder command prints them.
	f.gateNotesCollected = true
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
	// The ADDRESS declarations, before anything reads the binding: a `moved`
	// entry re-keys this stack's recorded row to its new file, so the units
	// below see an update where they would otherwise see a create beside an
	// orphan. IN MEMORY: the execute pass pushes on this same *folder, and its
	// push is what writes the re-key back — a dry-run never does.
	addresses, err := f.resolveAddresses()
	if err != nil {
		// A folder that could not be read is not a declaration to fix.
		if reason := addressFailureReason(err); reason == syncReasonUnreadable {
			return unknown(reason, "%s", err.Error())
		}
		return refuse(syncReasonAddressConflict, "%s", err.Error())
	}
	for _, m := range addresses.Moves {
		line.Units = append(line.Units, syncApplyUnitReport{
			Path: m.From, To: m.To, ID: m.ID, Action: string(applyActionMove),
			Detail: fmt.Sprintf("the %s's address moves from %s to %s; the %s itself, its name included, is not changed by the move", m.Kind, m.From, m.To, m.Kind),
		})
	}
	for _, r := range addresses.Removals {
		line.Units = append(line.Units, syncApplyUnitReport{
			Path: r.Path, ID: r.ID, Action: string(applyActionRemove),
			Detail: fmt.Sprintf("this folder stops managing the %s; it is left exactly as it is — nothing is deleted", r.Kind),
		})
	}
	plan.addresses = addresses
	// The alias pre-flight, exactly as each push loop runs it, with its WARNINGS
	// kept: they are not a refusal, every loop prints them, and a tree run that
	// dropped them would be the one place a dead binding goes unmentioned.
	aliases, bindReason, bindDetail := decideApplyBind(f, files)
	line.Notes = append(line.Notes, aliases.Warnings...)
	if bindReason != "" {
		// Under --allow-create, an address another folder has no claim at YET
		// is deferred, not refused: if this run deploys that folder, the row
		// exists by the time this one runs (resolveDeferredAddresses decides,
		// once every folder is planned). Without the flag nothing is created,
		// so the refusal stands as it always has — and so does any other.
		pending, ok := deferrablePending(f, aliases)
		if !ok || !flagSyncAllowCreate {
			return refuse(bindReason, "%s", bindDetail)
		}
		plan.pending, plan.pendingDetail = pending, bindDetail
	}

	plan.decisions = applyDecisionsFor(f, sortedPaths(files))
	if f.Kind.Name == wfdir.KindPipeline {
		// The METRIC files are creation units too — see pipelineMetricDecisions.
		// After the .sql files, the order the push itself runs them in.
		metrics, err := pipelineMetricDecisions(f)
		if err != nil {
			return unknown(syncReasonUnreadable, "%s", err.Error())
		}
		plan.decisions = append(plan.decisions, metrics...)
	}
	relieveAnchorlessAtDesired(ctx, f, files, plan.decisions)
	for _, d := range plan.decisions {
		line.Units = append(line.Units, syncApplyUnitReport{
			Path: d.Path, ID: d.ID, Action: string(d.Action), Reason: d.Reason, Detail: d.Detail,
		})
		if d.Creates() {
			plan.creates++
		}
	}
	markManagedUnits(f, line.Units)
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
	// The WRITE GATE for a row this run would CREATE, here and not only inside
	// the push: the create is the first thing written, and a folder whose code
	// writes outside this stack's feature must not leave an empty, claimed row
	// behind on its way to being refused. Every other write is gated by the
	// push itself, before its first byte (write_gate.go).
	if reason, detail := planWriteGate(ctx, f, files, plan.decisions); reason != "" {
		if reason == syncReasonWritesOutsideTarget || reason == syncReasonRunsOutsideTarget {
			return refuse(reason, "%s", detail)
		}
		return unknown(reason, "%s", detail)
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
		line.Claims = len(plan.f.managedBackfill())
		line.Detail = describeAttempt(plan.f.Kind, plan.decisions)
		describeAgainstLive(ctx, plan)
		// What the plan-time gate admitted outside the stack's feature is
		// said in a preview too: it is what the apply will do.
		line.Notes = append(line.Notes, plan.f.gateNotes...)
		return
	}
	// An ADOPTED folder settles its server state first — moves, removals and
	// the lazy claims (applyManaged) — and writes back a lock the server
	// disagreed with. A refusal here stops this folder before anything is
	// pushed. (Once: pass one settles a folder before creating its row.)
	if !settlePlanned(ctx, plan) {
		return
	}
	var adopted map[string]string
	detail, created, reviewed, notes, err := applyFolder(ctx, plan.f, &adopted)
	// Counted whatever the verdict: a push that made two rows and then failed on
	// the third made two rows, and a run that reported none would be the report
	// contradicting the organization. ADDED to what pass one made.
	plan.created += created
	// A create unit the push resolved by ADOPTING an existing row is not a row
	// this run made, and not one still to make: it is an update of a row that
	// was there all along. Relabelled before the summary is taken, so Creates
	// does not count it as a row left behind.
	for i := range line.Units {
		if id, ok := adopted[line.Units[i].Path]; ok && line.Units[i].Action == string(applyActionCreate) {
			line.Units[i].Action = string(applyActionUpdate)
			line.Units[i].ID = id
			line.Units[i].Detail = "adopted the metric already holding this file's name and definition (" + id + ") rather than creating one"
		}
	}
	// Appended whatever the verdict too, beside the alias pre-flight's: a lint
	// warning describes files that landed, which is true whether or not the
	// publish after them did. So are the write gate's notes on binds it
	// admitted outside the stack's feature.
	line.Notes = append(line.Notes, notes...)
	line.Notes = append(line.Notes, plan.f.gateNotes...)
	// What the address declarations did, once the push has WRITTEN it: the
	// units list is only printed by a dry-run, and a lock re-keyed by a real
	// apply is a change to a committed file somebody has to commit.
	if a := plan.addresses; a != nil && a.saved {
		for _, m := range a.Moves {
			line.Notes = append(line.Notes, fmt.Sprintf("moved %s → %s (%s %s) in %s — commit it", m.From, m.To, m.Kind, m.ID, plan.f.addressRecordFile()))
		}
		for _, r := range a.Removals {
			line.Notes = append(line.Notes, fmt.Sprintf("%s (%s %s) is no longer managed here and was left as it is — commit %s", r.Path, r.Kind, r.ID, plan.f.addressRecordFile()))
		}
	}
	// An instance's refusal of a workflow runtime gets the way out that `wf push`
	// prints under it (runtimeRefusalHint) — as a NOTE, so Detail stays the
	// one-line refusal a --json reader keys on.
	if hint := runtimeHintFor(err); hint != "" {
		line.Notes = append(line.Notes, hint)
	}
	var checksErr *checksNotAppliedError
	var tagsErr *tagsNotAppliedError
	switch {
	case err != nil && ctx.Err() != nil:
		// ⚠️ THE INTERRUPT IS ASKED ABOUT FIRST, because it is the case where the
		// loop's own error is least informative: a Ctrl-C mid-folder aborts
		// whatever request was in flight, and the sentence that comes back
		// describes the abort rather than the folder. Whether the write landed is
		// genuinely unknown, and the folders after this one already say
		// `interrupted` — see markApplyInterrupted.
		unknown(syncReasonInterrupted, "%s", err.Error())
	case err != nil && isManagedRefusal(plan.f, err):
		// A create the server's state refused (a tombstone or an unreadable
		// row holding the address): its own outcome, with its remedy.
		r, _ := managedRefusalOf(plan.f, err)
		detail := r.Detail
		if (r.Reason == syncReasonWritesOutsideTarget || r.Reason == syncReasonRunsOutsideTarget) && plan.f.managed != nil && plan.f.managed.wroteRecords {
			// The gate refused inside the push, AFTER settle had written this
			// folder's records — so "nothing was written" would not be true.
			detail = strings.TrimSuffix(detail, " Nothing was written") +
				" No row was written; only this folder's records in Ronja (its moved, removed or newly claimed addresses) were settled before the check"
		}
		if _, direct := asManagedRefusal(err); direct {
			refuse(r.Reason, "%s", detail)
		} else {
			refuse(r.Reason, "%s: %s", err.Error(), detail)
		}
	case errors.As(err, &tagsErr):
		// The SQL was published and a metric's tag half did not apply — asked
		// before the checks, which it may carry too. Split as the checks are: a
		// tag write that got no answer may have landed.
		if tagsErr.cause != nil {
			unknown(syncReasonTagsNotApplied, "%s", err.Error())
		} else {
			refuse(syncReasonTagsNotApplied, "%s", err.Error())
		}
	case errors.As(err, &checksErr):
		// The SQL was published and the health checks were not all applied. A
		// definite refusal is a refusal; a checks leg that got no answer — a
		// timeout, a 5xx, a read-back that could not be read — is unknown, under
		// the same reason, because a write that timed out may have landed.
		if checksErr.cause != nil {
			unknown(syncReasonChecksNotApplied, "%s", err.Error())
		} else {
			refuse(syncReasonChecksNotApplied, "%s", err.Error())
		}
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
	// The change is live; its anchor not being on the server is this folder's
	// one remaining problem, and the next apply's drift guard would read a
	// stale baseline without it — so a deploy that was otherwise clean says so.
	// (Every lock save recorded its anchor as it went; this only reports.)
	if st := plan.f.managed; st != nil && st.anchorErr != nil && line.Verdict == verdictApplied {
		reason := syncReasonAnchorNotRecorded
		if r, ok := asManagedRefusal(st.anchorErr); ok {
			reason = r.Reason
		}
		refuse(reason, "%s; but %v", line.Detail, st.anchorErr)
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
	// syncReasonChecksNotApplied — a pipeline folder's SQL was published (or had
	// nothing to publish) and its health checks did not all apply: refused,
	// orphaned, drifted, unsupported by an older server — or, as `unknown`,
	// unanswered. Its own reason rather than not_deployed, because the tables ARE
	// live: checks are ungoverned and advisory, so their leg never holds a table
	// deploy back, and the run still exits non-zero until it is settled.
	syncReasonChecksNotApplied = "checks_not_applied"
	// syncReasonTagsNotApplied — a pipeline folder's SQL was published (or had
	// nothing to publish) and the tag half of one or more metric files did not
	// apply: a failure that says nothing lasting about the file, which the next
	// run retries. Its own reason on the checks precedent: tags are not drafted,
	// so their leg never holds the SQL publish back, and the run still exits
	// non-zero until it lands. When the checks leg failed too, this reason wins
	// and the detail names both.
	syncReasonTagsNotApplied = "tags_not_applied"
)

// checksNotAppliedAfter folds the checks legs of a push and a publish into the
// folder's error, naming the tables that WERE published. nil when both legs
// applied.
func checksNotAppliedAfter(detail string, published []string, legs ...*checksNotAppliedError) error {
	out := &checksNotAppliedError{}
	for _, leg := range legs {
		if leg == nil {
			continue
		}
		out.problems = append(out.problems, leg.problems...)
		if out.cause == nil {
			out.cause = leg.cause
		}
	}
	if len(out.problems) == 0 {
		return nil
	}
	if len(published) > 0 {
		detail += " (" + strings.Join(published, ", ") + ")"
	}
	out.msg = fmt.Sprintf("%s, but the health checks did not all apply: %s", detail, strings.Join(out.problems, "; "))
	return out
}

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
// the two per-file kinds, and the folder itself for the three that have one unit.
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
	out := fmt.Sprintf("would %s whichever of its %d bound %s differ from what is live (a dry run does not compare them)",
		verb, updates, plural(updates, "thing"))
	if creates > 0 {
		out += fmt.Sprintf(", and create %d new %s (--allow-create)", creates, plural(creates, "row"))
	}
	return out
}

// describeAgainstLive sharpens a dry run's line for an adopted workflow,
// data-app or module folder whose one unit is an update: it reads the live row and makes
// the comparison apply makes first (liveHoldsFolder), so the line says what
// apply would do rather than what it would attempt — nothing, when live
// already holds the folder, and a push and publish when it does not. Reads
// only. A read that fails leaves the line as describeAttempt wrote it.
func describeAgainstLive(ctx context.Context, plan *plannedFolder) {
	line, f := &plan.line, plan.f
	switch f.Kind.Name {
	case wfdir.KindWorkflow, wfdir.KindDataApp, wfdir.KindModule:
	case wfdir.KindPipeline, wfdir.KindAutomation:
		// Per FILE, classic and adopted folders alike: apply runs both kinds
		// LiveFirst either way. Not while an address waits on another folder of
		// the run, whose row does not exist yet.
		if len(plan.pending) > 0 {
			return
		}
		if f.Kind.Name == wfdir.KindPipeline {
			comparePipelineUnits(ctx, plan)
		} else {
			compareAutomationUnits(ctx, plan)
		}
		if !refuseCompared(f.Kind, line) {
			line.Detail = describeCompared(f.Kind, line.Units, line.Claims)
		}
		return
	default:
		return
	}
	// An adopted folder only: a classic one (no folder id) previews exactly as
	// it always did.
	if !f.adoptedHere() || len(plan.pending) > 0 || len(line.Units) != 1 || line.Units[0].Action != string(applyActionUpdate) {
		return
	}
	held, refusal, err := compareWithLive(ctx, f, true)
	switch {
	case err != nil:
		return
	case refusal != nil:
		// The apply's own drift guard, asked before its checkout: the folder is
		// refused, in the words the apply would refuse it in.
		refuseUnit(&line.Units[0], syncReasonNotDeployed, "refused by apply's drift guard before its checkout — the folder's line says why")
		line.Verdict, line.Reason, line.Detail = verdictRefused, syncReasonNotDeployed, refusal.Error()
		return
	case held:
		line.Units[0].Action = string(applyActionUnchanged)
		line.Detail = fmt.Sprintf("nothing to push or publish: the live %s already holds this folder", f.Kind.Label)
	default:
		line.Detail = fmt.Sprintf("would push and publish the %s: the live one differs from this folder", f.Kind.Label)
	}
	if records := claimsRecorded(line.Claims); len(records) > 0 {
		line.Detail += "; apply " + joinAnd(records)
	}
}

// applyNothingToPush is a folder's line when its push had nothing to push and
// nothing staged awaited a publish: the apply wrote nothing for it.
const applyNothingToPush = "nothing to push"

// applyFolder runs one folder's own push and then its own publish.
//
// ⚠️ PUSH ALONE IS NOT A DEPLOY. Every kind but one stages a DRAFT and every
// push report ends `Next: ronja … publish`; an apply that stopped there would
// leave a repository of unpublished drafts and print success.
//
// ⚠️ AND THE FOURTH HAS NO DRAFT AT ALL. The automation loop writes production
// directly, so "apply publishes" must not be read as "everything goes through a
// reviewable draft" — for automations there is nothing to review.
//
// `reviewed` reports that a draft was submitted for review rather than
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
//   - workflow / data app / module: a draft that PRE-EXISTED this push
//     (draftAwaitsPublish). The push runs with LiveFirst, so a folder the live
//     row already holds returns before any checkout — no draft, no write, and
//     nothing to publish — and a draft that pre-exists an up-to-date push is
//     then one holding what live does not. "Is a draft open" could not answer
//     it, since a push that goes on checks one out on the way past;
//   - automation: nothing at all. That loop writes production directly, so there
//     is no publish here to skip.
//
// adopted, when not nil, receives the creation units the push resolved by
// adopting a row that already existed (path → id), so the report can say so
// rather than list them as rows still to create.
func applyFolder(ctx context.Context, f *folder, adopted *map[string]string) (detail string, created int, reviewed bool, notes []string, err error) {
	switch f.Kind.Name {
	case wfdir.KindPipeline:
		// NoCreate is the create gate a SECOND time, on the path that actually
		// creates. The plan above refused every folder holding a create unit
		// unless --allow-create was passed, and this is what keeps that true if
		// the two ever disagree: a metric used to be created here, in the push's
		// last pass, with no unit in the plan and no question asked.
		//
		// LiveFirst leaves a table its live row already holds alone, which is
		// what keeps a FRESH checkout — every CI run, with no local baseline to
		// say which files changed — from re-staging and re-publishing every
		// table in the folder on every run.
		push, pushErr := runPipelinePush(ctx, f, nil, pipelinePushOptions{NoCreate: !flagSyncAllowCreate, LiveFirst: true})
		// COUNTED EVEN ON THE ERROR PATH. A push that made two tables and then
		// failed on the third made two tables, and a report that said none would
		// send the reader looking for rows that are already there.
		created = countPipelineCreates(push)
		if adopted != nil {
			*adopted = adoptedPipelineMetrics(push)
		}
		// ⚠️ THE HEALTH-CHECK LEG DOES NOT HOLD THE SQL HOSTAGE. A push whose ONLY
		// problems are its checks (typed by runPipelinePush from its own tally)
		// still publishes: checks are ungoverned and advisory — "a failing check
		// never blocks anything" — while a docs or metric-prose failure is
		// governed and still blocks. The checks problem is not forgiven: it comes
		// back as the folder's verdict, checks_not_applied, after the publish.
		// Never on a cancelled context — an interrupted checks pass must not
		// read as "only the checks" and go on to commit.
		//
		// The TAG leg is the same, for the same reason: tags sit on the live
		// metric and are not drafted, so a tag half that failed — an
		// organization with no tag route yet, a role 403, a 5xx — publishes the
		// SQL anyway and comes back as tags_not_applied after it.
		var pushChecks *checksNotAppliedError
		var pushTags *tagsNotAppliedError
		switch {
		case pushErr == nil:
		case ctx.Err() != nil:
			return "", created, false, notes, pushErr
		case errors.As(pushErr, &pushTags):
			pushChecks = pushTags.checks
		case errors.As(pushErr, &pushChecks):
		default:
			return "", created, false, notes, pushErr
		}
		// Nothing to push AND nothing staged. With a draft recorded the publish
		// still has work — an earlier run's, whose commit never landed — and
		// running it costs nothing when there is none: runPipelinePublish's own
		// no-argument selection is this same list, and an empty one returns
		// `Nothing` before it makes a single request.
		if pushErr == nil && push.UpToDate && len(pipelineStagedDrafts(pipelineBaseline(f))) == 0 {
			return applyNothingToPush, created, false, notes, nil
		}
		pushed := countPipelinePushed(push)
		// A folder that BUILDS nothing — only docs sidecars, which write straight
		// to the live row — has nothing a publish could commit, and
		// runPipelinePublish refuses such a folder outright ("run push first"),
		// which here would refuse a deploy that landed whole.
		if buildsNothing, err := pipelineBuildsNothing(f); err != nil {
			return "", created, false, notes, err
		} else if buildsNothing {
			documented := 0
			for _, d := range push.Docs {
				if d.Outcome == docsOutcomeWritten {
					documented++
				}
			}
			detail := fmt.Sprintf("documented %d %s; this folder builds no table or metric, so there is nothing to publish", documented, plural(documented, "table"))
			if err := checksNotAppliedAfter(detail, nil, pushChecks, nil); err != nil {
				return "", created, false, notes, err
			}
			return detail, created, false, notes, nil
		}
		published, pubErr := runPipelinePublish(ctx, f, nil, true, false)
		// The same split on the publish leg: every commit landed and only the
		// checks applied after them did not.
		var pubChecks *checksNotAppliedError
		if pubErr != nil && !errors.As(pubErr, &pubChecks) {
			return "", created, false, notes, pubErr
		}
		// A TAG change is deployed by the push itself — tags sit on the live
		// metric and are not drafted — so it is said here, where "no draft was
		// staged" would otherwise read as "nothing was deployed".
		tagged := countPipelineTagsApplied(push)
		if published.Nothing {
			detail := fmt.Sprintf("pushed %d %s; no draft was staged to publish", pushed, plural(pushed, "file"))
			if tagged > 0 {
				detail = fmt.Sprintf("changed the tags on %d %s; no draft was staged to publish", tagged, plural(tagged, "metric"))
				if pushed > 0 {
					detail = fmt.Sprintf("pushed %d %s and changed the tags on %d %s; no draft was staged to publish",
						pushed, plural(pushed, "file"), tagged, plural(tagged, "metric"))
				}
			}
			if err := tagsNotAppliedAfter(detail, nil, pushTags, checksNotAppliedAfter(detail, nil, pushChecks, pubChecks)); err != nil {
				return "", created, false, notes, err
			}
			return detail, created, false, notes, nil
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
				return fmt.Sprintf("%s was submitted for review by an admin or a maintainer of the feature rather than committed, so this folder is NOT live", file.Path), created, true, notes, nil
			}
		}
		// COUNTED BY OUTCOME, not by the length of the list. published.Files holds
		// one entry per candidate, and reporting them all as published is a claim
		// about rows this run may not have moved.
		live := 0
		var livePaths []string
		for _, file := range published.Files {
			if file.Outcome == outcomePublished {
				live++
				livePaths = append(livePaths, file.Path)
			}
		}
		detail := fmt.Sprintf("pushed and published %d %s", live, plural(live, "table"))
		if tagged > 0 {
			detail += fmt.Sprintf(", and changed the tags on %d %s", tagged, plural(tagged, "metric"))
		}
		if err := tagsNotAppliedAfter(detail, livePaths, pushTags, checksNotAppliedAfter(detail, livePaths, pushChecks, pubChecks)); err != nil {
			return "", created, false, notes, err
		}
		return detail, created, false, notes, nil

	case wfdir.KindAutomation:
		// No draft, no publish: this loop writes production directly.
		// RequireAnchor on an adopted folder: an anchorless unit the plan let
		// through (relieveAnchorlessAtDesired) is decided again on the row the
		// push reads — the plan's read is not the write's.
		push, pushErr := runAutomationPush(ctx, f, automationPushOptions{RequireAnchor: f.managed != nil})
		created, written := countAutomationWrites(push)
		if pushErr != nil {
			return "", created, false, notes, pushErr
		}
		if push.UpToDate {
			return applyNothingToPush, created, false, notes, nil
		}
		// WRITTEN, not enumerated. push.Files carries one entry per file including
		// the `unchanged` ones, and "wrote 9 automations" about a run that wrote
		// one is the report over-claiming on the line an operator reads first.
		return fmt.Sprintf("wrote %d %s", written, plural(written, "automation")), created, false, notes, nil

	case wfdir.KindDataApp:
		push, pushErr := runAppPush(ctx, f, appPushOptions{Validate: true, LiveFirst: true, Apply: true})
		if push != nil && push.Created {
			created = 1
		}
		// The write-time lint the push's last file write returned, carried as
		// notes because apply is the one loop that publishes straight past them:
		// `ronja app push` prints them under its verdict, and a tree run that
		// swallowed them would deploy the one advisory the deploy should have
		// shown. Advisory, not a verdict — the publish below still runs — and
		// with no "push again to see them" hint, because after apply the folder
		// is up to date and a re-push writes nothing and prints nothing.
		if push != nil {
			for _, w := range push.Warnings {
				notes = append(notes, formatLintWarning(w))
			}
		}
		if pushErr != nil {
			return "", created, false, notes, pushErr
		}
		if push.UpToDate && !draftAwaitsPublish(push.DraftID, push.DraftPreexisted) {
			return applyNothingToPush, created, false, notes, nil
		}
		published, pubErr := runAppPublish(ctx, f, true)
		if pubErr != nil {
			return "", created, false, notes, pubErr
		}
		// ⚠️ REACHABLE, and NOT merely defensive. --no-request-review closes the
		// publishRouting leg, but runAppPublish sets outcomeSubmittedForReview a
		// SECOND way: a FIRST publish into a shared feature is committed and the
		// server answers `proposed`, which arrives with a nil commitErr and past no
		// noRequestReview guard at all (dataapp_publish.go). Under --allow-create
		// that is exactly the run apply makes, and without this scan it would report
		// `applied` for an app that only became somebody's proposal.
		if published.Outcome == outcomeSubmittedForReview {
			return "the draft was submitted for review by an admin or a maintainer of the feature rather than committed, so this folder is NOT live", created, true, notes, nil
		}
		return published.Detail, created, false, notes, nil

	case wfdir.KindModule:
		// The workflow arm's shape on the module loop — its own arm, because the
		// `default:` below runs the WORKFLOW push. There is no validate to run: a
		// module is marker-free, and the push's local guards are its whole check.
		push, pushErr := runModulePush(ctx, f, modulePushOptions{LiveFirst: true})
		if push != nil && push.Created {
			created = 1
		}
		if pushErr != nil {
			return "", created, false, notes, pushErr
		}
		if push.UpToDate && !draftAwaitsPublish(push.DraftID, push.DraftPreexisted) {
			return applyNothingToPush, created, false, notes, nil
		}
		published, pubErr := runModulePublish(ctx, f, publishOptions{NoRequestReview: true})
		if pubErr != nil {
			return "", created, false, notes, pubErr
		}
		// ⚠️ REACHABLE without any review routing at all: a module created in a
		// SHARED feature is born a proposal, and `module publish` reports that
		// as submitted for review with a nil error — an admin approves it, and
		// until then no workflow can import it. Not live, so not applied.
		if published.Outcome == outcomeSubmittedForReview {
			return "the module is a proposal awaiting an admin or a maintainer of the feature rather than committed, so this folder is NOT live", created, true, notes, nil
		}
		return published.Detail, created, false, notes, nil

	default:
		push, pushErr := runPush(ctx, f, pushOptions{Validate: true, LiveFirst: true, Apply: true})
		if push != nil && push.Created {
			created = 1
		}
		if push != nil && push.RuntimePinned {
			notes = append(notes, runtimePinnedNote(push.RuntimeVersion))
		}
		if pushErr != nil {
			return "", created, false, notes, pushErr
		}
		if push.UpToDate && !draftAwaitsPublish(push.DraftID, push.DraftPreexisted) {
			return applyNothingToPush, created, false, notes, nil
		}
		published, pubErr := runPublish(ctx, f, publishOptions{NoRequestReview: true})
		if pubErr != nil {
			return "", created, false, notes, pubErr
		}
		if published.Outcome == outcomeSubmittedForReview {
			return "the draft was submitted for review by an admin or a maintainer of the feature rather than committed, so this folder is NOT live", created, true, notes, nil
		}
		return published.Detail, created, false, notes, nil
	}
}

// draftAwaitsPublish answers the ONE question
// applyFolder's up-to-date branch turns on: is there a draft here holding
// something the live row does not? On the apply path that is now exactly "did a
// draft pre-exist this push", and the reason is the LiveFirst option, not this
// function:
//
//   - a folder the LIVE row already holds — files and metadata — never reaches
//     a checkout: the push returns up to date with no DraftID, so nothing is
//     published and nothing is left behind — which is what closed BL-d513;
//   - so an up-to-date push that DID resolve a draft is one where live differs
//     from the folder. A draft this push checked out would then have been
//     written to, and the push would not be up to date — leaving the draft an
//     earlier run (or the author's own `push`) left open, which holds the
//     folder while live does not: its publish never landed. Publishing it is
//     the deploy, whatever the difference is — files, a title, an entrypoint,
//     a declared calendar, an access grant. The old files-only comparison
//     missed the metadata ones;
//   - a PARENTLESS draft pre-exists by construction (DraftID == the binding's
//     id) and has no live row at all: it awaits publication by definition.
//
// ⚠️ PRE-EXISTENCE ALONE IS SOUND ONLY BECAUSE LiveFirst ASKS LIVE WHETHER OR
// NOT A DRAFT IS OPEN. A single-folder `push` still checks a draft out (it is the
// classic check-out gesture `wf test` runs against) and
// one that wrote nothing leaves a copy of live behind; were the live comparison
// made only when no draft is open, that copy would arrive here pre-existing and
// up to date, and publishing it would mint an empty version on every apply
// after a classic push. That leg is mutation-tested by
// TestSyncApplyAfterAClassicPushThatWroteNothingMintsNoVersion.
//
// The pre-existence leg itself is, honestly, unreachable on the apply path
// today: mutating it out leaves the suite green, because an up-to-date push
// over a draft it checked out ITSELF needs workflowRowHoldsFolder (or its
// app and module twins) to call live different while the push then writes nothing
// to a copy of it — a parity break. It is kept because it is what the gate
// means, and because under a parity break it turns this run's empty publish
// into a leftover draft.
//
// One function for the three draft kinds, which differ only in their result
// type. For a module — which has ONE draft, so a pre-existing one may be a
// colleague's — LiveFirst already returned for a live module holding the
// folder, so an up-to-date push that reached a draft wrote to nothing because
// that draft already holds the folder, which live does not: publishing it is
// the deploy.
func draftAwaitsPublish(draftID string, preexisted bool) bool {
	return draftID != "" && preexisted
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
	// A metric is a row too, and a run that made one and reported none would
	// be the summary contradicting the organization. An ADOPTED one was not
	// made by this run — an earlier push made it and lost the recording — and
	// counting it would be the summary contradicting the organization the other
	// way. (A table reconciled after a timed-out create IS counted: that create
	// was this run's, and it committed.)
	for _, metric := range push.Metrics {
		if metric.Created && !metric.Adopted {
			n++
		}
	}
	return n
}

// adoptedPipelineMetrics lists the metric files whose create this push resolved
// by ADOPTING the metric already holding the name — creation units the plan
// counted, with a row behind them after all.
func adoptedPipelineMetrics(push *pipelinePushResult) map[string]string {
	if push == nil {
		return nil
	}
	var out map[string]string
	for _, metric := range push.Metrics {
		if metric.Adopted {
			if out == nil {
				out = map[string]string{}
			}
			out[metric.Path] = metric.MetricID
		}
	}
	return out
}

// pipelineBuildsNothing reports a pipeline folder with no table and no metric
// file — the case runPipelinePublish refuses as "nothing to publish", which
// for a folder of docs sidecars alone is every run.
func pipelineBuildsNothing(f *folder) (bool, error) {
	if len(f.Binding.Tables) > 0 {
		return false, nil
	}
	metrics, err := readMetricFiles(f.Root)
	if err != nil {
		return false, err
	}
	return len(metrics) == 0, nil
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

// countPipelineTagsApplied counts the metrics whose TAGS this push changed.
// Counted apart from countPipelinePushed, which counts .sql files: a tag change
// stages nothing and so is invisible to every count the publish leg reports.
func countPipelineTagsApplied(push *pipelinePushResult) int {
	n := 0
	for _, metric := range push.Metrics {
		if metric.Tags != nil && metric.Tags.Outcome == tagOutcomeApplied {
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
		summary.Claims += f.Claims
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
	for _, w := range r.Warnings {
		fmt.Fprintf(out, "\n  WARNING (%s): %s\n    %s\n", w.Reason, w.Path, indentContinuation(w.Detail, "      "))
	}

	if len(r.Folders) == 0 {
		fmt.Fprintf(out, "\n  No %s found under this directory.\n", wfdir.ManifestName)
		return
	}

	fmt.Fprintf(out, "\n  %d %s, in %s order\n", len(r.Folders), plural(len(r.Folders), "folder"), r.Order)
	listed := r.Folders
	if len(r.Sequence) == len(r.Folders) {
		byPath := map[string]syncApplyFolderReport{}
		for _, f := range r.Folders {
			byPath[f.Path] = f
		}
		listed = make([]syncApplyFolderReport, 0, len(r.Sequence))
		for _, path := range r.Sequence {
			listed = append(listed, byPath[path])
		}
	}
	for _, f := range listed {
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
			fmt.Fprintf(out, "             %s\n", indentContinuation(f.Detail, "             "))
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
			fmt.Fprintf(out, "             note       %s\n", indentContinuation(note, "                          "))
		}
	}

	if r.DryRun {
		// Conditional, all of it: nothing was applied or refused yet.
		fmt.Fprintf(out, "\n  %s — %d would apply, %d would be refused, %d not deployable\n",
			dryRunVerdict(r.Verdict), r.Summary.Applied, r.Summary.Refused, r.Summary.Unknown)
	} else {
		fmt.Fprintf(out, "\n  %s — %d applied, %d refused, %d not deployable\n",
			r.Verdict, r.Summary.Applied, r.Summary.Refused, r.Summary.Unknown)
	}
	if n := len(r.LocksRewritten); n > 0 {
		sort.Strings(r.LocksRewritten)
		fmt.Fprintf(out, "  %s changed in %d adopted %s (%s). %s records in Ronja, so a deploy never needs it, but a fresh checkout's `sync apply --dry-run` and `sync status` compare against it: commit it. If it conflicts in a merge, take either side, then run `ronja sync apply`: it rewrites the lock from Ronja, and until it does, the dry run and status read the side you took — which can name a deployed .sql file as never deployed, or plan rows as adopt.\n",
			wfdir.LockName, n, plural(n, "folder"), strings.Join(r.LocksRewritten, ", "), agree(n, "That folder keeps its", "Those folders keep their"))
	}
	if n := r.Summary.Claims; n > 0 && r.DryRun {
		fmt.Fprintf(out, "  %d %s would be recorded in Ronja for rows a lock names and Ronja has no record of; no row changes for %s.\n",
			n, plural(n, "claim"), agree(n, "it", "them"))
	}
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
// per-file kinds, the kind itself for the three with one unit each.
func applyUnitLabel(unit syncApplyUnitReport, kind string) string {
	if unit.To != "" {
		return unit.Path + " → " + unit.To
	}
	if unit.Path != "" {
		return unit.Path
	}
	if k, ok := wfdir.KindByName(kind); ok {
		return "the " + k.Label
	}
	if kind != "" {
		return "the " + kind
	}
	return "this folder"
}

// dryRunVerdict is the tree's verdict word in a dry run's summary line, where
// nothing has happened yet: "would apply", "would be refused". unknown stays
// as it is — it is already an answer about what could be told.
func dryRunVerdict(verdict string) string {
	switch verdict {
	case verdictApplied:
		return "would apply"
	case verdictRefused:
		return "would be refused"
	}
	return verdict
}
