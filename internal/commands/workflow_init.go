package commands

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"unicode"
	"unicode/utf8"

	"github.com/ronjatech/ronja-cli/internal/wfdir"
	"github.com/spf13/cobra"
)

// `ronja wf init` is the bootstrap half of the loop: it turns a directory (and
// optionally a script you already have) into a workflow folder WITHOUT creating
// anything server-side.
//
// Nothing remote is the point. Converting an existing script into a workflow is
// an iterate-to-green process — markers to add, parameters to declare, secrets
// to bind — and doing that against a half-built workflow row means every failed
// attempt leaves state behind for someone to clean up. Here the first push is
// the first thing that exists.
func newWorkflowInitCmd() *cobra.Command {
	var fromPath string
	var featureID string
	var title string

	cmd := &cobra.Command{
		Use:   "init",
		Short: "Set up a workflow folder in the current directory",
		Long: `Set up a workflow folder in the current directory.

Writes ronja.json (the manifest) and .ronja/ (the local sync baseline, already
git-ignored). Nothing is created on the server — the workflow itself comes into
existence on your first push, which is what makes converting an existing script
an iterate-until-it-validates loop rather than a create-then-fix one.

Point --from at a script you already have and it is copied in as the entrypoint:

  ronja wf init --from scripts/monthly_report.py --feature feat-abc

Without --from you get an empty folder to write main.py into yourself:

  ronja wf init --feature feat-abc --title "Monthly report"

--feature names the feature the workflow will be created in; find it with
GET /api/v2/feature/query. The binding is recorded per instance, so run this
against the instance you intend to push to (--url, or your last login).`,
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			// MarkFlagRequired only asserts the flag was PASSED, so `--feature
			// ""` sails through it and writes a binding with no feature — which
			// nothing notices until the first push fails to create anything.
			featureID = strings.TrimSpace(featureID)
			if featureID == "" {
				return fmt.Errorf("--feature needs a feature id — find one with `GET /api/v2/feature/query`")
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
			if _, err := os.Stat(wfdir.ManifestPath(root)); err == nil {
				return fmt.Errorf("%s already exists — this directory is already a workflow folder",
					wfdir.ManifestPath(root))
			} else if !os.IsNotExist(err) {
				return fmt.Errorf("read %s: %w", wfdir.ManifestPath(root), err)
			}

			entrypoint := wfdir.DefaultEntrypoint
			copied := false
			var warnings []string
			if fromPath != "" {
				copied, err = copyEntrypoint(fromPath, root, entrypoint)
				if err != nil {
					return err
				}
				// Copy, never move: --from may point at a script other things
				// in the repo still import, and eating it would be a rude way
				// to find that out. But when the source sits INSIDE the folder
				// it is now duplicated, and every file in the folder is pushed
				// — so say so rather than let it show up as a mystery second
				// file in the workflow.
				if copied {
					if rel, relErr := filepath.Rel(root, mustAbs(fromPath)); relErr == nil &&
						!strings.HasPrefix(rel, "..") {
						warnings = append(warnings, fmt.Sprintf(
							"%s is still in this folder and would be pushed alongside %s — delete it if it was only the source",
							rel, entrypoint))
					}
				}
			}

			if strings.TrimSpace(title) == "" {
				title = deriveTitle(fromPath, root)
			}

			manifest := &wfdir.Manifest{
				Kind:       wfdir.KindWorkflow,
				Title:      title,
				Entrypoint: entrypoint,
			}
			// No workflowID: nothing exists server-side yet. The first push
			// creates the workflow and fills it in.
			manifest.SetBinding(
				wfdir.InstanceKey{URL: resolved.URL, TenantID: resolved.TenantID},
				wfdir.Binding{FeatureID: featureID})
			// An empty declaration rather than an absent one, so the folder owns
			// its parameters from the start and adding one is editing ronja.json
			// rather than discovering that the key exists. Harmless on a workflow
			// that does not exist yet, which is every folder init produces.
			manifest.SetParameters(nil)
			if err := wfdir.SaveManifest(root, manifest); err != nil {
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
				payload := map[string]any{
					"root":       root,
					"manifest":   wfdir.ManifestPath(root),
					"url":        resolved.URL,
					"kind":       wfdir.KindWorkflow,
					"title":      title,
					"entrypoint": entrypoint,
					"featureID":  featureID,
					// bound carries `wf status`'s meaning, not a second one:
					// the folder HAS an entry for this instance, which init just
					// wrote. The two commands disagreeing about the same folder
					// is worse than either answer being debatable. What is
					// genuinely absent is the workflow itself, and `created`
					// says that — with no workflowID, exactly as status reports
					// it until the first push.
					"bound":      true,
					"created":    false,
					"copiedFrom": fromPath,
				}
				if len(warnings) > 0 {
					payload["warnings"] = warnings
				}
				return emitJSON(payload)
			}
			printInitReport(root, resolved.URL, title, entrypoint, featureID, fromPath, copied)
			for _, w := range warnings {
				fmt.Fprintf(os.Stderr, "  Note: %s\n", w)
			}
			return nil
		},
	}

	cmd.Flags().StringVar(&fromPath, "from", "",
		"existing script to copy in as the entrypoint")
	cmd.Flags().StringVar(&featureID, "feature", "",
		"feature the workflow will be created in (required)")
	cmd.Flags().StringVar(&title, "title", "",
		"workflow title (default: derived from --from, or the directory name)")
	_ = cmd.MarkFlagRequired("feature")
	return cmd
}

