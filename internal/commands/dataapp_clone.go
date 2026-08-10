package commands

import (
	"fmt"
	"os"
	"path/filepath"

	"github.com/ronjatech/ronja-cli/internal/api"
	"github.com/ronjatech/ronja-cli/internal/wfdir"
	"github.com/spf13/cobra"
)

// `ronja app clone` copies an existing data app into a new local folder.
//
// READ-ONLY: no draft is created here, so cloning to read someone's app leaves
// nothing behind. Checkout happens on the first push.
func newDataAppCloneCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "clone <data-app-id> [directory]",
		Short: "Copy a data app into a new local folder",
		Long: `Copy a data app into a new local folder.

Fetches the app's source files and writes them into a directory (named after
the app's title unless you say otherwise), together with ronja.json and a
.ronja/ sync baseline:

  ronja app clone data_app-abc123
  ronja app clone data_app-abc123 ./revenue-explorer

If you already have an open draft of that app — from the web builder, or from
an earlier push — its files are cloned instead of the live ones, because that
is your newest state. This is said on stderr when it happens.

The app's allowlists come down into ronja.json's "access" block, so a clone is
a complete description of the app rather than only its source.

Nothing is created on the server: cloning does not open a draft. The target
directory must be empty or absent. Only live apps and drafts can be cloned;
versions, archived apps and proposals are refused with a pointer at the right
flow.`,
		Args: cobra.RangeArgs(1, 2),
		RunE: func(cmd *cobra.Command, args []string) error {
			resolved, err := resolveInstance()
			if err != nil {
				return err
			}
			client := api.New(resolved.URL, resolved.Token)
			appID := args[0]

			// An explicitly named directory can be rejected before a single
			// request: "that folder is not empty" does not become truer after three
			// round trips, and this is the form CI uses.
			root := ""
			if len(args) == 2 {
				if root, err = checkTarget(args[1]); err != nil {
					return err
				}
			}

			app, err := client.GetDataApp(cmd.Context(), appID)
			if err != nil {
				if api.StatusOf(err) == 404 {
					return fmt.Errorf("no data app %s on %s that you can read", appID, resolved.URL)
				}
				return err
			}
			if err := refuseUnworkableApp(app); err != nil {
				return err
			}

			// Prefer the caller's own draft: it is their newest state, and cloning
			// live over it would look like their web-builder edits had vanished.
			//
			// Resolved EXPLICITLY rather than left to GET :id/files, which would
			// silently answer with the draft anyway (see api.ListDataAppFiles). The
			// difference is that here we know WHICH row answered, and the baseline
			// records that row — a baseline naming the live app while holding draft
			// bytes is a lie every later command believes.
			source := app
			if app.Lifecycle == api.LifecycleLive {
				draft, draftErr := client.GetDataAppDraft(cmd.Context(), app.ID)
				switch {
				case draftErr != nil:
					fmt.Fprintf(os.Stderr,
						"  Note: could not check for your open draft (%v) — cloning the live version.\n",
						draftErr)
				case draft != nil:
					source = draft
					fmt.Fprintln(os.Stderr,
						"  Note: cloning your open draft (newer than the live version).")
				}
			}

			// The default name needs a title, so this one waits for the source — but
			// it still precedes the file fetch, which is the expensive leg.
			if root == "" {
				if root, err = checkTarget(wfdir.Slug(source.Name, wfdir.KindDataApp)); err != nil {
					return err
				}
			}

			files, err := client.ListDataAppFiles(cmd.Context(), source.ID)
			if err != nil {
				return fmt.Errorf("read files of data app %s: %w", source.ID, err)
			}
			// Checked BEFORE anything is created, and fatal rather than per-file:
			// the baseline written below claims every one of these paths is on disk,
			// and a set that cannot all be written produces a folder whose baseline
			// lies from birth — status invents deletions and a push acts on them.
			// The KIND is what makes this correct for a data app: a remote
			// dist/bundle.js is a legal server-side path that this folder's walk
			// would never see again.
			if err := wfdir.CheckLocalPaths(appPathsOf(files), wfdir.DataAppKind); err != nil {
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

			entrypoint := source.Entrypoint
			if entrypoint == "" {
				entrypoint = wfdir.DataAppKind.DefaultEntrypoint
			}
			// The binding is keyed by organization as well as instance, so it has to
			// be known before one is written.
			if err := ensureTenant(cmd.Context(), resolved); err != nil {
				return err
			}
			key := wfdir.InstanceKey{URL: resolved.URL, TenantID: resolved.TenantID}
			manifest := &wfdir.Manifest{
				Kind:       wfdir.KindDataApp,
				Title:      source.Name,
				Entrypoint: entrypoint,
			}
			// The STABLE identity, not source.ID: when the files came from a draft,
			// the draft's id dies at commit while the binding lives in the
			// customer's git.
			manifest.SetBinding(key, wfdir.Binding{
				DataAppID: app.IdentityID(),
				FeatureID: source.FeatureID,
			})
			// Always written, even when the app grants nothing: a clone is a
			// faithful copy, and a folder that came down without the key would
			// silently not manage the allowlists it can plainly see — so the next
			// push would leave them alone while `status` showed them.
			manifest.SetAccess(source.DataAppAccess)
			if err := wfdir.SaveManifest(root, manifest); err != nil {
				return err
			}
			state := &wfdir.State{}
			// The baseline records the row the files actually came from, which may
			// be the draft even though the binding names the live app.
			state.Set(key, baselineFromApp(source, files))
			if err := wfdir.SaveState(root, state); err != nil {
				return err
			}

			if flagJSON {
				return emitJSON(map[string]any{
					"root":            root,
					"url":             resolved.URL,
					"dataAppID":       app.IdentityID(),
					"featureID":       source.FeatureID,
					"title":           source.Name,
					"entrypoint":      entrypoint,
					"lifecycle":       app.Lifecycle,
					"sourceID":        source.ID,
					"sourceLifecycle": source.Lifecycle,
					"clonedFromDraft": source.ID != app.ID,
					"access":          source.DataAppAccess.Normalized(),
					// No skipped count: a clone that could not write every file
					// fails, so this is always the whole set.
					"files": len(files),
				})
			}
			printAppCloneReport(root, resolved.URL, app, source, entrypoint, len(files))
			return nil
		},
	}
	return cmd
}

func printAppCloneReport(root, url string, app, source *api.DataApp, entrypoint string, written int) {
	out := os.Stdout
	fmt.Fprintf(out, "  Cloned %q into %s\n\n", source.Name, root)
	fmt.Fprintf(out, "  Data app:   %s (%s)\n", app.IdentityID(), api.DescribeLifecycle(app.Lifecycle))
	if source.ID != app.ID {
		fmt.Fprintf(out, "  Source:     your draft %s\n", source.ID)
	}
	fmt.Fprintf(out, "  Feature:    %s\n", source.FeatureID)
	fmt.Fprintf(out, "  Entrypoint: %s\n", entrypoint)
	fmt.Fprintf(out, "  Access:     %s\n", describeAccess(source.DataAppAccess))
	fmt.Fprintf(out, "  Files:      %d\n", written)
	fmt.Fprintf(out, "  Instance:   %s\n", url)
	fmt.Fprintf(out, "\n  Next: cd %s && ronja app status\n", filepath.Base(root))
}
