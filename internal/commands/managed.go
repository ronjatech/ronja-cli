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

	"github.com/ronjatech/ronja-cli/internal/api"
	"github.com/ronjatech/ronja-cli/internal/wfdir"
)

// SERVER-SIDE STATE for an ADOPTED folder — one whose ronja.json carries an
// "id".
//
// The server keeps which repository address manages which row
// (`managed_resources`, api/v2/managed): (the stack's feature, this folder's
// id, the folder-relative file) → row id, plus the per-address ANCHOR a drift
// guard compares against. For an adopted folder that state WINS and the lock is
// a cache of it: before any loop runs, the folder reads its claims and
// rewrites its in-memory lock to say what the server says (refreshManaged).
// Every loop then runs exactly as it does today, because each already reads the
// lock and f.Binding — which is the point: one translation at the door, not a
// second code path in every loop.
//
// The four things that WRITE server state, and where:
//
//   - a CREATE carries `managed` (managedClaim) and claims its address in the
//     create's own transaction. Its 409 address_claimed adopts the winner
//     by id when that claim is ok (adoptOnAddressClaimed).
//   - an ACTING open settles what the refresh found pending (applyManaged):
//     `moved` re-keys (POST /managed/move), `removed` unmanages (DELETE),
//     --adopt-foreign relabels, and a row the lock records with no claim is
//     lazily claimed (POST /managed/claim).
//   - every LOCK SAVE records the anchors that changed (recordManagedAnchors):
//     the FOLDER's anchor first (PUT /managed/folder/applied, a CAS on its own
//     revision — the docs-sidecar and health-checks baselines, which belong to
//     no claim), then
//     each claim's (PUT /managed/:id/applied, a CAS on the claim's revision).
//   - `ronja sync adopt` and `ronja sync unmanage` (sync_adopt.go).
//
// A READ-ONLY path — status, `sync status`, `sync check`, `sync apply
// --dry-run` — refreshes in memory and writes nothing anywhere, server or disk.
//
// A folder with no id (LEGACY, the classic pattern) never reaches any of it: f.managed stays
// nil, and every function here is a no-op on a nil state.

// flagAdoptForeign lets an apply take over claims a DIFFERENT repository made
// in the same feature (a fork or template copy that kept the folder ids) — see
// foreignClaim. Registered beside --stack, on every folder group and on sync.
var flagAdoptForeign bool

// Reasons this file adds to the tree commands' vocabulary. Machine
// contract, like every other sync reason: add, never rename.
const (
	// syncReasonNotReadable — the server's claim names a row this credential
	// may not read: typically another person's unpublished draft.
	syncReasonNotReadable = "not_readable"
	// syncReasonTrashed — the claimed row is in the trash.
	syncReasonTrashed = "trashed"
	// syncReasonLost — the claimed row no longer exists.
	syncReasonLost = "lost"
	// syncReasonMovedOut — the claimed row now lives in another feature. Almost
	// always the claim is then a TOMBSTONE: it keeps its address, so nothing is
	// created there until it is unmanaged, and it is revived if the row moves
	// back.
	syncReasonMovedOut = "moved_out"
	// syncReasonClaimedElsewhere — the row this folder's lock records is
	// already managed at another address, so the lazy claim was refused.
	syncReasonClaimedElsewhere = "claimed_elsewhere"
	// syncReasonForeignClaim — the address was claimed by another repository
	// (a fork or template copy) targeting the same feature.
	syncReasonForeignClaim = "foreign_claim"
	// syncReasonClaimMismatch — a claim under this folder's id that this
	// folder's kind cannot hold at its address: two folders share an id.
	syncReasonClaimMismatch = "claim_mismatch"
	// syncReasonClaimRefused — the server refused a claim for a reason none of
	// the above names (forbidden, insufficient scope, not claimable …).
	syncReasonClaimRefused = "claim_refused"
	// syncReasonAnchorNotRecorded — the change landed and its anchor could not
	// be recorded on the server.
	syncReasonAnchorNotRecorded = "anchor_not_recorded"
	// syncReasonDuplicateFolderID — two folders in the walked tree carry one id.
	syncReasonDuplicateFolderID = "duplicate_folder_id"
	// syncReasonNotAdopted — `sync unmanage` on a folder with no id.
	syncReasonNotAdopted = "not_adopted"
	// syncReasonFolderAnchorTooLarge — the folder's anchor (its docs-sidecar
	// baselines) is over the server's cap, so none of it was recorded.
	syncReasonFolderAnchorTooLarge = "folder_anchor_too_large"
)

