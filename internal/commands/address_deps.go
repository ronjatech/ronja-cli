package commands

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"

	"github.com/ronjatech/ronja-cli/internal/api"
	"github.com/ronjatech/ronja-cli/internal/config"
	"github.com/ronjatech/ronja-cli/internal/markers"
	"github.com/ronjatech/ronja-cli/internal/wfdir"
)

// References BY ADDRESS: a folder declares something another folder
// of the same repository manages — a table or metric a pipeline builds, a
// workflow, an app, a module — by WHERE that folder declares it:
//
//	"orders": {"kind": "table", "from": "../../pipelines/sales", "address": "orders.sql"}
//
// and the CLI resolves it, per stack, through the repository's own server
// state: the other folder's id and that folder's stack OF THE SAME NAME name a
// claim (GET /managed?feature=&folder=), and the claim's row is the id the alias
// resolves to. No `bind` entry is needed for anything the repository manages,
// and one declaration answers in every stack that deploys both folders.
//
// The resolved ids are joined into the folder's codec (and into
// Selection.AddressBind for the pre-flight), so every loop resolves an address
// alias exactly as it resolves a bound one — toWire out, toDisk back.

// Why an address dependency did not resolve. Contract strings, like every sync
// reason: `sync status`, `sync check` and `sync apply` report them.
const (
	// syncReasonAddressUnresolved — the other folder's stack holds no usable
	// claim at the address: nothing has been applied there yet, or its claim
	// reads lost / trashed / not_readable / moved_out. Apply that folder first.
	syncReasonAddressUnresolved = "address_unresolved"
	// syncReasonAddressStackMismatch — the other folder does not declare the
	// stack being applied, or its stack of that name points at a different
	// instance or organization, or (a workflow's output) binds it there to a
	// table outside that stack's feature. An address must never resolve across
	// targets.
	syncReasonAddressStackMismatch = "address_stack_mismatch"
	// syncReasonAddressFolderUnreadable — "from" names no folder this build can
	// open, or one of a kind that cannot hold the declared kind.
	syncReasonAddressFolderUnreadable = "address_folder_unreadable"
	// syncReasonAddressNotAdopted — the other folder has no folder id (or its
	// stack was unmanaged), so it keeps no server state an address could be
	// resolved through.
	syncReasonAddressNotAdopted = "address_not_adopted"
	// syncReasonAddressOutsideRepository — "from" leads out of the repository
	// the folder belongs to (its git work tree; without one, the tree `ronja
	// sync` walks, or the folder itself). An address names a folder of the SAME
	// repository; one outside it is a folder nobody reviewing this one sees.
	syncReasonAddressOutsideRepository = "address_outside_repository"
	// syncReasonAddressNotWritten — a table named by address in a WORKFLOW
	// folder ("from" a workflow folder, "address" one of its dependencies)
	// names no dependency of that folder declared "write": true or "output":
	// true, so it is no table that workflow writes.
	syncReasonAddressNotWritten = "address_not_written"
)

// addressWalkRootKey carries the directory a `ronja sync` command walks, the
// repository boundary for a folder in no git work tree.
type addressWalkRootKey struct{}

// withAddressWalkRoot records the tree a command walks, for addressBoundary.
func withAddressWalkRoot(ctx context.Context, root string) context.Context {
	return context.WithValue(ctx, addressWalkRootKey{}, root)
}

// addressBoundary is the directory a folder's "from" may not leave, and how to
// say it: the git work tree holding the folder (the nearest ancestor with a
// .git entry, file or directory); without one, the tree a `ronja sync` command
// walks, when it holds the folder; without either, the folder itself.
func addressBoundary(ctx context.Context, root string) (dir, described string) {
	for dir := realPath(root); ; {
		if _, err := os.Lstat(filepath.Join(dir, ".git")); err == nil {
			return dir, fmt.Sprintf("the git repository at %s", dir)
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			break
		}
		dir = parent
	}
	if walk, ok := ctx.Value(addressWalkRootKey{}).(string); ok && walk != "" && pathWithin(realPath(walk), realPath(root)) {
		return realPath(walk), fmt.Sprintf("the tree being synced (%s)", walk)
	}
	return realPath(root), "this folder — it is in no git repository, so run `ronja sync` from the tree's root, or put the tree under git"
}

// realPath is path with every symlink resolved, so a link inside the
// repository cannot lead out of it; the cleaned path when it does not exist.
func realPath(path string) string {
	abs, err := filepath.Abs(path)
	if err != nil {
		abs = path
	}
	if real, err := filepath.EvalSymlinks(abs); err == nil {
		return real
	}
	return filepath.Clean(abs)
}

