package commands

import (
	"context"
	"fmt"
	"regexp"
	"slices"
	"sort"
	"strings"
	"time"

	"github.com/ronjatech/ronja-cli/internal/api"
	"github.com/ronjatech/ronja-cli/internal/wfdir"
	"github.com/spf13/cobra"
)

// `ronja app` is the data-app half of the folder loop `ronja wf` opened.
//
// It earns its place the same way: editing a data app from a folder is stateful
// (which row am I bound to, on which instance), multi-step (fork a draft,
// per-file sync, validate, commit) and needs a drift baseline that lives on
// disk. The same doctrine applies too — sync verbs yes, resource verbs no.
// There is no `app list` and no `app delete`; discovery stays on plain HTTP.
//
// A data app is NOT a workflow with a different file extension, and four
// differences are visible from the first command:
//
//  1. There is no persist-nothing dry run. `wf validate` can check a candidate
//     before anything exists, because POST /workflow/validate takes a whole
//     folder and saves none of it. POST /dataapp/:id/validate compiles a DRAFT,
//     so `app validate` means "check what is on the server" and push runs it at
//     the end rather than before.
//  2. Creating lands LIVE. POST /workflow makes a hidden draft; POST /dataapp
//     makes an empty live row. It serves the pending page until the first
//     publish, but it is visible to the feature from the moment it exists.
//  3. The entrypoint never moves. App.tsx is stamped at create and rdataapp has
//     no field to change it, so the manifest reports it rather than pushing it.
//  4. The allowlists are the folder's business. A workflow's bindings are
//     derived from markers in its code; a data app's are explicit grants nothing
//     server-side can recover from the files, so ronja.json owns them.
func newDataAppCmd() *cobra.Command {
	app := &cobra.Command{
		Use:     "app",
		Aliases: []string{"dataapp"},
		Short:   "Develop a Ronja data app from a local folder",
		Long: `Develop a Ronja data app from a local folder.

A data-app folder holds your React/TSX source plus a small manifest
(ronja.json) recording the app's title, its entrypoint, which tables, secrets,
agents and workflows the app is allowed to reach, and which app it maps to on
each instance. The sync baseline lives in .ronja/, which is local-only and
git-ignored for you.

Two ways in:

  ronja app clone <data-app-id>    copy an existing app down to a folder
  ronja app init --feature <id>    turn a component you already have into one

Then edit locally and check where you stand:

  ronja app status                 local changes, remote state, and drift
  ronja app fork <kit-path>        copy a built-in kit component in to customise
  ronja app push                   sync the folder into your draft and compile
  ronja app validate               recompile your draft and report diagnostics
  ronja app test                   render it headlessly and report what was seen
  ronja app publish                commit the draft (or ask an admin to)
  ronja app discard                throw your draft away

One thing worth knowing before the first push:

  - What the app may query lives in ronja.json under "access". Nothing on the
    server derives it from your code, so an app pushed without it can render
    but not read anything — and nothing reports the omission, since an empty
    allowlist compiles and publishes exactly like a full one. Listing a table
    is all it takes; reading tables needs no "capabilities" entry.

A first push creates the app as an unpublished draft that only you can see, the
same as a workflow, and publish promotes that draft in place — the id never
changes, so a link you shared while authoring keeps working.

Bindings are per-instance, so the same folder can target a local backend and
production without either overwriting the other's binding.`,
	}
	app.AddCommand(
		newDataAppInitCmd(), newDataAppCloneCmd(), newDataAppStatusCmd(),
		newDataAppForkCmd(), newDataAppPushCmd(), newDataAppValidateCmd(),
		newDataAppTestCmd(), newDataAppPublishCmd(), newDataAppDiscardCmd(),
	)
	return app
}

