package commands

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/ronjatech/ronja-cli/internal/api"
	"github.com/ronjatech/ronja-cli/internal/wfdir"
	"github.com/spf13/cobra"
)

// `ronja app init` turns a directory (and optionally a component you already
// have) into a data-app folder WITHOUT creating anything server-side.
//
// Nothing remote is the point, and it matters more here than it does for a
// workflow. `wf init` creates nothing because a half-built workflow row is
// litter; `app init` creates nothing because a half-built data app is litter
// that is LIVE — POST /dataapp makes a visible row, and the CLI has no
// `app delete` to take it back. So the first push is the first thing that
// exists, and it happens once the folder is worth pushing.
func newDataAppInitCmd() *cobra.Command {
	var fromPath string
	var featureID string
	var title string

	cmd := &cobra.Command{
		Use:   "init",
		Short: "Set up a data-app folder in the current directory",
		Long: `Set up a data-app folder in the current directory.

Writes ronja.json (the manifest) and .ronja/ (the local sync baseline, already
git-ignored). Nothing is created on the server — the app itself comes into
existence on your first push.

Point --from at a component you already have and it is copied in as the
entrypoint:

  ronja app init --from src/Revenue.tsx --feature collection-abc123

Without --from you get an empty folder to write App.tsx into yourself:

  ronja app init --feature collection-abc123 --title "Revenue explorer"

--feature names the feature the app will be created in; find it with
` + "`ronja api /api/v2/feature/query`" + `. An organization with no features yet answers
that with an empty list — make one with:

  ronja api -X POST -d '{"name":"Playground"}' /api/v2/feature

The binding is recorded per instance, so run this against the instance you
intend to push to (--url, or your last login).

The manifest starts with an empty "access" block. That is what the app is
allowed to read — tables, secrets, agents, workflows, codexes, metrics — and
nothing on the server infers it from your code, so fill it in before pushing or
the app will render with nothing to show. Nothing later warns you either: an app
with an empty allowlist compiles, validates and publishes exactly like a working
one, and only fails once someone opens it.

Leave "capabilities" empty unless the app calls one of these four, each declared
by its manifest name:

  ai              completeAI — ask the AI for a completion
  query_external  queryExternal — read an external or managed database
  write_external  executeExternal — write rows to a managed database
  upload_file     uploadFile — store a file in Ronja

Nothing else ever goes in "capabilities": reading Ronja tables, running an
allowed agent or workflow, evaluating an allowed metric, codex search and HTTP
fetch through an allowed secret are all granted by their "access" allowlists —
listing the table (or agent, workflow, metric, codex, secret) is all it takes —
and the org roster (users()) needs neither. Any other name is refused by
` + "`ronja app push`" + ` before it sends anything, since the server would drop it
silently and the app would fail in the browser.`,
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			// MarkFlagRequired only asserts the flag was PASSED, so `--feature ""`
			// sails through it and writes a binding with no feature — which nothing
			// notices until the first push fails to create anything.
			featureID = strings.TrimSpace(featureID)
			if featureID == "" {
				return fmt.Errorf("--feature needs a feature id — list them with `ronja api /api/v2/feature/query`, or create one with `ronja api -X POST -d '{\"name\":\"Playground\"}' /api/v2/feature`")
			}

			// The resolved instance AND organization are the KEY the binding is
			// written under, so a wrong or missing one produces a manifest no later
			// command matches.
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
			if _, err := os.Stat(wfdir.ManifestPath(root)); err == nil {
				return fmt.Errorf("%s already exists — this directory is already a Ronja folder",
					wfdir.ManifestPath(root))
			} else if !os.IsNotExist(err) {
				return fmt.Errorf("read %s: %w", wfdir.ManifestPath(root), err)
			}

			// Fixed, not chosen: rdataapp stamps App.tsx at create and carries no
			// field to change it, so offering an --entrypoint flag would be offering
			// something the server will not honour.
			entrypoint := wfdir.DataAppKind.DefaultEntrypoint
			copied := false
			scaffolded := false
			var warnings []string
			if fromPath != "" {
				copied, err = copyEntrypoint(fromPath, root, entrypoint)
				if err != nil {
					return err
				}
				// Copy, never move: --from may point at a component other things in
				// the repo still import. But when the source sits INSIDE the folder it
				// is now duplicated, and every file in the folder is pushed.
				if copied {
					if rel, relErr := filepath.Rel(root, mustAbs(fromPath)); relErr == nil &&
						!strings.HasPrefix(rel, "..") {
						warnings = append(warnings, fmt.Sprintf(
							"%s is still in this folder and would be pushed alongside %s — delete it if it was only the source",
							rel, entrypoint))
					}
				}
			}

			// Scaffold a WORKING entrypoint when there is nothing to copy and
			// nothing already there.
			//
			// A starter file rather than an empty folder because the one thing an
			// author cannot infer is the mount call, and getting it wrong produces
			// a blank page from a build that reported success. Making the correct
			// shape the default is worth more than any amount of prose telling
			// people to write it themselves.
			//
			// Never overwrites: an existing App.tsx is the author's, and --from has
			// already put one there.
			if fromPath == "" {
				scaffolded, err = scaffoldEntrypoint(root, entrypoint, title)
				if err != nil {
					return err
				}
			}

			if strings.TrimSpace(title) == "" {
				title = deriveTitle(fromPath, root)
			}

			manifest := &wfdir.Manifest{
				Kind:       wfdir.KindDataApp,
				Title:      title,
				Entrypoint: entrypoint,
			}
			// No dataAppID: nothing exists server-side yet. The first push creates
			// the app and fills it in.
			lock, err := recordFirstBinding(manifest,
				wfdir.InstanceKey{URL: resolved.URL, TenantID: resolved.TenantID},
				wfdir.Binding{FeatureID: featureID})
			if err != nil {
				return err
			}
			// An EMPTY declaration rather than an absent one, so the folder owns its
			// allowlists from the start and granting the app a table is editing
			// ronja.json rather than discovering that the key exists. Harmless on an
			// app that does not exist yet, which is every folder init produces.
			manifest.SetAccess(api.DataAppAccess{})
			if err := wfdir.SaveFolder(root, manifest, lock); err != nil {
				return err
			}
			// An EMPTY baseline rather than a fabricated one. There is no
			// server-side row to have synced from, so recording a sourceID would be
			// inventing a fact; every local file correctly reads as "added" until the
			// first push writes a real baseline.
			if err := wfdir.SaveState(root, &wfdir.State{}); err != nil {
				return err
			}

			if flagJSON {
				payload := map[string]any{
					"root":       root,
					"manifest":   wfdir.ManifestPath(root),
					"url":        resolved.URL,
					"kind":       wfdir.KindDataApp,
					"title":      title,
					"entrypoint": entrypoint,
					"featureID":  featureID,
					// bound carries `app status`'s meaning, not a second one: the
					// folder HAS an entry for this instance, which init just wrote.
					// What is genuinely absent is the app itself, and `created` says
					// that — with no dataAppID, exactly as status reports it until the
					// first push.
					"bound":      true,
					"created":    false,
					"copiedFrom": fromPath,
				}
				if len(warnings) > 0 {
					payload["warnings"] = warnings
				}
				return emitJSON(payload)
			}
			printAppInitReport(root, resolved.URL, title, entrypoint, featureID, fromPath, copied, scaffolded)
			for _, w := range warnings {
				fmt.Fprintf(os.Stderr, "  Note: %s\n", w)
			}
			return nil
		},
	}

	cmd.Flags().StringVar(&fromPath, "from", "",
		"existing component to copy in as the entrypoint")
	cmd.Flags().StringVar(&featureID, "feature", "",
		"feature the data app will be created in (required)")
	cmd.Flags().StringVar(&title, "title", "",
		"data app title (default: derived from --from, or the directory name)")
	_ = cmd.MarkFlagRequired("feature")
	return cmd
}

