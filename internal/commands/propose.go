package commands

import (
	"context"
	"encoding/json"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/ronjatech/ronja-cli/internal/api"
	"github.com/ronjatech/ronja-cli/internal/config"
	"github.com/ronjatech/ronja-cli/internal/dbdir"
	"github.com/ronjatech/ronja-cli/internal/wfdir"
	"github.com/spf13/cobra"
)

func newProposeCmd() *cobra.Command {
	var (
		noBrowser bool
		database  string
		describe  string
	)
	cmd := &cobra.Command{
		Use:   "propose [dir]",
		Short: "Ask the person you work for to approve these changes; deploys nothing itself",
		Long: `Ask the person you work for to approve these changes:

  ronja propose            # in the app folder, or the tree, to deploy

propose deploys nothing itself: it uploads a change list, unsent, and
nothing is deployed until that person approves it in Ronja. You describe it,
then send it (below). Pass a directory to propose another one
(` + "`ronja propose apps/`" + `); a folder named like a subcommand (wait,
describe, send, discard) as ` + "`ronja propose ./send`" + `.

The first propose of a lone folder Ronja keeps no state for, whose ronja.json
names its feature here (as ` + "`ronja app init`" + ` and ` + "`ronja app clone`" + ` write it),
sets the folder up first, once: it adds the folder's stack for that feature
under the default stack name (main, in the organization's default Space) or
--stack, then adopts that folder as ` + "`ronja sync adopt`" + ` does, writing a
folder id into ronja.json and claiming in Ronja the rows the folder already
deploys. It prints both steps and the files that changed: commit them.

A row has one folder. Propose refuses a folder whose app, workflow or module
another folder holds (claimed_elsewhere), naming that folder. To continue the
row from this folder instead, as a fresh clone of a live app does when its old
folder is gone, propose with --take-over:

  ronja app clone <data-app-id> apps/<name> --stack main
  ronja propose apps/<name> --take-over

It releases the other folder's claim and claims the row for this one, only
when this lone folder is based on the live version (cloned with --stack, which
records it) and the other folder is not in a git repository. The other
folder's next deploy, or its pending change list, is refused claimed_elsewhere.

Agents: ship with ` + "`ronja propose`" + `. It is the recommended way for an agent
to deploy apps, workflows, pipelines and database migrations: the person this
login belongs to approves it in Ronja, and Ronja applies exactly what was
uploaded. ` + "`ronja sync apply`" + ` and ` + "`ronja db migrate push`" + ` are for people and CI
deploying directly, without asking anyone.

It proposes the stack in ronja.json that points at the Ronja this credential
signs in to; --stack <name> picks one when several do.

"The person you work for" is the person whose token this is. They get a
notification once you send the list, and open its link; their Approve runs the
deploy in Ronja, as them, on exactly the copy uploaded here.

What it does:

  1. Plans the tree the way ` + "`ronja sync apply --dry-run --allow-create`" + ` does,
     with the same code, plus each migrations folder bound to a managed
     database (or the one --database names), the way
     ` + "`ronja db migrate status`" + ` does. It sends only reads.
  2. Uploads that plan and a copy of the directory: every file sync reads
     there, ronja.json and ronja.lock.json included. Nothing under a dot-name
     (.ronja/, .env, .git) and no credential is ever in the copy. With it go
     the apps it creates or changes as the deploy will write them, so the
     person can open each app's draft before approving; the deploy stops
     before writing if what it would write differs from the draft.
  3. Prints what would be created and modified, the link to approve it, each
     change under the key its note is written with, one
     ` + "`Draft <app>: <link>`" + ` line per app (the app as it will look once the list
     deploys; --json: drafts), or why it has none, and the next step.

Then describe it and send it. Nobody is told of the list until it is sent:

  ronja propose describe <id> > .change.json  # every key, "" where nothing is written
  ronja propose describe <id> .change.json    # once you wrote the title, the description and a note per change
  ronja propose send <id>                     # tells the person, and opens the link on a terminal

Write them for the person approving it, who may not read code: what it does
for them and why. --describe <file> saves a description in the propose itself.
On a chat's computer, call requestPublish instead of send: it sends the list.
Proposing the same folders again replaces the open list, sent or not, with a
new unsent one; its title, its description and each note whose change is still
in it carry over, and propose names each note it drops.
` + "`ronja propose discard <id>`" + ` ends a list you no longer mean to send.
--json carries status, the skeleton and next.

Once it is sent, run
` + "`ronja propose wait <id>`" + ` in the background: it ends when the list does, which
wakes you with what happened, and you carry on. Pass no --timeout there: one
only wakes you early to start it again. Without background commands,
ask the person to say when they have approved, then run it in the foreground
with --timeout under your tool's time limit (` + "`--timeout 9m`" + `), and again for as
long as it says still deploying. On rejected, tell the person and ask what to
change; on failed, tell them the reason, fix it, and propose again. When every
folder that did not go live was sent for review, tell the person and do not
propose again: an admin or a maintainer of its feature decides it in Ronja, and
proposing again sends the same review.

It refuses, and uploads nothing, when:

  - a folder is not adopted and propose cannot set it up: Ronja has to keep
    its state, so a deploy can never create the same resource twice. Each
    folder is named with the one command that sets it up: in a tree,
    ` + "`ronja propose`" + ` in that folder on its own; where ronja.json names no
    feature here, ` + "`ronja sync stack add --feature <id>`" + `.
  - a folder would not apply cleanly: the dry run's own refusal is printed.
    Each workflow, data app and module is planned as the copy the deploy
    runs on, with no .ronja/, so what that deploy would refuse is refused
    here: a folder whose stack records no live version (run
    ` + "`ronja sync adopt`" + `), or whose recorded version live has moved past.
    One exception: your own open draft of its row goes through, and the
    person sees it when they approve. A colleague's module draft refuses.
  - an app or workflow changed in Ronja since its folder last applied it (in
    the web app, by the chat's tools, from another clone): a change list never
    overwrites that. ` + "`ronja app pull`" + ` (or ` + "`ronja wf pull`" + `) takes it into the
    folder; then propose again.
  - a database has more than 40 migrations pending: one push applies at most
    40, so propose up to the one it names first, then the rest.
  - with --stack, a database outside the Space that stack is for
    (bind_space_mismatch, the check ` + "`ronja db migrate push --stack`" + ` makes), or
    a migrations folder outside a git work tree, where no Space can be read.
    Pass --database with a database in the stack's Space, from inside the
    repository.
  - a folder names, by address, a folder outside the directory proposed: the
    copy would not hold it. Propose from a directory that holds both.

A migrations folder is proposed for the database it remembers from its last
push. For a first migration, or to name the database yourself, pass
--database, as ` + "`ronja db migrate push --database`" + ` takes it:

  ronja propose --database mdb-abc     # the one migrations/ folder here

--database needs exactly one migrations folder holding .sql files under the
directory proposed, and propose remembers nothing. propose sends every folder
under the directory it is given, so run it from the folder that holds
migrations/, not from a repository root holding folders that are not adopted.`,
		Args: cobra.MaximumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			dir := "."
			if len(args) == 1 {
				dir = args[0]
			}
			if flagStack != "" {
				if err := wfdir.ValidStackName(flagStack); err != nil {
					return err
				}
			}
			var description json.RawMessage
			if describe != "" {
				var err error
				if description, err = readChangeListDescription(describe, cmd.InOrStdin()); err != nil {
					return err
				}
			}
			resolved, err := resolveInstance()
			if err != nil {
				return err
			}
			report, err := runPropose(cmd.Context(), dir, database, resolved)
			if err != nil {
				return err
			}
			describeErr := describeProposed(cmd.Context(), api.New(resolved.URL, resolved.Token), report, describe, description)
			if flagJSON {
				if err := emitJSON(report); err != nil {
					return err
				}
				return describeErr
			}
			printPropose(report)
			if report.Status == changeListPending && report.URL != "" && !noBrowser && isTerminal(os.Stdout) {
				_ = openBrowser(report.URL)
			}
			return describeErr
		},
	}
	cmd.Flags().StringVar(&flagStack, "stack", "",
		"stack in ronja.json to propose a deploy of (default: the one matching this credential)")
	cmd.Flags().StringVar(&database, "database", "",
		"managed database id for the one migrations folder under [dir] (default: the folder's saved database)")
	cmd.Flags().BoolVar(&flagTakeOver, "take-over", false,
		"continue a row another folder holds: release that folder's claim and claim the row for this one (a lone app, workflow or module folder based on the live version, never a git repository's)")
	cmd.Flags().BoolVar(&noBrowser, "no-browser", false, "print the link without opening it")
	cmd.Flags().StringVar(&describe, "describe", "",
		"save this description (a file `ronja propose describe` printed and you filled in, or - for stdin) on the new list in the same call")
	cmd.AddCommand(newProposeWaitCmd(), newProposeDescribeCmd(), newProposeSendCmd(), newProposeDiscardCmd())
	return cmd
}