// applyActionAdopt is a unit whose lock entry disagreed with the server's
// state and was rewritten from it: the row is the server's, not the
// lock's. Listed in place of `update`; it writes exactly what an update does.
const applyActionAdopt applyAction = "adopt"

// managedState is an adopted folder's server state for the selected stack.
type managedState struct {
	folderID  string
	featureID string
	stack     string
	source    string
	// claims is every claim of this folder in the feature, by address — as last
	// read or written. A claim in a refusal state is kept here only when a
	// `removed` entry releases it.
	claims map[string]*api.ManagedClaim
	// folder is the FOLDER's anchor as last read or written: the per-folder
	// state that belongs to no claim (wfdir.Lock.FolderAnchor). Never nil once
	// refreshed; revision 0 is a folder with no anchor yet.
	folder *api.ManagedFolderAnchor
	// folderTooLarge is the 413 the folder anchor met, kept so the command's
	// later saves do not resend it.
	folderTooLarge *managedRefusal
	// rewrites are the addresses whose lock entry the refresh rewrote from the
	// server, for the note and the `adopt` units.
	rewrites []managedRewrite
	// relabel are claims of a foreign repository that --adopt-foreign takes
	// over; release are claims in a refusal state that a `removed` entry lets
	// go. Both are pending until an acting path (applyManaged) sends them.
	relabel []string
	release []string
	settled bool
	// readOnly is a state an acting command that is not a deploy opened
	// (openFolder rather than openFolderForDeploy): it is read, and nothing is
	// ever written from it — no settle, no lazy claim, no anchor.
	readOnly bool
	// unclaimable are rows `sync adopt` was refused a claim for: reported there,
	// and not claimed a second time by the anchor pass.
	unclaimable map[string]bool
	// createRefusal is the first write this command refused on the server's
	// state: a create refused by it (address_claimed by a holder that is not
	// ok, or a name_taken whose address holds one), or an anchorless automation
	// that differs at write time (no_drift_anchor, automationPushOptions
	// RequireAnchor) — kept so a tree command reports the folder under that
	// outcome's reason, not as a generic not_deployed, when the loop folds the
	// refusal into a per-file count.
	createRefusal *managedRefusal
	// wroteRecords is set once applyManaged has written anything on the
	// server — a release, a relabel, a move or a lazy claim — so a refusal
	// after it does not say nothing was written (see sync apply's run).
	wroteRecords bool
	// anchorErr is the first anchor write that failed. The save that triggered
	// it does not fail — its local half landed — and the command reports it at
	// the end instead (see managedFinish and sync apply's run).
	anchorErr error
}

type managedRewrite struct {
	Address string
	Kind    string
	From    string
	To      string
	// HeldAt is set when the lock's entry here was dropped because the server's
	// state holds its row at this other address (a `moved` another run applied).
	HeldAt string
}

// managedRefusal is the error a refresh or a settle returns for a claim
// outcome that stops this folder: Reason is a sync* constant.
type managedRefusal struct {
	Reason string
	Detail string
}

func (e *managedRefusal) Error() string { return e.Detail }

// asManagedRefusal extracts a managedRefusal.
func asManagedRefusal(err error) (*managedRefusal, bool) {
	var r *managedRefusal
	if errors.As(err, &r) {
		return r, true
	}
	return nil, false
}

// managedReadError is a GET /managed that got no usable answer: the folder's
// state is unknown, which is not a refusal of anything in it.
type managedReadError struct{ err error }

func (e *managedReadError) Error() string { return e.err.Error() }
func (e *managedReadError) Unwrap() error { return e.err }

// adoptedHere reports whether this folder runs on server state: it has an id,
// and the selection names a stack with a feature that `ronja sync unmanage` has
// not released. A folder with an id selected through a legacy unnamed entry has
// no stack to label its claims with and no lock to cache them in, and runs
// legacy until a stack is named; a stack marked "unmanaged" runs legacy until
// `ronja sync adopt` names it again.
func (f *folder) adoptedHere() bool {
	return f.Manifest.Adopted() && f.Stack != "" && f.Binding.FeatureID != "" && !f.Manifest.Stacks[f.Stack].Unmanaged
}

