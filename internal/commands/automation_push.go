package commands

import (
	"context"
	"errors"
	"fmt"
	"os"
	"slices"
	"sort"
	"strings"
	"time"

	"github.com/ronjatech/ronja-cli/internal/api"
	"github.com/ronjatech/ronja-cli/internal/wfdir"
	"github.com/spf13/cobra"
)

// Per-file push outcomes. These are the --json `outcome` values, so they are a
// contract for anything scripting the CLI.
const (
	automationOutcomeCreated   = "created"
	automationOutcomeUpdated   = "updated"
	automationOutcomeUnchanged = "unchanged"
	automationOutcomeDeleted   = "deleted"
	automationOutcomeRefused   = "refused"
	// automationOutcomePending is a file `pull` left for the next deploy: one
	// with no automation behind it yet, which a pull never creates.
	automationOutcomePending = "pending"
)

// `ronja automation push` makes each automation match the file that describes
// it.
//
// The order of operations is the design, and it is the pipeline loop's: every
// local guard first — they need no network and their diagnosis is entirely local
// — then one listing, then the files in a stable order with the per-file
// refusals before the first byte is written.
func newAutomationPushCmd() *cobra.Command {
	var force, prune bool
	cmd := &cobra.Command{
		Use:   "push",
		Short: "Make each automation match its file",
		Long: `Make each automation match its file.

For each .json file in the folder:

  1. its aliases are resolved against this stack's binds, field by field
  2. the automation is created, if this file has never been pushed
  3. otherwise only the DECLARED fields the row does not already hold are sent

A field a file does not mention is not managed by this folder and is never
touched, so adding a key is how you take a field over. That includes the
automation's "name": without it the automation keeps the name it was created
under (the filename), and a rename in the web app stands; with it, the name is
managed, and a rename made in the web app since this folder last agreed with it
is refused rather than reverted.

It refuses, rather than guessing, when:

  * the automation changed on the server since your last push — there is no
    version and no compare-and-set here, so this anchor is the only drift guard
    there is. --force overwrites
  * a file turns an automation back ON that somebody paused after your last push
  * a file changes the trigger kind, the event name, or whether the run is
    reference-based — the update route carries none of the three, so sending the
    change would answer 200 for a write that never happened
  * a file this folder is bound to is gone. Declare what happened in ronja.json:
    "moved": [{"from": "old.json", "to": "new.json"}] for a rename (the same
    automation, name unchanged; needs "formatVersion": 5), or "removed":
    ["old.json"] to stop managing it and leave it running. --prune deletes
    those automations instead; without one of the three a bad rebase cannot
    silently stop one. --prune obeys the
    drift guard too — a row somebody has been editing is the last one to delete
    because a file went missing`,
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			resolved, err := resolveInstance()
			if err != nil {
				return err
			}
			f, err := openFolderForDeploy(cmd.Context(), resolved, wfdir.AutomationKind)
			if err != nil {
				return err
			}
			result, err := runAutomationPush(cmd.Context(), f, automationPushOptions{Force: force, Prune: prune})
			noteWrittenAutomationsManagedBy(cmd.Context(), f, result)
			// Emitted even on failure: a push that stopped part-way has really
			// created and updated automations, and "which ones" is the first
			// thing anybody needs to know.
			printManagedNotices(f.notices)
			if result != nil {
				result.ManagedInGit = f.notices
				if flagJSON {
					if emitErr := emitJSON(result); emitErr != nil {
						return emitErr
					}
				} else {
					printAutomationPushReport(result)
				}
			}
			return err
		},
	}
	cmd.Flags().BoolVar(&force, "force", false,
		"push even though an automation changed on the server since your last push")
	cmd.Flags().BoolVar(&prune, "prune", false,
		"delete the automations whose files are gone from this folder")
	return cmd
}

type automationPushOptions struct {
	Force bool
	Prune bool
	// RequireAnchor is `sync apply`'s, on an ADOPTED folder: a bound row this
	// folder holds no drift anchor for is written only when the row read at
	// WRITE time already equals the file (the anchor is then recorded and
	// nothing else is sent), and is otherwise refused no_drift_anchor. Apply's
	// plan answers the same question earlier (relieveAnchorlessAtDesired), but
	// the write comes after every earlier folder has run, and with no anchor
	// the drift and re-enable guards are both disarmed — so a pause made in
	// that window would be reverted by a push the plan had cleared. The single
	// folder push leaves it off: its write is how a folder earns an anchor.
	RequireAnchor bool
	// Pull is `ronja automation pull`'s anchor step: a file with no automation
	// behind it is left for the next deploy (automationOutcomePending) rather
	// than created — a pull writes no row.
	Pull bool
}

// automationPushResult is the --json shape and the human renderer's input, so
// the two cannot describe different things.
type automationPushResult struct {
	FeatureID string `json:"featureID,omitempty"`
	// Target names the instance and organization this push landed in, so a
	// mistake is visible where it happens rather than only from a later status.
	Target string                 `json:"target,omitempty"`
	Files  []automationFileResult `json:"files"`
	// Moved and Removed are what this folder's `moved` / `removed` declarations
	// did to the selected stack's lock — see pipelinePushResult's.
	Moved   []addressMove    `json:"moved,omitempty"`
	Removed []addressRemoval `json:"removed,omitempty"`
	// UpToDate reports a push that found nothing to do.
	UpToDate bool   `json:"upToDate"`
	Error    string `json:"error,omitempty"`
	// ManagedInGit is every automation this push wrote whose `managedBy` names a
	// repository folder other than this one (managed_notice.go).
	ManagedInGit []managedNotice `json:"managedInGit,omitempty"`
}

