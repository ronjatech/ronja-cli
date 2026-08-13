package commands

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/ronjatech/ronja-cli/internal/api"
	"github.com/ronjatech/ronja-cli/internal/config"
	"github.com/ronjatech/ronja-cli/internal/wfdir"
	"github.com/spf13/cobra"
)

// `ronja wf` is the one place the CLI owns more than a bootstrap.
//
// The design rule is "sync verbs yes, resource verbs no" — discovery, reads and
// one-shot writes belong on plain HTTP, and wrapping them would make the CLI a
// permanently incomplete mirror of the API. What justifies these commands is
// the same thing that justifies `login`: a loop HTTP does badly on its own.
// Editing a workflow from a folder is stateful (which row am I bound to, on
// which instance), multi-step (checkout, per-file sync, commit) and needs a
// drift baseline that lives on disk. Discovery still is not: there is
// deliberately no `wf list` and no `wf delete`.
func newWorkflowCmd() *cobra.Command {
	wf := &cobra.Command{
		Use:     "workflow",
		Aliases: []string{"wf"},
		Short:   "Develop a Ronja workflow from a local folder",
		Long: `Develop a Ronja workflow from a local folder.

A workflow folder holds your Python source plus a small manifest (ronja.json)
recording the workflow's title, its entrypoint, and which workflow it maps to
on each instance. The sync baseline lives in .ronja/, which is local-only and
git-ignored for you.

Two ways in:

  ronja wf clone <workflow-id>     copy an existing workflow down to a folder
  ronja wf init --feature <id>     turn a script you already have into one

Then edit locally and check where you stand:

  ronja wf status                  local changes, remote state, and drift
  ronja wf validate                check the folder server-side, save nothing
  ronja wf push                    sync the folder into your draft
  ronja wf test                    run your draft and report what happened
  ronja wf publish                 take the draft live (or ask an admin to)
  ronja wf discard                 throw your draft away

Bindings are per-instance, so the same folder can target a local backend and
production without either overwriting the other's binding.`,
	}
	wf.AddCommand(
		newWorkflowInitCmd(), newWorkflowCloneCmd(), newWorkflowStatusCmd(),
		newWorkflowValidateCmd(), newWorkflowPushCmd(), newWorkflowTestCmd(),
		newWorkflowPublishCmd(), newWorkflowDiscardCmd(),
	)
	return wf
}

// resolveInstance is the standard preamble: work out which instance we are
// talking to and refuse early if there is no credential for it.
func resolveInstance() (*config.Resolved, error) {
	resolved, err := config.Resolve(flagURL, flagProfile)
	if err != nil {
		return nil, err
	}
	if resolved.Token == "" {
		return nil, notSignedIn(resolved)
	}
	return resolved, nil
}

// ensureTenant fills in the ORGANIZATION, which the wf commands need because a
// workflow folder's binding is keyed by it: guessing it wrong produces a
// manifest whose binding no command will ever match — or, worse, a push that
// creates a duplicate workflow in the wrong place.
//
// Under a $RONJA_TOKEN credential it is asked of the SERVER, and never taken
// from a profile. The two are independent: `eval "$(ronja env)"` exports a
// token, and a later `ronja profile use other-org` in the same shell would
// otherwise bind one organization's workflow id to the other's credential.
//
// Called at the point the key is first NEEDED rather than as a preamble, so the
// cheap local refusals — a destination directory that is not empty, a file too
// big to push — still happen before anything goes over the network. A stored
// profile already records the organization, so this costs a request only on the
// environment-token path.
//
// `ronja api` and `ronja query` never call it: they key nothing locally, so the
// organization is the server's business.
func ensureTenant(ctx context.Context, resolved *config.Resolved) error {
	if resolved.TenantID != "" {
		return nil
	}
	me, err := api.New(resolved.URL, resolved.Token).Me(ctx)
	if err != nil {
		return fmt.Errorf("ask %s which organization this token belongs to: %w", resolved.URL, err)
	}
	if me.Tenant == nil {
		return fmt.Errorf("this token belongs to no organization, so there is nothing to work with — join one in the web app first")
	}
	resolved.TenantID = me.Tenant.ID
	return nil
}