// flagTakeOver is `propose --take-over`: the lone folder's claim step takes
// the row over from the folder that holds it (setUpLoneFolder,
// syncAdoptFolder).
var flagTakeOver bool

// proposeReport is the --json shape, and what the human report prints.
type proposeReport struct {
	// Status is the change list's, or "unchanged" when nothing would change
	// and nothing was proposed.
	Status    string     `json:"status"`
	ID        string     `json:"id,omitempty"`
	URL       string     `json:"url,omitempty"`
	ServerURL string     `json:"serverURL"`
	Stack     string     `json:"stack,omitempty"`
	Approver  string     `json:"approver,omitempty"`
	ExpiresAt *time.Time `json:"expiresAt,omitempty"`
	// Setup is what propose did to adopt a lone folder first, nil when it
	// did nothing.
	Setup  *proposeSetup `json:"setup,omitempty"`
	Counts proposeCounts `json:"counts"`
	// Drafts link each app Ronja stored a draft for: the app as it will look
	// once the list deploys.
	Drafts []api.ChangeListDraft `json:"drafts,omitempty"`
	Notes  []string              `json:"notes,omitempty"`
	// Title, Changes and Skeleton are the list as the server tells it, and
	// DroppedNotes the replaced list's notes it did not carry over.
	Title        string                  `json:"title,omitempty"`
	Changes      []api.ChangeListChange  `json:"changes,omitempty"`
	Skeleton     *api.ChangeListSkeleton `json:"skeleton,omitempty"`
	DroppedNotes []string                `json:"droppedNotes,omitempty"`
	// Next is the step to take next, as the server words it.
	Next string `json:"next,omitempty"`
}