// automationFileResult is what happened to one file.
type automationFileResult struct {
	Path         string `json:"path"`
	AutomationID string `json:"automationID,omitempty"`
	Name         string `json:"name,omitempty"`
	Outcome      string `json:"outcome"`
	Created      bool   `json:"created,omitempty"`
	// Changed names the fields this push wrote, or would have written.
	Changed []string `json:"changed,omitempty"`
	Error   string   `json:"error,omitempty"`
	Notes   []string `json:"notes,omitempty"`

	// infrastructural records that this file's write failed for a reason that has
	// nothing to do with the file — a transport error, a 429, or a 5xx. Not
	// serialized: it is a signal to the loop, and by the time a reader sees the
	// report the Error string already says what happened.
	infrastructural bool
	// drift marks a refusal for drift — what `ronja <kind> pull` takes into an
	// adopted folder — so the run's summary can name it.
	drift bool
}

func (r *automationFileResult) note(format string, args ...any) {
	r.Notes = append(r.Notes, fmt.Sprintf(format, args...))
}

// automationWriteIsInfrastructural reports a failure the NEXT file will hit too:
// a rate limit, a restarting instance, or a connection that never landed.
//
// Every other refusal on this surface is a property of one file, which is why
// this loop treats failure as data and keeps going. These are not: 40 files
// against an instance mid-deploy would otherwise produce 40 failed writes at
// full speed and a half-applied folder, and against a rate limit they would
// deepen the very backlog they are waiting on. Status 0 covers a transport
// error, which reaches here as no HTTP status at all.
//
// ⚠️ A 4xx IS NOT ON THIS LIST, AND A 403 ESPECIALLY IS NOT. The mailbox gate
// refuses per FILE — some files in a folder declare a mailbox and some do not —
// so aborting on one would stop pushing the files that were going to land, which
// is the opposite of what this exists for.
func automationWriteIsInfrastructural(err error) bool {
	status := api.StatusOf(err)
	return status == 0 || status == 429 || status >= 500
}

// automationOrphanError is the refusal a file that left the folder earns, in the
// one place the push and `sync apply`'s local pre-flight both read it from.
//
// It names the DECLARED ways through first, because they are the ones that
// leave the automation running: a "moved" entry for a rename, a "removed" entry
// to stop managing it. --prune is the third, and the only one that deletes.
func automationOrphanError(gone []goneAddress) error {
	lines := make([]string, 0, len(gone))
	for _, g := range gone {
		lines = append(lines, describeGone(g)+".")
	}
	return fmt.Errorf("%d %s bound to an automation and %s gone from this folder:\n    %s\n  Those automations are LIVE and this push leaves them alone. A file lost to a bad rebase must not silently stop one, so what happened has to be said. Restore the files if they went missing; to DELETE the automations instead, `ronja automation push --prune`.\n  ⚠️ --prune is a 30-day SOFT delete that also pauses the row, and this CLI cannot empty that trash (purging is a human-only route in the web app). A \"removed\" entry deletes nothing.",
		len(gone), plural(len(gone), "file"), agree(len(gone), "is", "are"),
		strings.Join(lines, "\n    "))
}

// refuseBoundAutomationsWithNoFeature is runAutomationPush's step-5 refusal, in
// the one place it and `sync apply`'s local pre-flight both read it from.
//
// ⚠️ It is NOT step 4's create refusal in another guise, and that is why the two
// are separate. A folder that creates nothing reaches step 5 with an empty
// featureID quite legally — the lock deliberately stores none — and
// ListAutomations("") filters every row away rather than failing, which the loop
// then reads as "every automation is gone".
func refuseBoundAutomationsWithNoFeature(f *folder) error {
	if len(f.Binding.Automations) == 0 || f.Binding.FeatureID != "" {
		return nil
	}
	return fmt.Errorf("this folder is bound to %d %s here, and nothing can be compared against without knowing which feature holds them, so nothing was pushed.\n  %s",
		len(f.Binding.Automations), plural(len(f.Binding.Automations), "automation"),
		automationFeatureAdvice(f))
}