func printAppInitReport(root, url, title, entrypoint, featureID, fromPath string, copied, scaffolded bool) {
	out := os.Stdout
	fmt.Fprintf(out, "  Data-app folder ready in %s\n\n", root)
	fmt.Fprintf(out, "  Title:      %s\n", title)
	fmt.Fprintf(out, "  Entrypoint: %s (fixed — a data app's entrypoint cannot be changed)\n", entrypoint)
	fmt.Fprintf(out, "  Feature:    %s\n", featureID)
	fmt.Fprintf(out, "  Instance:   %s (not pushed yet)\n", url)
	if copied {
		fmt.Fprintf(out, "\n  Copied %s -> %s\n", fromPath, entrypoint)
	} else if scaffolded {
		fmt.Fprintf(out, "\n  Wrote %s — a working starter app. Push it as-is to see it render,\n", entrypoint)
		fmt.Fprintf(out, "  then edit it.\n")
	} else if fromPath == "" {
		fmt.Fprintf(out, "\n  Write your app in %s.\n", entrypoint)
	}
	// Said HERE, at the one moment the author is about to write the entrypoint,
	// because it is the one line nobody can infer. The server's mount check now
	// REFUSES a bundle that mounts nothing, so this is no longer a silent
	// failure — but being told at init costs one push less than being told by a
	// failed one. `--from` gets it too: a component lifted out of another
	// project is exactly the shape that defines a component and never mounts.
	fmt.Fprintf(out, "\n  %s must MOUNT itself — end it with:\n", entrypoint)
	fmt.Fprintf(out, "      createRoot(document.getElementById(\"app\")).render(<App />);\n")
	fmt.Fprintf(out, "  Exporting a component is not enough; nothing calls it. Pushing one that\n")
	fmt.Fprintf(out, "  never mounts is refused at compile time, so it costs you a round trip.\n")
	// The folder loop is only half of what an author needs; the other half is
	// what goes INSIDE the entrypoint, and nothing else in the CLI says where
	// that is written down. Printed as a live URL on THIS instance so it cannot
	// drift from the instance being pushed to.
	fmt.Fprintf(out, "\n  What goes inside an app — the @ronja/app SDK (query, charts, applyBrand)\n")
	fmt.Fprintf(out, "  and the libraries you may import:\n")
	fmt.Fprintf(out, "      %s/docs/api/skills/creating-data-apps.md\n", strings.TrimRight(url, "/"))
	fmt.Fprintf(out, "\n  Commit %s with your code; .ronja/ is local-only and ignores itself.\n",
		wfdir.ManifestName)
	fmt.Fprintf(out, "  Grant the app what it needs to read under \"access\" in %s — nothing\n", wfdir.ManifestName)
	fmt.Fprintf(out, "  derives it from your code, and nothing warns you if you forget: an app\n")
	fmt.Fprintf(out, "  with an empty allowlist publishes green and reads nothing. Listing a\n")
	fmt.Fprintf(out, "  table is all it takes — reading tables needs no \"capabilities\" entry.\n")
	// The names, not a pointer to them: this is the one moment the author is
	// about to write the manifest, and a vocabulary they have to go and look up
	// is one they will guess at instead. Built from the same list `app push`
	// refuses against, so the report cannot name a set the push disagrees with.
	fmt.Fprintf(out, "  \"capabilities\" itself takes only these names — a push refuses any other:\n")
	fmt.Fprintf(out, "  %s. See `ronja app init --help`.\n", strings.Join(declarableCapabilities, ", "))
	fmt.Fprintf(out, "  Your first push creates the app as an unpublished draft only you can see;\n")
	fmt.Fprintf(out, "  publishing promotes it in place, so its link never changes.\n")
	fmt.Fprintf(out, "\n  Next: `ronja app status` to see where you stand.\n")
}