// requireAdoptedStackFeature refuses an ADOPTED folder (one with an id) whose
// selected stack names no feature. adoptedHere is false for it, so without
// this it would run legacy — deploying wherever the row it last made happens
// to live, with no claim and no write gate — and say nothing: a folder that
// declares it is managed, silently unmanaged. A stack marked "unmanaged" is
// the deliberate form of that, and is let through.
func (f *folder) requireAdoptedStackFeature() error {
	if !f.Manifest.Adopted() || f.Stack == "" || f.Binding.FeatureID != "" {
		return nil
	}
	if st, ok := f.Manifest.Stacks[f.Stack]; !ok || st.Unmanaged {
		return nil
	}
	return &managedRefusal{Reason: syncReasonNoFeature, Detail: fmt.Sprintf(
		"%s has a folder id, and %s names no \"featureID\". An adopted folder keeps its records in Ronja under its stack's feature and is checked against that feature before anything is written, so without one it would deploy unclaimed and unchecked. Add the feature this stack deploys to as \"featureID\" under stack %q in %s — or, to deploy this stack the classic way from %s alone, mark it \"unmanaged\": true. Nothing was written",
		wfdir.ManifestName, describeStackFor(f.Stack), f.Stack, wfdir.ManifestName, wfdir.LockName)}
}

// managedAddressOf is the address one claim kind uses for a folder-relative
// path: "" for a folder kind, the path otherwise.
func managedAddressOf(kind wfdir.Kind, path string) string {
	if wfdir.KeysRowsByFile(kind) {
		return path
	}
	return ""
}

// managedClaim is the `managed` object every create this folder sends carries,
// nil for a folder that is not adopted (the key is then absent from the body —
// a legacy folder's create is byte-identical to today's).
func (f *folder) managedClaim(address string) *api.ManagedClaimInput {
	if f.managed == nil {
		return nil
	}
	return &api.ManagedClaimInput{
		FolderID:    f.managed.folderID,
		Address:     address,
		StackLabel:  f.managed.stack,
		SourceLabel: f.managed.source,
	}
}