type proposeCounts struct {
	Created  int `json:"created"`
	Modified int `json:"modified"`
	// Removed are files taken out of a folder: Ronja stops managing their
	// rows and deletes nothing.
	Removed    int `json:"removed"`
	Migrations int `json:"migrations"`
}

func (c proposeCounts) total() int { return c.Created + c.Modified + c.Removed + c.Migrations }

func (c proposeCounts) String() string {
	var parts []string
	add := func(n int, s string) {
		if n > 0 {
			parts = append(parts, fmt.Sprintf("%d %s", n, s))
		}
	}
	add(c.Modified, "modified")
	add(c.Created, "created")
	add(c.Migrations, plural(c.Migrations, "database migration"))
	add(c.Removed, "removed from the folder (left in Ronja)")
	return strings.Join(parts, ", ")
}

const proposeStatusUnchanged = "unchanged"

// proposePlan is echangelist.Plan as the CLI sends it: Folders is exactly the
// `folders` of `ronja sync apply --dry-run --allow-create --json`.
type proposePlan struct {
	Dir       string                  `json:"dir"`
	Folders   []syncApplyFolderReport `json:"folders"`
	Databases []proposeDatabase       `json:"databases,omitempty"`
}

type proposeDatabase struct {
	Dir        string             `json:"dir"`
	DatabaseID string             `json:"databaseID"`
	Migrations []proposeMigration `json:"migrations"`
}

type proposeMigration struct {
	Name string `json:"name"`
	SQL  string `json:"sql"`
}

