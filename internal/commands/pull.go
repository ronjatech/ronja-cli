package commands

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/ronjatech/ronja-cli/internal/api"
	"github.com/ronjatech/ronja-cli/internal/markers"
	"github.com/ronjatech/ronja-cli/internal/wfdir"
	"github.com/spf13/cobra"
)

// `ronja <kind> pull` — THE BRIDGE BACK.
//
// A classic edit of a managed row is legal: somebody fixed it in the web app,
// through the agent, or from a cloned folder. The repository's next
// deploy then refuses the difference as drift. Pull is how the repository
// takes the change in: it rewrites an ADOPTED folder's files from the LIVE row,
// in the folder's own alias form (the codec's toDisk), prints the diff, and
// records the live row as what the folder last applied — so committing
// the diff and applying again sends no write at all.
//
// Four rules it keeps:
//
//   - It never overwrites work. A folder with uncommitted changes is refused,
//     and so is one outside git, where nothing can tell what is committed, and
//     so is a pull that would overwrite or delete a file git does not track
//     (an ignored one) — unless --force says to overwrite them.
//   - It never re-binds anything. ronja.json's dependencies and binds are never
//     touched: a live reference the folder's aliases cannot express is written
//     as the literal id and NAMED, line by line — never turned into a new
//     alias, which would silently decide what another stack binds.
//   - It reads LIVE only. The author's open draft is left exactly as it is, and
//     so is its drift guard: a later push of that draft is checked as before.
//   - It writes nothing on the server but anchors. The anchor is recorded by
//     the kind's own deploy push in its live-first mode — the same path, and
//     so the same anchor, a `sync apply` of the pulled folder would write —
//     under a guard (api.ReadOnlyGuard) that refuses every request but the two
//     anchor routes. A pull that could not reproduce the live row exactly
//     therefore cannot deploy anything: the guard names what it would have sent.

// Reasons a pull refuses with. Machine contract: add, never rename.
const (
	// pullReasonNotClaimed — the folder is adopted, but the server holds no
	// claim for what pull would read (the stack never deployed it from here).
	pullReasonNotClaimed = "not_claimed"
	// pullReasonPending — the folder declares a `moved` or `removed` the
	// server has not applied yet.
	pullReasonPending = "pending_declarations"
	// pullReasonDirty — the folder has uncommitted local changes.
	pullReasonDirty = "uncommitted_changes"
	// pullReasonOutsideGit — the folder is not in a git work tree.
	pullReasonOutsideGit = "outside_git"
	// pullReasonNotLive — the row has no live version to pull.
	pullReasonNotLive = "not_live"
	// pullReasonInexpressible — the folder cannot hold the live row as it is.
	pullReasonInexpressible = "inexpressible"
	// pullReasonWouldWrite — the anchor step would have written a row.
	pullReasonWouldWrite = "would_write"
	// pullReasonUndeployed — the folder changed a unit since its stack last
	// applied it, the live row changed too, and the pull would replace the
	// folder's change.
	pullReasonUndeployed = "undeployed_changes"
	// pullReasonUnrecoverable — a file the pull would overwrite or delete is
	// one git could not bring back: ignored, or untracked.
	pullReasonUnrecoverable = "unrecoverable_paths"
	// pullReasonSymlinked — a file the pull would write or delete lies
	// through a symbolic link inside the folder. --force does not override it.
	pullReasonSymlinked = "symlinked_path"
)

type pullOptions struct {
	DryRun bool
	Force  bool
}

