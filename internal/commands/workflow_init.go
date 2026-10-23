package commands

import (
	"errors"
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
// It CREATES nothing remote, which is the point. Converting an existing script
// into a workflow is an iterate-to-green process — markers to add, parameters to
// declare, secrets to bind — and doing that against a half-built workflow row
// means every failed attempt leaves state behind for someone to clean up. Here
// the first push is the first thing that exists.
//
// It does READ: one GET confirming --feature names a feature this credential can
// reach, before anything is written (see confirmFeatureIn), and one GET of the
// instance's defaults for a new workflow — on the runtime --runtime names, or
// on its default (see initRuntime).
func newWorkflowInitCmd() *cobra.Command {
	var fromPath string
	var featureID string
	var title string
	var runtime int
	var language string

	cmd := &cobra.Command{
		Use:   "init",
		Short: "Set up a workflow folder in the current directory",
		Long: `Set up a workflow folder in the current directory.

Writes ronja.json (the manifest) and .ronja/ (the local sync baseline, already
git-ignored). Nothing is created on the server — the workflow itself comes into
existence on your first push, which is what makes converting an existing script
an iterate-until-it-validates loop rather than a create-then-fix one.

Point --from at a script you already have and it is copied in as the entrypoint:

  ronja wf init --from scripts/monthly_report.py --feature collection-abc

Without --from you get a main.py to start from, written for that runtime:

  ronja wf init --feature collection-abc --title "Monthly report"

--feature names the feature the workflow will be created in; find it with
GET /api/v2/feature/query. The binding is recorded per instance, so run this
against the instance you intend to push to (--url, or your last login).

The folder is pinned to a runtime in ronja.json, and its first push creates the
workflow on exactly that runtime. Without --runtime it is the one the instance
creates new workflows on (it asks GET /api/v2/workflow/defaults); --runtime
picks another the instance offers. Runtime 1 and 2 are retired: a new workflow
cannot be created on them, so --runtime 1 and --runtime 2 are refused.

Runtime 3 is DURABLE — every @tools.step result is journaled, and so is every
SEND (email, event, upload, tools.http call), so a failed run can be resumed
(` + "`ronja wf test --resume`" + `) instead of re-run from the top, without
repeating what already went out — and runs in a container that holds no
credential for Ronja's table storage: its code reads a table only through
` + "`tools.query(\"SELECT ... FROM {{ ref('tbl::...') }}\")`" + `, never by reading a
parquet file itself.`,
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			// Only a runtime a new workflow can be created on. Runtime 1 and 2
			// are retired, so naming one is refused here, in the words the
			// instance would use at the first push — before a byte is written,
			// rather than a folder that can never be created.
			runtimeNamed := cmd.Flags().Changed("runtime")
			lang, err := resolveInitLanguage(language, fromPath, runtimeNamed, runtime)
			if err != nil {
				return err
			}
			language = lang
			if runtimeNamed {
				if wfdir.RetiredRuntime(runtime) {
					return fmt.Errorf("--runtime %d: runtime 1 and 2 are retired — a new workflow cannot be created on them. Omit --runtime (the folder then takes the instance's default) and read tables with tools.query (the {{ ref }} marker goes inside the SQL string)",
						runtime)
				}
				if !wfdir.CreatableRuntime(runtime) {
					return fmt.Errorf("--runtime must name a runtime the instance creates new workflows on (got %d); omit it for the instance's default",
						runtime)
				}
			}

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

			// The one thing about --feature this command can actually settle,
			// and it settles it before a single byte is written: not the copied
			// entrypoint, not the scaffold, not the manifest. A refusal here
			// leaves the directory as it found it, so there is nothing to clean
			// up before running init again with the right id.
			if err := confirmFeatureIn(cmd.Context(), featureID, resolved); err != nil {
				return err
			}

			// The runtime the folder is pinned to, and the starter written for
			// it. Settled before anything is written too, for the same reason.
			scaffold, err := initRuntime(cmd.Context(), resolved, runtime, runtimeNamed, language)
			if err != nil {
				return err
			}
			runtime = scaffold.runtime
			starter := scaffold.starter

			entrypoint := scaffold.entrypoint
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

			// A folder starts from code written for its runtime — on 3 the four
			// durable structuring rules (journal the discovery step, derive keys
			// rather than write them, return handles, return something from a
			// side-effecting step) are not things anyone infers from an empty
			// file.
			//
			// Only when there is nothing to overwrite: --from means the author
			// brought their own code, and an existing entrypoint is somebody's
			// work.
			scaffolded := false
			if fromPath == "" {
				scaffolded, err = writeStarter(root, entrypoint, starter)
				if errors.Is(err, errEmptyStarter) {
					return fmt.Errorf("%s named runtime %d for a new workflow but sent no starter %s — write %s yourself, or pass --from with the code to start from",
						describeTarget(resolved), runtime, entrypoint, entrypoint)
				}
				if err != nil {
					return err
				}
			}
			// package.json is written beside the entrypoint whether or not it
			// was scaffolded — --from brings code, not its dependency list — and
			// an existing one is left alone, as an existing entrypoint is.
			wrotePackageJSON := false
			if scaffold.packageJSON != "" {
				if wrotePackageJSON, err = writeStarter(root, packageJSONName, scaffold.packageJSON); err != nil {
					return err
				}
			}

			if strings.TrimSpace(title) == "" {
				title = deriveTitle(fromPath, root)
			}

			manifest := &wfdir.Manifest{
				Kind:       wfdir.KindWorkflow,
				Title:      title,
				Language:   language,
				Entrypoint: entrypoint,
			}
			// A folder INITIALIZED onto a stack is born adopted: its id is minted
			// here, locally, so its first push claims its address on the server and
			// CI never needs the lock. See mintFolderIDAtInit.
			if err := mintFolderIDAtInit(manifest); err != nil {
				return err
			}
			// ALWAYS pinned, named or not: the first push sends it, so the
			// workflow is created on the runtime init settled — not on whatever
			// the instance's default has become by then. Code from --from or an
			// adopted entrypoint was not written from that runtime's starter,
			// and the note below says so.
			manifest.Runtime = runtime
			if !scaffolded && !runtimeNamed {
				warnings = append(warnings, fmt.Sprintf(
					"this folder is pinned to runtime %d, the one %s creates new workflows on, and %s was not written from its starter — make sure the code is written for runtime %d before the first push, or pick another with `wf init --runtime`",
					runtime, describeTarget(resolved), entrypoint, runtime))
			}
			// No workflowID: nothing exists server-side yet. The first push
			// creates the workflow and fills it in.
			lock, err := recordFirstBinding(manifest,
				wfdir.InstanceKey{URL: resolved.URL, TenantID: resolved.TenantID},
				wfdir.Binding{FeatureID: featureID})
			if err != nil {
				return err
			}
			// An empty declaration rather than an absent one, so the folder owns
			// its parameters from the start and adding one is editing ronja.json
			// rather than discovering that the key exists. Harmless on a workflow
			// that does not exist yet, which is every folder init produces.
			manifest.SetParameters(nil)
			// No reportingTimezone, deliberately, and the asymmetry with the line
			// above is the point: a workflow declaring no parameters is the normal
			// starting state and the server has no default to inherit, whereas the
			// declared calendar DOES have an organization-wide default the create
			// stamps. Writing the key here would have to write something, and the
			// only value available without a round trip is "" — which means "declare
			// UTC", so every fresh folder would silently override its organization's
			// default. Absent means "inherit it", and `wf clone` records whatever
			// the row ends up with.
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
				payload := map[string]any{
					"root":     root,
					"manifest": wfdir.ManifestPath(root),
					"url":      resolved.URL,
					// The organization the binding names, alongside the instance.
					// `url` alone was never the whole target — a folder is bound to
					// (instance, organization), and a script reading back what init
					// just wrote had to go and ask for the half init already knew.
					"tenantID":     resolved.TenantID,
					"organization": describeOrganization(resolved),
					"kind":         wfdir.KindWorkflow,
					"title":        title,
					"entrypoint":   entrypoint,
					"featureID":    featureID,
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
					// The runtime the manifest pins, which the first push
					// creates the workflow on.
					"runtime":    runtime,
					"language":   manifest.LanguageName(),
					"scaffolded": scaffolded,
				}
				if scaffold.packageJSON != "" {
					payload["packageJSON"] = wrotePackageJSON
				}
				if len(warnings) > 0 {
					payload["warnings"] = warnings
				}
				return emitJSON(payload)
			}
			printInitReport(root, describeTarget(resolved), title, entrypoint, featureID, fromPath, copied, runtime, scaffolded)
			if wrotePackageJSON {
				fmt.Fprintf(os.Stdout, "  Wrote %s — declare npm packages under \"dependencies\", at exact versions.\n", packageJSONName)
			}
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
	cmd.Flags().StringVar(&language, "language", "",
		"the workflow's language: python (default) or js; fixed once the workflow is created")
	cmd.Flags().IntVar(&runtime, "runtime", 0,
		"workflow runtime to pin in the manifest (default: the one the instance creates new workflows on); runtime 1 and 2 are retired")
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

func printInitReport(root, target, title, entrypoint, featureID, fromPath string, copied bool, runtime int, scaffolded bool) {
	out := os.Stdout
	fmt.Fprintf(out, "  Workflow folder ready in %s\n\n", root)
	fmt.Fprintf(out, "  Title:      %s\n", title)
	fmt.Fprintf(out, "  Entrypoint: %s\n", entrypoint)
	fmt.Fprintf(out, "  Feature:    %s\n", featureID)
	if _, sentence := runtimeInfo(runtime); sentence != "" {
		fmt.Fprintf(out, "  Runtime:    %d — %s\n", runtime, sentence)
	}
	fmt.Fprintf(out, "  Target:     %s (not pushed yet)\n", target)
	if copied {
		fmt.Fprintf(out, "\n  Copied %s -> %s\n", fromPath, entrypoint)
	} else if scaffolded {
		fmt.Fprintf(out, "\n  Wrote a runtime-%d %s to start from.\n", runtime, entrypoint)
	} else if fromPath == "" {
		fmt.Fprintf(out, "\n  Write your code in %s.\n", entrypoint)
	}
	fmt.Fprintf(out, "\n  Commit %s with your code; .ronja/ is local-only and ignores itself.\n",
		wfdir.ManifestName)
	fmt.Fprintf(out, "  Next: `ronja wf status` to see where you stand.\n")
}
