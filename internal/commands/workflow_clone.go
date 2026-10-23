package commands

import (
	"context"
	"fmt"
	"os"
	"path/filepath"

	"github.com/ronjatech/ronja-cli/internal/api"
	"github.com/ronjatech/ronja-cli/internal/config"
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

			files, entrypoint, err := writeWorkflowClone(cmd.Context(), client, resolved, wf, source, root, cloneHooks{})
			if err != nil {
				return err
			}
			notices := appendManagedNotice(nil,
				noticeManagedBy(nil, wfdir.WorkflowKind, wfdir.ManagedWorkflow, wf.IdentityID(), wf.ManagedBy, noticeForClone))
			printManagedNotices(notices)

			if flagJSON {
				return emitJSON(withManagedInGit(map[string]any{
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
				}, notices))
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

// cloneHooks are what `ronja import feature` changes about writing a folder:
// the alias form its files and declarations are written in, the dependencies and
// binds that make that form resolve, and the folder id. ZERO for every clone
// command, which is what keeps a clone byte-identical to what it always wrote —
// the import writes each folder through the same function rather than a fork.
type cloneHooks struct {
	// codec rewrites what the server holds into the folder's alias form (an
	// empty codec is the identity).
	codec aliasCodec
	deps  map[string]wfdir.Dependency
	bind  map[string]string
	// folderID makes the folder adopted (format version 6): only a local
	// command mints one, and import is one.
	folderID string
	// access is a data app's allowlist block, already in alias form.
	access *api.DataAppAccess
	// recordTitle records the row's title as the stack's APPLIED title, the
	// way a push that finds live equal to desired does (recordAppliedTitle) —
	// import clones the live row, so the title it writes is the live one, and
	// an apply that had to record it itself would write an anchor.
	recordTitle bool
}

// appliedTitle records title as the stack's applied title when the hooks ask.
func (h cloneHooks) appliedTitle(lock *wfdir.Lock, title string) {
	if h.recordTitle && flagStack != "" {
		lock.SetAppliedTitle(flagStack, title)
	}
}

// finishManifest writes the hooks' declarations into a manifest the clone has
// bound: the dependencies, the selected stack's binds, and the folder id.
func (h cloneHooks) finishManifest(m *wfdir.Manifest) {
	if len(h.deps) > 0 {
		m.Dependencies = h.deps
	}
	if h.folderID != "" {
		m.ID = h.folderID
	}
	if len(h.bind) > 0 && flagStack != "" {
		st := m.Stacks[flagStack]
		st.Bind = h.bind
		m.Stacks[flagStack] = st
	}
}

// writeWorkflowClone lays a workflow down in root — files, ronja.json, the lock
// with its head-version anchor and the local baseline — from `source` (the live
// row or the caller's draft) of the workflow `wf`. Shared by `wf clone` and
// `import feature`.
func writeWorkflowClone(ctx context.Context, client *api.Client, resolved *config.Resolved, wf, source *api.Workflow, root string, hooks cloneHooks) ([]api.WorkflowFile, string, error) {
	files, err := client.ListWorkflowFiles(ctx, source.ID)
	if err != nil {
		return nil, "", fmt.Errorf("read files of workflow %s: %w", source.ID, err)
	}
	// Checked BEFORE anything is created, and fatal rather than
	// per-file: the baseline written below claims every one of these
	// paths is on disk, and a set that cannot all be written produces a
	// folder whose baseline lies from birth — status invents deletions
	// and a push acts on them. See wfdir.CheckLocalPaths.
	// The language is the row's, and only js is written: a python
	// manifest never carries the key.
	language := ""
	switch source.Language {
	case "", wfdir.LanguagePython:
	case wfdir.LanguageJS:
		language = wfdir.LanguageJS
	default:
		return nil, "", fmt.Errorf("workflow %s is written in %q, which this CLI does not know — run `ronja update`",
			source.ID, source.Language)
	}
	kind := wfdir.WorkflowKind.ForManifest(&wfdir.Manifest{Kind: wfdir.KindWorkflow, Language: language})
	if err := wfdir.CheckLocalPaths(pathsOf(files), kind); err != nil {
		return nil, "", err
	}

	if err := os.MkdirAll(root, 0o755); err != nil {
		return nil, "", fmt.Errorf("create %s: %w", root, err)
	}
	for _, f := range files {
		// Validated once more inside WriteFile: the path arrives from
		// the server, and writing it is a local filesystem operation
		// that must not be able to escape the folder.
		// In the folder's alias form (an empty codec, for a clone, is the
		// identity): what the server holds, de-aliased exactly as every later
		// remote read is, so the first status compares equal.
		if err := wfdir.WriteFile(root, f.Path, hooks.codec.toDisk(f.Content)); err != nil {
			return nil, "", fmt.Errorf("write %s: %w", f.Path, err)
		}
	}

	entrypoint := source.Entrypoint
	if entrypoint == "" {
		entrypoint = kind.DefaultEntrypoint
	}
	// The binding is keyed by organization as well as instance, so it
	// has to be known before one is written. Resolved here rather than
	// up front so the cheap refusals above still cost no round trip.
	if err := ensureTenant(ctx, resolved); err != nil {
		return nil, "", err
	}
	key := wfdir.InstanceKey{URL: resolved.URL, TenantID: resolved.TenantID}
	manifest := &wfdir.Manifest{
		Kind:       wfdir.KindWorkflow,
		Title:      source.Title,
		Language:   language,
		Entrypoint: entrypoint,
	}
	// The STABLE identity, not source.ID: when the files came from a
	// draft, the draft's id dies at commit while the binding lives in
	// the customer's git.
	lock, err := recordFirstBinding(manifest, key,
		wfdir.Binding{WorkflowID: wf.IdentityID(), FeatureID: source.FeatureID})
	if err != nil {
		return nil, "", err
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
		anchor, err := cloneAnchor(ctx, workflowHeadReader(client), wf.IdentityID(), source.ID, source.BaseVersionID)
		if err != nil {
			return nil, "", err
		}
		lock.SetHeadVersion(flagStack, anchor)
	}
	// Always written, even when the workflow declares none: a clone is a
	// faithful copy, and a folder that came down without the key would
	// silently not manage the parameters it can plainly see.
	manifest.SetParameters(hooks.codec.parametersToDisk(source.Parameters))
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
	// guess. A runtime this build does not KNOW is not recorded either:
	// LoadManifest refuses an unknown runtime, so recording it would
	// write a manifest this CLI refuses to open again.
	if source.RuntimeVersion != 0 && wfdir.ValidRuntime(source.RuntimeVersion) {
		manifest.Runtime = source.RuntimeVersion
	}
	hooks.finishManifest(manifest)
	hooks.appliedTitle(lock, source.Title)
	if err := wfdir.SaveFolder(root, manifest, lock); err != nil {
		return nil, "", err
	}
	state := &wfdir.State{}
	// The baseline records the row the files actually came from, which
	// may be the draft even though the binding names the live workflow —
	// de-aliased by the same codec the files were written through (empty,
	// and so the identity, for a clone).
	state.Set(key, baselineFrom(hooks.codec, source, files))
	if err := wfdir.SaveState(root, state); err != nil {
		return nil, "", err
	}

	return files, entrypoint, nil
}