// pathWithin reports whether path is dir or below it.
func pathWithin(dir, path string) bool {
	rel, err := filepath.Rel(dir, path)
	return err == nil && rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator))
}

// addressEdge is one RESOLVED address dependency: which alias, of which kind,
// managed by which folder at which address, and the row it named for this
// stack. Recorded on the folder (folder.addressEdges) so a later slice can
// order builds across folders — a table read by address is a build-ordering
// edge — without re-deriving it.
type addressEdge struct {
	Alias    string
	Kind     string
	FromRoot string
	Address  string
	ID       string
	// FeatureID is the feature the row's claim is in: the other folder's
	// stack of the same name — which the write gate notes when it is not
	// this stack's.
	FeatureID string
	// Written is a table another folder's WORKFLOW writes, named by its write
	// target or output there (resolveWorkflowTable): a read of what that
	// workflow writes, which confers no authority for this folder to write the
	// table. Never one of the rows this repository manages for writeAuthorityOf.
	Written bool
}

// addressOverlay is in-run resolution: a row CREATED
// earlier in the same `sync apply` resolves an address in memory, so standing a
// fresh stack up is one apply rather than apply, bind, commit, apply. It is
// keyed by the other folder's (feature, folder id, address) — the feature
// being that folder's stack of the same name, so the key is (folder id, stack,
// address) in the only form a resolution here can ask it in.
//
// Carried on the context rather than in a package variable: only `sync apply`
// fills one, for one run, and every other command resolves through the claims
// the server holds exactly as before.
type addressOverlay struct {
	mu   sync.Mutex
	rows map[string]overlayRow
}

// overlayRow is one row the run made or confirmed: its id and its managed kind
// (wfdir.Managed*), so a declared kind is checked against it as a claim's is.
type overlayRow struct {
	ID   string
	Kind string
}

type addressOverlayKey struct{}

func newAddressOverlay() *addressOverlay { return &addressOverlay{rows: map[string]overlayRow{}} }

func withAddressOverlay(ctx context.Context, o *addressOverlay) context.Context {
	return context.WithValue(ctx, addressOverlayKey{}, o)
}

func addressOverlayFrom(ctx context.Context) *addressOverlay {
	o, _ := ctx.Value(addressOverlayKey{}).(*addressOverlay)
	return o
}

func overlayKey(featureID, folderID, address string) string {
	return featureID + "\x00" + folderID + "\x00" + address
}

// record notes one row this run holds at an address.
func (o *addressOverlay) record(featureID, folderID, address, kind, id string) {
	if o == nil || featureID == "" || folderID == "" || id == "" {
		return
	}
	o.mu.Lock()
	defer o.mu.Unlock()
	o.rows[overlayKey(featureID, folderID, address)] = overlayRow{ID: id, Kind: kind}
}

// lookup answers a row this run holds at an address.
func (o *addressOverlay) lookup(featureID, folderID, address string) (overlayRow, bool) {
	if o == nil {
		return overlayRow{}, false
	}
	o.mu.Lock()
	defer o.mu.Unlock()
	row, ok := o.rows[overlayKey(featureID, folderID, address)]
	return row, ok
}

// recordFolder notes every row an ADOPTED folder's lock now records for its
// stack — what a run just created or confirmed there.
func (o *addressOverlay) recordFolder(f *folder) {
	if o == nil || f == nil || !f.adoptedHere() {
		return
	}
	for _, a := range f.Lock.ManagedAddresses(f.Stack, f.Kind) {
		o.record(f.Binding.FeatureID, f.Manifest.ID, a.Address, a.Kind, a.ID)
	}
}

// addressPending is an address dependency that did not resolve for one reason
// only: the other folder holds no claim at that address YET. `sync apply`
// defers such a dependency when the same run deploys that folder, and resolves
// it once that folder has run (addressOverlay); every other command refuses it
// address_unresolved, as before.
type addressPending struct {
	Alias     string
	Kind      string
	FromRoot  string
	FolderID  string
	FeatureID string
	Address   string
}

// hasAddressDependencies reports whether the manifest declares any dependency
// by address — the gate that keeps every other folder from paying a request.
func hasAddressDependencies(m *wfdir.Manifest) bool {
	for _, dep := range m.Dependencies {
		if dep.ByAddress() {
			return true
		}
	}
	return false
}

