package commands

import (
	"fmt"
	"os"
	"path/filepath"

	"github.com/ronjatech/ronja-cli/internal/api"
	"github.com/ronjatech/ronja-cli/internal/wfdir"
	"github.com/spf13/cobra"
)

// `ronja wf clone` copies an existing workflow into a new local folder.
//
// "clone" rather than "checkout" is deliberate muscle-memory: in git, clone is
// what makes a new local copy of something remote, and checkout is the
// branch-switching verb git itself has been moving away from. It is also
// READ-ONLY — no draft is created here. Checkout happens on the first push, so
// cloning to read someone's workflow leaves nothing behind.
func newWorkflowCloneCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "clone <workflow-id> [directory]",
		Short: "Copy a workflow into a new local folder",
		Long: `Copy a workflow into a new local folder.

Fetches the workflow's files and writes them into a directory (named after the
workflow's title unless you say otherwise), together with ronja.json and a
.ronja/ sync baseline:

  ronja wf clone wf-abc123
  ronja wf clone wf-abc123 ./monthly-report

If you already have an open draft of that workflow — from the web builder, or
from an earlier push — its files are cloned instead of the live ones, because
that is your newest state. This is said on stderr when it happens.

Nothing is created on the server: cloning does not open a draft. The target
directory must be empty or absent. Only live workflows and drafts can be
cloned; versions, archived workflows and proposals are refused with a pointer
at the right flow.`,
		Args: cobra.RangeArgs(1, 2),
		RunE: func(cmd *cobra.Command, args []string) error {
			resolved, err := resolveInstance()
			if err != nil {
				return err
			}
			client := api.New(resolved.URL, resolved.Token)
			workflowID := args[0]

			// An explicitly named directory can be rejected before a single
			// request: "that folder is not empty" does not become truer after
			// three round trips, and this is the form CI uses.
			root := ""
			if len(args) == 2 {
				if root, err = checkTarget(args[1]); err != nil {
					return err
				}
			}

			wf, err := client.GetWorkflow(cmd.Context(), workflowID)
			if err != nil {
				if api.StatusOf(err) == 404 {
					return fmt.Errorf("no workflow %s on %s that you can read", workflowID, resolved.URL)
				}
				return err
			}
			if err := refuseUnclonable(wf); err != nil {
				return err
			}

			// Prefer the caller's own draft: it is their newest state, and
			// cloning live over it would look like their web-builder edits had
			// vanished. Best-effort — a failure here degrades to the live files
			// rather than aborting a clone that can perfectly well proceed.
			source := wf
			if wf.Lifecycle == api.LifecycleLive {
				draft, draftErr := client.GetWorkflowDraft(cmd.Context(), wf.ID)
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

			// The default name needs a title, so this one waits for the source —
			// but it still precedes the file fetch, which is the expensive leg.
			if root == "" {
				if root, err = checkTarget(wfdir.Slug(source.Title, wfdir.KindWorkflow)); err != nil {
					return err
				}
			}

			files, err := client.ListWorkflowFiles(cmd.Context(), source.ID)
			if err != nil {
				return fmt.Errorf("read files of workflow %s: %w", source.ID, err)
			}
			// Checked BEFORE anything is created, and fatal rather than
			// per-file: the baseline written below claims every one of these
			// paths is on disk, and a set that cannot all be written produces a
			// folder whose baseline lies from birth — status invents deletions
			// and a push acts on them. See wfdir.CheckLocalPaths.
			if err := wfdir.CheckLocalPaths(pathsOf(files), wfdir.WorkflowKind); err != nil {
				return err
			}

			if err := os.MkdirAll(root, 0o755); err != nil {
				return fmt.Errorf("create %s: %w", root, err)
			}
			for _, f := range files {
				// Validated once more inside WriteFile: the path arrives from
				// the server, and writing it is a local filesystem operation
				// that must not be able to escape the folder.
				if err := wfdir.WriteFile(root, f.Path, f.Content); err != nil {
					return fmt.Errorf("write %s: %w", f.Path, err)
				}
			}

			entrypoint := source.Entrypoint
			if entrypoint == "" {
				entrypoint = wfdir.DefaultEntrypoint
			}
			// The binding is keyed by organization as well as instance, so it
			// has to be known before one is written. Resolved here rather than
			// up front so the cheap refusals above still cost no round trip.
			if err := ensureTenant(cmd.Context(), resolved); err != nil {
				return err
			}
			key := wfdir.InstanceKey{URL: resolved.URL, TenantID: resolved.TenantID}
			manifest := &wfdir.Manifest{
				Kind:       wfdir.KindWorkflow,
				Title:      source.Title,
				Entrypoint: entrypoint,
			}
			// The STABLE identity, not source.ID: when the files came from a
			// draft, the draft's id dies at commit while the binding lives in
			// the customer's git.
			lock, err := recordFirstBinding(manifest, key,
				wfdir.Binding{WorkflowID: wf.IdentityID(), FeatureID: source.FeatureID})
			if err != nil {
				return err
			}
			// The committed anchor: which live version this folder's content
			// descends from. A clone is the cleanest moment there is to record
			// one, and recording it here is what lets a CI checkout of the
			// result push without --force.
			//
			// ⚠️ Read off the SOURCE, not off the current head, and the two are
			// not the same when the files came from a draft: a draft forked
			// before somebody else published sits on an OLDER version, and a
			// folder that recorded the newer one would claim to have seen a
			// publish whose changes it does not contain — after which the first
			// baseline-less push would quietly overwrite it. So live clones
			// anchor on the head, draft clones on the draft's own base, and a
			// draft with no base (a workflow nobody has ever published) anchors
			// on the identity id, which is what an unversioned row's head is.
			if flagStack != "" {
				anchor, err := cloneAnchor(cmd.Context(), workflowHeadReader(client), wf.IdentityID(), source.ID, source.BaseVersionID)
				if err != nil {
					return err
				}
				lock.SetHeadVersion(flagStack, anchor)
			}
			// Always written, even when the workflow declares none: a clone is a
			// faithful copy, and a folder that came down without the key would
			// silently not manage the parameters it can plainly see.
			manifest.SetParameters(source.Parameters)
			// The declared calendar, but ONLY when the row declares one. An empty
			// value means the row declares nothing at all — permanently, not
			// pending a migration — and falls back to the caller's zone at run
			// time, which is a state the API
			// cannot express (an explicit "" patches the row to the literal UTC),
			// so writing the key for it would make the first push silently stamp
			// UTC onto a workflow whose calendar nobody asked to change. Absent
			// leaves it exactly as the server has it.
			if source.ReportingTimezone != "" {
				manifest.SetReportingTimezone(source.ReportingTimezone)
			}
			// The runtime is identity, not preference. A clone whose folder is
			// later pushed into a NEW workflow elsewhere must create THE SAME
			// runtime — absent, the create takes whatever default the instance
			// has, which is nobody's choice and need not be the source's. That
			// is why the STANDARD runtime is written down too: the instance
			// default is no longer 1, so a v1 workflow cloned and re-pushed
			// somewhere else would come back as something its code was not
			// written for.
			//
			// Zero is the one value not recorded: an instance too old to report
			// a runtime has told us nothing, and a folder must not declare a
			// guess. A runtime this build does not KNOW is not recorded either,
			// for the reason spelled out at the create write-back in
			// workflow_push.go: recording it would write a manifest this CLI
			// refuses to open again.
			if source.RuntimeVersion != 0 && wfdir.ValidRuntime(source.RuntimeVersion) {
				manifest.Runtime = source.RuntimeVersion
			}
			if err := wfdir.SaveFolder(root, manifest, lock); err != nil {
				return err
			}
			state := &wfdir.State{}
			// The baseline records the row the files actually came from, which
			// may be the draft even though the binding names the live workflow.
			// No codec: `clone` refuses a destination that is not empty, so the
			// folder it writes declares no dependencies and nothing could be
			// de-aliased. An empty codec is the identity (see alias.go), which is
			// what makes saying so cheaper than threading one through.
			state.Set(key, baselineFrom(aliasCodec{}, source, files))
			if err := wfdir.SaveState(root, state); err != nil {
				return err
			}

			if flagJSON {
				return emitJSON(map[string]any{
					"root":       root,
					"url":        resolved.URL,
					"workflowID": wf.IdentityID(),
					"featureID":  source.FeatureID,
					"title":      source.Title,
					"entrypoint": entrypoint,
					"lifecycle":  wf.Lifecycle,
					// Empty when the row declares none, which is also when the
					// manifest is left unmanaging it.
					"reportingTimezone": source.ReportingTimezone,
					// 1 for the standard runtime; the manifest key is written
					// only when this is 2.
					"runtimeVersion":  source.RuntimeVersion,
					"sourceID":        source.ID,
					"sourceLifecycle": source.Lifecycle,
					"clonedFromDraft": source.ID != wf.ID,
					// No skipped count: a clone that could not write every file
					// fails, so this is always the whole set.
					"files": len(files),
				})
			}
			printCloneReport(root, resolved.URL, wf, source, entrypoint, len(files))
			return nil
		},
	}
	return cmd
}