// refreshManaged reads this folder's claims and makes the in-memory lock say
// what the server says. READ-ONLY: it sends nothing but the GET and
// writes nothing to disk; what it finds pending is settled by applyManaged on
// an acting path.
//
// The outcomes, as built:
//
//   - claim ok → the lock records the claim's row, and its anchor from
//     `applied` (so the drift guard is armed); a lock that said otherwise is a
//     rewrite (an `adopt` unit, and a note).
//   - claim not_readable / trashed / lost / moved_out → refused with that
//     reason — unless a `removed` entry names the address, which releases it.
//   - no claim, lock has a row → left in the lock; applyManaged claims it.
//   - no claim, no lock → nothing here; the create that follows claims it.
func (f *folder) refreshManaged(ctx context.Context) error {
	f.managed = nil
	if !f.adoptedHere() {
		return nil
	}
	if f.Resolved.Token == "" {
		return &managedReadError{err: fmt.Errorf("%s has a folder id, so its state lives on %s — and this credential has no token to read it with. Sign in (`ronja login`) or set RONJA_TOKEN",
			wfdir.ManifestName, f.Resolved.URL)}
	}
	st := &managedState{
		folderID:  f.Manifest.ID,
		featureID: f.Binding.FeatureID,
		stack:     f.Stack,
		source:    sourceLabelOf(f.Root),
		claims:    map[string]*api.ManagedClaim{},
	}
	client := newClient(f.Resolved.URL, f.Resolved.Token)
	state, err := client.ReadManaged(ctx, st.featureID, st.folderID)
	if err != nil {
		return &managedReadError{err: describeManagedReadError(err, f)}
	}
	claims := state.Claims
	st.folder = state.Folder
	// The docs-sidecar records an older build of this branch kept on a claim's
	// anchor — the fallback read below, while the folder anchor holds none.
	legacyDocs := map[string]wfdir.LockTableDocs{}

	removed := map[string]bool{}
	for _, p := range f.Manifest.Removed {
		removed[p] = true
	}
	before := f.managedSnapshot()
	var problems []managedRefusal
	for i := range claims {
		c := claims[i]
		kind := wfdir.ManagedKindAt(f.Kind, c.Address)
		if kind == "" || kind != c.ResourceKind {
			problems = append(problems, managedRefusal{syncReasonClaimMismatch, fmt.Sprintf(
				"the server records a %s at address %q under this folder's id (%s), and a %s folder cannot hold one there — two folders share this id (a copied folder?). Give the copy its own: delete its \"id\" from %s and run `ronja sync adopt --stack %s` in it",
				c.ResourceKind, c.Address, st.folderID, f.Kind.Label, wfdir.ManifestName, f.Stack)})
			continue
		}
		if c.State != api.ManagedStateOK {
			if removed[c.Address] {
				// `removed` is the escape hatch for exactly this: the folder stops
				// managing the address, whatever state its row is in.
				st.claims[c.Address] = &c
				st.release = append(st.release, c.Address)
				f.Lock.DropManaged(f.Stack, f.Kind, c.Address)
				continue
			}
			problems = append(problems, describeClaimState(f, &c))
			continue
		}
		if foreignClaim(c.SourceLabel, st.source) {
			if !flagAdoptForeign {
				problems = append(problems, managedRefusal{syncReasonForeignClaim, fmt.Sprintf(
					"%s is managed from another repository (%s), and this folder is %s — a fork or a template copy that kept the folder id and targets the same feature. Re-run with --adopt-foreign to take the address over from here, or give this copy its own feature",
					describeManagedAddress(f, c.Address), orUnknown(c.SourceLabel), orUnknown(st.source))})
				continue
			}
			st.relabel = append(st.relabel, c.Address)
		}
		st.claims[c.Address] = &c
		var anchor map[string]any
		if len(c.Applied) > 0 {
			// SERVER WINS: the anchor it recorded is the drift baseline, whatever
			// the lock carries. An empty one (a claim nothing has anchored yet)
			// keeps the lock's for the same row, and is cleared for another.
			anchor = c.Applied
		}
		if err := f.Lock.SetManaged(f.Stack, f.Kind, c.Address, c.ResourceID, anchor); err != nil {
			return err
		}
		if docs, err := wfdir.TableDocsIn(c.Applied); err == nil {
			maps.Copy(legacyDocs, docs)
		}
	}
	if len(problems) > 0 {
		return joinManagedRefusals(problems)
	}
	// The FOLDER's anchor: the docs-sidecar and health-checks baselines, which
	// no claim carries. SERVER WINS for every sidecar and checks file it names
	// (after the claims, so over a checks record an older claim anchor carried).
	// Only while it names no sidecar are the docs records an older claim anchor
	// carried read instead — once; the next anchor write moves both onto the
	// folder anchor and clears them from the claims.
	folderDocs, err := wfdir.TableDocsIn(st.folder.Applied)
	if err != nil {
		return fmt.Errorf("read this folder's anchor on stack %q: %w", f.Stack, err)
	}
	if len(folderDocs) == 0 {
		f.Lock.MergeTableDocs(f.Stack, legacyDocs)
	}
	if err := f.Lock.SetFolderAnchor(f.Stack, st.folder.Applied); err != nil {
		return fmt.Errorf("read this folder's anchor on stack %q: %w", f.Stack, err)
	}
	// SERVER WINS row → address too. A lock entry naming a row that an ok claim
	// holds at ANOTHER address is a `moved` some other run applied and never
	// committed — the shape of every CI run that restores the lock committed at
	// adopt time. Left in place, the declaration would read as two addresses
	// bound to one row and refuse, and the lazy backfill would try to claim the
	// row a second time. Only an ok claim decides: a claim in a refusal state has
	// already refused above, unless `removed` released it.
	heldAt := map[string]string{}
	for addr, c := range st.claims {
		if c.State == api.ManagedStateOK {
			heldAt[c.ResourceKind+"\x00"+c.ResourceID] = addr
		}
	}
	relocated := map[string]string{}
	for _, a := range f.Lock.ManagedAddresses(f.Stack, f.Kind) {
		if held, ok := heldAt[a.Kind+"\x00"+a.ID]; ok && held != a.Address {
			f.Lock.DropRelocated(f.Stack, f.Kind, a.Address, held)
			relocated[a.Address] = held
		}
	}
	after := f.managedSnapshot()
	for addr, now := range after {
		if was, ok := before[addr]; !ok || was != now {
			st.rewrites = append(st.rewrites, managedRewrite{Address: addr, Kind: wfdir.ManagedKindAt(f.Kind, addr),
				From: before[addr].id, To: now.id})
		}
	}
	for addr, was := range before {
		if _, ok := after[addr]; !ok {
			st.rewrites = append(st.rewrites, managedRewrite{Address: addr, Kind: wfdir.ManagedKindAt(f.Kind, addr), From: was.id,
				HeldAt: relocated[addr]})
		}
	}
	sort.Slice(st.rewrites, func(i, j int) bool { return st.rewrites[i].Address < st.rewrites[j].Address })
	f.managed = st
	// The binding is a function of the lock: re-derive it, so every loop reads
	// the server's rows through the f.Binding it already reads — and resolve
	// the folder's own outputs again against the workflow it now names, which
	// in a checkout with no lock only the server's claim knows.
	f.matchBinding()
	if f.addressStack == f.Stack {
		f.resolveOutputs(ctx)
	}
	return nil
}

