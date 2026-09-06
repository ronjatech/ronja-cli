package commands

import (
	"context"
	"fmt"
	"os"
	"strings"
	"time"

	"github.com/ronjatech/ronja-cli/internal/api"
	"github.com/ronjatech/ronja-cli/internal/wfdir"
	"github.com/spf13/cobra"
)

// `ronja module` is the fifth sync loop, and it is the workflow loop with the
// workflow-only half removed rather than a new design.
//
// The doctrine is the same one `ronja wf` states: sync verbs yes, resource
// verbs no. What justifies a command group here is a loop HTTP does badly —
// stateful (which module am I bound to, on which instance), multi-step
// (checkout, per-file sync, commit) and needing a drift baseline that lives on
// disk. Discovery still is not: there is deliberately no `module list` and no
// `module delete`.
//
// ⚠️ THERE IS NO `run` AND NO `test`, and the absence is the primitive rather
// than an omission. A module is a package, not a program: no entrypoint, no
// parameters, no run identity. Testing a change to one means running a CONSUMER
// — edit the module draft, then `ronja wf test` in a workflow folder that
// imports it, which resolves the runner's own module draft on purpose (plan
// §3.4). A `module test` would have to invent a workflow to run it in, and the
// invented one is the thing that would be wrong.
func newModuleCmd() *cobra.Command {
	mod := &cobra.Command{
		Use:     "module",
		Aliases: []string{"mod"},
		Short:   "Develop a shared Python module from a local folder",
		Long: `Develop a shared Python module from a local folder.

A module is a versioned, named Python package that workflows IMPORT instead of
copying helper files between folders. A module folder holds your .py sources
plus a small manifest (ronja.json) recording the package name, the title, and
which module it maps to on each instance. The sync baseline lives in .ronja/,
which is local-only and git-ignored for you.

Two ways in:

  ronja module clone <module-id>     copy an existing module down to a folder
  ronja module init --feature <id>   turn a folder of helpers into one

Then edit locally and check where you stand:

  ronja module status                local changes, remote state, and drift
  ronja module validate              check the folder locally, save nothing
  ronja module push                  sync the folder into the module's draft
  ronja module publish               take the draft live (or ask an admin to)
  ronja module discard               throw the draft away

A module has no entrypoint and nothing to run, so there is no ` + "`test`" + ` and no
` + "`run`" + `: test a change by running a workflow that imports it (` + "`ronja wf test`" + `),
which resolves your own open module draft.

Consumers import it with a marker in import position:

  from {{ module('module-abc123') }} import tracker

Publishing a module changes NOTHING for a workflow that imports it until that
workflow is itself republished — a workflow pins the module version its author
last saved and tested with.

Bindings are per-instance, so the same folder can target a local backend and
production without either overwriting the other's binding.`,
	}
	mod.AddCommand(
		newModuleInitCmd(), newModuleCloneCmd(), newModuleStatusCmd(),
		newModuleValidateCmd(), newModulePushCmd(), newModulePublishCmd(),
		newModuleDiscardCmd(),
	)
	addStackFlag(mod)
	return mod
}

// moduleInitFile is the file that makes a module an importable package.
//
// The SERVER seeds one in the create transaction, so a module always has it —
// which is why the first push reads the row's files before writing anything,
// exactly as the workflow loop does for its seeded entrypoint. `module init`
// seeds one LOCALLY too, and the two are not redundant: an init that wrote no
// __init__.py would leave the folder in a state where the first push creates
// the module, reads back the server's seed, finds no local file at that path
// and DELETES it — which the server refuses, stopping the very first push.
//
// Aliased from wfdir rather than spelled again: the folder package has to know
// the name too (ModuleInitPath), and two spellings of one filename is exactly
// the shape that goes wrong quietly.
const moduleInitFile = wfdir.ModuleInitName

// maxModuleFileBytes caps one module file, mirroring the server's own per-file
// limit (rmodule's 1 MiB, itself the workflow number — module sources travel in
// the same run payload as the importing workflow's own files).
//
// Deliberately the same constant value as maxFileBytes rather than an alias of
// it: they are two servers' limits that happen to agree today, and a future
// divergence should be a one-line edit here rather than a puzzle about which
// loop the shared constant belongs to.
const maxModuleFileBytes = 1 << 20

// refuseUnclonableModule reports why a module row cannot back a folder, or nil
// when it can.
//
// Four workable lifecycles where a workflow has two, and the extra one is the
// point: a brand-new module in a SHARED feature is created `proposed`, not
// `draft` (a module edit fans out to every importer, so it gets the review a
// workflow edit gets). A loop that refused `proposed` the way `wf clone` does
// would refuse the folder that just created it — its own first push's row.
func refuseUnclonableModule(m *api.Module) error {
	switch m.Lifecycle {
	case api.LifecycleLive, api.LifecycleDraft, api.LifecycleProposed:
		return nil
	case api.LifecycleVersion:
		return fmt.Errorf("module %s is a committed version snapshot, not an editable module — a workflow pins it, so it can never be written to; clone the live module instead",
			m.ID)
	case api.LifecycleArchived:
		return fmt.Errorf("module %s is archived — restore it from the web UI first", m.ID)
	default:
		return fmt.Errorf("module %s has lifecycle %s, which this command does not work with",
			m.ID, api.DescribeLifecycle(m.Lifecycle))
	}
}