// folder is an opened synced folder — a workflow's or a data app's — plus the
// instance it is being read against.
type folder struct {
	Root string
	// Kind is what this folder describes. It decides which manifest kind is
	// accepted, which files are syncable, and which binding field names the row,
	// so it is carried rather than re-derived: a helper that guessed would be one
	// place for the two kinds to disagree.
	Kind     wfdir.Kind
	Manifest *wfdir.Manifest
	State    *wfdir.State
	Resolved *config.Resolved
	// Key is what the manifest and baseline are looked up by: this instance and
	// this organization.
	Key wfdir.InstanceKey
	// Binding and Bound describe this folder's link to Key. Unbound is an
	// ordinary state, not an error: it means the folder has never been pushed
	// there.
	Binding wfdir.Binding
	Bound   bool
	// BindingErr is set only when the ORGANIZATION is unknown and the folder is
	// bound to several on this instance — so it can only happen to a caller
	// that skipped resolveInstance, which today is the signed-out half of `wf
	// status`. Everything else knows its organization and matches exactly.
	BindingErr error
}

// openFolder finds the folder root from the working directory and loads both
// files. The manifest is required; a missing baseline is not (a colleague who
// cloned the repo from git correctly has no .ronja/).
//
// The ORGANIZATION is NOT resolved here. Reading a folder does not need one: a
// matched binding names the organization it belongs to, and adopting that is
// both free and more correct than asking the server what the ambient credential
// happens to be. Only WRITING a binding needs an authoritative answer, and the
// commands that do call ensureTenant at that point — which is also what keeps a
// local refusal (a file with null bytes, a directory that is not empty) costing
// no round trip.
func openFolder(ctx context.Context, resolved *config.Resolved, kind wfdir.Kind) (*folder, error) {
	cwd, err := os.Getwd()
	if err != nil {
		return nil, fmt.Errorf("locate working directory: %w", err)
	}
	root, err := wfdir.FindRoot(cwd)
	if err != nil {
		if errors.Is(err, wfdir.ErrNoManifest) {
			cmd := kindCommand(kind)
			return nil, fmt.Errorf("no %s here or in any parent directory — run this inside a folder created by `%s clone` or `%s init`",
				wfdir.ManifestName, cmd, cmd)
		}
		return nil, err
	}
	// LoadManifest refuses a folder of the OTHER kind, which is the guard that
	// keeps `ronja wf push` from creating a workflow out of a data app's TSX.
	manifest, err := wfdir.LoadManifest(root, kind)
	if err != nil {
		return nil, err
	}
	state, err := wfdir.LoadState(root)
	if err != nil {
		return nil, err
	}
	key := wfdir.InstanceKey{URL: resolved.URL, TenantID: resolved.TenantID}
	entry, bound, bindingErr := manifest.BindingEntry(key)
	if bound {
		// Adopt the matched entry's organization. When ours was already known
		// this changes nothing (the match was exact on it); when it was not,
		// this is the answer, and it cost no request.
		key = entry.Key()
	}
	return &folder{
		Root: root, Kind: kind, Manifest: manifest, State: state,
		Resolved: resolved, Key: key,
		Binding: entry.Binding, Bound: bound, BindingErr: bindingErr,
	}, nil
}

// kindCommand names the command group that drives a folder of this kind, for
// messages that tell the caller what to run next. A data-app failure that
// answers "run `ronja wf status`" points at a command that refuses the folder
// it was said about.
func kindCommand(kind wfdir.Kind) string {
	if kind.Name == wfdir.KindDataApp {
		return "ronja app"
	}
	return "ronja wf"
}

// ResourceID is the row this folder is bound to on its instance, empty when it
// has never been pushed there.
func (f *folder) ResourceID() string { return f.Binding.ResourceID(f.Kind) }

// bindNew resolves the organization and returns the key a NEW binding should be
// written under. Only the paths that create a binding need this, and they are
// the only ones that must not guess: an unbound folder has nothing to adopt an
// organization from, and writing one with the wrong organization — or none —
// produces a manifest no later command matches.
func (f *folder) bindNew(ctx context.Context) (wfdir.InstanceKey, error) {
	if err := ensureTenant(ctx, f.Resolved); err != nil {
		return wfdir.InstanceKey{}, err
	}
	f.Key = wfdir.InstanceKey{URL: f.Resolved.URL, TenantID: f.Resolved.TenantID}
	return f.Key, nil
}

