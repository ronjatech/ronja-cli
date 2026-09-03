package commands

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/ronjatech/ronja-cli/internal/api"
	"github.com/ronjatech/ronja-cli/internal/markers"
	"github.com/ronjatech/ronja-cli/internal/wfdir"
	"github.com/spf13/cobra"
)

// `ronja automation init` turns a directory into an automation folder WITHOUT
// creating anything server-side.
//
// Nothing remote, for the reason `pipeline init` gives: a folder is a SET, and a
// create-then-fix loop against a half-built set leaves a feature holding
// automations nobody meant to make. Here the first push is the first thing that
// exists.
//
// --cron and --workflow write a WORKING starter file, which is the answer to the
// alternative this loop was weighed against: declaring automations inside the
// workflow folder they trigger would have made "run this workflow on a cron" one
// commit, and a fifth command group is only worth it if the common case stays
// that cheap.
func newAutomationInitCmd() *cobra.Command {
	var featureID, title, cron, workflow, name string

	cmd := &cobra.Command{
		Use:   "init [directory]",
		Short: "Set up an automation folder",
		Long: `Set up an automation folder.

Writes ronja.json (the manifest). Nothing is created on the server — each
automation comes into existence on the first push of the .json file that
describes it.

  ronja automation init --feature collection-abc123
  ronja automation init --feature collection-abc123 --cron "0 2 * * *" --workflow nightly

--feature names the feature the automations will be created in; find it with
GET /api/v2/feature/query. It is optional here, but a push refuses without one.

--cron and --workflow write a starter file: a cron trigger running a workflow.
--workflow takes an ALIAS, which is declared as a dependency in ronja.json and
bound to this organization's workflow id by ` + "`ronja bind`" + ` — that is what lets
one committed folder deploy to several organizations.

Every .json file in the folder is one automation, and its filename is the
automation's name. Anything else is ignored.`,
		Args: cobra.MaximumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			featureID = strings.TrimSpace(featureID)
			// By SHAPE, before anything else happens — a typo is otherwise
			// invisible until the first push. Shared with `pipeline init`, since
			// a feature id is a feature id.
			if err := checkFeatureIDShape(featureID); err != nil {
				return err
			}
			workflow = strings.TrimSpace(workflow)
			cron = strings.TrimSpace(cron)
			name = strings.TrimSpace(name)
			if workflow != "" {
				if err := wfdir.ValidAliasName(workflow); err != nil {
					return fmt.Errorf("--workflow %q cannot be a dependency alias: %w\n  It is an ALIAS this folder declares, not a workflow id — `ronja bind` fills in the id", workflow, err)
				}
			}
			// Checked where it is ACCEPTED rather than where it is written.
			// wfdir.ValidatePath sees the path at the write and can only name the
			// path; this names the flag, which is the thing the reader typed.
			if err := checkStarterAutomationName(name); err != nil {
				return err
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
			if len(args) == 1 {
				if root, err = filepath.Abs(args[0]); err != nil {
					return fmt.Errorf("resolve %s: %w", args[0], err)
				}
			}
			// The refusal comes BEFORE the directory is created, so a second
			// `init` leaves the filesystem exactly as it found it.
			if _, err := os.Stat(wfdir.ManifestPath(root)); err == nil {
				return fmt.Errorf("%s already exists — this directory is already a synced folder",
					wfdir.ManifestPath(root))
			} else if !os.IsNotExist(err) {
				return fmt.Errorf("read %s: %w", wfdir.ManifestPath(root), err)
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
				Kind:  wfdir.KindAutomation,
				Title: title,
				// No entrypoint key at all: a folder of automations is a SET with
				// no distinguished member, so there is nothing for the concept to
				// name.
			}
			if workflow != "" {
				manifest.Dependencies = map[string]wfdir.Dependency{
					workflow: {Kind: markers.KindWorkflow},
				}
			}
			lock, err := recordFirstBinding(manifest,
				wfdir.InstanceKey{URL: resolved.URL, TenantID: resolved.TenantID},
				wfdir.Binding{FeatureID: featureID})
			if err != nil {
				return err
			}
			if err := wfdir.SaveFolder(root, manifest, lock); err != nil {
				return err
			}
			// ⚠️ NO .ronja/ — this is the one folder kind with no local baseline.
			// Every other loop keeps one because it cannot otherwise tell an edit
			// from a colleague's write; here a read returns the whole live
			// configuration, so the comparison is file-against-server and needs
			// nothing per-user. Writing an empty baseline would be a file nothing
			// ever reads, in a directory nothing ever writes.

			starter := ""
			if cron != "" || workflow != "" || name != "" {
				if starter, err = writeStarterAutomation(root, name, cron, workflow); err != nil {
					return err
				}
			}

			if flagJSON {
				return emitJSON(map[string]any{
					"root":        root,
					"manifest":    wfdir.ManifestPath(root),
					"url":         resolved.URL,
					"kind":        wfdir.KindAutomation,
					"title":       title,
					"featureID":   featureID,
					"bound":       true,
					"automations": 0,
					// The starter file this init wrote, empty when it wrote none.
					"file": starter,
				})
			}
			printAutomationInitReport(root, resolved.URL, title, featureID, starter, workflow)
			return nil
		},
	}

	cmd.Flags().StringVar(&featureID, "feature", "",
		"feature the automations will be created in (a push refuses without one)")
	cmd.Flags().StringVar(&title, "title", "",
		"folder title (default: the directory name)")
	cmd.Flags().StringVar(&cron, "cron", "",
		"write a starter automation on this cron schedule")
	cmd.Flags().StringVar(&workflow, "workflow", "",
		"the starter automation runs this workflow, named by an alias this folder declares")
	cmd.Flags().StringVar(&name, "name", "",
		"name the starter automation (default: the workflow alias)")
	return cmd
}

