package commands

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/ronjatech/ronja-cli/internal/wfdir"
	"github.com/spf13/cobra"
)

// `ronja pipeline init` turns a directory into a pipeline folder WITHOUT
// creating anything server-side.
//
// Nothing remote is the point, and it matters more here than for a workflow: a
// pipeline folder is a SET of tables, and a create-then-fix loop against a half
// built set leaves a feature holding tables nobody meant to make. Here the first
// push is the first thing that exists, and it creates exactly the files that are
// in the folder by then.
func newPipelineInitCmd() *cobra.Command {
	var featureID string
	var title string

	cmd := &cobra.Command{
		Use:   "init [directory]",
		Short: "Set up a pipeline folder",
		Long: `Set up a pipeline folder.

Writes ronja.json (the manifest) and .ronja/ (the local sync baseline, already
git-ignored). Nothing is created on the server — each table comes into
existence on the first push of the .sql file that describes it.

  ronja pipeline init --feature collection-abc123
  ronja pipeline init ./sales --feature collection-abc123 --title "Sales pipeline"

--feature names the feature the tables will be created in; find it with
GET /api/v2/feature/query. It is optional here, but a push refuses without one,
so passing it now is the difference between a folder that works and one that
stops at the first push.

Every .sql file in the folder is one derived table. Anything else is ignored.`,
		Args: cobra.MaximumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			featureID = strings.TrimSpace(featureID)
			// Checked by SHAPE, before anything else happens. A typo here is
			// otherwise invisible until the first push, which is several files and
			// possibly several days later — and what it reports there is a create
			// that failed, not a manifest that was wrong from the moment it was
			// written. This does not (and cannot) say the feature exists; it says
			// the id is not one.
			if err := checkFeatureIDShape(featureID); err != nil {
				return err
			}

			// The resolved instance AND organization are the KEY the binding is
			// written under, so a wrong or missing one produces a manifest no
			// later command matches. Nothing else here talks to the server; the
			// organization costs one request only when the credential came from
			// the environment rather than a stored profile.
			resolved, err := resolveInstance()
			if err != nil {
				return err
			}
			if err := ensureTenant(cmd.Context(), resolved); err != nil {
				return err
			}

			root, err := os.Getwd()
			if err != nil {
				return fmt.Errorf("locate working directory: %w", err)
			}
			if len(args) == 1 {
				if root, err = filepath.Abs(args[0]); err != nil {
					return fmt.Errorf("resolve %s: %w", args[0], err)
				}
			}
			// The refusal comes BEFORE the directory is created, so a second `init`
			// on an existing folder — or one that names a path by mistake — leaves
			// the filesystem exactly as it found it. Creating first meant a refused
			// init still left an empty directory behind.
			if _, err := os.Stat(wfdir.ManifestPath(root)); err == nil {
				return fmt.Errorf("%s already exists — this directory is already a synced folder",
					wfdir.ManifestPath(root))
			} else if !os.IsNotExist(err) {
				return fmt.Errorf("read %s: %w", wfdir.ManifestPath(root), err)
			}
			// Ahead of the directory being created, for the same reason the
			// manifest check is: a refused init leaves the filesystem exactly as
			// it found it. --feature is optional here, and confirmFeatureIn is a
			// no-op without one — a folder that names no feature refuses at its
			// first push, which is the behaviour that already existed.
			if err := confirmFeatureIn(cmd.Context(), featureID, resolved); err != nil {
				return err
			}
			if len(args) == 1 {
				if err := os.MkdirAll(root, 0o755); err != nil {
					return fmt.Errorf("create %s: %w", root, err)
				}
			}

			if strings.TrimSpace(title) == "" {
				title = deriveTitle("", root)
			}

			manifest := &wfdir.Manifest{
				Kind:  wfdir.KindPipeline,
				Title: title,
				// No entrypoint key at all. A pipeline folder has no file the
				// server runs, so writing `"entrypoint": ""` would be inviting
				// someone to fill in a field that means nothing — see the omitempty
				// on Manifest.Entrypoint.
			}
			// No tables: nothing exists server-side yet. The first push of each
			// file creates its table and records the binding.
			lock, err := recordFirstBinding(manifest,
				wfdir.InstanceKey{URL: resolved.URL, TenantID: resolved.TenantID},
				wfdir.Binding{FeatureID: featureID})
			if err != nil {
				return err
			}
			if err := wfdir.SaveFolder(root, manifest, lock); err != nil {
				return err
			}
			// An EMPTY baseline rather than a fabricated one. There is no
			// server-side row to have synced from, so recording table state would
			// be inventing a fact; every local .sql file correctly reads as "will
			// create" until the first push writes a real baseline.
			if err := wfdir.SaveState(root, &wfdir.State{}); err != nil {
				return err
			}

			if flagJSON {
				return emitJSON(map[string]any{
					"root":     root,
					"manifest": wfdir.ManifestPath(root),
					"url":      resolved.URL,
					// The organization the binding names, alongside the instance.
					// `url` alone was never the whole target — a folder is bound to
					// (instance, organization), and a script reading back what init
					// just wrote had to go and ask for the half init already knew.
					"tenantID":     resolved.TenantID,
					"organization": describeOrganization(resolved),
					"kind":         wfdir.KindPipeline,
					"title":        title,
					// Empty when --feature was not given, which is a folder that
					// cannot push yet and says so on the first attempt.
					"featureID": featureID,
					// `bound` carries `pipeline status`'s meaning: the folder HAS
					// an entry for this instance, which init just wrote. What is
					// genuinely absent is any table, and `tables` says that.
					"bound":  true,
					"tables": 0,
				})
			}
			printPipelineInitReport(root, describeTarget(resolved), title, featureID)
			return nil
		},
	}

	cmd.Flags().StringVar(&featureID, "feature", "",
		"feature the tables will be created in (a push refuses without one)")
	cmd.Flags().StringVar(&title, "title", "",
		"folder title (default: the directory name)")
	return cmd
}