// The data-app file budget, mirrored from backend/resource/rdataapp/
// store_files.go. Checked locally so a folder that cannot be pushed says so
// before the first request rather than failing part-way through a sync.
//
// The COUNT and TOTAL caps matter more here than they would for a workflow: a
// TSX folder plausibly contains node_modules/ or dist/, and while wfdir excludes
// those by name, an author who keeps generated output somewhere else would
// otherwise discover the 100-file ceiling on request 101 — half a push in, with
// the server reporting a count and naming nothing.
const (
	// appMaxFileBytes is rdataapp.maxFileContentBytes. Larger than a workflow's:
	// a bundled component with inline SVG is legitimately big.
	appMaxFileBytes = 5 * 1024 * 1024
	// appMaxFiles is rdataapp.maxFilesPerApp.
	appMaxFiles = 100
	// appMaxTotalBytes is rdataapp.maxTotalBytesPerApp.
	appMaxTotalBytes = 50 * 1024 * 1024
)

// checkAppPushable is checkPushable plus the data-app budget.
//
// Every refusal is local and names what to remove, because the server's version
// of each of these is a late failure with a number in it.
func checkAppPushable(files map[string]string) error {
	if err := checkPushable(files, wfdir.DataAppKind, appMaxFileBytes); err != nil {
		return err
	}
	if len(files) > appMaxFiles {
		return fmt.Errorf("this folder has %d files and a data app holds at most %d.\n  The usual cause is generated output or a dependency tree that is not excluded by name — check `ronja app status`, which lists everything being skipped, and move what is left out of the folder",
			len(files), appMaxFiles)
	}
	total := 0
	for _, content := range files {
		total += len(content)
	}
	if total > appMaxTotalBytes {
		return fmt.Errorf("this folder holds %d bytes of source and a data app holds at most %d in total.\n  Move whatever is not source out of the folder and try again",
			total, appMaxTotalBytes)
	}
	return nil
}

// declarableCapabilities is the closed vocabulary of "access"."capabilities" in
// a data-app manifest: the capabilities the server never hands an app on its
// own, one per SDK call no allowlist implies. Everything else an app can do —
// read the tables it lists, run an allowed agent or workflow, evaluate an
// allowed metric, search a codex, fetch through an allowed secret, read the org
// roster — is derived server-side from the allowlists, so naming it here grants
// nothing that listing the resource did not already grant.
//
// It is the backend's rscripttoken.dataAppAllowedCaps minus everything
// dataAppCaps auto-grants, and that correspondence is pinned there by
// TestDataAppCaps_DeclareOnlySetIsExactlyFour — whose failure message names
// this variable, because the CLI is a separate module and no compiler relates
// the two. The same four names are also written out in `ronja app init --help`,
// in the post-init report, in cli/README.md and on the docs site.
var declarableCapabilities = []string{"ai", "query_external", "write_external", "upload_file"}

// checkDeclaredCapabilities refuses a manifest that grants a capability which
// does not exist.
//
// A refusal rather than a warning, because the failure it replaces has no
// author-visible cause at all: nothing validates "capabilities" on the way in,
// and the token mint FILTERS the stored list (rscripttoken.dataAppCaps), so
// `"upload_fil"` pushes green, publishes green, shows no drift, and then
// surfaces weeks later as a "capability not granted" in one viewer's console.
// This is the only place that sees both the typo and the person who made it.
//
// The message ends by naming the stale-CLI case explicitly. The vocabulary is
// closed but not frozen: a CLI older than the server would otherwise refuse a
// perfectly valid capability while sounding certain it does not exist.
func checkDeclaredCapabilities(access api.DataAppAccess, manifestPath string) error {
	for _, declared := range access.Capabilities {
		if slices.Contains(declarableCapabilities, declared) {
			continue
		}
		return fmt.Errorf("%s grants %q under \"access\".\"capabilities\", and that is not a data-app capability.\n  Only these names ever belong there: %s. Everything else — reading tables, running an allowed agent or workflow, evaluating a metric, codex search, HTTP fetch through a secret — is granted by the allowlist naming the resource, not by a capability.\n  Nothing on the server would have told you: an unknown name is dropped when the app's token is minted, so this would have published green and failed in a viewer's browser.\n  A newer Ronja may accept more than these: if the server's docs name this capability, update the CLI",
			manifestPath, declared, strings.Join(declarableCapabilities, ", "))
	}
	return nil
}