// moduleIsUnpublished reports a row that IS the module rather than an edit
// shadow of one: a parentless draft (private feature) or a proposal (shared).
//
// The two branch together everywhere in this loop — neither has a live version
// behind it, so both make `publish` a first publish, both make `discard` a
// deletion, and both are their own head — which is exactly why this is one
// predicate rather than two lifecycle comparisons repeated at six sites.
func moduleIsUnpublished(m *api.Module) bool {
	if m == nil {
		return false
	}
	return m.ParentModuleID == "" &&
		(m.Lifecycle == api.LifecycleDraft || m.Lifecycle == api.LifecycleProposed)
}

// hashModuleFiles fingerprints a fetched file set for baseline and drift
// comparison.
func hashModuleFiles(files []api.ModuleFile) map[string]string {
	out := make(map[string]string, len(files))
	for _, f := range files {
		out[f.Path] = wfdir.HashString(f.Content)
	}
	return out
}

// moduleContentByPath flattens a fetched file set for content comparison.
func moduleContentByPath(files []api.ModuleFile) map[string]string {
	out := make(map[string]string, len(files))
	for _, f := range files {
		out[f.Path] = f.Content
	}
	return out
}

// modulePathsOf lists a fetched file set's paths, for CheckLocalPaths.
func modulePathsOf(files []api.ModuleFile) []string {
	out := make([]string, 0, len(files))
	for _, f := range files {
		out = append(out, f.Path)
	}
	return out
}

// moduleBaselineFrom builds the state entry recorded after a successful sync
// with one module row.
//
// Name and Title are recorded from the ROW, never from the manifest, for the
// reason baselineFrom records a workflow's: the baseline's job is to say what
// the server held at the last sync, and the drift guard compares the server
// against it. Recording what we WANTED it to be would make the guard agree with
// itself forever.
//
// Entrypoint stays empty — a module has none — and every parameter/zone/access
// field stays nil, which reads as "not part of this baseline" everywhere and is
// the honest answer for a primitive that has none of them.
func moduleBaselineFrom(source *api.Module, files []api.ModuleFile) *wfdir.InstanceState {
	inst := &wfdir.InstanceState{
		SourceID:          source.ID,
		SourceLifecycle:   source.Lifecycle,
		BaselineUpdatedAt: source.UpdatedAt.UTC().Format(time.RFC3339),
		Title:             source.Title,
		Name:              source.Name,
		Files:             map[string]wfdir.FileState{},
	}
	for _, f := range files {
		inst.Files[f.Path] = wfdir.FileState{
			SHA256:    wfdir.HashString(f.Content),
			UpdatedAt: f.UpdatedAt.UTC().Format(time.RFC3339),
		}
	}
	return inst
}

// moduleBaselineFromLocal is moduleBaselineFrom for content the CLI just WROTE
// rather than read back: after a push, the server holds exactly the bytes we
// sent. Per-file UpdatedAt is left empty — the field is diagnostics only, and
// inventing timestamps we did not receive would be worse than saying nothing.
//
// TWO rows, unlike moduleBaselineFrom's one. `source` is the row the files were
// written to — a draft, usually — and `identity` is the row whose name and title
// the server actually holds (see moduleIdentityRow). They are the same row only
// while the module is unpublished; recording the draft's copied metadata is what
// made an ordinary title edit read as drift on the next push.
func moduleBaselineFromLocal(source, identity *api.Module, files map[string]string) *wfdir.InstanceState {
	inst := &wfdir.InstanceState{
		SourceID:          source.ID,
		SourceLifecycle:   source.Lifecycle,
		BaselineUpdatedAt: source.UpdatedAt.UTC().Format(time.RFC3339),
		Title:             identity.Title,
		Name:              identity.Name,
		Files:             map[string]wfdir.FileState{},
	}
	for path, content := range files {
		inst.Files[path] = wfdir.FileState{SHA256: wfdir.HashString(content)}
	}
	return inst
}

