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
	var runtime int

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

Without --from you get an empty folder to write main.py into yourself:

  ronja wf init --feature collection-abc --title "Monthly report"

--feature names the feature the workflow will be created in; find it with
GET /api/v2/feature/query. The binding is recorded per instance, so run this
against the instance you intend to push to (--url, or your last login).

--runtime 2 makes it a DURABLE workflow: every @tools.step result is journaled,
and so is every SEND (email, event, upload, tools.http call), so a failed run can
be resumed (` + "`ronja wf test --resume`" + `) instead of re-run from the top — without
repeating what already went out.

--runtime 3 is durable too, and additionally gives the workflow a container that
holds no credential for Ronja's table storage: its code reads a table only
through ` + "`tools.query(\"SELECT ... FROM {{ ref('tbl::...') }}\")`" + `, never by
reading a parquet file itself.

Without --runtime the folder declares none and the instance chooses, which today
means runtime 3. Name one to pin it: --runtime 1 writes the key too, so a folder
that asks for the standard runtime gets it whatever the instance defaults to.

The runtime is stamped when the first push creates the workflow and can only be
raised afterwards, never lowered. With a durable runtime and no --from you also
get a durable main.py to start from — and saying nothing IS a durable runtime,
so a plain init scaffolds for the one it is about to create.`,
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			// Checked against the enumerated set rather than a range, because
			// the manifest is committed: a folder declaring a runtime this
			// instance has never heard of would fail at the first push, in a
			// message about a create body rather than about the flag that
			// caused it. Zero is excluded HERE even though ValidRuntime admits
			// it — an absent key means 1, but `--runtime 0` is a typo.
			if runtime == 0 || !wfdir.ValidRuntime(runtime) {
				return fmt.Errorf("--runtime must be %d, %d or %d (got %d) — %d is the default runtime, %d is the durable one whose steps are journaled, and %d is durable plus a container that reads tables only through tools.query",
					wfdir.RuntimeDefault, wfdir.RuntimeDurable, wfdir.RuntimeQuery, runtime,
					wfdir.RuntimeDefault, wfdir.RuntimeDurable, wfdir.RuntimeQuery)
			}

			// The runtime this folder will actually be CREATED at, which is not
			// the flag's default: an unnamed --runtime means "the instance
			// chooses", and its choice is RuntimeCreateDefault. Everything that
			// TELLS the author what they are getting — the scaffold and the init
			// report — has to speak about that one. Reading `runtime` there
			// instead handed a plain `wf init` an empty file and a report that
			// said nothing about the runtime, while the first push created a
			// workflow on 3: the author then wrote v1-shaped table reads into a
			// runtime that refuses them, and found out at the first run.
			//
			// The manifest key stays ABSENT either way (see below). Writing it
			// because we scaffolded for 3 would pin the folder to 3 and take the
			// first push's ability to write back whatever the server stamped.
			runtimeNamed := cmd.Flags().Changed("runtime")
			effectiveRuntime := runtime
			if !runtimeNamed {
				effectiveRuntime = wfdir.RuntimeCreateDefault
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

			// A durable folder starts from code that is durable — the four
			// structuring rules (journal the discovery step, derive keys rather
			// than write them, return handles, return something from a
			// side-effecting step) are not things anyone infers from an empty
			// file, and a v2 workflow written in v1 shapes journals nothing.
			//
			// Only when there is nothing to overwrite: --from means the author
			// brought their own code, and an existing entrypoint is somebody's
			// work. An explicit --runtime 1 still writes no scaffold at all, so a
			// folder that asked for the standard runtime gets the empty file it
			// always did.
			scaffolded := false
			if effectiveRuntime >= wfdir.RuntimeDurable && fromPath == "" {
				scaffolded, err = writeDurableScaffold(root, entrypoint, effectiveRuntime)
				if err != nil {
					return err
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
			// Written whenever the author NAMED a runtime, --runtime 1 included:
			// an absent key no longer means the standard runtime, it means "the
			// instance chooses", and its choice is not 1 any more. A folder that
			// asked for 1 and said nothing on disk would be created on whatever
			// the instance defaults to — the flag silently doing nothing.
			//
			// Absent when the flag was not passed at all, which keeps that
			// folder's manifest byte-identical to one written before runtimes
			// existed and leaves the choice where the author left it.
			if runtimeNamed {
				manifest.Runtime = runtime
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
					// The EFFECTIVE runtime, always, rather than the manifest key
					// — which is absent when nobody named one. A caller asking
					// what this folder will create wants the answer, not the
					// spelling.
					"runtime":    effectiveRuntime,
					"scaffolded": scaffolded,
				}
				if len(warnings) > 0 {
					payload["warnings"] = warnings
				}
				return emitJSON(payload)
			}
			printInitReport(root, resolved.URL, title, entrypoint, featureID, fromPath, copied, effectiveRuntime, scaffolded)
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
	cmd.Flags().IntVar(&runtime, "runtime", wfdir.RuntimeDefault,
		"workflow runtime to pin in the manifest: 1, 2 for a durable workflow whose steps are journaled and whose failed runs can be resumed, or 3 for durable plus a container that reads tables only through tools.query (unset: the folder declares none and the instance chooses)")
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

// durableScaffoldHeader opens the starter main.py, and is the ONE part of it
// that depends on which journaling runtime the folder declares: runtime 3 adds
// the rule that a Ronja table is read through tools.query and nothing else,
// which is the one thing an author cannot infer from the body below.
//
// The rule is stated, not demonstrated: a commented-out tools.query call would
// have to name a table id, and a fake id is a line nobody can run.
func durableScaffoldHeader(runtime int) string {
	header := fmt.Sprintf(`# Durable workflow (runtime %d). Every @tools.step result is journaled, so
# `+"`ronja wf test --resume`"+` replays the steps that already finished and re-runs
# only the work that never did.
`, runtime)
	if runtime >= wfdir.RuntimeQuery {
		header += `#
# Read a Ronja table ONLY with tools.query("… FROM {{ ref('tbl-id') }} …") — the
# marker goes inside the SQL string, and the container holds no credential for
# the table's files.
`
	}
	return header
}

// durableScaffoldBody is the rest of the starting main.py for a journaling
// workflow — everything below the header, and identical for every runtime that
// journals.
//
// It is not a tutorial: every line is a shape a durable workflow has to be
// written in for a resume to work at all, and the comments state the constraint
// rather than narrate the code. The server seeds its own one-line placeholder
// into every new workflow, so this is what the first push overwrites it with.
//
// It makes no table access at all, which is exactly what keeps it pushable as
// written under runtime 3's marker rules.
const durableScaffoldBody = `

@tools.step
def load_orders():
    # Journal the step that DISCOVERS the iteration set. A resume that recomputed
    # this list could iterate a different one and re-key every step below it.
    return ["A-1", "A-2", "A-3"]


@tools.step
def process(order_id):
    # Keys are derived from the function and its arguments, so each iteration of
    # the loop below is its own journal entry. A durable loop needs no key
    # strings, and a hand-written one would have to stay unique forever.
    #
    # Return a HANDLE — a table name, a file key, an id — never a DataFrame: step
    # results cross the journal as JSON, and the codec refuses a DataFrame as a
    # step argument. The step that needs the rows re-reads them from the handle.
    return "order-" + order_id


@tools.step
def publish(handles):
    # A side effect belongs inside a step, and that step must return a value: a
    # step returning None is not journaled, so a resume would do it a second time.
    return len(handles)


# tools.http is the journaled, requests-compatible client: tools.http.get(url)
# instead of requests.get(url). A raw requests call is NOT journaled and re-runs
# on every resume, side effects included.

# A SEND journals itself too — sendEmail, emitEvent, uploadFile and friends record
# their answer, so a resume replays it rather than sending again. Two identical
# sends in one scope are ONE send; differ an argument to send twice.

# tools.now(), never datetime.now(): the first execution journals the timestamp
# and a resume replays it, so a resumed run computes what the original would have.
# It is an ISO-8601 UTC STRING, not a datetime.
started = tools.now()

handles = []
for order_id in load_orders():
    handles.append(process(order_id))

count = publish(handles)

# END ON A BARE NAME, never on a call. The runtime reads the run body by
# evaluating the last line and refuses to evaluate a call, so a file ending in
# print(...) returns null however well the run went.
summary = f"{count} orders, started {started}"
summary
`

// durableScaffold is the starter main.py for the runtime the author asked for:
// a header that names it, and the shared body. Built rather than duplicated —
// runtime 3 is a superset of Durable and every shape in the body still applies,
// and a second copy would be a second place to keep the four structuring rules
// right.
func durableScaffold(runtime int) string {
	return durableScaffoldHeader(runtime) + durableScaffoldBody
}

// writeDurableScaffold writes the durable starter into the folder, reporting
// whether it wrote anything.
//
// An existing entrypoint is left ALONE rather than refused: `wf init` in a
// directory that already holds main.py is the documented way to adopt a script
// you have, and the one thing worse than an init that scaffolds nothing is one
// that ate the file it found. Nothing else in init depends on the scaffold, so
// there is no failure to report — only a note.
func writeDurableScaffold(root, entrypoint string, runtime int) (bool, error) {
	dest := filepath.Join(root, entrypoint)
	if _, err := os.Stat(dest); err == nil {
		return false, nil
	} else if !os.IsNotExist(err) {
		return false, fmt.Errorf("read %s: %w", dest, err)
	}
	if err := wfdir.WriteFile(root, entrypoint, durableScaffold(runtime)); err != nil {
		return false, err
	}
	return true, nil
}

func printInitReport(root, url, title, entrypoint, featureID, fromPath string, copied bool, runtime int, scaffolded bool) {
	out := os.Stdout
	fmt.Fprintf(out, "  Workflow folder ready in %s\n\n", root)
	fmt.Fprintf(out, "  Title:      %s\n", title)
	fmt.Fprintf(out, "  Entrypoint: %s\n", entrypoint)
	fmt.Fprintf(out, "  Feature:    %s\n", featureID)
	if _, sentence := runtimeInfo(runtime); sentence != "" {
		fmt.Fprintf(out, "  Runtime:    %d — %s\n", runtime, sentence)
	}
	fmt.Fprintf(out, "  Instance:   %s (not pushed yet)\n", url)
	if copied {
		fmt.Fprintf(out, "\n  Copied %s -> %s\n", fromPath, entrypoint)
	} else if scaffolded {
		fmt.Fprintf(out, "\n  Wrote a durable %s to start from.\n", entrypoint)
	} else if fromPath == "" {
		fmt.Fprintf(out, "\n  Write your code in %s.\n", entrypoint)
	}
	fmt.Fprintf(out, "\n  Commit %s with your code; .ronja/ is local-only and ignores itself.\n",
		wfdir.ManifestName)
	fmt.Fprintf(out, "  Next: `ronja wf status` to see where you stand.\n")
}