// refuseUnworkableApp reports why a data-app row cannot back a folder, or nil
// when it can. Only live and draft rows are workable: everything else is either
// immutable or governed by a flow the CLI has no business half-implementing.
func refuseUnworkableApp(app *api.DataApp) error {
	switch app.Lifecycle {
	case api.LifecycleLive, api.LifecycleDraft:
		return nil
	case api.LifecycleProposed:
		return fmt.Errorf("data app %s is a proposal awaiting admin approval — it is managed via the proposal flow, not from a folder",
			app.ID)
	case api.LifecycleVersion:
		return fmt.Errorf("data app %s is a committed version snapshot, not an editable app — restore it from the web UI, then clone the result",
			app.ID)
	case api.LifecycleArchived:
		return fmt.Errorf("data app %s is archived — restore it from the web UI first", app.ID)
	default:
		return fmt.Errorf("data app %s has lifecycle %s, which this command does not work with",
			app.ID, api.DescribeLifecycle(app.Lifecycle))
	}
}

// appTarget is what the server holds for a folder's binding: the row the binding
// names, plus the caller's own draft of it.
//
// It exists to make the draft resolution EXPLICIT, and that is not tidiness.
// A draft is its own address: GET /dataapp/:id/files answers with exactly the
// row named (see api.ListDataAppFiles), so a read that means the draft has to
// name it. Every read in this command tree therefore goes through here and
// then addresses Row().ID — never a live id in the hope that the server picks
// the draft.
// Getting that wrong records draft bytes under the live app's identity, which
// makes the drift guard compare two different rows and either invent a conflict
// or miss one.
type appTarget struct {
	// App is the row the binding names: nil for an unbound folder. Live once the
	// app has been published, and a parentless draft before that — a first push
	// creates the app as a draft and the same id is promoted in place on publish.
	App *api.DataApp
	// Draft is the caller's own open draft, nil when they have none.
	Draft *api.DataApp
}

// Row returns the row a push would write to without creating anything, or nil
// when a checkout is still needed.
func (t appTarget) Row() *api.DataApp {
	if t.Draft != nil {
		return t.Draft
	}
	if t.App != nil && t.App.Lifecycle == api.LifecycleDraft {
		return t.App
	}
	return nil
}

// FilesRow is the row whose files describe this folder's current server state:
// the draft when there is one, else the live app. This is the id every file read
// must be addressed to.
func (t appTarget) FilesRow() *api.DataApp {
	if row := t.Row(); row != nil {
		return row
	}
	return t.App
}

// Access is the target's persisted allowlists — the draft's when there is one,
// since that is the row a push writes to and the row a commit copies up.
func (t appTarget) Access() api.DataAppAccess {
	if row := t.Row(); row != nil {
		return row.DataAppAccess
	}
	if t.App != nil {
		return t.App.DataAppAccess
	}
	return api.DataAppAccess{}
}

// inspectAppTarget reads the binding's current state without changing anything,
// resolving the caller's draft explicitly.
func inspectAppTarget(ctx context.Context, client *api.Client, f *folder) (appTarget, error) {
	target, err := inspectAppRow(ctx, client, f)
	if err != nil || target.App == nil || target.App.Lifecycle == api.LifecycleDraft {
		return target, err
	}
	draft, err := client.GetDataAppDraft(ctx, target.App.ID)
	if err != nil {
		return appTarget{}, fmt.Errorf("check for your draft of %s: %w", target.App.ID, err)
	}
	target.Draft = draft
	return target, nil
}

// inspectAppRow is inspectAppTarget WITHOUT the draft round trip: the binding's
// row, the deleted-app message and the unworkable-lifecycle refusal, and nothing
// else.
//
// It is for commands that only need the app itself — `app test` used to be
// one, back when the preview endpoint substituted the caller's draft server-side;
// it now names the draft and goes through inspectAppTarget like every other
// command that needs the draft's id.
func inspectAppRow(ctx context.Context, client *api.Client, f *folder) (appTarget, error) {
	appID, err := f.ResourceID()
	if err != nil {
		return appTarget{}, err
	}
	if appID == "" {
		return appTarget{}, nil
	}
	app, err := client.GetDataApp(ctx, appID)
	if err != nil {
		if api.StatusOf(err) == 404 {
			return appTarget{}, fmt.Errorf("data app %s no longer exists on %s (or you lost access to it) — clone the app you meant into a fresh folder, or remove the binding from %s to start a new one",
				appID, f.Resolved.URL, wfdir.ManifestPath(f.Root))
		}
		return appTarget{}, err
	}
	if err := refuseUnworkableApp(app); err != nil {
		return appTarget{}, err
	}
	return appTarget{App: app}, nil
}