func newPullCmd(kind wfdir.Kind) *cobra.Command {
	var opts pullOptions
	cmd := &cobra.Command{
		Use:   "pull [dir]",
		Short: "Take the live " + kind.Label + " into this repository folder",
		Long: fmt.Sprintf(`Take the live %[1]s into this repository folder, then review the diff and
commit it; the next apply then writes nothing.

  %[2]s pull                 rewrite the folder and record the live version
  %[2]s pull --dry-run       print the diff and write nothing

It is the answer when a deploy of a folder that keeps its records in Ronja (one
with an "id" in ronja.json) refuses a change somebody made in the web app,
through the agent, or from a clone, as drift. Pull rewrites the folder's files
from the LIVE version, in the folder's own alias form, prints the diff, and
records the live version as what this folder last applied.

Pull never changes ronja.json's dependencies or binds. A reference the folder's
aliases cannot express is written as the literal id and listed, line by line.

It reads the live version only: an open draft of yours is left as it is, and a
later push of it is checked for drift exactly as before.

Refused when the folder has no folder id (clone a fresh copy, or run
`+"`ronja sync adopt`"+`), when its stack's records on the server are not usable
(the same refusals a deploy makes), when the folder has uncommitted changes —
or is not in git at all, where nothing can tell what is committed — and when it
would overwrite or delete a file git does not track (an ignored one), which
nothing could bring back. --force overwrites them.`, kind.Label, kind.Command),
		Args: cobra.MaximumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			dir := ""
			if len(args) == 1 {
				dir = args[0]
			}
			report, err := runPull(cmd.Context(), kind, dir, opts)
			if report != nil {
				if flagJSON {
					if emitErr := emitJSON(report); emitErr != nil {
						return emitErr
					}
				} else {
					printPullReport(kind, report)
				}
			}
			return err
		},
	}
	cmd.Flags().BoolVar(&opts.DryRun, "dry-run", false,
		"print the diff and write nothing, locally or on the server")
	cmd.Flags().BoolVar(&opts.Force, "force", false,
		"pull even though the folder has uncommitted changes, is not in git, or holds files git does not track that the pull overwrites — overwriting them")
	return cmd
}

// pullReport is the --json shape and the human report's input.
type pullReport struct {
	Root      string `json:"root"`
	Kind      string `json:"kind"`
	Stack     string `json:"stack"`
	FeatureID string `json:"featureID"`
	DryRun    bool   `json:"dryRun"`
	// Files is every file the pull changed (or, on a dry run, would change).
	Files []pullFileReport `json:"files"`
	// Literals are the live references written as literal ids, because no alias
	// of this folder names them.
	Literals []pullLiteral `json:"literals,omitempty"`
	// Notes are what the next deploy will still do, and anything else worth
	// saying about a pull that landed.
	Notes []string `json:"notes,omitempty"`
	// Pending is what the next deploy of the committed pull will still write,
	// and why — a change the folder can hold only by deploying it.
	Pending []string `json:"pending,omitempty"`
	// Replaced names the folder's own changes, never deployed, that the pull
	// replaced with what is live — only ever with --force (on a dry run, what
	// it would replace).
	Replaced []string `json:"replaced,omitempty"`
	// Kept names the folder's own changes, never deployed, that the pull left
	// as they are because the live row has not changed since the stack last
	// applied it. The next apply deploys them.
	Kept []string `json:"kept,omitempty"`
	// Anchored reports that the live rows are recorded as what this folder last
	// applied, on the server.
	Anchored bool `json:"anchored"`
	// Diff is the unified diff, one file after another.
	Diff string `json:"diff"`
}

type pullFileReport struct {
	Path string `json:"path"`
	// Change is added, modified or deleted.
	Change string `json:"change"`
}

// pullLiteral is one live reference the pull wrote as a literal id.
type pullLiteral struct {
	Path string `json:"path"`
	// Line is 1-based; 0 for a field of a JSON file, which Field names.
	Line  int    `json:"line,omitempty"`
	Field string `json:"field,omitempty"`
	Kind  string `json:"kind"`
	ID    string `json:"id"`
}

func (l pullLiteral) describe() string {
	where := l.Path
	if l.Line > 0 {
		where = fmt.Sprintf("%s:%d", l.Path, l.Line)
	} else if l.Field != "" {
		where = fmt.Sprintf("%s (%s)", l.Path, l.Field)
	}
	return fmt.Sprintf("%s — %s %s", where, l.Kind, l.ID)
}