// resolveAddressDependencies resolves this folder's address dependencies for
// the selected stack and rebuilds the codec over them. Never an error: what
// did not resolve is recorded per alias (Selection.AddressErrors) and refused
// by the pre-flight of whichever command acts, which is where every other
// unresolved alias is refused too.
func (f *folder) resolveAddressDependencies(ctx context.Context) {
	f.addressBind, f.addressErrors, f.addressEdges, f.addressPending, f.addressStack = nil, nil, nil, nil, f.Stack
	f.outputBind, f.outputErrs, f.outputsResolved, f.outputsFor = nil, nil, false, ""
	// A module's files are marker-free, so its `dependencies` block is inert
	// (check and apply say so); resolving it would only report a reason for a
	// block nothing reads.
	if f.Kind.Name == wfdir.KindModule {
		return
	}
	client := f.addressClient()
	if hasAddressDependencies(f.Manifest) {
		f.addressBind, f.addressErrors, f.addressEdges, f.addressPending = resolveAddressDeps(ctx, client, f.Root, f.Manifest, f.Stack)
	}
	if f.addressBind != nil {
		f.Codec = newAliasCodec(f.Manifest.Dependencies, f.codecBind())
	}
	f.resolveOutputs(ctx)
}

// addressClient is the client address resolution reads with, nil for a
// caller with no token (every lookup is then skipped).
func (f *folder) addressClient() *api.Client {
	if f.Resolved != nil && f.Resolved.Token != "" {
		return newClient(f.Resolved.URL, f.Resolved.Token)
	}
	return nil
}

// resolveOutputs (re)resolves the folder's own unbound outputs
// (resolveOwnOutputs) for the workflow f.Binding names now, and rebuilds the
// codec over them. Asked from resolveAddressDependencies and again by
// refreshManaged once the server's claims have re-derived the binding: in a
// checkout with no lock (a CI runner, a stack a CI run stood up) the workflow
// id exists only on the server, so the first ask finds none. A second ask for
// the same workflow id is a no-op.
func (f *folder) resolveOutputs(ctx context.Context) {
	if f.outputsResolved && f.outputsFor == f.Binding.WorkflowID {
		return
	}
	f.outputsResolved, f.outputsFor = true, f.Binding.WorkflowID
	// The previous answer's failures are dropped with it: an output alias is
	// never also an address alias, so these keys are the outputs' own.
	for alias := range f.outputErrs {
		delete(f.addressErrors, alias)
	}
	var outputErrs map[string]wfdir.AddressError
	f.outputBind, outputErrs = f.resolveOwnOutputs(ctx, f.addressClient())
	f.outputErrs = outputErrs
	// An output whose lookup FAILED is refused with the address failures —
	// never sent as its name, which would read back as drift for ever (see
	// resolveOwnOutputs). Separate aliases, so the merge order does not matter.
	for alias, e := range outputErrs {
		if f.addressErrors == nil {
			f.addressErrors = map[string]wfdir.AddressError{}
		}
		f.addressErrors[alias] = e
	}
	if f.outputBind != nil || f.addressBind != nil {
		f.Codec = newAliasCodec(f.Manifest.Dependencies, f.codecBind())
	}
}

// resolveOwnOutputs resolves, IN MEMORY, an `output` dependency this stack
// does not bind and the folder's code READS with `{{ ref() }}`, to the table
// the workflow's runs created under that logical name in the stack's feature.
//
// Unbound, an output is written by name, which is portable — but read by name
// it is a table handle the server resolves at save and STORES AS AN ID, so the
// folder's copy reads back as an id while its file keeps the name: permanent
// drift in every stack whose output exists only because a run made it (a new
// stack, the run after its first apply). Resolved here, the name goes out as
// the id and comes back as the name, exactly as a bound alias does — in the
// output's `{{ write() }}` markers too, as for a bound output. Before
// the first run there is no output to resolve to, and the name goes out as it
// is.
//
// Only an ADOPTED folder, only a workflow that exists, and only when some
// unbound output is ref-read — every other folder pays no request.
//
// A list that FAILS is not "no output yet": falling back to the name would
// deploy exactly the drift this exists to prevent, silently, and the next
// apply would refuse the folder as drifted for an edit nobody made. So each
// output the code reads is returned as a lookup_failed error instead, and
// refused with the address failures by the pre-flight (addressRefusals) — the
// same answer an address whose claims could not be read gets. Retrying costs
// one command; deploying costs a folder that reads as drifted until someone
// pulls it.
func (f *folder) resolveOwnOutputs(ctx context.Context, client *api.Client) (map[string]string, map[string]wfdir.AddressError) {
	if client == nil {
		return nil, nil
	}
	wanted := f.ownOutputsToLookUp()
	if len(wanted) == 0 {
		return nil, nil
	}
	tables, err := client.ListFeatureTables(ctx, f.Binding.FeatureID)
	if err != nil {
		errs := map[string]wfdir.AddressError{}
		for alias := range wanted {
			errs[alias] = wfdir.AddressError{Reason: syncReasonLookupFailed, Detail: fmt.Sprintf(
				"output %q is read with {{ ref() }}, and listing feature %s's tables to find the one this workflow's runs made failed: %v — sent as its name instead, it would come back as an id and read as drifted from then on, so nothing was written. Try again",
				alias, f.Binding.FeatureID, err)}
		}
		return nil, errs
	}
	out := map[string]string{}
	for _, t := range tables {
		if t.WorkflowID == f.Binding.WorkflowID && wanted[t.WorkflowLogicalName] {
			out[t.WorkflowLogicalName] = t.ID
		}
	}
	if len(out) == 0 {
		return nil, nil
	}
	return out, nil
}