// hashAppFiles fingerprints a fetched file set for baseline and drift
// comparison.
func hashAppFiles(files []api.DataAppFile) map[string]string {
	out := make(map[string]string, len(files))
	for _, f := range files {
		out[f.Path] = wfdir.HashString(f.Content)
	}
	return out
}

// appContentByPath flattens a fetched file set for content comparison.
func appContentByPath(files []api.DataAppFile) map[string]string {
	out := make(map[string]string, len(files))
	for _, f := range files {
		out[f.Path] = f.Content
	}
	return out
}

func appPathsOf(files []api.DataAppFile) []string {
	out := make([]string, 0, len(files))
	for _, f := range files {
		out = append(out, f.Path)
	}
	return out
}

// baselineAccess records the row's allowlists for the drift guard, always
// non-nil. "The server grants nothing" is a fact worth recording: a nil here
// means UNRECORDED, which disarms the guard, and that is only ever right for a
// state file written before allowlists were part of the baseline.
func baselineAccess(source *api.DataApp) *api.DataAppAccess {
	access := source.DataAppAccess.Normalized()
	return &access
}

// baselineFromApp builds the state entry recorded after a successful sync with
// one data-app row.
//
// Title and Entrypoint come from the ROW, never from the manifest: the
// baseline's job is to say what the server held at the last sync, and recording
// what we WANTED it to be would make the drift guard agree with itself forever.
func baselineFromApp(source *api.DataApp, files []api.DataAppFile) *wfdir.InstanceState {
	inst := &wfdir.InstanceState{
		SourceID:          source.ID,
		SourceLifecycle:   source.Lifecycle,
		BaselineUpdatedAt: source.UpdatedAt.UTC().Format(time.RFC3339),
		Title:             source.Name,
		Entrypoint:        source.Entrypoint,
		Access:            baselineAccess(source),
		Files:             map[string]wfdir.FileState{},
	}
	for _, f := range files {
		inst.Files[f.Path] = wfdir.FileState{
			SHA256:    wfdir.HashString(f.Content),
			UpdatedAt: f.UpdatedAt.UTC().Format(time.RFC3339),
		}
	}
	return inst
}

// baselineFromAppLocal is baselineFromApp for content the CLI just WROTE rather
// than read back: after a push, the server holds exactly the bytes we sent.
//
// Per-file UpdatedAt is left empty — the field is diagnostics only (drift is
// decided on the hash), and inventing timestamps we did not receive would be
// worse than saying nothing.
func baselineFromAppLocal(source *api.DataApp, files map[string]string) *wfdir.InstanceState {
	inst := &wfdir.InstanceState{
		SourceID:          source.ID,
		SourceLifecycle:   source.Lifecycle,
		BaselineUpdatedAt: source.UpdatedAt.UTC().Format(time.RFC3339),
		Title:             source.Name,
		Entrypoint:        source.Entrypoint,
		Access:            baselineAccess(source),
		Files:             map[string]wfdir.FileState{},
	}
	for path, content := range files {
		inst.Files[path] = wfdir.FileState{SHA256: wfdir.HashString(content)}
	}
	return inst
}

// accessSlot names one allowlist for rendering. Kept in one place so every
// surface — the push report, the drift line, the status block — names the seven
// lists identically and in the same order.
type accessSlot struct {
	Label string
	IDs   []string
}

func accessSlots(a api.DataAppAccess) []accessSlot {
	n := a.Normalized()
	return []accessSlot{
		{"table", n.AllowedTableIDs},
		{"secret", n.AllowedSecretIDs},
		{"agent", n.AllowedAgentIDs},
		{"workflow", n.AllowedWorkflowIDs},
		{"codex", n.AllowedCodexIDs},
		{"metric", n.AllowedMetricIDs},
		{"capability", n.Capabilities},
	}
}