// database, when set, is the database for the one migrations folder under
// dir, bound or not, in place of its binding.
func runPropose(ctx context.Context, dir, database string, resolved *config.Resolved) (*proposeReport, error) {
	base, err := filepath.Abs(dir)
	if err != nil {
		return nil, fmt.Errorf("resolve %s: %w", dir, err)
	}
	if err := ensureTenant(ctx, resolved); err != nil {
		return nil, err
	}
	key := wfdir.InstanceKey{URL: resolved.URL, TenantID: resolved.TenantID}
	client := api.New(resolved.URL, resolved.Token)

	found, err := discoverFolders(base)
	if err != nil {
		return nil, err
	}
	setup, err := setUpLoneFolder(ctx, base, found, resolved)
	if err != nil {
		return nil, err
	}
	if setup != nil {
		// Said now: a refusal below must not hide that ronja.json changed.
		if flagJSON {
			printProposeSetup(os.Stderr, setup)
		} else {
			printProposeSetup(os.Stdout, setup)
		}
		if found, err = discoverFolders(base); err != nil {
			return nil, err
		}
	}
	if err := refuseUnadopted(found); err != nil {
		return nil, err
	}
	plan := proposePlan{Dir: ".", Folders: []syncApplyFolderReport{}}
	if len(found) > 0 {
		folders, err := planProposedFolders(ctx, base, resolved)
		if err != nil {
			return nil, err
		}
		plan.Folders = folders
	}
	migrations, err := planProposedMigrations(ctx, client, resolved, base, key, database)
	if err != nil {
		return nil, err
	}
	plan.Databases = migrations.databases
	notes := migrations.notes
	if len(found) == 0 && migrations.bound == 0 {
		return nil, fmt.Errorf("nothing to propose under %s: it holds no %s and no migrations folder bound to a database", base, wfdir.ManifestName)
	}

	report := &proposeReport{ServerURL: resolved.URL, Stack: flagStack, Setup: setup, Counts: countProposed(plan), Notes: notes}
	if report.Counts.total() == 0 {
		report.Status = proposeStatusUnchanged
		return report, nil
	}

	archive, copied, err := packProposal(base, found, migrations.dirs)
	if err != nil {
		return nil, err
	}
	planJSON, err := json.Marshal(plan)
	if err != nil {
		return nil, err
	}
	previews, err := proposedPreviews(plan.Folders)
	if err != nil {
		return nil, err
	}
	me, err := client.Me(ctx)
	if err != nil {
		return nil, fmt.Errorf("ask %s who approves this: %w", resolved.URL, err)
	}
	list, err := client.ProposeChangeList(ctx, api.ChangeListProposal{
		Archive: archive, Previews: previews, Plan: planJSON, ServerURL: resolved.URL, Stack: flagStack, Unsent: true,
	})
	if err != nil {
		switch api.StatusOf(err) {
		case 404:
			return nil, fmt.Errorf("%s does not take change lists yet (%w) — deploy with `ronja sync apply` instead, or ask for the instance to be updated", resolved.URL, err)
		case 429:
			return nil, fmt.Errorf("%s is busy receiving other change lists (%w): nothing was proposed — run `ronja propose` again in a few seconds", resolved.URL, err)
		}
		return nil, fmt.Errorf("propose the change list: %w", err)
	}
	report.Status, report.ID, report.URL, report.Drafts = list.Status, list.ID, list.URL, list.Drafts
	report.Title, report.Changes, report.Skeleton, report.DroppedNotes, report.Next = list.Title, list.Changes, list.Skeleton, list.DroppedNotes, list.Next
	if !list.ExpiresAt.IsZero() {
		report.ExpiresAt = &list.ExpiresAt
	}
	report.Approver = personName(me)
	report.Notes = append(report.Notes, recordProposals(found, plan, copied, key, list.ID)...)
	return report, nil
}