// moduleFeatureIDFor works out which feature a module folder's create belongs
// to — the manifest's binding, or the bound row's own feature for a
// hand-written manifest that names a module but no feature.
//
// The workflow loop's featureIDFor cannot be reused: it takes an *api.Workflow.
// The MESSAGES are shared through folder.featureAdvice /
// featureAdviceForAnotherOrganization, which is where the per-shape wording
// (stack vs legacy entry, this organization vs another) actually lives.
func moduleFeatureIDFor(f *folder, known *api.Module) (string, error) {
	if f.Binding.FeatureID != "" {
		return f.Binding.FeatureID, nil
	}
	if known != nil && known.FeatureID != "" {
		return known.FeatureID, nil
	}
	if others := f.otherOrganizationsOn(); !f.Bound && len(others) > 0 {
		return "", fmt.Errorf("no feature recorded for organization %s on %s in %s — the entries there name %s instead, and a module's ids belong to the organization that holds them.\n  %s, or recreate the folder with `ronja module init --feature <id>`",
			f.Key.TenantID, f.Resolved.URL, wfdir.ManifestPath(f.Root),
			strings.Join(others, ", "), f.featureAdviceForAnotherOrganization())
	}
	return "", fmt.Errorf("no feature recorded for %s — %s, or recreate the folder with `ronja module init --feature <id>`",
		describeTarget(f.Resolved), f.featureAdvice())
}

// moduleMetadataPatch is what a push would change about the LIVE module's
// metadata, empty when the folder and the row already agree.
//
// A field is sent only when the manifest DECLARES it (non-empty) and the row
// disagrees — the Title rule the workflow loop already follows, applied to Name
// as well. An empty declaration means "this folder does not manage that field",
// never "clear it": the server would refuse an empty name anyway, and a folder
// whose manifest lost its title must not be able to blank the module's.
func moduleMetadataPatch(m *wfdir.Manifest, row *api.Module) api.ModulePatch {
	var patch api.ModulePatch
	if title := strings.TrimSpace(m.Title); title != "" && title != row.Title {
		patch.Title = title
	}
	if name := strings.TrimSpace(m.Name); name != "" && name != row.Name {
		patch.Name = name
	}
	return patch
}

// applyModulePatch mirrors an ACCEPTED patch onto the row in hand, so a
// baseline recorded after it describes what the server now holds rather than
// what it held a request ago. The workflow loop's applyPatch, for two fields.
func applyModulePatch(row *api.Module, patch api.ModulePatch) {
	if patch.Title != "" {
		row.Title = patch.Title
	}
	if patch.Name != "" {
		row.Name = patch.Name
	}
}

// describeModulePatch names what a metadata patch changes, for the report and
// for the error a failed one produces.
func describeModulePatch(patch api.ModulePatch) string {
	var parts []string
	if patch.Title != "" {
		parts = append(parts, "the title")
	}
	if patch.Name != "" {
		parts = append(parts, fmt.Sprintf("the package name to %q", patch.Name))
	}
	return strings.Join(parts, " and ")
}

// moduleHeadReader is the head-version reader for the shared anchor machinery
// in head_version.go. One line, and it is the whole cost of reusing the
// workflow loop's three-layer drift guard verbatim.
func moduleHeadReader(client *api.Client) headReader { return client.ModuleHeadVersionID }

// noteModuleSkipped reports the files the walk left out, so a `.md` or a
// fixture in the folder is visible as "not synced" rather than silently absent
// from the module.
func noteModuleSkipped(enumeration *wfdir.Enumeration) {
	for _, s := range enumeration.Skipped {
		fmt.Fprintf(os.Stderr, "  Note: skipping %s — %s\n", s.Path, s.Reason)
	}
}

// refreshModuleBaselineFromLive re-reads the now-live module and resets the
// sync baseline to it, reporting what went wrong when it cannot.
//
// It reports rather than prints because the CALLER decides what a failure
// means, and for every caller here it means "note it and carry on": the
// server-side change has already happened, and failing the command afterwards
// would report something that did happen as something that did not.
// noteBaselineRefresh is the shared degrader.
func refreshModuleBaselineFromLive(ctx context.Context, client *api.Client, f *folder, liveID string, anchor anchorPolicy) error {
	live, err := client.GetModule(ctx, liveID)
	if err != nil {
		return fmt.Errorf("re-read %s: %w", liveID, err)
	}
	files, err := client.ListModuleFiles(ctx, liveID)
	if err != nil {
		return fmt.Errorf("re-read the files of %s: %w", liveID, err)
	}
	// The same gate `module clone` applies before it writes a byte: a baseline
	// is a promise that these paths are on disk, and a path no local walk can
	// ever produce is a promise the next status reads as a LOCAL DELETION and
	// the next push acts on by deleting the file server-side.
	if err := wfdir.CheckLocalPaths(modulePathsOf(files), f.Kind); err != nil {
		return fmt.Errorf("the files of %s cannot all be tracked locally: %w", liveID, err)
	}
	f.State.Set(f.Key, moduleBaselineFrom(live, files))
	if err := wfdir.SaveState(f.Root, f.State); err != nil {
		return fmt.Errorf("write the local baseline: %w", err)
	}
	if anchor != anchorOnLive {
		return nil
	}
	return reanchorOnLive(ctx, moduleHeadReader(client), f, liveID)
}