// managedEntry is one address's recorded row and anchor, for the rewrite diff.
type managedEntry struct {
	id     string
	anchor string
}

func (f *folder) managedSnapshot() map[string]managedEntry {
	out := map[string]managedEntry{}
	for _, a := range f.Lock.ManagedAddresses(f.Stack, f.Kind) {
		out[a.Address] = managedEntry{id: a.ID, anchor: canonicalJSON(f.Lock.Anchor(f.Stack, f.Kind, a.Address))}
	}
	return out
}

// managedBackfill lists the rows this folder's lock records that the server
// has no claim for — what applyManaged claims (the lazy backfill).
//
// Only at an address whose FILE is still in the folder. A lock entry for a file
// that is gone is not a row this folder deploys: most often a stale lock (one
// committed before a `removed` was applied, restored by a CI checkout after the
// entry was taken out), and claiming it would write a record on the server for
// a row the folder then refuses as an orphan anyway. Rename detection reports
// the gone file; the server is left alone.
func (f *folder) managedBackfill() []wfdir.ManagedAddress {
	if f.managed == nil {
		return nil
	}
	var out []wfdir.ManagedAddress
	for _, a := range f.Lock.ManagedAddresses(f.Stack, f.Kind) {
		if _, claimed := f.managed.claims[a.Address]; !claimed && f.addressOnDisk(a.Address) {
			out = append(out, a)
		}
	}
	return out
}

// addressOnDisk reports whether a claim address's file is in the folder — always
// true for a folder kind, whose single address is the folder itself.
func (f *folder) addressOnDisk(address string) bool {
	if address == "" || !wfdir.KeysRowsByFile(f.Kind) {
		return true
	}
	_, err := os.Stat(filepath.Join(f.Root, filepath.FromSlash(address)))
	return err == nil
}

