package commands

import (
	"context"
	"errors"
	"fmt"
	"maps"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/ronjatech/ronja-cli/internal/api"
	"github.com/ronjatech/ronja-cli/internal/config"
	"github.com/ronjatech/ronja-cli/internal/wfdir"
	"github.com/spf13/cobra"
)

// `ronja sync adopt` and `ronja sync unmanage` move a tree between the two
// patterns:
//
//   - adopt goes classic → serious: it mints a folder id where one is missing
//     (writing ronja.json), claims every row the lock records for the stack's
//     feature (POST /managed/claim, one outcome per row), and records the
//     lock's anchors on the server — so a CI runner that never sees the lock
//     still has a drift baseline. It is local, and the only command besides
//     `init` that ever mints an id.
//   - unmanage goes serious → classic: it releases the folder's claims in the
//     stack's feature (DELETE /managed/:id, the rows untouched) and removes the
//     folder id — unless another stack of the folder still holds claims, which
//     would be stranded under an id nothing carries any more.
//
// Both are per-folder in outcome: a folder that refuses never stops the rest,
// and both are idempotent — running adopt twice claims nothing the second time
// (already_claimed), and unmanage on a folder with no claims left only removes
// the id.

// syncManagedReport is the --json shape of both commands.
type syncManagedReport struct {
	Dir     string                    `json:"dir"`
	URL     string                    `json:"url"`
	Stack   string                    `json:"stack"`
	Verdict string                    `json:"verdict"`
	Folders []syncManagedFolderReport `json:"folders"`
}

type syncManagedFolderReport struct {
	Path     string `json:"path"`
	Root     string `json:"root"`
	Kind     string `json:"kind,omitempty"`
	FolderID string `json:"folderID,omitempty"`
	// Minted reports that adopt wrote this folder's id — a change to ronja.json
	// that has to be committed.
	Minted bool `json:"minted,omitempty"`
	// IDRemoved reports that unmanage took this folder's id out of ronja.json.
	IDRemoved bool `json:"idRemoved,omitempty"`
	// StackUnmanaged reports that unmanage marked the stack "unmanaged" in
	// ronja.json because the id had to stay; Readopted that adopt cleared it.
	// Either is a change to commit.
	StackUnmanaged bool                  `json:"stackUnmanaged,omitempty"`
	Readopted      bool                  `json:"readopted,omitempty"`
	Verdict        string                `json:"verdict"`
	Reason         string                `json:"reason,omitempty"`
	Detail         string                `json:"detail,omitempty"`
	Units          []syncManagedUnitView `json:"units"`
	Notes          []string              `json:"notes,omitempty"`
}

// syncManagedUnitView is one address's outcome: claimed, already_claimed or
// refused (with the server's code) for adopt; unmanaged for unmanage.
type syncManagedUnitView struct {
	Address string `json:"address,omitempty"`
	Kind    string `json:"kind"`
	ID      string `json:"id,omitempty"`
	Outcome string `json:"outcome"`
	Code    string `json:"code,omitempty"`
	Detail  string `json:"detail,omitempty"`
}

const syncManagedOutcomeUnmanaged = "unmanaged"

func newSyncAdoptCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "adopt [dir]",
		Short: "Put every folder beneath a directory under server-side state",
		Long: `Put every folder beneath a directory under server-side state, for one stack.
Run it locally, once per stack, and commit ronja.json:

  ronja sync adopt --stack prod

For each folder that declares --stack, adopt:

  1. gives it a folder id ("id" in ronja.json) if it has none — the one
     change to commit;
  2. claims every row the stack's ronja.lock.json records, in the stack's
     feature, so the server knows which file of which folder manages which
     row;
  3. records each row's drift baseline on the server.

From then on the server holds the folder's state: a CI runner that throws its
checkout away no longer needs a committed ronja.lock.json, two concurrent
applies cannot both create the same row, and a lock that disagrees with the
server is rewritten from it.

Running it again is safe: a row already claimed reports already_claimed. A folder that refuses
(a row already managed somewhere else, a row in the trash) is reported and does
not stop the others.

The directory is the positional argument, or --dir, or the working directory.`,
		Args: cobra.MaximumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			return runSyncManagedCommand(cmd.Context(), args, "adopt", syncAdoptFolder)
		},
	}
}

func newSyncUnmanageCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "unmanage [dir]",
		Short: "Take every folder beneath a directory out of server-side state",
		Long: `Take every folder beneath a directory out of server-side state, for one stack.

For each adopted folder that declares --stack, unmanage releases its claims in
the stack's feature — the rows themselves are left exactly as they are, not
edited and not deleted — and removes the folder id from ronja.json, which
returns the folder to running off ronja.lock.json alone.

The id stays when another stack of the folder still holds claims under it:
removing it would strand those claims under an id nothing carries, and a later
adopt would be refused for every row they hold. Unmanage that stack too.

Anyone who may write the stack's feature may release a claim, including one on
a row they cannot read — which is how a claim stuck on somebody else's
unpublished draft, or on a row that no longer exists, is cleared.

Before releasing anything it writes every row the claims name into
ronja.lock.json, so a plain push afterwards updates those rows. A row you may
not read has no id to write: unmanage refuses, naming each such file and who
claimed it, because a push of that file afterwards would create a second row.
Have that person publish it (or run unmanage themselves), or pass --force to
release it anyway.

The directory is the positional argument, or --dir, or the working directory.`,
		Args: cobra.MaximumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			return runSyncManagedCommand(cmd.Context(), args, "unmanage", syncUnmanageFolder)
		},
	}
	cmd.Flags().BoolVar(&flagUnmanageForce, "force", false,
		"release claims on rows this credential may not read, which ronja.lock.json then does not record")
	return cmd
}

// flagUnmanageForce is `sync unmanage --force`: release a claim whose row this
// credential may not read, and which the lock therefore cannot record — see
// unreadableUnrecorded.
var flagUnmanageForce bool

// syncManagedFolderFunc does one command's work on one folder whose stack has
// already been resolved.
type syncManagedFolderFunc func(ctx context.Context, folder discoveredFolder, resolved *config.Resolved, line *syncManagedFolderReport)

func runSyncManagedCommand(ctx context.Context, args []string, verb string, each syncManagedFolderFunc) error {
	if flagStack == "" {
		return fmt.Errorf("`ronja sync %s` needs --stack <name>: a folder's server state is per stack (its feature)", verb)
	}
	if err := wfdir.ValidStackName(flagStack); err != nil {
		return err
	}
	dir := flagSyncDir
	if len(args) == 1 {
		dir = args[0]
	}
	resolved, err := resolveInstance()
	if err != nil {
		return withExitCode(syncExitUnknown, err)
	}
	if resolved.TenantID == "" {
		if err := ensureTenant(ctx, resolved); err != nil {
			return withExitCode(syncExitUnknown, err)
		}
	}
	found, err := discoverFolders(dir)
	if err != nil {
		return err
	}
	base, err := filepath.Abs(dir)
	if err != nil {
		return err
	}
	ctx = withAddressWalkRoot(ctx, base)
	key := wfdir.InstanceKey{URL: resolved.URL, TenantID: resolved.TenantID}
	report := &syncManagedReport{Dir: base, URL: resolved.URL, Stack: flagStack, Folders: []syncManagedFolderReport{}}
	verdicts := []string{}
	for _, folder := range found {
		line := syncManagedFolderReport{Path: folder.Rel, Root: folder.Root, Kind: folder.Kind.Name, Units: []syncManagedUnitView{}}
		switch {
		case ctx.Err() != nil:
			line.Verdict, line.Reason, line.Detail = verdictUnknown, syncReasonInterrupted, "not attempted — the run was interrupted"
		case folder.notChecked():
			line.Verdict, line.Reason, line.Detail = verdictUnknown, folder.Reason, folder.Detail
		default:
			lock, lockErr := wfdir.LoadLock(folder.Root)
			if lockErr != nil {
				line.Verdict, line.Reason, line.Detail = verdictUnknown, syncReasonUnreadable, lockErr.Error()
				break
			}
			if reason, detail := resolveStackForFolder(folder.Manifest, lock, key, flagStack); reason != "" {
				line.Verdict, line.Reason, line.Detail = verdictUnknown, reason, detail
				break
			}
			if _, declared := folder.Manifest.Stacks[flagStack]; !declared {
				line.Verdict, line.Reason, line.Detail = verdictUnknown, syncReasonStackAbsent,
					fmt.Sprintf("this folder does not declare stack %q in %s", flagStack, wfdir.ManifestName)
				break
			}
			each(ctx, folder, resolved, &line)
		}
		report.Folders = append(report.Folders, line)
		verdicts = append(verdicts, line.Verdict)
	}
	report.Verdict = worstApplyVerdict(verdicts)
	if len(found) == 0 {
		report.Verdict = verdictUnknown
	}
	if flagJSON {
		if err := emitJSON(report); err != nil {
			return err
		}
	} else {
		printSyncManaged(report, verb)
	}
	return syncManagedExit(report, verb)
}

