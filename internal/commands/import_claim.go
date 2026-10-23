package commands

import (
	"errors"
	"fmt"
	"maps"
	"os"
	"path/filepath"
	"slices"
	"sort"

	"github.com/ronjatech/ronja-cli/internal/api"
	"github.com/ronjatech/ronja-cli/internal/wfdir"
)

// The CLAIM half of `ronja import feature`: every written row claimed for the
// importing stack, the rows the server refused withdrawn from the tree and the
// folders that named them written again (settle), and the anchors recorded.
// Split from import.go.

// pendingImportClaim is one row a written folder's lock binds and the server
// has no claim for yet.
type pendingImportClaim struct {
	folder *importFolder
	f      *folder
	a      wfdir.ManagedAddress
}

// claimAll is step 5: claim every written row, in batches of at most 200, and
// record the anchors the locks carry. A row the server refuses leaves the tree,
// and every folder that referenced it is planned and written again with that
// reference as a data source (settle) — so no dependent is left pointing at a
// row nothing manages.
func (imp *featureImport) claimAll() {
	opened := map[*importFolder]*folder{}
	var pending []pendingImportClaim
	for _, w := range imp.folders {
		f, err := imp.openForClaims(w)
		if err != nil {
			imp.problems = append(imp.problems, fmt.Sprintf("%s: %v", w.Rel, err))
			continue
		}
		opened[w] = f
		for _, a := range f.managedBackfill() {
			pending = append(pending, pendingImportClaim{folder: w, f: f, a: a})
		}
	}
	// refused is every row the server refused, by id, with the index of its
	// outcome in imp.claims (its detail is finished once settle has run).
	refused := map[string]int{}
	claimedIn := map[*importFolder]int{}
	for start := 0; start < len(pending); start += api.MaxManagedClaimItems {
		batch := pending[start:min(start+api.MaxManagedClaimItems, len(pending))]
		items := make([]api.ManagedClaimItem, 0, len(batch))
		for _, p := range batch {
			items = append(items, api.ManagedClaimItem{ResourceKind: p.a.Kind, ResourceID: p.a.ID, ManagedClaimInput: *p.f.managedClaim(p.a.Address)})
		}
		results, err := imp.client.ClaimManaged(imp.ctx, imp.feature.ID, items)
		answered := map[int]api.ManagedClaimResult{}
		for _, r := range results {
			answered[r.Index] = r
		}
		for i, p := range batch {
			out := importClaimOutcome{Folder: p.folder.Rel, Address: p.a.Address, Kind: p.a.Kind, ID: p.a.ID}
			r, ok := answered[i]
			if err == nil && ok {
				out.Outcome, out.Code = r.Outcome, r.Code
			}
			switch {
			case out.Outcome == api.ManagedOutcomeClaimed || out.Outcome == api.ManagedOutcomeAlreadyClaimed:
				if r.Claim != nil {
					p.f.managed.claims[p.a.Address] = r.Claim
				}
				claimedIn[p.folder]++
				imp.claims = append(imp.claims, out)
			case out.Outcome == api.ManagedOutcomeRefused && importRefusalCodes[r.Code]:
				// The server REFUSED this row (another folder claimed it
				// meanwhile, the caller may not write it, …). The tree must hold
				// neither a lock entry the server refused nor the unit itself: a
				// file with no binding is one the next `--allow-create` turns
				// into a second row beside the one somebody else manages.
				out.Detail = describeClaimRefusal(p.f, p.a, r).Detail
				refused[p.a.ID] = len(imp.claims)
				imp.claims = append(imp.claims, out)
			default:
				// No answer that says anything about the row: a refused or lost
				// REQUEST (a token without structure:write, a timeout, a reply
				// lost after the server committed), or a per-item server fault
				// (internal_error, which the server's docs say to retry) or a
				// code this build does not know. Its lock entry STAYS: the row
				// is this folder's, and `sync adopt` (or the next deploy's lazy
				// claim) claims it. Dropping it would leave a folder whose next
				// `--allow-create` makes a second copy.
				why := "the server answered no outcome for this row"
				switch {
				case err != nil:
					why = err.Error()
				case ok && r.Error != "":
					why = fmt.Sprintf("%s: %s", orUnknown(r.Code), r.Error)
				case ok:
					why = fmt.Sprintf("the server answered %s (%s)", orUnknown(r.Outcome), orUnknown(r.Code))
				}
				out.Outcome, out.Detail = importOutcomeUnclaimed, why
				// Left alone by the anchor step below, too: it would claim the
				// row one by one behind this report's back, or fail a second
				// time with another message about the same row.
				if p.f.managed.unclaimable == nil {
					p.f.managed.unclaimable = map[string]bool{}
				}
				p.f.managed.unclaimable[p.a.Address] = true
				imp.problems = append(imp.problems, fmt.Sprintf("%s %s was written and not claimed (%s) — run `ronja sync adopt --stack %s` in the tree to claim it",
					p.folder.Rel, describeAddressKey(p.a.Address), why, imp.stack))
				imp.claims = append(imp.claims, out)
			}
		}
	}
	if len(refused) > 0 {
		imp.settle(refused, opened)
	}
	for _, w := range imp.folders {
		f := opened[w]
		if f == nil || w.withdrawn {
			continue
		}
		if claimedIn[w] == 0 {
			// Nothing of it is claimed: there is no anchor to record, and
			// `sync adopt` — the remedy each of its rows names — records them.
			continue
		}
		if err := f.writeManagedAnchors(imp.ctx); err != nil {
			imp.problems = append(imp.problems, fmt.Sprintf("%s: the rows were claimed and their drift baselines could not be recorded: %v", w.Rel, err))
		}
	}
}