// applyManaged settles what the refresh found pending, on an ACTING path only,
// before the loop runs — in the order that keeps each step's precondition true:
//
//  1. release the claims a `removed` entry names whose rows are unusable;
//  2. the address declarations: each `moved` re-keys its claim (POST
//     /managed/move) and each `removed` unmanages one (DELETE), evaluated
//     against the refreshed state — so a moved row is claimed at its new address
//     BEFORE anything below could try to claim it there a second time;
//  3. --adopt-foreign's relabels;
//  4. the lazy backfill: every row the lock records with no claim is claimed,
//     unless it is claimed elsewhere, which refuses and names that address.
//
// The ROW is never touched by any of it. Idempotent: a second call is a no-op.
func (f *folder) applyManaged(ctx context.Context) error {
	st := f.managed
	if st == nil || st.settled || st.readOnly {
		return nil
	}
	client := newClient(f.Resolved.URL, f.Resolved.Token)

	for _, addr := range st.release {
		c := st.claims[addr]
		if _, err := client.UnmanageManaged(ctx, c.ID); err != nil && api.StatusOf(err) != http.StatusNotFound {
			return fmt.Errorf("release the claim on %s: %w", describeManagedAddress(f, addr), err)
		}
		delete(st.claims, addr)
		st.wroteRecords = true
		fmt.Fprintf(os.Stderr, "  Note: %s is no longer managed here (\"removed\"); its row was left as it is.\n", describeManagedAddress(f, addr))
	}
	st.release = nil

	// --adopt-foreign's relabels, BEFORE the moves: a relabel is a write to the
	// claim itself, wherever it sits, and a claim a `moved` re-keys below would
	// otherwise be looked up at the address it just left, found nowhere, and
	// keep the other repository's label.
	for _, addr := range st.relabel {
		c := st.claims[addr]
		if c == nil {
			continue
		}
		stack, source := st.stack, st.source
		updated, err := client.RecordManagedApplied(ctx, c.ID, api.ManagedAppliedInput{
			ExpectedRevision: c.Revision, Applied: c.Applied, StackLabel: &stack, SourceLabel: &source,
		})
		if err != nil {
			return fmt.Errorf("take %s over from %s (--adopt-foreign): %w", describeManagedAddress(f, addr), c.SourceLabel, err)
		}
		fmt.Fprintf(os.Stderr, "  Note: --adopt-foreign — %s was managed from %s and is now managed from here.\n",
			describeManagedAddress(f, addr), orUnknown(c.SourceLabel))
		st.claims[addr] = updated
		st.wroteRecords = true
	}
	st.relabel = nil

	if wfdir.KeysRowsByFile(f.Kind) {
		// An evaluation that fails is the push's own refusal to make; nothing is
		// sent for it here.
		if out, err := f.resolveAddresses(); err == nil && out != nil {
			for _, m := range out.Moves {
				c := st.claims[m.From]
				if c == nil {
					continue
				}
				moved, err := client.MoveManaged(ctx, st.featureID, st.folderID, m.From, m.To)
				if err != nil {
					if holder, taken := api.AsAddressClaimed(err); taken {
						if holder.Departed() {
							return &managedRefusal{syncReasonMovedOut, fmt.Sprintf(
								"%s declares %s moved to %s, and on the server %s holds a tombstone — %s was moved to another feature, and the address stays taken until it is released. Release it with `ronja sync unmanage --stack %s` and re-adopt, or take the entry out",
								wfdir.ManifestName, m.From, m.To, m.To, describeRowOf(holder), f.Stack)}
						}
						return &managedRefusal{syncReasonAddressConflict, fmt.Sprintf(
							"%s declares %s moved to %s, and on the server %s already holds %s — one address holds one row. Take the entry out and add %q to \"removed\" if that row is the one to keep",
							wfdir.ManifestName, m.From, m.To, m.To, describeHolder(holder), m.From)}
					}
					if api.WireCodeOf(err) == api.ManagedCodeNotClaimed {
						// Another run applied this same declaration first: the claim
						// already sits at the new address, holding the same row.
						if cur := f.rereadManagedClaim(ctx, m.To); cur != nil && cur.State == api.ManagedStateOK && cur.ResourceKind == c.ResourceKind && cur.ResourceID == c.ResourceID {
							delete(st.claims, m.From)
							st.claims[m.To] = cur
							continue
						}
					}
					if api.WireCodeOf(err) == api.ManagedCodeMovedOut {
						return &managedRefusal{syncReasonMovedOut, fmt.Sprintf(
							"%s declares %s moved to %s, and the claim at %s became a tombstone while this ran — its row was moved to another feature, so the claim can only be released. Re-run to read its state",
							wfdir.ManifestName, m.From, m.To, m.From)}
					}
					return fmt.Errorf("move the claim %s → %s on stack %q: %w", m.From, m.To, f.Stack, err)
				}
				delete(st.claims, m.From)
				st.claims[m.To] = moved
				st.wroteRecords = true
			}
			for _, r := range out.Removals {
				c := st.claims[r.Path]
				if c == nil {
					continue
				}
				if _, err := client.UnmanageManaged(ctx, c.ID); err != nil && api.StatusOf(err) != http.StatusNotFound {
					return fmt.Errorf("unmanage %s on stack %q: %w", r.Path, f.Stack, err)
				}
				delete(st.claims, r.Path)
				st.wroteRecords = true
			}
		}
	}

	if backfill := f.managedBackfill(); len(backfill) > 0 {
		st.wroteRecords = true
		if err := f.claimManagedRows(ctx, client, backfill); err != nil {
			return err
		}
	}
	st.settled = true
	return nil
}

// claimManagedRows claims rows that already exist, in batches of the server's
// bound, and records each claim. Every item reports its own outcome; the
// refusals are folded into one managedRefusal naming each address.
func (f *folder) claimManagedRows(ctx context.Context, client *api.Client, rows []wfdir.ManagedAddress) error {
	st := f.managed
	if len(rows) == 0 {
		return nil
	}
	var problems []managedRefusal
	for start := 0; start < len(rows); start += api.MaxManagedClaimItems {
		end := min(start+api.MaxManagedClaimItems, len(rows))
		batch := rows[start:end]
		items := make([]api.ManagedClaimItem, 0, len(batch))
		for _, a := range batch {
			items = append(items, api.ManagedClaimItem{ResourceKind: a.Kind, ResourceID: a.ID, ManagedClaimInput: *f.managedClaim(a.Address)})
		}
		results, err := client.ClaimManaged(ctx, st.featureID, items)
		if err != nil {
			return fmt.Errorf("claim this folder's rows for stack %q (feature %s): %w", f.Stack, st.featureID, err)
		}
		for _, r := range results {
			if r.Index < 0 || r.Index >= len(batch) {
				continue
			}
			a := batch[r.Index]
			switch r.Outcome {
			case api.ManagedOutcomeClaimed, api.ManagedOutcomeAlreadyClaimed:
				if r.Claim != nil {
					st.claims[a.Address] = r.Claim
				}
			default:
				problems = append(problems, describeClaimRefusal(f, a, r))
			}
		}
	}
	if len(problems) > 0 {
		return joinManagedRefusals(problems)
	}
	return nil
}