func runAutomationPush(ctx context.Context, f *folder, opts automationPushOptions) (*automationPushResult, error) {
	client := newClient(f.Resolved.URL, f.Resolved.Token)
	result := &automationPushResult{
		FeatureID: f.Binding.FeatureID,
		Target:    describeTarget(f.Resolved),
		Files:     []automationFileResult{},
	}

	// --- 1. Local guards. Nothing here needs the network. -------------------
	local, enumeration, err := readAutomationFiles(f.Root)
	if err != nil {
		return nil, err
	}
	for _, s := range enumeration.Skipped {
		fmt.Fprintf(os.Stderr, "  Note: skipping %s — %s\n", s.Path, s.Reason)
	}
	if err := checkPushable(local, wfdir.AutomationKind, maxFileBytes); err != nil {
		return nil, err
	}
	// EVERY file is parsed before ANY of them is sent. A push that created four
	// automations and then stopped on a typo in the fifth leaves a feature in a
	// state nobody asked for, and the typo was readable before the first request.
	parsed, problems := parseAutomationFolder(local)
	if len(problems) > 0 {
		var lines []string
		for _, path := range sortedKeys(problems) {
			lines = append(lines, fmt.Sprintf("%s: %v", path, problems[path]))
		}
		return nil, fmt.Errorf("these files are not valid automation declarations, so nothing was pushed:\n    %s",
			strings.Join(lines, "\n    "))
	}
	aliases := checkAliases(f.Manifest, f.selection(), f.Codec, local,
		folderStems(f.Kind, local), folderFieldRefs(f.Kind, f.Root, local))
	if err := aliases.err(); err != nil {
		return nil, err
	}
	noteAliasWarnings(aliases)

	// Resolved BEFORE anything is sent, for the same reason the parse is: an
	// alias with no bind is a refusal the folder could have earned without a
	// credential, and earning it half way through a push is how a feature ends
	// up with three of five automations.
	resolvedFiles := make(map[string]*automationFile, len(parsed))
	var unresolved []string
	for _, path := range sortedPaths(local) {
		file := *parsed[path]
		if err := resolveAutomationRefs(&file, f.Codec, f.Stack, f.Binding.FeatureID); err != nil {
			unresolved = append(unresolved, fmt.Sprintf("%s:\n    %v", path, err))
			continue
		}
		resolvedFiles[path] = &file
	}
	if len(unresolved) > 0 {
		return nil, fmt.Errorf("these references do not resolve, so nothing was pushed:\n  %s",
			strings.Join(unresolved, "\n  "))
	}

	// --- 2. A file that left the folder is not an automation this deletes ----
	//
	// The ADDRESS declarations first: a `moved` entry re-keys this stack's
	// recorded automation to its new file, and a `removed` one stops managing it,
	// so neither path is an orphan below. In memory; written back once every
	// local guard has passed.
	addresses, err := f.resolveAddresses()
	if err != nil {
		return nil, err
	}
	result.Moved, result.Removed = addresses.Moves, addresses.Removals
	f.noteAddresses()
	orphans := automationOrphans(f.Binding, local)
	if len(orphans) > 0 && !opts.Prune {
		gone, err := f.goneAddresses()
		if err != nil {
			return nil, err
		}
		return nil, automationOrphanError(gone)
	}

	// --- 3. The authority a mailbox needs, said before it is needed ---------
	if err := checkAutomationMailboxAuthority(ctx, client, resolvedFiles); err != nil {
		return nil, err
	}
	// --- 3b. The WRITE GATE: a table in an agent action's reference set is one
	// its headless run may write, so it must be this stack's own or bound here
	// as a write source. Every file, before any is sent (write_gate.go).
	if err := gateAutomationReferences(ctx, client, f, resolvedFiles); err != nil {
		return nil, err
	}

	// --- 4. The local decision, per file ------------------------------------
	//
	// Create or update, and the refusal a create earns when this folder names no
	// feature. Taken through applyDecisionsFor so a tree-wide apply and its
	// dry-run read the same answer this loop acts on; nothing here needs a
	// request. See sync_decision.go.
	//
	// ⚠️ Its third answer — applyActionUnanchored, a bound row this folder has no
	// anchor for — is deliberately NOT acted on here. This loop has always
	// pushed such a file with both guards disarmed, and its write is what RECORDS
	// the anchor; refusing it here would leave the folder with no way to earn
	// one. Whether a tree-wide apply may write it is `sync apply`'s decision, not
	// a side effect of moving where the create fork is spelled.
	decisions := applyDecisionsFor(f, sortedPaths(local))
	if err := errFromDecisions(decisions); err != nil {
		return nil, err
	}
	// Every local guard has passed: the re-keyed addresses are recorded before
	// the first request, so the writes below land under the NEW paths.
	if err := f.saveAddresses(); err != nil {
		return nil, fmt.Errorf("record the moved addresses in %s: %w", f.addressRecordFile(), err)
	}
	creating := false
	for _, d := range decisions {
		if d.Creates() {
			creating = true
		}
	}
	// A create WRITES a binding, and a binding must name its organization
	// authoritatively — an environment token has none resolved until it asks.
	if creating && !f.Bound {
		if _, err := f.bindNew(ctx); err != nil {
			return nil, err
		}
	}

	// --- 5. One listing, and a truncated one is a refusal -------------------
	//
	// ⚠️ THE FEATURE IS DEMANDED HERE TOO, not only by step 4. A manifest without
	// "featureID" is legal and the lock deliberately stores none, so a folder that
	// creates nothing reaches this with an empty one — and ListAutomations("")
	// filters every row away rather than failing, which this loop then reads as
	// "every automation is gone": each bound file is refused as a broken binding
	// naming an empty feature, and with --prune the drift guard finds no row for
	// any orphan and deletes each one unguarded. `status` already refuses this
	// case with automationFeatureAdvice, so the same advice is given here rather
	// than a second wording of the same missing key.
	if err := refuseBoundAutomationsWithNoFeature(f); err != nil {
		return nil, err
	}
	rows := map[string]*api.Automation{}
	if len(f.Binding.Automations) > 0 {
		listed, err := client.ListAutomations(ctx, f.Binding.FeatureID)
		var truncated *api.ErrAutomationListTruncated
		switch {
		case errors.As(err, &truncated):
			// ⚠️ NOT a short list and NOT "no automations". A listing that saw a
			// prefix of the feature cannot say which rows are missing, and a row
			// this push cannot see reads as gone — which is exactly how a second
			// automation gets created beside a live one.
			return nil, fmt.Errorf("nothing was pushed: %v", truncated)
		case err != nil:
			return nil, fmt.Errorf("read the automations of %s: %w", f.Binding.FeatureID, err)
		}
		rows = automationRowsByID(listed)
	}

	// --- 6. The files, in a stable order ------------------------------------
	failed, attempted := 0, 0
	stopped := ""
	for _, path := range sortedPaths(local) {
		// One Ctrl-C, one message. Without this every remaining file makes its
		// own doomed request and prints its own refusal.
		if ctx.Err() != nil {
			result.Error = interruptedMessage
			return result, errors.New(interruptedMessage)
		}
		file := pushOneAutomation(ctx, client, f, path, resolvedFiles[path], rows, opts)
		result.Files = append(result.Files, file)
		attempted++
		// Saved after EVERY file, success or failure: a push that dies half way
		// has really created automations, and a folder that does not know their
		// ids creates them again on the retry.
		if err := f.saveFolder(); err != nil {
			fmt.Fprintf(os.Stderr, "  Note: could not record what was pushed in %s (%v) — the next push may create a duplicate.\n",
				f.bindingFiles(), err)
		}
		if file.Outcome == automationOutcomeRefused {
			failed++
		}
		if file.infrastructural {
			stopped = path
			break
		}
	}

	// --- 7. The prune, last, so it cannot cost a file that was going to push -
	if stopped == "" {
		for _, path := range orphans {
			file := pruneOneAutomation(ctx, client, f, path, rows, opts)
			result.Files = append(result.Files, file)
			attempted++
			if err := f.saveFolder(); err != nil {
				fmt.Fprintf(os.Stderr, "  Note: could not record the prune in %s (%v).\n", f.bindingFiles(), err)
			}
			if file.Outcome == automationOutcomeRefused {
				failed++
			}
			if file.infrastructural {
				stopped = path
				break
			}
		}
	}

	result.UpToDate = failed == 0 && !anyAutomationWritten(result.Files)
	if stopped != "" {
		// ⚠️ STOPPED, not "these files failed". The remaining ones were never
		// attempted, and reporting them as unattempted is the difference between
		// a folder somebody re-pushes and a folder somebody starts debugging.
		// Every write so far is recorded in the lock — saveFolder runs after each
		// one — so the retry is the whole command again, not a repair.
		left := len(local) + len(orphans) - attempted
		result.Error = fmt.Sprintf("stopped at %s, and %d further %s not attempted. That failure is the instance rather than the folder — a rate limit, a restart, or a connection that did not land — and sending the rest into it would only deepen it. Every write so far is recorded in %s, so the retry is this same push, not a repair",
			stopped, left, plural(left, "file"), f.bindingFiles())
		return result, fmt.Errorf("%s", result.Error)
	}
	if failed > 0 {
		result.Error = fmt.Sprintf("%d of %d file(s) did not land", failed, len(result.Files))
		if hint := f.pullHint(""); hint != "" && slices.ContainsFunc(result.Files, func(r automationFileResult) bool { return r.drift }) {
			result.Error += fmt.Sprintf(" — what changed on the server since this folder last applied it is taken into the folder with `%s pull` (then commit it)", f.Kind.Command)
		}
		return result, fmt.Errorf("%s", result.Error)
	}
	return result, nil
}