// pullPlan is what one kind's renderer read off the live rows: the new content
// of every file that changes, the manifest when its metadata does, and what
// the report owes the author.
type pullPlan struct {
	// files maps a folder-relative path to its new content; nil deletes it.
	// Only paths whose content changes.
	files map[string]*string
	// manifest is the folder's manifest with the live metadata, nil when
	// nothing in it changes. Its dependencies and binds are never touched.
	manifest *wfdir.Manifest
	literals []pullLiteral
	notes    []string
	// anchor records the live rows as applied, on the server, by the kind's
	// deploy push run under the read-only guard; it answers what the next
	// deploy will still write (pullReport.Pending).
	anchor func(ctx context.Context, f *folder) ([]string, error)
	// afterAnchor runs once the anchor landed (the local sync baseline).
	afterAnchor func(f *folder) error
	// gate is the write gate over what the folder holds once pulled — reads
	// only. runPull asks it before writing a file: a live row that writes or
	// runs outside the stack's feature is one this folder could not deploy,
	// and taking it in would commit code every later apply refuses.
	gate func(ctx context.Context) error

	// undeployed names what the folder changed since its stack last applied it
	// where the live row changed too: the pull would replace the folder's
	// change, which runPull refuses without --force (pullReasonUndeployed).
	undeployed []string
	// undeployedErr is a comparison that could not be made — refused the same
	// way, since a pull that cannot tell must not guess.
	undeployedErr error
	// kept names the folder's own changes, not yet deployed, that the pull
	// leaves as they are: the live row has not moved, so there is nothing to
	// absorb, and the next apply deploys them.
	kept []string
	// leftAlone is a plan that writes nothing and anchors nothing: the whole
	// unit is the folder's own change (kept).
	leftAlone bool
}

// pullRenderer reads the live rows of an adopted folder and plans its files.
type pullRenderer func(ctx context.Context, client *api.Client, f *folder) (*pullPlan, error)

func pullRendererFor(kind wfdir.Kind) pullRenderer {
	switch kind.Name {
	case wfdir.KindWorkflow:
		return pullWorkflow
	case wfdir.KindDataApp:
		return pullDataApp
	case wfdir.KindModule:
		return pullModule
	case wfdir.KindPipeline:
		return pullPipeline
	case wfdir.KindAutomation:
		return pullAutomation
	}
	return nil
}