// settleManagedForActing is the acting open's whole managed step: refresh,
// then settle (settleManaged), printing what the server's state rewrote.
func (f *folder) settleManagedForActing(ctx context.Context) error {
	if err := f.refreshManaged(ctx); err != nil {
		return err
	}
	notes, err := f.settleManaged(ctx)
	for _, n := range notes {
		fmt.Fprintf(os.Stderr, "  Note: %s.\n", n)
	}
	return err
}

// settleManaged is the ONE managed settle step, shared by the acting open and
// `sync apply`'s execute pass: send what the refresh found pending
// (applyManaged), and write the lock back if the server disagreed with it. It
// answers a note per rewrite, in one wording; the caller prints them or puts
// them on the folder's line. An anchor the lock has and the server lacks is
// recorded by the loop's next lock save, like every other.
func (f *folder) settleManaged(ctx context.Context) ([]string, error) {
	if f.managed == nil {
		return nil, nil
	}
	if err := f.applyManaged(ctx); err != nil {
		return nil, err
	}
	notes := f.managedRewriteNotes()
	if len(f.managed.rewrites) > 0 {
		if err := wfdir.SaveLock(f.Root, f.Lock); err != nil {
			return notes, fmt.Errorf("rewrite %s from the server's state: %w", wfdir.LockPath(f.Root), err)
		}
	}
	return notes, nil
}

// adoptManagedClaim binds this folder to the row an ok claim names — the
// address_claimed and name_taken recoveries. The claim's anchor comes with it,
// so the update that follows is guarded exactly as any other.
func (f *folder) adoptManagedClaim(address string, c *api.ManagedClaim) error {
	if f.managed == nil || c == nil || c.State != api.ManagedStateOK || c.ResourceID == "" {
		return fmt.Errorf("no readable claim to adopt")
	}
	switch f.Kind.Name {
	case wfdir.KindPipeline:
		if wfdir.ManagedKindAt(f.Kind, address) == wfdir.ManagedTable {
			f.recordBinding(f.Binding.WithTable(address, c.ResourceID))
		}
	case wfdir.KindAutomation:
		f.recordBinding(f.Binding.WithAutomation(address, c.ResourceID))
	default:
		b, err := wfdir.Binding{FeatureID: f.Binding.FeatureID}.WithResourceID(f.Kind, c.ResourceID)
		if err != nil {
			return err
		}
		f.recordBinding(b)
	}
	var anchor map[string]any
	if len(c.Applied) > 0 {
		anchor = c.Applied
	}
	if err := f.Lock.SetManaged(f.Stack, f.Kind, address, c.ResourceID, anchor); err != nil {
		return err
	}
	f.managed.claims[address] = c
	return nil
}

// adoptOnAddressClaimed is a create's 409 address_claimed, settled: the
// address already holds a row, the create was rolled back whole, and when the
// holder's claim is ok THAT row is this file's — adopted by id, never by name,
// and the caller continues as an update. Any other state refuses with that
// state's outcome. ok=false: err is not an address_claimed at all.
func (f *folder) adoptOnAddressClaimed(ctx context.Context, address string, err error) (id string, ok bool, refusal error) {
	holder, taken := api.AsAddressClaimed(err)
	if !taken || f.managed == nil {
		return "", false, nil
	}
	if holder == nil {
		// Released again between the collision and the server's read: re-read.
		holder = f.rereadManagedClaim(ctx, address)
	}
	if holder == nil {
		return "", true, &managedRefusal{syncReasonClaimedElsewhere, fmt.Sprintf(
			"%s was claimed by another apply while this one created, and the server could not name the holder — re-run to read its state", describeManagedAddress(f, address))}
	}
	if holder.State != api.ManagedStateOK {
		r := describeClaimState(f, holder)
		f.noteCreateRefusal(&r)
		return "", true, &r
	}
	if err := f.adoptManagedClaim(address, holder); err != nil {
		return "", true, err
	}
	fmt.Fprintf(os.Stderr, "  Note: %s was created by another apply at the same moment — adopting %s rather than creating a second one.\n",
		describeManagedAddress(f, address), holder.ResourceID)
	return holder.ResourceID, true, nil
}

// errAdoptedOnCreate is what a single-row create returns once
// adoptOnAddressClaimed has bound the folder to the row that won its address:
// the push restarts once, as the update it now is (see runPush).
var errAdoptedOnCreate = errors.New("adopted the row already holding this folder's address")

