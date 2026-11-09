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
	var noBrowser bool
	cmd := &cobra.Command{
		Use:   "propose [dir]",
		Short: "Ask the person you work for to approve these changes; changes nothing in Ronja itself",
		Long: `Ask the person you work for to approve these changes. propose changes
nothing in Ronja itself: it uploads a change list, and nothing is deployed until
that person approves it in Ronja.

  ronja propose            # the tree here
  ronja propose apps/      # another directory

It proposes the stack in ronja.json that points at the Ronja this credential
signs in to; --stack <name> picks one when several do.

"The person you work for" is the person whose token this is. They get a
notification and open the link this prints; their Approve runs the deploy in
Ronja, as them, on exactly the copy uploaded here.

What it does:

  1. Plans the tree the way ` + "`ronja sync apply --dry-run --allow-create`" + ` does,
     with the same code, plus each migrations folder bound to a managed
     database, the way ` + "`ronja db migrate status`" + ` does. It sends only reads.
  2. Uploads that plan and a copy of the directory: every file sync reads
     there, ronja.json and ronja.lock.json included. Nothing under a dot-name
     (.ronja/, .env, .git) and no credential is ever in the copy.
  3. Prints what would be created and modified, and the link to approve it.

It refuses, and uploads nothing, when:

  - a folder is not adopted: Ronja has to keep its state, so a deploy can never
    create the same resource twice. Run ` + "`ronja sync adopt --stack <name>`" + ` once.
  - a folder would not apply cleanly: the dry run's own refusal is printed.
  - a folder names, by address, a folder outside the directory proposed: the
    copy would not hold it. Propose from a directory that holds both.

To deploy directly, without asking anyone, run ` + "`ronja sync apply`" + ` and
` + "`ronja db migrate push`" + ` yourself.`,
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
			resolved, err := resolveInstance()
			if err != nil {
				return err
			}
			report, err := runPropose(cmd.Context(), dir, resolved)
			if err != nil {
				return err
			}
			if flagJSON {
				return emitJSON(report)
			}
			printPropose(report)
			if report.URL != "" && !noBrowser && isTerminal(os.Stdout) {
				_ = openBrowser(report.URL)
			}
			return nil
		},
	}
	cmd.Flags().StringVar(&flagStack, "stack", "",
		"stack in ronja.json to propose a deploy of (default: the one matching this credential)")
	cmd.Flags().BoolVar(&noBrowser, "no-browser", false, "print the link without opening it")
	return cmd
}