func runPull(ctx context.Context, kind wfdir.Kind, dir string, opts pullOptions) (*pullReport, error) {
	resolved, err := resolveInstance()
	if err != nil {
		return nil, err
	}
	root, err := pullRoot(dir, kind)
	if err != nil {
		return nil, err
	}
	// Opened READ-ONLY: the refresh reads the server's state and refuses a
	// claim that is not usable (lost, trashed, moved out, unreadable, foreign)
	// with the outcome a deploy names, and nothing is settled — a pull applies
	// no `moved`, `removed` or lazy claim. The anchor step below is the one
	// server write it makes.
	f, err := openActingFolderAt(ctx, root, resolved, kind, false)
	if err != nil {
		return nil, err
	}
	if err := refuseUnpullable(f); err != nil {
		return nil, err
	}
	if !opts.DryRun && !opts.Force {
		if err := refuseUncommitted(f); err != nil {
			return nil, err
		}
	}

	client := newClient(f.Resolved.URL, f.Resolved.Token)
	plan, err := pullRendererFor(kind)(ctx, client, f)
	if err != nil {
		return nil, err
	}
	// The write gate, before anything is written, a dry run included: one
	// gate point for every kind — the same check the deploy makes, over the
	// live row the pull would take in.
	if plan.gate != nil && !plan.leftAlone {
		if err := plan.gate(ctx); err != nil {
			return nil, pullGateRefusal(f, err)
		}
	}

	if !opts.DryRun && !opts.Force {
		if plan.undeployedErr != nil {
			return nil, plan.undeployedErr
		}
		if len(plan.undeployed) > 0 {
			return nil, &managedRefusal{Reason: pullReasonUndeployed, Detail: fmt.Sprintf(
				"this folder changed %s since stack %q last applied it, and the live %s changed too — a pull would replace this folder's change, which was never deployed, with what is live. Deploy it first (`ronja sync apply --stack %s` refuses the live change as drift, so you can see both and merge them by hand), preview with `%s pull --dry-run`, or pull --force to replace this folder's change with what is live",
				strings.Join(plan.undeployed, ", "), f.Stack, kind.Label, f.Stack, kind.Command)}
		}
	}

	report := &pullReport{
		Root: f.Root, Kind: kind.Name, Stack: f.Stack, FeatureID: f.Binding.FeatureID,
		DryRun: opts.DryRun, Files: []pullFileReport{}, Literals: plan.literals, Notes: plan.notes,
		Replaced: plan.undeployed, Kept: plan.kept,
	}
	// A comparison that could not be made is said whenever the refusal it
	// earns was skipped: a dry run previews it rather than a clean diff, and
	// --force says it replaced files nobody could check.
	if err := plan.undeployedErr; err != nil {
		switch {
		case opts.Force:
			report.Notes = append(report.Notes, fmt.Sprintf("--force: %v — so whether this replaced a change of this folder's that was never deployed was not checked; the diff shows what it replaced", err))
		case opts.DryRun:
			report.Notes = append(report.Notes, fmt.Sprintf("a pull without --force would refuse: %v", err))
		}
	}
	paths := plan.paths()
	before := map[string]*string{}
	for _, p := range paths {
		before[p] = readDiskFile(f.Root, p)
	}
	if opts.DryRun {
		after := map[string]*string{}
		for _, p := range paths {
			after[p] = plan.contentOf(p, before[p])
		}
		report.fill(paths, before, after)
		return report, nil
	}

	if err := refuseSymlinked(f, plan.writes()); err != nil {
		return nil, err
	}
	if !opts.Force {
		if err := refuseUnrecoverable(f, plan.writes()); err != nil {
			return nil, err
		}
	}
	for _, p := range sortedKeys(plan.files) {
		if err := writePulledFile(f.Root, p, plan.files[p]); err != nil {
			return nil, err
		}
	}
	if plan.manifest != nil {
		if err := wfdir.SaveManifest(f.Root, plan.manifest); err != nil {
			return nil, fmt.Errorf("write %s: %w", wfdir.ManifestPath(f.Root), err)
		}
		f.Manifest = plan.manifest
	}

	if plan.leftAlone {
		return report, nil
	}
	pending, anchorErr := f.anchorPulled(ctx, plan)
	report.Pending = pending
	// The diff of what is now ON DISK, whatever the anchor step wrote beside
	// the files (it saves ronja.json as every deploy does).
	after := map[string]*string{}
	for _, p := range paths {
		after[p] = readDiskFile(f.Root, p)
	}
	report.fill(paths, before, after)
	if anchorErr != nil {
		// Written, and not recorded: --json says so.
		report.Anchored = false
		return report, anchorErr
	}
	report.Anchored = true
	if plan.afterAnchor != nil {
		if err := plan.afterAnchor(f); err != nil {
			fmt.Fprintf(os.Stderr, "  Note: could not refresh the local sync baseline (%v) — `%s status` may show the pulled files as local changes until the next push.\n", err, kind.Command)
		}
	}
	return report, nil
}

// paths is every file the plan may change, ronja.json last. ronja.json is
// always among them: the anchor step saves it as every deploy does, and the
// diff is of what is on disk afterwards.
func (p *pullPlan) paths() []string {
	return append(sortedKeys(p.files), wfdir.ManifestName)
}

// writes is every path the pull itself writes or deletes: the planned files,
// and ronja.json when its metadata changes.
func (p *pullPlan) writes() []string {
	out := sortedKeys(p.files)
	if p.manifest != nil {
		out = append(out, wfdir.ManifestName)
	}
	return out
}

// contentOf is the planned content of one path; before is what is on disk.
func (p *pullPlan) contentOf(path string, before *string) *string {
	if path == wfdir.ManifestName {
		if p.manifest == nil {
			return before
		}
		body, err := encodeManifest(p.manifest)
		if err != nil {
			return before
		}
		return &body
	}
	return p.files[path]
}

// encodeManifest is SaveManifest's bytes, without the write.
func encodeManifest(m *wfdir.Manifest) (string, error) {
	body, err := json.MarshalIndent(m, "", "  ")
	if err != nil {
		return "", err
	}
	return string(body) + "\n", nil
}

// copyManifest is a deep copy, through the file format itself.
func copyManifest(m *wfdir.Manifest) (*wfdir.Manifest, error) {
	body, err := json.Marshal(m)
	if err != nil {
		return nil, err
	}
	var out wfdir.Manifest
	if err := json.Unmarshal(body, &out); err != nil {
		return nil, err
	}
	return &out, nil
}