// recordProposals records what the copy held of each draft-kind folder it
// changes: the deploy runs on the copy, so the folder's baseline never learns
// it (live_equals_desired.go). A failure is a note: the list is already
// proposed.
func recordProposals(found []discoveredFolder, plan proposePlan, copied map[string]map[string]string, key wfdir.InstanceKey, listID string) []string {
	writes, over := map[string]bool{}, map[string]string{}
	for _, f := range plan.Folders {
		over[f.Root] = f.over
		for _, u := range f.Units {
			if applyAction(u.Action) != applyActionUnchanged {
				writes[f.Root] = true
			}
		}
	}
	var notes []string
	for _, folder := range found {
		switch folder.Kind.Name {
		case wfdir.KindWorkflow, wfdir.KindDataApp, wfdir.KindModule:
		default:
			continue
		}
		if !writes[folder.Root] {
			continue
		}
		manifest, err := json.Marshal(folder.Manifest)
		if err == nil {
			err = wfdir.RecordProposal(folder.Root, wfdir.Proposal{
				URL: key.URL, TenantID: key.TenantID, ListID: listID, Files: copied[folder.Root], Manifest: manifest,
				Over: over[folder.Root],
			})
		}
		if err != nil {
			notes = append(notes, fmt.Sprintf("%s: this folder could not record what it proposed (%v), so once the list is approved its next push reads the deploy as a change made in Ronja", folder.Rel, err))
		}
	}
	return notes
}

// refuseUnadopted refuses every folder whose state Ronja does not keep. A
// change list is applied from a copy whose lock never comes back, so only a
// folder whose ids live on the server cannot create its resources twice.
func refuseUnadopted(found []discoveredFolder) error {
	var lines []string
	for _, folder := range found {
		if folder.Manifest == nil {
			continue
		}
		stack, declared := folder.Manifest.Stacks[flagStack]
		if folder.Manifest.Adopted() && (flagStack == "" || !declared || !stack.Unmanaged) {
			continue
		}
		if len(folder.Manifest.Stacks) == 0 {
			lines = append(lines, fmt.Sprintf("  %s: run `ronja propose` there, on its own: it adds the folder's stack and adopts it", folder.Rel))
			continue
		}
		lines = append(lines, fmt.Sprintf("  %s: run `ronja sync adopt --stack %s` there", folder.Rel, adoptStackName(folder.Manifest)))
	}
	return unadoptedError(lines)
}

func unadoptedError(lines []string) error {
	if len(lines) == 0 {
		return nil
	}
	return fmt.Errorf("nothing was proposed: %s not adopted, so Ronja does not keep %s state and a deploy of a copy could create %s resources twice. Run the command named for each once, commit, and propose again:\n%s",
		agree(len(lines), "this folder is", "these folders are"), agree(len(lines), "its", "their"), agree(len(lines), "its", "their"),
		strings.Join(lines, "\n"))
}

func adoptStackName(m *wfdir.Manifest) string {
	if flagStack != "" {
		return flagStack
	}
	if names := m.StackNames(); len(names) == 1 {
		return names[0]
	}
	return "<name>"
}

// planningProposal is set while propose plans, so a refusal names what to do
// before proposing again rather than a command that deploys directly.
var planningProposal bool

// dryRunReadAllow admits the one other non-GET planning sends, the workflow
// validate, which saves nothing; the guard itself admits a migration status
// (api.MigrationStatus). Every `sync apply --dry-run` and propose plan under
// it.
func dryRunReadAllow(method, path string) bool {
	return method == "POST" && path == "workflow/validate"
}

// proposeGuardError is the refusal of a write planning tried to send, which
// outranks err, or else err.
func proposeGuardError(guard *api.ReadOnlyGuard, err error) error {
	if blocked := guard.Blocked(); len(blocked) > 0 {
		return fmt.Errorf("nothing was proposed: planning would have sent %s, which propose never sends. This is a bug in ronja worth reporting", strings.Join(blocked, ", "))
	}
	return err
}