// probeManagedState reads one folder's state in a stack's feature, to learn
// before writing a folder id that the instance serves /managed, that this
// credential may read it, and that the feature is one it can use — the
// refusals describeManagedReadError names, without a folder to name them for.
func probeManagedState(ctx context.Context, resolved *config.Resolved, stack, featureID, folderID string) error {
	if resolved.Token == "" {
		return fmt.Errorf("an adopted folder keeps its state on %s, and this credential has no token to read it with. Sign in (`ronja login`) or set RONJA_TOKEN", resolved.URL)
	}
	_, err := newClient(resolved.URL, resolved.Token).ReadManaged(ctx, featureID, folderID)
	switch {
	case err == nil:
		return nil
	case noManagedRoute(err):
		return fmt.Errorf("%s does not serve /api/v2/managed (an instance older than server-side state), so it cannot keep this folder's records — adopt it against an instance that does", resolved.URL)
	case api.WireCodeOf(err) == api.InsufficientScopeCode:
		return fmt.Errorf("an adopted folder keeps its state on the server, and this token may not read it: adopting needs the structure:read and structure:write scopes, beside each kind's own. %w", err)
	}
	return fmt.Errorf("read the state of stack %q (feature %s) from %s: %w", stack, featureID, resolved.URL, err)
}