// describeAccess renders an allowlist set as one line — "2 tables, 1 secret" —
// or "no data access" when empty.
func describeAccess(a api.DataAppAccess) string {
	var parts []string
	for _, slot := range accessSlots(a) {
		if n := len(slot.IDs); n > 0 {
			parts = append(parts, fmt.Sprintf("%d %s", n, plural(n, slot.Label)))
		}
	}
	if len(parts) == 0 {
		return "no data access"
	}
	return strings.Join(parts, ", ")
}

// accessChange is one allowlist that would change, with the ids on each side.
type accessChange struct {
	Label   string   `json:"label"`
	Added   []string `json:"added"`
	Removed []string `json:"removed"`
}

// diffAccess reports what changing `from` into `to` would grant and revoke, per
// list.
//
// Rendered in FULL by push rather than summarised, because these are privileges:
// ronja.json is a committed file that grants a data app's viewers access to the
// tables it names, and "the allowlists differ" is not something anyone can
// review. What can be reviewed is "+ table-abc, − secret-xyz".
func diffAccess(from, to api.DataAppAccess) []accessChange {
	var out []accessChange
	fromSlots, toSlots := accessSlots(from), accessSlots(to)
	for i := range fromSlots {
		added := missingFrom(toSlots[i].IDs, fromSlots[i].IDs)
		removed := missingFrom(fromSlots[i].IDs, toSlots[i].IDs)
		if len(added) == 0 && len(removed) == 0 {
			continue
		}
		out = append(out, accessChange{Label: toSlots[i].Label, Added: added, Removed: removed})
	}
	return out
}

// missingFrom returns the members of ids not present in other, sorted.
func missingFrom(ids, other []string) []string {
	have := make(map[string]bool, len(other))
	for _, id := range other {
		have[id] = true
	}
	var out []string
	for _, id := range ids {
		if !have[id] {
			out = append(out, id)
		}
	}
	sort.Strings(out)
	return out
}

// appNamespaceRe matches the `ronja-app:` prefix esbuild stamps on an author
// file, but ONLY where it introduces a located diagnostic — a path followed by
// `:line:col:`. Anchoring on the digits is what keeps it from eating the same
// text out of a message that merely quotes it.
var appNamespaceRe = regexp.MustCompile(`ronja-app:(\S+?:\d+:\d+:)`)

// stripBundleNamespace removes the bundler's internal namespace from the file
// locations in a compile message, so `ronja app validate` reports
// `components/Today.tsx:37:38: …` — the path the author typed — rather than
// `ronja-app:components/Today.tsx:37:38: …`, which names a namespace they have
// never heard of and cannot open.
//
// Lives here rather than beside either caller: `app push` and `app validate`
// both render a compile message, and the second one to want this is how a
// second copy gets written.
//
// `ronja-vendor:` is deliberately LEFT ALONE. That prefix is the one piece of
// information distinguishing "your file" from "ours": a diagnostic attributed
// to the vendored SDK is a bug in Ronja, not something the author can fix by
// editing the path, and quietly rendering it as a bare filename would send them
// looking for a file their folder does not contain.
//
// Applied at RENDER, not where the message is built: `--json` relays the
// server's answer verbatim, and the human report is the half we format.
func stripBundleNamespace(msg string) string {
	return appNamespaceRe.ReplaceAllString(msg, "$1")
}

// stripBundleNamespacePath is the same removal for a path that arrives as its
// OWN field rather than embedded in a message — a compile diagnostic's File,
// which `ronja app test` renders structurally. The regex above cannot serve it:
// it anchors on the `:line:col:` that follows the path inside a message, and
// there is nothing to anchor on here.
//
// Same two rules, for the same reasons: `ronja-app:` is a namespace the author
// has never heard of, and `ronja-vendor:` stays, because it is what says the
// diagnostic is ours and not theirs.
func stripBundleNamespacePath(file string) string {
	return strings.TrimPrefix(file, "ronja-app:")
}