func adoptedOnCreate(err error) bool { return errors.Is(err, errAdoptedOnCreate) }

// adoptOnNameTaken is a table's or a metric's 409 name_taken in an adopted
// folder: the name gate runs before the claim insert, so a lost
// race to this address — or a lost 200 — answers name_taken. Re-read state: a
// claim now at this address is the winner, adopted by id; none means the name
// belongs to a row this folder does not manage, which is refused with the
// adoption remedy. Never adopted by name.
func (f *folder) adoptOnNameTaken(ctx context.Context, address, name, why string) (string, error) {
	holder := f.rereadManagedClaim(ctx, address)
	if holder != nil && holder.State == api.ManagedStateOK {
		if err := f.adoptManagedClaim(address, holder); err != nil {
			return "", err
		}
		fmt.Fprintf(os.Stderr, "  Note: %s answered name_taken and the server's state names %s at this address — adopting it rather than creating a second one.\n",
			describeManagedAddress(f, address), holder.ResourceID)
		return holder.ResourceID, nil
	}
	if holder != nil {
		r := describeClaimState(f, holder)
		f.noteCreateRefusal(&r)
		return "", &r
	}
	return "", fmt.Errorf("%q is already taken in feature %s:\n      %s\n    No address of this folder claims the row holding it, and an adopted folder never takes a row over by its name. If it IS this file's row, record its id under %q in %s and run `ronja sync adopt --stack %s`; otherwise rename the file or its declared name",
		name, f.Binding.FeatureID, why, address, wfdir.LockName, f.Stack)
}

// noteCreateRefusal keeps the first create refusal (see createRefusal).
func (f *folder) noteCreateRefusal(r *managedRefusal) {
	if f.managed != nil && f.managed.createRefusal == nil {
		f.managed.createRefusal = r
	}
}

// managedRefusalOf is the server-state outcome a failed push carries: the error
// itself, or the create refusal a per-file loop folded into its count.
func managedRefusalOf(f *folder, err error) (*managedRefusal, bool) {
	if r, ok := asManagedRefusal(err); ok {
		return r, true
	}
	if f.managed != nil && f.managed.createRefusal != nil {
		return f.managed.createRefusal, true
	}
	return nil, false
}

// rereadManagedClaim reads this folder's claim at one address again, nil when
// there is none (or the read fails — the caller then refuses).
func (f *folder) rereadManagedClaim(ctx context.Context, address string) *api.ManagedClaim {
	if f.managed == nil {
		return nil
	}
	client := newClient(f.Resolved.URL, f.Resolved.Token)
	claims, err := client.ListManaged(ctx, f.managed.featureID, f.managed.folderID)
	if err != nil {
		return nil
	}
	for i := range claims {
		if claims[i].Address == address {
			return &claims[i]
		}
	}
	return nil
}

// managedFolders are the adopted folders an acting single-folder command
// opened, so the command's end can report an anchor that failed to record.
// Reset with every command tree.
var managedFolders []*folder

// managedFinish is the end of an acting single-folder command: the first
// anchor that failed to record becomes the command's error. Nothing is
// recorded here — every lock save already recorded what it changed — so this
// only reports. sync apply does not register its folders here; it reports per
// folder instead.
func managedFinish() error {
	defer func() { managedFolders = nil }()
	for _, f := range managedFolders {
		if f.managed != nil && f.managed.anchorErr != nil {
			return fmt.Errorf("%s: the server's state was not updated: %w", f.Root, f.managed.anchorErr)
		}
	}
	return nil
}

// mintFolderIDAtInit gives a folder `init` creates ON A STACK its folder id —
// one of the two places an id is ever minted (the other is `ronja sync
// adopt`), and both are local commands a person runs: an id minted on
// a CI runner and lost with its checkout would make the next run mint another
// and duplicate every row, so clone, push and apply never mint.
//
// Only with --stack. Server state is per stack (its feature), and a folder
// initialized without one is written in the legacy instances[] shape and runs
// the classic loop exactly as it always has — an id there would only sit
// in the file doing nothing until a stack was named. Every kind's `init` calls
// it, modules included now that `ronja sync` walks module folders and can adopt
// and unmanage one.
func mintFolderIDAtInit(m *wfdir.Manifest) error {
	if flagStack == "" || m.ID != "" {
		return nil
	}
	id, err := wfdir.NewFolderID()
	if err != nil {
		return err
	}
	m.ID = id
	return nil
}