// withManifest is the folder as it would read with another manifest — for
// asking the push's own "would this write anything" questions of a planned
// manifest before it is written. The codec is the folder's, which a pull never
// changes (it touches no dependency or bind).
func (f *folder) withManifest(m *wfdir.Manifest) *folder {
	g := *f
	g.Manifest = m
	return &g
}

func readDiskFile(root, path string) *string {
	body, err := os.ReadFile(filepath.Join(root, filepath.FromSlash(path)))
	if err != nil {
		return nil
	}
	s := string(body)
	return &s
}

// writePulledFile writes one planned file, or deletes it.
func writePulledFile(root, path string, content *string) error {
	if content != nil {
		if err := wfdir.WriteFile(root, path, *content); err != nil {
			return fmt.Errorf("write %s: %w", path, err)
		}
		return nil
	}
	if err := os.Remove(filepath.Join(root, filepath.FromSlash(path))); err != nil && !errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("delete %s: %w", path, err)
	}
	return nil
}

// pullRoot is the folder a pull acts on: the one holding `dir`, or the working
// directory's.
func pullRoot(dir string, kind wfdir.Kind) (string, error) {
	if dir == "" {
		return folderRootHere(kind)
	}
	abs, err := filepath.Abs(dir)
	if err != nil {
		return "", fmt.Errorf("resolve %s: %w", dir, err)
	}
	root, err := wfdir.FindRoot(abs)
	if err != nil {
		if errors.Is(err, wfdir.ErrNoManifest) {
			return "", fmt.Errorf("no %s in %s or any parent directory — pull runs in a folder this repository deploys", wfdir.ManifestName, abs)
		}
		return "", err
	}
	return root, nil
}

// refuseUnpullable is every refusal that needs no file read: a folder that is
// not adopted on the selected stack, one whose state names nothing to pull,
// and one whose `moved` / `removed` declarations the server has not applied.
func refuseUnpullable(f *folder) error {
	if !f.Manifest.Adopted() {
		return &managedRefusal{Reason: syncReasonNotAdopted, Detail: fmt.Sprintf(
			"%s has no folder id, so it is not a folder a repository deploys from, and there is nothing to pull into it: a pull takes a change made elsewhere back into the repository that manages the row. To copy what is live into a fresh folder, use `%s clone`; to make this folder the one that manages its rows, run `ronja sync adopt --stack <name>` in it and commit %s",
			wfdir.ManifestName, f.Kind.Command, wfdir.ManifestName)}
	}
	if f.Stack == "" {
		return fmt.Errorf("%s names no stack this credential matches — name the stack to pull from with --stack <name>", wfdir.ManifestName)
	}
	if f.Manifest.Stacks[f.Stack].Unmanaged {
		return &managedRefusal{Reason: syncReasonNotAdopted, Detail: fmt.Sprintf(
			"stack %q is marked \"unmanaged\" in %s, so it deploys the classic way and no repository manages its rows on the server. Run `ronja sync adopt --stack %s` to manage them from here again, or clone a fresh copy with `%s clone`",
			f.Stack, wfdir.ManifestName, f.Stack, f.Kind.Command)}
	}
	if f.managed == nil {
		return &managedRefusal{Reason: pullReasonNotClaimed, Detail: fmt.Sprintf(
			"stack %q of this folder keeps no records on the server, so there is nothing to pull", f.Stack)}
	}
	st := f.managed
	if err := refuseUnholdableClaims(f); err != nil {
		return err
	}
	if wfdir.KeysRowsByFile(f.Kind) {
		if len(st.claims) == 0 {
			return &managedRefusal{Reason: pullReasonNotClaimed, Detail: fmt.Sprintf(
				"this folder manages nothing on stack %q yet — the server holds no claim under its id in feature %s. Deploy it first (`ronja sync apply --stack %s`), or claim what its lock records with `ronja sync adopt --stack %s`",
				f.Stack, f.Binding.FeatureID, f.Stack, f.Stack)}
		}
	} else if c := st.claims[""]; c == nil || c.State != api.ManagedStateOK {
		return &managedRefusal{Reason: pullReasonNotClaimed, Detail: fmt.Sprintf(
			"this %s is not managed from here on stack %q — the server holds no claim under this folder's id in feature %s. Deploy it first (`ronja sync apply --stack %s`), or claim the row its lock records with `ronja sync adopt --stack %s`",
			f.Kind.Label, f.Stack, f.Binding.FeatureID, f.Stack, f.Stack)}
	}
	if unclaimed := f.managedBackfill(); len(unclaimed) > 0 {
		var addrs []string
		for _, a := range unclaimed {
			addrs = append(addrs, orFolder(a.Address))
		}
		return &managedRefusal{Reason: pullReasonNotClaimed, Detail: fmt.Sprintf(
			"%s records %s on stack %q, and the server holds no claim for %s — a pull reads only what the server says this folder manages. Claim %s with `ronja sync adopt --stack %s`, then pull",
			wfdir.LockName, strings.Join(addrs, ", "), f.Stack, agree(len(addrs), "it", "them"), agree(len(addrs), "it", "them"), f.Stack)}
	}
	var pending []string
	for _, a := range st.release {
		pending = append(pending, fmt.Sprintf("%q in \"removed\"", a))
	}
	if wfdir.KeysRowsByFile(f.Kind) {
		out, err := f.resolveAddresses()
		if err != nil {
			return err
		}
		if out != nil {
			for _, m := range out.Moves {
				pending = append(pending, fmt.Sprintf("%s moved to %s", m.From, m.To))
			}
			for _, r := range out.Removals {
				pending = append(pending, fmt.Sprintf("%q in \"removed\"", r.Path))
			}
		}
	}
	if len(pending) > 0 {
		return &managedRefusal{Reason: pullReasonPending, Detail: fmt.Sprintf(
			"%s declares %s, which the server has not applied yet, so the folder and the server do not yet agree on which file holds which row. Deploy the declarations first (`ronja sync apply --stack %s`), then pull",
			wfdir.ManifestName, strings.Join(pending, ", "), f.Stack)}
	}
	return nil
}

