package commands

import (
	"fmt"
	"os"
	"strings"

	"github.com/ronjatech/ronja-cli/internal/wfdir"
	"github.com/spf13/cobra"
)

// `ronja module init` is the bootstrap half of the loop: it turns a directory
// into a module folder WITHOUT creating anything server-side.
//
// Nothing remote is the point, exactly as for `wf init`: reconciling three
// drifted copies of a helper file into one module is an iterate-until-it-looks-
// right process, and doing it against a half-built module row means every
// abandoned attempt leaves a governed resource — and, in a shared feature, a
// pending PROPOSAL — for somebody to clean up. Here the first push is the first
// thing that exists.
func newModuleInitCmd() *cobra.Command {
	var featureID string
	var title string

	cmd := &cobra.Command{
		Use:   "init <name>",
		Short: "Set up a module folder in the current directory",
		Long: `Set up a module folder in the current directory.

Writes ronja.json (the manifest), an ` + "`__init__.py`" + ` if the folder has none, and
.ronja/ (the local sync baseline, already git-ignored). Nothing is created on
the server — the module itself comes into existence on your first push.

The NAME is the Python package consumers import, so it has to be a valid Python
identifier, unique among the live modules in your organization, and not the name
of something the runtime already provides (json, requests, tools, …):

  ronja module init emab_tracker --feature collection-abc

--feature names the feature the module will be created in; find it with
GET /api/v2/feature/query. The binding is recorded per instance, so run this
against the instance you intend to push to (--url, or your last login).

Move an existing helper in by copying it into the folder — every .py file here
is pushed, and everything else is skipped.`,
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			// The package NAME is the one thing a module cannot be created
			// without, and it is a positional rather than a flag because it is
			// not optional in any sense: it is the identifier consumers type.
			name := strings.TrimSpace(args[0])
			if name == "" {
				return fmt.Errorf("a module needs a package name — the identifier a workflow writes after `import`")
			}
			// Refused HERE as well as server-side, and deliberately only for the
			// shape a person can see is wrong: a name with a dot or a dash in it
			// is not a Python identifier and never will be, so failing at the
			// first push — after the manifest is committed and a colleague has
			// pulled it — is the wrong moment to say so. The full rule (identifier
			// grammar, live uniqueness, the stdlib/runtime shadow list) stays
			// server-side, where it can see the other modules and the runtime
			// image; duplicating it would be a second list to keep in step.
			if err := checkLocalModuleName(name); err != nil {
				return err
			}

			// MarkFlagRequired only asserts the flag was PASSED, so `--feature ""`
			// sails through it and writes a binding with no feature — which
			// nothing notices until the first push fails to create anything.
			featureID = strings.TrimSpace(featureID)
			if featureID == "" {
				return fmt.Errorf("--feature needs a feature id — find one with `GET /api/v2/feature/query`")
			}

			// The resolved instance AND organization are the KEY the binding is
			// written under, so a wrong or missing one produces a manifest no
			// later command matches.
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
				return fmt.Errorf("%s already exists — this directory is already a synced folder",
					wfdir.ManifestPath(root))
			} else if !os.IsNotExist(err) {
				return fmt.Errorf("read %s: %w", wfdir.ManifestPath(root), err)
			}

			// The local seed. The SERVER also seeds an __init__.py inside the
			// create transaction, and the two are not redundant: without a local
			// one the first push creates the module, lists back the server's
			// seed, finds no local file at that path and DELETES it — which the
			// server refuses, stopping the very first push of every new folder.
			// Seeding here makes the first push's file set a superset of the
			// server's, which is the only shape the sync loop reconciles cleanly.
			//
			// An existing __init__.py is left alone: `module init` inside a
			// directory that already holds helpers is the documented way to adopt
			// them, and the one thing worse than an init that seeds nothing is
			// one that ate the file it found.
			seeded, err := seedModuleInit(root)
			if err != nil {
				return err
			}

			if strings.TrimSpace(title) == "" {
				// The package name, not the directory's: a module's title is a
				// label for the same thing the name identifies, and deriving it
				// from whatever the folder happens to be called produces a title
				// that disagrees with the import statement.
				title = name
			}

			manifest := &wfdir.Manifest{
				Kind:  wfdir.KindModule,
				Name:  name,
				Title: title,
				// No entrypoint, and no key for one: a module has nothing to run.
				// Manifest.Entrypoint is omitempty and ModuleKind's
				// DefaultEntrypoint is empty, so the file this writes carries no
				// mention of a concept the primitive does not have.
			}
			// No moduleID: nothing exists server-side yet. The first push creates
			// the module and fills it in.
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
			// server-side row to have synced from, so recording a sourceID would
			// be inventing a fact; every local file correctly reads as "added"
			// until the first push writes a real baseline.
			if err := wfdir.SaveState(root, &wfdir.State{}); err != nil {
				return err
			}

			if flagJSON {
				return emitJSON(map[string]any{
					"root":     root,
					"manifest": wfdir.ManifestPath(root),
					"url":      resolved.URL,
					"kind":     wfdir.KindModule,
					"name":     name,
					"title":    title,
					// bound carries `module status`'s meaning, not a second one:
					// the folder HAS an entry for this instance, which init just
					// wrote. What is genuinely absent is the module itself, and
					// `created` says that — with no moduleID, exactly as status
					// reports it until the first push.
					"featureID":     featureID,
					"bound":         true,
					"created":       false,
					"seededInitPy":  seeded,
					"seededInitFor": moduleInitFile,
				})
			}
			printModuleInitReport(root, resolved.URL, name, title, featureID, seeded)
			return nil
		},
	}

	cmd.Flags().StringVar(&featureID, "feature", "",
		"feature the module will be created in (required)")
	cmd.Flags().StringVar(&title, "title", "",
		"module title (default: the package name)")
	_ = cmd.MarkFlagRequired("feature")
	return cmd
}