// scaffoldEntrypoint writes a starter entrypoint, reporting whether it wrote one.
//
// The template is a WORKING app, not a stub with TODOs: the author's first push
// should render something, so that the loop is proved end to end before any of
// their own code is in it. Everything here earns its place —
//
//   - createRoot(...) on the last line is the reason this function exists. It is
//     the one line nobody can infer, and omitting it produces a blank page from
//     a build that reported success.
//   - Card comes from @ronja/app, so the starter also demonstrates that the SDK
//     is imported by name rather than installed.
//
// Never overwrites: a file already at that path belongs to the author, and
// silently replacing it would be the worst thing an init command could do.
func scaffoldEntrypoint(root, entrypoint, title string) (bool, error) {
	dest := filepath.Join(root, entrypoint)
	if _, err := os.Stat(dest); err == nil {
		return false, nil
	} else if !os.IsNotExist(err) {
		return false, fmt.Errorf("read %s: %w", dest, err)
	}
	heading := strings.TrimSpace(title)
	if heading == "" {
		heading = deriveTitle("", root)
	}
	body := fmt.Sprintf(`import { createRoot } from "react-dom/client";
import { Card } from "@ronja/app";

function App() {
  return (
    <Card title=%q>
      <p>Your app is running. Edit %s to build it out.</p>
    </Card>
  );
}

// Mounts the app. Without this line the bundle still compiles — and renders
// nothing, because nothing ever calls App.
createRoot(document.getElementById("app")).render(<App />);
`, heading, entrypoint)
	if err := os.WriteFile(dest, []byte(body), 0o644); err != nil {
		return false, fmt.Errorf("write %s: %w", dest, err)
	}
	return true, nil
}