func anyAutomationWritten(files []automationFileResult) bool {
	for _, file := range files {
		if file.Outcome != automationOutcomeUnchanged {
			return true
		}
	}
	return false
}

// pushOneAutomation runs the whole write for one file.
//
// Failure is DATA, not an error return: the caller records what happened, moves
// on, and exits non-zero at the end. A push of nine automations that stopped
// dead on the third would leave six perfectly pushable files unattempted.
func pushOneAutomation(ctx context.Context, client *api.Client, f *folder, path string,
	file *automationFile, rows map[string]*api.Automation, opts automationPushOptions) automationFileResult {

	// The name a CREATE stamps: the declared one when the file has it, the
	// filename's stem otherwise. After the create an undeclared name is never
	// written again (see automationDelta).
	name := automationNameFor(path)
	if file.Name != nil {
		name = *file.Name
	}
	out := automationFileResult{Path: path, Name: name, AutomationID: f.Binding.Automations[path]}
	refuse := func(format string, args ...any) automationFileResult {
		out.Outcome = automationOutcomeRefused
		out.Error = fmt.Sprintf(format, args...)
		fmt.Fprintf(os.Stderr, "  Refused: %s — %s\n", path, out.Error)
		return out
	}
	// refuseWrite is refuse for a failure the SERVER returned, so the loop can
	// tell "this file is wrong" from "this instance is not answering".
	refuseWrite := func(err error, format string, args ...any) automationFileResult {
		res := refuse(format, args...)
		res.infrastructural = automationWriteIsInfrastructural(err)
		return res
	}

	// --- Create, for a file with no automation behind it yet ----------------
	if out.AutomationID == "" && opts.Pull {
		out.Outcome = automationOutcomePending
		out.Error = "not deployed yet; the next apply creates it (with --allow-create)"
		return out
	}
	if out.AutomationID == "" {
		create := automationCreateInput(f.Binding.FeatureID, name, file)
		// The address this folder claims, in the create's own transaction — nil
		// (no key) for a folder with no id.
		create.Managed = f.managedClaim(path)
		row, err := client.CreateAutomation(ctx, create)
		if id, adopted, refusal := f.adoptOnAddressClaimed(ctx, path, err); err != nil && adopted {
			// Another apply created this address's automation at the same moment
			// and its claim won: that row is this file's, adopted by id, and the
			// file continues below as the update it now is.
			if refusal != nil {
				return refuse("%v", refusal)
			}
			out.AutomationID = id
			if _, known := rows[id]; !known {
				if adoptedRow, gerr := client.GetAutomation(ctx, id); gerr == nil {
					rows[id] = adoptedRow
				}
			}
			err, row = nil, nil
		}
		if err != nil {
			// ⚠️ A timeout is NOT reconciled here, unlike a table create, and the
			// difference is the surface rather than a decision: scheduled_jobs.name
			// is neither unique nor required, so there is no way to ask "did the
			// automation I meant to create land?" — only "does the feature hold
			// something called that", which is a different question with a
			// different answer whenever two automations share a name. Saying so is
			// the honest move; guessing would adopt the wrong row.
			return refuseWrite(err, "creating it in feature %s failed: %v\n    If the request timed out, look at the feature in the web app before pushing again — an automation's name is not unique, so nothing here can tell a landed create from a lost one, and the next push would make a second one",
				f.Binding.FeatureID, err)
		}
		if row != nil {
			out.AutomationID, out.Created = row.ID, true
			out.Outcome = automationOutcomeCreated
			recordAutomation(f, path, row)
			recordAutomationName(f, path, file, row)
			noteAutomationWarnings(&out, row)
			return out
		}
	}

	row, present := rows[out.AutomationID]
	if !present {
		return refuse("binding broken: %s is not in feature %s any more (deleted, moved, or you lost access to it).\n    Remove this file's entry from %s to create a new automation for it, or point it at the right id",
			out.AutomationID, f.Binding.FeatureID, wfdir.LockName)
	}
	// A file that declares no name leaves the row's name alone, so the report
	// names the automation by the name it HAS — the filename is only what a
	// create would have stamped. A declared name is what the push converges to.
	if file.Name == nil {
		out.Name = row.Name
	}

	// --- The three refusals, in the order whose MESSAGE is most specific ----
	if err := refuseUnpatchableAutomation(file, row); err != nil {
		return refuse("%v", err)
	}
	_, seen := f.Lock.AutomationSeen(f.Stack, path)

	// LIVE EQUALS DESIRED IS NEVER DRIFT, and it is asked FIRST. A row that
	// already holds every field this file declares has nothing to overwrite, so
	// there is nothing for the drift anchor to protect — and refusing it would
	// refuse this folder's own work whenever the anchor fell behind a write that
	// DID land (the anchor is recorded in a separate step from the write). The
	// anchor is re-recorded from the row just read, which is exactly what it
	// records: the moment this folder last agreed with the row.
	changed, patch := automationDelta(file, row)
	out.Changed = changed
	if patch.Empty() {
		out.Outcome = automationOutcomeUnchanged
		// A LEGACY instances[] folder is a no-op here, guarded inside
		// SetAutomationSeen: its bindings live in the manifest and a lock keyed by
		// stack name has nowhere to put its anchor. recordAutomation says that out
		// loud on the paths that WRITE; nothing was written here.
		f.Lock.SetAutomationSeen(f.Stack, path, row.ID, row.Stamp())
		recordAutomationName(f, path, file, row)
		return out
	}

	// No anchor, and something to write: refused when the caller requires an
	// anchor (see RequireAnchor). Decided HERE, on the row this push just read,
	// never on an earlier read — the window between the two is exactly where a
	// pause made by an operator would be written over with both guards off.
	if opts.RequireAnchor && seen == "" {
		detail := fmt.Sprintf("this folder holds no drift anchor for automation %s, and the row now differs from the file in %s — with no anchor a write would overwrite whatever moved on the server, and would turn the row back on if somebody had paused it. Push this folder on its own (`ronja automation push`) to write it and record the anchor, or bring the file in line with the row",
			out.AutomationID, strings.Join(changed, ", "))
		f.noteCreateRefusal(&managedRefusal{Reason: syncReasonNoDriftAnchor, Detail: detail})
		return refuse("%s", detail)
	}
	if err := automationReEnableGuard(file, row, seen); err != nil {
		if !opts.Force {
			out.drift = true
			return refuse("%v%s", err, f.pullHint("    "))
		}
		// --force is an explicit choice, and this is the one it is worth being
		// loud about: it turns an automation somebody paused back on.
		fmt.Fprintf(os.Stderr, "  Note: --force — %s: %v\n", path, err)
	}
	if drift := automationDrift(seen, row); drift == driftChanged {
		if !opts.Force {
			out.drift = true
			return refuse("this automation changed on the server since your last push (it was last written %s, and this folder last agreed with %s).\n    There is no version history and no compare-and-set here, so a push would overwrite whatever moved without knowing what it was. Run `ronja automation status` for the detail, pull the change into the file, or push --force to overwrite it%s",
				row.UpdatedAt.UTC().Format(time.RFC3339), seen, f.pullHint("    "))
		}
		fmt.Fprintf(os.Stderr, "  Note: --force — %s changed on the server since your last push; overwriting.\n", path)
	}
	// The NAME's own three arms (name_attr.go), for a file that declares one.
	// The anchor above usually catches a rename made in the web app first — any
	// write moves updatedAt — but not on a folder with no anchor, and its message
	// would not say it was the name. The baseline is the name this folder last
	// recorded, else the filename, which is what every push before the key
	// existed wrote.
	nameBase := nameBaseline{Recorded: f.Lock.AutomationName(f.Stack, path), Stem: automationNameFor(path)}.withCarried(f.Lock.AutomationStem(f.Stack, path))
	if decideName(file.Name, row.Name, nameBase) == nameDrift {
		reason := nameDriftReason("the automation", row.Name, *file.Name, nameBase)
		if !opts.Force {
			out.drift = true
			return refuse("%s.\n    Set \"name\" in the file to what it is now, remove the key to stop managing it, or push --force to rename it back%s", reason, nameRemedy(f, "automation push", "    "))
		}
		fmt.Fprintf(os.Stderr, "  Note: --force — %s: %s; renaming it back.\n", path, reason)
	}

	updated, err := client.UpdateAutomation(ctx, out.AutomationID, patch)
	if err != nil {
		// ⚠️ REPORTED AT THE MOMENT IT HAPPENS, because afterwards it is not
		// attributable. The row update and the reference / action writes are
		// SEPARATE server-side calls, so a failure at the second leaves the new
		// schedule with the old references — a state indistinguishable, a day
		// later, from somebody editing references in the web app.
		return refuseWrite(err, "%v%s", err, diagnoseAutomationHalfApply(ctx, client, out.AutomationID, file, changed))
	}
	out.Outcome = automationOutcomeUpdated
	recordAutomation(f, path, updated)
	recordAutomationName(f, path, file, updated)
	noteAutomationWarnings(&out, updated)
	// The write is VERIFIED against the row it returned. A field the server
	// accepted and did not store answers 200 and changes nothing — the exact
	// class of bug the two spelling mismatches on this surface produce — and
	// without this the folder would report it as pushed for ever.
	if left, _ := automationDelta(file, updated); len(left) > 0 {
		out.note("the server accepted the update and the row still differs in %s — the write was reported as successful, so this is worth looking at rather than re-pushing",
			strings.Join(left, ", "))
	}
	return out
}