// proposeReport is the --json shape, and what the human report prints.
type proposeReport struct {
	// Status is the change list's, or "unchanged" when nothing would change
	// and nothing was proposed.
	Status    string        `json:"status"`
	ID        string        `json:"id,omitempty"`
	URL       string        `json:"url,omitempty"`
	ServerURL string        `json:"serverURL"`
	Stack     string        `json:"stack,omitempty"`
	Approver  string        `json:"approver,omitempty"`
	ExpiresAt *time.Time    `json:"expiresAt,omitempty"`
	Counts    proposeCounts `json:"counts"`
	Notes     []string      `json:"notes,omitempty"`
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

func runPropose(ctx context.Context, dir string, resolved *config.Resolved) (*proposeReport, error) {
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
	migrations, err := planProposedMigrations(ctx, client, base, key)
	if err != nil {
		return nil, err
	}
	plan.Databases = migrations.databases
	notes := migrations.notes
	if len(found) == 0 && migrations.bound == 0 {
		return nil, fmt.Errorf("nothing to propose under %s: it holds no %s and no migrations folder bound to a database", base, wfdir.ManifestName)
	}

	report := &proposeReport{ServerURL: resolved.URL, Stack: flagStack, Counts: countProposed(plan), Notes: notes}
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
	me, err := client.Me(ctx)
	if err != nil {
		return nil, fmt.Errorf("ask %s who approves this: %w", resolved.URL, err)
	}
	list, err := client.ProposeChangeList(ctx, api.ChangeListProposal{
		Archive: archive, Plan: planJSON, ServerURL: resolved.URL, Stack: flagStack,
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
	report.Status, report.ID, report.URL = list.Status, list.ID, list.URL
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
		lines = append(lines, fmt.Sprintf("  %s: run `ronja sync adopt --stack %s` there", folder.Rel, adoptStackName(folder.Manifest)))
	}
	return unadoptedError(lines)
}

func unadoptedError(lines []string) error {
	if len(lines) == 0 {
		return nil
	}
	return fmt.Errorf("nothing was proposed: %s not adopted, so Ronja does not keep %s state and a deploy of a copy could create %s resources twice. Adopt once, commit, and propose again:\n%s",
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

// proposeReadAllow admits the non-GETs planning sends, which save nothing:
// the workflow validate, and a database's migrations route, which propose
// reaches only through api.MigrationStatus (always a dry run).
func proposeReadAllow(method, path string) bool {
	if method != "POST" {
		return false
	}
	if path == "workflow/validate" {
		return true
	}
	id, ok := strings.CutPrefix(path, "database/")
	id, ok2 := strings.CutSuffix(id, "/migrations")
	return ok && ok2 && id != "" && !strings.Contains(id, "/")
}

// planProposedFolders is `ronja sync apply --dry-run --allow-create` on base,
// in process, under a guard that refuses any write before it is sent. Every
// folder has to plan as applied.
func planProposedFolders(ctx context.Context, base string, resolved *config.Resolved) ([]syncApplyFolderReport, error) {
	savedDryRun, savedAllowCreate := flagSyncDryRun, flagSyncAllowCreate
	flagSyncDryRun, flagSyncAllowCreate = true, true
	defer func() { flagSyncDryRun, flagSyncAllowCreate = savedDryRun, savedAllowCreate }()

	guard := &api.ReadOnlyGuard{Allow: proposeReadAllow}
	gctx := withAddressCopyRoot(api.WithReadOnlyGuard(ctx, guard))
	report, err := syncApplyTree(gctx, base, resolved)
	if blocked := guard.Blocked(); len(blocked) > 0 {
		return nil, fmt.Errorf("nothing was proposed: planning would have sent %s, which propose never sends. This is a bug in ronja worth reporting", strings.Join(blocked, ", "))
	}
	if err != nil {
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

// proposedMigrations is what planProposedMigrations found.
type proposedMigrations struct {
	// databases are the folders with migrations a push would apply, and dirs
	// their absolute paths.
	databases []proposeDatabase
	dirs      []string
	// bound counts every migrations folder with a binding file, notes say why
	// one is not in the list.
	bound int
	notes []string
}

// planProposedMigrations finds every migrations folder under base bound to a
// database on this instance and organization, and asks the database what a
// push would apply, as `ronja db migrate status` does. A folder with nothing
// pending is not part of the list.
func planProposedMigrations(ctx context.Context, client *api.Client, base string, key wfdir.InstanceKey) (*proposedMigrations, error) {
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

	out := &proposedMigrations{bound: len(dirs)}
	for _, dir := range dirs {
		rel, err := filepath.Rel(base, dir)
		if err != nil {
			return nil, err
		}
		rel = filepath.ToSlash(rel)
		binding, err := dbdir.Load(dir)
		if err != nil {
			return nil, err
		}
		databaseID, err := binding.DatabaseForStack(key, flagStack)
		if err != nil {
			return nil, fmt.Errorf("%s: %w — run `ronja db migrate status --database <id>` there to say which database it is", rel, err)
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
		guard := &api.ReadOnlyGuard{Allow: proposeReadAllow}
		status, err := client.MigrationStatus(api.WithReadOnlyGuard(ctx, guard), databaseID, migrations, api.DefaultMigrationTimeout)
		if blocked := guard.Blocked(); len(blocked) > 0 {
			return nil, fmt.Errorf("nothing was proposed: planning would have sent %s, which propose never sends. This is a bug in ronja worth reporting", strings.Join(blocked, ", "))
		}
		if err != nil {
			return nil, fmt.Errorf("%s: the migration status of %s: %w", rel, databaseID, err)
		}
		if err := migrationDriftError(status); err != nil {
			return nil, fmt.Errorf("nothing was proposed: %s: %w", rel, err)
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
	fmt.Fprintf(out, "Proposed change list %s: %s.\n", r.ID, r.Counts)
	if r.URL != "" {
		fmt.Fprintf(out, "  %s\n", r.URL)
	}
	fmt.Fprintf(out, "Nothing changes until %s approves it in Ronja.\n", r.Approver)
}