// checkStarterAutomationName refuses a --name this folder could not read back as
// the automation it names.
//
// --workflow is validated and --name was not, so `--name a/b` wrote `a/b.json`
// — a real file, in a subdirectory, whose automation is then called `b`, because
// automationNameFor takes the stem of the BASE. Not a security hole (ValidatePath
// still refuses traversal at the write) but a silent disagreement between what
// the reader asked for and what the folder holds.
//
// Tested against the FOLDER's own rule rather than a second list of characters:
// a dot-name is never synced, so `--name .nightly` writes a starter file `init`
// reports and no later `status` or `push` can see.
func checkStarterAutomationName(name string) error {
	if name == "" {
		return nil
	}
	if strings.ContainsAny(name, `/\`) {
		return fmt.Errorf("--name %q cannot hold a path separator: it is the starter FILE's name as well as the automation's, so it writes a file in a subdirectory and names the automation after the last segment only", name)
	}
	path := name + wfdir.AutomationKind.SyncExt
	if reason := wfdir.NotSyncable(path, wfdir.AutomationKind); reason != "" {
		return fmt.Errorf("--name %q would write %s, which this folder does not sync: %s", name, path, reason)
	}
	return nil
}

// defaultStarterCron is what a starter file gets when --workflow was given
// without one. 02:00 daily is the schedule an overnight rebuild actually wants,
// and a file with no cronExpr would be refused by the server rather than left
// for the author to fill in.
const defaultStarterCron = "0 2 * * *"

// writeStarterAutomation writes one working automation file and returns its
// path.
//
// "Working" is the requirement: the file it writes must push as it stands, so it
// carries the fields a cron trigger needs and NOTHING ELSE. In particular it
// leaves `enabled` ABSENT — unmanaged — which is the state the three-state
// pointer exists for. A starter file declaring `enabled: true` is exactly the
// line that turns a paused automation back on months later, and writing one here
// would put it in every folder this command creates.
func writeStarterAutomation(root, name, cron, workflow string) (string, error) {
	if name == "" {
		name = workflow
	}
	if name == "" {
		name = "nightly"
	}
	if cron == "" {
		cron = defaultStarterCron
	}
	file := automationFile{
		TriggerKind: strPtr(api.TriggerKindCron),
		CronExpr:    &cron,
	}
	if workflow != "" {
		file.Action = &automationActionFile{
			Kind:   api.ActionKindWorkflow,
			Config: automationActionConfigFile{WorkflowID: &workflow},
		}
	} else {
		// No workflow named, so the automation runs the agent from a prompt. An
		// EMPTY prompt would be refused by the server, so the placeholder says
		// what to do rather than being a valid-looking blank.
		file.Action = &automationActionFile{Kind: api.ActionKindAgent}
		file.Prompt = strPtr("Describe what the agent should do on each run.")
	}
	body, err := json.MarshalIndent(file, "", "  ")
	if err != nil {
		return "", fmt.Errorf("render the starter automation: %w", err)
	}
	path := name + wfdir.AutomationKind.SyncExt
	if err := wfdir.WriteFile(root, path, string(body)+"\n"); err != nil {
		return "", fmt.Errorf("write %s: %w", path, err)
	}
	return path, nil
}

func strPtr(s string) *string { return &s }

func printAutomationInitReport(root, url, title, featureID, starter, workflow string) {
	out := os.Stdout
	fmt.Fprintf(out, "  Automation folder ready in %s\n\n", root)
	fmt.Fprintf(out, "  Title:    %s\n", title)
	if featureID != "" {
		fmt.Fprintf(out, "  Feature:  %s\n", featureID)
	} else {
		fmt.Fprintf(out, "  Feature:  none yet — add \"featureID\" to the instance entry in %s before pushing\n",
			wfdir.ManifestName)
	}
	fmt.Fprintf(out, "  Instance: %s (nothing pushed yet)\n", url)
	if starter != "" {
		fmt.Fprintf(out, "  File:     %s\n", starter)
	}
	fmt.Fprintf(out, "\n  Write one .json file per automation; the filename is its name, and anything\n")
	fmt.Fprintf(out, "  else in the folder is ignored. Commit %s and the files together.\n", wfdir.ManifestName)
	if workflow != "" {
		fmt.Fprintf(out, "\n  %s is declared as a workflow dependency and is not bound yet.\n", workflow)
		fmt.Fprintf(out, "  Next: `ronja bind` to point it at this organization's workflow, then\n")
		fmt.Fprintf(out, "        `ronja automation push`.\n")
		return
	}
	fmt.Fprintf(out, "  Next: `ronja automation status` to see where you stand.\n")
}