// recordAutomation writes what this folder now knows about one path: which row
// it is, and the moment it last agreed with it.
//
// The binding goes first and the anchor second, because SetBinding preserves an
// existing LockAutomation only when the id matches — writing the anchor first
// would have it dropped by the binding write on a first create.
func recordAutomation(f *folder, path string, row *api.Automation) {
	f.recordBinding(f.Binding.WithAutomation(path, row.ID))
	if f.Stack == "" {
		// A LEGACY instances[] folder has nowhere committed to keep the anchor —
		// the lock file is keyed by stack name and such a folder has none — so
		// the drift and re-enable guards stay disarmed for it. Said out loud
		// once, because "disarmed" reads as "passed" in every report.
		fmt.Fprintf(os.Stderr, "  Note: %s names no stack, so there is nowhere to record when this folder last agreed with %s — the drift guard stays off. Name a stack with --stack <name> to arm it.\n",
			wfdir.ManifestName, row.ID)
		return
	}
	f.Lock.SetAutomationSeen(f.Stack, path, row.ID, row.Stamp())
}

// recordAutomationName records the name a row holds as the one this folder agrees
// with — ONLY for a file that declares one, and only when the row really holds
// it. An undeclared name is unmanaged, and recording whatever the row is called
// would turn a rename made in the web app while nobody managed the name into
// this folder's baseline the day somebody adds the key.
func recordAutomationName(f *folder, path string, file *automationFile, row *api.Automation) {
	if file == nil || file.Name == nil || row == nil || !sameName(row.Name, *file.Name) {
		return
	}
	if sameName(f.Lock.AutomationName(f.Stack, path), row.Name) {
		return
	}
	f.Lock.SetAutomationName(f.Stack, path, row.ID, row.Name)
}