// describeTarget names where a command just wrote, as instance plus
// organization.
//
// Reported by the commands that CHANGE something, not only by the ones you run
// when you are already suspicious. `context` and `status` are what you check
// after the fact; a push is where sending a folder to the wrong organization
// actually happens, and it is worth one line to make that visible at the moment
// it occurs.
//
// The organization's NAME when a profile recorded one, its id otherwise (an
// environment token was never introduced to us).
func describeTarget(resolved *config.Resolved) string {
	org := ""
	if resolved.Entry != nil && !resolved.FromEnv {
		org = resolved.Entry.TenantName
	}
	if org == "" {
		org = resolved.TenantID
	}
	if org == "" {
		return resolved.URL
	}
	return fmt.Sprintf("%s on %s", org, resolved.URL)
}

// refuseUnclonable reports why a workflow row cannot back a folder, or nil when
// it can. Only live and draft rows are workable: everything else is either
// immutable or governed by a flow the CLI has no business half-implementing.
func refuseUnclonable(wf *api.Workflow) error {
	switch wf.Lifecycle {
	case api.LifecycleLive, api.LifecycleDraft:
		return nil
	case api.LifecycleProposed:
		return fmt.Errorf("workflow %s is a proposal awaiting admin approval — it is managed via the proposal flow, not from a folder",
			wf.ID)
	case api.LifecycleVersion:
		return fmt.Errorf("workflow %s is a committed version snapshot, not an editable workflow — restore it from the web UI, then clone the result",
			wf.ID)
	case api.LifecycleArchived:
		return fmt.Errorf("workflow %s is archived — restore it from the web UI first", wf.ID)
	default:
		return fmt.Errorf("workflow %s has lifecycle %s, which this command does not work with",
			wf.ID, api.DescribeLifecycle(wf.Lifecycle))
	}
}

// hashFiles fingerprints a fetched file set for baseline and drift comparison.
func hashFiles(files []api.WorkflowFile) map[string]string {
	out := make(map[string]string, len(files))
	for _, f := range files {
		out[f.Path] = wfdir.HashString(f.Content)
	}
	return out
}

// readLocalFiles enumerates a folder and reads the content of every syncable
// file, so the same set can be validated and pushed without walking twice.
//
// Content is held in memory on purpose: a workflow is source code, the server
// stores it in a text column, and the push guard below caps what may be sent
// anyway — so the whole folder is small by construction.
func readLocalFiles(root string, kind wfdir.Kind) (map[string]string, *wfdir.Enumeration, error) {
	enumeration, err := wfdir.Enumerate(root, kind)
	if err != nil {
		return nil, nil, err
	}
	files := make(map[string]string, len(enumeration.Files))
	for path := range enumeration.Files {
		body, err := os.ReadFile(filepath.Join(root, filepath.FromSlash(path)))
		if err != nil {
			return nil, nil, fmt.Errorf("read %s: %w", path, err)
		}
		files[path] = string(body)
	}
	return files, enumeration, nil
}

// maxFileBytes caps one WORKFLOW file, on both the push and validate paths.
//
// The file-SAVE path has no explicit limit — content is a text column — which
// is why the CLI needs one: pushing a 200 MB CSV somebody dropped in the folder
// fails somewhere deep in the request path with a message about nothing, and
// the diagnosis ("that is not source code") is entirely local. A megabyte is
// far beyond any hand-written Python file.
//
// The VALIDATE endpoint does have one, and it is deliberately this same number
// (rworkflow.maxValidateFileBytes) — so a folder this refuses is one the server
// would refuse anyway, less helpfully, and a folder it accepts is never
// rejected server-side for its size. Change both.
//
// Data apps have their own, LARGER limit (see appMaxFileBytes), which is why
// checkPushable takes the cap as an argument rather than reading this: refusing
// a 2 MB .tsx that rdataapp would have accepted is the same class of unhelpful
// local refusal, just pointed the other way.
const maxFileBytes = 1 << 20