// openForClaims opens a written folder the way a status does, with its server
// state read.
func (imp *featureImport) openForClaims(w *importFolder) (*folder, error) {
	f, err := openFolderForStatusAt(withAddressWalkRoot(imp.ctx, imp.base), w.Root, imp.resolved, w.Kind)
	if err == nil && f.OrgErr != nil {
		err = f.OrgErr
	}
	if err == nil && f.managed == nil {
		err = errors.New("the folder did not open as an adopted one")
	}
	if err != nil {
		return nil, err
	}
	f.ctx = imp.ctx
	return f, nil
}

// importRefusalCodes are the per-item codes that say the server will not
// claim the row as it stands (rmanaged's claim refusals, the row probe's
// not_found / forbidden / insufficient_scope, and a malformed item). Any other
// code — internal_error above all — says nothing about the row, which is then
// kept and reported unclaimed rather than removed.
var importRefusalCodes = map[string]bool{
	api.ManagedCodeAddressClaimed: true, api.ManagedCodeResourceClaimed: true, api.ManagedCodeMovedOut: true,
	api.ManagedCodeTrashed: true, api.ManagedCodeNotRoot: true, api.ManagedCodeNotClaimable: true,
	"not_found": true, "forbidden": true, api.InsufficientScopeCode: true, "invalid": true,
}

// importOutcomeUnclaimed is a row written and not claimed because the claim
// REQUEST got no per-row answer; it keeps its lock entry.
const importOutcomeUnclaimed = "unclaimed"