// syncAdoptFolder is adopt's work on one folder.
func syncAdoptFolder(ctx context.Context, folder discoveredFolder, resolved *config.Resolved, line *syncManagedFolderReport) {
	refuse := func(reason, format string, args ...any) {
		line.Verdict, line.Reason, line.Detail = verdictRefused, reason, fmt.Sprintf(format, args...)
	}
	// 0. Can the server hold this folder's state at all? Asked BEFORE
	// anything is written: an id in ronja.json makes every later deploy of the
	// folder read the server, so writing it for a stack with no feature, or
	// against an instance without /managed, or with a token that may not read
	// it, would turn a working classic folder into one that refuses every
	// deploy. The probe reads the id this run would write (a new one has no
	// claims, so it costs one empty answer).
	stack := folder.Manifest.Stacks[flagStack]
	if stack.FeatureID == "" {
		line.Verdict, line.Reason, line.Detail = verdictUnknown, syncReasonNoFeature,
			fmt.Sprintf("stack %q names no featureID in %s, and a claim is keyed by the stack's feature — add it, then adopt again. Nothing was written", flagStack, wfdir.ManifestName)
		return
	}
	id := folder.Manifest.ID
	if !folder.Manifest.Adopted() {
		minted, err := wfdir.NewFolderID()
		if err != nil {
			refuse(syncReasonUnreadable, "%v", err)
			return
		}
		id = minted
	}
	if err := probeManagedState(ctx, resolved, flagStack, stack.FeatureID, id); err != nil {
		line.Verdict, line.Reason, line.Detail = verdictUnknown, syncReasonLookupFailed, err.Error()+". Nothing was written"
		return
	}

	// 1. The id, minted above — the one write to ronja.json. A stack an
	// earlier `sync unmanage` released while the id stayed is named again here,
	// which is the other write.
	if !folder.Manifest.Adopted() || stack.Unmanaged {
		if !folder.Manifest.Adopted() {
			folder.Manifest.ID = id
			line.Minted = true
		}
		if stack.Unmanaged {
			stack.Unmanaged = false
			folder.Manifest.Stacks[flagStack] = stack
			line.Readopted = true
		}
		if err := wfdir.SaveManifest(folder.Root, folder.Manifest); err != nil {
			refuse(syncReasonUnreadable, "write the folder id into %s: %v", wfdir.ManifestPath(folder.Root), err)
			return
		}
	}
	line.FolderID = folder.Manifest.ID

	// 2. Read the server's state (the refresh every adopted folder makes).
	f, err := openFolderForStatusAt(ctx, folder.Root, resolved, folder.Kind)
	if err != nil {
		if r, ok := asManagedRefusal(err); ok {
			refuse(r.Reason, "%s", r.Detail)
			return
		}
		line.Verdict, line.Reason, line.Detail = verdictUnknown, syncReasonFolderRefused, err.Error()
		return
	}
	if f.OrgErr != nil {
		line.Verdict, line.Reason, line.Detail = verdictUnknown, syncReasonLookupFailed, f.OrgErr.Error()
		return
	}
	if f.managed == nil {
		line.Verdict, line.Reason, line.Detail = verdictUnknown, syncReasonNoFeature,
			fmt.Sprintf("stack %q names no featureID in %s, and a claim is keyed by the stack's feature — add it, then adopt again", flagStack, wfdir.ManifestName)
		return
	}
	st := f.managed
	if len(st.rewrites) > 0 {
		line.Notes = append(line.Notes, f.managedRewriteNotes()...)
		if err := wfdir.SaveLock(f.Root, f.Lock); err != nil {
			refuse(syncReasonUnreadable, "rewrite %s from the server's state: %v", wfdir.LockPath(f.Root), err)
			return
		}
	}
	for _, addr := range sortedClaimAddresses(st.claims) {
		c := st.claims[addr]
		line.Units = append(line.Units, syncManagedUnitView{Address: addr, Kind: c.ResourceKind, ID: c.ResourceID, Outcome: api.ManagedOutcomeAlreadyClaimed})
	}

	// 3. Claim every row the lock records and the server has no claim for —
	// each item on its own, reported on its own.
	client := newClient(f.Resolved.URL, f.Resolved.Token)
	pending := f.managedBackfill()
	refused := 0
	for start := 0; start < len(pending); start += api.MaxManagedClaimItems {
		batch := pending[start:min(start+api.MaxManagedClaimItems, len(pending))]
		items := make([]api.ManagedClaimItem, 0, len(batch))
		for _, a := range batch {
			items = append(items, api.ManagedClaimItem{ResourceKind: a.Kind, ResourceID: a.ID, ManagedClaimInput: *f.managedClaim(a.Address)})
		}
		results, err := client.ClaimManaged(ctx, st.featureID, items)
		if err != nil {
			if api.Unanswered(err) || api.IsTimeout(err) {
				line.Verdict, line.Reason, line.Detail = verdictUnknown, syncReasonUnanswered, err.Error()
				return
			}
			refuse(syncReasonClaimRefused, "claim this folder's rows in feature %s: %v", st.featureID, err)
			return
		}
		for _, r := range results {
			if r.Index < 0 || r.Index >= len(batch) {
				continue
			}
			a := batch[r.Index]
			unit := syncManagedUnitView{Address: a.Address, Kind: a.Kind, ID: a.ID, Outcome: r.Outcome, Code: r.Code}
			switch r.Outcome {
			case api.ManagedOutcomeClaimed, api.ManagedOutcomeAlreadyClaimed:
				if r.Claim != nil {
					st.claims[a.Address] = r.Claim
				}
			default:
				refused++
				unit.Detail = describeClaimRefusal(f, a, r).Detail
				if st.unclaimable == nil {
					st.unclaimable = map[string]bool{}
				}
				st.unclaimable[a.Address] = true
			}
			line.Units = append(line.Units, unit)
		}
	}
	sort.SliceStable(line.Units, func(i, j int) bool { return line.Units[i].Address < line.Units[j].Address })

	// 4. The anchors the lock carries, onto the claims — so a runner that never
	// sees the lock still has a drift baseline from the first apply.
	f.ctx = ctx
	if err := f.writeManagedAnchors(ctx); err != nil {
		var r *managedRefusal
		if errors.As(err, &r) {
			refuse(r.Reason, "%s", r.Detail)
		} else {
			refuse(syncReasonAnchorNotRecorded, "the rows were claimed, and their drift baselines could not be recorded: %v", err)
		}
		return
	}
	if refused > 0 {
		refuse(syncReasonClaimRefused, "%d of %d %s could not be claimed — see each one; the rest were", refused, len(line.Units), plural(len(line.Units), "row"))
		return
	}
	line.Verdict = verdictApplied
	line.Detail = fmt.Sprintf("%d %s managed from this folder", len(line.Units), plural(len(line.Units), "row"))
}