// ownOutputsToLookUp is the unbound outputs resolveOwnOutputs looks up: in an
// adopted workflow folder whose workflow exists, each the folder's code reads
// with `{{ ref() }}`. Every other unbound output is written by its name, and
// that is its whole state. Reads the folder's files, no request.
func (f *folder) ownOutputsToLookUp() map[string]bool {
	if f.Kind.Name != wfdir.KindWorkflow || !f.adoptedHere() || f.Binding.WorkflowID == "" {
		return nil
	}
	var unbound []string
	for _, alias := range f.Manifest.AliasNames() {
		if dep := f.Manifest.Dependencies[alias]; dep.Output && f.Bind[alias] == "" {
			unbound = append(unbound, alias)
		}
	}
	if len(unbound) == 0 {
		return nil
	}
	files, _, err := readLocalFiles(f.Root, f.Kind)
	if err != nil {
		return nil
	}
	read := map[string]bool{}
	for _, content := range files {
		for _, o := range markers.Scan(content) {
			if o.Family.Kind == markers.KindTable && !o.Positional {
				read[o.Arg] = true
			}
		}
	}
	for _, content := range optionsQuerySources(f.Manifest) {
		for _, o := range markers.Scan(content) {
			if o.Family.Kind == markers.KindTable && !o.Positional {
				read[o.Arg] = true
			}
		}
	}
	wanted := map[string]bool{}
	for _, alias := range unbound {
		if read[alias] {
			wanted[alias] = true
		}
	}
	return wanted
}

// lookupMissed reports an address or an unbound output this run owed a lookup
// and did not get an answer for: its resolution never ran (no credential, or
// an organization that could not be established), or it recorded a failure.
// Such an alias resolving to nothing says nothing about its binding. An
// unbound output nothing looks up — written by name only, or in a classic
// folder — resolves to nothing as its true state, and is never missed.
func (f *folder) lookupMissed(alias string) bool {
	dep := f.Manifest.Dependencies[alias]
	if _, failed := f.addressErrors[alias]; failed {
		return true
	}
	switch {
	case dep.ByAddress():
		// An address always names a row: resolving to nothing is a lookup
		// that did not run, failed, or waits on a row this run creates.
		return true
	case dep.Output && f.Bind[alias] == "":
		asked := f.outputsResolved && f.outputsFor == f.Binding.WorkflowID && f.addressClient() != nil
		return !asked && f.ownOutputsToLookUp()[alias]
	}
	return false
}

// codecBind is everything the codec resolves through: the stack's binds, the
// resolved addresses and the unbound outputs resolved in memory. The two never share a key — CheckBind refuses a bind
// beside an address declaration — so the merge order does not matter.
func (f *folder) codecBind() map[string]string {
	if len(f.addressBind) == 0 && len(f.outputBind) == 0 {
		return f.Bind
	}
	out := make(map[string]string, len(f.Bind)+len(f.addressBind)+len(f.outputBind))
	for alias, id := range f.Bind {
		out[alias] = id
	}
	for alias, id := range f.addressBind {
		out[alias] = id
	}
	// An unbound output resolved in memory (resolveOwnOutputs) — never one the
	// stack binds, which is what it answers instead of.
	for alias, id := range f.outputBind {
		if _, bound := out[alias]; !bound {
			out[alias] = id
		}
	}
	return out
}