// The id prefixes a feature row can carry, mirroring rrn.IsFeature
// (backend/lib/rrn/rrn.go): "collection-" is what rfeature mints, and "col-" is
// a legacy prefix no longer minted that existing tenants still hold rows under.
// A client that knew only the first would refuse a perfectly ordinary old
// feature.
const (
	featureIDPrefix       = "collection-"
	legacyFeatureIDPrefix = "col-"
)

// checkFeatureIDShape refuses a --feature value that is not a feature id.
//
// By SHAPE, and BEFORE confirmFeatureIn asks the server whether the id names a
// feature this credential can reach. The two answer different questions and the
// shape one answers better: a table id, a workspace id or a feature's NAME is
// not a feature id at all, and saying so names the real prefix — where the
// server's answer would only be "no feature you can reach", which reads as a
// permissions problem.
//
// An empty value is accepted: --feature is optional, and a folder without one
// refuses at its first push with a message that names the fix.
func checkFeatureIDShape(featureID string) error {
	if featureID == "" ||
		strings.HasPrefix(featureID, featureIDPrefix) ||
		strings.HasPrefix(featureID, legacyFeatureIDPrefix) {
		return nil
	}
	return fmt.Errorf("--feature %q is not a feature id — those start with %q (older ones with %q).\n  Find yours with: ronja api /api/v2/feature/query --jq '.result[] | \"\\(.id)  \\(.name)\"' -r",
		featureID, featureIDPrefix, legacyFeatureIDPrefix)
}

func printPipelineInitReport(root, target, title, featureID string) {
	out := os.Stdout
	fmt.Fprintf(out, "  Pipeline folder ready in %s\n\n", root)
	fmt.Fprintf(out, "  Title:    %s\n", title)
	if featureID != "" {
		fmt.Fprintf(out, "  Feature:  %s\n", featureID)
	} else {
		fmt.Fprintf(out, "  Feature:  none yet — add \"featureID\" to the instance entry in %s before pushing\n",
			wfdir.ManifestName)
	}
	fmt.Fprintf(out, "  Target:   %s (nothing pushed yet)\n", target)
	fmt.Fprintf(out, "\n  Write one .sql file per derived table; anything else in the folder is ignored.\n")
	fmt.Fprintf(out, "  Commit %s with your SQL; .ronja/ is local-only and ignores itself.\n",
		wfdir.ManifestName)
	fmt.Fprintf(out, "  Next: `ronja pipeline status` to see where you stand.\n")
}