// canonicalLockStack is one stack's managed rows and anchors in the lock, as
// one comparable string.
func canonicalLockStack(lock *wfdir.Lock, stack string, kind wfdir.Kind) string {
	var b strings.Builder
	for _, a := range lock.ManagedAddresses(stack, kind) {
		fmt.Fprintf(&b, "%s\x00%s\x00%s\n", a.Address, a.ID, canonicalJSON(lock.Anchor(stack, kind, a.Address)))
	}
	b.WriteString(canonicalJSON(lock.FolderAnchor(stack)))
	return b.String()
}

func sortedClaimAddresses(claims map[string]*api.ManagedClaim) []string {
	out := make([]string, 0, len(claims))
	for addr := range claims {
		out = append(out, addr)
	}
	sort.Strings(out)
	return out
}

// syncUnmanageFolder is unmanage's work on one folder.
//
// It reads the claims directly rather than through the refresh: the refresh
// REFUSES a lost, trashed, unreadable or moved-out claim, and releasing exactly
// those is what unmanage is for.
func syncUnmanageFolder(ctx context.Context, folder discoveredFolder, resolved *config.Resolved, line *syncManagedFolderReport) {
	refuse := func(reason, format string, args ...any) {
		line.Verdict, line.Reason, line.Detail = verdictRefused, reason, fmt.Sprintf(format, args...)
	}
	m := folder.Manifest
	if !m.Adopted() {
		line.Verdict, line.Reason, line.Detail = verdictApplied, syncReasonNotAdopted, "this folder has no folder id — it already runs off ronja.lock.json alone"
		return
	}
	line.FolderID = m.ID
	stack := m.Stacks[flagStack]
	if stack.FeatureID == "" {
		line.Verdict, line.Reason, line.Detail = verdictUnknown, syncReasonNoFeature,
			fmt.Sprintf("stack %q names no featureID in %s, so it has no claims to release", flagStack, wfdir.ManifestName)
		return
	}
	client := newClient(resolved.URL, resolved.Token)
	var claims []api.ManagedClaim
	var folderAnchor *api.ManagedFolderAnchor
	state, err := client.ReadManaged(ctx, stack.FeatureID, m.ID)
	switch {
	case err != nil && noManagedRoute(err):
		// An instance that does not serve /managed holds no server state at
		// all, so there is nothing to release and nothing the id could strand
		// there — and this is the remedy the refusal on such an instance names.
		line.Notes = append(line.Notes, fmt.Sprintf("%s does not serve /api/v2/managed, so it holds no claims for this folder — only the folder id is removed", resolved.URL))
	case err == nil:
		claims, folderAnchor = state.Claims, state.Folder
	default:
		line.Verdict, line.Reason, line.Detail = verdictUnknown, syncReasonLookupFailed,
			fmt.Sprintf("read this folder's claims in feature %s: %v", stack.FeatureID, err)
		return
	}
	lock, err := wfdir.LoadLock(folder.Root)
	if err != nil {
		line.Verdict, line.Reason, line.Detail = verdictUnknown, syncReasonUnreadable, err.Error()
		return
	}
	kind, known := wfdir.KindByName(m.Kind)
	if !known {
		line.Verdict, line.Reason, line.Detail = verdictUnknown, syncReasonUnreadable, fmt.Sprintf("%s names kind %q, which this CLI does not know", wfdir.ManifestName, m.Kind)
		return
	}
	// A row this credential may NOT read names no id, so the lock cannot be
	// given it — and a classic push of its file after the release would create
	// a second row. Refused, naming each, unless --force: the remedy is with
	// whoever can read it. A lock that already records the address has the row.
	if hidden := unreadableUnrecorded(claims, lock, flagStack, kind); len(hidden) > 0 {
		var names []string
		for _, c := range hidden {
			names = append(names, fmt.Sprintf("%s (claimed by %s)", orFolder(c.Address), orUnknown(c.Claimant)))
		}
		consequence := fmt.Sprintf("%s cannot record %s, so a classic push of %s after the release would create a second row",
			wfdir.LockName, agree(len(hidden), "its row", "their rows"), agree(len(hidden), "that file", "those files"))
		if !flagUnmanageForce {
			refuse(syncReasonNotReadable, "%d %s %s managed by a row this credential may not read — typically the claimant's unpublished draft: %s. %s. Have the claimant publish it, so that unmanage can record it, or unmanage with a credential that can read it; or pass --force to release it anyway. Nothing was released",
				len(hidden), agree(len(hidden), "address", "addresses"), agree(len(hidden), "is", "are"), strings.Join(names, ", "), consequence)
			return
		}
		line.Notes = append(line.Notes, fmt.Sprintf("--force — released claims on rows this credential may not read: %s. %s until the row's id is recorded there by hand",
			strings.Join(names, ", "), consequence))
	}
	// The lock is the classic pattern's ONLY state, and a row a lock-less run
	// created is known to the server alone. So every ok claim's row — and its
	// anchor — is written into the lock BEFORE anything is released: released
	// first, a crash in between would leave a row nothing records, and the next
	// classic push would create a second one. Saved now, for the same reason.
	// A tombstone whose row is back in this feature (revivalMissed) is this
	// stack's row too: it is recorded, so `sync adopt` claims it again.
	before := canonicalLockStack(lock, flagStack, kind)
	heldAt := map[string]string{}
	legacyDocs := map[string]wfdir.LockTableDocs{}
	for _, c := range claims {
		if (c.State != api.ManagedStateOK && !revivalMissed(&c)) || c.ResourceID == "" || wfdir.ManagedKindAt(kind, c.Address) != c.ResourceKind {
			continue
		}
		var anchor map[string]any
		if len(c.Applied) > 0 {
			anchor = c.Applied
		}
		if err := lock.SetManaged(flagStack, kind, c.Address, c.ResourceID, anchor); err != nil {
			refuse(syncReasonUnreadable, "record %s in %s before releasing it: %v", orFolder(c.Address), wfdir.LockName, err)
			return
		}
		heldAt[c.ResourceKind+"\x00"+c.ResourceID] = c.Address
		if docs, err := wfdir.TableDocsIn(c.Applied); err == nil {
			maps.Copy(legacyDocs, docs)
		}
	}
	// The FOLDER anchor's records too — the docs-sidecar baselines, which no
	// claim carries — or, while it holds none, what an older claim anchor kept.
	if folderAnchor != nil {
		docs, err := wfdir.TableDocsIn(folderAnchor.Applied)
		if err != nil {
			refuse(syncReasonUnreadable, "read this folder's anchor before releasing it: %v", err)
			return
		}
		if len(docs) == 0 {
			docs = legacyDocs
		}
		lock.MergeTableDocs(flagStack, docs)
	}
	for _, a := range lock.ManagedAddresses(flagStack, kind) {
		if held, ok := heldAt[a.Kind+"\x00"+a.ID]; ok && held != a.Address {
			lock.DropRelocated(flagStack, kind, a.Address, held)
		}
	}
	if canonicalLockStack(lock, flagStack, kind) != before {
		if err := wfdir.SaveLock(folder.Root, lock); err != nil {
			refuse(syncReasonUnreadable, "record the server's rows in %s before releasing them: %v", wfdir.LockPath(folder.Root), err)
			return
		}
		line.Notes = append(line.Notes, fmt.Sprintf("%s now records every row the server's claims named, so the classic pattern updates them rather than creating new ones — commit it", wfdir.LockName))
	}
	// The stack is marked "unmanaged" BEFORE anything is released, and saved.
	// Released first, a crash before the mark would leave the id with no mark,
	// and the stack's next apply would read the folder as adopted and lazily
	// claim every row again. Marked first, the stack runs from the lock written
	// above whatever happens next, and a second unmanage finishes the release.
	// The mark is cleared below with the id when no other stack needs it.
	if !stack.Unmanaged {
		stack.Unmanaged = true
		m.Stacks[flagStack] = stack
		if err := wfdir.SaveManifest(folder.Root, m); err != nil {
			refuse(syncReasonUnreadable, "mark stack %q unmanaged in %s before releasing its claims: %v", flagStack, wfdir.ManifestPath(folder.Root), err)
			return
		}
	}
	lockChanged := false
	for _, c := range claims {
		if _, err := client.UnmanageManaged(ctx, c.ID); err != nil && api.StatusOf(err) != http.StatusNotFound {
			refuse(syncReasonClaimRefused, "release the claim on %s: %v", orFolder(c.Address), err)
			return
		}
		line.Units = append(line.Units, syncManagedUnitView{Address: c.Address, Kind: c.ResourceKind, ID: c.ResourceID, Outcome: syncManagedOutcomeUnmanaged,
			Detail: "claim released; the row is left exactly as it is"})
		// A LOST row is gone for good (ids are never reused), and a TOMBSTONE's
		// row was moved to another feature on purpose, so the lock's record of
		// either is not this stack's row any more: dropped, so a later adopt does
		// not try to claim it, a classic push does not write into another
		// feature, and an apply with --allow-create can create the file's row
		// again. A tombstone whose row this credential may not read names no id;
		// the lock's entry at its address is then the row it held. Every other
		// row keeps its lock entry — the classic loop runs off it.
		if c.State == api.ManagedStateLost || (c.Departed() && !revivalMissed(&c)) {
			if recorded := lock.ManagedID(flagStack, kind, c.Address); recorded != "" && (recorded == c.ResourceID || (c.Departed() && c.ResourceID == "")) {
				lock.DropManaged(flagStack, kind, c.Address)
				lockChanged = true
			}
		}
	}
	if lockChanged {
		if err := wfdir.SaveLock(folder.Root, lock); err != nil {
			refuse(syncReasonUnreadable, "drop the lost rows from %s: %v", wfdir.LockPath(folder.Root), err)
			return
		}
	}
	// The folder anchor goes last of the server's state: its records are in the
	// lock already. Asked even when the read saw none, so an anchor a concurrent
	// apply created since is not left behind; none is a 404, and fine.
	if state != nil {
		if _, err := client.ReleaseManagedFolder(ctx, stack.FeatureID, m.ID); err != nil && api.WireCodeOf(err) != api.ManagedCodeNoFolderAnchor && !noManagedRoute(err) {
			refuse(syncReasonClaimRefused, "release this folder's anchor (its docs-sidecar baselines, already written into %s): %v", wfdir.LockName, err)
			return
		}
	}

	// The id goes only when no OTHER stack of this folder still holds claims
	// under it. A stack on the same feature shares these claims (an address is
	// keyed by the feature, not the stack), so only other features are asked —
	// and one this credential cannot ask (another instance or organization)
	// keeps the id, because removing it on a guess strands real claims.
	var holding, unverified []string
	for _, name := range m.StackNames() {
		other := m.Stacks[name]
		if name == flagStack || other.FeatureID == "" || other.FeatureID == stack.FeatureID {
			continue
		}
		if !wfdir.Matches(other.Key(), wfdir.InstanceKey{URL: resolved.URL, TenantID: resolved.TenantID}) {
			unverified = append(unverified, name)
			continue
		}
		rest, err := client.ReadManaged(ctx, other.FeatureID, m.ID)
		switch {
		case err != nil && noManagedRoute(err):
			// No server state on this instance: nothing of that stack's to strand.
		case err != nil:
			unverified = append(unverified, name)
		case len(rest.Claims) > 0:
			holding = append(holding, fmt.Sprintf("%s (%d)", name, len(rest.Claims)))
		case rest.Folder.Revision > 0:
			// A folder anchor alone (a docs-only folder) is state under the id
			// too: its docs-sidecar baselines.
			holding = append(holding, fmt.Sprintf("%s (a folder anchor)", name))
		}
	}
	if len(holding)+len(unverified) > 0 {
		var why []string
		if len(holding) > 0 {
			why = append(why, "still holding claims or a folder anchor: "+strings.Join(holding, ", "))
		}
		if len(unverified) > 0 {
			why = append(why, "not checkable with this credential: "+strings.Join(unverified, ", "))
		}
		// The stack STAYS marked "unmanaged" (saved above): with the id still
		// there, its next apply would otherwise read the folder as adopted and
		// lazily claim every row again.
		line.StackUnmanaged = true
		line.Notes = append(line.Notes, fmt.Sprintf("the folder id stays in %s — other stacks of this folder are %s; removing the id would strand their claims. Stack %q is marked \"unmanaged\" there instead, so it runs from %s alone until `ronja sync adopt --stack %s`. Run `ronja sync unmanage --stack <name>` for each other stack to return the whole folder to the classic pattern",
			wfdir.ManifestName, strings.Join(why, "; "), flagStack, wfdir.LockName, flagStack))
	} else {
		m.RemoveFolderID()
		// Without an id every stack runs legacy; an "unmanaged" mark would only
		// be noise in the committed file.
		for name, other := range m.Stacks {
			if other.Unmanaged {
				other.Unmanaged = false
				m.Stacks[name] = other
			}
		}
		if err := wfdir.SaveManifest(folder.Root, m); err != nil {
			refuse(syncReasonUnreadable, "remove the folder id from %s: %v", wfdir.ManifestPath(folder.Root), err)
			return
		}
		line.IDRemoved = true
	}
	line.Verdict = verdictApplied
	line.Detail = fmt.Sprintf("released %d %s; the rows are untouched", len(line.Units), plural(len(line.Units), "claim"))
}