// refuseUnholdableClaims refuses a pull whose claims name an address the folder
// cannot hold as a file. A pipeline or automation pull writes each claim's file
// AT the claim's address, and the server checks an address only for its shape —
// so anyone who can write a row could claim it under this folder's id (which is
// committed, and public with the repository) at `ronja.json` or
// `.github/workflows/x.json`, and the next pull would write there. The draft
// kinds refuse a live file path the same way (wfdir.CheckLocalPaths).
func refuseUnholdableClaims(f *folder) error {
	if !wfdir.KeysRowsByFile(f.Kind) {
		return nil
	}
	var bad []string
	for _, addr := range sortedClaimAddresses(f.managed.claims) {
		if err := wfdir.ValidatePath(addr); err != nil {
			bad = append(bad, fmt.Sprintf("%q (%v)", addr, err))
		} else if reason := wfdir.StructuralExclusion(addr, f.Kind); reason != "" {
			bad = append(bad, fmt.Sprintf("%q (%s)", addr, reason))
		}
	}
	if len(bad) == 0 {
		return nil
	}
	return &managedRefusal{Reason: pullReasonInexpressible, Detail: fmt.Sprintf(
		"the server records claims under this folder's id at %s — not files a %s folder can hold, so no deploy of this folder made them. Nothing was written. Release them (`ronja sync unmanage --stack %s`, then `ronja sync adopt --stack %s` claims this folder's own rows again) and find out who made them",
		strings.Join(bad, ", "), f.Kind.Label, f.Stack, f.Stack)}
}

// anchorPulled records the live rows as what this folder last applied, by the
// kind's own deploy push in its live-first mode, under the read-only guard. The
// state was opened read-only; this is where it may write — anchors, and
// nothing else (no `moved`, no `removed`, no lazy claim).
func (f *folder) anchorPulled(ctx context.Context, plan *pullPlan) ([]string, error) {
	guard := &api.ReadOnlyGuard{Allow: anchorWriteOnly}
	gctx := api.WithReadOnlyGuard(ctx, guard)
	f.ctx = gctx
	f.managed.readOnly = false
	f.managed.settled = true
	notes, err := plan.anchor(gctx, f)
	if blocked := guard.Blocked(); len(blocked) > 0 {
		return notes, &managedRefusal{Reason: pullReasonWouldWrite, Detail: fmt.Sprintf(
			"the pulled files are written, but recording them as applied would have sent %s, so nothing was sent and nothing is recorded on the server. The files are the live %s: commit them. If `%s status` then shows no difference, the next apply only records them (a live row the folder holds is never drift) and writes no row; if it shows one, that is what the next apply would send. Either way this is a bug in ronja worth reporting",
			strings.Join(blocked, ", "), f.Kind.Label, f.Kind.Command)}
	}
	if err != nil {
		return notes, fmt.Errorf("the pulled files are written, but recording the live state as applied failed: %w", err)
	}
	if f.managed.anchorErr != nil {
		return notes, fmt.Errorf("the pulled files are written, but the server's state was not updated: %w — pull again", f.managed.anchorErr)
	}
	return notes, nil
}