// resolveAddressDeps is the resolution itself, root-parameterised so `sync`
// can ask it of any folder in a tree. One GET /managed per (other folder) —
// several aliases into one folder share it.
func resolveAddressDeps(ctx context.Context, client *api.Client, root string, m *wfdir.Manifest, stack string) (map[string]string, map[string]wfdir.AddressError, []addressEdge, map[string]addressPending) {
	bind := map[string]string{}
	errs := map[string]wfdir.AddressError{}
	pending := map[string]addressPending{}
	var edges []addressEdge
	overlay := addressOverlayFrom(ctx)
	refuse := func(alias, reason, format string, args ...any) {
		errs[alias] = wfdir.AddressError{Reason: reason, Detail: fmt.Sprintf("dependency %q: ", alias) + fmt.Sprintf(format, args...)}
	}
	type claimsOf struct {
		claims []api.ManagedClaim
		err    error
	}
	read := map[string]*claimsOf{}
	// claimsAt is one GET /managed per (feature, folder), however many
	// aliases ask it.
	claimsAt := func(featureID, folderID string) ([]api.ManagedClaim, error) {
		key := featureID + "\x00" + folderID
		got := read[key]
		if got == nil {
			claims, err := client.ListManaged(ctx, featureID, folderID)
			got = &claimsOf{claims: claims, err: err}
			read[key] = got
		}
		return got.claims, got.err
	}
	// tablesAt is one listing per feature, however many outputs ask it.
	type tablesOf struct {
		tables []*api.TableListItem
		err    error
	}
	listed := map[string]*tablesOf{}
	tablesAt := func(featureID string) ([]*api.TableListItem, error) {
		got := listed[featureID]
		if got == nil {
			tables, err := client.ListFeatureTables(ctx, featureID)
			got = &tablesOf{tables: tables, err: err}
			listed[featureID] = got
		}
		return got.tables, got.err
	}

	for _, alias := range m.AliasNames() {
		dep := m.Dependencies[alias]
		if !dep.ByAddress() {
			continue
		}
		otherRoot := filepath.Clean(filepath.Join(root, filepath.FromSlash(dep.From)))
		where := fmt.Sprintf("%s (address %q)", dep.From, dep.Address)
		if boundary, described := addressBoundary(ctx, root); !pathWithin(boundary, realPath(otherRoot)) {
			refuse(alias, syncReasonAddressOutsideRepository,
				"\"from\": %q leads out of %s — an address names a folder of the same repository", dep.From, described)
			continue
		}
		if stack == "" {
			refuse(alias, syncReasonAddressStackMismatch,
				"an address resolves through the other folder's stack of the same name, and this folder is not deploying through a named stack — name one with --stack")
			continue
		}
		kind, known, err := folderKindAt(otherRoot)
		if err != nil || !known {
			refuse(alias, syncReasonAddressFolderUnreadable,
				"\"from\": %q names no folder this ronja can open (%s) — the path is relative to this folder", dep.From, describeMissingFolder(otherRoot, err))
			continue
		}
		other, err := wfdir.LoadManifest(otherRoot, kind)
		if err != nil {
			refuse(alias, syncReasonAddressFolderUnreadable, "%s does not load: %v", dep.From, err)
			continue
		}
		if !addressKindHeldBy(dep.Kind, kind, dep.Address) {
			refuse(alias, syncReasonAddressFolderUnreadable,
				"%s is a %s folder, and a %s at address %q is not something it manages", dep.From, kind.Label, dep.Kind, dep.Address)
			continue
		}
		here := m.Stacks[stack]
		there, declared := other.Stacks[stack]
		if !declared {
			refuse(alias, syncReasonAddressStackMismatch,
				"%s declares no stack %q, so nothing says which of its deployments this stack reads — declare it there (`ronja sync adopt --stack %s` after its first apply)", dep.From, stack, stack)
			continue
		}
		if !wfdir.Matches(here.Key(), there.Key()) {
			refuse(alias, syncReasonAddressStackMismatch,
				"%s's stack %q deploys to %s and this folder's to %s — an address resolves only within one target, never across organizations or instances",
				dep.From, stack, describeKeyForApply(there.Key()), describeKeyForApply(here.Key()))
			continue
		}
		if problem := addressSameSpace(here, there); problem != nil {
			errs[alias] = *problem
			continue
		}
		if !other.Adopted() || there.Unmanaged {
			refuse(alias, syncReasonAddressNotAdopted,
				"%s keeps no server state for stack %q (it has no folder id, or that stack was unmanaged), so its rows cannot be found by address — run `ronja sync adopt --stack %s` there", dep.From, stack, stack)
			continue
		}
		if there.FeatureID == "" {
			refuse(alias, syncReasonAddressUnresolved, "%s's stack %q names no feature yet — apply that folder first", dep.From, stack)
			continue
		}
		if kind.Name == wfdir.KindWorkflow && dep.Kind == markers.KindTable {
			// A table that workflow writes, named by its dependency there.
			tableAt := func(id string) (*api.Table, error) { return client.GetTable(ctx, id) }
			id, feature, failure := resolveWorkflowTable(client != nil, overlay, claimsAt, tablesAt, tableAt, dep, other, there, stack)
			if failure != nil {
				errs[alias] = wfdir.AddressError{Reason: failure.Reason, Detail: fmt.Sprintf("dependency %q: ", alias) + failure.Detail}
				continue
			}
			bind[alias] = id
			edges = append(edges, addressEdge{Alias: alias, Kind: dep.Kind, FromRoot: otherRoot, Address: dep.Address, ID: id, FeatureID: feature, Written: true})
			continue
		}
		if row, ok := overlay.lookup(there.FeatureID, other.ID, dep.Address); ok {
			if !claimKindFits(dep.Kind, row.Kind) {
				refuse(alias, syncReasonAddressUnresolved,
					"%s's row at %s is a %s, and this folder declares it a %s", dep.From, describeAddressKey(dep.Address), row.Kind, dep.Kind)
				continue
			}
			bind[alias] = row.ID
			edges = append(edges, addressEdge{Alias: alias, Kind: dep.Kind, FromRoot: otherRoot, Address: dep.Address, ID: row.ID, FeatureID: there.FeatureID})
			continue
		}
		if client == nil {
			refuse(alias, syncReasonNoCredential,
				"%s is resolved through Ronja's record of that folder, and there is no credential to read it with — sign in with `ronja login`", where)
			continue
		}
		claims, err := claimsAt(there.FeatureID, other.ID)
		if err != nil {
			refuse(alias, syncReasonLookupFailed, "reading %s's state in feature %s: %v", dep.From, there.FeatureID, err)
			continue
		}
		got := &claimsOf{claims: claims}
		var claim *api.ManagedClaim
		for i := range got.claims {
			if got.claims[i].Address == dep.Address {
				claim = &got.claims[i]
				break
			}
		}
		switch {
		case claim == nil:
			refuse(alias, syncReasonAddressUnresolved,
				"%s manages no row at %s in stack %q yet — apply that folder first, then this one", dep.From, describeAddressKey(dep.Address), stack)
			// The one refusal `sync apply` can defer: a run that deploys that
			// folder creates the row, and resolves this in memory.
			pending[alias] = addressPending{Alias: alias, Kind: dep.Kind, FromRoot: otherRoot, FolderID: other.ID,
				FeatureID: there.FeatureID, Address: dep.Address}
			continue
		case claim.State != api.ManagedStateOK || claim.ResourceID == "":
			refuse(alias, syncReasonAddressUnresolved,
				"%s's row at %s reads %s in stack %q, so it names no row this folder can use — run `ronja sync status` there", dep.From, describeAddressKey(dep.Address), claim.State, stack)
			continue
		case !claimKindFits(dep.Kind, claim.ResourceKind):
			refuse(alias, syncReasonAddressUnresolved,
				"%s's row at %s is a %s, and this folder declares it a %s", dep.From, describeAddressKey(dep.Address), claim.ResourceKind, dep.Kind)
			continue
		}
		bind[alias] = claim.ResourceID
		edges = append(edges, addressEdge{Alias: alias, Kind: dep.Kind, FromRoot: otherRoot, Address: dep.Address, ID: claim.ResourceID, FeatureID: there.FeatureID})
	}
	if len(bind) == 0 {
		bind = nil
	}
	if len(errs) == 0 {
		errs = nil
	}
	if len(pending) == 0 {
		pending = nil
	}
	return bind, errs, edges, pending
}