// unreadableUnrecorded is every claim whose row this credential may not read
// (so the claim names no id) at an address the lock records no row for — the
// rows unmanage cannot hand to the classic pattern. A tombstone is not one: its
// row was moved away on purpose and its lock entry is dropped anyway.
func unreadableUnrecorded(claims []api.ManagedClaim, lock *wfdir.Lock, stack string, kind wfdir.Kind) []api.ManagedClaim {
	var out []api.ManagedClaim
	for _, c := range claims {
		if c.State == api.ManagedStateNotReadable && c.ResourceID == "" && lock.ManagedID(stack, kind, c.Address) == "" {
			out = append(out, c)
		}
	}
	return out
}

// syncManagedExit maps the tree verdict onto the sync exit codes.
func syncManagedExit(r *syncManagedReport, verb string) error {
	switch r.Verdict {
	case verdictRefused:
		return withExitCode(syncExitDrifted, fmt.Errorf("`ronja sync %s` refused for at least one folder — see above", verb))
	case verdictUnknown:
		if len(r.Folders) == 0 {
			return withExitCode(syncExitUnknown, fmt.Errorf("no %s found under %s", wfdir.ManifestName, r.Dir))
		}
		return withExitCode(syncExitUnknown, fmt.Errorf("at least one folder could not be %s — see above", map[string]string{"adopt": "adopted", "unmanage": "unmanaged"}[verb]))
	}
	return nil
}