// noteAutomationWarnings carries the server's non-blocking advisories about a
// write that SUCCEEDED onto the file's result.
//
// They are a property of the WRITE and never of the row — absent on get and list
// — so they have to be reported at the moment they arrive or not at all.
func noteAutomationWarnings(out *automationFileResult, row *api.Automation) {
	for _, warning := range row.Warnings {
		out.note("%s", warning)
	}
	if row.WebhookSigningSecret != "" {
		// Its ONLY appearance anywhere. There is no route that reads it back and
		// clearing one is human-only, so a caller that does not write it down now
		// cannot get it back.
		out.note("webhook signing secret (shown once, and never again): %s", row.WebhookSigningSecret)
	}
}

// automationReEnableGuard refuses a file that turns an automation back ON after
// somebody paused it.
//
// The case: an operator pauses a runaway email-triggered automation at 02:00
// (disabled_reason='user'), and a folder declaring enabled:true — a line added
// months earlier to stop `status` reporting drift on an unmanaged field — is
// pushed by CI on the next merge. The pause is CLEARABLE (a `user` reason is one
// of the three CallerMayClearDisabledReason admits), so without a gate the
// automation is live again with nothing in the output reading as "I just
// reversed an incident response".
//
// ⚠️ It is NOT a second line of defence, and its doc must not pretend otherwise.
// `updatedAt` moves on any write, so a row somebody paused has moved, and the
// ordinary drift refusal already catches it. What this earns is the MESSAGE: a
// reader who is told "somebody disabled this, and here is the reason they gave"
// acts differently from one told "the row changed". Its one exclusive case — a
// folder that never recorded an anchor — is a case where it is disarmed anyway.
func automationReEnableGuard(file *automationFile, row *api.Automation, seen string) error {
	if file.Enabled == nil || !*file.Enabled || row.Enabled || seen == "" {
		return nil
	}
	// ⚠️ time.Parse, never a string comparison. The anchor is RFC3339Nano, which
	// trims trailing zeros, so a fraction-less stamp sorts AFTER a fractional one
	// and `Z` sorts above `.` — a string `>` here would be wrong intermittently,
	// which is the worst way for a guard nobody re-runs to be wrong.
	anchor, err := time.Parse(time.RFC3339Nano, seen)
	if err != nil || !row.UpdatedAt.After(anchor) {
		return nil
	}
	who := "somebody"
	switch row.DisabledReason {
	case "user":
		who = "a person"
	case "":
		// The row is off and nothing said why. Still worth refusing: the reason
		// column is only written by the paths that set it, and an absent one is
		// not evidence that nobody meant it.
	default:
		who = row.DisabledReason
	}
	return fmt.Errorf("this file declares enabled:true and the automation is OFF — %s disabled it at %s, after this folder last agreed with it (%s).\n    Pushing would turn it back on with nobody in the loop. Set \"enabled\": false to match, remove the key to stop managing it at all, or push --force if turning it on is what you mean",
		who, row.UpdatedAt.UTC().Format(time.RFC3339), seen)
}