// resolveWorkflowTable resolves a table named by address in a WORKFLOW
// folder: `{"kind": "table", "from": "<workflow folder>", "address": "<its
// dependency>"}`, where that dependency is declared "write": true or
// "output": true there. Per stack of the same name, it is the table that
// dependency writes in that stack:
//
//   - "write": the workflow folder's binding in the stack — a write source
//     is always bound, so the stack names it, wherever it is;
//   - "output": its binding if the stack has one, else the table the
//     workflow's runs made under that name in the stack's OWN feature
//     (TableListItem.WorkflowID / WorkflowLogicalName). Before the first run
//     there is none, which is refused with the remedy — never answered with a
//     table of that name in another feature, which is exactly how a second
//     stack's app ended up reading production. A bound output must be a table
//     of the stack's own feature too: a stack block copied from another stack
//     binds that stack's table, and is refused address_stack_mismatch.
//
// The workflow's id comes from the run's overlay or that folder's claim, and
// the feature's tables from tablesAt (one listing per feature). feature is
// the feature the table is known to be in — the stack's own for an output,
// "" for a write source.
func resolveWorkflowTable(signedIn bool, overlay *addressOverlay,
	claimsAt func(featureID, folderID string) ([]api.ManagedClaim, error),
	tablesAt func(featureID string) ([]*api.TableListItem, error),
	tableAt func(id string) (*api.Table, error),
	dep wfdir.Dependency, other *wfdir.Manifest, there wfdir.Stack, stack string) (id, feature string, failure *wfdir.AddressError) {
	fail := func(reason, format string, args ...any) (string, string, *wfdir.AddressError) {
		return "", "", &wfdir.AddressError{Reason: reason, Detail: fmt.Sprintf(format, args...)}
	}
	written, declared := other.Dependencies[dep.Address]
	switch {
	case !declared:
		return fail(syncReasonAddressNotWritten,
			"%s declares no dependency %q — a table named by address in a workflow folder is one of its dependencies declared \"write\": true or \"output\": true", dep.From, dep.Address)
	case written.Kind != markers.KindTable || !written.Write && !written.Output || written.ByAddress():
		return fail(syncReasonAddressNotWritten,
			"%s's dependency %q is not a table that workflow writes — name one it declares \"write\": true or \"output\": true", dep.From, dep.Address)
	}
	bound := there.Bind[dep.Address]
	switch {
	case bound != "" && written.Write:
		return bound, "", nil
	case written.Write:
		return fail(syncReasonAddressUnresolved,
			"%s does not bind its write target %q in stack %q, so nothing says which table it writes there — bind it there first", dep.From, dep.Address, stack)
	case !signedIn:
		return fail(syncReasonNoCredential,
			"%s's output %q is found through Ronja's record of that folder, and there is no credential to read it with — sign in with `ronja login`", dep.From, dep.Address)
	}
	if bound != "" {
		// The table itself, not the feature's listing: the listing leaves out a
		// hidden or archived table, which is still that stack's own output.
		table, err := tableAt(bound)
		switch status := api.StatusOf(err); {
		case err == nil && table.FeatureID == there.FeatureID:
			return bound, there.FeatureID, nil
		case err == nil:
			return fail(syncReasonAddressStackMismatch,
				"%s binds its output %q in stack %q to %s, which is in feature %s, not that stack's feature %s — a stack reads its own output, and a binding copied from another stack reads that stack's table; bind the table in %s, or remove the binding so the output is the table the workflow's runs make there",
				dep.From, dep.Address, stack, bound, orUnknown(table.FeatureID), there.FeatureID, there.FeatureID)
		case status == 400 || status == 403 || status == 404:
			return fail(syncReasonAddressStackMismatch,
				"%s binds its output %q in stack %q to %s, which this credential cannot read, so nothing shows it is a table of that stack's feature %s — bind the table in %s, or remove the binding so the output is the table the workflow's runs make there",
				dep.From, dep.Address, stack, bound, there.FeatureID, there.FeatureID)
		default:
			return fail(syncReasonLookupFailed, "reading %s, %s's output %q in stack %q: %v", bound, dep.From, dep.Address, stack, err)
		}
	}
	workflowID := ""
	if row, ok := overlay.lookup(there.FeatureID, other.ID, ""); ok {
		workflowID = row.ID
	} else {
		claims, err := claimsAt(there.FeatureID, other.ID)
		if err != nil {
			return fail(syncReasonLookupFailed, "reading %s's state in feature %s: %v", dep.From, there.FeatureID, err)
		}
		for _, c := range claims {
			if c.Address == "" && c.State == api.ManagedStateOK && c.ResourceKind == wfdir.ManagedWorkflow {
				workflowID = c.ResourceID
			}
		}
	}
	runFirst := fmt.Sprintf("run the workflow on stack %q once (`ronja wf run --stack %s` in %s), then apply again", stack, stack, dep.From)
	if workflowID == "" {
		return fail(syncReasonAddressUnresolved,
			"%s has no workflow in stack %q yet, and its output %q is the table that workflow's first run makes — apply it, %s", dep.From, stack, dep.Address, runFirst)
	}
	tables, err := tablesAt(there.FeatureID)
	if err != nil {
		return fail(syncReasonLookupFailed, "listing feature %s's tables to find %s's output %q: %v", there.FeatureID, dep.From, dep.Address, err)
	}
	for _, t := range tables {
		if t.FeatureID == there.FeatureID && t.WorkflowID == workflowID && t.WorkflowLogicalName == dep.Address {
			return t.ID, there.FeatureID, nil
		}
	}
	return fail(syncReasonAddressUnresolved,
		"%s's workflow has not made its output %q in stack %q's feature %s yet — %s", dep.From, dep.Address, stack, there.FeatureID, runFirst)
}