// checkPushable refuses a local file set that cannot be synced safely, BEFORE
// any request is made.
//
// `wf validate` runs it too, so the messages below say "synced" rather than
// "pushed": a validate that refused a 200 MB CSV by telling you to fix it and
// push again would be answering a question nobody asked.
//
// Three refusals, all hard rather than skip-with-a-warning, because in each
// case skipping silently produces a workflow that differs from the folder the
// author is looking at:
//
//   - Paths differing only by case. They are two rows server-side and ONE file
//     on macOS or Windows, so a colleague cloning this workflow there loses
//     one of them. Possible to create on Linux, so it is checked everywhere
//     rather than left to the filesystem to notice.
//   - Files over maxFileBytes, or containing NUL bytes. `content` is a text
//     column; binary content is either rejected with an opaque encoding error
//     or stored as something nobody can read back.
func checkPushable(files map[string]string, maxBytes int) error {
	byFold := map[string][]string{}
	for path := range files {
		fold := strings.ToLower(path)
		byFold[fold] = append(byFold[fold], path)
	}
	var collisions []string
	for _, group := range byFold {
		if len(group) < 2 {
			continue
		}
		sort.Strings(group)
		collisions = append(collisions, strings.Join(group, " and "))
	}
	if len(collisions) > 0 {
		sort.Strings(collisions)
		return fmt.Errorf("this folder has file paths that differ only by capitalisation, which are one file on a case-insensitive filesystem:\n    %s\n  Rename one of each pair before syncing",
			strings.Join(collisions, "\n    "))
	}

	var rejected []string
	for _, path := range sortedPaths(files) {
		content := files[path]
		switch {
		case len(content) > maxBytes:
			rejected = append(rejected, fmt.Sprintf("%s (%d bytes, limit %d)", path, len(content), maxBytes))
		case strings.ContainsRune(content, 0):
			rejected = append(rejected, path+" (binary content: contains null bytes)")
		}
	}
	if len(rejected) > 0 {
		return fmt.Errorf("these files cannot be synced to Ronja — a workflow holds source code, not data or binaries:\n    %s\n  Remove them from the folder (or keep them out of it) and try again",
			strings.Join(rejected, "\n    "))
	}
	return nil
}

// sortedPaths orders a file map's keys, so every listing and every request
// sequence is deterministic.
func sortedPaths(files map[string]string) []string {
	out := make([]string, 0, len(files))
	for path := range files {
		out = append(out, path)
	}
	sort.Strings(out)
	return out
}

// validateFilesOf turns a local file set into the validate request's file list.
func validateFilesOf(files map[string]string) []api.ValidateFile {
	out := make([]api.ValidateFile, 0, len(files))
	for _, path := range sortedPaths(files) {
		out = append(out, api.ValidateFile{Path: path, Content: files[path]})
	}
	return out
}

// bindingsOf reads a workflow row's stamped bindings into the same shape
// validate reports, so "what this would bind to" and "what it now binds to"
// print identically.
func bindingsOf(wf *api.Workflow) api.ValidateBindings {
	return api.ValidateBindings{
		InputTableIDs:  wf.InputTableIDs,
		OutputTableIDs: wf.OutputTableIDs,
		SecretIDs:      wf.SecretIDs,
		QuerySecretIDs: wf.QuerySecretIDs,
		AgentIDs:       wf.AgentIDs,
		CodexIDs:       wf.CodexIDs,
	}
}

// describeBindings renders a binding set as one line — "2 input tables, 1
// output table, 1 secret" — or "no data or secret bindings" when empty.
func describeBindings(b api.ValidateBindings) string {
	var parts []string
	for _, kind := range []struct {
		ids      []string
		singular string
	}{
		{b.InputTableIDs, "input table"},
		{b.OutputTableIDs, "output table"},
		{b.SecretIDs, "secret"},
		{b.QuerySecretIDs, "query secret"},
		{b.AgentIDs, "agent"},
		{b.CodexIDs, "codex"},
	} {
		if n := len(kind.ids); n > 0 {
			parts = append(parts, fmt.Sprintf("%d %s", n, plural(n, kind.singular)))
		}
	}
	if len(parts) == 0 {
		return "no data or secret bindings"
	}
	return strings.Join(parts, ", ")
}