// diagnoseAutomationHalfApply asks the row what actually landed after an update
// failed, and renders the answer as a sentence appended to the refusal.
//
// It costs one GET, ONLY on the failure path, and it is the difference between
// "the update failed" and "the schedule landed and the references did not" —
// which is the state §5.4 warns about and the one nobody can reconstruct later.
// A read that fails degrades to the uncertain wording rather than claiming
// either half.
func diagnoseAutomationHalfApply(ctx context.Context, client *api.Client, id string,
	file *automationFile, wanted []string) string {

	const uncertain = "\n    ⚠️ The row and its references are written by SEPARATE server-side calls, so this may have landed in part — the new schedule with the old references, say. Run `ronja automation status` before pushing again."
	row, err := client.GetAutomation(ctx, id)
	if err != nil || row == nil {
		return uncertain
	}
	left, _ := automationDelta(file, row)
	stillOff := map[string]bool{}
	for _, field := range left {
		stillOff[field] = true
	}
	var landed []string
	for _, field := range wanted {
		if !stillOff[field] {
			landed = append(landed, field)
		}
	}
	if len(landed) == 0 {
		return "\n    Nothing landed: the row still differs in every field this push meant to write."
	}
	sort.Strings(landed)
	if len(left) == 0 {
		return "\n    ⚠️ Every field this push meant to write DID land, and the failure came after it — so the automation now matches the file despite the error."
	}
	sort.Strings(left)
	return fmt.Sprintf("\n    ⚠️ THIS UPDATE HALF-APPLIED. %s now matches the file; %s does not. The row and its references are written by separate server-side calls, and a day from now this is indistinguishable from somebody editing it in the web app.",
		strings.Join(landed, ", "), strings.Join(left, ", "))
}