// settle is the second pass over the rows the server refused (the import's
// fifth step): each one leaves the tree, as a row claimed elsewhere would never have
// entered it, and every folder that held one or referenced one runs the
// portability pass again without it and is written again — so a reference to a
// withdrawn table becomes a data source bound in this stack rather than an
// address nothing will ever claim. It converges in that one pass: rewriting a
// folder claims nothing, so it cannot be refused anything new.
func (imp *featureImport) settle(refused map[string]int, opened map[*importFolder]*folder) {
	gone := make(map[string]bool, len(refused))
	for id := range refused {
		gone[id] = true
		delete(imp.byID, id)
	}
	// A table a withdrawn WORKFLOW folder writes is no longer one a folder of
	// the tree writes: a folder reading it by address is planned again, and
	// binds it as a data source instead.
	unwritten := map[string]bool{}
	for id, w := range imp.writtenBy {
		if !slices.ContainsFunc(w.folder.rows, func(row *importRow) bool { return !gone[row.res.ID] }) {
			unwritten[id] = true
			delete(imp.writtenBy, id)
		}
	}
	var affected []*importFolder
	for _, w := range imp.folders {
		holds, names := false, false
		for _, row := range w.rows {
			if gone[row.res.ID] {
				holds = true
			}
			for _, ref := range row.res.References {
				if gone[ref.TargetID] || unwritten[ref.TargetID] {
					names = true
				}
			}
		}
		switch {
		case holds && !slices.ContainsFunc(w.rows, func(row *importRow) bool { return !gone[row.res.ID] }):
			// Every row it held was refused: the folder holds nothing else.
			w.withdrawn = true
			if err := os.RemoveAll(w.Root); err != nil {
				imp.problems = append(imp.problems, fmt.Sprintf("%s: its rows were refused and the folder could not be removed: %v", w.Rel, err))
			}
		case holds || names:
			affected = append(affected, w)
		}
	}
	rewritten := map[string]bool{}
	for _, w := range affected {
		imp.forgetFolder(w.Rel)
		w.replanWithout(gone)
	}
	imp.planPortability(affected, imp.byID)
	for _, w := range workflowFoldersFirst(affected) {
		err := imp.rewrite(w)
		if err == nil {
			err = imp.relay(w)
		}
		if err != nil {
			imp.problems = append(imp.problems, fmt.Sprintf("%s: it held or named a row the server refused, and it could not be written again without it, so it still does: %v", w.Rel, err))
			continue
		}
		rewritten[w.Rel] = true
		f, err := imp.openForClaims(w)
		if err != nil {
			delete(opened, w)
			imp.problems = append(imp.problems, fmt.Sprintf("%s: %v", w.Rel, err))
			continue
		}
		// The rows the report already calls unclaimed stay so on the folder
		// opened again: its anchor step would otherwise claim them behind the
		// report's back, or fail a second time with another message.
		if prev := opened[w]; prev != nil && prev.managed != nil {
			f.managed.unclaimable = prev.managed.unclaimable
		}
		opened[w] = f
	}
	order := make([]int, 0, len(refused))
	for _, i := range refused {
		order = append(order, i)
	}
	sort.Ints(order)
	for _, i := range order {
		out := &imp.claims[i]
		folderGone := false
		for _, w := range imp.folders {
			if w.Rel == out.Folder {
				folderGone = w.withdrawn
			}
		}
		switch {
		case folderGone, rewritten[out.Folder]:
			out.Detail += " — removed from the tree, and every reference to it declared as a data source bound in this stack"
		default:
			out.Detail += " — and it could not be removed from the tree"
		}
		imp.problems = append(imp.problems, fmt.Sprintf("%s %s: %s", out.Folder, describeAddressKey(out.Address), out.Detail))
	}
}

// forgetFolder drops every report entry the portability pass made for one
// folder, before the pass runs over it again.
func (imp *featureImport) forgetFolder(rel string) {
	imp.literals = slices.DeleteFunc(imp.literals, func(l importLiteral) bool { return l.Folder == rel })
	imp.sources = slices.DeleteFunc(imp.sources, func(d importDataSource) bool { return d.Folder == rel })
	imp.addresses = slices.DeleteFunc(imp.addresses, func(a importAddress) bool { return a.Folder == rel })
	imp.outputs = slices.DeleteFunc(imp.outputs, func(o importOutput) bool { return o.Folder == rel })
	maps.DeleteFunc(imp.writtenBy, func(_ string, w writtenTable) bool { return w.folder.Rel == rel })
	imp.notCarried = slices.DeleteFunc(imp.notCarried, func(c importNotCarried) bool { return c.Folder == rel })
}

// relay writes a folder again, in place: into a staging directory inside the
// import root first, then swapped for the folder, so a failure part way leaves
// the folder as it was.
func (imp *featureImport) relay(w *importFolder) error {
	staging, err := os.MkdirTemp(imp.base, importStagingPrefix)
	if err != nil {
		return err
	}
	defer os.RemoveAll(staging)
	fresh := filepath.Join(staging, "folder")
	if err := imp.writeFolder(w, fresh); err != nil {
		return err
	}
	old := filepath.Join(staging, "old")
	if err := os.Rename(w.Root, old); err != nil {
		return err
	}
	if err := os.Rename(fresh, w.Root); err != nil {
		if rerr := os.Rename(old, w.Root); rerr != nil {
			return fmt.Errorf("%w (and the folder could not be put back: %v)", err, rerr)
		}
		return err
	}
	return nil
}
