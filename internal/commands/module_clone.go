package commands

import (
	"fmt"
	"os"
	"path/filepath"

	"github.com/ronjatech/ronja-cli/internal/api"
	"github.com/ronjatech/ronja-cli/internal/wfdir"
	"github.com/spf13/cobra"
)

// `ronja module clone` copies an existing module into a new local folder.
//
// READ-ONLY, exactly like `wf clone`: no draft is created here. Checkout
// happens on the first push, so cloning to READ a shared module — which is a
// perfectly ordinary thing to want, since a module is by definition somebody
// else's code you are about to import — leaves nothing behind.
func newModuleCloneCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "clone <module-id> [directory]",
		Short: "Copy a module into a new local folder",
		Long: `Copy a module into a new local folder.

Fetches the module's .py files and writes them into a directory (named after the
module's package name unless you say otherwise), together with ronja.json and a
.ronja/ sync baseline:

  ronja module clone module-abc123
  ronja module clone module-abc123 ./emab-tracker

If the module has an open draft — from an earlier push, or from a colleague —
its files are cloned instead of the live ones, because that is the newest state
and the row your next push would write to. This is said on stderr when it
happens.

Nothing is created on the server: cloning does not open a draft. The target
directory must be empty or absent. Committed VERSIONS cannot be cloned — they
are the immutable snapshots consumer workflows pin, so there is nothing to sync
back into.`,
		Args: cobra.RangeArgs(1, 2),
		RunE: func(cmd *cobra.Command, args []string) error {
			resolved, err := resolveInstance()
			if err != nil {
				return err
			}
			client := api.New(resolved.URL, resolved.Token)
			moduleID := args[0]

			// An explicitly named directory can be rejected before a single
			// request: "that folder is not empty" does not become truer after
			// three round trips, and this is the form CI uses.
			root := ""
			if len(args) == 2 {
				if root, err = checkTarget(args[1]); err != nil {
					return err
				}
			}

			mod, err := client.GetModule(cmd.Context(), moduleID)
			if err != nil {
				if api.StatusOf(err) == 404 {
					return fmt.Errorf("no module %s on %s that you can read", moduleID, resolved.URL)
				}
				return err
			}
			if err := refuseUnclonableModule(mod); err != nil {
				return err
			}

			// Prefer the open draft: it is the newest state, and the row a push
			// would write to. Best-effort — a failure here degrades to the live
			// files rather than aborting a clone that can perfectly well proceed.
			//
			// ⚠️ Unlike a workflow's, a module draft is NOT per-user: there is
			// one draft per module (a draft is a snapshot of the whole file set,
			// so two would each drop the other's edits at commit). So this may be
			// a COLLEAGUE's open draft, and cloning it is right for the same
			// reason cloning your own is — it is the row your push lands in.
			source := mod
			if mod.Lifecycle == api.LifecycleLive {
				draft, draftErr := client.GetModuleDraft(cmd.Context(), mod.ID)
				switch {
				case draftErr != nil:
					fmt.Fprintf(os.Stderr,
						"  Note: could not check for an open draft (%v) — cloning the live version.\n",
						draftErr)
				case draft != nil:
					source = draft
					fmt.Fprintf(os.Stderr,
						"  Note: cloning the open draft %s (newer than the live version) — a module has ONE draft, so it may be a colleague's.\n",
						draft.ID)
				}
			}

			// The default name needs the package name, so this one waits for the
			// source — but it still precedes the file fetch, the expensive leg.
			//
			// FileSlug rather than Slug: a package name is an identifier whose
			// underscores are content (emab_tracker, not emab-tracker), and the
			// directory should read like the thing it holds.
			if root == "" {
				if root, err = checkTarget(wfdir.FileSlug(source.Name, wfdir.KindModule)); err != nil {
					return err
				}
			}

			files, err := client.ListModuleFiles(cmd.Context(), source.ID)
			if err != nil {
				return fmt.Errorf("read files of module %s: %w", source.ID, err)
			}
			// Checked BEFORE anything is created, and fatal rather than per-file:
			// the baseline written below claims every one of these paths is on
			// disk, and a set that cannot all be written produces a folder whose
			// baseline lies from birth — status invents deletions and a push acts
			// on them. See wfdir.CheckLocalPaths.
			//
			// It also enforces the .py rule from the OTHER side: a module folder
			// syncs .py and ignores everything else, so a non-.py file the server
			// somehow holds would be written once and then invisible to every
			// walk. Today the server refuses to store one, which makes this a
			// guard rather than a case anyone hits — and exactly the kind of
			// guard whose absence is discovered by losing somebody's file.
			if err := wfdir.CheckLocalPaths(modulePathsOf(files), wfdir.ModuleKind); err != nil {
				return err
			}

			if err := os.MkdirAll(root, 0o755); err != nil {
				return fmt.Errorf("create %s: %w", root, err)
			}
			for _, f := range files {
				// Validated once more inside WriteFile: the path arrives from the
				// server, and writing it is a local filesystem operation that must
				// not be able to escape the folder.
				if err := wfdir.WriteFile(root, f.Path, f.Content); err != nil {
					return fmt.Errorf("write %s: %w", f.Path, err)
				}
			}

			// The binding is keyed by organization as well as instance, so it has
			// to be known before one is written. Resolved here rather than up
			// front so the cheap refusals above still cost no round trip.
			if err := ensureTenant(cmd.Context(), resolved); err != nil {
				return err
			}
			key := wfdir.InstanceKey{URL: resolved.URL, TenantID: resolved.TenantID}
			manifest := &wfdir.Manifest{
				Kind:  wfdir.KindModule,
				Name:  source.Name,
				Title: source.Title,
			}
			// The STABLE identity, not source.ID: when the files came from a
			// draft, the draft's id dies at commit while the binding lives in the
			// customer's git.
			lock, err := recordFirstBinding(manifest, key,
				wfdir.Binding{ModuleID: mod.IdentityID(), FeatureID: source.FeatureID})
			if err != nil {
				return err
			}
			// The committed anchor: which live version this folder's content
			// descends from. A clone is the cleanest moment there is to record
			// one, and recording it here is what lets a CI checkout of the result
			// push without --force.
			//
			// ⚠️ Read off the SOURCE, not off the current head, and the two are
			// not the same when the files came from a draft: a draft forked before
			// somebody else published sits on an OLDER version, and a folder that
			// recorded the newer one would claim to have seen a publish whose
			// changes it does not contain — after which the first baseline-less
			// push would quietly overwrite it.
			//
			// cloneAnchor, not dataAppCloneAnchor: rmodule stamps base_version_id
			// the way rworkflow does (the head VERSION row's id, falling back to
			// the parent's own id for an unversioned module), so the workflow
			// function's three cases are literally this primitive's three cases.
			// The rdataapp divergence that forced a second function — a base that
			// holds the LIVE APP'S id for every draft — does not exist here.
			if flagStack != "" {
				anchor, err := cloneAnchor(cmd.Context(), moduleHeadReader(client),
					mod.IdentityID(), source.ID, source.BaseVersionID)
				if err != nil {
					return err
				}
				lock.SetHeadVersion(flagStack, anchor)
			}
			if err := wfdir.SaveFolder(root, manifest, lock); err != nil {
				return err
			}
			state := &wfdir.State{}
			// The baseline records the row the files actually came from, which may
			// be the draft even though the binding names the live module.
			state.Set(key, moduleBaselineFrom(source, files))
			if err := wfdir.SaveState(root, state); err != nil {
				return err
			}

			if flagJSON {
				return emitJSON(map[string]any{
					"root":            root,
					"url":             resolved.URL,
					"moduleID":        mod.IdentityID(),
					"featureID":       source.FeatureID,
					"name":            source.Name,
					"title":           source.Title,
					"lifecycle":       mod.Lifecycle,
					"sourceID":        source.ID,
					"sourceLifecycle": source.Lifecycle,
					"clonedFromDraft": source.ID != mod.ID,
					// No skipped count: a clone that could not write every file
					// fails, so this is always the whole set.
					"files": len(files),
				})
			}
			printModuleCloneReport(root, resolved.URL, mod, source, len(files))
			return nil
		},
	}
	return cmd
}

func printModuleCloneReport(root, url string, mod, source *api.Module, written int) {
	out := os.Stdout
	fmt.Fprintf(out, "  Cloned %q into %s\n\n", source.Name, root)
	fmt.Fprintf(out, "  Module:   %s (%s)\n", mod.IdentityID(), api.DescribeLifecycle(mod.Lifecycle))
	if source.ID != mod.ID {
		fmt.Fprintf(out, "  Source:   the open draft %s\n", source.ID)
	}
	fmt.Fprintf(out, "  Package:  %s\n", source.Name)
	fmt.Fprintf(out, "  Title:    %s\n", source.Title)
	fmt.Fprintf(out, "  Feature:  %s\n", source.FeatureID)
	fmt.Fprintf(out, "  Files:    %d\n", written)
	fmt.Fprintf(out, "  Instance: %s\n", url)
	fmt.Fprintf(out, "\n  Next: cd %s && ronja module status\n", filepath.Base(root))
}