// planProposedFolders is `ronja sync apply --dry-run --allow-create` on base,
// in process, under a guard that refuses any write before it is sent. Every
// folder has to plan as applied.
func planProposedFolders(ctx context.Context, base string, resolved *config.Resolved) ([]syncApplyFolderReport, error) {
	savedDryRun, savedAllowCreate := flagSyncDryRun, flagSyncAllowCreate
	flagSyncDryRun, flagSyncAllowCreate, planningProposal = true, true, true
	defer func() { flagSyncDryRun, flagSyncAllowCreate, planningProposal = savedDryRun, savedAllowCreate, false }()

	guard := &api.ReadOnlyGuard{Allow: dryRunReadAllow}
	gctx := withAddressCopyRoot(api.WithReadOnlyGuard(ctx, guard))
	report, err := syncApplyTree(gctx, base, resolved)
	if err := proposeGuardError(guard, err); err != nil {
		return nil, err
	}
	if err := passOwnDrafts(gctx, api.New(resolved.URL, resolved.Token), report.Folders); err != nil {
		return nil, err
	}

	var refused, unadopted []string
	for _, f := range report.Folders {
		switch {
		case f.Verdict != verdictApplied:
			line := fmt.Sprintf("  %s: %s", f.Path, f.Verdict)
			if f.Reason != "" {
				line += " (" + f.Reason + ")"
			}
			if f.Detail != "" {
				line += " — " + f.Detail
			}
			for _, u := range f.Units {
				if u.Action == string(applyActionRefuse) || u.Action == string(applyActionUnanchored) {
					line += fmt.Sprintf("\n      %s: %s %s", applyUnitLabel(u, f.Kind), u.Action, u.Detail)
				}
			}
			refused = append(refused, line)
		case !f.adopted:
			unadopted = append(unadopted, fmt.Sprintf("  %s: Ronja does not keep its state on this stack; run `ronja sync adopt --stack <name>` there", f.Path))
		}
	}
	if len(refused) > 0 {
		return nil, fmt.Errorf("nothing was proposed: a change list has to plan cleanly, and `ronja sync apply --dry-run --allow-create` says:\n%s", strings.Join(refused, "\n"))
	}
	if err := unadoptedError(unadopted); err != nil {
		return nil, err
	}
	return report.Folders, nil
}

// passOwnDrafts turns each folder refused only because the proposer's own
// draft of its row is open (draft_open) into the update it would be once that
// draft is gone: the person sees the draft when they approve, and the deploy
// discards it. A workflow or data-app draft is always the caller's own; a
// module has one draft, which is the proposer's only when they drafted it and
// edited it last. Anyone else's stays refused, naming them.
func passOwnDrafts(ctx context.Context, client *api.Client, folders []syncApplyFolderReport) error {
	var me string
	for i := range folders {
		line := &folders[i]
		if line.Verdict != verdictRefused || line.Reason != syncReasonDraftOpen || len(line.Units) != 1 || line.Units[0].Draft == nil {
			continue
		}
		u := &line.Units[0]
		d := u.Draft
		if line.Kind == wfdir.KindModule {
			if me == "" {
				who, err := client.Me(ctx)
				if err != nil {
					return fmt.Errorf("ask who is proposing, to tell their own module draft from a colleague's: %w", err)
				}
				me = who.User.ID
			}
			if d.DrafterUserID != me || d.UpdatedBy != me {
				line.Detail = fmt.Sprintf("module %s has an open draft (%s) that is not only yours: drafted by %s and last edited by %s at %s. Approving a change list never discards a colleague's draft: ask them to publish or discard it, then propose again",
					u.ID, d.ID, orUnknown(d.DrafterUserID), orUnknown(d.UpdatedBy), d.UpdatedAt.UTC().Format(time.RFC3339))
				continue
			}
		}
		edited := d.UpdatedAt.UTC().Format(time.RFC3339)
		u.Action = string(applyActionUpdate)
		u.Detail = fmt.Sprintf("your open draft %s (last edited %s) is discarded when this list deploys", d.ID, edited)
		line.Verdict, line.Reason = verdictApplied, ""
		kind, _ := wfdir.KindByName(line.Kind)
		line.Detail = fmt.Sprintf("would push and publish the %s once your open draft %s (last edited %s) is discarded", kind.Label, d.ID, edited)
	}
	return nil
}