func printSyncManaged(r *syncManagedReport, verb string) {
	out := os.Stdout
	fmt.Fprintf(out, "ronja sync %s — stack %q on %s, under %s\n\n", verb, r.Stack, r.URL, r.Dir)
	changed := false
	for _, line := range r.Folders {
		head := line.Verdict
		if line.Reason != "" {
			head += " (" + line.Reason + ")"
		}
		fmt.Fprintf(out, "  %-10s %s\n", head, line.Path)
		if line.FolderID != "" {
			suffix := ""
			if line.Minted {
				suffix = " — new, written to " + wfdir.ManifestName
			}
			if line.IDRemoved {
				suffix = " — removed from " + wfdir.ManifestName
			}
			if line.StackUnmanaged {
				suffix = fmt.Sprintf(" — kept; stack %q marked \"unmanaged\" in %s", r.Stack, wfdir.ManifestName)
			}
			if line.Readopted {
				suffix += fmt.Sprintf(" — stack %q managed again", r.Stack)
			}
			fmt.Fprintf(out, "             folder id %s%s\n", line.FolderID, suffix)
		}
		if line.Minted || line.IDRemoved || line.StackUnmanaged || line.Readopted {
			changed = true
		}
		for _, u := range line.Units {
			addr := u.Address
			if addr == "" {
				addr = "(the " + u.Kind + ")"
			}
			fmt.Fprintf(out, "             %-16s %s %s", u.Outcome, addr, u.ID)
			if u.Code != "" {
				fmt.Fprintf(out, " [%s]", u.Code)
			}
			fmt.Fprintln(out)
			if u.Detail != "" && u.Outcome == api.ManagedOutcomeRefused {
				fmt.Fprintf(out, "                %s\n", u.Detail)
			}
		}
		if line.Detail != "" {
			fmt.Fprintf(out, "             %s\n", line.Detail)
		}
		for _, n := range line.Notes {
			fmt.Fprintf(out, "             Note: %s\n", n)
		}
	}
	if changed {
		fmt.Fprintf(out, "\nCommit %s: the folder id, and which stacks it manages, is what tells every runner where this folder's state lives.\n", wfdir.ManifestName)
	}
}