// copyEntrypoint copies --from into the folder as the entrypoint, reporting
// whether a copy actually happened.
//
// Three cases where it does not: --from IS the entrypoint already (running init
// in a directory that has main.py in it is the obvious way to do this), a
// destination that already holds exactly those bytes (a re-run after a failed
// init, where writing them again would change nothing), and a destination that
// exists with DIFFERENT content — the one case that is refused rather than
// overwritten, because the one thing worse than a failed init is one that ate
// the script it was pointed at.
//
// The destination is always the entrypoint name, never the source's basename:
// a source called "monthly report (v2).py" is a path the server rejects, and
// silently renaming to something legal is less surprising than a mid-push 400.
func copyEntrypoint(fromPath, root, entrypoint string) (bool, error) {
	dest := filepath.Join(root, entrypoint)
	srcAbs, err := filepath.Abs(fromPath)
	if err != nil {
		return false, fmt.Errorf("resolve %s: %w", fromPath, err)
	}
	destAbs, err := filepath.Abs(dest)
	if err != nil {
		return false, fmt.Errorf("resolve %s: %w", dest, err)
	}
	if srcAbs == destAbs {
		// Already in place: `wf init --from main.py` run inside the directory
		// that holds it. Still verify it is readable, so init does not claim to
		// have set up a folder around a file it cannot open.
		if _, err := os.Stat(srcAbs); err != nil {
			return false, fmt.Errorf("read %s: %w", fromPath, err)
		}
		return false, nil
	}
	body, err := os.ReadFile(srcAbs)
	if err != nil {
		return false, fmt.Errorf("read %s: %w", fromPath, err)
	}
	if existing, err := os.ReadFile(dest); err == nil {
		if string(existing) == string(body) {
			// Byte-identical, so there is nothing to overwrite and nothing to
			// lose: this is re-running init after it failed on something later
			// (a bad --feature, an unwritable manifest), and refusing here would
			// send the author to move a file aside only to put back its exact
			// contents.
			return false, nil
		}
		return false, fmt.Errorf("%s already exists — move it aside, or drop --from and keep the file you have",
			entrypoint)
	} else if !os.IsNotExist(err) {
		return false, fmt.Errorf("read %s: %w", dest, err)
	}
	if err := wfdir.WriteFile(root, entrypoint, string(body)); err != nil {
		return false, err
	}
	return true, nil
}

// mustAbs is filepath.Abs for the cases where a failure only costs a warning
// that does not get printed.
func mustAbs(path string) string {
	abs, err := filepath.Abs(path)
	if err != nil {
		return path
	}
	return abs
}

// deriveTitle picks a default title: the source script's name if there is one,
// otherwise the directory's. Both are what the human already called this thing.
func deriveTitle(fromPath, root string) string {
	base := ""
	if fromPath != "" {
		base = strings.TrimSuffix(filepath.Base(fromPath), filepath.Ext(fromPath))
	}
	if strings.TrimSpace(base) == "" || base == "." {
		base = filepath.Base(root)
	}
	base = strings.ReplaceAll(strings.ReplaceAll(base, "_", " "), "-", " ")
	base = strings.TrimSpace(base)
	if base == "" || base == "." || base == string(filepath.Separator) {
		return "Untitled workflow"
	}
	// Byte-sliced, this mangles every non-ASCII first letter: base[:1] of "över"
	// is half a rune, and upper-casing it produces a title starting with U+FFFD.
	// Filenames are not ASCII, and the one the CLI is most likely to meet is a
	// Swedish one.
	first, size := utf8.DecodeRuneInString(base)
	if first == utf8.RuneError && size <= 1 {
		// Not valid UTF-8 — leave it exactly as the filesystem gave it rather
		// than corrupt it further.
		return base
	}
	return string(unicode.ToUpper(first)) + base[size:]
}

func printInitReport(root, url, title, entrypoint, featureID, fromPath string, copied bool) {
	out := os.Stdout
	fmt.Fprintf(out, "  Workflow folder ready in %s\n\n", root)
	fmt.Fprintf(out, "  Title:      %s\n", title)
	fmt.Fprintf(out, "  Entrypoint: %s\n", entrypoint)
	fmt.Fprintf(out, "  Feature:    %s\n", featureID)
	fmt.Fprintf(out, "  Instance:   %s (not pushed yet)\n", url)
	if copied {
		fmt.Fprintf(out, "\n  Copied %s -> %s\n", fromPath, entrypoint)
	} else if fromPath == "" {
		fmt.Fprintf(out, "\n  Write your code in %s.\n", entrypoint)
	}
	fmt.Fprintf(out, "\n  Commit %s with your code; .ronja/ is local-only and ignores itself.\n",
		wfdir.ManifestName)
	fmt.Fprintf(out, "  Next: `ronja wf status` to see where you stand.\n")
}