// checkTarget resolves a clone destination and refuses it if anything is
// already there, returning the absolute path.
//
// Split out so it can run at the earliest moment the path is known — which for
// an explicitly named directory is before any request at all.
func checkTarget(dir string) (string, error) {
	root, err := filepath.Abs(dir)
	if err != nil {
		return "", fmt.Errorf("resolve %s: %w", dir, err)
	}
	empty, err := wfdir.DirIsEmpty(root)
	if err != nil {
		return "", err
	}
	if !empty {
		return "", fmt.Errorf("%s is not empty — clone into a new directory", root)
	}
	return root, nil
}

func pathsOf(files []api.WorkflowFile) []string {
	out := make([]string, 0, len(files))
	for _, f := range files {
		out = append(out, f.Path)
	}
	return out
}

func printCloneReport(root, url string, wf, source *api.Workflow, entrypoint string, written int) {
	out := os.Stdout
	fmt.Fprintf(out, "  Cloned %q into %s\n\n", source.Title, root)
	fmt.Fprintf(out, "  Workflow:   %s (%s)\n", wf.IdentityID(), api.DescribeLifecycle(wf.Lifecycle))
	if source.ID != wf.ID {
		fmt.Fprintf(out, "  Source:     your draft %s\n", source.ID)
	}
	fmt.Fprintf(out, "  Feature:    %s\n", source.FeatureID)
	fmt.Fprintf(out, "  Entrypoint: %s\n", entrypoint)
	// Printed only when the row declares one — the same condition under which
	// the manifest now manages it, so the report says what the folder holds.
	if source.ReportingTimezone != "" {
		fmt.Fprintf(out, "  Timezone:   %s\n", source.ReportingTimezone)
	}
	fmt.Fprintf(out, "  Files:      %d\n", written)
	fmt.Fprintf(out, "  Instance:   %s\n", url)
	fmt.Fprintf(out, "\n  Next: cd %s && ronja wf status\n", filepath.Base(root))
}