// proposedMigrations is what planProposedMigrations found.
type proposedMigrations struct {
	// databases are the folders with migrations a push would apply, and dirs
	// their absolute paths.
	databases []proposeDatabase
	dirs      []string
	// bound counts every migrations folder with a binding file, or the one
	// --database names; notes say why one is not in the list.
	bound int
	notes []string
}

// planProposedMigrations finds every migrations folder under base bound to a
// database on this instance and organization, and asks the database what a
// push would apply, as `ronja db migrate status` does, after the Space check
// `ronja db migrate push` makes with --stack. A folder with nothing pending is
// not part of the list. With database, base must hold exactly one migrations
// folder holding .sql files, bound or not, and it targets that database.
func planProposedMigrations(ctx context.Context, client *api.Client, resolved *config.Resolved, base string, key wfdir.InstanceKey, database string) (*proposedMigrations, error) {
	var dirs []string
	err := filepath.WalkDir(base, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if !d.IsDir() {
			return nil
		}
		if path != base && skipWalkDir(d.Name()) {
			return fs.SkipDir
		}
		if database != "" {
			if dbdir.HasMigrations(path) {
				dirs = append(dirs, path)
			}
			return nil
		}
		if info, err := os.Stat(filepath.Join(path, dbdir.MigrationsDir)); err != nil || !info.IsDir() {
			return nil
		}
		if _, err := os.Stat(dbdir.Path(path)); err == nil {
			dirs = append(dirs, path)
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	if database != "" && len(dirs) != 1 {
		return nil, oneMigrationsFolderError(base, dirs)
	}

	out := &proposedMigrations{bound: len(dirs)}
	for _, dir := range dirs {
		rel, err := filepath.Rel(base, dir)
		if err != nil {
			return nil, err
		}
		rel = filepath.ToSlash(rel)
		databaseID := database
		if databaseID == "" {
			binding, err := dbdir.Load(dir)
			if err != nil {
				return nil, err
			}
			databaseID, err = binding.DatabaseForStack(key, flagStack)
			if err != nil {
				return nil, fmt.Errorf("%s: %w — run `ronja db migrate status --database <id>` there to say which database it is", rel, err)
			}
		}
		if databaseID == "" {
			note := fmt.Sprintf("%s/%s is bound to a database on another instance or organization, so it is not part of this change list", rel, dbdir.MigrationsDir)
			if flagStack != "" {
				note = fmt.Sprintf("%s/%s remembers no database for stack %s on this instance and organization, so it is not part of this change list; `ronja db migrate status --stack %s --database <id>` there says which one it is", rel, dbdir.MigrationsDir, flagStack, flagStack)
			}
			out.notes = append(out.notes, note)
			continue
		}
		migrations, err := dbdir.ReadMigrations(dir)
		if err != nil {
			return nil, fmt.Errorf("%s: %w", rel, err)
		}
		guard := &api.ReadOnlyGuard{Allow: dryRunReadAllow}
		gctx := api.WithReadOnlyGuard(ctx, guard)
		if err := proposeGuardError(guard, checkDatabaseSpace(gctx, client, resolved, key, dir, databaseID, "proposed", false)); err != nil {
			return nil, err
		}
		status, err := client.MigrationStatus(gctx, databaseID, migrations, api.DefaultMigrationTimeout)
		if gerr := proposeGuardError(guard, nil); gerr != nil {
			return nil, gerr
		}
		if err != nil {
			return nil, fmt.Errorf("%s: the migration status of %s: %w", rel, databaseID, err)
		}
		if err := migrationDriftError(status); err != nil {
			return nil, fmt.Errorf("nothing was proposed: %s: %w", rel, err)
		}
		if pending := status.Pending(); len(pending) > maxPendingPerPush {
			return nil, fmt.Errorf("nothing was proposed: %s: %d migrations are not applied yet, and one push applies at most %d (they run in one transaction). Propose up to and including %s first (move the later files out of %s), then the rest, as `ronja db migrate push --help` describes",
				rel, len(pending), maxPendingPerPush, pending[maxPendingPerPush-1].Name, filepath.ToSlash(filepath.Join(rel, dbdir.MigrationsDir)))
		}
		sql := make(map[string]string, len(migrations))
		for _, m := range migrations {
			sql[m.Name] = m.SQL
		}
		db := proposeDatabase{Dir: rel, DatabaseID: databaseID}
		for _, p := range status.Pending() {
			db.Migrations = append(db.Migrations, proposeMigration{Name: p.Name, SQL: sql[p.Name]})
		}
		if len(db.Migrations) == 0 {
			continue
		}
		out.databases = append(out.databases, db)
		out.dirs = append(out.dirs, dir)
	}
	return out, nil
}

func oneMigrationsFolderError(base string, dirs []string) error {
	if len(dirs) == 0 {
		return fmt.Errorf("nothing was proposed: --database names the database of the one %[1]s/ folder under %[2]s, and there is no %[1]s/ folder holding .sql files there. Run propose from the folder that holds %[1]s/", dbdir.MigrationsDir, base)
	}
	lines := make([]string, 0, len(dirs))
	for _, dir := range dirs {
		rel, err := filepath.Rel(base, dir)
		if err != nil {
			rel = dir
		}
		lines = append(lines, "  "+filepath.ToSlash(filepath.Join(rel, dbdir.MigrationsDir)))
	}
	return fmt.Errorf("nothing was proposed: --database names the database of one %s folder, and %s holds %d. Run propose with --database from the folder that holds the one you mean:\n%s",
		dbdir.MigrationsDir, base, len(dirs), strings.Join(lines, "\n"))
}

func countProposed(plan proposePlan) proposeCounts {
	var c proposeCounts
	for _, f := range plan.Folders {
		for _, u := range f.Units {
			switch applyAction(u.Action) {
			case applyActionCreate:
				c.Created++
			case applyActionUpdate, applyActionAdopt, applyActionMove:
				c.Modified++
			case applyActionRemove:
				c.Removed++
			}
		}
	}
	for _, db := range plan.Databases {
		c.Migrations += len(db.Migrations)
	}
	return c
}

func personName(me *api.Me) string {
	name := strings.TrimSpace(me.User.FirstName + " " + me.User.LastName)
	if name != "" {
		return name
	}
	if me.User.Email != "" {
		return me.User.Email
	}
	return "the person this token belongs to"
}

func printPropose(r *proposeReport) {
	out := os.Stdout
	for _, n := range r.Notes {
		fmt.Fprintf(out, "  Note: %s\n", n)
	}
	if r.Status == proposeStatusUnchanged {
		fmt.Fprintln(out, "Nothing to propose: Ronja already holds everything in this directory.")
		return
	}
	unsent := r.Status == changeListUnsent
	if unsent {
		fmt.Fprintf(out, "Proposed change list %s: %s. Not sent yet: nobody has been notified.\n", r.ID, r.Counts)
	} else {
		fmt.Fprintf(out, "Proposed change list %s: %s.\n", r.ID, r.Counts)
	}
	if r.URL != "" {
		fmt.Fprintf(out, "  %s\n", r.URL)
	}
	if unsent {
		printChanges(out, r.Changes)
	}
	for _, d := range r.Drafts {
		switch {
		case d.URL != "":
			fmt.Fprintf(out, "  Draft %s: %s\n", terminalSafe(d.Title), d.URL)
		case d.Refused != "":
			fmt.Fprintf(out, "  Draft %s: none, because %s\n", terminalSafe(d.Title), d.Refused)
		}
	}
	if !unsent {
		fmt.Fprintf(out, "Nothing changes until %s approves it in Ronja.\n", r.Approver)
		fmt.Fprintln(out, "This Ronja takes no description: it sent the list as proposed.")
		fmt.Fprintf(out, "To continue once it is decided, run: %s   (agents: in the background)\n", carryCredential("ronja propose wait "+r.ID))
		return
	}
	if len(r.DroppedNotes) > 0 {
		fmt.Fprintf(out, "Notes dropped, since their change is no longer in this list: %s\n", terminalSafe(strings.Join(r.DroppedNotes, ", ")))
	}
	printNextStep(out, r.ID, r.Skeleton, r.Next)
}