// pruneOneAutomation deletes the automation behind a file that is gone, and
// drops the binding with it.
//
// ⚠️ IT RESPECTS THE DRIFT ANCHOR, exactly as an update does, and the argument is
// stronger here than there: "this row moved since you last agreed with it" is a
// weaker reason to refuse an edit than to refuse a DELETE. A file lost to a bad
// rebase plus a row somebody has been editing in the web app is the pairing that
// makes --prune expensive, and it is the pairing this catches. --force overrides
// it, as it does every other refusal in this loop.
//
// A row the listing did not return is deleted without the check: there is
// nothing to compare against, and the DELETE answers for itself.
func pruneOneAutomation(ctx context.Context, client *api.Client, f *folder, path string,
	rows map[string]*api.Automation, opts automationPushOptions) automationFileResult {

	id := f.Binding.Automations[path]
	out := automationFileResult{Path: path, Name: automationNameFor(path), AutomationID: id}
	if row, present := rows[id]; present && !opts.Force {
		_, seen := f.Lock.AutomationSeen(f.Stack, path)
		if automationDrift(seen, row) == driftChanged {
			out.Outcome = automationOutcomeRefused
			out.Error = fmt.Sprintf("this automation changed on the server since your last push (it was last written %s, and this folder last agreed with %s), and --prune would DELETE it.\n    A row somebody has been working on is the last one to delete on the strength of a missing file. Look at it in the web app, restore %s if the deletion was a bad rebase, or push --prune --force to delete it anyway",
				row.UpdatedAt.UTC().Format(time.RFC3339), seen, path)
			fmt.Fprintf(os.Stderr, "  Refused: %s — %s\n", path, out.Error)
			return out
		}
	}
	reason := fmt.Sprintf("%s was removed from the automation folder %s and pushed with --prune", path, f.Manifest.Title)
	if err := client.DeleteAutomation(ctx, id, reason); err != nil {
		out.Outcome = automationOutcomeRefused
		out.Error = fmt.Sprintf("deleting %s failed: %v", id, err)
		out.infrastructural = automationWriteIsInfrastructural(err)
		fmt.Fprintf(os.Stderr, "  Refused: %s — %s\n", path, out.Error)
		return out
	}
	out.Outcome = automationOutcomeDeleted
	out.note("this is a 30-day SOFT delete: the automation stops firing and is paused in the trash, which this CLI cannot empty — purging is a human-only route in the web app")
	f.recordBinding(f.Binding.WithoutAutomation(path))
	return out
}

// checkAutomationMailboxAuthority refuses a push that needs mailbox privileges
// this caller does not have, BEFORE anything is written.
//
// A mailbox trigger, or a mailbox-kind reference, requires USR_ADMIN and an
// admin-scoped (admin:write) token, while this loop's own scope is `automation`.
// Left to the server, the refusal arrives as a 403 on the fourth of nine files,
// with the first three already written.
//
// ⚠️ ONLY THE ROLE IS CHECKED, and the split is stated rather than papered over:
// /me answers what role this credential carries, and nothing answers what SCOPES
// a token was minted with. So an admin gets a note naming the scope rather than
// a claim that it was verified — and a lookup that fails degrades to letting the
// server answer, since "I could not ask" is not grounds to refuse a push.
func checkAutomationMailboxAuthority(ctx context.Context, client *api.Client, files map[string]*automationFile) error {
	needs := map[string]string{}
	for path, file := range files {
		if why := automationMailboxAuthority(file); why != "" {
			needs[path] = why
		}
	}
	if len(needs) == 0 {
		return nil
	}
	paths := sortedKeys(needs)
	me, err := client.Me(ctx)
	if err != nil {
		fmt.Fprintf(os.Stderr, "  Note: %s %s a mailbox, which needs the admin role and an admin-scoped token; this could not be checked (%v), so the server will answer.\n",
			strings.Join(paths, ", "), agree(len(paths), "declares", "declare"), err)
		return nil
	}
	if !isAdmin(me) {
		var lines []string
		for _, path := range paths {
			lines = append(lines, fmt.Sprintf("%s declares %s", path, needs[path]))
		}
		return fmt.Errorf("nothing was pushed:\n    %s\n  Binding a mailbox needs the ADMIN role and an admin-scoped (admin:write) token, and this credential is not an admin. Ask an admin to push these files, or drop the mailbox from them",
			strings.Join(lines, "\n    "))
	}
	fmt.Fprintf(os.Stderr, "  Note: %s %s a mailbox — that needs an admin-scoped (admin:write) token as well as the admin role you have, and nothing here can read a token's scopes. A 403 from the server is what a scope-limited token looks like.\n",
		strings.Join(paths, ", "), agree(len(paths), "declares", "declare"))
	return nil
}

func printAutomationPushReport(r *automationPushResult) {
	out := os.Stdout
	if r.Target != "" {
		fmt.Fprintf(out, "  Target:  %s\n", r.Target)
	}
	if r.FeatureID != "" {
		fmt.Fprintf(out, "  Feature: %s\n\n", r.FeatureID)
	}
	if len(r.Files) == 0 {
		fmt.Fprintf(out, "  Nothing to push — this folder holds no automation files.\n")
		return
	}
	for _, file := range r.Files {
		fmt.Fprintf(out, "  %-9s %s", file.Outcome, file.Path)
		if file.AutomationID != "" {
			fmt.Fprintf(out, "  (%s)", file.AutomationID)
		}
		fmt.Fprintln(out)
		if len(file.Changed) > 0 && file.Outcome == automationOutcomeUpdated {
			fmt.Fprintf(out, "            wrote %s\n", strings.Join(file.Changed, ", "))
		}
		if file.Error != "" {
			fmt.Fprintf(out, "            %s\n", file.Error)
		}
		for _, note := range file.Notes {
			fmt.Fprintf(out, "            note: %s\n", note)
		}
	}
	if r.UpToDate {
		fmt.Fprintf(out, "\n  Everything already matches.\n")
	}
}