// checkLocalModuleName refuses the name shapes that are wrong on their face,
// and nothing else.
//
// The division of labour is deliberate: this catches what a person can see —
// a character Python will never accept in an identifier, or a leading digit —
// so the mistake is reported at the moment they type it rather than at the first
// push of a manifest a colleague has already pulled. Everything that needs to
// know about the ORGANIZATION (is this name taken) or about the RUNTIME (does it
// shadow a module the image ships) stays server-side, where the answer lives.
// A local copy of either would be a second list, and a second list drifts.
func checkLocalModuleName(name string) error {
	for i, r := range name {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r == '_':
		case r >= '0' && r <= '9' && i > 0:
		default:
			if i == 0 && r >= '0' && r <= '9' {
				return fmt.Errorf("%q is not a Python identifier — a package name cannot start with a digit, so `import %s` would be a syntax error",
					name, name)
			}
			return fmt.Errorf("%q is not a Python identifier — %q is not allowed in one, so `import %s` would be a syntax error. Use letters, digits and underscores",
				name, string(r), name)
		}
	}
	return nil
}

// seedModuleInit writes the package's __init__.py when the folder has none,
// reporting whether it wrote anything.
//
// The content is a one-line docstring rather than an empty file, for a reason
// worth stating: an empty file and a missing one look identical in most
// diff-shaped views, and this file's presence is the difference between a
// package and a directory. A line saying what it is survives being read by
// whoever wonders why it exists.
//
// ⚠️ It describes the module marker in WORDS and does not spell one. A module
// file carrying a Ronja marker is refused at save, and this text survives only
// because the marker scan strips docstrings first — so a seed containing one is
// a file that saves for a reason unrelated to what it says. Somebody reformats
// the docstring, or the strip order changes, and `module init` starts producing
// folders whose very first push is refused.
func seedModuleInit(root string) (bool, error) {
	dest := wfdir.ModuleInitPath(root)
	if _, err := os.Stat(dest); err == nil {
		return false, nil
	} else if !os.IsNotExist(err) {
		return false, fmt.Errorf("read %s: %w", dest, err)
	}
	body := "\"\"\"Shared Ronja module. A workflow imports this package through its module marker.\"\"\"\n"
	if err := wfdir.WriteFile(root, moduleInitFile, body); err != nil {
		return false, err
	}
	return true, nil
}

func printModuleInitReport(root, url, name, title, featureID string, seeded bool) {
	out := os.Stdout
	fmt.Fprintf(out, "  Module folder ready in %s\n\n", root)
	fmt.Fprintf(out, "  Package:  %s\n", name)
	fmt.Fprintf(out, "  Title:    %s\n", title)
	fmt.Fprintf(out, "  Feature:  %s\n", featureID)
	fmt.Fprintf(out, "  Instance: %s (not pushed yet)\n", url)
	if seeded {
		fmt.Fprintf(out, "\n  Wrote %s — it is what makes `import %s` resolve.\n", moduleInitFile, name)
	}
	fmt.Fprintf(out, "\n  Put your .py files here; anything else in the folder is skipped.\n")
	fmt.Fprintf(out, "  Commit %s with your code; .ronja/ is local-only and ignores itself.\n",
		wfdir.ManifestName)
	fmt.Fprintf(out, "  Next: `ronja module status` to see where you stand.\n")
}