// addressSameSpace is where the same-SPACE half of the rule hooks
// in: two stacks with the same url and tenantID must ALSO name features in the
// same Space, or a Dev deployment could resolve an address to a Main row. Spaces
// are invisible to this build (a stack does not name one yet), so it
// answers nil; the url/tenantID half is enforced above.
func addressSameSpace(here, there wfdir.Stack) *wfdir.AddressError {
	_, _ = here, there
	return nil
}

// addressKindHeldBy reports whether a folder of `kind` manages a dependency of
// depKind at `address`: a pipeline holds tables (`*.sql`) and metrics
// (`metrics/*.json`, declared kind table); the folder kinds hold their one row
// at "".
func addressKindHeldBy(depKind string, kind wfdir.Kind, address string) bool {
	switch kind.Name {
	case wfdir.KindPipeline:
		return depKind == markers.KindTable && address != ""
	case wfdir.KindWorkflow:
		// The workflow itself at "", or a table it writes, named by its
		// dependency there (resolveWorkflowTable says which).
		return depKind == markers.KindWorkflow && address == "" || depKind == markers.KindTable && address != ""
	case wfdir.KindDataApp:
		return depKind == markers.KindDataApp && address == ""
	case wfdir.KindModule:
		return depKind == markers.KindModule && address == ""
	}
	return false
}