// The width of a report's key column, which is not the same everywhere: push
// and publish align to "Draft:    ", while a status block is one word wider —
// "Entrypoint: " sets it. Passed to printResourceURL rather than duplicating
// its rule per column, since which column a report uses says nothing about
// when a link should be printed.
const (
	reportKeyWidth = 10
	statusKeyWidth = 12
)

// printResourceURL closes a report with the frontend page for what was just
// written, in the same key-value column as the ids above it — keyWidth being
// that column, one of the two constants above.
//
// The url is whatever the SERVER put on the response we already hold, and this
// is the only thing done with it — nothing here templates a route or picks an
// origin. That is the point rather than tidiness: the instance URL a profile
// records is the API origin, and the deploy template puts the backend on api.*
// and the frontend on app.*, so the link this replaces pointed at a host that
// serves no pages. (The server had the same bug from its own side; both were
// fixed by giving one package the mapping AND the origin, backend/lib/deeplink.)
//
// ABSENCE IS SILENT. An instance with no configured frontend origin returns no
// url — the normal state of a dev box, which is exactly where this gets tested
// — so there is nothing to say and nothing to warn about. No link, no note, no
// non-zero exit, and above all no locally-guessed fallback. Every report that
// prints a link comes through here, so that rule has exactly one home.
//
// io.Writer and not *os.File: every caller passes a command's out stream, and
// nothing here needs a file. The concrete type only narrowed who could call
// this — link_test.go already drives it through the command harness, but a
// direct assertion against a buffer should not have to open one.
func printResourceURL(out io.Writer, keyWidth int, url string) {
	if url == "" {
		return
	}
	fmt.Fprintf(out, "  %-*s%s\n", keyWidth, "URL:", url)
}

// plural pluralises the handful of binding nouns above. "codex" is the reason
// this is not a bare +"s".
func plural(n int, singular string) string {
	if n == 1 {
		return singular
	}
	if strings.HasSuffix(singular, "x") {
		return singular + "es"
	}
	return singular + "s"
}

// baselineFrom builds the state entry recorded after a successful sync with one
// workflow row.
//
// Title and Entrypoint are recorded from the ROW, never from the manifest: the
// baseline's job is to say what the server held at the last sync, and the drift
// guard compares the server against it. Recording what we WANTED it to be would
// make the guard agree with itself forever.
// baselineParameters records the row's declaration for the drift guard, always
// non-nil. "The server declared none" is a fact worth recording: a nil here
// means UNRECORDED, which disarms the guard, and that is only ever right for a
// state file written before parameters were part of the baseline.
func baselineParameters(source *api.Workflow) *[]api.WorkflowParameter {
	params := append([]api.WorkflowParameter{}, source.Parameters...)
	return &params
}

func baselineFrom(source *api.Workflow, files []api.WorkflowFile) *wfdir.InstanceState {
	inst := &wfdir.InstanceState{
		SourceID:          source.ID,
		SourceLifecycle:   source.Lifecycle,
		BaselineUpdatedAt: source.UpdatedAt.UTC().Format(time.RFC3339),
		Title:             source.Title,
		Entrypoint:        source.Entrypoint,
		Parameters:        baselineParameters(source),
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

// baselineFromLocal is baselineFrom for content the CLI just WROTE rather than
// read back: after a push, the server holds exactly the bytes we sent.
//
// Re-fetching every file to build the baseline instead would be a second full
// round trip to learn something we already know. Per-file UpdatedAt is left
// empty — the field is diagnostics only (drift is decided on the hash), and
// inventing timestamps we did not receive would be worse than saying nothing.
func baselineFromLocal(source *api.Workflow, files map[string]string) *wfdir.InstanceState {
	inst := &wfdir.InstanceState{
		SourceID:          source.ID,
		SourceLifecycle:   source.Lifecycle,
		BaselineUpdatedAt: source.UpdatedAt.UTC().Format(time.RFC3339),
		Title:             source.Title,
		Entrypoint:        source.Entrypoint,
		Parameters:        baselineParameters(source),
		Files:             map[string]wfdir.FileState{},
	}
	for path, content := range files {
		inst.Files[path] = wfdir.FileState{SHA256: wfdir.HashString(content)}
	}
	return inst
}