// pullGateRefusal is the write gate's refusal as a pull says it: the live row
// is the thing outside, not this folder's code.
func pullGateRefusal(f *folder, err error) error {
	r, ok := asManagedRefusal(err)
	if !ok {
		return err
	}
	return &managedRefusal{Reason: r.Reason, Detail: fmt.Sprintf(
		"the live %s on stack %q reaches outside the stack's feature, so this folder could not deploy it as it is — the next apply would refuse it — and a pull does not take it in. Bind what it reaches for this stack as described below and pull again, or undo the change in Ronja. %s",
		f.Kind.Label, f.Stack, r.Detail)}
}

// anchorWriteOnly admits the two routes an anchor is recorded through —
// PUT /managed/:id/applied and PUT /managed/folder/applied — and the write
// gate's POST /workflow/validate, which persists nothing: the live-first push
// that records a workflow's anchor gates the folder first.
func anchorWriteOnly(method, path string) bool {
	if method == "POST" && path == "workflow/validate" {
		return true
	}
	if method != "PUT" {
		return false
	}
	if path == "managed/folder/applied" {
		return true
	}
	rest, ok := strings.CutPrefix(path, "managed/")
	if !ok {
		return false
	}
	id, ok := strings.CutSuffix(rest, "/applied")
	return ok && id != "" && !strings.Contains(id, "/")
}

// pullHint is the remedy a drift refusal names beside --force in an ADOPTED
// folder: `ronja <kind> pull` takes the server's version into the folder, and
// the next apply writes nothing. "" for a classic folder, whose remedies
// stay what they were — it has nothing to pull into.
func (f *folder) pullHint(indent string) string {
	if f == nil || f.Manifest == nil || !f.adoptedHere() {
		return ""
	}
	return fmt.Sprintf("\n%sOr, to take the change made on the server into this folder instead: `%s pull` rewrites the folder with what is live, and refuses if that would replace a change of this folder's that was never deployed — commit what it writes, and the next apply writes nothing.", indent, f.Kind.Command)
}

// markerLiterals names every id-form marker argument in `after` that `before`
// did not already spell: a reference the live row gained that no alias of this
// folder names, written as the literal id. A literal the folder already
// carried is the folder's own choice, and is not news.
func markerLiterals(path string, before *string, after string) []pullLiteral {
	seen := map[string]bool{}
	if before != nil {
		for _, o := range markers.Scan(*before) {
			seen[o.Family.Kind+"\x00"+o.Arg] = true
		}
	}
	var out []pullLiteral
	for i, line := range strings.Split(after, "\n") {
		for _, o := range markers.Scan(line) {
			if o.Positional || !markers.IsResourceID(o.Family.Kind, o.Arg) || seen[o.Family.Kind+"\x00"+o.Arg] {
				continue
			}
			out = append(out, pullLiteral{Path: path, Line: i + 1, Kind: o.Family.Kind, ID: o.Arg})
		}
	}
	return out
}

// fileSetChanges plans the files of a folder kind whose files are a set: every
// live file in its disk form, every local syncable file live no longer holds
// deleted. Only paths whose content changes.
func fileSetChanges(local, remote map[string]string) map[string]*string {
	out := map[string]*string{}
	for path, content := range remote {
		if have, ok := local[path]; ok && have == content {
			continue
		}
		c := content
		out[path] = &c
	}
	for path := range local {
		if _, ok := remote[path]; !ok {
			out[path] = nil
		}
	}
	return out
}