// claimKindFits reports whether a claim's resource kind answers a dependency of
// depKind. A metric is declared `table` (its id is a table id).
func claimKindFits(depKind, resourceKind string) bool {
	switch depKind {
	case markers.KindTable:
		return resourceKind == wfdir.ManagedTable || resourceKind == wfdir.ManagedMetric
	case markers.KindWorkflow:
		return resourceKind == wfdir.ManagedWorkflow
	case markers.KindDataApp:
		return resourceKind == wfdir.ManagedDataApp
	case markers.KindModule:
		return resourceKind == wfdir.ManagedModule
	}
	return false
}

func describeAddressKey(address string) string {
	if address == "" {
		return "the folder itself"
	}
	return fmt.Sprintf("%q", address)
}

func describeMissingFolder(root string, err error) string {
	if err != nil {
		return err.Error()
	}
	return wfdir.ManifestPath(root) + " declares no folder kind"
}

// addressRefusals turns the unresolved address dependencies into the
// pre-flight's refusals, one per alias, sorted. An address dependency with
// neither a resolution nor a recorded reason was never asked about (a command
// that opened the folder without reading server state).
func addressRefusals(m *wfdir.Manifest, sel wfdir.Selection) []addressRefusal {
	var out []addressRefusal
	for _, alias := range m.AliasNames() {
		dep := m.Dependencies[alias]
		if dep.Output && !dep.ByAddress() {
			// An unbound output read by ref whose lookup failed
			// (resolveOwnOutputs) — refused like an address that could not be
			// read. One that resolved, or needed no lookup, records nothing.
			if e, failed := sel.AddressErrors[alias]; failed {
				out = append(out, addressRefusal{Alias: alias, Reason: e.Reason, Detail: e.Detail})
			}
			continue
		}
		if !dep.ByAddress() {
			continue
		}
		if _, ok := sel.AddressBind[alias]; ok {
			continue
		}
		e, recorded := sel.AddressErrors[alias]
		if !recorded {
			e = wfdir.AddressError{Reason: syncReasonAddressUnresolved, Detail: fmt.Sprintf(
				"dependency %q is declared by address (%s, %s) and was not resolved — this command does not read the repository's server state", alias, dep.From, describeAddressKey(dep.Address))}
		}
		out = append(out, addressRefusal{Alias: alias, Reason: e.Reason, Detail: e.Detail})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Alias < out[j].Alias })
	return out
}

// addressRefusal is one unresolved address dependency, with its reason code.
type addressRefusal struct {
	Alias  string
	Reason string
	Detail string
}

// crossFolderTableReads is the subset of this folder's resolved address
// dependencies that read a TABLE another folder builds — the edges a
// cross-folder build order is made of.
func (f *folder) crossFolderTableReads() []addressEdge {
	var out []addressEdge
	for _, e := range f.addressEdges {
		if e.Kind == markers.KindTable && strings.HasSuffix(e.Address, ".sql") {
			out = append(out, e)
		}
	}
	return out
}

// addressStatusVerdict is `sync status`'s answer for a folder whose ADDRESS
// dependencies did not all resolve: `unknown`, with the first address reason.
// Comparing such a folder would de-alias the server's rows with half a codec
// and report drift that is not there. Zero (empty Verdict) for every folder
// whose addresses resolved, and for one that declares none.
func addressStatusVerdict(ctx context.Context, folder discoveredFolder, resolved *config.Resolved) folderVerdict {
	if folder.Manifest == nil || !hasAddressDependencies(folder.Manifest) {
		return folderVerdict{}
	}
	f, err := openFolderForStatusAt(ctx, folder.Root, resolved, folder.Kind)
	if err != nil || f.OrgErr != nil {
		// The per-kind report opens the folder again and says this properly.
		return folderVerdict{}
	}
	problems := addressRefusals(f.Manifest, f.selection())
	if len(problems) == 0 {
		return folderVerdict{}
	}
	details := make([]string, 0, len(problems))
	for _, p := range problems {
		details = append(details, p.Detail)
	}
	return folderVerdict{Verdict: verdictUnknown, Reason: problems[0].Reason, Detail: strings.Join(details, "; ")}
}
